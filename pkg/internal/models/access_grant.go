package models

import (
	"time"

	"gorm.io/gorm"
)

// AccessGrantStatus 接入申请状态
type AccessGrantStatus string

const (
	AccessPending  AccessGrantStatus = "pending"
	AccessApproved AccessGrantStatus = "approved"
	AccessRejected AccessGrantStatus = "rejected"
	AccessRevoked  AccessGrantStatus = "revoked"
)

// AgentAccessGrant 公开发现后的接入申请。
//
// channel 只做中介：存申请、owner 审批、把结果暴露给 agent 拉取。
// 真正的连接鉴权仍在 agent 侧 authorized_peers.json（Noise 白名单）。
type AgentAccessGrant struct {
	ID           string            `gorm:"primaryKey;type:varchar(36)" json:"id"`
	AgentID      string            `gorm:"type:varchar(64);uniqueIndex:uidx_agent_caller,priority:1;index;not null" json:"agent_id"`
	CallerFP     string            `gorm:"type:varchar(32);uniqueIndex:uidx_agent_caller,priority:2;index;not null" json:"caller_fp"` // 16-hex
	CallerPubKey string            `gorm:"type:varchar(64);not null" json:"caller_pubkey"`                                          // base64 raw 32-byte X25519
	CallerName   string            `gorm:"type:varchar(100)" json:"caller_name"`
	Message      string            `gorm:"type:text" json:"message"`
	Status       AccessGrantStatus `gorm:"type:varchar(16);index;not null" json:"status"`
	DecidedAt    *time.Time        `json:"decided_at,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	DeletedAt    gorm.DeletedAt    `gorm:"index" json:"-"`
}
