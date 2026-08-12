package models

import (
	"time"

	"gorm.io/gorm"
)

// Agent 已注册的 agent（注册平台核心实体）
//
// AgentID 由 agent 侧在"出生"时生成（agent-bridge identity.json：
// agentId = "acp_agent_" + sha256(pubkey) 前缀 hex），注册时原样上报，
// 服务端不生成、不修改，只做格式校验和唯一性约束。
//
// 归属关系：一个 agent 绑定一个 channel（多实例部署时即 hub 的设备通道），
// UserID 冗余自 channel owner，鉴权查询时避免 join。
type Agent struct {
	AgentID     string         `gorm:"primaryKey;type:varchar(64)"        json:"agent_id"`
	ChannelID   string         `gorm:"type:varchar(36);index;not null"    json:"channel_id"`
	UserID      string         `gorm:"type:varchar(36);index;not null"    json:"user_id"`
	Name        string         `gorm:"type:varchar(100)"                  json:"name"` // 公开时必填；私有可空
	Description string         `gorm:"type:text"                          json:"description"`
	AgentFP     string         `gorm:"type:varchar(32);index"             json:"agent_fp"`   // 公钥指纹（16 hex），Noise 配对用
	PathPrefix  string         `gorm:"type:varchar(128)"                  json:"path_prefix"` // hub 模式路由前缀，如 /p/<instanceId>/
	DeviceID    string         `gorm:"type:varchar(64)"                   json:"device_id"`   // 最近连接设备（legacy 握手模式）
	Capacity    int            `gorm:"default:5"                          json:"capacity"`    // 并发对话容量，默认 5，可改
	ActiveCount int            `gorm:"default:0"                          json:"active_count"`
	IsPublic    bool           `gorm:"default:false"                      json:"is_public"` // 默认不公开；公开后可被搜索
	LastSeenAt  time.Time      `json:"last_seen_at"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`
}

// AgentOnlineWindow LastSeenAt 距今在此窗口内视为在线（注册/握手/心跳都会刷新）
const AgentOnlineWindow = 5 * time.Minute

// Online 判断 agent 是否在线（基于 LastSeenAt 新鲜度）
func (a *Agent) Online() bool {
	return time.Since(a.LastSeenAt) < AgentOnlineWindow
}
