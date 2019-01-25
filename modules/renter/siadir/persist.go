package siadir

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/encoding"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/errors"
	"gitlab.com/NebulousLabs/writeaheadlog"
)

// ApplyUpdates  applies a number of writeaheadlog updates to the corresponding
// SiaDir. This method can apply updates from different SiaDirs and should only
// be run before the SiaDirs are loaded from disk right after the startup of
// siad. Otherwise we might run into concurrency issues.
func ApplyUpdates(updates ...writeaheadlog.Update) error {
	// Apply updates.
	for _, u := range updates {
		err := applyUpdate(modules.ProdDependencies, u)
		if err != nil {
			return errors.AddContext(err, "failed to apply update")
		}
	}
	return nil
}

// applyUpdate applies the wal update
func applyUpdate(deps modules.Dependencies, update writeaheadlog.Update) error {
	switch update.Name {
	case updateDeleteName:
		// Delete from disk
		err := os.RemoveAll(readDeleteUpdate(update))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	case updateMetadataName:
		return readAndApplyMetadataUpdate(deps, update)
	default:
		return fmt.Errorf("Update not recognized: %v", update.Name)
	}
}

// readAndApplyMetadataUpdate reads the metadata update and then applies it.
// This helper assumes that the file is not currently open and so should only be
// called on startup before any siadir is loaded from disk
func readAndApplyMetadataUpdate(deps modules.Dependencies, update writeaheadlog.Update) error {
	// Decode update.
	data, path, err := readMetadataUpdate(update)
	if err != nil {
		return err
	}

	// Write out the data to the real file, with a sync.
	file, err := deps.OpenFile(path, os.O_RDWR|os.O_TRUNC|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Compose(err, file.Close())
	}()

	// Write and sync.
	_, err = file.Write(data)
	if err != nil {
		return err
	}
	return file.Sync()
}

// readDeleteUpdate unmarshals the update's instructions and returns the
// encoded path.
func readDeleteUpdate(update writeaheadlog.Update) string {
	if update.Name != updateDeleteName {
		err := errors.New("readDeleteUpdate can't read non-delete updates")
		build.Critical(err)
		return ""
	}
	return string(update.Instructions)
}

// managedCreateAndApplyTransaction is a helper method that creates a writeaheadlog
// transaction and applies it.
func managedCreateAndApplyTransaction(wal *writeaheadlog.WAL, updates ...writeaheadlog.Update) error {
	// Create the writeaheadlog transaction.
	txn, err := wal.NewTransaction(updates)
	if err != nil {
		return errors.AddContext(err, "failed to create wal txn")
	}
	// No extra setup is required. Signal that it is done.
	if err := <-txn.SignalSetupComplete(); err != nil {
		return errors.AddContext(err, "failed to signal setup completion")
	}
	// Apply the updates.
	if err := ApplyUpdates(updates...); err != nil {
		return errors.AddContext(err, "failed to apply updates")
	}
	// Updates are applied. Let the writeaheadlog know.
	if err := txn.SignalUpdatesApplied(); err != nil {
		return errors.AddContext(err, "failed to signal that updates are applied")
	}
	return nil
}

// createDirMetadataAll creates a path on disk to the provided siaPath and make
// sure that all the parent directories have metadata files.
func createDirMetadataAll(siaPath, rootDir string) ([]writeaheadlog.Update, error) {
	// Create path to directory
	if err := os.MkdirAll(filepath.Join(rootDir, siaPath), 0700); err != nil {
		return nil, err
	}

	// Create metadata
	var updates []writeaheadlog.Update
	for {
		siaPath = filepath.Dir(siaPath)
		if siaPath == "." {
			siaPath = ""
		}
		_, update, err := createDirMetadata(siaPath, rootDir)
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(update, writeaheadlog.Update{}) {
			updates = append(updates, update)
		}
		if siaPath == "" {
			break
		}
	}
	return updates, nil
}

// createMetadataUpdate is a helper method which creates a writeaheadlog update for
// updating the siaDir metadata
func createMetadataUpdate(metadata siaDirMetadata) (writeaheadlog.Update, error) {
	// Encode metadata
	data, err := json.Marshal(metadata)
	if err != nil {
		return writeaheadlog.Update{}, err
	}
	path := filepath.Join(metadata.RootDir, metadata.SiaPath, SiaDirExtension)

	// Create update
	return writeaheadlog.Update{
		Name:         updateMetadataName,
		Instructions: encoding.MarshalAll(data, path),
	}, nil
}

// readMetadataUpdate unmarshals the update's instructions and returns the
// metadata encoded in the instructions.
func readMetadataUpdate(update writeaheadlog.Update) (data []byte, path string, err error) {
	if update.Name != updateMetadataName {
		err = errors.New("readMetadataUpdate can't read non-metadata updates")
		build.Critical(err)
		return
	}
	err = encoding.UnmarshalAll(update.Instructions, &data, &path)
	return
}

// applyUpdates  applies a number of writeaheadlog updates to the corresponding
// SiaDir.
func (sd *SiaDir) applyUpdates(updates ...writeaheadlog.Update) error {
	// Open the file
	siaDirPath := filepath.Join(sd.staticMetadata.RootDir, sd.staticMetadata.SiaPath, SiaDirExtension)
	file, err := sd.deps.OpenFile(siaDirPath, os.O_RDWR|os.O_TRUNC|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if err == nil {
			// If no error occurred we sync and close the file.
			err = errors.Compose(file.Sync(), file.Close())
		} else {
			// Otherwise we still need to close the file.
			err = errors.Compose(err, file.Close())
		}
	}()

	// Apply updates.
	for _, u := range updates {
		err := func() error {
			switch u.Name {
			case updateDeleteName:
				// Delete from disk
				err := os.RemoveAll(readDeleteUpdate(u))
				if os.IsNotExist(err) {
					return nil
				}
				return err
			case updateMetadataName:
				return sd.readAndApplyMetadataUpdate(file, u)
			default:
				return fmt.Errorf("Update not recognized: %v", u.Name)
			}
		}()
		if err != nil {
			return errors.AddContext(err, "failed to apply update")
		}
	}
	return nil
}

// createAndApplyTransaction is a helper method that creates a writeaheadlog
// transaction and applies it.
func (sd *SiaDir) createAndApplyTransaction(updates ...writeaheadlog.Update) error {
	// This should never be called on a deleted directory.
	if sd.deleted {
		return errors.New("shouldn't apply updates on deleted directory")
	}
	// Create the writeaheadlog transaction.
	txn, err := sd.wal.NewTransaction(updates)
	if err != nil {
		return errors.AddContext(err, "failed to create wal txn")
	}
	// No extra setup is required. Signal that it is done.
	if err := <-txn.SignalSetupComplete(); err != nil {
		return errors.AddContext(err, "failed to signal setup completion")
	}
	// Apply the updates.
	if err := sd.applyUpdates(updates...); err != nil {
		return errors.AddContext(err, "failed to apply updates")
	}
	// Updates are applied. Let the writeaheadlog know.
	if err := txn.SignalUpdatesApplied(); err != nil {
		return errors.AddContext(err, "failed to signal that updates are applied")
	}
	return nil
}

// createDeleteUpdate is a helper method that creates a writeaheadlog for
// deleting a directory.
func (sd *SiaDir) createDeleteUpdate() writeaheadlog.Update {
	return writeaheadlog.Update{
		Name:         updateDeleteName,
		Instructions: []byte(filepath.Join(sd.staticMetadata.RootDir, sd.staticMetadata.SiaPath)),
	}
}

// saveDir saves the whole SiaDir atomically.
func (sd *SiaDir) saveDir() error {
	metadataUpdate, err := sd.saveMetadataUpdate()
	if err != nil {
		return err
	}
	return sd.createAndApplyTransaction(metadataUpdate)
}

// saveMetadataUpdate saves the metadata of the SiaDir
func (sd *SiaDir) saveMetadataUpdate() (writeaheadlog.Update, error) {
	return createMetadataUpdate(sd.staticMetadata)
}

// readAndApplyMetadataUpdate reads the metadata update for a file and then
// applies it.
func (sd *SiaDir) readAndApplyMetadataUpdate(file modules.File, update writeaheadlog.Update) error {
	// Decode update.
	data, path, err := readMetadataUpdate(update)
	if err != nil {
		return err
	}

	// Sanity check path belongs to siadir
	siaDirPath := filepath.Join(sd.staticMetadata.RootDir, sd.staticMetadata.SiaPath, SiaDirExtension)
	if path != siaDirPath {
		build.Critical(fmt.Sprintf("can't apply update for file %s to SiaDir %s", path, siaDirPath))
		return nil
	}

	// Write and sync.
	_, err = file.Write(data)
	if err != nil {
		return err
	}
	return nil
}