package models

import "time"

// Config 服务全局配置
type Config struct {
	ServerPort int    `json:"server_port"`
	BaseURL    string `json:"base_url"`    // e.g. https://channel.example.com
	BaseDomain string `json:"base_domain"` // e.g. channel.example.com

	// 数据库
	DatabaseURL string `json:"database_url"`

	// Redis
	RedisAddr     string `json:"redis_addr"`
	RedisPassword string `json:"redis_password"`
	RedisDB       int    `json:"redis_db"`

	// 认证
	TokenTTL    time.Duration `json:"token_ttl"`
	MaxChannels int           `json:"max_channels"`

	// OAuth - 微信开放平台
	WechatAppID     string `json:"wechat_app_id"`
	WechatAppSecret string `json:"wechat_app_secret"`

	// OAuth - Google
	GoogleClientID     string `json:"google_client_id"`
	GoogleClientSecret string `json:"google_client_secret"`

	// SMTP 邮件（用于邮箱注册/登录验证码）
	SMTPHost     string `json:"smtp_host"`
	SMTPPort     int    `json:"smtp_port"`
	SMTPUsername string `json:"smtp_username"`
	SMTPPassword string `json:"smtp_password"`
	SMTPFrom     string `json:"smtp_from"` // 发件人地址，如 noreply@example.com

	// 管理后台
	AdminEmail string `json:"admin_email"` // 允许登录管理后台的 Google 账号邮箱

	// 安全
	// AllowedOrigins 控制 CORS 允许的来源，例如 ["https://app.example.com"]
	// 留空表示不允许任何跨域请求；设为 ["*"] 表示允许所有来源（不建议生产环境使用）
	AllowedOrigins []string `json:"allowed_origins"`

	// TCP/UDP 端口范围
	TCPPortRangeStart int `json:"tcp_port_range_start"` // 如 10000
	TCPPortRangeEnd   int `json:"tcp_port_range_end"`   // 如 20000
}

// RateLimitConfig 限流配置（嵌入在 Channel 中使用）
type RateLimitConfig struct {
	BandwidthMBps  float64 `json:"bandwidth_mbps"`
	MaxConnections int     `json:"max_connections"`
	RequestsPerSec int     `json:"requests_per_sec"`
}

// DefaultConfig 默认配置，使用 SQLite 方便本地开发
var DefaultConfig = Config{
	ServerPort:        8080,
	BaseURL:           "http://localhost:8080",
	BaseDomain:        "localhost:8080",
	DatabaseURL:       "sqlite:./channel.db",
	RedisAddr:         "localhost:6379",
	RedisPassword:     "",
	RedisDB:           0,
	TokenTTL:          15 * time.Minute,
	MaxChannels:       5,
	TCPPortRangeStart: 10000,
	TCPPortRangeEnd:   20000,
	SMTPPort:          587,
}
