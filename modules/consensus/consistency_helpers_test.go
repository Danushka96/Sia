package consensus

import (
	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/modules/consensus/database"
)

// dbConsensusChecksum is a convenience function to call consensusChecksum
// without a bolt.Tx.
func (cs *ConsensusSet) dbConsensusChecksum() (checksum crypto.Hash) {
	err := cs.db.Update(func(tx database.Tx) error {
		checksum = consensusChecksum(tx)
		return nil
	})
	if err != nil {
		panic(err)
	}
	return checksum
}
