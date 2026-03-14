package services

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type ChannelService struct {
	db     *DatabaseService
	redis  *RedisService
	config *models.Config
}

func NewChannelService(db *DatabaseService, redis *RedisService, config *models.Config) *ChannelService {
	return &ChannelService{
		db:     db,
		redis:  redis,
		config: config,
	}
}

var (
	ErrMaxChannelsExceeded = errors.New("maximum number of channels exceeded")
	ErrInvalidChannelType  = errors.New("invalid channel type")
	ErrChannelNotFound     = errors.New("channel not found")
	ErrNotChannelOwner     = errors.New("user does not own this channel")
)

func (c *ChannelService) CreateChannel(userID, name, description, channelType, target string, config map[string]interface{}) (*models.Channel, error) {
	var count int64
	if err := c.db.DB.Model(&models.UserChannel{}).
		Where("user_id = ? AND deleted_at IS NULL", userID).
		Count(&count).Error; err != nil {
		return nil, err
	}

	if count >= int64(c.config.MaxChannels) {
		return nil, ErrMaxChannelsExceeded
	}

	if !isValidChannelType(channelType) {
		return nil, ErrInvalidChannelType
	}

	channelID := uuid.New().String()
	endpoint := generateEndpointSimple(channelType, c.config.BaseDomain, channelID)

	// tunnel 类型自动生成永久密钥
	var secret string
	if strings.HasPrefix(channelType, "tunnel-") {
		secret = generateChannelSecret()
	}

	channel := &models.Channel{
		ID:          channelID,
		Name:        name,
		Description: description,
		Type:        channelType,
		Target:      target,
		Endpoint:    endpoint,
		Secret:      secret,
		IsActive:    true,
		Config:      config,
		Stats:       models.ChannelStats{},
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	tx := c.db.DB.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	if err := tx.Create(channel).Error; err != nil {
		tx.Rollback()
		return nil, err
	}

	userChannel := &models.UserChannel{
		ID:        uuid.New().String(),
		UserID:    userID,
		ChannelID: channel.ID,
		CreatedAt: time.Now(),
	}

	if err := tx.Create(userChannel).Error; err != nil {
		tx.Rollback()
		return nil, err
	}

	if err := tx.Commit().Error; err != nil {
		return nil, err
	}

	cacheKey := fmt.Sprintf("channel:%s", channel.ID)
	c.redis.Set(cacheKey, channel, 24*time.Hour)

	return channel, nil
}

func (c *ChannelService) GetChannelByID(channelID string) (*models.Channel, error) {
	cacheKey := fmt.Sprintf("channel:%s", channelID)
	var channel models.Channel
	if err := c.redis.GetStruct(cacheKey, &channel); err == nil {
		return &channel, nil
	}

	if err := c.db.DB.Where("id = ?", channelID).First(&channel).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrChannelNotFound
		}
		return nil, err
	}

	c.redis.Set(cacheKey, channel, 24*time.Hour)
	return &channel, nil
}

func (c *ChannelService) GetUserChannels(userID string) ([]*models.Channel, error) {
	var channels []*models.Channel
	err := c.db.DB.
		Joins("INNER JOIN user_channels ON user_channels.channel_id = channels.id").
		Where("user_channels.user_id = ? AND user_channels.deleted_at IS NULL AND channels.deleted_at IS NULL", userID).
		Find(&channels).Error
	return channels, err
}

func (c *ChannelService) UpdateChannel(userID, channelID string, updates map[string]interface{}) (*models.Channel, error) {
	var userChannel models.UserChannel
	err := c.db.DB.Where("user_id = ? AND channel_id = ? AND deleted_at IS NULL", userID, channelID).
		First(&userChannel).Error
	if err != nil {
		return nil, ErrNotChannelOwner
	}

	var channel models.Channel
	if err = c.db.DB.Where("id = ?", channelID).First(&channel).Error; err != nil {
		return nil, err
	}

	if name, ok := updates["name"].(string); ok {
		channel.Name = name
	}
	if desc, ok := updates["description"].(string); ok {
		channel.Description = desc
	}
	if isActive, ok := updates["is_active"].(bool); ok {
		channel.IsActive = isActive
	}
	if cfg, ok := updates["config"].(map[string]interface{}); ok {
		channel.Config = cfg
	}

	channel.UpdatedAt = time.Now()

	if err := c.db.DB.Save(&channel).Error; err != nil {
		return nil, err
	}

	cacheKey := fmt.Sprintf("channel:%s", channelID)
	c.redis.Set(cacheKey, channel, 24*time.Hour)
	return &channel, nil
}

func (c *ChannelService) DeleteChannel(userID, channelID string) error {
	var userChannel models.UserChannel
	err := c.db.DB.Where("user_id = ? AND channel_id = ? AND deleted_at IS NULL", userID, channelID).
		First(&userChannel).Error
	if err != nil {
		return ErrNotChannelOwner
	}

	if err := c.db.DB.Model(&models.UserChannel{}).
		Where("id = ?", userChannel.ID).
		Delete(&models.UserChannel{}).Error; err != nil {
		return err
	}

	// 软删除 channel 本身
	if err := c.db.DB.Model(&models.Channel{}).
		Where("id = ?", channelID).
		Delete(&models.Channel{}).Error; err != nil {
		return err
	}

	cacheKey := fmt.Sprintf("channel:%s", channelID)
	c.redis.Delete(cacheKey)
	return nil
}

// IsOwner 判断 user 是否拥有该 channel
func (c *ChannelService) IsOwner(userID, channelID string) bool {
	var count int64
	c.db.DB.Model(&models.UserChannel{}).
		Where("user_id = ? AND channel_id = ? AND deleted_at IS NULL", userID, channelID).
		Count(&count)
	return count > 0
}

func (c *ChannelService) UpdateChannelStats(channelID string, bytes, requests int64, connections int) error {
	var channel models.Channel
	if err := c.db.DB.Where("id = ?", channelID).First(&channel).Error; err != nil {
		return err
	}

	channel.Stats.TotalBytes += bytes
	channel.Stats.TotalRequests += requests
	channel.Stats.TodayBytes += bytes
	channel.Stats.TodayRequests += requests
	channel.Stats.ActiveConnections = connections
	channel.UpdatedAt = time.Now()

	return c.db.DB.Save(&channel).Error
}

func isValidChannelType(t string) bool {
	switch t {
	case "http", "https", "ws", "tcp", "udp",
		"tunnel-http", "tunnel-tcp":
		return true
	}
	return false
}

// generateEndpointSimple generates a stable endpoint URL for http/https/ws types.
// TCP/UDP endpoints are assigned after port allocation (see ChannelServiceV2).
func generateEndpointSimple(channelType, baseDomain, channelID string) string {
	switch channelType {
	case "http":
		return fmt.Sprintf("http://%s/proxy/%s", baseDomain, channelID)
	case "https":
		return fmt.Sprintf("https://%s/proxy/%s", baseDomain, channelID)
	case "ws":
		return fmt.Sprintf("ws://%s/proxy/%s", baseDomain, channelID)
	case "tunnel-http":
		return fmt.Sprintf("http://%s/proxy/%s", baseDomain, channelID)
	case "tunnel-tcp":
		return fmt.Sprintf("tcp://%s/channel/%s", baseDomain, channelID)
	case "tcp":
		// Port will be assigned at runtime; placeholder for now
		return fmt.Sprintf("tcp://%s/channel/%s", baseDomain, channelID)
	case "udp":
		return fmt.Sprintf("udp://%s/channel/%s", baseDomain, channelID)
	default:
		return fmt.Sprintf("%s/channel/%s", baseDomain, channelID)
	}
}
// generateChannelSecret 生成格式为 "ch_sec_<32位hex>" 的永久密钥
func generateChannelSecret() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "ch_sec_" + hex.EncodeToString(b)
}

// GetChannelBySecret 通过 secret 查找 channel（用于 tunnel agent 认证）
func (c *ChannelService) GetChannelBySecret(secret string) (*models.Channel, error) {
	var channel models.Channel
	if err := c.db.DB.Where("secret = ? AND deleted_at IS NULL", secret).First(&channel).Error; err != nil {
		return nil, err
	}
	return &channel, nil
}

// RotateSecret 重新生成 channel secret（旧 secret 立即失效，在线 agent 将被踢出）
func (c *ChannelService) RotateSecret(userID, channelID string) (string, error) {
	if !c.IsOwner(userID, channelID) {
		return "", ErrNotChannelOwner
	}
	newSecret := generateChannelSecret()
	if err := c.db.DB.Model(&models.Channel{}).
		Where("id = ?", channelID).
		Update("secret", newSecret).Error; err != nil {
		return "", err
	}
	// 清除缓存
	c.redis.Delete(fmt.Sprintf("channel:%s", channelID))
	return newSecret, nil
}
