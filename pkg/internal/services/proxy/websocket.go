package proxy

import (
	"fmt"
	"log"
	"net/http"
	"sync"

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
		clientConn.WriteMessage(websocket.CloseMessage, []byte(fmt.Sprintf("Cannot reach target: %v", err)))
		return
	}
	defer targetConn.Close()

	var totalBytes int64
	var wg sync.WaitGroup
	wg.Add(2)

	relay := func(src, dst *websocket.Conn, label string) {
		defer wg.Done()
		for {
			mt, msg, err := src.ReadMessage()
			if err != nil {
				return
			}
			totalBytes += int64(len(msg))

			// Bandwidth check (best-effort; drop if exceeded)
			if ok, _, _ := rateLimitSvc.CheckBandwidth(channelID, clientIP, int64(len(msg))); !ok {
				src.WriteMessage(websocket.CloseMessage, []byte("bandwidth limit exceeded"))
				return
			}

			if err := dst.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	}

	go relay(clientConn, targetConn, "client->target")
	go relay(targetConn, clientConn, "target->client")
	wg.Wait()

	go channelSvc.UpdateChannelStats(channelID, totalBytes, 0, 0)
}