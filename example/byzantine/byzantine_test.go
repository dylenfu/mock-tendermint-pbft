package byzantine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	abcicli "github.com/tendermint/tendermint/abci/client"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/consensus"
	"github.com/tendermint/tendermint/evidence"
	"github.com/tendermint/tendermint/example/common"
	"github.com/tendermint/tendermint/libs/log"
	tmsync "github.com/tendermint/tendermint/libs/sync"
	mempl "github.com/tendermint/tendermint/mempool"
	"github.com/tendermint/tendermint/p2p"
	tmcons "github.com/tendermint/tendermint/proto/consensus"
	tmproto "github.com/tendermint/tendermint/proto/types"
	sm "github.com/tendermint/tendermint/state"
	"github.com/tendermint/tendermint/store"
	"github.com/tendermint/tendermint/types"
	dbm "github.com/tendermint/tm-db"
)

var (
	errPubKeyIsNotSet = errors.New("pubkey is not set. Look for \"Can't get private validator pubkey\" errors")
)

// Byzantine node sends two different prevotes (nil and blockID) to the same
// validator.
// go test -v github.com/tendermint/tendermint/example/byzantine -run TestByzantine
func TestByzantine(t *testing.T) {
	config := common.ConfigSetup(t)

	nValidators := 4
	prevoteHeight := int64(2)
	testName := "consensus_byzantine_test"
	tickerFunc := common.NewMockTickerFunc(true)
	appFunc := common.NewCounter

	genDoc, privVals := common.RandGenesisDoc(config, nValidators, false, 30)
	states := make([]*consensus.State, nValidators)

	for i := 0; i < nValidators; i++ {
		func() {
			logger := common.ConsensusLogger().With("test", "byzantine", "validator", i)
			stateDB := dbm.NewMemDB() // each state needs its own db
			stateStore := sm.NewStore(stateDB)
			state, _ := stateStore.LoadFromDBOrGenesisDoc(genDoc)

			thisConfig := common.ResetConfig(fmt.Sprintf("%s_%d", testName, i))
			defer os.RemoveAll(thisConfig.RootDir)

			common.EnsureDir(path.Dir(thisConfig.Consensus.WalFile()), 0700) // dir for wal
			app := appFunc()
			vals := types.TM2PB.ValidatorUpdates(state.Validators)
			app.InitChain(abci.RequestInitChain{Validators: vals})

			blockDB := dbm.NewMemDB()
			blockStore := store.NewBlockStore(blockDB)

			// one for mempool, one for consensus
			mtx := new(tmsync.RWMutex)
			proxyAppConnMem := abcicli.NewLocalClient(mtx, app)
			proxyAppConnCon := abcicli.NewLocalClient(mtx, app)

			// Make Mempool
			mempool := mempl.NewCListMempool(thisConfig.Mempool, proxyAppConnMem, 0)
			mempool.SetLogger(log.TestingLogger().With("module", "mempool"))
			if thisConfig.Consensus.WaitForTxs() {
				mempool.EnableTxsAvailable()
			}

			// Make a full instance of the evidence pool
			evidenceDB := dbm.NewMemDB()
			evpool, err := evidence.NewPool(logger.With("module", "evidence"), evidenceDB, stateStore, blockStore)
			require.NoError(t, err)

			// Make State
			blockExec := sm.NewBlockExecutor(stateStore, log.TestingLogger(), proxyAppConnCon, mempool, evpool)
			cs := consensus.NewState(thisConfig.Consensus, state, blockExec, blockStore, mempool, evpool)
			cs.SetLogger(cs.Logger)
			// set private validator
			pv := privVals[i]
			cs.SetPrivValidator(pv)

			eventBus := types.NewEventBus()
			eventBus.SetLogger(log.TestingLogger().With("module", "events"))
			err = eventBus.Start()
			require.NoError(t, err)
			cs.SetEventBus(eventBus)

			cs.SetTimeoutTicker(tickerFunc())
			cs.SetLogger(logger)

			states[i] = cs
		}()
	}

	rts := common.ReactorSetup(t, nValidators, states, 100) // buffer must be large enough to not deadlock

	var bzNodeID p2p.NodeID

	// Set the first state's reactor as the dedicated byzantine reactor and grab
	// the NodeID that corresponds to the state so we can reference the reactor.
	bzNodeState := states[0]
	for nID, s := range rts.States {
		if s == bzNodeState {
			bzNodeID = nID
			break
		}
	}

	bzReactor := rts.Reactors[bzNodeID]

	// alter prevote so that the byzantine node double votes when height is 2
	bzNodeState.DoPrevote = func(height int64, round int32) {
		// allow first height to happen normally so that byzantine validator is no longer proposer
		if height == prevoteHeight {
			prevote1, err := bzNodeState.SignVote(
				tmproto.PrevoteType,
				bzNodeState.ProposalBlock.Hash(),
				bzNodeState.ProposalBlockParts.Header(),
			)
			require.NoError(t, err)

			prevote2, err := bzNodeState.SignVote(tmproto.PrevoteType, nil, types.PartSetHeader{})
			require.NoError(t, err)

			// send two votes to all peers (1st to one half, 2nd to another half)
			i := 0
			for _, ps := range bzReactor.Peers {
				if i < len(bzReactor.Peers)/2 {
					bzNodeState.Logger.Info("signed and pushed vote", "vote", prevote1, "peer", ps.PeerID)
					bzReactor.VoteCh.Out <- p2p.Envelope{
						To: ps.PeerID,
						Message: &tmcons.Vote{
							Vote: prevote1.ToProto(),
						},
					}
				} else {
					bzNodeState.Logger.Info("signed and pushed vote", "vote", prevote2, "peer", ps.PeerID)
					bzReactor.VoteCh.Out <- p2p.Envelope{
						To: ps.PeerID,
						Message: &tmcons.Vote{
							Vote: prevote2.ToProto(),
						},
					}
				}

				i++
			}
		} else {
			bzNodeState.Logger.Info("behaving normally")
			bzNodeState.DefaultDoPrevote(height, round)
		}
	}

	// Introducing a lazy proposer means that the time of the block committed is
	// different to the timestamp that the other nodes have. This tests to ensure
	// that the evidence that finally gets proposed will have a valid timestamp.
	// lazyProposer := states[1]
	lazyNodeState := states[1]

	lazyNodeState.DecideProposal = func(height int64, round int32) {
		lazyNodeState.Logger.Info("Lazy Proposer proposing condensed commit")
		require.NotNil(t, lazyNodeState.PrivValidator)

		var commit *types.Commit
		switch {
		case lazyNodeState.Height == lazyNodeState.State.InitialHeight:
			// We're creating a proposal for the first block.
			// The commit is empty, but not nil.
			commit = types.NewCommit(0, 0, types.BlockID{}, nil)
		case lazyNodeState.LastCommit.HasTwoThirdsMajority():
			// Make the commit from LastCommit
			commit = lazyNodeState.LastCommit.MakeCommit()
		default: // This shouldn't happen.
			lazyNodeState.Logger.Error("enterPropose: Cannot propose anything: No commit for the previous block")
			return
		}

		// omit the last signature in the commit
		commit.Signatures[len(commit.Signatures)-1] = types.NewCommitSigAbsent()

		if lazyNodeState.PrivValidatorPubKey == nil {
			// If this node is a validator & proposer in the current round, it will
			// miss the opportunity to create a block.
			lazyNodeState.Logger.Error(fmt.Sprintf("enterPropose: %v", errPubKeyIsNotSet))
			return
		}
		proposerAddr := lazyNodeState.PrivValidatorPubKey.Address()

		block, blockParts := lazyNodeState.BlockExec.CreateProposalBlock(
			lazyNodeState.Height, lazyNodeState.State, commit, proposerAddr,
		)

		// Flush the WAL. Otherwise, we may not recompute the same proposal to sign,
		// and the privValidator will refuse to sign anything.
		if err := lazyNodeState.Wal.FlushAndSync(); err != nil {
			lazyNodeState.Logger.Error("Error flushing to disk")
		}

		// Make proposal
		propBlockID := types.BlockID{Hash: block.Hash(), PartSetHeader: blockParts.Header()}
		proposal := types.NewProposal(height, round, lazyNodeState.ValidRound, propBlockID)
		p := proposal.ToProto()
		if err := lazyNodeState.PrivValidator.SignProposal(context.Background(), lazyNodeState.State.ChainID, p); err == nil {
			proposal.Signature = p.Signature

			// send proposal and block parts on internal msg queue
			lazyNodeState.SendInternalMessage(consensus.MsgInfo{&consensus.ProposalMessage{proposal}, ""})
			for i := 0; i < int(blockParts.Total()); i++ {
				part := blockParts.GetPart(i)
				lazyNodeState.SendInternalMessage(consensus.MsgInfo{&consensus.BlockPartMessage{lazyNodeState.Height, lazyNodeState.Round, part}, ""})
			}
			lazyNodeState.Logger.Info("Signed proposal", "height", height, "round", round, "proposal", proposal)
			lazyNodeState.Logger.Debug(fmt.Sprintf("Signed proposal block: %v", block))
		} else if !lazyNodeState.ReplayMode {
			lazyNodeState.Logger.Error("enterPropose: Error signing proposal", "height", height, "round", round, "err", err)
		}
	}

	for _, reactor := range rts.Reactors {
		state := reactor.State.GetState()
		reactor.SwitchToConsensus(state, false)
	}

	// Evidence should be submitted and committed at the third height but
	// we will check the first six just in case
	evidenceFromEachValidator := make([]types.Evidence, nValidators)

	wg := new(sync.WaitGroup)
	i := 0
	for _, sub := range rts.Subs {
		wg.Add(1)

		go func(j int, s types.Subscription) {
			defer wg.Done()
			for {
				select {
				case msg := <-s.Out():
					require.NotNil(t, msg)
					block := msg.Data().(types.EventDataNewBlock).Block
					if len(block.Evidence.Evidence) != 0 {
						evidenceFromEachValidator[j] = block.Evidence.Evidence[0]
						return
					}
				case <-s.Canceled():
					require.Fail(t, "subscription failed for %d", j)
					return
				}
			}
		}(i, sub)

		i++
	}

	wg.Wait()

	pubkey, err := bzNodeState.PrivValidator.GetPubKey(context.Background())
	require.NoError(t, err)

	for idx, ev := range evidenceFromEachValidator {
		if assert.NotNil(t, ev, idx) {
			ev, ok := ev.(*types.DuplicateVoteEvidence)
			assert.True(t, ok)
			assert.Equal(t, pubkey.Address(), ev.VoteA.ValidatorAddress)
			assert.Equal(t, prevoteHeight, ev.Height())
		}
	}
}
