package handlers

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/services"
	"github.com/gin-gonic/gin"
)

// MailboxHandler channel 双向信箱 API。
//
// Caller（免登录，IP 限流）：
//   POST   /api/v1/mailbox/:agent_id/messages
//   GET    /api/v1/mailbox/:agent_id/replies?caller_fp=
//   POST   /api/v1/mailbox/:agent_id/replies/ack
//
// Agent（channel secret HMAC）：
//   GET    /api/v1/mailbox/:agent_id/pending
//   POST   /api/v1/mailbox/:agent_id/ack
//   POST   /api/v1/mailbox/:agent_id/replies
type MailboxHandler struct {
	mailboxSvc *services.MailboxService
	agentSvc   *services.AgentService
	channelSvc *services.ChannelService
	tunnelMgr  *services.TunnelManager
	redis      *services.RedisService
	nonces     *services.NonceCache
}

func NewMailboxHandler(
	mailboxSvc *services.MailboxService,
	agentSvc *services.AgentService,
	channelSvc *services.ChannelService,
	tunnelMgr *services.TunnelManager,
	redis *services.RedisService,
) *MailboxHandler {
	return &MailboxHandler{
		mailboxSvc: mailboxSvc,
		agentSvc:   agentSvc,
		channelSvc: channelSvc,
		tunnelMgr:  tunnelMgr,
		redis:      redis,
		nonces:     services.NewNonceCache(),
	}
}

const mailboxRateLimit = 30 // 每 IP 每分钟

// ── Caller APIs ──────────────────────────────────────────────────────────────

type depositMessageRequest struct {
	CallerFP   string `json:"caller_fp"  binding:"required"`
	MessageID  string `json:"message_id"`
	SessionID  string `json:"session_id"`
	Ciphertext string `json:"ciphertext" binding:"required"`
}

// DepositMessage caller 留言（密文）
// POST /api/v1/mailbox/:agent_id/messages
func (h *MailboxHandler) DepositMessage(c *gin.Context) {
	if !h.allowMailboxIP(c) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
		return
	}
	agentID := c.Param("agent_id")
	var req depositMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	msg, err := h.mailboxSvc.DepositInbound(services.DepositInboundParams{
		AgentID:    agentID,
		CallerFP:   req.CallerFP,
		MessageID:  req.MessageID,
		SessionID:  req.SessionID,
		Ciphertext: req.Ciphertext,
	})
	if err != nil {
		h.writeMailboxErr(c, err)
		return
	}

	// 通知在线 agent 有新信（best-effort）
	h.notifyMailWaiting(agentID)

	pending, _ := h.mailboxSvc.PendingCount(agentID)
	c.JSON(http.StatusCreated, gin.H{
		"id":         msg.ID,
		"message_id": msg.MessageID,
		"pending":    pending,
		"expires_at": msg.ExpiresAt.Format(time.RFC3339),
	})
}

// ListReplies caller 拉取回复
// GET /api/v1/mailbox/:agent_id/replies?caller_fp=&after=
func (h *MailboxHandler) ListReplies(c *gin.Context) {
	if !h.allowMailboxIP(c) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
		return
	}
	callerFP := c.Query("caller_fp")
	if callerFP == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "caller_fp is required"})
		return
	}
	var after time.Time
	if s := c.Query("after"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			after = t
		}
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))

	rows, err := h.mailboxSvc.ListReplies(c.Param("agent_id"), callerFP, after, limit)
	if err != nil {
		h.writeMailboxErr(c, err)
		return
	}

	out := make([]gin.H, 0, len(rows))
	for _, m := range rows {
		out = append(out, gin.H{
			"id":         m.ID,
			"message_id": m.MessageID,
			"reply_to":   m.ReplyTo,
			"session_id": m.SessionID,
			"ciphertext": m.Ciphertext,
			"created_at": m.CreatedAt.Format(time.RFC3339),
		})
	}
	c.JSON(http.StatusOK, gin.H{"replies": out, "total": len(out)})
}

type ackRepliesRequest struct {
	CallerFP string   `json:"caller_fp" binding:"required"`
	IDs      []string `json:"ids"       binding:"required,min=1"`
}

// AckReplies caller 确认已落本地
// POST /api/v1/mailbox/:agent_id/replies/ack
func (h *MailboxHandler) AckReplies(c *gin.Context) {
	if !h.allowMailboxIP(c) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
		return
	}
	var req ackRepliesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	n, err := h.mailboxSvc.AckReplies(c.Param("agent_id"), req.CallerFP, req.IDs)
	if err != nil {
		h.writeMailboxErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"acked": n})
}

// ── Agent APIs（HMAC） ───────────────────────────────────────────────────────

type agentAuthFields struct {
	Timestamp string `json:"timestamp" binding:"required"`
	Nonce     string `json:"nonce"     binding:"required"`
	Signature string `json:"signature" binding:"required"`
}

// ClaimPending agent 拉取待处理留言
// GET /api/v1/mailbox/:agent_id/pending?limit=&timestamp=&nonce=&signature=
func (h *MailboxHandler) ClaimPending(c *gin.Context) {
	agentID := c.Param("agent_id")
	if !h.verifyAgentHMAC(c, agentID, c.Query("timestamp"), c.Query("nonce"), c.Query("signature")) {
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "5"))
	rows, err := h.mailboxSvc.ClaimPending(agentID, limit)
	if err != nil {
		h.writeMailboxErr(c, err)
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, m := range rows {
		out = append(out, gin.H{
			"id":         m.ID,
			"message_id": m.MessageID,
			"session_id": m.SessionID,
			"caller_fp":  m.CallerFP,
			"ciphertext": m.Ciphertext,
			"created_at": m.CreatedAt.Format(time.RFC3339),
		})
	}
	c.JSON(http.StatusOK, gin.H{"messages": out, "total": len(out)})
}

type ackInboundRequest struct {
	agentAuthFields
	IDs []string `json:"ids" binding:"required,min=1"`
}

// AckInbound agent 确认处理完留言
// POST /api/v1/mailbox/:agent_id/ack
func (h *MailboxHandler) AckInbound(c *gin.Context) {
	agentID := c.Param("agent_id")
	var req ackInboundRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !h.verifyAgentHMAC(c, agentID, req.Timestamp, req.Nonce, req.Signature) {
		return
	}
	n, err := h.mailboxSvc.AckInbound(agentID, req.IDs)
	if err != nil {
		h.writeMailboxErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"acked": n})
}

type depositReplyRequest struct {
	agentAuthFields
	CallerFP   string `json:"caller_fp"  binding:"required"`
	MessageID  string `json:"message_id"`
	ReplyTo    string `json:"reply_to"   binding:"required"`
	SessionID  string `json:"session_id"`
	Ciphertext string `json:"ciphertext" binding:"required"`
}

// DepositReply agent 回投回复密文
// POST /api/v1/mailbox/:agent_id/replies
func (h *MailboxHandler) DepositReply(c *gin.Context) {
	agentID := c.Param("agent_id")
	var req depositReplyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !h.verifyAgentHMAC(c, agentID, req.Timestamp, req.Nonce, req.Signature) {
		return
	}
	msg, err := h.mailboxSvc.DepositReply(services.DepositReplyParams{
		AgentID:    agentID,
		CallerFP:   req.CallerFP,
		MessageID:  req.MessageID,
		ReplyTo:    req.ReplyTo,
		SessionID:  req.SessionID,
		Ciphertext: req.Ciphertext,
	})
	if err != nil {
		h.writeMailboxErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"id":         msg.ID,
		"message_id": msg.MessageID,
		"reply_to":   msg.ReplyTo,
	})
}

// ── helpers ──────────────────────────────────────────────────────────────────

func (h *MailboxHandler) verifyAgentHMAC(c *gin.Context, agentID, timestamp, nonce, signature string) bool {
	if timestamp == "" || nonce == "" || signature == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "timestamp, nonce, signature required"})
		return false
	}
	if err := services.ValidateTimestamp(timestamp); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "timestamp validation failed: " + err.Error()})
		return false
	}
	if !h.nonces.CheckAndStore(nonce) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "nonce already used"})
		return false
	}
	agent, err := h.agentSvc.GetByID(agentID)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "agent not found"})
		return false
	}
	channel, err := h.channelSvc.GetChannelByID(agent.ChannelID)
	if err != nil || channel.Secret == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "channel secret unavailable"})
		return false
	}
	// signing_string = "{channel_id}\n{agent_id}\n{timestamp}\n{nonce}"（与注册一致）
	signing := channel.ID + "\n" + agentID + "\n" + timestamp + "\n" + nonce
	if !services.VerifySignatureRaw(channel.Secret, signing, signature) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return false
	}
	return true
}

func (h *MailboxHandler) notifyMailWaiting(agentID string) {
	if h.tunnelMgr == nil {
		return
	}
	agent, err := h.agentSvc.GetByID(agentID)
	if err != nil {
		return
	}
	h.tunnelMgr.BroadcastControl(agent.ChannelID, &services.TunnelMessage{
		Type:    services.TunnelMsgMailWaiting,
		AgentID: agentID,
	})
}

func (h *MailboxHandler) allowMailboxIP(c *gin.Context) bool {
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

func (h *MailboxHandler) writeMailboxErr(c *gin.Context, err error) {
	switch err {
	case services.ErrMailboxAgentNotFound:
		c.JSON(http.StatusNotFound, gin.H{"error": "agent not found"})
	case services.ErrMailboxFull:
		c.JSON(http.StatusInsufficientStorage, gin.H{"error": "mailbox full"})
	case services.ErrMailboxTooLarge:
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "ciphertext too large"})
	case services.ErrMailboxInvalidFP:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid caller_fp"})
	case services.ErrMailboxEmptyBody:
		c.JSON(http.StatusBadRequest, gin.H{"error": "ciphertext required"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "mailbox operation failed"})
	}
}
