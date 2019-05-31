package siafile

import (
	"os"

	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/errors"
)

type (
	// Snapshot is a snapshot of a SiaFile. A snapshot is a deep-copy and
	// can be accessed without locking at the cost of being a frozen readonly
	// representation of a siafile which only exists in memory.
	Snapshot struct {
		staticChunks              []Chunk
		staticFileSize            int64
		staticPieceSize           uint64
		staticErasureCode         modules.ErasureCoder
		staticMasterKey           crypto.CipherKey
		staticMode                os.FileMode
		staticPubKeyTable         []HostPublicKey
		staticSiaPath             modules.SiaPath
		staticCombinedChunkStatus uint8
		staticUID                 SiafileUID
	}
)

// SnapshotReader is a helper type that allows reading a raw SiaFile from disk
// while keeping the file in memory locked.
type SnapshotReader struct {
	f  *os.File
	sf *SiaFile
}

// Close closes the underlying file.
func (sfr *SnapshotReader) Close() error {
	sfr.sf.mu.RUnlock()
	return sfr.f.Close()
}

// Read calls Read on the underlying file.
func (sfr *SnapshotReader) Read(b []byte) (int, error) {
	return sfr.f.Read(b)
}

// Stat returns the FileInfo of the underlying file.
func (sfr *SnapshotReader) Stat() (os.FileInfo, error) {
	return sfr.f.Stat()
}

// SnapshotReader creates a io.ReadCloser that can be used to read the raw
// Siafile from disk.
func (sf *SiaFile) SnapshotReader() (*SnapshotReader, error) {
	// Lock the file.
	sf.mu.RLock()
	if sf.deleted {
		sf.mu.RUnlock()
		return nil, errors.New("can't copy deleted SiaFile")
	}
	// Open file.
	f, err := os.Open(sf.siaFilePath)
	if err != nil {
		sf.mu.RUnlock()
		return nil, err
	}
	return &SnapshotReader{
		sf: sf,
		f:  f,
	}, nil
}

// ChunkIndexByOffset will return the chunkIndex that contains the provided
// offset of a file and also the relative offset within the chunk. If the
// offset is out of bounds, chunkIndex will be equal to NumChunk().
func (s *Snapshot) ChunkIndexByOffset(offset uint64) (chunkIndex uint64, off uint64) {
	chunkIndex = offset / s.ChunkSize()
	off = offset % s.ChunkSize()
	return
}

// ChunkSize returns the size of a single chunk of the file.
func (s *Snapshot) ChunkSize() uint64 {
	return s.staticPieceSize * uint64(s.staticErasureCode.MinPieces())
}

// CombinedChunkStatus returns the combined chunk status of the file.
func (s *Snapshot) CombinedChunkStatus() uint8 {
	return s.staticCombinedChunkStatus
}

// ErasureCode returns the erasure coder used by the file.
func (s *Snapshot) ErasureCode() modules.ErasureCoder {
	return s.staticErasureCode
}

// MasterKey returns the masterkey used to encrypt the file.
func (s *Snapshot) MasterKey() crypto.CipherKey {
	return s.staticMasterKey
}

// Mode returns the FileMode of the file.
func (s *Snapshot) Mode() os.FileMode {
	return s.staticMode
}

// NumChunks returns the number of chunks the file consists of. This will
// return the number of chunks the file consists of even if the file is not
// fully uploaded yet.
func (s *Snapshot) NumChunks() uint64 {
	return uint64(len(s.staticChunks))
}

// Pieces returns all the pieces for a chunk in a slice of slices that contains
// all the pieces for a certain index.
func (s *Snapshot) Pieces(chunkIndex uint64) [][]Piece {
	// Return the pieces. Since the snapshot is meant to be used read-only, we
	// don't have to return a deep-copy here.
	return s.staticChunks[chunkIndex].Pieces
}

// PieceSize returns the size of a single piece of the file.
func (s *Snapshot) PieceSize() uint64 {
	return s.staticPieceSize
}

// SiaPath returns the SiaPath of the file.
func (s *Snapshot) SiaPath() modules.SiaPath {
	return s.staticSiaPath
}

// Size returns the size of the file.
func (s *Snapshot) Size() uint64 {
	return uint64(s.staticFileSize)
}

// UID returns the UID of the file.
func (s *Snapshot) UID() SiafileUID {
	return s.staticUID
}

// Snapshot creates a snapshot of the SiaFile.
func (sf *siaFileSetEntry) Snapshot() (*Snapshot, error) {
	mk := sf.MasterKey()
	sf.mu.RLock()

	// Copy PubKeyTable.
	pkt := make([]HostPublicKey, len(sf.pubKeyTable))
	copy(pkt, sf.pubKeyTable)

	chunks := make([]Chunk, 0, sf.numChunks())
	// Figure out how much memory we need to allocate for the piece sets and
	// pieces.
	allChunks := sf.allChunks()
	var numPieceSets, numPieces int
	for chunkIndex := range allChunks {
		numPieceSets += len(allChunks[chunkIndex].Pieces)
		for pieceIndex := range allChunks[chunkIndex].Pieces {
			numPieces += len(allChunks[chunkIndex].Pieces[pieceIndex])
		}
	}
	// Allocate all the piece sets and pieces at once.
	allPieceSets := make([][]Piece, numPieceSets)
	allPieces := make([]Piece, numPieces)

	// Copy fullChunks. Partial chunk will be handled later.
	for chunkIndex := range sf.fullChunks {
		pieces := allPieceSets[:len(allChunks[chunkIndex].Pieces)]
		allPieceSets = allPieceSets[len(allChunks[chunkIndex].Pieces):]
		for pieceIndex := range pieces {
			pieces[pieceIndex] = allPieces[:len(allChunks[chunkIndex].Pieces[pieceIndex])]
			allPieces = allPieces[len(allChunks[chunkIndex].Pieces[pieceIndex]):]
			for i, piece := range allChunks[chunkIndex].Pieces[pieceIndex] {
				pieces[pieceIndex][i] = Piece{
					HostPubKey: sf.pubKeyTable[piece.HostTableOffset].PublicKey,
					MerkleRoot: piece.MerkleRoot,
				}
			}
		}
		chunks = append(chunks, Chunk{
			Pieces: pieces,
		})
	}
	// Handle potential partial chunk.
	if sf.staticMetadata.CombinedChunkStatus > CombinedChunkStatusIncomplete {
		partialChunkPieces, err := sf.partialsSiaFile.Pieces(sf.staticMetadata.CombinedChunkIndex)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, Chunk{
			Pieces: partialChunkPieces,
		})
	} else if sf.staticMetadata.CombinedChunkStatus > CombinedChunkStatusNoChunk {
		// Add empty chunk
		chunks = append(chunks, Chunk{
			Pieces: make([][]Piece, sf.staticMetadata.staticErasureCode.NumPieces()),
		})
	}
	// Get non-static metadata fields under lock.
	fileSize := sf.staticMetadata.FileSize
	mode := sf.staticMetadata.Mode
	sf.mu.RUnlock()

	sf.staticSiaFileSet.mu.Lock()
	sp := sf.staticSiaFileSet.siaPath(sf)
	sf.staticSiaFileSet.mu.Unlock()

	return &Snapshot{
		staticChunks:              chunks,
		staticCombinedChunkStatus: sf.staticMetadata.CombinedChunkStatus,
		staticFileSize:            fileSize,
		staticPieceSize:           sf.staticMetadata.StaticPieceSize,
		staticErasureCode:         sf.staticMetadata.staticErasureCode,
		staticMasterKey:           mk,
		staticMode:                mode,
		staticPubKeyTable:         pkt,
		staticSiaPath:             sp,
		staticUID:                 sf.staticMetadata.StaticUniqueID,
	}, nil
}
