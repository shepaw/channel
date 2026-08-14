package handlers

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"github.com/edenzou/channel-service/pkg/internal/services"
	"github.com/gin-gonic/gin"
)

// MailboxHandler 云端收件箱 API（shepaw ↔ agent-bridge 异步信箱）。
//
// Caller（免登录，IP 限流）：
//   POST   /api/v1/mailbox/:target_id/messages
//   GET    /api/v1/mailbox/:target_id/replies?caller_fp=
//   POST   /api/v1/mailbox/:target_id/replies/ack
//   GET    /api/v1/inbox/replies?caller_fp=          （跨 target 统一收取）
//   POST   /api/v1/inbox/replies/ack
//
// Agent/group handler（channel secret HMAC）：
//   GET    /api/v1/mailbox/:target_id/pending
//   POST   /api/v1/mailbox/:target_id/ack
//   POST   /api/v1/mailbox/:target_id/replies
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

const mailboxRateLimit = 30

type depositMessageRequest struct {
	TargetType string `json:"target_type"` // agent | group，默认 agent
	CallerFP   string `json:"caller_fp" binding:"required"`
	MessageID  string `json:"message_id"`
	RequestID  string `json:"request_id"`
	SessionID  string `json:"session_id"`
	GroupID    string `json:"group_id"`
	Kind       string `json:"kind"` // chat | article
	Ciphertext string `json:"ciphertext" binding:"required"`
}

// DepositMessage caller 投递密文留言
// POST /api/v1/mailbox/:target_id/messages
func (h *MailboxHandler) DepositMessage(c *gin.Context) {
	if !h.allowMailboxIP(c) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
		return
	}
	targetID := c.Param("target_id")
	if targetID == "" {
		targetID = c.Param("agent_id") // 兼容旧路由参数名
	}
	var req depositMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	msg, err := h.mailboxSvc.DepositInbound(services.DepositInboundParams{
		TargetType: parseTargetType(req.TargetType),
		TargetID:   targetID,
		CallerFP:   req.CallerFP,
		MessageID:  req.MessageID,
		RequestID:  req.RequestID,
		SessionID:  req.SessionID,
		GroupID:    req.GroupID,
		Kind:       parseMailboxKind(req.Kind),
		Ciphertext: req.Ciphertext,
	})
	if err != nil {
		h.writeMailboxErr(c, err)
		return
	}

	h.notifyMailWaiting(targetID, msg.TargetType)

	pending, _ := h.mailboxSvc.PendingCount(targetID)
	c.JSON(http.StatusCreated, gin.H{
		"id":          msg.ID,
		"message_id":  msg.MessageID,
		"request_id":  msg.RequestID,
		"session_id":  msg.SessionID,
		"group_id":    msg.GroupID,
		"target_type": msg.TargetType,
		"target_id":   msg.TargetID,
		"kind":        msg.Kind,
		"pending":     pending,
		"expires_at":  msg.ExpiresAt.Format(time.RFC3339),
	})
}

// ListReplies caller 拉取指定 target 的回复
// GET /api/v1/mailbox/:target_id/replies?caller_fp=&after=
func (h *MailboxHandler) ListReplies(c *gin.Context) {
	if !h.allowMailboxIP(c) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
		return
	}
	targetID := h.pathTargetID(c)
	callerFP := c.Query("caller_fp")
	if callerFP == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "caller_fp is required"})
		return
	}
	after := parseAfterQuery(c)
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))

	rows, err := h.mailboxSvc.ListReplies(targetID, callerFP, after, limit)
	if err != nil {
		h.writeMailboxErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"replies": serializeReplies(rows), "total": len(rows)})
}

// ListInboxReplies caller 跨 target 统一拉取待收回复（app 上线时收取）
// GET /api/v1/inbox/replies?caller_fp=&after=
func (h *MailboxHandler) ListInboxReplies(c *gin.Context) {
	if !h.allowMailboxIP(c) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
		return
	}
	callerFP := c.Query("caller_fp")
	if callerFP == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "caller_fp is required"})
		return
	}
	after := parseAfterQuery(c)
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))

	rows, err := h.mailboxSvc.ListAllRepliesForCaller(callerFP, after, limit)
	if err != nil {
		h.writeMailboxErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"replies": serializeReplies(rows), "total": len(rows)})
}

type ackRepliesRequest struct {
	CallerFP string   `json:"caller_fp" binding:"required"`
	IDs      []string `json:"ids" binding:"required,min=1"`
}

// AckReplies caller 确认已落本地（单 target）
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
	n, err := h.mailboxSvc.AckReplies(h.pathTargetID(c), req.CallerFP, req.IDs)
	if err != nil {
		h.writeMailboxErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"acked": n})
}

// AckInboxReplies caller 跨 target 确认回复
// POST /api/v1/inbox/replies/ack
func (h *MailboxHandler) AckInboxReplies(c *gin.Context) {
	if !h.allowMailboxIP(c) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
		return
	}
	var req ackRepliesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	n, err := h.mailboxSvc.AckRepliesGlobal(req.CallerFP, req.IDs)
	if err != nil {
		h.writeMailboxErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"acked": n})
}

type agentAuthFields struct {
	Timestamp string `json:"timestamp" binding:"required"`
	Nonce     string `json:"nonce" binding:"required"`
	Signature string `json:"signature" binding:"required"`
}

// ClaimPending agent/group handler 拉取待处理留言
func (h *MailboxHandler) ClaimPending(c *gin.Context) {
	targetID := h.pathTargetID(c)
	if !h.verifyAgentHMAC(c, targetID, c.Query("timestamp"), c.Query("nonce"), c.Query("signature")) {
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "5"))
	rows, err := h.mailboxSvc.ClaimPending(targetID, limit)
	if err != nil {
		h.writeMailboxErr(c, err)
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, m := range rows {
		out = append(out, gin.H{
			"id":          m.ID,
			"message_id":  m.MessageID,
			"request_id":  m.RequestID,
			"session_id":  m.SessionID,
			"group_id":    m.GroupID,
			"target_type": m.TargetType,
			"target_id":   m.TargetID,
			"kind":        m.Kind,
			"caller_fp":   m.CallerFP,
			"ciphertext":  m.Ciphertext,
			"created_at":  m.CreatedAt.Format(time.RFC3339),
		})
	}
	c.JSON(http.StatusOK, gin.H{"messages": out, "total": len(out)})
}

type ackInboundRequest struct {
	agentAuthFields
	IDs []string `json:"ids" binding:"required,min=1"`
}

// AckInbound agent/group handler 确认处理完留言
func (h *MailboxHandler) AckInbound(c *gin.Context) {
	targetID := h.pathTargetID(c)
	var req ackInboundRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !h.verifyAgentHMAC(c, targetID, req.Timestamp, req.Nonce, req.Signature) {
		return
	}
	n, err := h.mailboxSvc.AckInbound(targetID, req.IDs)
	if err != nil {
		h.writeMailboxErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"acked": n})
}

type depositReplyRequest struct {
	agentAuthFields
	CallerFP   string `json:"caller_fp" binding:"required"`
	MessageID  string `json:"message_id"`
	ReplyTo    string `json:"reply_to" binding:"required"`
	RequestID  string `json:"request_id"`
	SessionID  string `json:"session_id"`
	GroupID    string `json:"group_id"`
	Kind       string `json:"kind"`
	Ciphertext string `json:"ciphertext" binding:"required"`
}

// DepositReply agent/group handler 回投回复密文
func (h *MailboxHandler) DepositReply(c *gin.Context) {
	targetID := h.pathTargetID(c)
	var req depositReplyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !h.verifyAgentHMAC(c, targetID, req.Timestamp, req.Nonce, req.Signature) {
		return
	}
	msg, err := h.mailboxSvc.DepositReply(services.DepositReplyParams{
		TargetID:   targetID,
		CallerFP:   req.CallerFP,
		MessageID:  req.MessageID,
		ReplyTo:    req.ReplyTo,
		RequestID:  req.RequestID,
		SessionID:  req.SessionID,
		GroupID:    req.GroupID,
		Kind:       parseMailboxKind(req.Kind),
		Ciphertext: req.Ciphertext,
	})
	if err != nil {
		h.writeMailboxErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"id":          msg.ID,
		"message_id":  msg.MessageID,
		"request_id":  msg.RequestID,
		"reply_to":    msg.ReplyTo,
		"session_id":  msg.SessionID,
		"group_id":    msg.GroupID,
		"target_type": msg.TargetType,
		"target_id":   msg.TargetID,
		"kind":        msg.Kind,
	})
}

func (h *MailboxHandler) pathTargetID(c *gin.Context) string {
	if id := c.Param("target_id"); id != "" {
		return id
	}
	return c.Param("agent_id")
}

func parseAfterQuery(c *gin.Context) time.Time {
	var after time.Time
	if s := c.Query("after"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			after = t
		}
	}
	return after
}

func parseTargetType(s string) models.MailboxTargetType {
	switch s {
	case "group":
		return models.MailboxTargetGroup
	default:
		return models.MailboxTargetAgent
	}
}

func parseMailboxKind(s string) models.MailboxKind {
	switch s {
	case "stream":
		return models.MailboxKindStream
	case "article":
		return models.MailboxKindArticle
	default:
		return models.MailboxKindChat
	}
}

func serializeReplies(rows []models.MailboxMessage) []gin.H {
	out := make([]gin.H, 0, len(rows))
	for _, m := range rows {
		out = append(out, gin.H{
			"id":          m.ID,
			"message_id":  m.MessageID,
			"request_id":  m.RequestID,
			"reply_to":    m.ReplyTo,
			"session_id":  m.SessionID,
			"group_id":    m.GroupID,
			"target_type": m.TargetType,
			"target_id":   m.TargetID,
			"agent_id":    m.TargetID, // 兼容旧客户端
			"kind":        m.Kind,
			"ciphertext":  m.Ciphertext,
			"created_at":  m.CreatedAt.Format(time.RFC3339),
		})
	}
	return out
}

func (h *MailboxHandler) verifyAgentHMAC(c *gin.Context, targetID, timestamp, nonce, signature string) bool {
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
	agent, err := h.agentSvc.GetByID(targetID)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "agent not found"})
		return false
	}
	channel, err := h.channelSvc.GetChannelByID(agent.ChannelID)
	if err != nil || channel.Secret == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "channel secret unavailable"})
		return false
	}
	signing := channel.ID + "\n" + targetID + "\n" + timestamp + "\n" + nonce
	if !services.VerifySignatureRaw(channel.Secret, signing, signature) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return false
	}
	return true
}

func (h *MailboxHandler) notifyMailWaiting(targetID string, targetType models.MailboxTargetType) {
	if h.tunnelMgr == nil || targetType != models.MailboxTargetAgent {
		return
	}
	agent, err := h.agentSvc.GetByID(targetID)
	if err != nil {
		return
	}
	h.tunnelMgr.BroadcastControl(agent.ChannelID, &services.TunnelMessage{
		Type:    services.TunnelMsgMailWaiting,
		AgentID: targetID,
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
	case services.ErrMailboxTargetNotFound, services.ErrMailboxAgentNotFound:
		c.JSON(http.StatusNotFound, gin.H{"error": "mailbox target not found"})
	case services.ErrMailboxInvalidTarget:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid mailbox target"})
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
