package handlers

import (
	"net/http"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"github.com/edenzou/channel-service/pkg/internal/services"
	"github.com/gin-gonic/gin"
)

// ChannelHandler 处理 channel CRUD 和限流规则管理
type ChannelHandler struct {
	channelSvc   *services.ChannelService
	channelSvcV2 *services.ChannelServiceV2
	rateLimitSvc *services.RateLimitService
	proxyHandler *ProxyHandler // 用于 TCP/UDP 注册时启动监听器
	config       *models.Config
}

func NewChannelHandler(channelSvc *services.ChannelService, rateLimitSvc *services.RateLimitService, config *models.Config) *ChannelHandler {
	return &ChannelHandler{
		channelSvc:   channelSvc,
		rateLimitSvc: rateLimitSvc,
		config:       config,
	}
}

// SetV2 注入 V2 服务和代理 handler（在 main 中调用）
func (h *ChannelHandler) SetV2(v2 *services.ChannelServiceV2, ph *ProxyHandler) {
	h.channelSvcV2 = v2
	h.proxyHandler = ph
}

type CreateChannelRequest struct {
	Name        string                 `json:"name"   binding:"required,max=100"`
	Description string                 `json:"description"`
	Type        string                 `json:"type"   binding:"required,oneof=http https ws tcp udp"`
	Target      string                 `json:"target" binding:"required"`
	Config      map[string]interface{} `json:"config"`
}

type UpdateChannelRequest struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	IsActive    *bool                  `json:"is_active"`
	Config      map[string]interface{} `json:"config"`
}

type AddRateLimitRuleRequest struct {
	RuleType   string  `json:"rule_type"    binding:"required,oneof=bandwidth connections requests"`
	LimitValue float64 `json:"limit_value"  binding:"required,min=0"`
	TimeWindow string  `json:"time_window"  binding:"required"` // "1s", "1m", "1h"
}

// Create 创建 channel，需要 access token 认证
func (h *ChannelHandler) Create(c *gin.Context) {
	var req CreateChannelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
		return
	}

	// TCP/UDP 需要通过 V2 分配端口
	if (req.Type == "tcp" || req.Type == "udp") && h.channelSvcV2 != nil && h.proxyHandler != nil {
		channel, port, err := h.channelSvcV2.CreateChannelWithPort(userID, req.Name, req.Description, req.Type, req.Target, req.Config)
		if err != nil {
			switch err {
			case services.ErrMaxChannelsExceeded:
				c.JSON(http.StatusForbidden, gin.H{"error": "Maximum number of channels exceeded"})
			case services.ErrInvalidChannelType:
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel type"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create channel: " + err.Error()})
			}
			return
		}

		// 立即启动监听器
		var startErr error
		if req.Type == "tcp" {
			startErr = h.proxyHandler.StartTCPProxy(channel.ID, channel.Target, port)
		} else {
			startErr = h.proxyHandler.StartUDPProxy(channel.ID, channel.Target, port)
		}
		if startErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Channel created but proxy failed to start: " + startErr.Error()})
			return
		}

		c.JSON(http.StatusCreated, channel)
		return
	}

	// HTTP/HTTPS/WS/tunnel-http/tunnel-tcp
	channel, err := h.channelSvc.CreateChannel(userID, req.Name, req.Description, req.Type, req.Target, req.Config)
	if err != nil {
		switch err {
		case services.ErrMaxChannelsExceeded:
			c.JSON(http.StatusForbidden, gin.H{"error": "Maximum number of channels exceeded"})
		case services.ErrInvalidChannelType:
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid channel type"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create channel: " + err.Error()})
		}
		return
	}

	// tunnel 类型：在响应中附带 secret 和启动命令（仅首次创建时展示 secret）
	if channel.Secret != "" {
		baseURL := h.config.BaseURL
		setupCmd := "channel-agent --server " + baseURL +
			" --channel-id " + channel.ID +
			" --secret " + channel.Secret +
			" --target " + channel.Target
		c.JSON(http.StatusCreated, gin.H{
			"channel":       channel,
			"secret":        channel.Secret,
			"setup_command": setupCmd,
			"notice":        "⚠️  请保存此 secret，后续无法再次查看。如需重置请调用 rotate-secret 接口。",
		})
		return
	}

	c.JSON(http.StatusCreated, channel)
}

// GetAll 获取当前用户的所有 channel
func (h *ChannelHandler) GetAll(c *gin.Context) {
	userID := c.GetString("user_id")
	channels, err := h.channelSvc.GetUserChannels(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get channels"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"channels": channels, "total": len(channels)})
}

// GetByID 获取单个 channel
func (h *ChannelHandler) GetByID(c *gin.Context) {
	channelID := c.Param("id")
	userID := c.GetString("user_id")

	channel, err := h.channelSvc.GetChannelByID(channelID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Channel not found"})
		return
	}

	if !h.channelSvc.IsOwner(userID, channelID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not channel owner"})
		return
	}

	c.JSON(http.StatusOK, channel)
}

// Update 更新 channel
func (h *ChannelHandler) Update(c *gin.Context) {
	channelID := c.Param("id")
	userID := c.GetString("user_id")

	var req UpdateChannelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	updates := map[string]interface{}{}
	if req.Name != "" {
		updates["name"] = req.Name
	}
	if req.Description != "" {
		updates["description"] = req.Description
	}
	if req.IsActive != nil {
		updates["is_active"] = *req.IsActive
	}
	if req.Config != nil {
		updates["config"] = req.Config
	}

	channel, err := h.channelSvc.UpdateChannel(userID, channelID, updates)
	if err != nil {
		switch err {
		case services.ErrNotChannelOwner:
			c.JSON(http.StatusForbidden, gin.H{"error": "Not channel owner"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update channel"})
		}
		return
	}

	c.JSON(http.StatusOK, channel)
}

// Delete 删除 channel
func (h *ChannelHandler) Delete(c *gin.Context) {
	channelID := c.Param("id")
	userID := c.GetString("user_id")

	if err := h.channelSvc.DeleteChannel(userID, channelID); err != nil {
		switch err {
		case services.ErrNotChannelOwner:
			c.JSON(http.StatusForbidden, gin.H{"error": "Not channel owner"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete channel"})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Channel deleted successfully"})
}

// AddRateLimitRule 为 channel 添加限流规则
func (h *ChannelHandler) AddRateLimitRule(c *gin.Context) {
	channelID := c.Param("id")
	userID := c.GetString("user_id")

	var req AddRateLimitRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if !h.channelSvc.IsOwner(userID, channelID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Not channel owner"})
		return
	}

	timeWindow, err := time.ParseDuration(req.TimeWindow)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid time_window format (e.g. 1s, 1m, 1h)"})
		return
	}

	if err := h.rateLimitSvc.AddRule(channelID, req.RuleType, req.LimitValue, timeWindow); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to add rate limit rule"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"message": "Rate limit rule added successfully"})
}

// GetRateLimitRules 获取 channel 的限流规则列表
func (h *ChannelHandler) GetRateLimitRules(c *gin.Context) {
	channelID := c.Param("id")
	rules, err := h.rateLimitSvc.GetRules(channelID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get rate limit rules"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"rules": rules, "total": len(rules)})
}

// DeleteRateLimitRule 删除限流规则
func (h *ChannelHandler) DeleteRateLimitRule(c *gin.Context) {
	ruleID := c.Param("rule_id")
	if err := h.rateLimitSvc.DeleteRule(ruleID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete rate limit rule"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Rate limit rule deleted successfully"})
}
