package handlers

import (
	"net/http"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"github.com/edenzou/channel-service/pkg/internal/services"
	"github.com/gin-gonic/gin"
)

// EmailAuthHandler 处理邮箱注册/登录相关接口
type EmailAuthHandler struct {
	authSvc  *services.AuthService
	emailSvc *services.EmailService
	config   *models.Config
}

func NewEmailAuthHandler(authSvc *services.AuthService, emailSvc *services.EmailService, config *models.Config) *EmailAuthHandler {
	return &EmailAuthHandler{authSvc: authSvc, emailSvc: emailSvc, config: config}
}

// ─── 请求/响应结构 ───────────────────────────────────────────────────────────

type SendCodeRequest struct {
	Email   string `json:"email" binding:"required,email"`
	Purpose string `json:"purpose" binding:"required,oneof=register login"`
}

type RegisterRequest struct {
	Email    string `json:"email"    binding:"required,email"`
	Password string `json:"password" binding:"required,min=8"`
	Name     string `json:"name"`
	Code     string `json:"code"     binding:"required,len=6"`
}

type EmailPasswordLoginRequest struct {
	Email    string `json:"email"    binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

type EmailCodeLoginRequest struct {
	Email string `json:"email" binding:"required,email"`
	Code  string `json:"code"  binding:"required,len=6"`
}

// ─── 发送验证码 ──────────────────────────────────────────────────────────────

// SendCode godoc
//
//	POST /api/v1/auth/email/send-code
//	Body: { "email": "foo@example.com", "purpose": "register"|"login" }
func (h *EmailAuthHandler) SendCode(c *gin.Context) {
	var req SendCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	code, err := h.authSvc.SendEmailCode(req.Email, req.Purpose)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate code"})
		return
	}

	if err := h.emailSvc.SendVerificationCode(req.Email, code, req.Purpose); err != nil {
		// 发送失败不影响已入库的验证码，记录日志后返回错误
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Failed to send email: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Verification code sent"})
}

// ─── 邮箱注册 ────────────────────────────────────────────────────────────────

// Register godoc
//
//	POST /api/v1/auth/email/register
//	Body: { "email", "password", "name"(optional), "code" }
func (h *EmailAuthHandler) Register(c *gin.Context) {
	var req RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user, err := h.authSvc.RegisterWithEmail(req.Email, req.Password, req.Name, req.Code)
	if err != nil {
		status, msg := mapEmailAuthError(err)
		c.JSON(status, gin.H{"error": msg})
		return
	}

	token, err := h.authSvc.GenerateAccessToken(user.ID, h.config.TokenTTL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"user":  userResponse(user),
		"token": token.Token,
		"expires_at": token.ExpiresAt,
	})
}

// ─── 邮箱登录（密码） ─────────────────────────────────────────────────────────

// LoginPassword godoc
//
//	POST /api/v1/auth/email/login/password
//	Body: { "email", "password" }
func (h *EmailAuthHandler) LoginPassword(c *gin.Context) {
	var req EmailPasswordLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user, err := h.authSvc.LoginWithEmailPassword(req.Email, req.Password)
	if err != nil {
		status, msg := mapEmailAuthError(err)
		c.JSON(status, gin.H{"error": msg})
		return
	}

	token, err := h.authSvc.GenerateAccessToken(user.ID, h.config.TokenTTL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"user":       userResponse(user),
		"token":      token.Token,
		"expires_at": token.ExpiresAt,
	})
}

// ─── 邮箱登录（验证码，无密码） ───────────────────────────────────────────────

// LoginCode godoc
//
//	POST /api/v1/auth/email/login/code
//	Body: { "email", "code" }
func (h *EmailAuthHandler) LoginCode(c *gin.Context) {
	var req EmailCodeLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user, err := h.authSvc.LoginWithEmailCode(req.Email, req.Code)
	if err != nil {
		status, msg := mapEmailAuthError(err)
		c.JSON(status, gin.H{"error": msg})
		return
	}

	token, err := h.authSvc.GenerateAccessToken(user.ID, h.config.TokenTTL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"user":       userResponse(user),
		"token":      token.Token,
		"expires_at": token.ExpiresAt,
	})
}

// ─── helpers ────────────────────────────────────────────────────────────────

func userResponse(u *models.User) gin.H {
	return gin.H{
		"id":             u.ID,
		"email":          u.Email,
		"name":           u.Name,
		"avatar":         u.Avatar,
		"provider":       u.Provider,
		"email_verified": u.EmailVerified,
	}
}

func mapEmailAuthError(err error) (int, string) {
	switch err {
	case services.ErrEmailAlreadyExists:
		return http.StatusConflict, "Email already registered"
	case services.ErrInvalidCredentials:
		return http.StatusUnauthorized, "Invalid email or password"
	case services.ErrInvalidCode:
		return http.StatusUnprocessableEntity, "Invalid or expired verification code"
	case services.ErrCodeAlreadyUsed:
		return http.StatusUnprocessableEntity, "Verification code already used"
	default:
		return http.StatusInternalServerError, "Internal server error"
	}
}
