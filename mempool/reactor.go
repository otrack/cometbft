package mempool

import (
	"errors"
	"time"

	"fmt"

	abci "github.com/cometbft/cometbft/abci/types"
	cfg "github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/libs/log"
	cmtsync "github.com/cometbft/cometbft/libs/sync"
	"github.com/cometbft/cometbft/p2p"
	protomem "github.com/cometbft/cometbft/proto/tendermint/mempool"
	"github.com/cometbft/cometbft/types"
)

// Reactor handles mempool tx broadcasting amongst peers.
// It maintains a map from peer ID to counter, to prevent gossiping txs to the
// peers you received it from.
type Reactor struct {
	p2p.BaseReactor
	config  *cfg.MempoolConfig
	mempool Mempool
	ids     *mempoolIDs

	// `txSenders` maps every received transaction to the set of peer IDs that
	// have sent the transaction to this node. Sender IDs are used during
	// transaction propagation to avoid sending a transaction to a peer that
	// already has it. A sender ID is the internal peer ID used in the mempool
	// to identify the sender, storing two bytes with each transaction instead
	// of 20 bytes for the types.NodeID.
	txSenders    map[types.TxKey]map[uint16]bool
	txSendersMtx cmtsync.Mutex
}

// NewReactor returns a new Reactor with the given config and mempool.
func NewReactor(config *cfg.MempoolConfig, mempool Mempool) *Reactor {
	memR := &Reactor{
		config:    config,
		mempool:   mempool,
		ids:       newMempoolIDs(),
		txSenders: make(map[types.TxKey]map[uint16]bool),
	}
	memR.BaseReactor = *p2p.NewBaseReactor("Mempool", memR)
	memR.mempool.SetTxRemovedCallback(func(txKey types.TxKey) { memR.removeSenders(txKey) })
	return memR
}

// InitPeer implements Reactor by creating a state for the peer.
func (memR *Reactor) InitPeer(peer p2p.Peer) p2p.Peer {
	memR.ids.ReserveForPeer(peer)
	return peer
}

// SetLogger sets the Logger on the reactor and the underlying mempool.
func (memR *Reactor) SetLogger(l log.Logger) {
	memR.Logger = l
	memR.mempool.SetLogger(l)
}

// OnStart implements p2p.BaseReactor.
func (memR *Reactor) OnStart() error {
	if !memR.config.Broadcast {
		memR.Logger.Info("Tx broadcasting is disabled")
	}

	return nil
}

// GetChannels implements Reactor by returning the list of channels for this
// reactor.
func (memR *Reactor) GetChannels() []*p2p.ChannelDescriptor {
	largestTx := make([]byte, memR.config.MaxTxBytes)
	batchMsg := protomem.Message{
		Sum: &protomem.Message_Txs{
			Txs: &protomem.Txs{Txs: [][]byte{largestTx}},
		},
	}

	return []*p2p.ChannelDescriptor{
		{
			ID:                  MempoolChannel,
			Priority:            5,
			RecvMessageCapacity: batchMsg.Size(),
			MessageType:         &protomem.Message{},
		},
	}
}

// AddPeer implements Reactor.
// It starts a broadcast routine ensuring all txs are forwarded to the given peer.
func (memR *Reactor) AddPeer(peer p2p.Peer) {
	if memR.config.Broadcast {
		go memR.broadcastTxRoutine(peer)
	}
}

// RemovePeer implements Reactor.
func (memR *Reactor) RemovePeer(peer p2p.Peer, _ interface{}) {
	memR.ids.Reclaim(peer)
	// broadcast routine checks if peer is gone and returns
}

// Receive implements Reactor.
// It adds any received transactions to the mempool.
func (memR *Reactor) Receive(e p2p.Envelope) {
	memR.Logger.Debug("Receive", "src", e.Src, "chId", e.ChannelID, "msg", e.Message)
	switch msg := e.Message.(type) {
	case *protomem.Txs:
		protoTxs := msg.GetTxs()
		if len(protoTxs) == 0 {
			memR.Logger.Error("received empty txs from peer", "src", e.Src)
			return
		}

		for _, txBytes := range protoTxs {
			tx := types.Tx(txBytes)
			reqRes, err := memR.mempool.CheckTx(tx)
			if errors.Is(err, ErrTxInCache) {
				memR.Logger.Debug("Tx already exists in cache", "tx", tx.String())
			} else if err != nil {
				memR.Logger.Info("Could not check tx", "tx", tx.String(), "err", err)
			} else {
				// Record the sender only when the transaction is valid and, as
				// a consequence, added to the mempool. Senders are stored until
				// the transaction is removed from the mempool. Note that it's
				// possible a tx is still in the cache but no longer in the
				// mempool. For example, after committing a block, txs are
				// removed from mempool but not the cache.
				reqRes.SetCallback(func(res *abci.Response) {
					if res.GetCheckTx().Code == abci.CodeTypeOK {
						memR.addSender(tx.Key(), memR.ids.GetForPeer(e.Src))
					}
				})
			}
		}
	default:
		memR.Logger.Error("unknown message type", "src", e.Src, "chId", e.ChannelID, "msg", e.Message)
		memR.Switch.StopPeerForError(e.Src, fmt.Errorf("mempool cannot handle message of type: %T", e.Message))
		return
	}

	// broadcasting happens from go routines per peer
}

// PeerState describes the state of a peer.
type PeerState interface {
	GetHeight() int64
}

// Send new mempool txs to peer.
func (memR *Reactor) broadcastTxRoutine(peer p2p.Peer) {
	peerID := memR.ids.GetForPeer(peer)

	var entry *Entry
	iter := memR.mempool.NewIterator()

	for {
		// In case of both next.NextWaitChan() and peer.Quit() are variable at the same time
		if !memR.IsRunning() || !peer.IsRunning() {
			return
		}

		if entry == nil {
			// Wait until either: a mempool entry is available in the channel,
			// or the peer was disconnected, or the reactor stopped.
			select {
			case <-iter.WaitNext():
				entry = iter.NextEntry()
				if entry == nil {
					// There is no next entry, or the entry we found got removed in the
					// meantime. Try again.
					continue
				}
			case <-peer.Quit():
				return
			case <-memR.Quit():
				return
			}
		}

		// Make sure the peer is up to date.
		peerState, ok := peer.Get(types.PeerStateKey).(PeerState)
		if !ok {
			// Peer does not have a state yet. We set it in the consensus reactor, but
			// when we add peer in Switch, the order we call reactors#AddPeer is
			// different every time due to us using a map. Sometimes other reactors
			// will be initialized before the consensus reactor. We should wait a few
			// milliseconds and retry.
			time.Sleep(PeerCatchupSleepIntervalMS * time.Millisecond)
			continue
		}

		// If we suspect that the peer is lagging behind, at least by more than
		// one block, we don't send the transaction immediately. This code
		// reduces the mempool size and the recheck-tx rate of the receiving
		// node. See [RFC 103] for an analysis on this optimization.
		//
		// [RFC 103]: https://github.com/cometbft/cometbft/pull/735
		if peerState.GetHeight() < entry.Height()-1 {
			time.Sleep(PeerCatchupSleepIntervalMS * time.Millisecond)
			continue
		}

		// NOTE: Transaction batching was disabled due to
		// https://github.com/tendermint/tendermint/issues/5796

		if !memR.isSender(entry.tx.Key(), peerID) {
			success := peer.Send(p2p.Envelope{
				ChannelID: MempoolChannel,
				Message:   &protomem.Txs{Txs: [][]byte{entry.tx}},
			})
			if !success {
				time.Sleep(PeerCatchupSleepIntervalMS * time.Millisecond)
				continue
			}
		}

		// We are done with this entry; set it to nil to fetch the next one.
		entry = nil
	}
}

func (memR *Reactor) isSender(txKey types.TxKey, peerID uint16) bool {
	memR.txSendersMtx.Lock()
	defer memR.txSendersMtx.Unlock()

	sendersSet, ok := memR.txSenders[txKey]
	return ok && sendersSet[peerID]
}

func (memR *Reactor) addSender(txKey types.TxKey, senderID uint16) bool {
	memR.txSendersMtx.Lock()
	defer memR.txSendersMtx.Unlock()

	if sendersSet, ok := memR.txSenders[txKey]; ok {
		sendersSet[senderID] = true
		return false
	}
	memR.txSenders[txKey] = map[uint16]bool{senderID: true}
	return true
}

func (memR *Reactor) removeSenders(txKey types.TxKey) {
	memR.txSendersMtx.Lock()
	defer memR.txSendersMtx.Unlock()

	if memR.txSenders != nil {
		delete(memR.txSenders, txKey)
	}
}
