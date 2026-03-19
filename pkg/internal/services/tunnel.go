package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// TunnelMessageType 隧道消息类型
type TunnelMessageType string

const (
	TunnelMsgRequest   TunnelMessageType = "request"
	TunnelMsgResponse  TunnelMessageType = "response"
	TunnelMsgPing      TunnelMessageType = "ping"
	TunnelMsgPong      TunnelMessageType = "pong"
	TunnelMsgData      TunnelMessageType = "data"
	TunnelMsgClose     TunnelMessageType = "close"
	TunnelMsgWsConnect TunnelMessageType = "ws_connect" // server→agent: establish WS connection
	TunnelMsgWsData    TunnelMessageType = "ws_data"    // bidirectional: transport WS frame data
	TunnelMsgWsClose   TunnelMessageType = "ws_close"   // bidirectional: close WS connection
)

// TunnelMessage 隧道消息结构
type TunnelMessage struct {
	Type      TunnelMessageType `json:"type"`
	StreamID  int64             `json:"stream_id,omitempty"`
	Method    string            `json:"method,omitempty"`
	Path      string            `json:"path,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Status    int               `json:"status,omitempty"`
	Body      string            `json:"body,omitempty"` // base64 编码
	Error     string            `json:"error,omitempty"`
	WsMsgType int               `json:"ws_msg_type,omitempty"` // websocket.TextMessage=1 / BinaryMessage=2
}

// TunnelConn 代表一个已连接的本地 agent
type TunnelConn struct {
	channelID  string
	secret     string // 连接时使用的 secret，用于 rotate 后踢出旧连接
	conn       *websocket.Conn
	mu         sync.Mutex
	streams    map[int64]chan *TunnelMessage // stream_id -> 响应 channel
	streamsMu  sync.RWMutex
	lastPingAt time.Time
	closed     bool
	closeCh    chan struct{}
}

// TunnelManager 管理所有活跃的隧道连接
type TunnelManager struct {
	tunnels sync.Map // channelID -> *TunnelConn
}

var globalStreamID int64

func NewTunnelManager() *TunnelManager {
	return &TunnelManager{}
}

// Register 注册一条隧道连接（secret 用于后续 rotate 踢出验证）
func (tm *TunnelManager) Register(channelID, secret string, conn *websocket.Conn) *TunnelConn {
	tc := &TunnelConn{
		channelID:  channelID,
		secret:     secret,
		conn:       conn,
		streams:    make(map[int64]chan *TunnelMessage),
		lastPingAt: time.Now(),
		closeCh:    make(chan struct{}),
	}

	// 如果有旧连接，先踢掉
	if old, ok := tm.tunnels.LoadAndDelete(channelID); ok {
		old.(*TunnelConn).close()
	}

	tm.tunnels.Store(channelID, tc)

	go tc.readLoop()
	go tc.pingLoop()

	return tc
}

// Unregister 注销隧道连接
func (tm *TunnelManager) Unregister(channelID string) {
	if v, ok := tm.tunnels.LoadAndDelete(channelID); ok {
		v.(*TunnelConn).close()
	}
}

// KickIfSecret 如果当前连接使用的是指定 secret，则踢掉它（用于 rotate secret 后立即生效）
func (tm *TunnelManager) KickIfSecret(channelID, oldSecret string) {
	if v, ok := tm.tunnels.Load(channelID); ok {
		tc := v.(*TunnelConn)
		if tc.secret == oldSecret {
			tm.tunnels.Delete(channelID)
			tc.close()
		}
	}
}

// Get 获取指定 channel 的隧道连接
func (tm *TunnelManager) Get(channelID string) (*TunnelConn, bool) {
	v, ok := tm.tunnels.Load(channelID)
	if !ok {
		return nil, false
	}
	return v.(*TunnelConn), true
}

// IsOnline 判断 channel 是否在线
func (tm *TunnelManager) IsOnline(channelID string) bool {
	_, ok := tm.tunnels.Load(channelID)
	return ok
}

// ForwardHTTP 通过隧道转发 HTTP 请求，返回响应（超时 30s）
func (tm *TunnelManager) ForwardHTTP(channelID string, r *http.Request) (*TunnelMessage, error) {
	tc, ok := tm.Get(channelID)
	if !ok {
		return nil, fmt.Errorf("channel %s 未连接隧道", channelID)
	}

	// 读取请求体
	var bodyB64 string
	if r.Body != nil {
		bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 32*1024*1024))
		if err == nil && len(bodyBytes) > 0 {
			bodyB64 = base64.StdEncoding.EncodeToString(bodyBytes)
		}
	}

	// 构建请求路径（含 query）
	path := r.URL.Path
	if r.URL.RawQuery != "" {
		path = path + "?" + r.URL.RawQuery
	}

	// 简化 headers
	headers := make(map[string]string)
	for k, vs := range r.Header {
		if len(vs) > 0 {
			headers[k] = vs[0]
		}
	}

	streamID := atomic.AddInt64(&globalStreamID, 1)
	respCh := make(chan *TunnelMessage, 1)
	tc.registerStream(streamID, respCh)
	defer tc.unregisterStream(streamID)

	msg := &TunnelMessage{
		Type:     TunnelMsgRequest,
		StreamID: streamID,
		Method:   r.Method,
		Path:     path,
		Headers:  headers,
		Body:     bodyB64,
	}

	if err := tc.send(msg); err != nil {
		return nil, fmt.Errorf("发送请求失败: %v", err)
	}

	// 等待响应，最多 30 秒
	select {
	case resp := <-respCh:
		return resp, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("隧道请求超时（30s）")
	case <-tc.closeCh:
		return nil, fmt.Errorf("隧道连接已断开")
	}
}

// NextStreamID returns a unique stream ID for new proxy streams.
func (tm *TunnelManager) NextStreamID() int64 {
	return atomic.AddInt64(&globalStreamID, 1)
}

// ForwardWS proxies a WebSocket connection through the tunnel to the local agent.
// clientConn is the already-upgraded WebSocket connection from the external caller.
func (tm *TunnelManager) ForwardWS(channelID string, streamID int64, originalPath string, headers map[string]string, clientConn *websocket.Conn) error {
	tc, ok := tm.Get(channelID)
	if !ok {
		return fmt.Errorf("channel %s 未连接隧道", channelID)
	}

	// Register a bidirectional data channel (buffered to avoid blocking agent)
	dataCh := make(chan *TunnelMessage, 64)
	tc.registerStream(streamID, dataCh)
	defer tc.unregisterStream(streamID)

	// Notify the agent to establish a WS connection to the local ACP Server
	if err := tc.send(&TunnelMessage{
		Type:     TunnelMsgWsConnect,
		StreamID: streamID,
		Path:     originalPath, // full path+query; agent strips /proxy/{channel_id} prefix
		Headers:  headers,      // includes Authorization: Bearer <token>
	}); err != nil {
		return fmt.Errorf("发送 ws_connect 失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// client → agent direction
	go func() {
		defer cancel()
		for {
			msgType, data, err := clientConn.ReadMessage()
			if err != nil {
				tc.send(&TunnelMessage{Type: TunnelMsgWsClose, StreamID: streamID}) //nolint:errcheck
				return
			}
			tc.send(&TunnelMessage{ //nolint:errcheck
				Type:      TunnelMsgWsData,
				StreamID:  streamID,
				Body:      base64.StdEncoding.EncodeToString(data),
				WsMsgType: msgType,
			})
		}
	}()

	// agent → client direction
	defer clientConn.Close()
	for {
		select {
		case msg, ok := <-dataCh:
			if !ok {
				return nil
			}
			if msg.Type == TunnelMsgWsClose {
				return nil
			}
			data, _ := base64.StdEncoding.DecodeString(msg.Body)
			if err := clientConn.WriteMessage(msg.WsMsgType, data); err != nil {
				cancel()
				return err
			}
		case <-ctx.Done():
			return nil
		case <-tc.closeCh:
			return nil
		}
	}
}

// ── TunnelConn 内部方法 ────────────────────────────────────────────────────────

func (tc *TunnelConn) send(msg *TunnelMessage) error {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return tc.conn.WriteMessage(websocket.TextMessage, data)
}

func (tc *TunnelConn) registerStream(streamID int64, ch chan *TunnelMessage) {
	tc.streamsMu.Lock()
	tc.streams[streamID] = ch
	tc.streamsMu.Unlock()
}

func (tc *TunnelConn) unregisterStream(streamID int64) {
	tc.streamsMu.Lock()
	delete(tc.streams, streamID)
	tc.streamsMu.Unlock()
}

func (tc *TunnelConn) readLoop() {
	defer tc.close()

	for {
		_, data, err := tc.conn.ReadMessage()
		if err != nil {
			return
		}

		var msg TunnelMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case TunnelMsgPong:
			tc.lastPingAt = time.Now()

		case TunnelMsgResponse:
			tc.streamsMu.RLock()
			ch, ok := tc.streams[msg.StreamID]
			tc.streamsMu.RUnlock()
			if ok {
				select {
				case ch <- &msg:
				default:
				}
			}

		case TunnelMsgWsData, TunnelMsgWsClose:
			tc.streamsMu.RLock()
			ch, ok := tc.streams[msg.StreamID]
			tc.streamsMu.RUnlock()
			if ok {
				select {
				case ch <- &msg:
				default:
				}
			}
		}
	}
}

func (tc *TunnelConn) pingLoop() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// 超过 60s 没收到 pong，断开
			if time.Since(tc.lastPingAt) > 60*time.Second {
				tc.close()
				return
			}
			tc.send(&TunnelMessage{Type: TunnelMsgPing})
		case <-tc.closeCh:
			return
		}
	}
}

func (tc *TunnelConn) close() {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	if !tc.closed {
		tc.closed = true
		close(tc.closeCh)
		tc.conn.Close()
	}
}
