package handlers

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"github.com/edenzou/channel-service/pkg/internal/services"
	"github.com/gin-gonic/gin"
)

// AccessHandler 接入申请中介 API。
//
// Caller（免登录 + IP 限流）：
//   POST /api/v1/access-requests
//   GET  /api/v1/access-requests/mine?agent_id=&caller_fp=
//
// Owner（Bearer）：
//   GET  /api/v1/agents/:agent_id/access-requests
//   POST /api/v1/access-requests/:id/approve|reject|revoke
//
// Agent（HMAC，与 register 同签名串）：
//   GET  /api/v1/agents/:agent_id/grants?since=&timestamp=&nonce=&signature=
type AccessHandler struct {
	accessSvc  *services.AccessService
	agentSvc   *services.AgentService
	channelSvc *services.ChannelService
	tunnelMgr  *services.TunnelManager
	redis      *services.RedisService
	nonces     *services.NonceCache
	baseURL    string
}

func NewAccessHandler(
	accessSvc *services.AccessService,
	agentSvc *services.AgentService,
	channelSvc *services.ChannelService,
	tunnelMgr *services.TunnelManager,
	redis *services.RedisService,
	baseURL string,
) *AccessHandler {
	return &AccessHandler{
		accessSvc:  accessSvc,
		agentSvc:   agentSvc,
		channelSvc: channelSvc,
		tunnelMgr:  tunnelMgr,
		redis:      redis,
		nonces:     services.NewNonceCache(),
		baseURL:    baseURL,
	}
}

const accessRateLimit = 30

type createAccessRequest struct {
	AgentID      string `json:"agent_id"      binding:"required"`
	CallerFP     string `json:"caller_fp"     binding:"required"`
	CallerPubKey string `json:"caller_pubkey" binding:"required"`
	CallerName   string `json:"caller_name"   binding:"max=100"`
	Message      string `json:"message"       binding:"max=500"`
}

// CreateRequest POST /api/v1/access-requests
func (h *AccessHandler) CreateRequest(c *gin.Context) {
	if !h.allowAccessIP(c) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
		return
	}
	var req createAccessRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	grant, err := h.accessSvc.Request(services.RequestAccessParams{
		AgentID:      req.AgentID,
		CallerFP:     req.CallerFP,
		CallerPubKey: req.CallerPubKey,
		CallerName:   req.CallerName,
		Message:      req.Message,
	})
	if err != nil {
		h.writeAccessErr(c, err)
		return
	}
	h.notifyGrantChange(grant.AgentID)
	c.JSON(http.StatusCreated, h.grantView(grant, false))
}

// GetMine GET /api/v1/access-requests/mine?agent_id=&caller_fp=
func (h *AccessHandler) GetMine(c *gin.Context) {
	if !h.allowAccessIP(c) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
		return
	}
	agentID := c.Query("agent_id")
	callerFP := c.Query("caller_fp")
	if agentID == "" || callerFP == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "agent_id and caller_fp required"})
		return
	}
	grant, err := h.accessSvc.GetMine(agentID, callerFP)
	if err != nil {
		h.writeAccessErr(c, err)
		return
	}
	c.JSON(http.StatusOK, h.grantView(grant, grant.Status == models.AccessApproved))
}

// ListByAgent GET /api/v1/agents/:agent_id/access-requests
func (h *AccessHandler) ListByAgent(c *gin.Context) {
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	rows, err := h.accessSvc.ListByAgent(userID, c.Param("agent_id"), c.Query("status"))
	if err != nil {
		h.writeAccessErr(c, err)
		return
	}
	out := make([]gin.H, 0, len(rows))
	for i := range rows {
		out = append(out, h.grantView(&rows[i], false))
	}
	c.JSON(http.StatusOK, gin.H{"requests": out, "total": len(out)})
}

// Approve POST /api/v1/access-requests/:id/approve
func (h *AccessHandler) Approve(c *gin.Context) {
	h.decide(c, models.AccessApproved)
}

// Reject POST /api/v1/access-requests/:id/reject
func (h *AccessHandler) Reject(c *gin.Context) {
	h.decide(c, models.AccessRejected)
}

// Revoke POST /api/v1/access-requests/:id/revoke
func (h *AccessHandler) Revoke(c *gin.Context) {
	h.decide(c, models.AccessRevoked)
}

func (h *AccessHandler) decide(c *gin.Context, next models.AccessGrantStatus) {
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	grant, err := h.accessSvc.Decide(userID, c.Param("id"), next)
	if err != nil {
		h.writeAccessErr(c, err)
		return
	}
	h.notifyGrantChange(grant.AgentID)
	c.JSON(http.StatusOK, h.grantView(grant, false))
}

// ListGrantsForAgent GET /api/v1/agents/:agent_id/grants
func (h *AccessHandler) ListGrantsForAgent(c *gin.Context) {
	agentID := c.Param("agent_id")
	if !h.verifyAgentHMAC(c, agentID, c.Query("timestamp"), c.Query("nonce"), c.Query("signature")) {
		return
	}
	var since time.Time
	if s := c.Query("since"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			since = t
		}
	}
	rows, err := h.accessSvc.ListForAgentSync(agentID, since)
	if err != nil {
		h.writeAccessErr(c, err)
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, g := range rows {
		out = append(out, gin.H{
			"id":            g.ID,
			"caller_fp":     g.CallerFP,
			"caller_pubkey": g.CallerPubKey,
			"caller_name":   g.CallerName,
			"status":        g.Status,
			"updated_at":    g.UpdatedAt.Format(time.RFC3339),
		})
	}
	c.JSON(http.StatusOK, gin.H{"grants": out, "total": len(out)})
}

func (h *AccessHandler) grantView(g *models.AgentAccessGrant, withConnect bool) gin.H {
	view := gin.H{
		"id":            g.ID,
		"agent_id":      g.AgentID,
		"caller_fp":     g.CallerFP,
		"caller_pubkey": g.CallerPubKey,
		"caller_name":   g.CallerName,
		"message":       g.Message,
		"status":        g.Status,
		"created_at":    g.CreatedAt.Format(time.RFC3339),
		"updated_at":    g.UpdatedAt.Format(time.RFC3339),
	}
	if g.DecidedAt != nil {
		view["decided_at"] = g.DecidedAt.Format(time.RFC3339)
	}
	if withConnect {
		if agent, err := h.agentSvc.GetByID(g.AgentID); err == nil {
			view["endpoint"] = services.BuildAgentWSEndpoint(h.baseURL, agent.ChannelID, agent.PathPrefix, agent.AgentID)
			view["agent_fp"] = agent.AgentFP
			view["agent_pubkey"] = agent.AgentPubKey
			view["agent_name"] = agent.Name
			view["channel_id"] = agent.ChannelID
			view["online"] = services.AgentReachable(agent, h.tunnelMgr)
		}
	}
	return view
}

func (h *AccessHandler) verifyAgentHMAC(c *gin.Context, agentID, timestamp, nonce, signature string) bool {
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
	signing := channel.ID + "\n" + agentID + "\n" + timestamp + "\n" + nonce
	if !services.VerifySignatureRaw(channel.Secret, signing, signature) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return false
	}
	return true
}

func (h *AccessHandler) notifyGrantChange(agentID string) {
	if h.tunnelMgr == nil {
		return
	}
	agent, err := h.agentSvc.GetByID(agentID)
	if err != nil {
		return
	}
	h.tunnelMgr.BroadcastControl(agent.ChannelID, &services.TunnelMessage{
		Type:    services.TunnelMsgAccessGrant,
		AgentID: agentID,
	})
}

func (h *AccessHandler) allowAccessIP(c *gin.Context) bool {
	if h.redis == nil {
		return true
	}
	ip := c.ClientIP()
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	window := time.Now().Truncate(time.Minute).Unix()
	key := fmt.Sprintf("rl:access:%s:%d", ip, window)
	n, err := h.redis.IncrBy(key, 1)
	if err != nil {
		return true
	}
	if n == 1 {
		_ = h.redis.Expire(key, 2*time.Minute)
	}
	return n <= accessRateLimit
}

func (h *AccessHandler) writeAccessErr(c *gin.Context, err error) {
	switch err {
	case services.ErrAccessNotFound, services.ErrAgentNotFound:
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case services.ErrAccessNotOwner:
		c.JSON(http.StatusForbidden, gin.H{"error": "not agent owner"})
	case services.ErrAccessNotPublic:
		c.JSON(http.StatusForbidden, gin.H{"error": "agent is not public"})
	case services.ErrAccessInvalidPubKey, services.ErrAccessFPMismatch, services.ErrMailboxInvalidFP:
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case services.ErrAccessBadStatus:
		c.JSON(http.StatusConflict, gin.H{"error": "invalid status transition"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "access operation failed"})
	}
}
