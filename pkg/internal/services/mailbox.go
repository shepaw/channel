package services

import (
	"errors"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// MailboxService channel 双向信箱：caller 留言、agent 拉取/回投、caller 收信。
// 全程只处理密文；加解密在 agent / app 两端完成。
type MailboxService struct {
	db *DatabaseService
}

func NewMailboxService(db *DatabaseService) *MailboxService {
	return &MailboxService{db: db}
}

var (
	ErrMailboxAgentNotFound = errors.New("agent not found")
	ErrMailboxFull          = errors.New("mailbox full")
	ErrMailboxTooLarge      = errors.New("ciphertext too large")
	ErrMailboxNotFound      = errors.New("mailbox message not found")
	ErrMailboxInvalidFP     = errors.New("invalid caller_fp")
	ErrMailboxEmptyBody     = errors.New("ciphertext required")
	ErrMailboxDupReply      = errors.New("reply already exists for this message_id")
)

// DepositInboundParams caller → agent 留言
type DepositInboundParams struct {
	AgentID    string
	CallerFP   string
	MessageID  string
	SessionID  string
	Ciphertext string // base64
}

// DepositInbound 投递留言。按 agent 配额限流；同 message_id 幂等（已存在则返回已有记录）。
func (s *MailboxService) DepositInbound(p DepositInboundParams) (*models.MailboxMessage, error) {
	if err := s.validateDeposit(p.AgentID, p.CallerFP, p.Ciphertext); err != nil {
		return nil, err
	}
	if p.MessageID == "" {
		p.MessageID = uuid.New().String()
	}

	// 幂等：同 agent + message_id + inbound 已存在则直接返回
	var existing models.MailboxMessage
	err := s.db.DB.Where(
		"agent_id = ? AND direction = ? AND message_id = ? AND deleted_at IS NULL",
		p.AgentID, models.MailboxDirectionInbound, p.MessageID,
	).First(&existing).Error
	if err == nil {
		return &existing, nil
	}
	if err != gorm.ErrRecordNotFound {
		return nil, err
	}

	if err := s.ensureQuota(p.AgentID); err != nil {
		return nil, err
	}

	msg := &models.MailboxMessage{
		ID:         uuid.New().String(),
		AgentID:    p.AgentID,
		Direction:  models.MailboxDirectionInbound,
		Status:     models.MailboxStatusPending,
		CallerFP:   p.CallerFP,
		MessageID:  p.MessageID,
		SessionID:  p.SessionID,
		Ciphertext: p.Ciphertext,
		SizeBytes:  len(p.Ciphertext),
		ExpiresAt:  time.Now().Add(models.MailboxTTL),
	}
	if err := s.db.DB.Create(msg).Error; err != nil {
		return nil, err
	}
	return msg, nil
}

// ClaimPending 原子领取最多 limit 条 pending inbound（含超时回收的 in_flight）。
// 领取后标记 in_flight；agent 处理完须 Ack。
func (s *MailboxService) ClaimPending(agentID string, limit int) ([]models.MailboxMessage, error) {
	if _, err := s.requireAgent(agentID); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 1
	}
	if limit > 20 {
		limit = 20
	}

	s.recoverTimedOut(agentID)

	var claimed []models.MailboxMessage
	err := s.db.DB.Transaction(func(tx *gorm.DB) error {
		var rows []models.MailboxMessage
		// 不用 SELECT FOR UPDATE：SQLite 不支持；改为「查出后按 status=pending 条件更新」，
		// 更新行数与查出不一致时说明被并发抢走，本事务只返回成功改到的行。
		if err := tx.
			Where(
				"agent_id = ? AND direction = ? AND status = ? AND deleted_at IS NULL AND expires_at > ?",
				agentID, models.MailboxDirectionInbound, models.MailboxStatusPending, time.Now(),
			).
			Order("created_at ASC").
			Limit(limit).
			Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		now := time.Now()
		ids := make([]string, len(rows))
		for i := range rows {
			ids[i] = rows[i].ID
		}
		res := tx.Model(&models.MailboxMessage{}).
			Where("id IN ? AND status = ?", ids, models.MailboxStatusPending).
			Updates(map[string]interface{}{
				"status":     models.MailboxStatusInFlight,
				"claimed_at": now,
				"updated_at": now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		// 重新读出实际变成 in_flight 的行（本事务内）
		return tx.Where("id IN ? AND status = ?", ids, models.MailboxStatusInFlight).Find(&claimed).Error
	})
	return claimed, err
}

// AckInbound agent 确认已处理（成功投递回复或丢弃）后删除 inbound。
func (s *MailboxService) AckInbound(agentID string, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if _, err := s.requireAgent(agentID); err != nil {
		return 0, err
	}
	res := s.db.DB.Where(
		"agent_id = ? AND direction = ? AND id IN ?",
		agentID, models.MailboxDirectionInbound, ids,
	).Delete(&models.MailboxMessage{})
	return res.RowsAffected, res.Error
}

// DepositReplyParams agent → caller 回复
type DepositReplyParams struct {
	AgentID    string
	CallerFP   string
	MessageID  string // 回复自身的 id
	ReplyTo    string // 原留言 message_id（幂等键）
	SessionID  string
	Ciphertext string
}

// DepositReply 投递回复。同一 reply_to 幂等；写完不自动删 inbound（由 AckInbound 负责）。
func (s *MailboxService) DepositReply(p DepositReplyParams) (*models.MailboxMessage, error) {
	if err := s.validateDeposit(p.AgentID, p.CallerFP, p.Ciphertext); err != nil {
		return nil, err
	}
	if p.ReplyTo == "" {
		return nil, errors.New("reply_to is required")
	}
	if p.MessageID == "" {
		p.MessageID = uuid.New().String()
	}

	var existing models.MailboxMessage
	err := s.db.DB.Where(
		"agent_id = ? AND direction = ? AND reply_to = ? AND deleted_at IS NULL",
		p.AgentID, models.MailboxDirectionReply, p.ReplyTo,
	).First(&existing).Error
	if err == nil {
		return &existing, nil // 幂等
	}
	if err != gorm.ErrRecordNotFound {
		return nil, err
	}

	if err := s.ensureQuota(p.AgentID); err != nil {
		return nil, err
	}

	msg := &models.MailboxMessage{
		ID:         uuid.New().String(),
		AgentID:    p.AgentID,
		Direction:  models.MailboxDirectionReply,
		Status:     models.MailboxStatusPending,
		CallerFP:   p.CallerFP,
		MessageID:  p.MessageID,
		SessionID:  p.SessionID,
		ReplyTo:    p.ReplyTo,
		Ciphertext: p.Ciphertext,
		SizeBytes:  len(p.Ciphertext),
		ExpiresAt:  time.Now().Add(models.MailboxTTL),
	}
	if err := s.db.DB.Create(msg).Error; err != nil {
		return nil, err
	}
	return msg, nil
}

// ListReplies 列出某 caller 在该 agent 下未收取的回复（pending）。
func (s *MailboxService) ListReplies(agentID, callerFP string, after time.Time, limit int) ([]models.MailboxMessage, error) {
	if _, err := s.requireAgent(agentID); err != nil {
		return nil, err
	}
	if !agentFPRegex.MatchString(callerFP) {
		return nil, ErrMailboxInvalidFP
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}

	q := s.db.DB.Where(
		"agent_id = ? AND direction = ? AND status = ? AND caller_fp = ? AND deleted_at IS NULL AND expires_at > ?",
		agentID, models.MailboxDirectionReply, models.MailboxStatusPending, callerFP, time.Now(),
	)
	if !after.IsZero() {
		q = q.Where("created_at > ?", after)
	}

	var rows []models.MailboxMessage
	err := q.Order("created_at ASC").Limit(limit).Find(&rows).Error
	return rows, err
}

// AckReplies caller 确认已持久化到本地后删除回复。
func (s *MailboxService) AckReplies(agentID, callerFP string, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if _, err := s.requireAgent(agentID); err != nil {
		return 0, err
	}
	res := s.db.DB.Where(
		"agent_id = ? AND direction = ? AND caller_fp = ? AND id IN ?",
		agentID, models.MailboxDirectionReply, callerFP, ids,
	).Delete(&models.MailboxMessage{})
	return res.RowsAffected, res.Error
}

// PendingCount 当前可领取的 inbound 数量（含可回收的超时 in_flight）
func (s *MailboxService) PendingCount(agentID string) (int64, error) {
	s.recoverTimedOut(agentID)
	var n int64
	err := s.db.DB.Model(&models.MailboxMessage{}).
		Where(
			"agent_id = ? AND direction = ? AND status = ? AND deleted_at IS NULL AND expires_at > ?",
			agentID, models.MailboxDirectionInbound, models.MailboxStatusPending, time.Now(),
		).Count(&n).Error
	return n, err
}

func (s *MailboxService) validateDeposit(agentID, callerFP, ciphertext string) error {
	if _, err := s.requireAgent(agentID); err != nil {
		return err
	}
	if !agentFPRegex.MatchString(callerFP) {
		return ErrMailboxInvalidFP
	}
	if ciphertext == "" {
		return ErrMailboxEmptyBody
	}
	if len(ciphertext) > models.MailboxMaxCipherBytes {
		return ErrMailboxTooLarge
	}
	return nil
}

func (s *MailboxService) requireAgent(agentID string) (*models.Agent, error) {
	var agent models.Agent
	if err := s.db.DB.Where("agent_id = ? AND deleted_at IS NULL", agentID).First(&agent).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrMailboxAgentNotFound
		}
		return nil, err
	}
	return &agent, nil
}

func (s *MailboxService) ensureQuota(agentID string) error {
	var n int64
	if err := s.db.DB.Model(&models.MailboxMessage{}).
		Where("agent_id = ? AND deleted_at IS NULL", agentID).
		Count(&n).Error; err != nil {
		return err
	}
	if n >= int64(models.MailboxMaxPerAgent) {
		return ErrMailboxFull
	}
	return nil
}

// recoverTimedOut 把超时的 in_flight 重新变为 pending（at-least-once）
func (s *MailboxService) recoverTimedOut(agentID string) {
	cutoff := time.Now().Add(-models.MailboxVisibilityTimeout)
	_ = s.db.DB.Model(&models.MailboxMessage{}).
		Where(
			"agent_id = ? AND direction = ? AND status = ? AND claimed_at < ? AND deleted_at IS NULL",
			agentID, models.MailboxDirectionInbound, models.MailboxStatusInFlight, cutoff,
		).
		Updates(map[string]interface{}{
			"status":     models.MailboxStatusPending,
			"claimed_at": nil,
			"updated_at": time.Now(),
		}).Error
}
