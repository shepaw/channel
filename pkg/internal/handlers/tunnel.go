package handlers

import (
	"log"
	"net/http"
	"strings"

	"github.com/edenzou/channel-service/pkg/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// TunnelHandler 处理本地 agent 的隧道连接请求
type TunnelHandler struct {
	tunnelMgr  *services.TunnelManager
	channelSvc *services.ChannelService
	authSvc    *services.AuthService
}

func NewTunnelHandler(tunnelMgr *services.TunnelManager, channelSvc *services.ChannelService, authSvc *services.AuthService) *TunnelHandler {
	return &TunnelHandler{
		tunnelMgr:  tunnelMgr,
		channelSvc: channelSvc,
		authSvc:    authSvc,
	}
}

var tunnelUpgrader = websocket.Upgrader{
	HandshakeTimeout: 10 * 1e9, // 10s
	CheckOrigin: func(r *http.Request) bool {
		return true // agent 连接不限制 origin
	},
}

// Connect 本地 agent 调用此接口建立隧道 WebSocket 连接
// GET /tunnel/connect?channel_id=xxx&secret=ch_sec_xxx
// 认证方式：channel_secret（永久密钥，不依赖临时 token）
func (h *TunnelHandler) Connect(c *gin.Context) {
	channelID := c.Query("channel_id")
	if channelID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "channel_id is required"})
		return
	}

	// 支持 query param secret= 或 Authorization: Bearer ch_sec_xxx
	secret := c.Query("secret")
	if secret == "" {
		secret = extractBearerToken(c.Request)
	}
	if secret == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "channel secret is required (use ?secret=ch_sec_xxx or Authorization: Bearer)"})
		return
	}

	// 必须以 ch_sec_ 开头，避免误用 user token
	if !strings.HasPrefix(secret, "ch_sec_") {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid secret format, must be a channel secret (ch_sec_...)"})
		return
	}

	// 通过 secret 查找 channel（直接验证归属，无需额外 token）
	channel, err := h.channelSvc.GetChannelBySecret(secret)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid channel secret"})
		return
	}

	// 二次确认：channel_id 和 secret 必须匹配（防止 secret 被用于其他 channel）
	if channel.ID != channelID {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "channel_id and secret do not match"})
		return
	}

	// channel 必须是 tunnel 类型
	if channel.Type != "tunnel-http" && channel.Type != "tunnel-tcp" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "channel type must be tunnel-http or tunnel-tcp"})
		return
	}

	// channel 必须是激活状态
	if !channel.IsActive {
		c.JSON(http.StatusForbidden, gin.H{"error": "channel is disabled"})
		return
	}

	// WebSocket 升级
	conn, err := tunnelUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("Tunnel WebSocket upgrade failed for %s: %v", channelID, err)
		return
	}

	log.Printf("✅ Tunnel connected: channel=%s type=%s", channelID, channel.Type)

	// 注册到 TunnelManager（自动踢掉旧连接）
	h.tunnelMgr.Register(channelID, secret, conn)
}

// Status 查询指定 channel 的隧道是否在线（无需认证，公开接口）
// GET /tunnel/status/:channel_id
func (h *TunnelHandler) Status(c *gin.Context) {
	channelID := c.Param("channel_id")
	online := h.tunnelMgr.IsOnline(channelID)
	c.JSON(http.StatusOK, gin.H{
		"channel_id": channelID,
		"online":     online,
	})
}

// RotateSecret 重新生成 channel secret，并踢掉当前在线的 agent（需要用户认证）
// POST /api/v1/channels/:id/rotate-secret
func (h *TunnelHandler) RotateSecret(c *gin.Context) {
	channelID := c.Param("id")
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// 先拿到旧 secret，用于踢掉在线连接
	oldChannel, err := h.channelSvc.GetChannelByID(channelID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
		return
	}

	// 生成新 secret
	newSecret, err := h.channelSvc.RotateSecret(userID, channelID)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}

	// 踢掉使用旧 secret 的在线 agent
	if oldChannel.Secret != "" {
		h.tunnelMgr.KickIfSecret(channelID, oldChannel.Secret)
		log.Printf("🔑 Secret rotated for channel %s, old agent kicked", channelID)
	}

	c.JSON(http.StatusOK, gin.H{
		"channel_id": channelID,
		"secret":     newSecret,
		"message":    "Secret rotated. Old agent has been disconnected.",
	})
}

// extractBearerToken 从 Authorization header 提取 Bearer token
func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if len(auth) > 7 && auth[:7] == "Bearer " {
		return auth[7:]
	}
	return ""
}
