package models

import (
	"time"

	"gorm.io/gorm"
)

// MailboxDirection 收件箱条目方向
type MailboxDirection string

const (
	MailboxDirectionInbound MailboxDirection = "inbound" // caller → agent/group 留言
	MailboxDirectionReply   MailboxDirection = "reply"   // agent/group → caller 回复
)

// MailboxStatus 投递状态（at-least-once：in_flight 超时后重新变 pending）
type MailboxStatus string

const (
	MailboxStatusPending  MailboxStatus = "pending"
	MailboxStatusInFlight MailboxStatus = "in_flight"
)

// MailboxTargetType 路由目标：单个 agent 或群会话
type MailboxTargetType string

const (
	MailboxTargetAgent MailboxTargetType = "agent"
	MailboxTargetGroup MailboxTargetType = "group"
)

// MailboxKind 投递内容类型（可扩展：chat → article → …）
type MailboxKind string

const (
	MailboxKindChat    MailboxKind = "chat"
	MailboxKindStream  MailboxKind = "stream" // 流式增量片段（打字机）
	MailboxKindArticle MailboxKind = "article"
)

// MailboxMessage 云端收件箱中的一条密文消息。
//
// Channel 定位为 shepaw ↔ agent-bridge 之间的云端信箱，不是代理身份：
// 只存密文与路由元数据，看不到明文（E2E seal-box）。
//
// 路由键 TargetID 为 agent_id 或 group_id；GroupID 可选，表示群上下文；
// RequestID 用于异步回复与 inflight turn 串联；SessionID 标识会话；
// MessageID / ReplyTo 关联一次对话往返。
type MailboxMessage struct {
	ID          string             `gorm:"primaryKey;type:varchar(36)" json:"id"`
	TargetType  MailboxTargetType  `gorm:"type:varchar(16);index:idx_mailbox_target_dir_status,priority:1;not null;default:agent" json:"target_type"`
	TargetID    string             `gorm:"type:varchar(64);index:idx_mailbox_target_dir_status,priority:2;not null" json:"target_id"`
	// AgentID 兼容旧列名；新写入与 TargetID 保持一致
	AgentID     string             `gorm:"type:varchar(64);index:idx_mailbox_agent_dir_status,priority:1;not null" json:"agent_id"`
	Direction   MailboxDirection   `gorm:"type:varchar(16);index:idx_mailbox_target_dir_status,priority:3;index:idx_mailbox_agent_dir_status,priority:2;not null" json:"direction"`
	Status      MailboxStatus      `gorm:"type:varchar(16);index:idx_mailbox_target_dir_status,priority:4;index:idx_mailbox_agent_dir_status,priority:3;not null" json:"status"`
	Kind        MailboxKind        `gorm:"type:varchar(16);index;not null;default:chat" json:"kind"`
	CallerFP    string             `gorm:"type:varchar(32);index;not null" json:"caller_fp"` // 16-hex 公钥指纹
	MessageID   string             `gorm:"type:varchar(64);index;not null" json:"message_id"`
	RequestID   string             `gorm:"type:varchar(64);index" json:"request_id,omitempty"` // 异步 turn / inflight 关联
	SessionID   string             `gorm:"type:varchar(64);index" json:"session_id"`
	GroupID     string             `gorm:"type:varchar(64);index" json:"group_id,omitempty"` // 群上下文（target 为 agent 时也可填）
	ReplyTo     string             `gorm:"type:varchar(64);index" json:"reply_to,omitempty"`
	Ciphertext  string             `gorm:"type:text;not null" json:"ciphertext"`
	SizeBytes   int                `gorm:"not null" json:"size_bytes"`
	ClaimedAt   *time.Time         `json:"claimed_at,omitempty"`
	ExpiresAt   time.Time          `gorm:"index" json:"expires_at"`
	CreatedAt   time.Time          `json:"created_at"`
	UpdatedAt   time.Time          `json:"updated_at"`
	DeletedAt   gorm.DeletedAt     `gorm:"index" json:"-"`
}

// 收件箱配额与超时（服务端常量；后续可进 config）
const (
	MailboxMaxPerTarget      = 500
	MailboxMaxCipherBytes    = 32 * 1024
	MailboxTTL               = 7 * 24 * time.Hour
	MailboxVisibilityTimeout = 5 * time.Minute
)

// Legacy alias
const MailboxMaxPerAgent = MailboxMaxPerTarget
