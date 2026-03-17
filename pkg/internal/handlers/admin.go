package handlers

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"github.com/edenzou/channel-service/pkg/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	// Admin session cookie name
	adminSessionCookie = "admin_session"

	// Admin session TTL
	adminSessionTTL = 8 * time.Hour
)

// AdminHandler 管理后台处理器
type AdminHandler struct {
	authSvc *services.AuthService
	redis   *services.RedisService
	config  *models.Config
}

func NewAdminHandler(authSvc *services.AuthService, redis *services.RedisService, config *models.Config) *AdminHandler {
	return &AdminHandler{authSvc: authSvc, redis: redis, config: config}
}

// ─── 页面路由 ────────────────────────────────────────────────────────────────

func (h *AdminHandler) LoginPage(c *gin.Context) {
	// 已登录直接跳转
	if h.isAdminLoggedIn(c) {
		c.Redirect(http.StatusTemporaryRedirect, "/admin/dashboard")
		return
	}
	c.HTML(http.StatusOK, "admin_login.html", nil)
}

func (h *AdminHandler) DashboardPage(c *gin.Context) {
	c.HTML(http.StatusOK, "admin_dashboard.html", gin.H{
		"AdminEmail": h.config.AdminEmail,
	})
}

// ─── Google OAuth（Admin 专用） ───────────────────────────────────────────────

// GoogleInitiate 发起 Admin Google 登录（state 加 admin: 前缀，与普通登录隔离）
func (h *AdminHandler) GoogleInitiate(c *gin.Context) {
	clientID := h.config.GoogleClientID
	if clientID == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Google login not configured"})
		return
	}

	state := "admin:" + uuid.New().String()
	h.redis.Set(fmt.Sprintf("admin:google:state:%s", state), "1", 10*time.Minute)

	redirectURI := url.QueryEscape(fmt.Sprintf("%s/admin/auth/google/callback", h.config.BaseURL))
	authURL := fmt.Sprintf(
		"https://accounts.google.com/o/oauth2/v2/auth?client_id=%s&redirect_uri=%s&response_type=code&scope=email+profile&state=%s",
		clientID, redirectURI, state,
	)
	c.Redirect(http.StatusTemporaryRedirect, authURL)
}

// GoogleCallback Admin Google OAuth 回调
func (h *AdminHandler) GoogleCallback(c *gin.Context) {
	code := c.Query("code")
	state := c.Query("state")

	if code == "" {
		c.Redirect(http.StatusTemporaryRedirect, "/admin/login?error=no_code")
		return
	}

	// 验证 state
	stateKey := fmt.Sprintf("admin:google:state:%s", state)
	if _, err := h.redis.Get(stateKey); err != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/admin/login?error=invalid_state")
		return
	}
	h.redis.Delete(stateKey)

	// 换取 token
	googleToken, err := exchangeGoogleCode(
		h.config.GoogleClientID,
		h.config.GoogleClientSecret,
		fmt.Sprintf("%s/admin/auth/google/callback", h.config.BaseURL),
		code,
	)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/admin/login?error=google_exchange_failed")
		return
	}

	// 获取用户信息
	googleUser, err := getGoogleUserInfo(googleToken.AccessToken)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, "/admin/login?error=google_userinfo_failed")
		return
	}

	// ★ 邮箱白名单校验
	if googleUser.Email != h.config.AdminEmail {
		c.Redirect(http.StatusTemporaryRedirect, "/admin/login?error=unauthorized")
		return
	}

	// 创建 admin session
	sessionID := uuid.New().String()
	sessionKey := fmt.Sprintf("admin:session:%s", sessionID)
	h.redis.Set(sessionKey, googleUser.Email, adminSessionTTL)

	// 写 cookie（HttpOnly + SameSite=Lax）
	maxAge := int(adminSessionTTL.Seconds())
	c.SetCookie(adminSessionCookie, sessionID, maxAge, "/admin", "", false, true)

	c.Redirect(http.StatusTemporaryRedirect, "/admin/dashboard")
}

// Logout 退出
func (h *AdminHandler) Logout(c *gin.Context) {
	if sessionID, err := c.Cookie(adminSessionCookie); err == nil {
		h.redis.Delete(fmt.Sprintf("admin:session:%s", sessionID))
	}
	c.SetCookie(adminSessionCookie, "", -1, "/admin", "", false, true)
	c.Redirect(http.StatusTemporaryRedirect, "/admin/login")
}

// ─── Admin API ────────────────────────────────────────────────────────────────

// GetStats 概览统计
func (h *AdminHandler) GetStats(c *gin.Context) {
	db := h.authSvc.DB()

	var userCount, channelCount, activeChannelCount int64
	db.Model(&models.User{}).Count(&userCount)
	db.Model(&models.Channel{}).Count(&channelCount)
	db.Model(&models.Channel{}).Where("is_active = ?", true).Count(&activeChannelCount)

	c.JSON(http.StatusOK, gin.H{
		"users":           userCount,
		"channels":        channelCount,
		"active_channels": activeChannelCount,
	})
}

// ListUsers 用户列表（分页）
func (h *AdminHandler) ListUsers(c *gin.Context) {
	db := h.authSvc.DB()

	page, size := parsePagination(c)
	offset := (page - 1) * size

	var total int64
	db.Model(&models.User{}).Count(&total)

	var users []models.User
	db.Order("created_at DESC").Offset(offset).Limit(size).Find(&users)

	// 附带每个用户的 channel 数
	type UserRow struct {
		models.User
		ChannelCount int64 `json:"channel_count"`
	}
	rows := make([]UserRow, 0, len(users))
	for _, u := range users {
		var cnt int64
		db.Model(&models.UserChannel{}).
			Where("user_id = ? AND deleted_at IS NULL", u.ID).
			Count(&cnt)
		rows = append(rows, UserRow{User: u, ChannelCount: cnt})
	}

	c.JSON(http.StatusOK, gin.H{
		"users": rows,
		"pagination": gin.H{
			"page":  page,
			"size":  size,
			"total": total,
		},
	})
}

// ListChannels 所有 channel 列表（带用户信息，支持按 user_id / type 过滤）
func (h *AdminHandler) ListChannels(c *gin.Context) {
	db := h.authSvc.DB()

	page, size := parsePagination(c)
	offset := (page - 1) * size

	query := db.Model(&models.Channel{})
	if t := c.Query("type"); t != "" {
		query = query.Where("type = ?", t)
	}
	if uid := c.Query("user_id"); uid != "" {
		query = query.
			Joins("INNER JOIN user_channels ON user_channels.channel_id = channels.id AND user_channels.deleted_at IS NULL").
			Where("user_channels.user_id = ?", uid)
	}

	var total int64
	query.Count(&total)

	var channels []models.Channel
	query.Order("channels.created_at DESC").Offset(offset).Limit(size).Find(&channels)

	// 附带 owner 信息
	type ChannelRow struct {
		models.Channel
		OwnerID    string `json:"owner_id"`
		OwnerEmail string `json:"owner_email"`
		OwnerName  string `json:"owner_name"`
	}
	rows := make([]ChannelRow, 0, len(channels))
	for _, ch := range channels {
		var uc models.UserChannel
		row := ChannelRow{Channel: ch}
		if err := db.Where("channel_id = ? AND deleted_at IS NULL", ch.ID).First(&uc).Error; err == nil {
			var u models.User
			if err2 := db.Where("id = ?", uc.UserID).First(&u).Error; err2 == nil {
				row.OwnerID = u.ID
				row.OwnerEmail = u.Email
				row.OwnerName = u.Name
			}
		}
		rows = append(rows, row)
	}

	c.JSON(http.StatusOK, gin.H{
		"channels": rows,
		"pagination": gin.H{
			"page":  page,
			"size":  size,
			"total": total,
		},
	})
}

// ToggleChannel 启用 / 禁用 channel
func (h *AdminHandler) ToggleChannel(c *gin.Context) {
	channelID := c.Param("id")
	db := h.authSvc.DB()

	var channel models.Channel
	if err := db.Where("id = ?", channelID).First(&channel).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Channel not found"})
		return
	}

	channel.IsActive = !channel.IsActive
	channel.UpdatedAt = time.Now()
	db.Save(&channel)

	c.JSON(http.StatusOK, gin.H{
		"id":        channel.ID,
		"is_active": channel.IsActive,
		"message":   map[bool]string{true: "Channel enabled", false: "Channel disabled"}[channel.IsActive],
	})
}

// DeleteChannel 强制删除 channel（管理员操作）
func (h *AdminHandler) DeleteChannel(c *gin.Context) {
	channelID := c.Param("id")
	db := h.authSvc.DB()

	db.Where("channel_id = ?", channelID).Delete(&models.UserChannel{})
	db.Where("id = ?", channelID).Delete(&models.Channel{})

	c.JSON(http.StatusOK, gin.H{"message": "Channel deleted"})
}

// ─── Middleware ───────────────────────────────────────────────────────────────

// AdminAuthMiddleware 校验 admin session cookie
func AdminAuthMiddleware(redis *services.RedisService, cfg *models.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		sessionID, err := c.Cookie(adminSessionCookie)
		if err != nil || sessionID == "" {
			if isAPIPath(c.Request.URL.Path) {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "Admin login required"})
			} else {
				c.Redirect(http.StatusTemporaryRedirect, "/admin/login")
			}
			c.Abort()
			return
		}

		email, err := redis.Get(fmt.Sprintf("admin:session:%s", sessionID))
		if err != nil || email != cfg.AdminEmail {
			if isAPIPath(c.Request.URL.Path) {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "Session expired or invalid"})
			} else {
				c.Redirect(http.StatusTemporaryRedirect, "/admin/login")
			}
			c.Abort()
			return
		}

		c.Set("admin_email", email)
		c.Next()
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func (h *AdminHandler) isAdminLoggedIn(c *gin.Context) bool {
	sessionID, err := c.Cookie(adminSessionCookie)
	if err != nil || sessionID == "" {
		return false
	}
	email, err := h.redis.Get(fmt.Sprintf("admin:session:%s", sessionID))
	return err == nil && email == h.config.AdminEmail
}

func parsePagination(c *gin.Context) (page, size int) {
	page = 1
	size = 20
	if p := c.Query("page"); p != "" {
		fmt.Sscanf(p, "%d", &page)
	}
	if s := c.Query("size"); s != "" {
		fmt.Sscanf(s, "%d", &size)
	}
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 20
	}
	return
}

func isAPIPath(path string) bool {
	return len(path) >= 11 && path[:11] == "/admin/api/"
}
