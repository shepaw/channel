package proxy

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/services"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

type WebSocketProxy struct {
	targetURL string
}

func NewWebSocketProxy(target string) (*WebSocketProxy, error) {
	return &WebSocketProxy{targetURL: target}, nil
}

func (w *WebSocketProxy) ServeHTTP(rw http.ResponseWriter, r *http.Request, channelID, clientIP string, rateLimitSvc *services.RateLimitService, channelSvc *services.ChannelService) {
	allowed, _, err := rateLimitSvc.CheckConnections(channelID)
	if err != nil || !allowed {
		http.Error(rw, "Too many connections", http.StatusTooManyRequests)
		return
	}
	defer rateLimitSvc.DecrementConnections(channelID)

	clientConn, err := upgrader.Upgrade(rw, r, nil)
	if err != nil {
		log.Printf("WS upgrade error: %v", err)
		return
	}
	defer clientConn.Close()

	// Build target address
	targetAddr := w.targetURL
	if r.URL.Path != "" {
		targetAddr += r.URL.Path
	}
	if r.URL.RawQuery != "" {
		targetAddr += "?" + r.URL.RawQuery
	}

	targetConn, _, err := websocket.DefaultDialer.Dial(targetAddr, nil)
	if err != nil {
		log.Printf("WS dial target error: %v", err)
		_ = clientConn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(
				websocket.CloseInternalServerErr,
				fmt.Sprintf("Cannot reach target: %v", err),
			),
			time.Now().Add(2*time.Second),
		)
		return
	}
	defer targetConn.Close()

	var totalBytes int64
	var wg sync.WaitGroup
	wg.Add(2)

	// sendClose 写一个标准 WebSocket Close 帧。
	// relay 一端结束时必须把 Close 信号显式送到对端，
	// 否则 app 端只能感知到 TCP 断开（1006 abnormal closure），
	// 带确认按钮的流式消息会停在"加载中"，按钮无法点击。
	sendClose := func(c *websocket.Conn, code int, reason string) {
		_ = c.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(code, reason),
			time.Now().Add(2*time.Second),
		)
	}

	relay := func(src, dst *websocket.Conn, label string) {
		defer wg.Done()
		for {
			mt, msg, err := src.ReadMessage()
			if err != nil {
				// src 已断开：告诉 dst 正常关闭，让 dst 侧的 client 能完成流。
				if ce, ok := err.(*websocket.CloseError); ok {
					sendClose(dst, ce.Code, ce.Text)
				} else {
					sendClose(dst, websocket.CloseNormalClosure, "")
				}
				return
			}
			totalBytes += int64(len(msg))

			// Bandwidth check (best-effort; drop if exceeded)
			if ok, _, _ := rateLimitSvc.CheckBandwidth(channelID, clientIP, int64(len(msg))); !ok {
				sendClose(src, websocket.ClosePolicyViolation, "bandwidth limit exceeded")
				sendClose(dst, websocket.ClosePolicyViolation, "bandwidth limit exceeded")
				return
			}

			if err := dst.WriteMessage(mt, msg); err != nil {
				// 写失败也要把信号回送给 src，避免对端长连接一直挂着。
				sendClose(src, websocket.CloseInternalServerErr, "peer write failed")
				return
			}
		}
	}

	go relay(clientConn, targetConn, "client->target")
	go relay(targetConn, clientConn, "target->client")
	wg.Wait()

	go channelSvc.UpdateChannelStats(channelID, totalBytes, 0, 0)
}