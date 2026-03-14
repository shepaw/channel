// channel-agent — 本地代理客户端
// 运行在本地机器上，主动连接到 channel service，建立反向隧道
// 用法：channel-agent --server https://channel.example.com --channel-id xxx --secret ch_sec_xxx --target http://localhost:3000
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// ── 消息协议（与服务端 tunnel.go 保持一致） ─────────────────────────────────

type MsgType string

const (
	MsgRequest  MsgType = "request"
	MsgResponse MsgType = "response"
	MsgPing     MsgType = "ping"
	MsgPong     MsgType = "pong"
	MsgClose    MsgType = "close"
)

type Message struct {
	Type     MsgType           `json:"type"`
	StreamID int64             `json:"stream_id,omitempty"`
	Method   string            `json:"method,omitempty"`
	Path     string            `json:"path,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Status   int               `json:"status,omitempty"`
	Body     string            `json:"body,omitempty"` // base64
	Error    string            `json:"error,omitempty"`
}

// ── Agent ────────────────────────────────────────────────────────────────────

type Agent struct {
	server    string // https://channel.example.com
	channelID string
	secret    string // channel secret（ch_sec_xxx），永久有效，不依赖 token
	target    string // http://localhost:3000

	conn   *websocket.Conn
	mu     sync.Mutex
	closed bool

	// 流量统计
	totalRequests int64
	totalBytes    int64
}

func NewAgent(server, channelID, secret, target string) *Agent {
	return &Agent{
		server:    server,
		channelID: channelID,
		secret:    secret,
		target:    target,
	}
}

// Run 连接并保持运行，断线后自动重连（指数退避）
func (a *Agent) Run() {
	backoff := 2 * time.Second
	maxBackoff := 60 * time.Second

	for {
		log.Printf("🔌 Connecting to %s (channel: %s)...", a.server, a.channelID)

		err := a.connect()
		if err != nil {
			log.Printf("❌ Connection failed: %v", err)
		} else {
			log.Printf("✅ Connected! Forwarding to %s", a.target)
			backoff = 2 * time.Second // 连接成功后重置退避时间
			a.loop()
			log.Printf("⚠️  Connection closed")
		}

		if a.closed {
			return
		}
		log.Printf("↩️  Reconnecting in %s...", backoff)
		time.Sleep(backoff)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// connect 建立 WebSocket 连接，用 channel secret 认证
func (a *Agent) connect() error {
	// 构建 WebSocket URL
	base := strings.TrimRight(a.server, "/")
	base = strings.Replace(base, "https://", "wss://", 1)
	base = strings.Replace(base, "http://", "ws://", 1)

	u, err := url.Parse(base + "/tunnel/connect")
	if err != nil {
		return fmt.Errorf("invalid server URL: %v", err)
	}
	q := u.Query()
	q.Set("channel_id", a.channelID)
	q.Set("secret", a.secret) // 使用永久 channel secret，不用临时 token
	u.RawQuery = q.Encode()

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		return err
	}

	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()
	return nil
}

// loop 主消息循环
func (a *Agent) loop() {
	defer func() {
		a.mu.Lock()
		if a.conn != nil {
			a.conn.Close()
			a.conn = nil
		}
		a.mu.Unlock()
	}()

	for {
		_, data, err := a.conn.ReadMessage()
		if err != nil {
			return
		}

		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case MsgPing:
			// 回复 pong，维持心跳
			a.send(&Message{Type: MsgPong})

		case MsgRequest:
			// 并发处理每个请求 stream
			go a.handleRequest(&msg)

		case MsgClose:
			log.Printf("⚠️  Server closed the tunnel (secret may have been rotated)")
			return
		}
	}
}

// handleRequest 将服务端下发的请求转发给本地应用，并回传响应
func (a *Agent) handleRequest(req *Message) {
	// 解码请求体
	var bodyReader io.Reader
	if req.Body != "" {
		bodyBytes, err := base64.StdEncoding.DecodeString(req.Body)
		if err == nil && len(bodyBytes) > 0 {
			bodyReader = strings.NewReader(string(bodyBytes))
		}
	}

	targetURL := strings.TrimRight(a.target, "/") + req.Path

	httpReq, err := http.NewRequest(req.Method, targetURL, bodyReader)
	if err != nil {
		a.send(&Message{
			Type:     MsgResponse,
			StreamID: req.StreamID,
			Status:   502,
			Error:    fmt.Sprintf("build request error: %v", err),
		})
		return
	}

	// 转发原始 headers（跳过 host）
	for k, v := range req.Headers {
		if strings.ToLower(k) != "host" {
			httpReq.Header.Set(k, v)
		}
	}

	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		a.send(&Message{
			Type:     MsgResponse,
			StreamID: req.StreamID,
			Status:   502,
			Error:    fmt.Sprintf("local request error: %v", err),
		})
		return
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	bodyB64 := base64.StdEncoding.EncodeToString(bodyBytes)

	// 简化响应 headers
	respHeaders := make(map[string]string)
	for k, vs := range resp.Header {
		if len(vs) > 0 {
			respHeaders[k] = vs[0]
		}
	}

	a.send(&Message{
		Type:     MsgResponse,
		StreamID: req.StreamID,
		Status:   resp.StatusCode,
		Headers:  respHeaders,
		Body:     bodyB64,
	})

	// 更新统计
	atomic.AddInt64(&a.totalRequests, 1)
	atomic.AddInt64(&a.totalBytes, int64(len(bodyBytes)))
}

// send 线程安全地发送消息
func (a *Agent) send(msg *Message) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.conn == nil {
		return fmt.Errorf("not connected")
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return a.conn.WriteMessage(websocket.TextMessage, data)
}

// Stop 停止 agent
func (a *Agent) Stop() {
	a.mu.Lock()
	a.closed = true
	if a.conn != nil {
		a.conn.Close()
	}
	a.mu.Unlock()
}

// PrintStats 打印流量统计
func (a *Agent) PrintStats() {
	reqs := atomic.LoadInt64(&a.totalRequests)
	bytes := atomic.LoadInt64(&a.totalBytes)
	log.Printf("📊 Stats: %d requests, %s transferred", reqs, formatBytes(bytes))
}

func formatBytes(b int64) string {
	switch {
	case b < 1024:
		return fmt.Sprintf("%d B", b)
	case b < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%.2f MB", float64(b)/1024/1024)
	}
}

// ── 入口 ─────────────────────────────────────────────────────────────────────

func main() {
	server := flag.String("server", "", "Channel service 地址，如 https://channel.example.com")
	channelID := flag.String("channel-id", "", "Channel ID")
	secret := flag.String("secret", "", "Channel Secret（ch_sec_xxx），在 dashboard 创建 channel 时获取")
	target := flag.String("target", "http://localhost:8080", "本地应用地址")
	flag.Parse()

	if *server == "" || *channelID == "" || *secret == "" {
		fmt.Fprintln(os.Stderr, "用法: channel-agent --server <url> --channel-id <id> --secret <ch_sec_xxx> [--target <local-url>]")
		fmt.Fprintln(os.Stderr, "示例: channel-agent --server https://channel.example.com --channel-id abc-123 --secret ch_sec_xxxx --target http://localhost:3000")
		fmt.Fprintln(os.Stderr, "\nSecret 在创建 tunnel channel 时由服务端生成，仅展示一次。")
		fmt.Fprintln(os.Stderr, "如已丢失，可在 dashboard 调用 rotate-secret 重新生成。")
		os.Exit(1)
	}

	log.Printf("🚀 channel-agent 启动")
	log.Printf("   Server:     %s", *server)
	log.Printf("   Channel ID: %s", *channelID)
	log.Printf("   Target:     %s", *target)

	agent := NewAgent(*server, *channelID, *secret, *target)

	// 优雅退出
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-quit
		log.Println("⏹  Stopping...")
		agent.PrintStats()
		agent.Stop()
		os.Exit(0)
	}()

	agent.Run()
}
