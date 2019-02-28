package renter

// TODO: Renter will try to download to repair a piece even if there are not
// enough workers to make any progress on the repair.  This should be fixed.

import (
	"container/heap"
	"io/ioutil"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gitlab.com/NebulousLabs/Sia/modules/renter/siafile"
	"gitlab.com/NebulousLabs/fastrand"

	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/types"
)

// repairTarget is a helper type for telling the repair heap what type of
// files/chunks to target for repair
type repairTarget int

// targetStuckChunks tells the repair loop to target stuck chunks for repair and
// targetUnstuckChunks tells the repair loop to target unstuck chunks for repair
const (
	targetError repairTarget = iota
	targetStuckChunks
	targetUnstuckChunks
)

// uploadHeap contains a priority-sorted heap of all the chunks being uploaded
// to the renter, along with some metadata.
type uploadHeap struct {
	heap uploadChunkHeap

	// heapChunks is a map containing all the chunks that are currently in the
	// heap. Chunks are added and removed from the map when chunks are pushed
	// and popped off the heap
	//
	// repairingChunks is a map containing all the chunks are that currently
	// assigned to workers and are being repaired/worked on.
	heapChunks      map[uploadChunkID]struct{}
	repairingChunks map[uploadChunkID]struct{}

	// Control channels
	newUploads        chan struct{}
	repairNeeded      chan struct{}
	stuckChunkFound   chan struct{}
	stuckChunkSuccess chan string

	mu sync.Mutex
}

// uploadChunkHeap is a bunch of priority-sorted chunks that need to be either
// uploaded or repaired.
type uploadChunkHeap []*unfinishedUploadChunk

// Implementation of heap.Interface for uploadChunkHeap.
func (uch uploadChunkHeap) Len() int { return len(uch) }
func (uch uploadChunkHeap) Less(i, j int) bool {
	// If both chunks are stuck, check which chunk has lower completion
	// percentage
	if uch[i].stuck && uch[j].stuck {
		return float64(uch[i].piecesCompleted)/float64(uch[i].piecesNeeded) < float64(uch[j].piecesCompleted)/float64(uch[j].piecesNeeded)
	}
	// If chunk i is stuck return true to prioritize it
	if uch[i].stuck {
		return true
	}
	// If chunk j is stuck return false to prioritize it
	if uch[j].stuck {
		return false
	}
	// If neither chunk is stuck, check which chunk has lower completion
	// percentage
	return float64(uch[i].piecesCompleted)/float64(uch[i].piecesNeeded) < float64(uch[j].piecesCompleted)/float64(uch[j].piecesNeeded)
}
func (uch uploadChunkHeap) Swap(i, j int)       { uch[i], uch[j] = uch[j], uch[i] }
func (uch *uploadChunkHeap) Push(x interface{}) { *uch = append(*uch, x.(*unfinishedUploadChunk)) }
func (uch *uploadChunkHeap) Pop() interface{} {
	old := *uch
	n := len(old)
	x := old[n-1]
	*uch = old[0 : n-1]
	return x
}

// managedLen will return the length of the heap
func (uh *uploadHeap) managedLen() int {
	uh.mu.Lock()
	defer uh.mu.Unlock()
	return uh.heap.Len()
}

// managedPush will add a chunk to the upload heap.
func (uh *uploadHeap) managedPush(uuc *unfinishedUploadChunk) {
	// Check whether this chunk is already being repaired. If not, add it to the
	// upload chunk heap.
	uh.mu.Lock()
	_, exists1 := uh.heapChunks[uuc.id]
	_, exists2 := uh.repairingChunks[uuc.id]
	if !exists1 && !exists2 {
		uh.heapChunks[uuc.id] = struct{}{}
		uh.heap.Push(uuc)
	}
	uh.mu.Unlock()
}

// managedPop will pull a chunk off of the upload heap and return it.
func (uh *uploadHeap) managedPop() (uc *unfinishedUploadChunk) {
	uh.mu.Lock()
	if len(uh.heap) > 0 {
		uc = heap.Pop(&uh.heap).(*unfinishedUploadChunk)
		delete(uh.heapChunks, uc.id)
	}
	uh.mu.Unlock()
	return uc
}

// buildUnfinishedChunks will pull all of the unfinished chunks out of a file.
//
// TODO / NOTE: This code can be substantially simplified once the files store
// the HostPubKey instead of the FileContractID, and can be simplified even
// further once the layout is per-chunk instead of per-filecontract.
func (r *Renter) buildUnfinishedChunks(entrys []*siafile.SiaFileSetEntry, hosts map[string]struct{}, target repairTarget) []*unfinishedUploadChunk {
	// Sanity check that there are entries
	if len(entrys) == 0 {
		r.log.Debugln("WARN: no entries passed into buildUnfinishedChunks")
		return nil
	}

	// If we don't have enough workers for the file, don't repair it right now.
	if len(r.workerPool) < entrys[0].ErasureCode().MinPieces() {
		r.log.Debugln("Not building any chunks from file as there are not enough workers")
		// Mark all chunks as stuck
		if err := entrys[0].MarkAllChunksAsStuck(); err != nil {
			r.log.Println("WARN: unable to mark all chunks as stuck:", err)
		}
		// Close all entrys
		for _, entry := range entrys {
			err := entry.Close()
			if err != nil {
				r.log.Println("WARN: Could not close file:", err)
			}
		}
		return nil
	}

	// Assemble chunk indexes, stuck Loop should only be adding stuck chunks and
	// the repair loop should only be adding unstuck chunks
	var chunkIndexes []int
	for i, entry := range entrys {
		if (target == targetStuckChunks) != entry.StuckChunkByIndex(uint64(i)) {
			// Close unneeded entrys
			err := entry.Close()
			if err != nil {
				r.log.Println("WARN: Could not close file:", err)
			}
			continue
		}
		chunkIndexes = append(chunkIndexes, i)
	}

	// Sanity check that we have chunk indices to go through
	if len(chunkIndexes) == 0 {
		r.log.Println("WARN: no chunk indices gathered, can't add chunks to heap")
		return nil
	}

	// Assemble the set of chunks.
	//
	// TODO / NOTE: Future files may have a different method for determining the
	// number of chunks. Changes will be made due to things like sparse files,
	// and the fact that chunks are going to be different sizes.
	newUnfinishedChunks := make([]*unfinishedUploadChunk, len(chunkIndexes))
	for i, index := range chunkIndexes {
		// Sanity check: fileUID should not be the empty value.
		if entrys[i].UID() == "" {
			build.Critical("empty string for file UID")
		}

		// Create unfinishedUploadChunk
		newUnfinishedChunks[i] = &unfinishedUploadChunk{
			fileEntry: entrys[i],

			id: uploadChunkID{
				fileUID: entrys[i].UID(),
				index:   uint64(index),
			},

			index:  uint64(index),
			length: entrys[i].ChunkSize(),
			offset: int64(uint64(index) * entrys[i].ChunkSize()),

			// memoryNeeded has to also include the logical data, and also
			// include the overhead for encryption.
			//
			// TODO / NOTE: If we adjust the file to have a flexible encryption
			// scheme, we'll need to adjust the overhead stuff too.
			//
			// TODO: Currently we request memory for all of the pieces as well
			// as the minimum pieces, but we perhaps don't need to request all
			// of that.
			memoryNeeded:  entrys[i].PieceSize()*uint64(entrys[i].ErasureCode().NumPieces()+entrys[i].ErasureCode().MinPieces()) + uint64(entrys[i].ErasureCode().NumPieces())*entrys[i].MasterKey().Type().Overhead(),
			minimumPieces: entrys[i].ErasureCode().MinPieces(),
			piecesNeeded:  entrys[i].ErasureCode().NumPieces(),
			stuck:         entrys[i].StuckChunkByIndex(uint64(index)),

			physicalChunkData: make([][]byte, entrys[i].ErasureCode().NumPieces()),

			pieceUsage:  make([]bool, entrys[i].ErasureCode().NumPieces()),
			unusedHosts: make(map[string]struct{}),
		}
		// Every chunk can have a different set of unused hosts.
		for host := range hosts {
			newUnfinishedChunks[i].unusedHosts[host] = struct{}{}
		}
	}

	// Build a map of host public keys.
	pks := make(map[string]types.SiaPublicKey)
	for _, pk := range entrys[0].HostPublicKeys() {
		pks[pk.String()] = pk
	}

	// Iterate through the pieces of all chunks of the file and mark which
	// hosts are already in use for a particular chunk. As you delete hosts
	// from the 'unusedHosts' map, also increment the 'piecesCompleted' value.
	for i, index := range chunkIndexes {
		pieces, err := entrys[0].Pieces(uint64(index))
		if err != nil {
			r.log.Println("failed to get pieces for building incomplete chunks")
			return nil
		}
		for pieceIndex, pieceSet := range pieces {
			for _, piece := range pieceSet {
				// Get the contract for the piece.
				pk, exists := pks[piece.HostPubKey.String()]
				if !exists {
					build.Critical("Couldn't find public key in map. This should never happen")
				}
				contractUtility, exists := r.hostContractor.ContractUtility(pk)
				if !exists {
					// File contract does not seem to be part of the host anymore.
					continue
				}
				if !contractUtility.GoodForRenew {
					// We are no longer renewing with this contract, so it does not
					// count for redundancy.
					continue
				}

				// Mark the chunk set based on the pieces in this contract.
				_, exists = newUnfinishedChunks[i].unusedHosts[pk.String()]
				redundantPiece := newUnfinishedChunks[i].pieceUsage[pieceIndex]
				if exists && !redundantPiece {
					newUnfinishedChunks[i].pieceUsage[pieceIndex] = true
					newUnfinishedChunks[i].piecesCompleted++
					delete(newUnfinishedChunks[i].unusedHosts, pk.String())
				} else if exists {
					// This host has a piece, but it is the same piece another
					// host has. We should still remove the host from the
					// unusedHosts since one host having multiple pieces of a
					// chunk might lead to unexpected issues. e.g. if a host
					// has multiple pieces and another host with redundant
					// pieces goes offline, we end up with false redundancy
					// reporting.
					delete(newUnfinishedChunks[i].unusedHosts, pk.String())
				}
			}
		}
	}

	// Iterate through the set of newUnfinishedChunks and remove any that are
	// completed.
	incompleteChunks := newUnfinishedChunks[:0]
	for i := 0; i < len(newUnfinishedChunks); i++ {
		if newUnfinishedChunks[i].piecesCompleted < newUnfinishedChunks[i].piecesNeeded {
			incompleteChunks = append(incompleteChunks, newUnfinishedChunks[i])
			continue
		}
		// Close entry of completed chunk
		err := newUnfinishedChunks[i].fileEntry.Close()
		if err != nil {
			r.log.Println("WARN: could not close file:", err)
		}
	}
	// TODO: Don't return chunks that can't be downloaded, uploaded or otherwise
	// helped by the upload process. These chunks should be marked as stuck
	return incompleteChunks
}

// managedBuildAndPushRandomChunk randomly selects a file and builds the
// unfinished chunks, then randomly adds one chunk to the upload heap
func (r *Renter) managedBuildAndPushRandomChunk(files []*siafile.SiaFileSetEntry, hosts map[string]struct{}, target repairTarget) {
	// Sanity check that there are files
	if len(files) == 0 {
		return
	}
	// Grab a random file
	randFileIndex := fastrand.Intn(len(files))
	file := files[randFileIndex]
	id := r.mu.Lock()
	// Build the unfinished stuck chunks from the file
	unfinishedUploadChunks := r.buildUnfinishedChunks(file.CopyEntry(int(file.NumChunks())), hosts, target)
	r.mu.Unlock(id)
	// Sanity check that there are stuck chunks
	if len(unfinishedUploadChunks) == 0 {
		r.log.Println("WARN: no stuck unfinishedUploadChunks returned from buildUnfinishedChunks, so no stuck chunks will be added to the heap")
		return
	}
	// Add a random stuck chunk to the upload heap and set its stuckRepair field
	// to true
	randChunkIndex := fastrand.Intn(len(unfinishedUploadChunks))
	randChunk := unfinishedUploadChunks[randChunkIndex]
	randChunk.stuckRepair = true
	r.uploadHeap.managedPush(randChunk)
	return
}

// managedBuildAndPushChunks builds the unfinished upload chunks and adds them
// to the upload heap
func (r *Renter) managedBuildAndPushChunks(files []*siafile.SiaFileSetEntry, hosts map[string]struct{}, target repairTarget) {
	// Loop through the whole set of files and get a list of chunks to add to
	// the heap.
	for _, file := range files {
		id := r.mu.Lock()
		unfinishedUploadChunks := r.buildUnfinishedChunks(file.CopyEntry(int(file.NumChunks())), hosts, target)
		r.mu.Unlock(id)
		if len(unfinishedUploadChunks) == 0 {
			r.log.Println("No unfinishedUploadChunks returned from buildUnfinishedChunks, so no chunks will be added to the heap")
			return
		}
		for i := 0; i < len(unfinishedUploadChunks); i++ {
			r.uploadHeap.managedPush(unfinishedUploadChunks[i])
		}
	}
}

// managedBuildChunkHeap will iterate through all of the files in the renter and
// construct a chunk heap.
func (r *Renter) managedBuildChunkHeap(dirSiaPath string, hosts map[string]struct{}, target repairTarget) {
	// Get Directory files
	var files []*siafile.SiaFileSetEntry
	fileinfos, err := ioutil.ReadDir(filepath.Join(r.staticFilesDir, dirSiaPath))
	if err != nil {
		return
	}
	for _, fi := range fileinfos {
		// skip sub directories and non siafiles
		ext := filepath.Ext(fi.Name())
		if fi.IsDir() || ext != siafile.ShareExtension {
			continue
		}

		// Open SiaFile
		siaPath := filepath.Join(dirSiaPath, strings.TrimSuffix(fi.Name(), ext))
		file, err := r.staticFileSet.Open(siaPath)
		if err != nil {
			return
		}

		// For stuck chunk repairs, check to see if file has stuck chunks
		if target == targetStuckChunks && file.NumStuckChunks() == 0 {
			// Close unneeded files
			err := file.Close()
			if err != nil {
				r.log.Println("WARN: Could not close file:", err)
			}
			continue
		}

		// For normal repairs, ignore files that have been recently repaired
		if target == targetUnstuckChunks && time.Since(file.RecentRepairTime()) < fileRepairInterval {
			// Close unneeded files
			err := file.Close()
			if err != nil {
				r.log.Println("WARN: Could not close file:", err)
			}
			continue
		}
		files = append(files, file)
	}

	// Check if any files were selected from directory
	if len(files) == 0 {
		r.log.Println("No files pulled from", dirSiaPath, "to build the repair heap")
		return
	}

	// Build the unfinished upload chunks and add them to the upload heap
	switch target {
	case targetStuckChunks:
		r.log.Debugln("Adding stuck chunk to heap")
		r.managedBuildAndPushRandomChunk(files, hosts, target)
	case targetUnstuckChunks:
		r.log.Debugln("Adding chunks to heap")
		r.managedBuildAndPushChunks(files, hosts, target)
	default:
		r.log.Println("WARN: repair target not recognized", target)
	}

	// Close all files
	for _, file := range files {
		err := file.Close()
		if err != nil {
			r.log.Println("WARN: Could not close file:", err)
		}
	}
}

// managedPrepareNextChunk takes the next chunk from the chunk heap and prepares
// it for upload. Preparation includes blocking until enough memory is
// available, fetching the logical data for the chunk (either from the disk or
// from the network), erasure coding the logical data into the physical data,
// and then finally passing the work onto the workers.
func (r *Renter) managedPrepareNextChunk(uuc *unfinishedUploadChunk, hosts map[string]struct{}) {
	// Grab the next chunk, loop until we have enough memory, update the amount
	// of memory available, and then spin up a thread to asynchronously handle
	// the rest of the chunk tasks.
	if !r.memoryManager.Request(uuc.memoryNeeded, memoryPriorityLow) {
		return
	}
	// Fetch the chunk in a separate goroutine, as it can take a long time and
	// does not need to bottleneck the repair loop.
	go r.threadedFetchAndRepairChunk(uuc)
}

// managedRefreshHostsAndWorkers will reset the set of hosts and the set of
// workers for the renter.
func (r *Renter) managedRefreshHostsAndWorkers() map[string]struct{} {
	// Grab the current set of contracts and use them to build a list of hosts
	// that are currently active. The hosts are assembled into a map where the
	// key is the String() representation of the host's SiaPublicKey.
	//
	// TODO / NOTE: This code can be removed once files store the HostPubKey
	// of the hosts they are using, instead of just the FileContractID.
	currentContracts := r.hostContractor.Contracts()
	hosts := make(map[string]struct{})
	for _, contract := range currentContracts {
		hosts[contract.HostPublicKey.String()] = struct{}{}
	}
	// Refresh the worker pool as well.
	r.managedUpdateWorkerPool()
	return hosts
}

// managedRepairLoop works through the upload heap repairing chunks. The repair
// loop will continue until the renter stops, there are no more chunks, or
// enough time has passed indicated by the rebuildHeapSignal
func (r *Renter) managedRepairLoop(hosts map[string]struct{}) {
	var consecutiveChunkRepairs int
	rebuildHeapSignal := time.After(rebuildChunkHeapInterval)
	for {
		select {
		case <-r.tg.StopChan():
			// Return if the renter has shut down.
			return
		case <-rebuildHeapSignal:
			// Return if workers/heap need to be refreshed.
			return
		default:
		}

		// Return if not online.
		if !r.g.Online() {
			return
		}

		// Check if there is work by trying to pop of the next chunk from
		// the heap.
		nextChunk := r.uploadHeap.managedPop()
		if nextChunk == nil {
			return
		}

		// Check if file is reasonably healthy
		health, _, _ := nextChunk.fileEntry.CachedHealth()
		if health < 0.8 {
			// File is reasonably healthy so update the recent repair time for
			// the file
			err := nextChunk.fileEntry.UpdateRecentRepairTime()
			if err != nil {
				r.log.Printf("WARN: unable to update the recent repair time of %v : %v", nextChunk.fileEntry.SiaPath(), err)
			}
		}

		// Make sure we have enough workers for this chunk to reach minimum
		// redundancy. Otherwise we ignore this chunk for now, mark it as stuck
		// and let the stuck loop work on it
		id := r.mu.RLock()
		availableWorkers := len(r.workerPool)
		r.mu.RUnlock(id)
		if availableWorkers < nextChunk.minimumPieces {
			err := nextChunk.fileEntry.SetStuck(nextChunk.index, true)
			if err != nil {
				r.log.Println("WARN: unable to set chunk as stuck:", err)
			}
			go r.threadedBubbleMetadata(nextChunk.fileEntry.DirSiaPath())
			continue
		}

		// Perform the work. managedPrepareNextChunk will block until
		// enough memory is available to perform the work, slowing this
		// thread down to using only the resources that are available.
		r.managedPrepareNextChunk(nextChunk, hosts)
		consecutiveChunkRepairs++

		// Check if enough chunks are currently being repaired
		if consecutiveChunkRepairs >= maxConsecutiveChunkRepairs {
			// Pull all of the chunks out of the heap and return. Save the stuck
			// chunks, as this is the repair loop and we aren't trying to erase
			// the stuck chunks.
			var stuckChunks []*unfinishedUploadChunk
			for r.uploadHeap.managedLen() > 0 {
				if c := r.uploadHeap.managedPop(); c.stuck {
					stuckChunks = append(stuckChunks, c)
				}
			}
			for _, sc := range stuckChunks {
				r.uploadHeap.managedPush(sc)
			}
			return
		}
	}
}

// threadedUploadLoop is a background thread that checks on the health of files,
// tracking the least healthy files and queuing the worst ones for repair.
func (r *Renter) threadedUploadLoop() {
	err := r.tg.Add()
	if err != nil {
		return
	}
	defer r.tg.Done()

	for {
		select {
		case <-r.tg.StopChan():
			// Return if the renter has shut down.
			return
		default:
		}

		// Wait until the renter is online to proceed.
		if !r.managedBlockUntilOnline() {
			// The renter shut down before the internet connection was restored.
			return
		}

		// Find Directory that needs to be repaired
		dirSiaPath, dirHealth, err := r.managedWorstHealthDirectory()
		if err != nil {
			r.log.Println("WARN: getting worst health directory:", err)
			return
		}

		// Check if directory requires repairing. We only want to repair
		// directories with a health worse than the repairHealthThreshold to
		// save resources
		if dirHealth < RemoteRepairDownloadThreshold {
			// Block until new work is required.
			select {
			case <-r.uploadHeap.newUploads:
				// User has uploaded a new file.
			case <-r.uploadHeap.repairNeeded:
				// Health loop found a file in need of repair
			case <-r.tg.StopChan():
				// The renter has shut down.
				return
			}
			continue
		}

		// Refresh the worker pool and get the set of hosts that are currently
		// useful for uploading.
		hosts := r.managedRefreshHostsAndWorkers()

		// Build a min-heap of chunks organized by upload progress.
		r.managedBuildChunkHeap(dirSiaPath, hosts, targetUnstuckChunks)
		r.uploadHeap.mu.Lock()
		heapLen := r.uploadHeap.heap.Len()
		r.uploadHeap.mu.Unlock()
		if heapLen == 0 {
			r.log.Printf("No chunks added to the heap for repair from `%v` even through health was %v", dirSiaPath, dirHealth)
			// Call threadedBubble to make sure that directory information is
			// accurate
			r.threadedBubbleMetadata(dirSiaPath)
			continue
		}
		r.log.Println("Repairing", heapLen, "chunks from", dirSiaPath)

		// Work through the heap and repair files
		r.managedRepairLoop(hosts)

		// Once we have worked through the heap, call bubble to update the
		// directory metadata
		r.threadedBubbleMetadata(dirSiaPath)
	}
}
