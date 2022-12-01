package eth

import (
	"fmt"
	"github.com/mantlenetworkio/mantle/l2geth/core"
	"github.com/mantlenetworkio/mantle/l2geth/core/types"

	"github.com/mantlenetworkio/mantle/l2geth/core/forkid"
	"github.com/mantlenetworkio/mantle/l2geth/log"
	"github.com/mantlenetworkio/mantle/l2geth/p2p"
	"github.com/mantlenetworkio/mantle/l2geth/p2p/enode"
)

func (pm *ProtocolManager) makeConsensusProtocol(version uint) p2p.Protocol {
	length := consensusProtocolLength

	return p2p.Protocol{
		Name:    consensusProtocolName,
		Version: version,
		Length:  length,
		Run:     pm.consensusHandler,
		NodeInfo: func() interface{} {
			return pm.NodeInfo()
		},
		PeerInfo: func(id enode.ID) interface{} {
			if p := pm.peers.Peer(fmt.Sprintf("%x", id[:8])); p != nil {
				return p.Info()
			}
			return nil
		},
	}
}

func (pm *ProtocolManager) removePeerTmp(id string) {
	// Short circuit if the peer was already removed
	peer := pm.peersTmp.Peer(id)
	if peer == nil {
		return
	}
	log.Debug("Removing Ethereum consensus peer", "peer", id)

	if err := pm.peersTmp.Unregister(id); err != nil {
		log.Error("Consensus Peer removal failed", "peer", id, "err", err)
	}
	// Hard disconnect at the networking layer
	if peer != nil {
		peer.Peer.Disconnect(p2p.DiscUselessPeer)
	}
}

func (pm *ProtocolManager) consensusHandler(peer *p2p.Peer, rw p2p.MsgReadWriter) error {
	p := pm.newPeer(int(eth64), peer, rw)
	select {
	case pm.newPeerCh <- p:
		pm.wg.Add(1)
		defer pm.wg.Done()
		// Ignore maxPeers if this is a trusted peer
		if pm.peersTmp.Len() >= pm.maxPeers && !p.Peer.Info().Network.Trusted {
			return p2p.DiscTooManyPeers
		}
		p.Log().Debug("Ethereum consensus peer connected", "name", p.Name())
		// Execute the Ethereum handshake
		var (
			genesis = pm.blockchain.Genesis()
			head    = pm.blockchain.CurrentHeader()
			hash    = head.Hash()
			number  = head.Number.Uint64()
			td      = pm.blockchain.GetTd(hash, number)
		)
		if err := p.Handshake(pm.networkID, td, hash, genesis.Hash(), forkid.NewID(pm.blockchain), pm.forkFilter); err != nil {
			p.Log().Debug("Ethereum handshake failed", "err", err)
			return err
		}
		if rw, ok := p.rw.(*meteredMsgReadWriter); ok {
			rw.Init(p.version)
		}
		// Register the peer locally
		if err := pm.peersTmp.Register(p); err != nil {
			p.Log().Error("Ethereum peer registration failed", "err", err)
			return err
		}
		defer pm.removePeerTmp(p.id)

		// Handle incoming messages until the connection is torn down
		for {
			if err := pm.handleConsensusMsg(p); err != nil {
				p.Log().Debug("Ethereum consensus message handling failed", "err", err)
				return err
			}
		}
	case <-pm.quitSync:
		return p2p.DiscQuitting
	}
}

func (pm *ProtocolManager) handleConsensusMsg(p *peer) error {
	// Read the next message from the remote peer, and ensure it's fully consumed
	msg, err := p.rw.ReadMsg()
	if err != nil {
		return err
	}
	if msg.Size > consensusMaxMsgSize {
		return errResp(ErrMsgTooLarge, "%v > %v", msg.Size, protocolMaxMsgSize)
	}
	defer msg.Discard()

	// Handle the message depending on its contents
	switch {
	case msg.Code == BatchPeriodStartMsg:
		// todo: BatchPeriodStartMsg handle
		log.Info("Batch Period Start Msg")
	case msg.Code == BatchPeriodEndMsg:
		// todo: BatchPeriodEndMsg handle
		log.Info("Batch Period End Msg")

	default:
		return errResp(ErrInvalidMsgCode, "%v", msg.Code)
	}

	return nil
}

// ---------------------------- Consensus Control Messages ----------------------------

// BatchPeriodStartMsg will
func (pm *ProtocolManager) batchPeriodStartMsgBroadcastLoop() {
	// automatically stops if unsubscribe
	for obj := range pm.batchStartMsgSub.Chan() {
		if se, ok := obj.Data.(core.BatchPeriodStartEvent); ok {
			pm.BroadcastBatchPeriodStartMsg(se.Msg) // First propagate block to peers
		}
	}
}

func (pm *ProtocolManager) BroadcastBatchPeriodStartMsg(msg *types.BatchPeriodStartMsg) {
	peers := pm.peersTmp.PeersWithoutStartMsg(msg.BatchIndex)
	for _, p := range peers {
		p.AsyncSendBatchPeriodStartMsg(msg)
	}
	log.Trace("Broadcast batch period start msg")
}

func (p *peer) AsyncSendBatchPeriodStartMsg(msg *types.BatchPeriodStartMsg) {
	select {
	case p.queuedStartMsg <- msg:
		p.knowStartMsg.Add(msg.BatchIndex)
		for p.knowStartMsg.Cardinality() >= maxKnownPrs {
			p.knowStartMsg.Pop()
		}

	default:
		p.Log().Debug("Dropping batch period start msg propagation", "batch index", msg.BatchIndex)
	}
}

// BatchPeriodEndMsg
func (pm *ProtocolManager) batchPeriodEndMsgBroadcastLoop() {
	// automatically stops if unsubscribe
	for obj := range pm.batchEndMsgSub.Chan() {
		if ee, ok := obj.Data.(core.BatchPeriodEndEvent); ok {
			pm.BroadcastBatchPeriodEndMsg(ee.Msg) // First propagate block to peers
		}
	}
}

func (pm *ProtocolManager) BroadcastBatchPeriodEndMsg(msg *types.BatchPeriodEndMsg) {
	peers := pm.peersTmp.PeersWithoutEndMsg(msg.BatchIndex)
	for _, p := range peers {
		p.AsyncSendBatchPeriodEndMsg(msg)
	}
	log.Trace("Broadcast batch period end msg")
}

func (p *peer) AsyncSendBatchPeriodEndMsg(msg *types.BatchPeriodEndMsg) {
	select {
	case p.queuedEndMsg <- msg:
		p.knowEndMsg.Add(msg.BatchIndex)
		for p.knowEndMsg.Cardinality() >= maxKnownPrs {
			p.knowEndMsg.Pop()
		}

	default:
		p.Log().Debug("Dropping batch period end msg propagation", "batch index", msg.BatchIndex)
	}
}

// ---------------------------- Proposers ----------------------------

// SendBatchPeriodStart sends a batch of transaction receipts, corresponding to the
// ones requested from an already RLP encoded format.
func (p *peer) SendBatchPeriodStart(bs *types.BatchPeriodStartMsg) error {
	p.knowStartMsg.Add(bs.BatchIndex)
	// Mark all the producers as known, but ensure we don't overflow our limits
	for p.knowStartMsg.Cardinality() >= maxKnownPrs {
		p.knowStartMsg.Pop()
	}
	return p2p.Send(p.rw, BatchPeriodStartMsg, bs)
}

func (p *peer) SendBatchPeriodEnd(be *types.BatchPeriodEndMsg) error {
	p.knowEndMsg.Add(be.BatchIndex)
	// Mark all the producers as known, but ensure we don't overflow our limits
	for p.knowEndMsg.Cardinality() >= maxKnownPrs {
		p.knowEndMsg.Pop()
	}
	return p2p.Send(p.rw, BatchPeriodEndMsg, be)
}
