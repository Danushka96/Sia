package renter

// The download code follows a hopefully clean/intuitive flow for getting super
// high and computationally efficient parallelism on downloads. When a download
// is requested, it gets split into its respective chunks (which are downloaded
// individually) and then put into the download heap. The primary purpose of the
// download heap is to keep downloads on standby until there is enough memory
// available to send the downloads off to the workers. The heap is sorted first
// by priority, but then a few other criteria as well.
//
// Some downloads, in particular downloads issued by the repair code, have
// already had their memory allocated. These downloads get to skip the heap and
// go straight for the workers.
//
// When a download is distributed to workers, it is given to every single worker
// without checking whether that worker is appropriate for the download. Each
// worker has their own queue, which is bottlenecked by the fact that a worker
// can only process one item at a time. When the worker gets to a download
// request, it determines whether it is suited for downloading that particular
// file. The criteria it uses include whether or not it has a piece of that
// chunk, how many other workers are currently downloading pieces or have
// completed pieces for that chunk, and finally things like worker latency and
// worker price.
//
// If the worker chooses to download a piece, it will register itself with that
// piece, so that other workers know how many workers are downloading each
// piece. This keeps everything cleanly coordinated and prevents too many
// workers from downloading a given piece, while at the same time you don't need
// a giant messy coordinator tracking everything. If a worker chooses not to
// download a piece, it will add itself to the list of standby workers, so that
// in the event of a failure, the worker can be returned to and used again as a
// backup worker. The worker may also decide that it is not suitable at all (for
// example, if the worker has recently had some consecutive failures, or if the
// worker doesn't have access to a piece of that chunk), in which case it will
// mark itself as unavailable to the chunk.
//
// As workers complete, they will release memory and check on the overall state
// of the chunk. If some workers fail, they will enlist the standby workers to
// pick up the slack.
//
// When the final required piece finishes downloading, the worker who completed
// the final piece will spin up a separate thread to decrypt, decode, and write
// out the download. That thread will then clean up any remaining resources, and
// if this was the final unfinished chunk in the download, it'll mark the
// download as complete.

// The download process has a slightly complicating factor, which is overdrive
// workers. Traditionally, if you need 10 pieces to recover a file, you will use
// 10 workers. But if you have an overdrive of '2', you will actually use 12
// workers, meaning you download 2 more pieces than you need. This means that up
// to two of the workers can be slow or fail and the download can still complete
// quickly. This complicates resource handling, because not all memory can be
// released as soon as a download completes - there may be overdrive workers
// still out fetching the file. To handle this, a catchall 'cleanUp' function is
// used which gets called every time a worker finishes, and every time recovery
// completes. The result is that memory gets cleaned up as required, and no
// overarching coordination is needed between the overdrive workers (who do not
// even know that they are overdrive workers) and the recovery function.

// By default, the download code organizes itself around having maximum possible
// throughput. That is, it is highly parallel, and exploits that parallelism as
// efficiently and effectively as possible. The hostdb does a good of selecting
// for hosts that have good traits, so we can generally assume that every host
// or worker at our disposable is reasonably effective in all dimensions, and
// that the overall selection is generally geared towards the user's
// preferences.
//
// We can leverage the standby workers in each unfinishedDownloadChunk to
// emphasize various traits. For example, if we want to prioritize latency,
// we'll put a filter in the 'managedProcessDownloadChunk' function that has a
// worker go standby instead of accept a chunk if the latency is higher than the
// targeted latency. These filters can target other traits as well, such as
// price and total throughput.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"gitlab.com/NebulousLabs/errors"

	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/modules/renter/siafile"
	"gitlab.com/NebulousLabs/Sia/persist"
	"gitlab.com/NebulousLabs/Sia/types"
)

type (
	// A download is a file download that has been queued by the renter.
	download struct {
		// Data progress variables.
		atomicDataReceived         uint64 // Incremented as data completes, will stop at 100% file progress.
		atomicTotalDataTransferred uint64 // Incremented as data arrives, includes overdrive, contract negotiation, etc.

		// Other progress variables.
		chunksRemaining uint64        // Number of chunks whose downloads are incomplete.
		completeChan    chan struct{} // Closed once the download is complete.
		err             error         // Only set if there was an error which prevented the download from completing.

		// downloadCompleteFunc is a slice of functions which are called when
		// completeChan is closed.
		downloadCompleteFuncs []func(error) error

		// Timestamp information.
		endTime         time.Time // Set immediately before closing 'completeChan'.
		staticStartTime time.Time // Set immediately when the download object is created.

		// Basic information about the file.
		destination           downloadDestination
		destinationString     string          // The string reported to the user to indicate the download's destination.
		staticDestinationType string          // "memory buffer", "http stream", "file", etc.
		staticLength          uint64          // Length to download starting from the offset.
		staticOffset          uint64          // Offset within the file to start the download.
		staticSiaPath         modules.SiaPath // The path of the siafile at the time the download started.

		// Retrieval settings for the file.
		staticLatencyTarget time.Duration // In milliseconds. Lower latency results in lower total system throughput.
		staticOverdrive     int           // How many extra pieces to download to prevent slow hosts from being a bottleneck.
		staticPriority      uint64        // Downloads with higher priority will complete first.

		// Utilities.
		log           *persist.Logger // Same log as the renter.
		memoryManager *memoryManager  // Same memoryManager used across the renter.
		mu            sync.Mutex      // Unique to the download object.
	}

	// downloadParams is the set of parameters to use when downloading a file.
	downloadParams struct {
		destination       downloadDestination // The place to write the downloaded data.
		destinationType   string              // "file", "buffer", "http stream", etc.
		destinationString string              // The string to report to the user for the destination.
		file              *siafile.Snapshot   // The file to download.

		latencyTarget time.Duration // Workers above this latency will be automatically put on standby initially.
		length        uint64        // Length of download. Cannot be 0.
		needsMemory   bool          // Whether new memory needs to be allocated to perform the download.
		offset        uint64        // Offset within the file to start the download. Must be less than the total filesize.
		overdrive     int           // How many extra pieces to download to prevent slow hosts from being a bottleneck.
		priority      uint64        // Files with a higher priority will be downloaded first.
	}
)

// managedCancel cancels a download by marking it as failed.
func (d *download) managedCancel() {
	d.managedFail(modules.ErrDownloadCancelled)
}

// managedFail will mark the download as complete, but with the provided error.
// If the download has already failed, the error will be updated to be a
// concatenation of the previous error and the new error.
func (d *download) managedFail(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// If the download is already complete, extend the error.
	complete := d.staticComplete()
	if complete && d.err != nil {
		return
	} else if complete && d.err == nil {
		d.log.Critical("download is marked as completed without error, but then managedFail was called with err:", err)
		return
	}

	// Mark the download as complete and set the error.
	d.err = err
	d.markComplete()
}

// markComplete is a helper method which closes the completeChan and and
// executes the downloadCompleteFuncs. The completeChan should always be closed
// using this method.
func (d *download) markComplete() {
	// Avoid calling markComplete multiple times. In a production build
	// build.Critical won't panic which is fine since we set
	// downloadCompleteFunc to nil after executing them. We still don't want to
	// close the completeChan again though to avoid a crash.
	if d.staticComplete() {
		build.Critical("Can't call markComplete multiple times")
	} else {
		defer close(d.completeChan)
	}
	// Execute the downloadCompleteFuncs before closing the channel. This gives
	// the initiator of the download the nice guarantee that waiting for the
	// completeChan to be closed also means that the downloadCompleteFuncs are
	// done.
	var err error
	for _, f := range d.downloadCompleteFuncs {
		err = errors.Compose(err, f(d.err))
	}
	// Log potential errors.
	if err != nil {
		d.log.Println("Failed to execute at least one downloadCompleteFunc", err)
	}
	// Set downloadCompleteFuncs to nil to avoid executing them multiple times.
	d.downloadCompleteFuncs = nil
}

// onComplete registers a function to be called when the download is completed.
// This can either mean that the download succeeded or failed. The registered
// functions are executed in the same order as they are registered and waiting
// for the download's completeChan to be closed implies that the registered
// functions were executed.
func (d *download) onComplete(f func(error) error) {
	select {
	case <-d.completeChan:
		if err := f(d.err); err != nil {
			d.log.Println("Failed to execute downloadCompleteFunc", err)
		}
		return
	default:
	}
	d.downloadCompleteFuncs = append(d.downloadCompleteFuncs, f)
}

// staticComplete is a helper function to indicate whether or not the download
// has completed.
func (d *download) staticComplete() bool {
	select {
	case <-d.completeChan:
		return true
	default:
		return false
	}
}

// Err returns the error encountered by a download, if it exists.
func (d *download) Err() (err error) {
	d.mu.Lock()
	err = d.err
	d.mu.Unlock()
	return err
}

// OnComplete registers a function to be called when the download is completed.
// This can either mean that the download succeeded or failed. The registered
// functions are executed in the same order as they are registered and waiting
// for the download's completeChan to be closed implies that the registered
// functions were executed.
func (d *download) OnComplete(f func(error) error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onComplete(f)
}

// Download performs a file download using the passed parameters and blocks
// until the download is finished.
func (r *Renter) Download(p modules.RenterDownloadParameters) error {
	if err := r.tg.Add(); err != nil {
		return err
	}
	defer r.tg.Done()
	d, err := r.managedDownload(p)
	if err != nil {
		return err
	}
	// Block until the download has completed
	select {
	case <-d.completeChan:
		return d.Err()
	case <-r.tg.StopChan():
		return errors.New("download interrupted by shutdown")
	}
}

// DownloadAsync performs a file download using the passed parameters without
// blocking until the download is finished.
func (r *Renter) DownloadAsync(p modules.RenterDownloadParameters, f func(error) error) (cancel func(), err error) {
	if err := r.tg.Add(); err != nil {
		return nil, err
	}
	defer r.tg.Done()
	d, err := r.managedDownload(p)
	if err != nil {
		return nil, err
	}
	if f != nil {
		d.onComplete(f)
	}
	return d.managedCancel, err
}

// managedDownload performs a file download using the passed parameters and
// returns the download object and an error that indicates if the download
// setup was successful.
func (r *Renter) managedDownload(p modules.RenterDownloadParameters) (*download, error) {
	// Lookup the file associated with the nickname.
	entry, err := r.staticFileSet.Open(p.SiaPath)
	if err != nil {
		return nil, err
	}
	defer entry.Close()
	defer entry.UpdateAccessTime()

	// Validate download parameters.
	isHTTPResp := p.Httpwriter != nil
	if p.Async && isHTTPResp {
		return nil, errors.New("cannot async download to http response")
	}
	if isHTTPResp && p.Destination != "" {
		return nil, errors.New("destination cannot be specified when downloading to http response")
	}
	if !isHTTPResp && p.Destination == "" {
		return nil, errors.New("destination not supplied")
	}
	if p.Destination != "" && !filepath.IsAbs(p.Destination) {
		return nil, errors.New("destination must be an absolute path")
	}
	if p.Offset == entry.Size() && entry.Size() != 0 {
		return nil, errors.New("offset equals filesize")
	}
	// Sentinel: if length == 0, download the entire file.
	if p.Length == 0 {
		if p.Offset > entry.Size() {
			return nil, errors.New("offset cannot be greater than file size")
		}
		p.Length = entry.Size() - p.Offset
	}
	// Check whether offset and length is valid.
	if p.Offset < 0 || p.Offset+p.Length > entry.Size() {
		return nil, fmt.Errorf("offset and length combination invalid, max byte is at index %d", entry.Size()-1)
	}

	// Instantiate the correct downloadWriter implementation.
	var dw downloadDestination
	var destinationType string
	if isHTTPResp {
		dw = newDownloadDestinationWriter(p.Httpwriter)
		destinationType = "http stream"
	} else {
		osFile, err := os.OpenFile(p.Destination, os.O_CREATE|os.O_WRONLY, entry.Mode())
		if err != nil {
			return nil, err
		}
		dw = &downloadDestinationFile{f: osFile}
		destinationType = "file"
	}

	// If the destination is a httpWriter, we set the Content-Length in the
	// header.
	if isHTTPResp {
		w, ok := p.Httpwriter.(http.ResponseWriter)
		if ok {
			w.Header().Set("Content-Length", fmt.Sprint(p.Length))
		}
	}

	// Prepare snapshot.
	snap, err := entry.Snapshot()
	if err != nil {
		return nil, err
	}
	// Create the download object.
	d, err := r.managedNewDownload(downloadParams{
		destination:       dw,
		destinationType:   destinationType,
		destinationString: p.Destination,
		file:              snap,

		latencyTarget: 25e3 * time.Millisecond, // TODO: high default until full latency support is added.
		length:        p.Length,
		needsMemory:   true,
		offset:        p.Offset,
		overdrive:     3, // TODO: moderate default until full overdrive support is added.
		priority:      5, // TODO: moderate default until full priority support is added.
	})
	if closer, ok := dw.(io.Closer); err != nil && ok {
		// If the destination can be closed we do so.
		return nil, errors.Compose(err, closer.Close())
	} else if err != nil {
		return nil, err
	}

	// Register some cleanup for when the download is done.
	d.OnComplete(func(_ error) error {
		// close the destination if possible.
		if closer, ok := dw.(io.Closer); ok {
			return closer.Close()
		}
		return nil
	})

	// Add the download object to the download history if it's not a stream.
	if destinationType != destinationTypeSeekStream {
		r.downloadHistoryMu.Lock()
		r.downloadHistory = append(r.downloadHistory, d)
		r.downloadHistoryMu.Unlock()
	}

	// Return the download object
	return d, nil
}

// managedNewDownload creates and initializes a download based on the provided
// parameters.
func (r *Renter) managedNewDownload(params downloadParams) (*download, error) {
	// Input validation.
	if params.file == nil {
		return nil, errors.New("no file provided when requesting download")
	}
	if params.length < 0 {
		return nil, errors.New("download length must be zero or a positive whole number")
	}
	if params.offset < 0 {
		return nil, errors.New("download offset cannot be a negative number")
	}
	if params.offset+params.length > params.file.Size() {
		return nil, errors.New("download is requesting data past the boundary of the file")
	}

	// Create the download object.
	d := &download{
		completeChan: make(chan struct{}),

		staticStartTime: time.Now(),

		destination:           params.destination,
		destinationString:     params.destinationString,
		staticDestinationType: params.destinationType,
		staticLatencyTarget:   params.latencyTarget,
		staticLength:          params.length,
		staticOffset:          params.offset,
		staticOverdrive:       params.overdrive,
		staticSiaPath:         params.file.SiaPath(),
		staticPriority:        params.priority,

		log:           r.log,
		memoryManager: r.memoryManager,
	}

	// Update the endTime of the download when it's done.
	d.onComplete(func(_ error) error {
		d.endTime = time.Now()
		return nil
	})

	// Nothing more to do for 0-byte files or 0-length downloads.
	if d.staticLength == 0 {
		d.markComplete()
		return d, nil
	}

	// Determine which chunks to download.
	minChunk, minChunkOffset := params.file.ChunkIndexByOffset(params.offset)
	maxChunk, maxChunkOffset := params.file.ChunkIndexByOffset(params.offset + params.length)
	// If the maxChunkOffset is exactly 0 we need to subtract 1 chunk. e.g. if
	// the chunkSize is 100 bytes and we want to download 100 bytes from offset
	// 0, maxChunk would be 1 and maxChunkOffset would be 0. We want maxChunk
	// to be 0 though since we don't actually need any data from chunk 1.
	if maxChunk > 0 && maxChunkOffset == 0 {
		maxChunk--
	}
	// Make sure the requested chunks are within the boundaries.
	if minChunk == params.file.NumChunks() || maxChunk == params.file.NumChunks() {
		return nil, errors.New("download is requesting a chunk that is past the boundary of the file")
	}

	// For each chunk, assemble a mapping from the contract id to the index of
	// the piece within the chunk that the contract is responsible for.
	chunkMaps := make([]map[string]downloadPieceInfo, maxChunk-minChunk+1)
	for chunkIndex := minChunk; chunkIndex <= maxChunk; chunkIndex++ {
		// Create the map.
		chunkMaps[chunkIndex-minChunk] = make(map[string]downloadPieceInfo)
		// Get the pieces for the chunk.
		pieces, err := params.file.Pieces(uint64(chunkIndex))
		if err != nil {
			return nil, err
		}
		for pieceIndex, pieceSet := range pieces {
			for _, piece := range pieceSet {
				// Sanity check - the same worker should not have two pieces for
				// the same chunk.
				_, exists := chunkMaps[chunkIndex-minChunk][piece.HostPubKey.String()]
				if exists {
					r.log.Println("ERROR: Worker has multiple pieces uploaded for the same chunk.", params.file.SiaPath(), chunkIndex, pieceIndex, piece.HostPubKey.String())
				}
				chunkMaps[chunkIndex-minChunk][piece.HostPubKey.String()] = downloadPieceInfo{
					index: uint64(pieceIndex),
					root:  piece.MerkleRoot,
				}
			}
		}
	}

	// Queue the downloads for each chunk.
	writeOffset := int64(0) // where to write a chunk within the download destination.
	d.chunksRemaining += maxChunk - minChunk + 1
	for i := minChunk; i <= maxChunk; i++ {
		udc := &unfinishedDownloadChunk{
			destination: params.destination,
			erasureCode: params.file.ErasureCode(),
			masterKey:   params.file.MasterKey(),

			staticChunkIndex: i,
			staticCacheID:    fmt.Sprintf("%v:%v", d.staticSiaPath, i),
			staticChunkMap:   chunkMaps[i-minChunk],
			staticChunkSize:  params.file.ChunkSize(),
			staticPieceSize:  params.file.PieceSize(),

			// TODO: 25ms is just a guess for a good default. Really, we want to
			// set the latency target such that slower workers will pick up the
			// later chunks, but only if there's a very strong chance that
			// they'll finish before the earlier chunks finish, so that they do
			// no contribute to low latency.
			//
			// TODO: There is some sane minimum latency that should actually be
			// set based on the number of pieces 'n', and the 'n' fastest
			// workers that we have.
			staticLatencyTarget: params.latencyTarget + (25 * time.Duration(i-minChunk)), // Increase target by 25ms per chunk.
			staticNeedsMemory:   params.needsMemory,
			staticPriority:      params.priority,

			completedPieces:   make([]bool, params.file.ErasureCode().NumPieces()),
			physicalChunkData: make([][]byte, params.file.ErasureCode().NumPieces()),
			pieceUsage:        make([]bool, params.file.ErasureCode().NumPieces()),

			download:   d,
			renterFile: params.file,
		}

		// Set the fetchOffset - the offset within the chunk that we start
		// downloading from.
		if i == minChunk {
			udc.staticFetchOffset = minChunkOffset
		} else {
			udc.staticFetchOffset = 0
		}
		// Set the fetchLength - the number of bytes to fetch within the chunk
		// that we start downloading from.
		if i == maxChunk && maxChunkOffset != 0 {
			udc.staticFetchLength = maxChunkOffset - udc.staticFetchOffset
		} else {
			udc.staticFetchLength = params.file.ChunkSize() - udc.staticFetchOffset
		}
		// Set the writeOffset within the destination for where the data should
		// be written.
		udc.staticWriteOffset = writeOffset
		writeOffset += int64(udc.staticFetchLength)

		// TODO: Currently all chunks are given overdrive. This should probably
		// be changed once the hostdb knows how to measure host speed/latency
		// and once we can assign overdrive dynamically.
		udc.staticOverdrive = params.overdrive

		// Add this chunk to the chunk heap, and notify the download loop that
		// there is work to do.
		r.managedAddChunkToDownloadHeap(udc)
		select {
		case r.newDownloads <- struct{}{}:
		default:
		}
	}
	return d, nil
}

// DownloadHistory returns the list of downloads that have been performed. Will
// include downloads that have not yet completed. Downloads will be roughly,
// but not precisely, sorted according to start time.
//
// TODO: Currently the DownloadHistory only contains downloads from this
// session, does not contain downloads that were executed for the purposes of
// repairing, and has no way to clear the download history if it gets long or
// unwieldy. It's not entirely certain which of the missing features are
// actually desirable, please consult core team + app dev community before
// deciding what to implement.
func (r *Renter) DownloadHistory() []modules.DownloadInfo {
	r.downloadHistoryMu.Lock()
	defer r.downloadHistoryMu.Unlock()

	downloads := make([]modules.DownloadInfo, len(r.downloadHistory))
	for i := range r.downloadHistory {
		// Order from most recent to least recent.
		d := r.downloadHistory[len(r.downloadHistory)-i-1]
		d.mu.Lock() // Lock required for d.endTime only.
		downloads[i] = modules.DownloadInfo{
			Destination:     d.destinationString,
			DestinationType: d.staticDestinationType,
			Length:          d.staticLength,
			Offset:          d.staticOffset,
			SiaPath:         d.staticSiaPath,

			Completed:            d.staticComplete(),
			EndTime:              d.endTime,
			Received:             atomic.LoadUint64(&d.atomicDataReceived),
			StartTime:            d.staticStartTime,
			StartTimeUnix:        d.staticStartTime.UnixNano(),
			TotalDataTransferred: atomic.LoadUint64(&d.atomicTotalDataTransferred),
		}
		// Release download lock before calling d.Err(), which will acquire the
		// lock. The error needs to be checked separately because we need to
		// know if it's 'nil' before grabbing the error string.
		d.mu.Unlock()
		if d.Err() != nil {
			downloads[i].Error = d.Err().Error()
		} else {
			downloads[i].Error = ""
		}
	}
	return downloads
}

// ClearDownloadHistory clears the renter's download history inclusive of the
// provided before and after timestamps
//
// TODO: This function can be improved by implementing a binary search, the
// trick will be making the binary search be just as readable while handling
// all the edge cases
func (r *Renter) ClearDownloadHistory(after, before time.Time) error {
	if err := r.tg.Add(); err != nil {
		return err
	}
	defer r.tg.Done()
	r.downloadHistoryMu.Lock()
	defer r.downloadHistoryMu.Unlock()

	// Check to confirm there are downloads to clear
	if len(r.downloadHistory) == 0 {
		return nil
	}

	// Timestamp validation
	if before.Before(after) {
		return errors.New("before timestamp can not be newer then after timestamp")
	}

	// Clear download history if both before and after timestamps are zero values
	if before.Equal(types.EndOfTime) && after.IsZero() {
		r.downloadHistory = r.downloadHistory[:0]
		return nil
	}

	// Find and return downloads that are not within the given range
	withinTimespan := func(t time.Time) bool {
		return (t.After(after) || t.Equal(after)) && (t.Before(before) || t.Equal(before))
	}
	filtered := r.downloadHistory[:0]
	for _, d := range r.downloadHistory {
		if !withinTimespan(d.staticStartTime) {
			filtered = append(filtered, d)
		}
	}
	r.downloadHistory = filtered
	return nil
}
