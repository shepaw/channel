package services

import (
	"fmt"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// RateLimitService 管理每个 channel 的限流规则
type RateLimitService struct {
	db    *DatabaseService
	redis *RedisService
}

func NewRateLimitService(db *DatabaseService, redis *RedisService) *RateLimitService {
	return &RateLimitService{db: db, redis: redis}
}

// CheckBandwidth 检查带宽限制（Mbps），返回 (允许, 剩余Mbps, error)
func (r *RateLimitService) CheckBandwidth(channelID, clientIP string, bytes int64) (bool, float64, error) {
	rule, err := r.getActiveRule(channelID, "bandwidth")
	if err != nil {
		return true, 0, nil // 无规则 = 不限速
	}

	window := time.Duration(rule.TimeWindow)
	windowStart := time.Now().Truncate(window)
	key := fmt.Sprintf("rl:%s:%s:bw:%s", channelID, clientIP, windowStart.Format(time.RFC3339))

	current, err := r.redis.IncrBy(key, bytes)
	if err != nil {
		return true, 0, err
	}
	if expiry := windowStart.Add(window).Sub(time.Now()); expiry > 0 {
		r.redis.Expire(key, expiry)
	}

	// bytes/window → Mbps
	mbps := float64(current) * 8 / (1024 * 1024) / window.Seconds()
	allowed := mbps <= rule.LimitValue
	remaining := rule.LimitValue - mbps
	if remaining < 0 {
		remaining = 0
	}
	return allowed, remaining, nil
}

// CheckConnections 检查并发连接数，返回 (允许, 剩余数, error)
func (r *RateLimitService) CheckConnections(channelID string) (bool, float64, error) {
	rule, err := r.getActiveRule(channelID, "connections")
	if err != nil {
		return true, 0, nil
	}

	key := fmt.Sprintf("rl:%s:conns", channelID)
	current, err := r.redis.IncrBy(key, 1)
	if err != nil {
		return true, 0, err
	}

	allowed := current <= int64(rule.LimitValue)
	remaining := rule.LimitValue - float64(current)
	if remaining < 0 {
		remaining = 0
	}
	return allowed, remaining, nil
}

// DecrementConnections 连接断开时递减计数
func (r *RateLimitService) DecrementConnections(channelID string) error {
	key := fmt.Sprintf("rl:%s:conns", channelID)
	_, err := r.redis.IncrBy(key, -1)
	return err
}

// CheckRequests 检查请求速率（rps），返回 (允许, 剩余rps, error)
func (r *RateLimitService) CheckRequests(channelID, clientIP string) (bool, float64, error) {
	rule, err := r.getActiveRule(channelID, "requests")
	if err != nil {
		return true, 0, nil
	}

	window := time.Duration(rule.TimeWindow)
	windowStart := time.Now().Truncate(window)
	key := fmt.Sprintf("rl:%s:%s:req:%s", channelID, clientIP, windowStart.Format(time.RFC3339))

	current, err := r.redis.Incr(key)
	if err != nil {
		return true, 0, err
	}
	if expiry := windowStart.Add(window).Sub(time.Now()); expiry > 0 {
		r.redis.Expire(key, expiry)
	}

	rps := float64(current) / window.Seconds()
	allowed := rps <= rule.LimitValue
	remaining := rule.LimitValue - rps
	if remaining < 0 {
		remaining = 0
	}
	return allowed, remaining, nil
}

// AddRule 添加限流规则
func (r *RateLimitService) AddRule(channelID, ruleType string, limitValue float64, timeWindow time.Duration) error {
	rule := &models.RateLimitRule{
		ID:         uuid.New().String(),
		ChannelID:  channelID,
		RuleType:   ruleType,
		LimitValue: limitValue,
		TimeWindow: int64(timeWindow),
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	return r.db.DB.Create(rule).Error
}

// GetRules 获取 channel 的所有限流规则
func (r *RateLimitService) GetRules(channelID string) ([]*models.RateLimitRule, error) {
	var rules []*models.RateLimitRule
	err := r.db.DB.Where("channel_id = ? AND deleted_at IS NULL", channelID).Find(&rules).Error
	return rules, err
}

// DeleteRule 删除限流规则
func (r *RateLimitService) DeleteRule(ruleID string) error {
	return r.db.DB.Delete(&models.RateLimitRule{}, "id = ?", ruleID).Error
}

// getActiveRule 获取指定类型的最新有效规则
func (r *RateLimitService) getActiveRule(channelID, ruleType string) (*models.RateLimitRule, error) {
	var rule models.RateLimitRule
	err := r.db.DB.
		Where("channel_id = ? AND rule_type = ? AND deleted_at IS NULL", channelID, ruleType).
		Order("created_at DESC").
		First(&rule).Error
	if err == gorm.ErrRecordNotFound {
		return nil, fmt.Errorf("no rule")
	}
	return &rule, err
}
