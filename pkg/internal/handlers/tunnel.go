package handlers

import (
	"fmt"
	"log"
	"net/http"

	"github.com/edenzou/channel-service/pkg/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// TunnelHandler 处理本地 agent 的隧道连接请求
type TunnelHandler struct {
	tunnelMgr  *services.TunnelManager
	channelSvc *services.ChannelService
	authSvc    *services.AuthService
	nonces     *services.NonceCache // 防重放 nonce 缓存
}

func NewTunnelHandler(tunnelMgr *services.TunnelManager, channelSvc *services.ChannelService, authSvc *services.AuthService) *TunnelHandler {
	return &TunnelHandler{
		tunnelMgr:  tunnelMgr,
		channelSvc: channelSvc,
		authSvc:    authSvc,
		nonces:     services.NewNonceCache(),
	}
}

var tunnelUpgrader = websocket.Upgrader{
	HandshakeTimeout: 10 * 1e9, // 10s
	CheckOrigin: func(r *http.Request) bool {
		return true // agent 连接不限制 origin
	},
}

// Connect 本地 agent 调用此接口建立隧道 WebSocket 连接
// GET /tunnel/connect?channel_id=xxx&timestamp=xxx&nonce=xxx&signature=xxx
// 认证方式：HMAC-SHA256 签名（密钥永远不在请求中传输）
//
// 签名计算方式:
//   signing_string = "{channel_id}\n{timestamp}\n{nonce}"
//   signature = HMAC-SHA256(channel_secret, signing_string)
func (h *TunnelHandler) Connect(c *gin.Context) {
	channelID := c.Query("channel_id")
	if channelID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "channel_id is required"})
		return
	}

	// 提取签名参数
	timestamp := c.Query("timestamp")
	nonce := c.Query("nonce")
	signature := c.Query("signature")

	if timestamp == "" || nonce == "" || signature == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "signature auth required: timestamp, nonce, signature params are required"})
		return
	}

	// 1. 校验时间戳在合理范围内（±5 分钟），防止重放
	if err := services.ValidateTimestamp(timestamp); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": fmt.Sprintf("timestamp validation failed: %v", err)})
		return
	}

	// 2. 校验 nonce 未被使用过（防止同一窗口内重放）
	if !h.nonces.CheckAndStore(nonce) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "nonce already used (possible replay attack)"})
		return
	}

	// 3. 通过 channel_id 查找 channel，获取服务端存储的 secret
	channel, err := h.channelSvc.GetChannelByID(channelID)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "channel not found"})
		return
	}

	// channel 必须有 secret（tunnel 类型）
	if channel.Secret == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "channel has no secret configured"})
		return
	}

	// 4. 使用服务端存储的 secret 验证签名
	if !services.VerifySignature(channel.Secret, channelID, timestamp, nonce, signature) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return
	}

	// channel 必须是 tunnel 类型
	if channel.Type != "tunnel-http" && channel.Type != "tunnel-tcp" && channel.Type != "tunnel-ws" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "channel type must be tunnel-http, tunnel-ws or tunnel-tcp"})
		return
	}

	// channel 必须是激活状态
	if !channel.IsActive {
		c.JSON(http.StatusForbidden, gin.H{"error": "channel is disabled"})
		return
	}

	// Optional short-name alias: agents that want to be reachable at
	// `/c/<alias>/...` instead of `/proxy/<channel_id>/...` pass
	// `&endpoint=<alias>` on the handshake URL. Claim is idempotent —
	// reconnecting with the same alias is fine — but a different channel
	// trying to steal a claimed alias is refused with 409 so the operator
	// sees the collision in the agent logs.
	if alias := c.Query("endpoint"); alias != "" {
		if err := h.channelSvc.ClaimAlias(channelID, alias); err != nil {
			log.Printf("⚠️  Alias claim failed: channel=%s alias=%q err=%v", channelID, alias, err)
			c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("alias claim failed: %v", err)})
			return
		}
		log.Printf("🔗 Channel %s bound alias %q", channelID, alias)
	}

	// WebSocket 升级
	conn, err := tunnelUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("Tunnel WebSocket upgrade failed for %s: %v", channelID, err)
		return
	}

	log.Printf("✅ Tunnel connected: channel=%s type=%s (signature auth)", channelID, channel.Type)

	// 注册到 TunnelManager（自动踢掉旧连接）
	// 注意：这里传入 channel.Secret 用于后续 rotate 时踢出旧连接
	h.tunnelMgr.Register(channelID, channel.Secret, conn)
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

