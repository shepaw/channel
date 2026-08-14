package services

import (
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

// InboxReplyNotification server→caller push when a sealed reply is deposited.
type InboxReplyNotification struct {
	Type      string `json:"type"` // mail_reply
	ReplyID   string `json:"reply_id"`
	TargetID  string `json:"target_id"`
	RequestID string `json:"request_id,omitempty"`
	ReplyTo   string `json:"reply_to,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	Kind      string `json:"kind,omitempty"`
}

// InboxSubscriberManager tracks caller WebSocket subscriptions for inbox push.
type InboxSubscriberManager struct {
	mu   sync.RWMutex
	subs map[string]map[*InboxSubscriberConn]struct{} // caller_fp → conns
}

func NewInboxSubscriberManager() *InboxSubscriberManager {
	return &InboxSubscriberManager{
		subs: make(map[string]map[*InboxSubscriberConn]struct{}),
	}
}

// NewInboxSubscriberConn wraps a WebSocket for inbox push delivery.
func NewInboxSubscriberConn(callerFP string, conn *websocket.Conn) *InboxSubscriberConn {
	return &InboxSubscriberConn{
		CallerFP: callerFP,
		conn:     conn,
	}
}

// InboxSubscriberConn one caller inbox subscribe WebSocket.
type InboxSubscriberConn struct {
	CallerFP string
	conn     *websocket.Conn
	sendMu   sync.Mutex
	closed   atomic.Bool
	targets  map[string]struct{} // empty = receive all targets for this caller
	targetMu sync.RWMutex
}

func (m *InboxSubscriberManager) Register(callerFP string, c *InboxSubscriberConn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.subs[callerFP] == nil {
		m.subs[callerFP] = make(map[*InboxSubscriberConn]struct{})
	}
	m.subs[callerFP][c] = struct{}{}
}

func (m *InboxSubscriberManager) Unregister(callerFP string, c *InboxSubscriberConn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	set := m.subs[callerFP]
	if set == nil {
		return
	}
	delete(set, c)
	if len(set) == 0 {
		delete(m.subs, callerFP)
	}
}

// NotifyReply pushes mail_reply to subscribed callers (filtered by target).
func (m *InboxSubscriberManager) NotifyReply(callerFP string, n InboxReplyNotification) {
	m.mu.RLock()
	set := m.subs[callerFP]
	if len(set) == 0 {
		m.mu.RUnlock()
		return
	}
	conns := make([]*InboxSubscriberConn, 0, len(set))
	for c := range set {
		conns = append(conns, c)
	}
	m.mu.RUnlock()

	for _, c := range conns {
		if !c.wantsTarget(n.TargetID) {
			continue
		}
		_ = c.sendJSON(n)
	}
}

func (c *InboxSubscriberConn) wantsTarget(targetID string) bool {
	c.targetMu.RLock()
	defer c.targetMu.RUnlock()
	if len(c.targets) == 0 {
		return true
	}
	_, ok := c.targets[targetID]
	return ok
}

func (c *InboxSubscriberConn) setTargets(targets []string) {
	c.targetMu.Lock()
	defer c.targetMu.Unlock()
	c.targets = make(map[string]struct{}, len(targets))
	for _, t := range targets {
		if t != "" {
			c.targets[t] = struct{}{}
		}
	}
}

func (c *InboxSubscriberConn) addTargets(targets []string) {
	c.targetMu.Lock()
	defer c.targetMu.Unlock()
	if c.targets == nil {
		c.targets = make(map[string]struct{})
	}
	for _, t := range targets {
		if t != "" {
			c.targets[t] = struct{}{}
		}
	}
}

func (c *InboxSubscriberConn) removeTargets(targets []string) {
	c.targetMu.Lock()
	defer c.targetMu.Unlock()
	for _, t := range targets {
		delete(c.targets, t)
	}
}

func (c *InboxSubscriberConn) SendJSON(v any) error {
	return c.sendJSON(v)
}

func (c *InboxSubscriberConn) AddTargets(targets []string) {
	c.addTargets(targets)
}

func (c *InboxSubscriberConn) RemoveTargets(targets []string) {
	c.removeTargets(targets)
}

func (c *InboxSubscriberConn) sendJSON(v any) error {
	if c.closed.Load() {
		return websocket.ErrCloseSent
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.closed.Load() {
		return websocket.ErrCloseSent
	}
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

func (c *InboxSubscriberConn) Close() {
	if c.closed.Swap(true) {
		return
	}
	_ = c.conn.Close()
}
