// Copyright (C) 2019-2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package state

import (
	"fmt"
	"time"

	"github.com/google/btree"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/hashing"
	"github.com/ava-labs/avalanchego/utils/math"
	"github.com/ava-labs/avalanchego/utils/wrappers"
	"github.com/ava-labs/avalanchego/vms/components/avax"
	"github.com/ava-labs/avalanchego/vms/platformvm/blocks"
	"github.com/ava-labs/avalanchego/vms/platformvm/genesis"
	"github.com/ava-labs/avalanchego/vms/platformvm/status"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
)

// var errNotYetImplemented = errors.New("NOT YET IMPLEMENTED")

func (ms *merkleState) sync(genesis []byte) error {
	shouldInit, err := ms.shouldInit()
	if err != nil {
		return fmt.Errorf(
			"failed to check if the database is initialized: %w",
			err,
		)
	}

	// If the database is empty, create the platform chain anew using the
	// provided genesis state
	if shouldInit {
		if err := ms.init(genesis); err != nil {
			return fmt.Errorf(
				"failed to initialize the database: %w",
				err,
			)
		}
	}

	return ms.load()
}

func (ms *merkleState) shouldInit() (bool, error) {
	has, err := ms.singletonDB.Has(initializedKey)
	return !has, err
}

func (ms *merkleState) doneInit() error {
	return ms.singletonDB.Put(initializedKey, nil)
}

func (ms *merkleState) init(genesisBytes []byte) error {
	// Create the genesis block and save it as being accepted (We don't do
	// genesisBlock.Accept() because then it'd look for genesisBlock's
	// non-existent parent)
	genesisID := hashing.ComputeHash256Array(genesisBytes)
	genesisBlock, err := blocks.NewApricotCommitBlock(genesisID, 0 /*height*/)
	if err != nil {
		return err
	}

	genesisState, err := genesis.ParseState(genesisBytes)
	if err != nil {
		return err
	}
	if err := ms.syncGenesis(genesisBlock, genesisState); err != nil {
		return err
	}

	if err := ms.doneInit(); err != nil {
		return err
	}

	return ms.Commit()
}

func (ms *merkleState) syncGenesis(genesisBlk blocks.Block, genesis *genesis.State) error {
	genesisBlkID := genesisBlk.ID()
	ms.SetLastAccepted(genesisBlkID)
	ms.SetTimestamp(time.Unix(int64(genesis.Timestamp), 0))
	ms.SetCurrentSupply(constants.PrimaryNetworkID, genesis.InitialSupply)
	ms.AddStatelessBlock(genesisBlk)

	// Persist UTXOs that exist at genesis
	for _, utxo := range genesis.UTXOs {
		ms.AddUTXO(utxo)
	}

	// Persist primary network validator set at genesis
	for _, vdrTx := range genesis.Validators {
		tx, ok := vdrTx.Unsigned.(*txs.AddValidatorTx)
		if !ok {
			return fmt.Errorf("expected tx type *txs.AddValidatorTx but got %T", vdrTx.Unsigned)
		}

		stakeAmount := tx.Validator.Wght
		stakeDuration := tx.Validator.Duration()
		currentSupply, err := ms.GetCurrentSupply(constants.PrimaryNetworkID)
		if err != nil {
			return err
		}

		potentialReward := ms.rewards.Calculate(
			stakeDuration,
			stakeAmount,
			currentSupply,
		)
		newCurrentSupply, err := math.Add64(currentSupply, potentialReward)
		if err != nil {
			return err
		}

		staker, err := NewCurrentStaker(vdrTx.ID(), tx, potentialReward)
		if err != nil {
			return err
		}

		ms.PutCurrentValidator(staker)
		ms.AddTx(vdrTx, status.Committed)
		ms.SetCurrentSupply(constants.PrimaryNetworkID, newCurrentSupply)
	}

	for _, chain := range genesis.Chains {
		unsignedChain, ok := chain.Unsigned.(*txs.CreateChainTx)
		if !ok {
			return fmt.Errorf("expected tx type *txs.CreateChainTx but got %T", chain.Unsigned)
		}

		// Ensure all chains that the genesis bytes say to create have the right
		// network ID
		if unsignedChain.NetworkID != ms.ctx.NetworkID {
			return avax.ErrWrongNetworkID
		}

		ms.AddChain(chain)
		ms.AddTx(chain, status.Committed)
	}

	// updateValidators is set to false here to maintain the invariant that the
	// primary network's validator set is empty before the validator sets are
	// initialized.
	return ms.write(false /*=updateValidators*/, 0)
}

// Load pulls data previously stored on disk that is expected to be in memory.
func (ms *merkleState) load() error {
	errs := wrappers.Errs{}
	errs.Add(
		ms.loadMerkleMetadata(),
		ms.loadCurrentStakers(),
		ms.loadPendingStakers(),
		ms.initValidatorSets(),

		ms.logMerkleRoot(),
	)
	return errs.Err
}

func (ms *merkleState) loadMerkleMetadata() error {
	// load chainTime
	chainTimeBytes, err := ms.merkleDB.Get(merkleChainTimeKey)
	if err != nil {
		return err
	}
	chainTime := time.Time{}
	if err := chainTime.UnmarshalBinary(chainTimeBytes); err != nil {
		return err
	}
	ms.SetTimestamp(chainTime)

	// load last accepted block
	blkIDBytes, err := ms.merkleDB.Get(merkleLastAcceptedBlkIDKey)
	if err != nil {
		return err
	}
	lastAcceptedBlkID := ids.Empty
	copy(lastAcceptedBlkID[:], blkIDBytes)
	ms.SetLastAccepted(lastAcceptedBlkID)

	// load supplies
	suppliedPrefix := merkleSuppliesKeyPrefix()
	iter := ms.merkleDB.NewIteratorWithPrefix(suppliedPrefix)
	defer iter.Release()
	for iter.Next() {
		_, subnetID := splitMerkleSuppliesKey(iter.Key())
		supply, err := database.ParseUInt64(iter.Value())
		if err != nil {
			return err
		}
		ms.supplies[subnetID] = supply
	}
	return iter.Error()
}

func (ms *merkleState) loadCurrentStakers() error {
	// TODO ABENEGIA: Check missing metadata
	ms.currentStakers = newBaseStakers()

	prefix := make([]byte, len(currentStakersSectionPrefix))
	copy(prefix, currentStakersSectionPrefix)

	iter := ms.merkleDB.NewIteratorWithPrefix(prefix)
	defer iter.Release()
	for iter.Next() {
		data := &stakersData{}
		if _, err := txs.GenesisCodec.Unmarshal(iter.Value(), data); err != nil {
			return fmt.Errorf("failed to deserialize current stakers data: %w", err)
		}

		tx, err := txs.Parse(txs.GenesisCodec, data.TxBytes)
		if err != nil {
			return fmt.Errorf("failed to parsing current stakerTx: %w", err)
		}
		stakerTx, ok := tx.Unsigned.(txs.Staker)
		if !ok {
			return fmt.Errorf("expected tx type txs.Staker but got %T", tx.Unsigned)
		}

		staker, err := NewCurrentStaker(tx.ID(), stakerTx, data.PotentialReward)
		if err != nil {
			return err
		}
		if staker.Priority.IsValidator() {
			// TODO: why not PutValidator/PutDelegator??
			validator := ms.currentStakers.getOrCreateValidator(staker.SubnetID, staker.NodeID)
			validator.validator = staker
			ms.currentStakers.stakers.ReplaceOrInsert(staker)
		} else {
			validator := ms.currentStakers.getOrCreateValidator(staker.SubnetID, staker.NodeID)
			if validator.delegators == nil {
				validator.delegators = btree.NewG(defaultTreeDegree, (*Staker).Less)
			}
			validator.delegators.ReplaceOrInsert(staker)
			ms.currentStakers.stakers.ReplaceOrInsert(staker)
		}
	}
	return iter.Error()
}

func (ms *merkleState) loadPendingStakers() error {
	// TODO ABENEGIA: Check missing metadata
	ms.pendingStakers = newBaseStakers()

	prefix := make([]byte, len(pendingStakersSectionPrefix))
	copy(prefix, pendingStakersSectionPrefix)

	iter := ms.merkleDB.NewIteratorWithPrefix(prefix)
	defer iter.Release()
	for iter.Next() {
		data := &stakersData{}
		if _, err := txs.GenesisCodec.Unmarshal(iter.Value(), data); err != nil {
			return fmt.Errorf("failed to deserialize pending stakers data: %w", err)
		}

		tx, err := txs.Parse(txs.GenesisCodec, data.TxBytes)
		if err != nil {
			return fmt.Errorf("failed to parsing pending stakerTx: %w", err)
		}
		stakerTx, ok := tx.Unsigned.(txs.Staker)
		if !ok {
			return fmt.Errorf("expected tx type txs.Staker but got %T", tx.Unsigned)
		}

		staker, err := NewPendingStaker(tx.ID(), stakerTx)
		if err != nil {
			return err
		}
		if staker.Priority.IsValidator() {
			validator := ms.pendingStakers.getOrCreateValidator(staker.SubnetID, staker.NodeID)
			validator.validator = staker
			ms.pendingStakers.stakers.ReplaceOrInsert(staker)
		} else {
			validator := ms.pendingStakers.getOrCreateValidator(staker.SubnetID, staker.NodeID)
			if validator.delegators == nil {
				validator.delegators = btree.NewG(defaultTreeDegree, (*Staker).Less)
			}
			validator.delegators.ReplaceOrInsert(staker)
			ms.pendingStakers.stakers.ReplaceOrInsert(staker)
		}
	}
	return iter.Error()
}

// Invariant: initValidatorSets requires loadCurrentValidators to have already
// been called.
func (ms *merkleState) initValidatorSets() error {
	primaryValidators, ok := ms.cfg.Validators.Get(constants.PrimaryNetworkID)
	if !ok {
		return errMissingValidatorSet
	}
	if primaryValidators.Len() != 0 {
		// Enforce the invariant that the validator set is empty here.
		return errValidatorSetAlreadyPopulated
	}
	err := ms.ValidatorSet(constants.PrimaryNetworkID, primaryValidators)
	if err != nil {
		return err
	}

	vl := validators.NewLogger(ms.ctx.Log, ms.bootstrapped, constants.PrimaryNetworkID, ms.ctx.NodeID)
	primaryValidators.RegisterCallbackListener(vl)

	ms.metrics.SetLocalStake(primaryValidators.GetWeight(ms.ctx.NodeID))
	ms.metrics.SetTotalStake(primaryValidators.Weight())

	for subnetID := range ms.cfg.TrackedSubnets {
		subnetValidators := validators.NewSet()
		err := ms.ValidatorSet(subnetID, subnetValidators)
		if err != nil {
			return err
		}

		if !ms.cfg.Validators.Add(subnetID, subnetValidators) {
			return fmt.Errorf("%w: %s", errDuplicateValidatorSet, subnetID)
		}

		vl := validators.NewLogger(ms.ctx.Log, ms.bootstrapped, subnetID, ms.ctx.NodeID)
		subnetValidators.RegisterCallbackListener(vl)
	}
	return nil
}
