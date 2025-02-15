package slasher

import (
	"context"
	"testing"

	mock "github.com/prysmaticlabs/prysm/beacon-chain/blockchain/testing"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/signing"
	dbtest "github.com/prysmaticlabs/prysm/beacon-chain/db/testing"
	slashingsmock "github.com/prysmaticlabs/prysm/beacon-chain/operations/slashings/mock"
	slashertypes "github.com/prysmaticlabs/prysm/beacon-chain/slasher/types"
	"github.com/prysmaticlabs/prysm/beacon-chain/state/stategen"
	"github.com/prysmaticlabs/prysm/config/params"
	types "github.com/prysmaticlabs/prysm/consensus-types/primitives"
	"github.com/prysmaticlabs/prysm/crypto/bls"
	"github.com/prysmaticlabs/prysm/encoding/bytesutil"
	ethpb "github.com/prysmaticlabs/prysm/proto/prysm/v1alpha1"
	"github.com/prysmaticlabs/prysm/testing/require"
	"github.com/prysmaticlabs/prysm/testing/util"
	logTest "github.com/sirupsen/logrus/hooks/test"
)

func Test_processQueuedBlocks_DetectsDoubleProposals(t *testing.T) {
	hook := logTest.NewGlobal()
	slasherDB := dbtest.SetupSlasherDB(t)
	beaconDB := dbtest.SetupDB(t)
	ctx, cancel := context.WithCancel(context.Background())

	beaconState, err := util.NewBeaconState()
	require.NoError(t, err)

	// Initialize validators in the state.
	numVals := params.BeaconConfig().MinGenesisActiveValidatorCount
	validators := make([]*ethpb.Validator, numVals)
	privKeys := make([]bls.SecretKey, numVals)
	for i := range validators {
		privKey, err := bls.RandKey()
		require.NoError(t, err)
		privKeys[i] = privKey
		validators[i] = &ethpb.Validator{
			PublicKey:             privKey.PublicKey().Marshal(),
			WithdrawalCredentials: make([]byte, 32),
		}
	}
	err = beaconState.SetValidators(validators)
	require.NoError(t, err)
	domain, err := signing.Domain(
		beaconState.Fork(),
		0,
		params.BeaconConfig().DomainBeaconProposer,
		beaconState.GenesisValidatorsRoot(),
	)
	require.NoError(t, err)

	mockChain := &mock.ChainService{
		State: beaconState,
	}
	s := &Service{
		serviceCfg: &ServiceConfig{
			Database:             slasherDB,
			StateNotifier:        &mock.MockStateNotifier{},
			HeadStateFetcher:     mockChain,
			StateGen:             stategen.New(beaconDB),
			SlashingPoolInserter: &slashingsmock.PoolMock{},
		},
		params:    DefaultParams(),
		blksQueue: newBlocksQueue(),
	}

	parentRoot := bytesutil.ToBytes32([]byte("parent"))
	err = s.serviceCfg.StateGen.SaveState(ctx, parentRoot, beaconState)
	require.NoError(t, err)

	currentSlotChan := make(chan types.Slot)
	exitChan := make(chan struct{})
	go func() {
		s.processQueuedBlocks(ctx, currentSlotChan)
		exitChan <- struct{}{}
	}()

	signedBlkHeaders := []*slashertypes.SignedBlockHeaderWrapper{
		createProposalWrapper(t, 4, 1, []byte{1}),
		createProposalWrapper(t, 4, 1, []byte{1}),
		createProposalWrapper(t, 4, 1, []byte{1}),
		createProposalWrapper(t, 4, 1, []byte{2}),
	}

	// Add valid signatures to the block headers we are testing.
	for _, proposalWrapper := range signedBlkHeaders {
		proposalWrapper.SignedBeaconBlockHeader.Header.ParentRoot = parentRoot[:]
		headerHtr, err := proposalWrapper.SignedBeaconBlockHeader.Header.HashTreeRoot()
		require.NoError(t, err)
		container := &ethpb.SigningData{
			ObjectRoot: headerHtr[:],
			Domain:     domain,
		}
		signingRoot, err := container.HashTreeRoot()
		require.NoError(t, err)
		privKey := privKeys[proposalWrapper.SignedBeaconBlockHeader.Header.ProposerIndex]
		proposalWrapper.SignedBeaconBlockHeader.Signature = privKey.Sign(signingRoot[:]).Marshal()
	}

	s.blksQueue.extend(signedBlkHeaders)

	currentSlot := types.Slot(4)
	currentSlotChan <- currentSlot
	cancel()
	<-exitChan
	require.LogsContain(t, hook, "Proposer slashing detected")
}

func Test_processQueuedBlocks_NotSlashable(t *testing.T) {
	hook := logTest.NewGlobal()
	slasherDB := dbtest.SetupSlasherDB(t)
	ctx, cancel := context.WithCancel(context.Background())

	beaconState, err := util.NewBeaconState()
	require.NoError(t, err)
	currentSlot := types.Slot(4)
	require.NoError(t, beaconState.SetSlot(currentSlot))
	mockChain := &mock.ChainService{
		State: beaconState,
		Slot:  &currentSlot,
	}

	s := &Service{
		serviceCfg: &ServiceConfig{
			Database:         slasherDB,
			StateNotifier:    &mock.MockStateNotifier{},
			HeadStateFetcher: mockChain,
		},
		params:    DefaultParams(),
		blksQueue: newBlocksQueue(),
	}
	currentSlotChan := make(chan types.Slot)
	exitChan := make(chan struct{})
	go func() {
		s.processQueuedBlocks(ctx, currentSlotChan)
		exitChan <- struct{}{}
	}()
	s.blksQueue.extend([]*slashertypes.SignedBlockHeaderWrapper{
		createProposalWrapper(t, 4, 1, []byte{1}),
		createProposalWrapper(t, 4, 1, []byte{1}),
	})
	currentSlotChan <- currentSlot
	cancel()
	<-exitChan
	require.LogsDoNotContain(t, hook, "Proposer slashing detected")
}

func createProposalWrapper(t *testing.T, slot types.Slot, proposerIndex types.ValidatorIndex, signingRoot []byte) *slashertypes.SignedBlockHeaderWrapper {
	header := &ethpb.BeaconBlockHeader{
		Slot:          slot,
		ProposerIndex: proposerIndex,
		ParentRoot:    params.BeaconConfig().ZeroHash[:],
		StateRoot:     bytesutil.PadTo(signingRoot, 32),
		BodyRoot:      params.BeaconConfig().ZeroHash[:],
	}
	signRoot, err := header.HashTreeRoot()
	require.NoError(t, err)
	fakeSig := make([]byte, 96)
	copy(fakeSig, "hello")
	return &slashertypes.SignedBlockHeaderWrapper{
		SignedBeaconBlockHeader: &ethpb.SignedBeaconBlockHeader{
			Header:    header,
			Signature: fakeSig,
		},
		SigningRoot: signRoot,
	}
}
