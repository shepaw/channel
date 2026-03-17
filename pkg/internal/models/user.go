package models

import (
	"time"

	"gorm.io/gorm"
)

// User 用户表
type User struct {
	ID           string         `gorm:"primaryKey;type:varchar(36)" json:"id"`
	Email        string         `gorm:"uniqueIndex;not null"        json:"email"`
	Name         string         `json:"name"`
	Avatar       string         `json:"avatar"`
	Provider     string         `json:"provider"`    // wechat | google | email
	ProviderID   string         `json:"provider_id"` // 第三方平台 openid/sub；email 注册时等同于 email
	PasswordHash string         `gorm:"type:varchar(255)" json:"-"` // email 注册时存 bcrypt hash
	EmailVerified bool          `gorm:"default:false" json:"email_verified"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	DeletedAt    gorm.DeletedAt `gorm:"index" json:"-"`
}

// EmailVerification 邮箱验证码表（注册 & 找回密码通用）
type EmailVerification struct {
	ID        string         `gorm:"primaryKey;type:varchar(36)" json:"id"`
	Email     string         `gorm:"type:varchar(255);index;not null" json:"email"`
	Code      string         `gorm:"type:varchar(8);not null" json:"code"`
	Purpose   string         `gorm:"type:varchar(32);not null" json:"purpose"` // register | login
	Attempts  int            `gorm:"default:0" json:"-"`                       // 错误次数，超过上限自动失效
	ExpiresAt time.Time      `json:"expires_at"`
	UsedAt    *time.Time     `json:"used_at,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

// AccessToken 临时访问令牌
type AccessToken struct {
	ID        string         `gorm:"primaryKey;type:varchar(36)" json:"id"`
	UserID    string         `gorm:"type:varchar(36);index"      json:"user_id"`
	Token     string         `gorm:"uniqueIndex;not null"        json:"token"`
	ExpiresAt time.Time      `json:"expires_at"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

// UserChannel 用户与 channel 的多对多关联
type UserChannel struct {
	ID        string         `gorm:"primaryKey;type:varchar(36)" json:"id"`
	UserID    string         `gorm:"type:varchar(36);index"      json:"user_id"`
	ChannelID string         `gorm:"type:varchar(36);index"      json:"channel_id"`
	CreatedAt time.Time      `json:"created_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}
