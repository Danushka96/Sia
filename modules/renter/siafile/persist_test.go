package siafile

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/types"
	"gitlab.com/NebulousLabs/errors"
	"gitlab.com/NebulousLabs/fastrand"
	"gitlab.com/NebulousLabs/writeaheadlog"
)

// equalFiles is a helper that compares two SiaFiles for equality.
func equalFiles(sf, sf2 *SiaFile) error {
	// Backup the metadata structs for both files.
	md := sf.staticMetadata
	md2 := sf2.staticMetadata
	// Compare the timestamps first since they can't be compared with
	// DeepEqual.
	if sf.staticMetadata.AccessTime.Unix() != sf2.staticMetadata.AccessTime.Unix() {
		return errors.New("AccessTime's don't match")
	}
	if sf.staticMetadata.ChangeTime.Unix() != sf2.staticMetadata.ChangeTime.Unix() {
		return errors.New("ChangeTime's don't match")
	}
	if sf.staticMetadata.CreateTime.Unix() != sf2.staticMetadata.CreateTime.Unix() {
		return errors.New("CreateTime's don't match")
	}
	if sf.staticMetadata.ModTime.Unix() != sf2.staticMetadata.ModTime.Unix() {
		return errors.New("ModTime's don't match")
	}
	if sf.staticMetadata.LastHealthCheckTime.Unix() != sf2.staticMetadata.LastHealthCheckTime.Unix() {
		return errors.New("LastHealthCheckTime's don't match")
	}
	// Set the timestamps to zero for DeepEqual.
	sf.staticMetadata.AccessTime = time.Time{}
	sf.staticMetadata.ChangeTime = time.Time{}
	sf.staticMetadata.CreateTime = time.Time{}
	sf.staticMetadata.ModTime = time.Time{}
	sf.staticMetadata.LastHealthCheckTime = time.Time{}
	sf2.staticMetadata.AccessTime = time.Time{}
	sf2.staticMetadata.ChangeTime = time.Time{}
	sf2.staticMetadata.CreateTime = time.Time{}
	sf2.staticMetadata.ModTime = time.Time{}
	sf2.staticMetadata.LastHealthCheckTime = time.Time{}
	// Compare the rest of sf and sf2.
	if !reflect.DeepEqual(sf.staticMetadata, sf2.staticMetadata) {
		fmt.Println(sf.staticMetadata)
		fmt.Println(sf2.staticMetadata)
		return errors.New("sf metadata doesn't equal sf2 metadata")
	}
	if !reflect.DeepEqual(sf.pubKeyTable, sf2.pubKeyTable) {
		fmt.Println(sf.pubKeyTable)
		fmt.Println(sf2.pubKeyTable)
		return errors.New("sf pubKeyTable doesn't equal sf2 pubKeyTable")
	}
	if !reflect.DeepEqual(sf.allChunks(), sf2.allChunks()) {
		fmt.Println(sf.numChunks(), sf2.numChunks())
		return errors.New("sf chunks don't equal sf2 chunks")
	}
	if sf.siaFilePath != sf2.siaFilePath {
		return fmt.Errorf("sf2 filepath was %v but should be %v",
			sf2.siaFilePath, sf.siaFilePath)
	}
	// Restore the original metadata.
	sf.staticMetadata = md
	sf2.staticMetadata = md2
	return nil
}

// addRandomHostKeys adds n random host keys to the SiaFile's pubKeyTable. It
// doesn't write them to disk.
func (sf *SiaFile) addRandomHostKeys(n int) {
	for i := 0; i < n; i++ {
		// Create random specifier and key.
		algorithm := types.Specifier{}
		fastrand.Read(algorithm[:])

		// Create random key.
		key := fastrand.Bytes(32)

		// Append new key to slice.
		sf.pubKeyTable = append(sf.pubKeyTable, HostPublicKey{
			PublicKey: types.SiaPublicKey{
				Algorithm: algorithm,
				Key:       key,
			},
			Used: true,
		})
	}
}

// customTestFileAndWAL creates an empty SiaFile for testing and also returns
// the WAL used in the creation and the path of the WAL.
func customTestFileAndWAL(siaFilePath, source string, rc modules.ErasureCoder, sk crypto.CipherKey, fileSize uint64, numChunks int, fileMode os.FileMode) (*SiaFile, *writeaheadlog.WAL, string) {
	// Create the path to the file.
	dir, _ := filepath.Split(siaFilePath)
	err := os.MkdirAll(dir, 0700)
	if err != nil {
		panic(err)
	}
	// Create a test wal
	wal, walPath := newTestWAL()
	// Create the corresponding partials file if it doesn't exist already.
	var partialsSiaFile *SiaFile
	partialsSiaPath := modules.CombinedSiaFilePath(rc)
	partialsSiaFilePath := partialsSiaPath.SiaPartialsFileSysPath(dir)
	if _, err = os.Stat(partialsSiaFilePath); os.IsNotExist(err) {
		partialsSiaFile, err = New(partialsSiaFilePath, "", wal, rc, sk, 0, fileMode, nil, false)
	} else {
		partialsSiaFile, err = LoadSiaFile(partialsSiaFilePath, wal)
	}
	if err != nil {
		panic(fmt.Sprint("failed to load partialsSiaFile", err))
	}
	// Check that the partials file is sane.
	if partialsSiaFile.numChunks() > 0 {
		panic(fmt.Sprint("partialsSiaFile shouldn't have any chunks but had ", partialsSiaFile.numChunks()))
	}
	partialsEntry := &SiaFileSetEntry{
		dummyEntry(partialsSiaFile),
		uint64(fastrand.Intn(math.MaxInt32)),
	}
	// Create the file.
	sf, err := New(siaFilePath, source, wal, rc, sk, fileSize, fileMode, partialsEntry, false)
	if err != nil {
		panic(err)
	}
	// Check that the number of chunks in the file is correct.
	if numChunks >= 0 && sf.numChunks() != uint64(numChunks) {
		panic(fmt.Sprint("newTestFile didn't create the expected number of chunks: ", sf.numChunks()))
	}
	return sf, wal, walPath
}

// newBlankTestFileAndWAL is like customTestFileAndWAL but uses random params
// and allows the caller to specify how many chunks the file should at least
// contain.
func newBlankTestFileAndWAL(minChunks int) (*SiaFile, *writeaheadlog.WAL, string) {
	siaFilePath, _, source, rc, sk, fileSize, numChunks, fileMode := newTestFileParams(1, true)
	return customTestFileAndWAL(siaFilePath, source, rc, sk, fileSize, numChunks, fileMode)
}

// newBlankTestFileAndWALWithEC is like customTestFileAndWAL but let's the
// caller specify custom erasure code settings.
func newBlankTestFileAndWALWithEC(ec modules.ErasureCoder) (*SiaFile, *writeaheadlog.WAL, string) {
	siaFilePath, _, source, rc, sk, fileSize, numChunks, fileMode := newTestFileParams(1, true)
	return customTestFileAndWAL(siaFilePath, source, rc, sk, fileSize, numChunks, fileMode)
}

// newBlankTestFile is a helper method to create a SiaFile for testing without
// any hosts or uploaded pieces.
func newBlankTestFile() *SiaFile {
	sf, _, _ := newBlankTestFileAndWAL(1)
	return sf
}

// newTestFile creates a SiaFile for testing where each chunk has a random
// number of pieces.
func newTestFile() *SiaFile {
	sf := newBlankTestFile()
	if err := setCombinedChunkOfTestFile(sf); err != nil {
		panic(err)
	}
	// Add pieces to each chunk.
	for chunkIndex := range sf.allChunks() {
		for pieceIndex := 0; pieceIndex < sf.ErasureCode().NumPieces(); pieceIndex++ {
			numPieces := fastrand.Intn(3) // up to 2 hosts for each piece
			for i := 0; i < numPieces; i++ {
				pk := types.SiaPublicKey{Key: fastrand.Bytes(crypto.EntropySize)}
				mr := crypto.Hash{}
				fastrand.Read(mr[:])
				if err := sf.AddPiece(pk, uint64(chunkIndex), uint64(pieceIndex), mr); err != nil {
					panic(err)
				}
			}
		}
	}
	return sf
}

// newTestFileParams creates the required parameters for creating a siafile and
// creates a directory for the file
func newTestFileParams(minChunks int, partialChunk bool) (string, modules.SiaPath, string, modules.ErasureCoder, crypto.CipherKey, uint64, int, os.FileMode) {
	// Create arguments for new file.
	sk := crypto.GenerateSiaKey(crypto.TypeDefaultRenter)
	pieceSize := modules.SectorSize - sk.Type().Overhead()
	siaPath := modules.RandomSiaPath()
	rc, err := NewRSCode(10, 20)
	if err != nil {
		panic(err)
	}
	numChunks := fastrand.Intn(10) + minChunks
	chunkSize := pieceSize * uint64(rc.MinPieces())
	fileSize := chunkSize * uint64(numChunks)
	if partialChunk {
		fileSize-- // force partial chunk
	}
	fileMode := os.FileMode(777)
	source := string(hex.EncodeToString(fastrand.Bytes(8)))

	// Create the path to the file.
	siaFilePath := siaPath.SiaFileSysPath(filepath.Join(os.TempDir(), "siafiles", hex.EncodeToString(fastrand.Bytes(16))))
	dir, _ := filepath.Split(siaFilePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		panic(err)
	}
	return siaFilePath, siaPath, source, rc, sk, fileSize, numChunks, fileMode
}

// newTestWal is a helper method to create a WAL for testing.
func newTestWAL() (*writeaheadlog.WAL, string) {
	// Create the wal.
	walsDir := filepath.Join(os.TempDir(), "wals")
	if err := os.MkdirAll(walsDir, 0700); err != nil {
		panic(err)
	}
	walFilePath := filepath.Join(walsDir, hex.EncodeToString(fastrand.Bytes(8)))
	_, wal, err := writeaheadlog.New(walFilePath)
	if err != nil {
		panic(err)
	}
	return wal, walFilePath
}

// TestNewFile tests that a new file has the correct contents and size and that
// loading it from disk also works.
func TestNewFile(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()
	sf := newTestFile()

	// Check that StaticPagesPerChunk was set correctly.
	if sf.staticMetadata.StaticPagesPerChunk != numChunkPagesRequired(sf.staticMetadata.staticErasureCode.NumPieces()) {
		t.Fatal("StaticPagesPerChunk wasn't set correctly")
	}

	// Marshal the metadata.
	md, err := marshalMetadata(sf.staticMetadata)
	if err != nil {
		t.Fatal(err)
	}
	// Marshal the pubKeyTable.
	pkt, err := marshalPubKeyTable(sf.pubKeyTable)
	if err != nil {
		t.Fatal(err)
	}
	// Marshal the chunks.
	var chunks [][]byte
	for _, chunk := range sf.fullChunks {
		c := marshalChunk(chunk)
		chunks = append(chunks, c)
	}

	// Save the SiaFile to make sure cached fields are persisted too.
	if err := sf.saveFile(); err != nil {
		t.Fatal(err)
	}

	// Open the file.
	f, err := os.OpenFile(sf.siaFilePath, os.O_RDWR, 777)
	if err != nil {
		t.Fatal("Failed to open file", err)
	}
	defer f.Close()
	// Check the filesize. It should be equal to the offset of the last chunk
	// on disk + its marshaled length.
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != sf.chunkOffset(len(sf.fullChunks)-1)+int64(len(chunks[len(chunks)-1])) {
		t.Fatal("file doesn't have right size")
	}
	// Compare the metadata to the on-disk metadata.
	readMD := make([]byte, len(md))
	_, err = f.ReadAt(readMD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readMD, md) {
		t.Fatal("metadata doesn't equal on-disk metadata")
	}
	// Compare the pubKeyTable to the on-disk pubKeyTable.
	readPKT := make([]byte, len(pkt))
	_, err = f.ReadAt(readPKT, sf.staticMetadata.PubKeyTableOffset)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readPKT, pkt) {
		t.Fatal("pubKeyTable doesn't equal on-disk pubKeyTable")
	}
	// Compare the chunks to the on-disk chunks one-by-one.
	readChunk := make([]byte, int(sf.staticMetadata.StaticPagesPerChunk)*pageSize)
	for chunkIndex := range sf.fullChunks {
		_, err := f.ReadAt(readChunk, sf.chunkOffset(chunkIndex))
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if !bytes.Equal(readChunk[:len(chunks[chunkIndex])], chunks[chunkIndex]) {
			t.Fatal("readChunks don't equal on-disk readChunks")
		}
	}
	// Load the file from disk and check that they are the same.
	sf2, err := LoadSiaFile(sf.siaFilePath, sf.wal)
	if err != nil {
		t.Fatal("failed to load SiaFile from disk", err)
	}
	// Compare the files.
	if err := equalFiles(sf, sf2); err != nil {
		t.Fatal(err)
	}
}

// TestCreateReadInsertUpdate tests if an update can be created using createInsertUpdate
// and if the created update can be read using readInsertUpdate.
func TestCreateReadInsertUpdate(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	sf := newTestFile()
	// Create update randomly
	index := int64(fastrand.Intn(100))
	data := fastrand.Bytes(10)
	update := sf.createInsertUpdate(index, data)
	// Read update
	readPath, readIndex, readData, err := readInsertUpdate(update)
	if err != nil {
		t.Fatal("Failed to read update", err)
	}
	// Compare values
	if readPath != sf.siaFilePath {
		t.Error("paths doesn't match")
	}
	if readIndex != index {
		t.Error("index doesn't match")
	}
	if !bytes.Equal(readData, data) {
		t.Error("data doesn't match")
	}
}

// TestCreateReadDeleteUpdate tests if an update can be created using
// createDeleteUpdate and if the created update can be read using
// readDeleteUpdate.
func TestCreateReadDeleteUpdate(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	sf := newTestFile()
	update := sf.createDeleteUpdate()
	// Read update
	path := readDeleteUpdate(update)
	// Compare values
	if path != sf.siaFilePath {
		t.Error("paths doesn't match")
	}
}

// TestDelete tests if deleting a siafile removes the file from disk and sets
// the deleted flag correctly.
func TestDelete(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create SiaFileSet with SiaFile
	entry, _, _ := newTestSiaFileSetWithFile()
	// Delete file.
	if err := entry.Delete(); err != nil {
		t.Fatal("Failed to delete file", err)
	}
	// Check if file was deleted and if deleted flag was set.
	if !entry.Deleted() {
		t.Fatal("Deleted flag was not set correctly")
	}
	if _, err := os.Open(entry.siaFilePath); !os.IsNotExist(err) {
		t.Fatal("Expected a file doesn't exist error but got", err)
	}
}

// TestRename tests if renaming a siafile moves the file correctly and also
// updates the metadata.
func TestRename(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create SiaFileSet with SiaFile
	entry, sfs, _ := newTestSiaFileSetWithFile()

	// Create new paths for the file.
	sfs.mu.Lock()
	oldSiaPathStr := sfs.siaPath(entry.siaFileSetEntry).String()
	sfs.mu.Unlock()
	newSiaPath, err := modules.NewSiaPath(oldSiaPathStr + "1")
	if err != nil {
		t.Fatal(err)
	}
	newSiaFilePath := newSiaPath.SiaFileSysPath(sfs.staticSiaFileDir)
	oldSiaFilePath := entry.siaFilePath

	// Rename file
	if err := entry.Rename(newSiaFilePath); err != nil {
		t.Fatal("Failed to rename file", err)
	}

	// Check if the file was moved.
	if _, err := os.Open(oldSiaFilePath); !os.IsNotExist(err) {
		t.Fatal("Expected a file doesn't exist error but got", err)
	}
	f, err := os.Open(newSiaFilePath)
	if err != nil {
		t.Fatal("Failed to open file at new location", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// Check the metadata.
	if entry.siaFilePath != newSiaFilePath {
		t.Fatal("SiaFilePath wasn't updated correctly")
	}
	sfs.mu.Lock()
	siaPath := sfs.siaPath(entry.siaFileSetEntry)
	if !siaPath.Equals(newSiaPath) {
		t.Fatal("SiaPath wasn't updated correctly", siaPath, newSiaPath)
	}
	sfs.mu.Unlock()
}

// TestApplyUpdates tests a variety of functions that are used to apply
// updates.
func TestApplyUpdates(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	t.Run("TestApplyUpdates", func(t *testing.T) {
		siaFile := newTestFile()
		testApply(t, siaFile, ApplyUpdates)
	})
	t.Run("TestSiaFileApplyUpdates", func(t *testing.T) {
		siaFile := newTestFile()
		testApply(t, siaFile, siaFile.applyUpdates)
	})
	t.Run("TestCreateAndApplyTransaction", func(t *testing.T) {
		siaFile := newTestFile()
		testApply(t, siaFile, siaFile.createAndApplyTransaction)
	})
}

// TestZeroByteFileCompat checks that 0-byte siafiles that have been uploaded
// before caching was introduced have the correct cached values after being
// loaded.
func TestZeroByteFileCompat(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create the file.
	siaFilePath, _, source, rc, sk, _, _, fileMode := newTestFileParams(1, true)
	sf, wal, _ := customTestFileAndWAL(siaFilePath, source, rc, sk, 0, 1, fileMode)
	// Check that the number of chunks in the file is correct.
	if sf.numChunks() != 1 {
		panic(fmt.Sprint("newTestFile didn't create the expected number of chunks ", sf.numChunks()))
	}
	// Set the cached fields to 0 like they would be if the file was already
	// uploaded before caching was introduced.
	sf.staticMetadata.CachedHealth = 0
	sf.staticMetadata.CachedStuckHealth = 0
	sf.staticMetadata.CachedRedundancy = 0
	sf.staticMetadata.CachedUploadProgress = 0
	// Save the file and reload it.
	if err := sf.Save(); err != nil {
		t.Fatal(err)
	}
	sf, err := loadSiaFile(siaFilePath, wal, modules.ProdDependencies)
	if err != nil {
		t.Fatal(err)
	}
	// Make sure the loaded file has the correct cached values.
	if sf.staticMetadata.CachedHealth != 0 {
		t.Fatalf("CachedHealth should be 0 but was %v", sf.staticMetadata.CachedHealth)
	}
	if sf.staticMetadata.CachedStuckHealth != 0 {
		t.Fatalf("CachedStuckHealth should be 0 but was %v", sf.staticMetadata.CachedStuckHealth)
	}
	expectedRedundancy := float64(rc.NumPieces()) / float64(rc.MinPieces())
	if sf.staticMetadata.CachedRedundancy != expectedRedundancy {
		t.Fatalf("CachedRedundancy should be %v but was %v", expectedRedundancy, sf.staticMetadata.CachedRedundancy)
	}
	if sf.staticMetadata.CachedUploadProgress != 100 {
		t.Fatalf("CachedUploadProgress should be 100 but was %v", sf.staticMetadata.CachedUploadProgress)
	}
}

// TestSaveSmallHeader tests the saveHeader method for a header that is not big
// enough to need more than a single page on disk.
func TestSaveSmallHeader(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	sf := newBlankTestFile()

	// Add some host keys.
	sf.addRandomHostKeys(10)

	// Save the header.
	updates, err := sf.saveHeaderUpdates()
	if err != nil {
		t.Fatal("Failed to create updates to save header", err)
	}
	if err := sf.createAndApplyTransaction(updates...); err != nil {
		t.Fatal("Failed to save header", err)
	}

	// Manually open the file to check its contents.
	f, err := os.Open(sf.siaFilePath)
	if err != nil {
		t.Fatal("Failed to open file", err)
	}
	defer f.Close()

	// Make sure the metadata was written to disk correctly.
	rawMetadata, err := marshalMetadata(sf.staticMetadata)
	if err != nil {
		t.Fatal("Failed to marshal metadata", err)
	}
	readMetadata := make([]byte, len(rawMetadata))
	if _, err := f.ReadAt(readMetadata, 0); err != nil {
		t.Fatal("Failed to read metadata", err)
	}
	if !bytes.Equal(rawMetadata, readMetadata) {
		fmt.Println(string(rawMetadata))
		fmt.Println(string(readMetadata))
		t.Fatal("Metadata on disk doesn't match marshaled metadata")
	}

	// Make sure that the pubKeyTable was written to disk correctly.
	rawPubKeyTAble, err := marshalPubKeyTable(sf.pubKeyTable)
	if err != nil {
		t.Fatal("Failed to marshal pubKeyTable", err)
	}
	readPubKeyTable := make([]byte, len(rawPubKeyTAble))
	if _, err := f.ReadAt(readPubKeyTable, sf.staticMetadata.PubKeyTableOffset); err != nil {
		t.Fatal("Failed to read pubKeyTable", err)
	}
	if !bytes.Equal(rawPubKeyTAble, readPubKeyTable) {
		t.Fatal("pubKeyTable on disk doesn't match marshaled pubKeyTable")
	}
}

// TestSaveLargeHeader tests the saveHeader method for a header that uses more than a single page on disk and forces a call to allocateHeaderPage
func TestSaveLargeHeader(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	sf := newBlankTestFile()

	// Add some host keys. This should force the SiaFile to allocate a new page
	// for the pubKeyTable.
	sf.addRandomHostKeys(100)

	// Open the file.
	f, err := os.OpenFile(sf.siaFilePath, os.O_RDWR, 777)
	if err != nil {
		t.Fatal("Failed to open file", err)
	}
	defer f.Close()

	// Write some data right after the ChunkOffset as a checksum.
	chunkData := fastrand.Bytes(100)
	_, err = f.WriteAt(chunkData, sf.staticMetadata.ChunkOffset)
	if err != nil {
		t.Fatal("Failed to write random chunk data", err)
	}

	// Save the header.
	updates, err := sf.saveHeaderUpdates()
	if err != nil {
		t.Fatal("Failed to create updates to save header", err)
	}
	if err := sf.createAndApplyTransaction(updates...); err != nil {
		t.Fatal("Failed to save header", err)
	}

	// Make sure the chunkOffset was updated correctly.
	if sf.staticMetadata.ChunkOffset != 2*pageSize {
		t.Fatal("ChunkOffset wasn't updated correctly", sf.staticMetadata.ChunkOffset, 2*pageSize)
	}

	// Make sure that the checksum was moved correctly.
	readChunkData := make([]byte, len(chunkData))
	if _, err := f.ReadAt(readChunkData, sf.staticMetadata.ChunkOffset); err != nil {
		t.Fatal("Checksum wasn't moved correctly")
	}

	// Make sure the metadata was written to disk correctly.
	rawMetadata, err := marshalMetadata(sf.staticMetadata)
	if err != nil {
		t.Fatal("Failed to marshal metadata", err)
	}
	readMetadata := make([]byte, len(rawMetadata))
	if _, err := f.ReadAt(readMetadata, 0); err != nil {
		t.Fatal("Failed to read metadata", err)
	}
	if !bytes.Equal(rawMetadata, readMetadata) {
		fmt.Println(string(rawMetadata))
		fmt.Println(string(readMetadata))
		t.Fatal("Metadata on disk doesn't match marshaled metadata")
	}

	// Make sure that the pubKeyTable was written to disk correctly.
	rawPubKeyTAble, err := marshalPubKeyTable(sf.pubKeyTable)
	if err != nil {
		t.Fatal("Failed to marshal pubKeyTable", err)
	}
	readPubKeyTable := make([]byte, len(rawPubKeyTAble))
	if _, err := f.ReadAt(readPubKeyTable, sf.staticMetadata.PubKeyTableOffset); err != nil {
		t.Fatal("Failed to read pubKeyTable", err)
	}
	if !bytes.Equal(rawPubKeyTAble, readPubKeyTable) {
		t.Fatal("pubKeyTable on disk doesn't match marshaled pubKeyTable")
	}
}

// testApply tests if a given method applies a set of updates correctly.
func testApply(t *testing.T, siaFile *SiaFile, apply func(...writeaheadlog.Update) error) {
	// Create an update that writes random data to a random index i.
	index := fastrand.Intn(100) + 1
	data := fastrand.Bytes(100)
	update := siaFile.createInsertUpdate(int64(index), data)

	// Apply update.
	if err := apply(update); err != nil {
		t.Fatal("Failed to apply update", err)
	}
	// Open file.
	file, err := os.Open(siaFile.siaFilePath)
	if err != nil {
		t.Fatal("Failed to open file", err)
	}
	// Check if correct data was written.
	readData := make([]byte, len(data))
	if _, err := file.ReadAt(readData, int64(index)); err != nil {
		t.Fatal("Failed to read written data back from disk", err)
	}
	if !bytes.Equal(data, readData) {
		t.Fatal("Read data doesn't equal written data")
	}
}

// TestUpdateUsedHosts tests the UpdateUsedHosts method.
func TestUpdateUsedHosts(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	sf := newBlankTestFile()
	sf.addRandomHostKeys(10)

	// All the host keys should be used.
	for _, entry := range sf.pubKeyTable {
		if !entry.Used {
			t.Fatal("all hosts are expected to be used at the beginning of the test")
		}
	}

	// Report only half the hosts as still being used.
	var used []types.SiaPublicKey
	for i, entry := range sf.pubKeyTable {
		if i%2 == 0 {
			used = append(used, entry.PublicKey)
		}
	}
	if err := sf.UpdateUsedHosts(used); err != nil {
		t.Fatal("failed to update hosts", err)
	}

	// Create a map of the used keys for faster lookups.
	usedMap := make(map[string]struct{})
	for _, key := range used {
		usedMap[key.String()] = struct{}{}
	}

	// Check that the flag was set correctly.
	for _, entry := range sf.pubKeyTable {
		_, exists := usedMap[entry.PublicKey.String()]
		if entry.Used != exists {
			t.Errorf("expected flag to be %v but was %v", exists, entry.Used)
		}
	}

	// Reload the siafile to see if the flags were also persisted.
	var err error
	sf, err = LoadSiaFile(sf.siaFilePath, sf.wal)
	if err != nil {
		t.Fatal(err)
	}

	// Check that the flags are still set correctly.
	for _, entry := range sf.pubKeyTable {
		_, exists := usedMap[entry.PublicKey.String()]
		if entry.Used != exists {
			t.Errorf("expected flag to be %v but was %v", exists, entry.Used)
		}
	}

	// Also check the flags in order. Making sure that persisting them didn't
	// change the order.
	for i, entry := range sf.pubKeyTable {
		expectedUsed := i%2 == 0
		if entry.Used != expectedUsed {
			t.Errorf("expected flag to be %v but was %v", expectedUsed, entry.Used)
		}
	}
}

// TestChunkOffset tests the chunkOffset method.
func TestChunkOffset(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	sf := newTestFile()

	// Set the static pages per chunk to a random value.
	sf.staticMetadata.StaticPagesPerChunk = uint8(fastrand.Intn(5)) + 1

	// Calculate the offset of the first chunk.
	offset1 := sf.chunkOffset(0)
	if expectedOffset := sf.staticMetadata.ChunkOffset; expectedOffset != offset1 {
		t.Fatalf("expected offset %v but got %v", sf.staticMetadata.ChunkOffset, offset1)
	}

	// Calculate the offset of the second chunk.
	offset2 := sf.chunkOffset(1)
	if expectedOffset := offset1 + int64(sf.staticMetadata.StaticPagesPerChunk)*pageSize; expectedOffset != offset2 {
		t.Fatalf("expected offset %v but got %v", expectedOffset, offset2)
	}

	// Make sure that the offsets we calculated are not the same due to not
	// initializing the file correctly.
	if offset2 == offset1 {
		t.Fatal("the calculated offsets are the same")
	}
}

// TestSaveChunk checks that saveChunk creates an updated which if applied
// writes the correct data to disk.
func TestSaveChunk(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	sf := newTestFile()

	// Choose a random chunk from the file and replace it.
	chunkIndex := fastrand.Intn(len(sf.fullChunks))
	chunk := randomChunk()
	sf.fullChunks[chunkIndex] = chunk

	// Write the chunk to disk using saveChunk.
	update := sf.saveChunkUpdate(chunkIndex)
	if err := sf.createAndApplyTransaction(update); err != nil {
		t.Fatal(err)
	}

	// Marshal the chunk.
	marshaledChunk := marshalChunk(chunk)

	// Read the chunk from disk.
	f, err := os.Open(sf.siaFilePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	readChunk := make([]byte, len(marshaledChunk))
	if _, err := f.ReadAt(readChunk, sf.chunkOffset(chunkIndex)); err != nil {
		t.Fatal(err)
	}

	// The marshaled chunk should equal the chunk we read from disk.
	if !bytes.Equal(readChunk, marshaledChunk) {
		t.Fatal("marshaled chunk doesn't equal chunk on disk")
	}
}

// TestUniqueIDMissing makes sure that loading a siafile sets the unique id in
// the metadata if it wasn't set before.
func TestUniqueIDMissing(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create a new file.
	sf, wal, _ := newBlankTestFileAndWAL(1)
	// It should have a UID.
	if sf.staticMetadata.StaticUniqueID == "" {
		t.Fatal("unique ID wasn't set")
	}
	// Set the UID to a blank string and save the file.
	sf.staticMetadata.StaticUniqueID = ""
	if err := sf.saveFile(); err != nil {
		t.Fatal(err)
	}
	// Load the file again.
	sf, err := LoadSiaFile(sf.siaFilePath, wal)
	if err != nil {
		t.Fatal(err)
	}
	// It should have a UID now.
	if sf.staticMetadata.StaticUniqueID == "" {
		t.Fatal("unique ID wasn't set after loading file")
	}
}

// TestSaveLoadDeletePartialChunk tests SavePartialChunk, LoadPartialChunk and
// DeletePartialChunk.
func TestSaveLoadDeletePartialChunk(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create a new siafile
	sf := newBlankTestFile()
	testDir := filepath.Join(os.TempDir(), t.Name())
	// The chunk status should be set to "combinedChunkStatusHasChunk"
	if sf.staticMetadata.CombinedChunkStatus != CombinedChunkStatusHasChunk {
		t.Fatal("Initial status wasn't combinedChunkStatusNoChunk")
	}
	// LoadPartialChunk and SetCombinedChunk should fail.
	if _, err := sf.LoadPartialChunk(); err == nil {
		t.Fatal("LoadPartialChunk should fail")
	}
	combinedChunk := fastrand.Bytes(int(sf.ChunkSize()))
	if err := SetCombinedChunk([]PartialChunkInfo{{sf: sf}}, "", combinedChunk, testDir); err == nil {
		t.Fatal("SetCombinedChunk should fail")
	}
	// Save a chunk that's too big.
	if err := sf.SavePartialChunk(fastrand.Bytes(int(sf.staticChunkSize()))); err == nil {
		t.Fatal("Shouldn't be able to save chunk that is too big")
	}
	// Save a valid chunk
	partialChunk := fastrand.Bytes(int(sf.staticChunkSize()) - 1)
	if err := sf.SavePartialChunk(partialChunk); err != nil {
		t.Fatal(err)
	}
	// The chunk status should be set to "combinedChunkStatusIncomplete"
	if sf.staticMetadata.CombinedChunkStatus != CombinedChunkStatusIncomplete {
		t.Fatal("Initial status wasn't combinedChunkStatusIncomplete")
	}
	// Make sure the partial chunk was written to disk correctly.
	path := strings.TrimSuffix(sf.SiaFilePath(), modules.SiaFileExtension) + modules.PartialChunkExtension
	b, err := ioutil.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, partialChunk) {
		t.Fatal("Read partial chunk doesn't match written chunk")
	}
	// SavePartialChunk should fail.
	if err := sf.SavePartialChunk(partialChunk); err == nil {
		t.Fatal("SavePartialChunk should fail")
	}
	// LoadPartialChunk and make sure it has the right contents.
	b, err = sf.LoadPartialChunk()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, partialChunk) {
		t.Fatal("Loaded partial chunk doesn't match written chunk")
	}
	// SavePartialChunk should fail.
	if err := sf.SavePartialChunk(partialChunk); err == nil {
		t.Fatal("SavePartialChunk should fail")
	}
	// Set the combined chunk.
	if err := SetCombinedChunk([]PartialChunkInfo{{sf: sf}}, "", combinedChunk, testDir); err != nil {
		t.Fatal(err)
	}
	// The chunk status should be set to "combinedChunkStatusCompleted"
	if sf.staticMetadata.CombinedChunkStatus != CombinedChunkStatusCompleted {
		t.Fatal("Initial status wasn't combinedChunkStatusCompleted")
	}
	// Make sure the file is gone.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("File wasn't removed correctly")
	}
	// All three methods should fail now
	if err := sf.SavePartialChunk(partialChunk); err == nil {
		t.Fatal("SavePartialChunk should fail")
	}
	if err := SetCombinedChunk([]PartialChunkInfo{{sf: sf}}, "", combinedChunk, testDir); err == nil {
		t.Fatal("SetCombinedChunk should fail")
	}
	if _, err := sf.LoadPartialChunk(); err == nil {
		t.Fatal("LoadPartialChunk should fail")
	}
}
