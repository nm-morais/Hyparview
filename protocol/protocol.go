package protocol

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net"
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
	protoID = 1000
	name    = "Hyparview"
)

type HyparviewConfig struct {
	SelfPeer struct {
		AnalyticsPort int    `yaml:"analyticsPort"`
		Port          int    `yaml:"port"`
		Host          string `yaml:"host"`
	} `yaml:"self"`
	BootstrapPeers []struct {
		Port          int    `yaml:"port"`
		Host          string `yaml:"host"`
		AnalyticsPort int    `yaml:"analyticsPort"`
	} `yaml:"bootstrapPeers"`

	DialTimeoutMiliseconds         int    `yaml:"dialTimeoutMiliseconds"`
	LogFolder                      string `yaml:"logFolder"`
	JoinTimeSeconds                int    `yaml:"joinTimeSeconds"`
	ActiveViewSize                 int    `yaml:"activeViewSize"`
	PassiveViewSize                int    `yaml:"passiveViewSize"`
	ARWL                           int    `yaml:"arwl"`
	PRWL                           int    `yaml:"pwrl"`
	Ka                             int    `yaml:"ka"`
	Kp                             int    `yaml:"kp"`
	MinShuffleTimerDurationSeconds int    `yaml:"minShuffleTimerDurationSeconds"`
	DebugTimerDurationSeconds      int    `yaml:"debugTimerDurationSeconds"`
}
type Hyparview struct {
	babel                 protocolManager.ProtocolManager
	lastShuffleMsg        *ShuffleMessage
	timeStart             time.Time
	logger                *logrus.Logger
	conf                  *HyparviewConfig
	selfIsBootstrap       bool
	bootstrapNodes        []peer.Peer
	danglingNeighCounters map[string]int
	*HyparviewState
}

func NewHyparviewProtocol(babel protocolManager.ProtocolManager, conf *HyparviewConfig) protocol.Protocol {
	logger := logs.NewLogger(name)
	selfIsBootstrap := false
	bootstrapNodes := []peer.Peer{}
	for _, p := range conf.BootstrapPeers {
		boostrapNode := peer.NewPeer(net.ParseIP(p.Host), uint16(p.Port), uint16(p.AnalyticsPort))
		bootstrapNodes = append(bootstrapNodes, boostrapNode)
		if peer.PeersEqual(babel.SelfPeer(), boostrapNode) {
			selfIsBootstrap = true
			break
		}
	}
	logger.Infof("Starting with selfPeer:= %+v", babel.SelfPeer())
	logger.Infof("%+v", babel.SelfPeer().AnalyticsPort())
	for _, b := range bootstrapNodes {
		logger.Infof("%+v", b.AnalyticsPort())
	}
	logger.Infof("Starting with bootstraps:= %+v", bootstrapNodes)
	logger.Infof("Starting with selfIsBootstrap:= %+v", selfIsBootstrap)
	return &Hyparview{
		babel:          babel,
		lastShuffleMsg: nil,
		timeStart:      time.Time{},
		logger:         logger,
		conf:           conf,

		bootstrapNodes:        bootstrapNodes,
		selfIsBootstrap:       selfIsBootstrap,
		danglingNeighCounters: make(map[string]int),
		HyparviewState: &HyparviewState{
			activeView: &View{
				capacity: conf.ActiveViewSize,
				asArr:    []*PeerState{},
				asMap:    map[string]*PeerState{},
			},
			passiveView: &View{
				capacity: conf.PassiveViewSize,
				asArr:    []*PeerState{},
				asMap:    map[string]*PeerState{},
			},
		},
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
	h.babel.RegisterTimerHandler(protoID, PromoteTimerID, h.HandlePromoteTimer)
	h.babel.RegisterTimerHandler(protoID, DebugTimerID, h.HandleDebugTimer)
	h.babel.RegisterTimerHandler(protoID, MaintenanceTimerID, h.HandleMaintenanceTimer)

	h.babel.RegisterMessageHandler(protoID, JoinMessage{}, h.HandleJoinMessage)
	h.babel.RegisterMessageHandler(protoID, ForwardJoinMessage{}, h.HandleForwardJoinMessage)
	h.babel.RegisterMessageHandler(protoID, ForwardJoinMessageReply{}, h.HandleForwardJoinMessageReply)
	h.babel.RegisterMessageHandler(protoID, ShuffleMessage{}, h.HandleShuffleMessage)
	h.babel.RegisterMessageHandler(protoID, ShuffleReplyMessage{}, h.HandleShuffleReplyMessage)
	h.babel.RegisterMessageHandler(protoID, NeighbourMessage{}, h.HandleNeighbourMessage)
	h.babel.RegisterMessageHandler(protoID, NeighbourMaintenanceMessage{}, h.HandleNeighbourMaintenanceMessage)
	h.babel.RegisterMessageHandler(protoID, NeighbourMessageReply{}, h.HandleNeighbourReplyMessage)
	h.babel.RegisterMessageHandler(protoID, DisconnectMessage{}, h.HandleDisconnectMessage)
}

func (h *Hyparview) Start() {
	h.logger.Infof("Starting with confs: %+v", h.conf)
	h.babel.RegisterTimer(h.ID(), ShuffleTimer{duration: 3 * time.Second})
	h.babel.RegisterPeriodicTimer(h.ID(), PromoteTimer{duration: 7 * time.Second}, true)
	h.babel.RegisterPeriodicTimer(h.ID(), DebugTimer{time.Duration(h.conf.DebugTimerDurationSeconds) * time.Second}, true)
	h.babel.RegisterPeriodicTimer(h.ID(), MaintenanceTimer{1 * time.Second}, false)
	h.joinOverlay()
	h.timeStart = time.Now()
}

func (h *Hyparview) joinOverlay() {
	if time.Since(h.timeStart) < time.Duration(h.conf.JoinTimeSeconds)*time.Second {
		h.logger.Infof("Not rejoining since not enough time has passed: %+v", h.conf.JoinTimeSeconds)
		return
	}

	if len(h.bootstrapNodes) == 0 {
		h.logger.Panic("No nodes to join overlay...")
	}
	for _, b := range h.bootstrapNodes {
		if peer.PeersEqual(b, h.babel.SelfPeer()) {
			continue
		}
		toSend := JoinMessage{}
		h.logger.Info("Joining overlay...")
		h.babel.SendMessageSideStream(toSend, b, b.ToTCPAddr(), protoID, protoID)
		return
	}
}

func (h *Hyparview) InConnRequested(dialerProto protocol.ID, p peer.Peer) bool {
	if dialerProto != h.ID() {
		h.logger.Warnf("Denying connection  from peer %+v", p)
		return false
	}

	return true
}

func (h *Hyparview) OutConnDown(p peer.Peer) {
	h.handleNodeDown(p)
	h.logger.Errorf("Peer %s out connection went down", p.String())
}

func (h *Hyparview) DialFailed(p peer.Peer) {
	h.logger.Errorf("Failed to dial peer %s", p.String())
	h.handleNodeDown(p)
}

func (h *Hyparview) handleNodeDown(p peer.Peer) {
	h.logger.Errorf("Node %s DOWN", p.String())
	defer h.logHyparviewState()
	defer h.babel.Disconnect(h.ID(), p)
	if removed := h.activeView.remove(p); removed != nil {
		if removed.outConnected {
			h.logger.Infof("Emitting Neigh down notification...")
			h.babel.SendNotification(NeighborDownNotification{
				PeerDown: p,
				View:     h.getView(),
			})
		} else {
			h.logger.Warnf("Peer in active view but was not connected")
		}
		if !h.activeView.isFull() {
			if h.passiveView.size() == 0 {
				if h.activeView.size() == 0 {
					h.joinOverlay()
				}
				return
			}
			newNeighbor := h.passiveView.getRandomElementsFromView(1)
			h.logger.Warnf("replacing downed with node %s from passive view", newNeighbor[0].String())
			h.sendMessageTmpTransport(NeighbourMessage{
				HighPrio: h.activeView.size() <= 1, // TODO review this
			}, newNeighbor[0])
		}
	} else {
		h.logger.Warnf("Peer down was not in view")
	}
}

func (h *Hyparview) logInView() {
	type viewWithLatencies []struct {
		IP      string `json:"ip,omitempty"`
		Latency int    `json:"latency,omitempty"`
	}
	toPrint := viewWithLatencies{}
	for _, p := range h.activeView.asArr {
		toPrint = append(toPrint, struct {
			IP      string "json:\"ip,omitempty\""
			Latency int    "json:\"latency,omitempty\""
		}{
			IP:      p.IP().String(),
			Latency: int(0),
		})
	}
	res, err := json.Marshal(toPrint)
	if err != nil {
		panic(err)
	}
	h.logger.Infof("<inView> %s", string(res))
}

func (h *Hyparview) DialSuccess(sourceProto protocol.ID, p peer.Peer) bool {
	if sourceProto != h.ID() {
		return false
	}
	foundPeer, found := h.activeView.get(p)
	if found {
		foundPeer.outConnected = true
		h.logger.Info("Dialed node in active view")
		h.activeView.asMap[p.String()] = foundPeer
		h.babel.SendNotification(NeighborUpNotification{
			PeerUp: foundPeer,
			View:   h.getView(),
		})
		return true
	}

	h.logger.Warnf("Disconnecting connection from peer %+v because it is not in active view", p)
	h.babel.Disconnect(h.ID(), p)
	return false
}

func (h *Hyparview) MessageDelivered(msg message.Message, p peer.Peer) {
	h.logger.Infof("Message of type [%s] body: %+v was sent to %s", reflect.TypeOf(msg), msg, p.String())
}

func (h *Hyparview) MessageDeliveryErr(msg message.Message, p peer.Peer, err errors.Error) {
	h.logger.Warnf("Message %s was not sent to %s because: %s", reflect.TypeOf(msg), p.String(), err.Reason())
	_, isNeighMsg := msg.(NeighbourMessage)
	if isNeighMsg {
		h.passiveView.remove(p)
	}
}

func (h *Hyparview) getView() map[string]peer.Peer {
	toRet := map[string]peer.Peer{}
	for _, p := range h.activeView.asArr {
		if p.outConnected {
			toRet[p.String()] = p
		}
	}
	return toRet
}

// ---------------- Protocol handlers (messages) ----------------

func (h *Hyparview) HandleJoinMessage(sender peer.Peer, msg message.Message) {
	h.logger.Infof("Received join message from %s", sender)
	if h.activeView.isFull() {
		h.dropRandomElemFromActiveView()
	}
	toSend := ForwardJoinMessage{
		TTL:            uint32(h.conf.ARWL),
		OriginalSender: sender,
	}
	h.addPeerToActiveView(sender)
	h.sendMessageTmpTransport(ForwardJoinMessageReply{}, sender)
	for _, neigh := range h.activeView.asArr {
		if peer.PeersEqual(neigh, sender) {
			continue
		}

		if neigh.outConnected {
			h.logger.Infof("Sending ForwardJoin (original=%s) message to: %s", sender.String(), neigh.String())
			h.sendMessage(toSend, neigh)
		}
	}
}

func (h *Hyparview) HandleForwardJoinMessage(sender peer.Peer, msg message.Message) {
	fwdJoinMsg := msg.(ForwardJoinMessage)
	h.logger.Infof("Received forward join message with ttl = %d, originalSender=%s from %s",
		fwdJoinMsg.TTL,
		fwdJoinMsg.OriginalSender.String(),
		sender.String())

	if fwdJoinMsg.OriginalSender == h.babel.SelfPeer() {
		h.logger.Panic("Received forward join message sent by myself")
	}

	if fwdJoinMsg.TTL == 0 || h.activeView.size() == 1 {
		if fwdJoinMsg.TTL == 0 {
			h.logger.Infof("Accepting forwardJoin message from %s, fwdJoinMsg.TTL == 0", fwdJoinMsg.OriginalSender.String())
		}
		if h.activeView.size() == 1 {
			h.logger.Infof("Accepting forwardJoin message from %s, h.activeView.size() == 1", fwdJoinMsg.OriginalSender.String())
		}
		if h.addPeerToActiveView(fwdJoinMsg.OriginalSender) {
			h.sendMessageTmpTransport(ForwardJoinMessageReply{}, fwdJoinMsg.OriginalSender)
		}
		return
	}

	if fwdJoinMsg.TTL == uint32(h.conf.PRWL) {
		h.addPeerToPassiveView(fwdJoinMsg.OriginalSender)
	}

	rndSample := h.activeView.getRandomElementsFromView(1, fwdJoinMsg.OriginalSender, sender)
	if len(rndSample) == 0 { // only know original sender, act as if join message
		h.logger.Errorf("Cannot forward forwardJoin message, dialing %s", fwdJoinMsg.OriginalSender.String())
		if h.addPeerToActiveView(fwdJoinMsg.OriginalSender) {
			h.sendMessageTmpTransport(ForwardJoinMessageReply{}, fwdJoinMsg.OriginalSender)
		}
		return
	}

	toSend := ForwardJoinMessage{
		TTL:            fwdJoinMsg.TTL - 1,
		OriginalSender: fwdJoinMsg.OriginalSender,
	}
	nodeToSendTo := rndSample[0]
	h.logger.Infof(
		"Forwarding forwardJoin (original=%s) with TTL=%d message to : %s",
		fwdJoinMsg.OriginalSender.String(),
		toSend.TTL,
		nodeToSendTo.String(),
	)
	h.sendMessage(toSend, nodeToSendTo)
}

func (h *Hyparview) HandleForwardJoinMessageReply(sender peer.Peer, msg message.Message) {
	h.logger.Infof("Received forward join message reply from  %s", sender.String())
	h.addPeerToActiveView(sender)
}

func (h *Hyparview) HandleNeighbourMessage(sender peer.Peer, msg message.Message) {
	neighborMsg := msg.(NeighbourMessage)
	h.logger.Infof("Received neighbor message %+v", neighborMsg)

	if neighborMsg.HighPrio {
		if h.addPeerToActiveView(sender) {
			reply := NeighbourMessageReply{
				Accepted: true,
			}
			h.sendMessageTmpTransport(reply, sender)
		}
		return
	}

	if h.activeView.isFull() {
		reply := NeighbourMessageReply{
			Accepted: false,
		}
		h.sendMessageTmpTransport(reply, sender)
		return
	}
	if h.addPeerToActiveView(sender) {
		reply := NeighbourMessageReply{
			Accepted: true,
		}
		h.sendMessageTmpTransport(reply, sender)
	}
}

func (h *Hyparview) HandleNeighbourMaintenanceMessage(sender peer.Peer, msg message.Message) {
	if p, ok := h.activeView.get(sender); ok {
		if p.outConnected {
			delete(h.danglingNeighCounters, sender.String())
			return
		} else {
			h.babel.Dial(h.ID(), sender, sender.ToTCPAddr())
			return
		}
	}
	h.logger.Warn("Got maintenance message from not a neigh")
	_, ok := h.danglingNeighCounters[sender.String()]
	if !ok {
		h.danglingNeighCounters[sender.String()] = 0
	}
	h.danglingNeighCounters[sender.String()]++
	if h.danglingNeighCounters[sender.String()] >= 3 {
		h.babel.SendMessageSideStream(DisconnectMessage{}, sender, sender.ToTCPAddr(), h.ID(), h.ID())
		h.logger.Warn("Disconnecting due to maintenance msg")
	}
}

func (h *Hyparview) HandleNeighbourReplyMessage(sender peer.Peer, msg message.Message) {
	h.logger.Info("Received neighbor reply message")
	neighborReplyMsg := msg.(NeighbourMessageReply)
	if neighborReplyMsg.Accepted {
		h.addPeerToActiveView(sender)
	}
}

func (h *Hyparview) HandleShuffleMessage(sender peer.Peer, msg message.Message) {
	shuffleMsg := msg.(ShuffleMessage)
	if shuffleMsg.TTL > 0 {
		rndSample := h.activeView.getRandomElementsFromView(1, sender)
		if len(rndSample) != 0 {
			toSend := ShuffleMessage{
				ID:    shuffleMsg.ID,
				TTL:   shuffleMsg.TTL - 1,
				Peers: shuffleMsg.Peers,
			}
			h.logger.Debug("Forwarding shuffle message to :", rndSample[0].String())
			h.sendMessage(toSend, rndSample[0])
			return
		}
	}
	//  TTL is 0 or have no nodes to forward to
	//  select random nr of hosts from passive view
	exclusions := append(shuffleMsg.Peers, sender)
	toSend := h.passiveView.getRandomElementsFromView(len(shuffleMsg.Peers), exclusions...)
	h.mergeShuffleMsgPeersWithPassiveView(shuffleMsg.Peers, toSend)
	reply := ShuffleReplyMessage{
		ID:    shuffleMsg.ID,
		Peers: toSend,
	}
	h.sendMessageTmpTransport(reply, sender)
}

func (h *Hyparview) mergeShuffleMsgPeersWithPassiveView(shuffleMsgPeers, peersToKickFirst []peer.Peer) {
	for _, receivedHost := range shuffleMsgPeers {
		if h.babel.SelfPeer().String() == receivedHost.String() {
			continue
		}

		if h.activeView.contains(receivedHost) || h.passiveView.contains(receivedHost) {
			continue
		}

		if h.passiveView.isFull() { // if passive view is not full, skip check and add directly
			removed := false
			for _, firstToKick := range peersToKickFirst {
				if h.passiveView.remove(firstToKick) != nil {
					removed = true
					break
				}
			}
			if !removed {
				h.passiveView.dropRandom() // drop random element to make space
			}
		}
		h.addPeerToPassiveView(receivedHost)
	}
}

func (h *Hyparview) HandleShuffleReplyMessage(sender peer.Peer, m message.Message) {
	shuffleReplyMsg := m.(ShuffleReplyMessage)
	h.logger.Infof("Received shuffle reply message %+v", shuffleReplyMsg)
	peersToDiscardFirst := []peer.Peer{}
	if h.lastShuffleMsg != nil {
		peersToDiscardFirst = append(peersToDiscardFirst, h.lastShuffleMsg.Peers...)
	}
	h.lastShuffleMsg = nil
	h.mergeShuffleMsgPeersWithPassiveView(shuffleReplyMsg.Peers, peersToDiscardFirst)
}

// ---------------- Protocol handlers (timers) ----------------

func (h *Hyparview) HandlePromoteTimer(t timer.Timer) {
	h.logger.Info("Promote timer trigger")
	if time.Since(h.timeStart) > time.Duration(h.conf.JoinTimeSeconds)*time.Second {
		if h.activeView.size() == 0 && h.passiveView.size() == 0 {
			h.joinOverlay()
			return
		}
		if !h.activeView.isFull() && h.passiveView.size() > 0 {
			h.logger.Warn("Promoting node from passive view to active view")
			newNeighbor := h.passiveView.getRandomElementsFromView(1)
			h.sendMessageTmpTransport(NeighbourMessage{
				HighPrio: h.activeView.size() <= 1, // TODO review this
			}, newNeighbor[0])
		}
	}
}

func (h *Hyparview) HandleMaintenanceTimer(t timer.Timer) {
	for _, p := range h.activeView.asArr {
		if !p.outConnected {
			h.babel.Dial(h.ID(), p, p.ToTCPAddr())
		}
		h.sendMessage(NeighbourMaintenanceMessage{}, p)
	}
}

func (h *Hyparview) HandleShuffleTimer(t timer.Timer) {
	h.logger.Info("Shuffle timer trigger")
	minShuffleDuration := time.Duration(h.conf.MinShuffleTimerDurationSeconds) * time.Second

	// add jitter to emission of shuffle messages
	toWait := minShuffleDuration + time.Duration(float32(minShuffleDuration)*rand.Float32())
	h.babel.RegisterTimer(h.ID(), ShuffleTimer{duration: toWait})

	if h.activeView.size() == 0 {
		h.logger.Info("No nodes to send shuffle message message to")
		return
	}

	rndNode := h.activeView.getRandomElementsFromView(1)
	passiveViewRandomPeers := h.passiveView.getRandomElementsFromView(h.conf.Kp-1, rndNode...)
	activeViewRandomPeers := h.activeView.getRandomElementsFromView(h.conf.Ka, rndNode...)
	peers := append(passiveViewRandomPeers, activeViewRandomPeers...)
	peers = append(peers, h.babel.SelfPeer())
	toSend := ShuffleMessage{
		ID:    uint32(getRandInt(math.MaxUint32)),
		TTL:   uint32(h.conf.PRWL),
		Peers: peers,
	}
	h.lastShuffleMsg = &toSend
	h.logger.Info("Sending shuffle message to: ", rndNode[0].String())
	h.sendMessage(toSend, rndNode[0])
}

func (h *Hyparview) HandleDisconnectMessage(sender peer.Peer, m message.Message) {
	h.logger.Warnf("Got Disconnect message from %s", sender.String())
	h.handleNodeDown(sender)
}

// ---------------- Auxiliary functions ----------------

func (h *Hyparview) logHyparviewState() {
	h.logger.Info("------------- Hyparview state -------------")
	var toLog string
	toLog = "Active view : "
	for _, p := range h.activeView.asArr {
		toLog += fmt.Sprintf("%s, ", p.String())
	}
	h.logger.Info(toLog)
	toLog = "Passive view : "
	for _, p := range h.passiveView.asArr {
		toLog += fmt.Sprintf("%s, ", p.String())
	}
	h.logger.Info(toLog)
	h.logger.Info("-------------------------------------------")
}

func (h *Hyparview) sendMessage(msg message.Message, target peer.Peer) {
	h.babel.SendMessage(msg, target, h.ID(), h.ID(), false)
}

func (h *Hyparview) sendMessageTmpTransport(msg message.Message, target peer.Peer) {
	h.babel.SendMessageSideStream(msg, target, target.ToTCPAddr(), h.ID(), h.ID())
}

func (h *Hyparview) HandleDebugTimer(t timer.Timer) {
	h.logInView()
}
