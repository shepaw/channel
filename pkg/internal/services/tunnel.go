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
	TunnelMsgRequest     TunnelMessageType = "request"
	TunnelMsgResponse    TunnelMessageType = "response"
	TunnelMsgPing        TunnelMessageType = "ping"
	TunnelMsgPong        TunnelMessageType = "pong"
	TunnelMsgData        TunnelMessageType = "data"
	TunnelMsgClose       TunnelMessageType = "close"
	TunnelMsgWsConnect   TunnelMessageType = "ws_connect"   // server→agent: establish WS connection
	TunnelMsgWsData      TunnelMessageType = "ws_data"      // bidirectional: transport WS frame data
	TunnelMsgWsClose     TunnelMessageType = "ws_close"     // bidirectional: close WS connection
	TunnelMsgMailWaiting TunnelMessageType = "mail_waiting" // server→agent: mailbox has pending inbound
	TunnelMsgAccessGrant TunnelMessageType = "access_grant" // server→agent: access grant changed, sync peers
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
	AgentID   string            `json:"agent_id,omitempty"`    // mail_waiting 等控制消息
	// 收件箱控制面元信息（不含 ciphertext）。agent 先按 message_id 去重，再去 inbox 拉正文。
	MessageID string `json:"message_id,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	CallerFP  string `json:"caller_fp,omitempty"`
	Kind      string `json:"kind,omitempty"`
}

// TunnelConn 代表一个已连接的本地 agent（某台设备的一条隧道连接）
type TunnelConn struct {
	channelID  string
	deviceID   string // 连接来源设备；空串表示旧客户端（单一默认设备）
	secret     string // 连接时使用的 secret，用于 rotate 后踢出旧连接
	conn       *websocket.Conn
	mu         sync.Mutex
	streams    map[int64]chan *TunnelMessage // stream_id -> 响应 channel
	streamsMu  sync.RWMutex
	lastPingAt time.Time
	closed     bool
	closeCh    chan struct{}
}

// connSet 一个 channel 下的全部设备连接（多设备共存，请求轮询分发）
type connSet struct {
	mu    sync.RWMutex
	conns map[string]*TunnelConn // deviceID -> conn
	rr    uint64                 // round-robin 计数器
}

// TunnelManager 管理所有活跃的隧道连接
type TunnelManager struct {
	tunnels sync.Map // channelID -> *connSet
}

var globalStreamID int64

func NewTunnelManager() *TunnelManager {
	return &TunnelManager{}
}

// Register 注册一条隧道连接（secret 用于后续 rotate 踢出验证）。
// 同一 channel 下按 deviceID 区分设备：同一设备重连只踢掉自己的旧连接，
// 不同设备共存；deviceID 为空（旧客户端）时等价于单一默认设备，行为与旧版一致。
func (tm *TunnelManager) Register(channelID, deviceID, secret string, conn *websocket.Conn) *TunnelConn {
	tc := &TunnelConn{
		channelID:  channelID,
		deviceID:   deviceID,
		secret:     secret,
		conn:       conn,
		streams:    make(map[int64]chan *TunnelMessage),
		lastPingAt: time.Now(),
		closeCh:    make(chan struct{}),
	}

	set := tm.getOrCreateSet(channelID)
	set.mu.Lock()
	if old, ok := set.conns[deviceID]; ok {
		old.close() // 同设备重连：只踢掉自己的旧连接
	}
	set.conns[deviceID] = tc
	set.mu.Unlock()

	go tc.readLoop(func() { tm.removeConn(channelID, deviceID, tc) })
	go tc.pingLoop()

	return tc
}

func (tm *TunnelManager) getOrCreateSet(channelID string) *connSet {
	if v, ok := tm.tunnels.Load(channelID); ok {
		return v.(*connSet)
	}
	set := &connSet{conns: make(map[string]*TunnelConn)}
	actual, _ := tm.tunnels.LoadOrStore(channelID, set)
	return actual.(*connSet)
}

// removeConn 在连接退出时从集合中移除自己（仅当集合里仍是这条连接，
// 避免误删同设备重连后的新连接）。空集合不删除——保留在 map 中无害
// （Get 对空集合返回 false），可避免与并发 Register 的删除竞态。
func (tm *TunnelManager) removeConn(channelID, deviceID string, tc *TunnelConn) {
	v, ok := tm.tunnels.Load(channelID)
	if !ok {
		return
	}
	set := v.(*connSet)
	set.mu.Lock()
	if cur, ok := set.conns[deviceID]; ok && cur == tc {
		delete(set.conns, deviceID)
	}
	set.mu.Unlock()
}

// Unregister 注销 channel 的全部隧道连接
func (tm *TunnelManager) Unregister(channelID string) {
	if v, ok := tm.tunnels.LoadAndDelete(channelID); ok {
		set := v.(*connSet)
		set.mu.Lock()
		for _, tc := range set.conns {
			tc.close()
		}
		set.conns = make(map[string]*TunnelConn)
		set.mu.Unlock()
	}
}

// KickIfSecret 踢掉该 channel 下所有使用指定 secret 的连接（rotate secret 后立即生效）
func (tm *TunnelManager) KickIfSecret(channelID, oldSecret string) {
	v, ok := tm.tunnels.Load(channelID)
	if !ok {
		return
	}
	set := v.(*connSet)
	set.mu.Lock()
	for deviceID, tc := range set.conns {
		if tc.secret == oldSecret {
			delete(set.conns, deviceID)
			tc.close()
		}
	}
	set.mu.Unlock()
}

// Get 轮询获取指定 channel 的一条在线隧道连接（多设备负载分摊；
// 单个请求/WS 会话粘性在选中的连接上，stream 状态随连接隔离）
func (tm *TunnelManager) Get(channelID string) (*TunnelConn, bool) {
	v, ok := tm.tunnels.Load(channelID)
	if !ok {
		return nil, false
	}
	set := v.(*connSet)
	set.mu.RLock()
	defer set.mu.RUnlock()

	n := len(set.conns)
	if n == 0 {
		return nil, false
	}
	// round-robin：从 rr 位置开始找第一条未关闭的连接（rr 用原子操作，读锁下安全）
	devices := make([]string, 0, n)
	for d := range set.conns {
		devices = append(devices, d)
	}
	start := int(atomic.AddUint64(&set.rr, 1) % uint64(len(devices)))
	for i := 0; i < len(devices); i++ {
		tc := set.conns[devices[(start+i)%len(devices)]]
		if tc != nil && !tc.closed {
			return tc, true
		}
	}
	return nil, false
}

// IsOnline 判断 channel 是否在线（任一设备连接存活即在线）
func (tm *TunnelManager) IsOnline(channelID string) bool {
	_, ok := tm.Get(channelID)
	return ok
}

// OnlineDevices 返回 channel 当前在线的设备 ID 列表
func (tm *TunnelManager) OnlineDevices(channelID string) []string {
	v, ok := tm.tunnels.Load(channelID)
	if !ok {
		return nil
	}
	set := v.(*connSet)
	set.mu.RLock()
	defer set.mu.RUnlock()
	devices := make([]string, 0, len(set.conns))
	for d, tc := range set.conns {
		if !tc.closed {
			devices = append(devices, d)
		}
	}
	return devices
}

// BroadcastControl 向 channel 下所有在线设备广播一条控制消息（如 mail_waiting）。
// 失败静默忽略——控制消息是优化，不是可靠性路径（agent 仍会定时拉信箱）。
func (tm *TunnelManager) BroadcastControl(channelID string, msg *TunnelMessage) {
	v, ok := tm.tunnels.Load(channelID)
	if !ok {
		return
	}
	set := v.(*connSet)
	set.mu.RLock()
	conns := make([]*TunnelConn, 0, len(set.conns))
	for _, tc := range set.conns {
		if tc != nil && !tc.closed {
			conns = append(conns, tc)
		}
	}
	set.mu.RUnlock()
	for _, tc := range conns {
		_ = tc.send(msg)
	}
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
	// sendClose 向 client 端发送一个标准 WebSocket Close 帧。
	// 必须显式下发，否则对端只能感知到 TCP 断开（1006 abnormal closure），
	// 前端把这种情况当作"流仍在进行"，导致确认/审批组件的按钮无法点击。
	sendClose := func(code int, reason string) {
		_ = clientConn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(code, reason),
			time.Now().Add(2*time.Second),
		)
	}
	for {
		select {
		case msg, ok := <-dataCh:
			if !ok {
				sendClose(websocket.CloseNormalClosure, "")
				return nil
			}
			if msg.Type == TunnelMsgWsClose {
				sendClose(websocket.CloseNormalClosure, "")
				return nil
			}
			data, _ := base64.StdEncoding.DecodeString(msg.Body)
			if err := clientConn.WriteMessage(msg.WsMsgType, data); err != nil {
				cancel()
				sendClose(websocket.CloseInternalServerErr, "write failed")
				return err
			}
		case <-ctx.Done():
			sendClose(websocket.CloseNormalClosure, "")
			return nil
		case <-tc.closeCh:
			sendClose(websocket.CloseGoingAway, "tunnel closed")
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

// readLoop 读取 agent 上行消息并分发到各 stream；退出时调用 onExit
// （让 TunnelManager 把自己从连接集合中摘除，保证 IsOnline 反映真实状态）
func (tc *TunnelConn) readLoop(onExit func()) {
	defer func() {
		tc.close()
		if onExit != nil {
			onExit()
		}
	}()

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
			if !ok {
				continue
			}
			// Close 帧必须送达，否则 client 端的流式消息（例如确认/审批卡片）
			// 会停在"加载中"状态，按钮无法点击。这里采用阻塞发送 + closeCh 逃生，
			// 避免像 Data 帧那样被 default 分支静默丢弃。
			if msg.Type == TunnelMsgWsClose {
				select {
				case ch <- &msg:
				case <-tc.closeCh:
					return
				}
			} else {
				select {
				case ch <- &msg:
				default:
					// Data 帧在消费者严重落后时仍可能被丢弃——这是已知的独立问题，
					// 本次修复只保证 Close 帧的送达。
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
