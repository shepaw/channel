package services

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"gorm.io/gorm"
)

// AgentService agent 注册中心：登记、心跳、按 channel 查询
type AgentService struct {
	db *DatabaseService
}

func NewAgentService(db *DatabaseService) *AgentService {
	return &AgentService{db: db}
}

var (
	ErrAgentNotFound       = errors.New("agent not found")
	ErrAgentChannelBound   = errors.New("agent_id already bound to another channel")
	ErrInvalidAgentID      = errors.New("invalid agent_id format")
	ErrInvalidAgentFP      = errors.New("invalid agent_fp format")
	ErrAgentChannelNoOwner = errors.New("channel has no owner")
	ErrAgentNameRequired   = errors.New("name is required when agent is public")
	ErrAgentNotPublic      = errors.New("agent is not public")
)

// agentIDRegex 校验客户端上报的 agent_id。
// agent-bridge 生成的格式为 "acp_agent_" + 8 位 hex（identity.json），
// 这里适度放宽到 3-64 位小写字母数字及 _-，避免对未来 id 方案过度约束。
var agentIDRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{2,63}$`)

// agentFPRegex 公钥指纹：16 位 hex
var agentFPRegex = regexp.MustCompile(`^[0-9a-f]{16}$`)

// RegisterAgentParams 一次注册/心跳上报的参数
type RegisterAgentParams struct {
	AgentID     string
	ChannelID   string
	Name        string
	Description string
	AgentFP     string
	AgentPubKey string // base64 raw 32-byte；批准后下发给 caller
	PathPrefix  string
	DeviceID    string
	Capacity    int // <=0 表示不变/默认
	ActiveCount int
}

// Register 注册或刷新一个 agent（幂等）：
//   - agent_id 不存在 → 新建，绑定 channel 及其 owner
//   - agent_id 已存在且属于同一 channel → 更新可变字段并刷新 LastSeenAt（兼作心跳）
//   - agent_id 已存在但属于其他 channel → ErrAgentChannelBound（不允许静默挪走，
//     防止他人用自己的 channel 抢占别人的 agent 身份）
func (s *AgentService) Register(p RegisterAgentParams) (*models.Agent, error) {
	if !agentIDRegex.MatchString(p.AgentID) {
		return nil, ErrInvalidAgentID
	}
	if p.AgentFP != "" && !agentFPRegex.MatchString(p.AgentFP) {
		return nil, ErrInvalidAgentFP
	}

	ownerID, err := s.channelOwnerID(p.ChannelID)
	if err != nil {
		return nil, err
	}

	var agent models.Agent
	err = s.db.DB.Where("agent_id = ? AND deleted_at IS NULL", p.AgentID).First(&agent).Error
	switch {
	case err == nil:
		// 已存在：同 channel 刷新，异 channel 拒绝
		if agent.ChannelID != p.ChannelID {
			return nil, ErrAgentChannelBound
		}
		if p.Name != "" {
			agent.Name = p.Name
		}
		if p.Description != "" {
			agent.Description = p.Description
		}
		if p.AgentFP != "" {
			agent.AgentFP = p.AgentFP
		}
		if p.AgentPubKey != "" {
			agent.AgentPubKey = p.AgentPubKey
		}
		if p.PathPrefix != "" {
			agent.PathPrefix = p.PathPrefix
		}
		if p.DeviceID != "" {
			agent.DeviceID = p.DeviceID
		}
		if p.Capacity > 0 {
			agent.Capacity = p.Capacity
		}
		agent.ActiveCount = p.ActiveCount
		agent.LastSeenAt = time.Now()
		if err := s.db.DB.Save(&agent).Error; err != nil {
			return nil, err
		}
		return &agent, nil

	case err == gorm.ErrRecordNotFound:
		capacity := p.Capacity
		if capacity <= 0 {
			capacity = 5 // 默认并发容量
		}
		agent = models.Agent{
			AgentID:     p.AgentID,
			ChannelID:   p.ChannelID,
			UserID:      ownerID,
			Name:        p.Name,
			Description: p.Description,
			AgentFP:     p.AgentFP,
			AgentPubKey: p.AgentPubKey,
			PathPrefix:  p.PathPrefix,
			DeviceID:    p.DeviceID,
			Capacity:    capacity,
			ActiveCount: p.ActiveCount,
			IsPublic:    false, // 默认不公开
			LastSeenAt:  time.Now(),
		}
		if err := s.db.DB.Create(&agent).Error; err != nil {
			return nil, err
		}
		return &agent, nil

	default:
		return nil, err
	}
}

// GetByID 按 agent_id 查询
func (s *AgentService) GetByID(agentID string) (*models.Agent, error) {
	var agent models.Agent
	if err := s.db.DB.Where("agent_id = ? AND deleted_at IS NULL", agentID).First(&agent).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrAgentNotFound
		}
		return nil, err
	}
	return &agent, nil
}

// ListByChannel 列出 channel 下所有 agent（含软删除过滤）
func (s *AgentService) ListByChannel(channelID string) ([]models.Agent, error) {
	var agents []models.Agent
	err := s.db.DB.
		Where("channel_id = ? AND deleted_at IS NULL", channelID).
		Order("created_at ASC").
		Find(&agents).Error
	return agents, err
}

// UpdateAgentParams owner 可改的名片字段。指针用于区分"未设置"与"显式清空/关闭"。
type UpdateAgentParams struct {
	Name        *string
	Description *string
	IsPublic    *bool
	Capacity    *int // >0 才写入；公开无关
}

// Update 更新 agent 名片（owner 操作）。
// 置 is_public=true 时最终 name 必须非空；若同时传了空 name 也拒绝。
func (s *AgentService) Update(userID, agentID string, p UpdateAgentParams) (*models.Agent, error) {
	agent, err := s.GetByID(agentID)
	if err != nil {
		return nil, err
	}
	if agent.UserID != userID {
		return nil, ErrNotChannelOwner
	}

	if p.Name != nil {
		agent.Name = *p.Name
	}
	if p.Description != nil {
		agent.Description = *p.Description
	}
	if p.IsPublic != nil {
		agent.IsPublic = *p.IsPublic
	}
	if p.Capacity != nil && *p.Capacity > 0 {
		agent.Capacity = *p.Capacity
	}

	// 公开时必须有名字（最终态校验，覆盖「先公开再改空名」的绕过）
	if agent.IsPublic && agent.Name == "" {
		return nil, ErrAgentNameRequired
	}

	agent.UpdatedAt = time.Now()
	if err := s.db.DB.Save(agent).Error; err != nil {
		return nil, err
	}
	return agent, nil
}

// SearchPublicParams 公开目录搜索参数
type SearchPublicParams struct {
	Query    string // 匹配 name / description / agent_id（大小写不敏感）
	Page     int    // 从 1 开始
	PageSize int    // 默认 20，最大 50
}

// SearchPublic 只返回 is_public=true 的 agent；按最近活跃优先。
func (s *AgentService) SearchPublic(p SearchPublicParams) ([]models.Agent, int64, error) {
	page := p.Page
	if page < 1 {
		page = 1
	}
	pageSize := p.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 50 {
		pageSize = 50
	}

	q := s.db.DB.Model(&models.Agent{}).
		Where("is_public = ? AND deleted_at IS NULL", true)

	if query := strings.TrimSpace(p.Query); query != "" {
		// 剥掉 LIKE 通配符，避免用户输入扩大匹配面（SQLite/Postgres 通配符语义一致）
		query = strings.Map(func(r rune) rune {
			if r == '%' || r == '_' {
				return -1
			}
			return r
		}, query)
		if query != "" {
			like := "%" + query + "%"
			q = q.Where("(LOWER(name) LIKE LOWER(?) OR LOWER(description) LIKE LOWER(?) OR LOWER(agent_id) LIKE LOWER(?))", like, like, like)
		}
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var agents []models.Agent
	err := q.Order("last_seen_at DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&agents).Error
	return agents, total, err
}

// GetPublicCard 取公开名片；非公开或不存在统一返回 ErrAgentNotPublic，避免枚举私有 agent。
func (s *AgentService) GetPublicCard(agentID string) (*models.Agent, error) {
	agent, err := s.GetByID(agentID)
	if err != nil {
		return nil, ErrAgentNotPublic
	}
	if !agent.IsPublic {
		return nil, ErrAgentNotPublic
	}
	return agent, nil
}

// Delete 注销 agent（owner 操作）
func (s *AgentService) Delete(userID, agentID string) error {
	agent, err := s.GetByID(agentID)
	if err != nil {
		return err
	}
	if agent.UserID != userID {
		return ErrNotChannelOwner
	}
	return s.db.DB.Where("agent_id = ?", agentID).Delete(&models.Agent{}).Error
}

// channelOwnerID 查 channel 的 owner（取第一个关联用户）
func (s *AgentService) channelOwnerID(channelID string) (string, error) {
	var uc models.UserChannel
	err := s.db.DB.
		Where("channel_id = ? AND deleted_at IS NULL", channelID).
		Order("created_at ASC").
		First(&uc).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return "", ErrAgentChannelNoOwner
		}
		return "", err
	}
	return uc.UserID, nil
}
