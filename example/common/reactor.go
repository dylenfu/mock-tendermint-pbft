package common

import (
	"context"
	"github.com/fortytw2/leaktest"
	"github.com/stretchr/testify/require"
	"github.com/tendermint/tendermint/consensus"
	"github.com/tendermint/tendermint/p2p"
	"github.com/tendermint/tendermint/p2p/p2ptest"
	tmcons "github.com/tendermint/tendermint/proto/consensus"
	"github.com/tendermint/tendermint/types"
	"testing"
)

type ReactorTestSuite struct {
	network             *p2ptest.Network
	States              map[p2p.NodeID]*consensus.State
	Reactors            map[p2p.NodeID]*consensus.Reactor
	Subs                map[p2p.NodeID]types.Subscription
	stateChannels       map[p2p.NodeID]*p2p.Channel
	dataChannels        map[p2p.NodeID]*p2p.Channel
	voteChannels        map[p2p.NodeID]*p2p.Channel
	voteSetBitsChannels map[p2p.NodeID]*p2p.Channel
}

func ReactorSetup(t *testing.T, numNodes int, states []*consensus.State, size int) *ReactorTestSuite {
	t.Helper()

	rts := &ReactorTestSuite{
		network:  p2ptest.MakeNetwork(t, p2ptest.NetworkOptions{NumNodes: numNodes}),
		States:   make(map[p2p.NodeID]*consensus.State),
		Reactors: make(map[p2p.NodeID]*consensus.Reactor, numNodes),
		Subs:     make(map[p2p.NodeID]types.Subscription, numNodes),
	}

	rts.stateChannels = rts.network.MakeChannelsNoCleanup(t, consensus.StateChannel, new(tmcons.Message), size)
	rts.dataChannels = rts.network.MakeChannelsNoCleanup(t, consensus.DataChannel, new(tmcons.Message), size)
	rts.voteChannels = rts.network.MakeChannelsNoCleanup(t, consensus.VoteChannel, new(tmcons.Message), size)
	rts.voteSetBitsChannels = rts.network.MakeChannelsNoCleanup(t, consensus.VoteSetBitsChannel, new(tmcons.Message), size)

	i := 0
	for nodeID, node := range rts.network.Nodes {
		state := states[i]

		reactor := consensus.NewReactor(
			state.Logger.With("node", nodeID),
			state,
			rts.stateChannels[nodeID],
			rts.dataChannels[nodeID],
			rts.voteChannels[nodeID],
			rts.voteSetBitsChannels[nodeID],
			node.MakePeerUpdates(t),
			true,
		)

		reactor.SetEventBus(state.EventBus)

		blocksSub, err := state.EventBus.Subscribe(context.Background(), testSubscriber, types.EventQueryNewBlock, size)
		require.NoError(t, err)

		rts.States[nodeID] = state
		rts.Subs[nodeID] = blocksSub
		rts.Reactors[nodeID] = reactor

		// simulate handle initChain in handshake
		if state.State.LastBlockHeight == 0 {
			require.NoError(t, state.BlockExec.Store().Save(state.State))
		}

		require.NoError(t, reactor.Start())
		require.True(t, reactor.IsRunning())

		i++
	}

	require.Len(t, rts.Reactors, numNodes)

	// start the in-memory network and connect all peers with each other
	rts.network.Start(t)

	t.Cleanup(func() {
		for nodeID, r := range rts.Reactors {
			require.NoError(t, rts.States[nodeID].EventBus.Stop())
			require.NoError(t, r.Stop())
			require.False(t, r.IsRunning())
		}

		leaktest.Check(t)
	})

	return rts
}
