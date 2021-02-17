package main

import (
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"time"

	"github.com/nm-morais/go-babel/pkg/errors"
	"github.com/nm-morais/go-babel/pkg/logs"
	"github.com/nm-morais/go-babel/pkg/message"
	"github.com/nm-morais/go-babel/pkg/peer"
	"github.com/nm-morais/go-babel/pkg/protocol"
	"github.com/nm-morais/go-babel/pkg/protocolManager"
	"github.com/nm-morais/go-babel/pkg/timer"
	"github.com/sirupsen/logrus"
)

const (
	protoID = 2000
	name    = "Hyparview"
)

type Hyparview struct {
	babel          protocolManager.ProtocolManager
	contactNode    peer.Peer
	activeView     []peer.Peer
	passiveView    []peer.Peer
	pendingDials   map[string]bool
	lastShuffleMsg *ShuffleMessage
	timeStart      time.Time
	logger         *logrus.Logger
	conf           HyparviewConfig
}

func NewHyparviewProtocol(contactNode peer.Peer, babel protocolManager.ProtocolManager, conf *HyparviewConfig) protocol.Protocol {
	return &Hyparview{
		contactNode:  contactNode,
		activeView:   make([]peer.Peer, 0, conf.activeViewSize),
		passiveView:  make([]peer.Peer, 0, conf.passiveViewSize),
		pendingDials: make(map[string]bool),
		logger:       logs.NewLogger(name),
		babel:        babel,
	}
}

func (h *Hyparview) ID() protocol.ID {
	return protoID
}

func (h *Hyparview) Name() string {
	return name
}

func (h *Hyparview) Logger() *logrus.Logger {
	return h.logger
}

func (h *Hyparview) Init() {
	h.babel.RegisterTimerHandler(protoID, ShuffleTimerID, h.HandleShuffleTimer)
	h.babel.RegisterMessageHandler(protoID, JoinMessage{}, h.HandleJoinMessage)
	h.babel.RegisterMessageHandler(protoID, ForwardJoinMessage{}, h.HandleForwardJoinMessage)
	h.babel.RegisterMessageHandler(protoID, ShuffleMessage{}, h.HandleShuffleMessage)
	h.babel.RegisterMessageHandler(protoID, ShuffleReplyMessage{}, h.HandleShuffleReplyMessage)
	h.babel.RegisterMessageHandler(protoID, NeighbourMessage{}, h.HandleNeighbourMessage)
	h.babel.RegisterMessageHandler(protoID, NeighbourMessageReply{}, h.HandleNeighbourReplyMessage)
	h.babel.RegisterMessageHandler(protoID, DisconnectMessage{}, h.HandleDisconnectMessage)
}

func (h *Hyparview) Start() {
	h.babel.RegisterTimer(h.ID(), ShuffleTimer{duration: 3 * time.Second})
	if peer.PeersEqual(h.babel.SelfPeer(), h.contactNode) {
		return
	}
	toSend := JoinMessage{}
	h.logger.Info("Sending join message...")
	h.babel.SendMessageSideStream(toSend, h.contactNode, h.contactNode.ToTCPAddr(), protoID, protoID)
}

func (h *Hyparview) InConnRequested(dialerProto protocol.ID, p peer.Peer) bool {
	if dialerProto != h.ID() {
		return false
	}

	if h.isPeerInView(p, h.activeView) {
		return false
	}
	if h.isActiveViewFull() || len(h.activeView)+len(h.pendingDials) >= h.conf.activeViewSize {
		return false
	}
	h.pendingDials[p.String()] = true
	h.babel.Dial(h.ID(), p, p.ToTCPAddr())
	return true
}

func (h *Hyparview) OutConnDown(p peer.Peer) {
	h.dropPeerFromActiveView(p)
	h.logger.Errorf("Peer %s down", p.String())
	h.logHyparviewState()
}

func (h *Hyparview) DialSuccess(sourceProto protocol.ID, p peer.Peer) bool {
	iPeer := p
	delete(h.pendingDials, p.String())
	if sourceProto != h.ID() {
		return false
	}

	if h.isPeerInView(p, h.activeView) {
		h.logger.Info("Dialed node is already on active view")
		return true
	}

	h.dropPeerFromPassiveView(iPeer)
	if h.isActiveViewFull() {
		h.dropRandomElemFromActiveView()
	}

	h.addPeerToActiveView(iPeer)
	h.logHyparviewState()

	return true
}

func (h *Hyparview) DialFailed(p peer.Peer) {
	delete(h.pendingDials, p.String())
	h.logger.Errorf("Failed to dial peer %s", p.String())
	h.logHyparviewState()
}

func (h *Hyparview) MessageDelivered(msg message.Message, p peer.Peer) {
	if msg.Type() == DisconnectMessageType {
		h.babel.Disconnect(h.ID(), p)
		h.logger.Infof("Disconnecting from %s", p.String())
	}
	h.logger.Infof("Message %+v was sent to %s", msg, p.String())
}

func (h *Hyparview) MessageDeliveryErr(msg message.Message, p peer.Peer, err errors.Error) {
	h.logger.Warnf("Message %s was not sent to %s because: %s", reflect.TypeOf(msg), p.String(), err.Reason())
}

// ---------------- Protocol handlers (messages) ----------------

func (h *Hyparview) HandleJoinMessage(sender peer.Peer, msg message.Message) {
	h.logger.Info("Received join message")
	if h.isActiveViewFull() {
		h.dropRandomElemFromActiveView()
	}
	iPeer := sender
	h.dialNodeToActiveView(iPeer)
	if len(h.activeView) > 0 {
		toSend := ForwardJoinMessage{
			TTL:            uint32(h.conf.ARWL),
			OriginalSender: sender,
		}
		for _, activePeer := range h.activeView {
			h.logger.Infof("Sending ForwardJoin (original=%s) message to: %s", sender.String(), activePeer.String())
			h.sendMessage(toSend, activePeer)
		}
	} else {
		h.logger.Warn("Did not send forwardJoin messages because i do not have enough peers")
	}
}

func (h *Hyparview) HandleForwardJoinMessage(sender peer.Peer, msg message.Message) {
	fwdJoinMsg := msg.(ForwardJoinMessage)
	iPeer := sender
	h.logger.Infof("Received forward join message with ttl = %d from %s", fwdJoinMsg.TTL, sender.String())

	if fwdJoinMsg.TTL == 0 || len(h.activeView) == 1 {
		h.logger.Errorf("Accepting forwardJoin message from %s", fwdJoinMsg.OriginalSender.String())
		h.dialNodeToActiveView(fwdJoinMsg.OriginalSender)
		return
	}

	if fwdJoinMsg.TTL == uint32(h.conf.PRWL) {
		if !h.isPeerInView(fwdJoinMsg.OriginalSender, h.passiveView) && !h.isPeerInView(
			fwdJoinMsg.OriginalSender,
			h.activeView,
		) {
			if h.isPassiveViewFull() {
				h.dropRandomElemFromPassiveView()
			}
			h.addPeerToPassiveView(fwdJoinMsg.OriginalSender)
		}
	}

	rndNodes := h.getRandomElementsFromView(1, h.activeView, fwdJoinMsg.OriginalSender, iPeer)
	if len(rndNodes) == 0 { // only know original sender, act as if join message
		h.logger.Errorf("Cannot forward forwardJoin message, dialing %s", fwdJoinMsg.OriginalSender.String())
		h.dialNodeToActiveView(fwdJoinMsg.OriginalSender)
		return
	}

	toSend := ForwardJoinMessage{
		TTL:            fwdJoinMsg.TTL - 1,
		OriginalSender: fwdJoinMsg.OriginalSender,
	}

	h.logger.Infof(
		"Forwarding forwardJoin (original=%s) with TTL=%d message to : %s",
		fwdJoinMsg.OriginalSender.String(),
		toSend.TTL,
		rndNodes[0].String(),
	)
	h.sendMessage(toSend, rndNodes[0])
}

func (h *Hyparview) HandleNeighbourMessage(sender peer.Peer, msg message.Message) {
	h.logger.Info("Received neighbor message")
	neighborMsg := msg.(NeighbourMessage)
	if neighborMsg.HighPrio {
		reply := NeighbourMessageReply{
			Accepted: true,
		}
		h.dropRandomElemFromActiveView()
		h.pendingDials[sender.String()] = true
		h.sendMessageTmpTransport(reply, sender)
	} else {
		if len(h.activeView) < h.conf.activeViewSize {
			reply := NeighbourMessageReply{
				Accepted: true,
			}
			h.pendingDials[sender.String()] = true
			h.sendMessageTmpTransport(reply, sender)
		} else {
			reply := NeighbourMessageReply{
				Accepted: false,
			}
			h.sendMessageTmpTransport(reply, sender)
		}
	}
}

func (h *Hyparview) HandleNeighbourReplyMessage(sender peer.Peer, msg message.Message) {
	h.logger.Info("Received neighbor reply message")
	neighborReplyMsg := msg.(NeighbourMessageReply)
	if neighborReplyMsg.Accepted {
		h.dialNodeToActiveView(sender)
	}
}

func (h *Hyparview) HandleShuffleMessage(sender peer.Peer, msg message.Message) {
	shuffleMsg := msg.(ShuffleMessage)
	if shuffleMsg.TTL > 0 {
		if len(h.activeView) > 1 {
			toSend := ShuffleMessage{
				ID:    shuffleMsg.ID,
				TTL:   shuffleMsg.TTL - 1,
				Peers: shuffleMsg.Peers,
			}
			rndNodes := h.getRandomElementsFromView(1, h.activeView, h.babel.SelfPeer(), sender)
			if len(rndNodes) != 0 {
				h.logger.Debug("Forwarding shuffle message to :", rndNodes[0].String())
				h.sendMessage(toSend, rndNodes[0])
				return
			}
		}
	}

	//  TTL is 0 or have no nodes to forward to
	//  select random nr of hosts from passive view
	exclusions := append(shuffleMsg.Peers, h.babel.SelfPeer(), sender)
	toSend := h.getRandomElementsFromView(len(shuffleMsg.Peers), h.passiveView, exclusions...)
	for _, receivedHost := range shuffleMsg.Peers {
		if h.babel.SelfPeer().String() == receivedHost.String() {
			continue
		}

		if h.isPeerInView(receivedHost, h.activeView) || h.isPeerInView(receivedHost, h.passiveView) {
			continue
		}

		if h.isPassiveViewFull() { // if passive view is not full, skip check and add directly
			found := false
			for _, sentNode := range toSend {
				if h.dropPeerFromPassiveView(sentNode) {
					found = true
					break
				}
			}
			if !found {
				h.dropRandomElemFromPassiveView() // drop random element to make space
			}
		}
		h.addPeerToPassiveView(receivedHost)
	}
	reply := ShuffleReplyMessage{
		Peers: toSend,
	}
	h.sendMessageTmpTransport(reply, sender)
}

func (h *Hyparview) HandleShuffleReplyMessage(sender peer.Peer, m message.Message) {
	h.logger.Info("Received shuffle reply message")
	shuffleReplyMsg := m.(ShuffleReplyMessage)

	for _, receivedHost := range shuffleReplyMsg.Peers {
		if h.babel.SelfPeer().String() == receivedHost.String() {
			continue
		}

		if h.isPeerInView(receivedHost, h.activeView) || h.isPeerInView(receivedHost, h.passiveView) {
			continue
		}
		if h.isPassiveViewFull() { // if passive view is not full, skip check and add directly
			if h.lastShuffleMsg != nil && shuffleReplyMsg.ID == h.lastShuffleMsg.ID {
				for i, sentPeer := range h.lastShuffleMsg.Peers {
					if h.dropPeerFromPassiveView(sentPeer) {
						h.lastShuffleMsg.Peers = append(h.lastShuffleMsg.Peers[:i], h.lastShuffleMsg.Peers[i:]...)
						break
					}
					h.lastShuffleMsg.Peers = append(h.lastShuffleMsg.Peers[:i], h.lastShuffleMsg.Peers[i:]...)
				}
			} else {
				h.dropRandomElemFromPassiveView() // drop random element to make space
			}
		}
		h.addPeerToPassiveView(receivedHost)
	}
	h.lastShuffleMsg = nil
}

// ---------------- Protocol handlers (timers) ----------------

func (h *Hyparview) HandleShuffleTimer(t timer.Timer) {
	h.logger.Info("Shuffle timer trigger")
	toWait := time.Duration(h.conf.MinShuffleTimerDurationSeconds)*time.Second + time.Duration(float32(time.Second)*rand.Float32())
	h.babel.RegisterTimer(h.ID(), ShuffleTimer{duration: toWait})

	if time.Since(h.timeStart) > time.Duration(h.conf.joinTimeSeconds)*time.Second {
		if len(h.activeView) == 0 && len(h.passiveView) == 0 && !peer.PeersEqual(h.babel.SelfPeer(), h.contactNode) {
			toSend := JoinMessage{}
			h.pendingDials[h.contactNode.String()] = true
			h.babel.SendMessageSideStream(toSend, h.contactNode, h.contactNode.ToUDPAddr(), protoID, protoID)
			return
		}
		if !h.isActiveViewFull() && len(h.pendingDials)+len(h.activeView) <= h.conf.activeViewSize && len(h.passiveView) > 0 {
			h.logger.Warn("Promoting node from passive view to active view")
			aux := h.getRandomElementsFromView(1, h.passiveView)
			if len(aux) > 0 {
				h.dialNodeToActiveView(aux[0])
			}
		}
	}

	if len(h.activeView) == 0 {
		h.logger.Info("No nodes to send shuffle message message to")
		return
	}

	passiveViewRandomPeers := h.getRandomElementsFromView(h.conf.Kp-1, h.passiveView)
	activeViewRandomPeers := h.getRandomElementsFromView(h.conf.Ka, h.activeView)
	peers := append(passiveViewRandomPeers, activeViewRandomPeers...)
	peers = append(peers, h.babel.SelfPeer())
	randID := getRandInt(math.MaxUint32)
	toSend := ShuffleMessage{
		ID:    uint32(randID),
		TTL:   uint32(h.conf.PRWL),
		Peers: peers,
	}
	h.lastShuffleMsg = &toSend
	rndNode := h.activeView[getRandInt(len(h.activeView))]
	h.logger.Info("Sending shuffle message to: ", rndNode.String())
	h.sendMessage(toSend, rndNode)
}

func (h *Hyparview) HandleDisconnectMessage(sender peer.Peer, m message.Message) {
	h.logger.Warn("Got Disconnect message")
	iPeer := sender
	h.dropPeerFromActiveView(iPeer)
	if h.isPassiveViewFull() {
		h.dropRandomElemFromPassiveView()
	}
	h.addPeerToPassiveView(iPeer)
}

// ---------------- Auxiliary functions ----------------

func (h *Hyparview) logHyparviewState() {
	h.logger.Info("------------- Hyparview state -------------")
	var toLog string
	toLog = "Active view : "
	for _, p := range h.activeView {
		toLog += fmt.Sprintf("%s, ", p.String())
	}
	h.logger.Info(toLog)
	toLog = "Passive view : "
	for _, p := range h.passiveView {
		toLog += fmt.Sprintf("%s, ", p.String())
	}
	h.logger.Info(toLog)
	toLog = "Pending dials: "
	for p := range h.pendingDials {
		toLog += fmt.Sprintf("%s, ", p)
	}
	h.logger.Info(toLog)
	h.logger.Info("-------------------------------------------")
}

func (h *Hyparview) getRandomElementsFromView(amount int, view []peer.Peer, exclusions ...peer.Peer) []peer.Peer {
	dest := make([]peer.Peer, len(view))
	perm := rand.Perm(len(view))
	for i, v := range perm {
		dest[v] = view[i]
	}

	var toSend []peer.Peer

	for i := 0; i < len(dest) && len(toSend) < amount; i++ {
		excluded := false
		curr := dest[i]
		for _, exclusion := range exclusions {
			if peer.PeersEqual(exclusion, curr) { // skip exclusions
				excluded = true
				break
			}
		}
		if !excluded {
			toSend = append(toSend, curr)
		}
	}

	return toSend
}

func (h *Hyparview) isPeerInView(target peer.Peer, view []peer.Peer) bool {
	for _, p := range view {
		if peer.PeersEqual(p, target) {
			return true
		}
	}
	return false
}

func (h *Hyparview) dropPeerFromActiveView(target peer.Peer) {
	for i, p := range h.activeView {
		if peer.PeersEqual(p, target) {
			h.activeView = append(h.activeView[:i], h.activeView[i+1:]...)
		}
	}
	h.babel.Disconnect(protoID, target)
}

func (h *Hyparview) dropPeerFromPassiveView(target peer.Peer) bool {
	for i, p := range h.passiveView {
		if peer.PeersEqual(p, target) {
			h.passiveView = append(h.passiveView[:i], h.passiveView[i+1:]...)
			return true
		}
	}
	h.logHyparviewState()
	return false
}

func (h *Hyparview) dialNodeToActiveView(p peer.Peer) {
	if h.isPeerInView(p, h.activeView) || h.pendingDials[p.String()] {
		return
	}
	h.logger.Infof("dialing new node %s", p.String())
	h.babel.Dial(h.ID(), p, p.ToTCPAddr())
	h.pendingDials[p.String()] = true
	h.logHyparviewState()
}

func (h *Hyparview) addPeerToActiveView(newPeer peer.Peer) {
	if peer.PeersEqual(h.babel.SelfPeer(), newPeer) {
		h.logger.Panic("Trying to add self to active view")
	}

	if h.isActiveViewFull() {
		h.logger.Panic("Cannot add node to active pool because it is full")
	}

	if h.isPeerInView(newPeer, h.activeView) {
		h.logger.Panic("Trying to add node already in view")
	}

	if h.isPeerInView(newPeer, h.passiveView) {
		h.logger.Panic("Trying to add node to active view already in passive view")
	}

	h.logger.Warnf("Added peer %s to active view", newPeer.String())
	h.activeView = append(h.activeView, newPeer)
	h.logHyparviewState()
}

func (h *Hyparview) addPeerToPassiveView(newPeer peer.Peer) {
	if h.isPassiveViewFull() {
		h.logger.Panic("Trying to add node to view when view is full")
	}

	if peer.PeersEqual(newPeer, h.babel.SelfPeer()) {
		h.logger.Panic("trying to add self to passive view ")
	}

	if h.isPeerInView(newPeer, h.passiveView) {
		h.logger.Panic("Trying to add node already in view")
	}

	if h.isPeerInView(newPeer, h.activeView) {
		h.logger.Panic("Trying to add node to passive view already in active view")
	}

	h.logger.Warnf("Added peer %s to passive view", newPeer.String())
	h.passiveView = append(h.passiveView, newPeer)
	h.logHyparviewState()
}

func (h *Hyparview) isActiveViewFull() bool {
	return len(h.activeView) >= h.conf.activeViewSize
}

func (h *Hyparview) isPassiveViewFull() bool {
	return len(h.passiveView) >= h.conf.passiveViewSize
}

func (h *Hyparview) dropRandomElemFromActiveView() {
	toRemove := getRandInt(len(h.activeView))
	h.logger.Warnf("Dropping element %s from active view", h.activeView[toRemove].String())
	removed := h.activeView[toRemove]
	h.activeView = append(h.activeView[:toRemove], h.activeView[toRemove+1:]...)
	h.addPeerToPassiveView(removed)
	go func() {
		disconnectMsg := DisconnectMessage{}
		h.sendMessage(disconnectMsg, removed)
		h.babel.Disconnect(h.ID(), removed)
	}()
	h.logHyparviewState()
}

func (h *Hyparview) dropRandomElemFromPassiveView() {
	toRemove := getRandInt(len(h.passiveView))
	h.logger.Warnf("Dropping element %s from passive view", h.passiveView[toRemove].String())
	h.passiveView = append(h.passiveView[:toRemove], h.passiveView[toRemove+1:]...)
	h.logHyparviewState()
}

func (h *Hyparview) sendMessage(msg message.Message, target peer.Peer) {
	h.babel.SendMessage(msg, target, h.ID(), h.ID(), false)
}

func (h *Hyparview) sendMessageTmpTransport(msg message.Message, target peer.Peer) {
	h.babel.SendMessageSideStream(msg, target, target.ToTCPAddr(), h.ID(), h.ID())
}
