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

// AgentHandler agent 注册中心接口：
//   - POST /api/v1/agents/register              agent 侧上报（HMAC 签名，无需用户 token）
//   - GET  /api/v1/channels/:id/agents          owner 查看 channel 下的 agent 列表
//   - PUT  /api/v1/agents/:agent_id             owner 更新名片 / 公开开关
//   - DELETE /api/v1/agents/:agent_id           owner 注销 agent
//   - GET  /api/v1/discovery/agents             公开目录搜索（免登录 + IP 限流）
//   - GET  /api/v1/discovery/agents/:agent_id   公开名片
type AgentHandler struct {
	agentSvc   *services.AgentService
	channelSvc *services.ChannelService
	redis      *services.RedisService
	nonces     *services.NonceCache
}

func NewAgentHandler(agentSvc *services.AgentService, channelSvc *services.ChannelService, redis *services.RedisService) *AgentHandler {
	return &AgentHandler{
		agentSvc:   agentSvc,
		channelSvc: channelSvc,
		redis:      redis,
		nonces:     services.NewNonceCache(),
	}
}

// discoveryRateLimit 公开搜索接口每 IP 每分钟请求上限
const discoveryRateLimit = 60

// PublicAgentView 公开名片视图——刻意不暴露 channel_id / endpoint / secret / stats
type PublicAgentView struct {
	AgentID     string `json:"agent_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	AgentFP     string `json:"agent_fp"`
	Capacity    int    `json:"capacity"`
	Online      bool   `json:"online"`
}

func toPublicView(a models.Agent) PublicAgentView {
	return PublicAgentView{
		AgentID:     a.AgentID,
		Name:        a.Name,
		Description: a.Description,
		AgentFP:     a.AgentFP,
		Capacity:    a.Capacity,
		Online:      a.Online(),
	}
}

// RegisterAgentRequest 注册/心跳请求体。
// 认证方式与 /tunnel/connect 相同：HMAC-SHA256(channel_secret, signing_string)，
// signing_string = "{channel_id}\n{agent_id}\n{timestamp}\n{nonce}"（比隧道签名串多一段 agent_id，
// 防止签名被转用到其他 agent）。secret 永不在请求中传输。
type RegisterAgentRequest struct {
	ChannelID   string `json:"channel_id" binding:"required"`
	AgentID     string `json:"agent_id"   binding:"required"`
	AgentFP     string `json:"agent_fp"`
	Name        string `json:"name"        binding:"max=100"`
	Description string `json:"description"`
	PathPrefix  string `json:"path_prefix" binding:"max=128"`
	DeviceID    string `json:"device_id"   binding:"max=64"`
	Capacity    int    `json:"capacity"     binding:"min=0,max=1000"`
	ActiveCount int    `json:"active_count" binding:"min=0"`
	Timestamp   string `json:"timestamp"   binding:"required"`
	Nonce       string `json:"nonce"       binding:"required"`
	Signature   string `json:"signature"   binding:"required"`
}

// Register 注册或刷新 agent（幂等，兼作心跳）。
// hub 模式下一台设备的多个实例各调一次；重复调用刷新 LastSeenAt。
func (h *AgentHandler) Register(c *gin.Context) {
	var req RegisterAgentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 1. 时间戳窗口校验（±5 分钟）
	if err := services.ValidateTimestamp(req.Timestamp); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "timestamp validation failed: " + err.Error()})
		return
	}

	// 2. nonce 防重放
	if !h.nonces.CheckAndStore(req.Nonce) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "nonce already used (possible replay attack)"})
		return
	}

	// 3. 查 channel 拿 secret 验签
	channel, err := h.channelSvc.GetChannelByID(req.ChannelID)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "channel not found"})
		return
	}
	if channel.Secret == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "channel has no secret configured"})
		return
	}
	if !verifyAgentSignature(channel.Secret, req.ChannelID, req.AgentID, req.Timestamp, req.Nonce, req.Signature) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return
	}

	// 4. upsert
	agent, err := h.agentSvc.Register(services.RegisterAgentParams{
		AgentID:     req.AgentID,
		ChannelID:   req.ChannelID,
		Name:        req.Name,
		Description: req.Description,
		AgentFP:     req.AgentFP,
		PathPrefix:  req.PathPrefix,
		DeviceID:    req.DeviceID,
		Capacity:    req.Capacity,
		ActiveCount: req.ActiveCount,
	})
	if err != nil {
		switch err {
		case services.ErrAgentChannelBound:
			c.JSON(http.StatusConflict, gin.H{"error": "agent_id already bound to another channel"})
		case services.ErrInvalidAgentID:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent_id format"})
		case services.ErrInvalidAgentFP:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent_fp format"})
		case services.ErrAgentChannelNoOwner:
			c.JSON(http.StatusBadRequest, gin.H{"error": "channel has no owner"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to register agent"})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"agent_id": agent.AgentID,
		"online":   agent.Online(),
		"message":  "agent registered",
	})
}

// ListByChannel 列出 channel 下的 agent（owner 鉴权）
// GET /api/v1/channels/:id/agents
func (h *AgentHandler) ListByChannel(c *gin.Context) {
	channelID := c.Param("id")
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if !h.channelSvc.IsOwner(userID, channelID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "not channel owner"})
		return
	}

	agents, err := h.agentSvc.ListByChannel(channelID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list agents"})
		return
	}

	// 附带在线状态（基于 last_seen_at 新鲜度）
	type agentView struct {
		AgentID     string `json:"agent_id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		AgentFP     string `json:"agent_fp"`
		PathPrefix  string `json:"path_prefix"`
		Capacity    int    `json:"capacity"`
		ActiveCount int    `json:"active_count"`
		IsPublic    bool   `json:"is_public"`
		Online      bool   `json:"online"`
		LastSeenAt  string `json:"last_seen_at"`
		CreatedAt   string `json:"created_at"`
	}
	views := make([]agentView, 0, len(agents))
	for _, a := range agents {
		views = append(views, agentView{
			AgentID:     a.AgentID,
			Name:        a.Name,
			Description: a.Description,
			AgentFP:     a.AgentFP,
			PathPrefix:  a.PathPrefix,
			Capacity:    a.Capacity,
			ActiveCount: a.ActiveCount,
			IsPublic:    a.IsPublic,
			Online:      a.Online(),
			LastSeenAt:  a.LastSeenAt.Format("2006-01-02 15:04:05"),
			CreatedAt:   a.CreatedAt.Format("2006-01-02 15:04:05"),
		})
	}

	c.JSON(http.StatusOK, gin.H{"agents": views, "total": len(views)})
}

// UpdateAgentRequest owner 更新名片。指针字段：nil=不改，非 nil=写入（含空串/false）。
type UpdateAgentRequest struct {
	Name        *string `json:"name" binding:"omitempty,max=100"`
	Description *string `json:"description"`
	IsPublic    *bool   `json:"is_public"`
	Capacity    *int    `json:"capacity" binding:"omitempty,min=1,max=1000"`
}

// Update 更新 agent 名片 / 公开开关（owner 鉴权）
// PUT /api/v1/agents/:agent_id
func (h *AgentHandler) Update(c *gin.Context) {
	agentID := c.Param("agent_id")
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	var req UpdateAgentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	agent, err := h.agentSvc.Update(userID, agentID, services.UpdateAgentParams{
		Name:        req.Name,
		Description: req.Description,
		IsPublic:    req.IsPublic,
		Capacity:    req.Capacity,
	})
	if err != nil {
		switch err {
		case services.ErrAgentNotFound:
			c.JSON(http.StatusNotFound, gin.H{"error": "agent not found"})
		case services.ErrNotChannelOwner:
			c.JSON(http.StatusForbidden, gin.H{"error": "not agent owner"})
		case services.ErrAgentNameRequired:
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is required when making agent public"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update agent"})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"agent_id":    agent.AgentID,
		"name":        agent.Name,
		"description": agent.Description,
		"is_public":   agent.IsPublic,
		"capacity":    agent.Capacity,
		"online":      agent.Online(),
	})
}

// Delete 注销 agent（owner 鉴权）
// DELETE /api/v1/agents/:agent_id
func (h *AgentHandler) Delete(c *gin.Context) {
	agentID := c.Param("agent_id")
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	if err := h.agentSvc.Delete(userID, agentID); err != nil {
		switch err {
		case services.ErrAgentNotFound:
			c.JSON(http.StatusNotFound, gin.H{"error": "agent not found"})
		case services.ErrNotChannelOwner:
			c.JSON(http.StatusForbidden, gin.H{"error": "not agent owner"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete agent"})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "agent deleted"})
}

// Discover 公开目录搜索（免登录）。只返回名片字段，不含 endpoint。
// GET /api/v1/discovery/agents?q=&page=&page_size=
func (h *AgentHandler) Discover(c *gin.Context) {
	if !h.allowDiscovery(c) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded, try again later"})
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))

	agents, total, err := h.agentSvc.SearchPublic(services.SearchPublicParams{
		Query:    c.Query("q"),
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to search agents"})
		return
	}

	views := make([]PublicAgentView, 0, len(agents))
	for _, a := range agents {
		views = append(views, toPublicView(a))
	}

	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 50 {
		pageSize = 50
	}

	c.JSON(http.StatusOK, gin.H{
		"agents":    views,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

// GetPublicCard 取单个公开名片；非公开与不存在统一 404，防枚举。
// GET /api/v1/discovery/agents/:agent_id
func (h *AgentHandler) GetPublicCard(c *gin.Context) {
	if !h.allowDiscovery(c) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded, try again later"})
		return
	}

	agent, err := h.agentSvc.GetPublicCard(c.Param("agent_id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "agent not found"})
		return
	}
	c.JSON(http.StatusOK, toPublicView(*agent))
}

// allowDiscovery IP 限流：每 IP 每分钟 discoveryRateLimit 次。
func (h *AgentHandler) allowDiscovery(c *gin.Context) bool {
	if h.redis == nil {
		return true
	}
	ip := c.ClientIP()
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	window := time.Now().Truncate(time.Minute).Unix()
	key := fmt.Sprintf("rl:discovery:%s:%d", ip, window)
	n, err := h.redis.IncrBy(key, 1)
	if err != nil {
		return true // 限流失败时放行，避免拖垮公开接口
	}
	if n == 1 {
		_ = h.redis.Expire(key, 2*time.Minute)
	}
	return n <= discoveryRateLimit
}

// verifyAgentSignature 校验注册签名。
// signingString = "{channel_id}\n{agent_id}\n{timestamp}\n{nonce}"
func verifyAgentSignature(secret, channelID, agentID, timestamp, nonce, signature string) bool {
	signingString := channelID + "\n" + agentID + "\n" + timestamp + "\n" + nonce
	return services.VerifySignatureRaw(secret, signingString, signature)
}
