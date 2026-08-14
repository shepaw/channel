package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"regexp"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// InboxSubscribeHandler caller-side inbox push WebSocket.
//
// GET /api/v1/inbox/subscribe?caller_fp=<16-hex>
// Client → server: {"type":"subscribe","targets":["acp_agent_…"]}
//                {"type":"unsubscribe","targets":["…"]}
// Server → client: {"type":"mail_reply","reply_id":…,"target_id":…,…}
type InboxSubscribeHandler struct {
	subs  *services.InboxSubscriberManager
	redis *services.RedisService
}

func NewInboxSubscribeHandler(subs *services.InboxSubscriberManager, redis *services.RedisService) *InboxSubscribeHandler {
	return &InboxSubscribeHandler{subs: subs, redis: redis}
}

var inboxSubscribeUpgrader = websocket.Upgrader{
	HandshakeTimeout: 10 * time.Second,
	CheckOrigin: func(r *http.Request) bool { return true },
}

type inboxClientMessage struct {
	Type    string   `json:"type"`
	Targets []string `json:"targets"`
}

var inboxCallerFPRegex = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Connect upgrades to WebSocket and registers the caller for push notifications.
func (h *InboxSubscribeHandler) Connect(c *gin.Context) {
	if !h.allowSubscribeIP(c) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
		return
	}

	callerFP := c.Query("caller_fp")
	if callerFP == "" || !inboxCallerFPRegex.MatchString(callerFP) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "caller_fp is required"})
		return
	}

	conn, err := inboxSubscribeUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("Inbox subscribe WS upgrade failed: %v", err)
		return
	}

	sub := services.NewInboxSubscriberConn(callerFP, conn)
	h.subs.Register(callerFP, sub)
	defer func() {
		h.subs.Unregister(callerFP, sub)
		sub.Close()
	}()

	_ = sub.SendJSON(gin.H{"type": "subscribed", "caller_fp": callerFP})

	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(90 * time.Second))
			return nil
		})
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			conn.SetReadDeadline(time.Now().Add(90 * time.Second))
			var msg inboxClientMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			switch msg.Type {
			case "subscribe":
				sub.AddTargets(msg.Targets)
			case "unsubscribe":
				sub.RemoveTargets(msg.Targets)
			case "pong":
				// keep-alive ack
			}
		}
	}()

	for {
		select {
		case <-done:
			return
		case <-pingTicker.C:
			_ = sub.SendJSON(gin.H{"type": "ping"})
		}
	}
}

func (h *InboxSubscribeHandler) allowSubscribeIP(c *gin.Context) bool {
	if h.redis == nil {
		return true
	}
	ip := c.ClientIP()
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	window := time.Now().Truncate(time.Minute).Unix()
	key := fmt.Sprintf("rl:mailbox:%s:%d", ip, window)
	n, err := h.redis.IncrBy(key, 1)
	if err != nil {
		return true
	}
	if n == 1 {
		_ = h.redis.Expire(key, 2*time.Minute)
	}
	return n <= mailboxRateLimit
}
