package handlers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"github.com/edenzou/channel-service/pkg/internal/services"
	"github.com/gin-gonic/gin"
)

type AuthHandler struct {
	authSvc *services.AuthService
	config  *models.Config
}

func NewAuthHandler(authSvc *services.AuthService, config *models.Config) *AuthHandler {
	return &AuthHandler{
		authSvc: authSvc,
		config:  config,
	}
}

type LoginRequest struct {
	Provider   string `json:"provider" binding:"required,oneof=wechat google"`
	ProviderID string `json:"provider_id" binding:"required"`
	Email      string `json:"email" binding:"required,email"`
	Name       string `json:"name"`
	Avatar     string `json:"avatar"`
}

type TokenRequest struct {
	TTL string `json:"ttl"` // 如 "15m", "1h", "24h"
}

type TokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (h *AuthHandler) Login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user, err := h.authSvc.CreateOrGetUser(req.Provider, req.ProviderID, req.Email, req.Name, req.Avatar)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create/get user"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"id":      user.ID,
		"email":   user.Email,
		"name":    user.Name,
		"avatar":  user.Avatar,
		"provider": user.Provider,
	})
}

// maxTokenTTL 用户可申请的 token 最大有效期，超出则拒绝
const maxTokenTTL = 90 * 24 * time.Hour

func (h *AuthHandler) GenerateToken(c *gin.Context) {
	var req TokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
		return
	}

	// 解析TTL
	ttl := h.config.TokenTTL
	if req.TTL != "" {
		parsedTTL, err := time.ParseDuration(req.TTL)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid TTL format"})
			return
		}
		if parsedTTL <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "TTL must be positive"})
			return
		}
		if parsedTTL > maxTokenTTL {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("TTL too large, maximum is %s", maxTokenTTL)})
			return
		}
		ttl = parsedTTL
	}

	// 生成token
	accessToken, err := h.authSvc.GenerateAccessToken(userID, ttl)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}

	c.JSON(http.StatusOK, TokenResponse{
		Token:     accessToken.Token,
		ExpiresAt: accessToken.ExpiresAt,
	})
}

func (h *AuthHandler) RevokeToken(c *gin.Context) {
	token := c.GetHeader("Authorization")
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Authorization header required"})
		return
	}

	// 去掉Bearer前缀
	if len(token) > 7 && token[:7] == "Bearer " {
		token = token[7:]
	}

	err := h.authSvc.RevokeToken(token)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to revoke token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Token revoked successfully"})
}