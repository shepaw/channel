package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// JSONMap 兼容 SQLite / PostgreSQL 的 JSON 字段类型
type JSONMap map[string]interface{}

func (j JSONMap) Value() (driver.Value, error) {
	if j == nil {
		return nil, nil
	}
	b, err := json.Marshal(j)
	return string(b), err
}

func (j *JSONMap) Scan(value interface{}) error {
	if value == nil {
		*j = nil
		return nil
	}
	var raw []byte
	switch v := value.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return fmt.Errorf("JSONMap: cannot scan type %T", value)
	}
	return json.Unmarshal(raw, j)
}

// ChannelStats 流量统计
type ChannelStats struct {
	TotalBytes        int64 `json:"total_bytes"`
	TotalRequests     int64 `json:"total_requests"`
	ActiveConnections int   `json:"active_connections"`
	TodayBytes        int64 `json:"today_bytes"`
	TodayRequests     int64 `json:"today_requests"`
}

func (cs ChannelStats) Value() (driver.Value, error) {
	b, err := json.Marshal(cs)
	return string(b), err
}

func (cs *ChannelStats) Scan(value interface{}) error {
	if value == nil {
		return nil
	}
	var raw []byte
	switch v := value.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return fmt.Errorf("ChannelStats: cannot scan type %T", value)
	}
	return json.Unmarshal(raw, cs)
}

// Channel 代理通道
type Channel struct {
	ID          string         `gorm:"primaryKey;type:varchar(36)"  json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Type        string         `json:"type"`     // http | https | ws | tcp | udp | tunnel-http | tunnel-tcp | tunnel-ws
	Target      string         `json:"target"`   // 目标地址，如 http://127.0.0.1:3000
	Endpoint    string         `gorm:"uniqueIndex;not null" json:"endpoint"` // 分配的公开地址
	Secret      string         `gorm:"type:varchar(64);index" json:"secret,omitempty"` // tunnel channel 专用密钥
	IsActive    bool           `json:"is_active"`
	Config      JSONMap        `gorm:"type:text"    json:"config"`
	Stats       ChannelStats   `gorm:"type:text"    json:"stats"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`
}

// RateLimitRule 单个通道的限流规则
type RateLimitRule struct {
	ID         string         `gorm:"primaryKey;type:varchar(36)" json:"id"`
	ChannelID  string         `gorm:"type:varchar(36);index"      json:"channel_id"`
	RuleType   string         `json:"rule_type"`   // bandwidth | connections | requests
	LimitValue float64        `json:"limit_value"` // Mbps / 并发数 / rps
	TimeWindow int64          `json:"time_window"` // nanoseconds（time.Duration 存 int64）
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	DeletedAt  gorm.DeletedAt `gorm:"index" json:"-"`
}
