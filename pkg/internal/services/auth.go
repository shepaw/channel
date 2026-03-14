package services

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

type AuthService struct {
	db    *DatabaseService
	redis *RedisService
}

func NewAuthService(db *DatabaseService, redis *RedisService) *AuthService {
	return &AuthService{db: db, redis: redis}
}

var (
	ErrUserNotFound        = errors.New("user not found")
	ErrInvalidToken        = errors.New("invalid token")
	ErrTokenExpired        = errors.New("token expired")
	ErrEmailAlreadyExists  = errors.New("email already registered")
	ErrInvalidCredentials  = errors.New("invalid email or password")
	ErrInvalidCode         = errors.New("invalid or expired verification code")
	ErrCodeAlreadyUsed     = errors.New("verification code already used")
)

func (s *AuthService) CreateOrGetUser(provider, providerID, email, name, avatar string) (*models.User, error) {
	var user models.User
	err := s.db.DB.Where("provider = ? AND provider_id = ?", provider, providerID).First(&user).Error

	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}

	if err == gorm.ErrRecordNotFound {
		user = models.User{
			ID:         uuid.New().String(),
			Email:      email,
			Name:       name,
			Avatar:     avatar,
			Provider:   provider,
			ProviderID: providerID,
			CreatedAt:  time.Now(),
			UpdatedAt:  time.Now(),
		}
		if err := s.db.DB.Create(&user).Error; err != nil {
			return nil, err
		}
	} else {
		user.Name = name
		user.Avatar = avatar
		user.UpdatedAt = time.Now()
		s.db.DB.Save(&user)
	}

	return &user, nil
}

func (s *AuthService) GenerateAccessToken(userID string, ttl time.Duration) (*models.AccessToken, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}

	token := base64.URLEncoding.EncodeToString(b)
	at := &models.AccessToken{
		ID:        uuid.New().String(),
		UserID:    userID,
		Token:     token,
		ExpiresAt: time.Now().Add(ttl),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	if err := s.db.DB.Create(at).Error; err != nil {
		return nil, err
	}

	key := fmt.Sprintf("token:%s", token)
	s.redis.Set(key, userID, ttl)

	return at, nil
}

func (s *AuthService) ValidateToken(tokenString string) (*models.User, error) {
	key := fmt.Sprintf("token:%s", tokenString)
	userID, err := s.redis.Get(key)
	if err != nil {
		var at models.AccessToken
		if err := s.db.DB.Where("token = ? AND deleted_at IS NULL", tokenString).First(&at).Error; err != nil {
			return nil, ErrInvalidToken
		}
		if at.ExpiresAt.Before(time.Now()) {
			return nil, ErrTokenExpired
		}
		ttl := time.Until(at.ExpiresAt)
		if ttl > 0 {
			s.redis.Set(key, at.UserID, ttl)
		}
		userID = at.UserID
	}

	var user models.User
	if err := s.db.DB.Where("id = ?", userID).First(&user).Error; err != nil {
		return nil, ErrUserNotFound
	}

	return &user, nil
}

func (s *AuthService) RevokeToken(tokenString string) error {
	if err := s.db.DB.Where("token = ?", tokenString).Delete(&models.AccessToken{}).Error; err != nil {
		return err
	}
	key := fmt.Sprintf("token:%s", tokenString)
	return s.redis.Delete(key)
}

// ─── 邮箱注册 / 登录 ────────────────────────────────────────────────────────

// SendEmailCode 生成并保存 6 位验证码，返回 code（由 EmailService 发送）
func (s *AuthService) SendEmailCode(email, purpose string) (string, error) {
	code, err := generateNumericCode(6)
	if err != nil {
		return "", err
	}

	// 软删除该邮箱同目的的旧验证码，避免堆积
	s.db.DB.Where("email = ? AND purpose = ? AND deleted_at IS NULL", email, purpose).
		Delete(&models.EmailVerification{})

	ev := &models.EmailVerification{
		ID:        uuid.New().String(),
		Email:     email,
		Code:      code,
		Purpose:   purpose,
		ExpiresAt: time.Now().Add(10 * time.Minute),
		CreatedAt: time.Now(),
	}
	if err := s.db.DB.Create(ev).Error; err != nil {
		return "", err
	}
	return code, nil
}

// RegisterWithEmail 用邮箱+密码注册（需先验证邮箱验证码）
func (s *AuthService) RegisterWithEmail(email, password, name, code string) (*models.User, error) {
	// 校验验证码
	if err := s.consumeEmailCode(email, code, "register"); err != nil {
		return nil, err
	}

	// 检查邮箱是否已被注册
	var count int64
	s.db.DB.Model(&models.User{}).Where("email = ? AND deleted_at IS NULL", email).Count(&count)
	if count > 0 {
		return nil, ErrEmailAlreadyExists
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	if name == "" {
		// 默认取邮箱 @ 前缀
		for i, c := range email {
			if c == '@' {
				name = email[:i]
				break
			}
		}
	}

	user := &models.User{
		ID:            uuid.New().String(),
		Email:         email,
		Name:          name,
		Provider:      "email",
		ProviderID:    email,
		PasswordHash:  string(hash),
		EmailVerified: true,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if err := s.db.DB.Create(user).Error; err != nil {
		return nil, err
	}
	return user, nil
}

// LoginWithEmailPassword 邮箱+密码登录
func (s *AuthService) LoginWithEmailPassword(email, password string) (*models.User, error) {
	var user models.User
	if err := s.db.DB.Where("email = ? AND provider = 'email' AND deleted_at IS NULL", email).
		First(&user).Error; err != nil {
		return nil, ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}
	return &user, nil
}

// LoginWithEmailCode 邮箱+验证码登录（无密码）
func (s *AuthService) LoginWithEmailCode(email, code string) (*models.User, error) {
	if err := s.consumeEmailCode(email, code, "login"); err != nil {
		return nil, err
	}

	var user models.User
	err := s.db.DB.Where("email = ? AND deleted_at IS NULL", email).First(&user).Error
	if err == gorm.ErrRecordNotFound {
		// 不存在则自动创建（免密账号）
		name := email
		for i, c := range email {
			if c == '@' {
				name = email[:i]
				break
			}
		}
		user = models.User{
			ID:            uuid.New().String(),
			Email:         email,
			Name:          name,
			Provider:      "email",
			ProviderID:    email,
			EmailVerified: true,
			CreatedAt:     time.Now(),
			UpdatedAt:     time.Now(),
		}
		if err := s.db.DB.Create(&user).Error; err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return &user, nil
}

// consumeEmailCode 校验并标记验证码为已使用
func (s *AuthService) consumeEmailCode(email, code, purpose string) error {
	var ev models.EmailVerification
	err := s.db.DB.Where(
		"email = ? AND code = ? AND purpose = ? AND deleted_at IS NULL",
		email, code, purpose,
	).First(&ev).Error
	if err != nil {
		return ErrInvalidCode
	}
	if ev.UsedAt != nil {
		return ErrCodeAlreadyUsed
	}
	if ev.ExpiresAt.Before(time.Now()) {
		return ErrInvalidCode
	}
	now := time.Now()
	s.db.DB.Model(&ev).Update("used_at", &now)
	return nil
}

// generateNumericCode 生成 n 位纯数字验证码
func generateNumericCode(n int) (string, error) {
	digits := make([]byte, n)
	for i := range digits {
		v, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		digits[i] = byte('0') + byte(v.Int64())
	}
	return string(digits), nil
}