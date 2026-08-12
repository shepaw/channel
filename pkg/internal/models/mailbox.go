package models

import (
	"time"

	"gorm.io/gorm"
)

// MailboxDirection 信箱条目方向
type MailboxDirection string

const (
	MailboxDirectionInbound  MailboxDirection = "inbound"  // caller → agent 留言
	MailboxDirectionReply    MailboxDirection = "reply"    // agent → caller 回复
)

// MailboxStatus 投递状态（at-least-once：in_flight 超时后重新变 pending）
type MailboxStatus string

const (
	MailboxStatusPending  MailboxStatus = "pending"
	MailboxStatusInFlight MailboxStatus = "in_flight"
)

// MailboxMessage channel 双向信箱中的一条密文消息。
// Channel 只存密文与路由元数据，看不到明文（E2E seal-box）。
type MailboxMessage struct {
	ID          string           `gorm:"primaryKey;type:varchar(36)" json:"id"`
	AgentID     string           `gorm:"type:varchar(64);index:idx_mailbox_agent_dir_status,priority:1;not null" json:"agent_id"`
	Direction   MailboxDirection `gorm:"type:varchar(16);index:idx_mailbox_agent_dir_status,priority:2;not null" json:"direction"`
	Status      MailboxStatus    `gorm:"type:varchar(16);index:idx_mailbox_agent_dir_status,priority:3;not null" json:"status"`
	CallerFP    string           `gorm:"type:varchar(32);index;not null" json:"caller_fp"` // 16-hex 公钥指纹
	MessageID   string           `gorm:"type:varchar(64);index;not null" json:"message_id"` // 客户端生成，回复用 reply_to 关联
	SessionID   string           `gorm:"type:varchar(64)" json:"session_id"`
	ReplyTo     string           `gorm:"type:varchar(64);index" json:"reply_to,omitempty"` // 仅 reply 方向：对应 inbound 的 message_id
	Ciphertext  string           `gorm:"type:text;not null" json:"ciphertext"`                 // base64 seal-box
	SizeBytes   int              `gorm:"not null" json:"size_bytes"`
	ClaimedAt   *time.Time       `json:"claimed_at,omitempty"` // in_flight 起始时间
	ExpiresAt   time.Time        `gorm:"index" json:"expires_at"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
	DeletedAt   gorm.DeletedAt   `gorm:"index" json:"-"`
}

// 信箱配额与超时（服务端常量；后续可进 config）
const (
	MailboxMaxPerAgent     = 500
	MailboxMaxCipherBytes  = 32 * 1024 // 单条密文上限
	MailboxTTL             = 7 * 24 * time.Hour
	MailboxVisibilityTimeout = 5 * time.Minute
)
