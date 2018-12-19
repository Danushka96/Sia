package database

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/coreos/bbolt"
	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/encoding"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/types"
)

// A Tx is a database transaction.
type Tx interface {
	Bucket(name []byte) *bolt.Bucket
	CreateBucket(name []byte) (*bolt.Bucket, error)
	CreateBucketIfNotExists(name []byte) (*bolt.Bucket, error)
	DeleteBucket(name []byte) error
	ForEach(func([]byte, *bolt.Bucket) error) error

	// ConsensusChecksum grabs a checksum of the consensus set by pushing all
	// of the elements in sorted order into a Merkle tree and taking the root.
	// All consensus sets with the same current block should have identical
	// consensus checksums.
	ConsensusChecksum() crypto.Hash

	// CheckCurrencyCounts returns an error if the sum of siacoin outputs,
	// siafund outputs, and delayed siacoin outputs does not match expected
	// values.
	CheckCurrencyCounts() error

	// MarkInconsistent marks the database as inconsistent.
	MarkInconsistent()

	// SiafundPool returns the value of the Siafund pool.
	SiafundPool() types.Currency
	// SetSiafundPool sets the value of the Siafund pool.
	SetSiafundPool(pool types.Currency)

	// BlockHeight returns the height of the blockchain.
	BlockHeight() types.BlockHeight
	// SetBlockHeight sets the height of the blockchain.
	SetBlockHeight(height types.BlockHeight)

	// PushPath appends a BlockID to the current path.
	PushPath(id types.BlockID)
	// PopPath removes the last BlockID in the current path.
	PopPath()
	// BlockID returns the ID of the block at the specified height in the
	// current path.
	BlockID(height types.BlockHeight) types.BlockID

	// ChangeEntry returns the ChangeEntry with the specified id.
	ChangeEntry(id modules.ConsensusChangeID) (ChangeEntry, bool)
	// AppendChangeEntry appends ce to the list of change entries.
	AppendChangeEntry(ce ChangeEntry)
	// ChangeLogTailID returns the ID of the most recently-appended
	// ChangeEntry.
	ChangeLogTailID() modules.ConsensusChangeID

	// DifficultyTotals returns the difficulty adjustment parameters for a
	// given block.
	DifficultyTotals(id types.BlockID) (totalTime int64, totalTarget types.Target)
	// SetDifficultyTotals sets the difficulty adjustment parameters for a
	// given block.
	SetDifficultyTotals(id types.BlockID, totalTime int64, totalTarget types.Target)
}

type txWrapper struct {
	*bolt.Tx
}

// ConsensusChecksum implements the Tx interface.
func (tx txWrapper) ConsensusChecksum() crypto.Hash {
	// Create a checksum tree.
	tree := crypto.NewTree()

	// For all of the constant buckets, push every key and every value. Buckets
	// are sorted in byte-order, therefore this operation is deterministic.
	consensusSetBuckets := []*bolt.Bucket{
		tx.Bucket(blockPath),
		tx.Bucket(siacoinOutputs),
		tx.Bucket(fileContracts),
		tx.Bucket(siafundOutputs),
		tx.Bucket(siafundPool),
	}
	for i := range consensusSetBuckets {
		err := consensusSetBuckets[i].ForEach(func(k, v []byte) error {
			tree.Push(k)
			tree.Push(v)
			return nil
		})
		if build.DEBUG && err != nil {
			panic(err)
		}
	}

	// Iterate through all the buckets looking for buckets prefixed with
	// prefixDSCO or prefixFCEX. Buckets are presented in byte-sorted order by
	// name.
	err := tx.ForEach(func(name []byte, b *bolt.Bucket) error {
		// If the bucket is not a delayed siacoin output bucket or a file
		// contract expiration bucket, skip.
		if !bytes.HasPrefix(name, prefixDSCO) && !bytes.HasPrefix(name, prefixFCEX) {
			return nil
		}

		// The bucket is a prefixed bucket - add all elements to the tree.
		return b.ForEach(func(k, v []byte) error {
			tree.Push(k)
			tree.Push(v)
			return nil
		})
	})
	if build.DEBUG && err != nil {
		panic(err)
	}

	return tree.Root()
}

// MarkInconsistent implements the Tx interface.
func (tx txWrapper) MarkInconsistent() {
	cerr := tx.Bucket(consistency).Put(consistency, encoding.Marshal(true))
	if build.DEBUG && cerr != nil {
		panic(cerr)
	}
}

// SiafundPool implements the Tx interface.
func (tx txWrapper) SiafundPool() types.Currency {
	var pool types.Currency
	err := encoding.Unmarshal(tx.Bucket(siafundPool).Get(siafundPool), &pool)
	if build.DEBUG && err != nil {
		panic(err)
	}
	return pool
}

// SetSiafundPool implements the Tx interface.
func (tx txWrapper) SetSiafundPool(pool types.Currency) {
	err := tx.Bucket(siafundPool).Put(siafundPool, encoding.Marshal(pool))
	if build.DEBUG && err != nil {
		panic(err)
	}
}

// BlockHeight implements the Tx interface.
func (tx txWrapper) BlockHeight() types.BlockHeight {
	var height types.BlockHeight
	err := encoding.Unmarshal(tx.Bucket(blockHeight).Get(blockHeight), &height)
	if build.DEBUG && err != nil {
		panic(err)
	}
	return height
}

// SetBlockHeight implements the Tx interface.
func (tx txWrapper) SetBlockHeight(height types.BlockHeight) {
	err := tx.Bucket(blockHeight).Put(blockHeight, encoding.Marshal(height))
	if build.DEBUG && err != nil {
		panic(err)
	}
}

// BlockID implements the Tx interface.
func (tx txWrapper) BlockID(height types.BlockHeight) types.BlockID {
	var id types.BlockID
	copy(id[:], tx.Bucket(blockPath).Get(encoding.Marshal(height)))
	return id
}

// PushPath implements the Tx interface.
func (tx txWrapper) PushPath(id types.BlockID) {
	newHeight := tx.BlockHeight() + 1
	tx.SetBlockHeight(newHeight)

	err := tx.Bucket(blockPath).Put(encoding.Marshal(newHeight), id[:])
	if build.DEBUG && err != nil {
		panic(err)
	}
}

// PopPath implements the Tx interface.
func (tx txWrapper) PopPath() {
	oldHeight := tx.BlockHeight()
	tx.SetBlockHeight(oldHeight - 1)

	err := tx.Bucket(blockPath).Delete(encoding.Marshal(oldHeight))
	if build.DEBUG && err != nil {
		panic(err)
	}
}

// ChangeEntry implements the Tx interface.
func (tx txWrapper) ChangeEntry(id modules.ConsensusChangeID) (ChangeEntry, bool) {
	var cn changeNode
	changeNodeBytes := tx.Bucket(changeLog).Get(id[:])
	if changeNodeBytes == nil {
		return ChangeEntry{}, false
	}
	err := encoding.Unmarshal(changeNodeBytes, &cn)
	if build.DEBUG && err != nil {
		panic(err)
	}
	return cn.Entry, true
}

// ChangeLogTailID implements the Tx interface.
func (tx txWrapper) ChangeLogTailID() (id modules.ConsensusChangeID) {
	tailID := tx.Bucket(changeLog).Get(changeLogTailID)
	copy(id[:], tailID)
	return
}

// AppendChangeEntry implements the Tx interface.
func (tx txWrapper) AppendChangeEntry(ce ChangeEntry) {
	ceid := ce.ID()
	b := tx.Bucket(changeLog)
	err := b.Put(ceid[:], encoding.Marshal(changeNode{Entry: ce}))
	if build.DEBUG && err != nil {
		panic(err)
	}

	// If this is not the first change entry, update the previous entry to
	// point to this one.
	if tailID := b.Get(changeLogTailID); tailID != nil {
		var tailCN changeNode
		err = encoding.Unmarshal(b.Get(tailID), &tailCN)
		if build.DEBUG && err != nil {
			panic(err)
		}
		tailCN.Next = ceid
		err = b.Put(tailID, encoding.Marshal(tailCN))
		if build.DEBUG && err != nil {
			panic(err)
		}
	}

	// Update the tail ID.
	err = b.Put(changeLogTailID, ceid[:])
	if build.DEBUG && err != nil {
		panic(err)
	}
}

// DifficultyTotals implements the Tx interface.
func (tx txWrapper) DifficultyTotals(id types.BlockID) (totalTime int64, totalTarget types.Target) {
	bytes := tx.Bucket(bucketOak).Get(id[:])
	if bytes == nil {
		return 0, types.Target{}
	}
	totalTime = int64(binary.LittleEndian.Uint64(bytes[:8]))
	copy(totalTarget[:], bytes[8:])
	return
}

// SetDifficultyTotals implements the Tx interface.
func (tx txWrapper) SetDifficultyTotals(id types.BlockID, totalTime int64, totalTarget types.Target) {
	bytes := make([]byte, 40)
	binary.LittleEndian.PutUint64(bytes[:8], uint64(totalTime))
	copy(bytes[8:], totalTarget[:])
	err := tx.Bucket(bucketOak).Put(id[:], bytes)
	if build.DEBUG && err != nil {
		panic(err)
	}
}

// CheckCurrencyCounts implements the Tx interface.
func (tx txWrapper) CheckCurrencyCounts() error {
	if err := tx.checkSiacoinsCount(); err != nil {
		return err
	}
	if err := tx.checkSiafundsCount(); err != nil {
		return err
	}
	if err := tx.checkDSCOsCount(); err != nil {
		return err
	}
	return nil
}

// checkSiacoinCount checks that the number of siacoins countable within the
// consensus set equal the expected number of siacoins for the block height.
func (tx txWrapper) checkSiacoinsCount() error {
	// Iterate through all the buckets looking for the delayed siacoin output
	// buckets.
	var dscoSiacoins types.Currency
	err := tx.ForEach(func(name []byte, b *bolt.Bucket) error {
		// Check if the bucket is a delayed siacoin output bucket.
		if !bytes.HasPrefix(name, prefixDSCO) {
			return nil
		}

		// Sum up the delayed outputs in this bucket.
		err := b.ForEach(func(_, delayedOutput []byte) error {
			var sco types.SiacoinOutput
			err := encoding.Unmarshal(delayedOutput, &sco)
			if err != nil {
				return err
			}
			dscoSiacoins = dscoSiacoins.Add(sco.Value)
			return nil
		})
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Add all of the siacoin outputs.
	var scoSiacoins types.Currency
	err = tx.Bucket(siacoinOutputs).ForEach(func(_, scoBytes []byte) error {
		var sco types.SiacoinOutput
		err := encoding.Unmarshal(scoBytes, &sco)
		if err != nil {
			return err
		}
		scoSiacoins = scoSiacoins.Add(sco.Value)
		return nil
	})
	if err != nil {
		return err
	}

	// Add all of the payouts from file contracts.
	var fcSiacoins types.Currency
	err = tx.Bucket(fileContracts).ForEach(func(_, fcBytes []byte) error {
		var fc types.FileContract
		err := encoding.Unmarshal(fcBytes, &fc)
		if err != nil {
			return err
		}
		var fcCoins types.Currency
		for _, output := range fc.ValidProofOutputs {
			fcCoins = fcCoins.Add(output.Value)
		}
		fcSiacoins = fcSiacoins.Add(fcCoins)
		return nil
	})
	if err != nil {
		return err
	}

	// Add all of the siafund claims.
	pool := tx.SiafundPool()
	var claimSiacoins types.Currency
	err = tx.Bucket(siafundOutputs).ForEach(func(_, sfoBytes []byte) error {
		var sfo types.SiafundOutput
		err := encoding.Unmarshal(sfoBytes, &sfo)
		if err != nil {
			return err
		}

		coinsPerFund := pool.Sub(sfo.ClaimStart)
		claimCoins := coinsPerFund.Mul(sfo.Value).Div(types.SiafundCount)
		claimSiacoins = claimSiacoins.Add(claimCoins)
		return nil
	})
	if err != nil {
		return err
	}

	expectedSiacoins := types.CalculateNumSiacoins(tx.BlockHeight())
	totalSiacoins := dscoSiacoins.Add(scoSiacoins).Add(fcSiacoins).Add(claimSiacoins)
	if !totalSiacoins.Equals(expectedSiacoins) {
		diagnostics := fmt.Sprintf("Wrong number of siacoins\nDsco: %v\nSco: %v\nFc: %v\nClaim: %v\n", dscoSiacoins, scoSiacoins, fcSiacoins, claimSiacoins)
		if totalSiacoins.Cmp(expectedSiacoins) < 0 {
			diagnostics += fmt.Sprintf("total: %v\nexpected: %v\n expected is bigger: %v", totalSiacoins, expectedSiacoins, expectedSiacoins.Sub(totalSiacoins))
		} else {
			diagnostics += fmt.Sprintf("total: %v\nexpected: %v\n expected is bigger: %v", totalSiacoins, expectedSiacoins, totalSiacoins.Sub(expectedSiacoins))
		}
		return errors.New(diagnostics)
	}

	return nil
}

// checkSiafundsCount checks that the number of siafunds countable within the
// consensus set equal the expected number of siafunds for the block height.
func (tx txWrapper) checkSiafundsCount() error {
	var total types.Currency
	err := tx.Bucket(siafundOutputs).ForEach(func(_, siafundOutputBytes []byte) error {
		var sfo types.SiafundOutput
		if err := encoding.Unmarshal(siafundOutputBytes, &sfo); err != nil {
			return err
		}
		total = total.Add(sfo.Value)
		return nil
	})
	if err != nil {
		return err
	}
	if !total.Equals(types.SiafundCount) {
		return errors.New("wrong number of siafunds in the consensus set")
	}
	return nil
}

// checkDSCOsCount scans the sets of delayed siacoin outputs and checks for
// consistency.
func (tx txWrapper) checkDSCOsCount() error {
	// Create a map to track which delayed siacoin output maps exist, and
	// another map to track which ids have appeared in the dsco set.
	dscoTracker := make(map[types.BlockHeight]struct{})
	idMap := make(map[types.SiacoinOutputID]struct{})

	// Iterate through all the buckets looking for the delayed siacoin output
	// buckets, and check that they are for the correct heights.
	err := tx.ForEach(func(name []byte, b *bolt.Bucket) error {
		// If the bucket is not a delayed siacoin output bucket or a file
		// contract expiration bucket, skip.
		if !bytes.HasPrefix(name, prefixDSCO) {
			return nil
		}

		// Add the bucket to the dscoTracker.
		var height types.BlockHeight
		if err := encoding.Unmarshal(name[len(prefixDSCO):], &height); err != nil {
			return err
		}
		_, exists := dscoTracker[height]
		if exists {
			return errors.New("repeat dsco map")
		}
		dscoTracker[height] = struct{}{}

		var total types.Currency
		err := b.ForEach(func(idBytes, delayedOutput []byte) error {
			// Check that the output id has not appeared in another dsco.
			var id types.SiacoinOutputID
			copy(id[:], idBytes)
			_, exists := idMap[id]
			if exists {
				return errors.New("repeat delayed siacoin output")
			}
			idMap[id] = struct{}{}

			// Sum the funds in the bucket.
			var sco types.SiacoinOutput
			if err := encoding.Unmarshal(delayedOutput, &sco); err != nil {
				return err
			}
			total = total.Add(sco.Value)
			return nil
		})
		if err != nil {
			return err
		}

		// Check that the minimum value has been achieved - the coinbase from
		// an earlier block is guaranteed to be in the bucket.
		minimumValue := types.CalculateCoinbase(height - types.MaturityDelay)
		if total.Cmp(minimumValue) < 0 {
			return errors.New("total number of coins in the delayed output bucket is incorrect")
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Check that all of the correct heights are represented.
	currentHeight := tx.BlockHeight()
	expectedBuckets := 0
	for i := currentHeight + 1; i <= currentHeight+types.MaturityDelay; i++ {
		if i < types.MaturityDelay {
			continue
		}
		_, exists := dscoTracker[i]
		if !exists {
			return errors.New("missing a dsco bucket")
		}
		expectedBuckets++
	}
	if len(dscoTracker) != expectedBuckets {
		return errors.New("too many dsco buckets")
	}
	return nil
}
