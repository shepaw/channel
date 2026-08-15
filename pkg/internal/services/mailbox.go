package services

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// MailboxService 云端收件箱：caller 留言、agent/group 拉取/回投、caller 收信。
// 全程只处理密文；加解密在 agent-bridge / shepaw 两端完成。
type MailboxService struct {
	db *DatabaseService
}

func NewMailboxService(db *DatabaseService) *MailboxService {
	return &MailboxService{db: db}
}

var (
	ErrMailboxTargetNotFound = errors.New("mailbox target not found")
	ErrMailboxAgentNotFound  = ErrMailboxTargetNotFound // 兼容旧名
	ErrMailboxFull           = errors.New("mailbox full")
	ErrMailboxTooLarge       = errors.New("ciphertext too large")
	ErrMailboxNotFound       = errors.New("mailbox message not found")
	ErrMailboxInvalidFP      = errors.New("invalid caller_fp")
	ErrMailboxEmptyBody      = errors.New("ciphertext required")
	ErrMailboxDupReply       = errors.New("reply already exists for this message_id")
	ErrMailboxInvalidTarget  = errors.New("invalid mailbox target")
)

var mailboxTargetIDRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{2,63}$`)

// DepositInboundParams caller → agent/group 留言
type DepositInboundParams struct {
	TargetType models.MailboxTargetType
	TargetID   string
	CallerFP   string
	MessageID  string
	RequestID  string
	SessionID  string
	GroupID    string
	Kind       models.MailboxKind
	Ciphertext string
}

// DepositInbound 投递留言。按 target 配额限流；同 message_id 幂等。
func (s *MailboxService) DepositInbound(p DepositInboundParams) (*models.MailboxMessage, error) {
	resolvedType, targetID := normalizeTarget(p.TargetType, p.TargetID)
	inferred, err := s.requireTarget(resolvedType, targetID)
	if err != nil {
		return nil, err
	}
	if err := s.validateDeposit(inferred, targetID, p.CallerFP, p.Ciphertext); err != nil {
		return nil, err
	}
	if p.MessageID == "" {
		p.MessageID = uuid.New().String()
	}
	kind := p.Kind
	if kind == "" {
		kind = models.MailboxKindChat
	}

	var existing models.MailboxMessage
	err = s.db.DB.Where(
		"target_id = ? AND direction = ? AND message_id = ? AND deleted_at IS NULL",
		targetID, models.MailboxDirectionInbound, p.MessageID,
	).First(&existing).Error
	if err == nil {
		return &existing, nil
	}
	if err != gorm.ErrRecordNotFound {
		return nil, err
	}

	if err := s.ensureQuota(targetID); err != nil {
		return nil, err
	}

	msg := &models.MailboxMessage{
		ID:         uuid.New().String(),
		TargetType: inferred,
		TargetID:   targetID,
		AgentID:    targetID,
		Direction:  models.MailboxDirectionInbound,
		Status:     models.MailboxStatusPending,
		Kind:       kind,
		CallerFP:   p.CallerFP,
		MessageID:  p.MessageID,
		RequestID:  p.RequestID,
		SessionID:  p.SessionID,
		GroupID:    p.GroupID,
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
func (s *MailboxService) ClaimPending(targetID string, limit int) ([]models.MailboxMessage, error) {
	if _, err := s.requireTarget("", targetID); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 1
	}
	if limit > 20 {
		limit = 20
	}

	s.recoverTimedOut(targetID)

	var claimed []models.MailboxMessage
	err := s.db.DB.Transaction(func(tx *gorm.DB) error {
		var rows []models.MailboxMessage
		if err := tx.
			Where(
				"target_id = ? AND direction = ? AND status = ? AND deleted_at IS NULL AND expires_at > ?",
				targetID, models.MailboxDirectionInbound, models.MailboxStatusPending, time.Now(),
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
		return tx.Where("id IN ? AND status = ?", ids, models.MailboxStatusInFlight).Find(&claimed).Error
	})
	return claimed, err
}

// AckInbound agent/group 确认已处理 inbound。
func (s *MailboxService) AckInbound(targetID string, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if _, err := s.requireTarget("", targetID); err != nil {
		return 0, err
	}
	res := s.db.DB.Where(
		"target_id = ? AND direction = ? AND id IN ?",
		targetID, models.MailboxDirectionInbound, ids,
	).Delete(&models.MailboxMessage{})
	return res.RowsAffected, res.Error
}

// DepositReplyParams agent/group → caller 回复
type DepositReplyParams struct {
	TargetType models.MailboxTargetType
	TargetID   string
	CallerFP   string
	MessageID  string
	ReplyTo    string
	RequestID  string
	SessionID  string
	GroupID    string
	Kind       models.MailboxKind
	Ciphertext string
}

// DepositReply 投递回复。同一 reply_to 幂等。
func (s *MailboxService) DepositReply(p DepositReplyParams) (*models.MailboxMessage, error) {
	resolvedType, targetID := normalizeTarget(p.TargetType, p.TargetID)
	inferred, err := s.requireTarget(resolvedType, targetID)
	if err != nil {
		return nil, err
	}
	if err := s.validateDeposit(inferred, targetID, p.CallerFP, p.Ciphertext); err != nil {
		return nil, err
	}
	if p.ReplyTo == "" {
		return nil, errors.New("reply_to is required")
	}
	if p.MessageID == "" {
		p.MessageID = uuid.New().String()
	}
	kind := p.Kind
	if kind == "" {
		kind = models.MailboxKindChat
	}

	var existing models.MailboxMessage
	err = s.db.DB.Where(
		"target_id = ? AND direction = ? AND message_id = ? AND deleted_at IS NULL",
		targetID, models.MailboxDirectionReply, p.MessageID,
	).First(&existing).Error
	if err == nil {
		return &existing, nil
	}
	if err != gorm.ErrRecordNotFound {
		return nil, err
	}

	if err := s.ensureQuota(targetID); err != nil {
		return nil, err
	}

	msg := &models.MailboxMessage{
		ID:         uuid.New().String(),
		TargetType: inferred,
		TargetID:   targetID,
		AgentID:    targetID,
		Direction:  models.MailboxDirectionReply,
		Status:     models.MailboxStatusPending,
		Kind:       kind,
		CallerFP:   p.CallerFP,
		MessageID:  p.MessageID,
		RequestID:  p.RequestID,
		SessionID:  p.SessionID,
		GroupID:    p.GroupID,
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

// ListReplies 列出某 caller 在指定 target 下未收取的回复。
func (s *MailboxService) ListReplies(targetID, callerFP string, after time.Time, limit int) ([]models.MailboxMessage, error) {
	if _, err := s.requireTarget("", targetID); err != nil {
		return nil, err
	}
	return s.listRepliesQuery(
		s.db.DB.Where(
			"target_id = ? AND direction = ? AND status = ? AND caller_fp = ? AND deleted_at IS NULL AND expires_at > ?",
			targetID, models.MailboxDirectionReply, models.MailboxStatusPending, callerFP, time.Now(),
		),
		after, limit,
	)
}

// ListAllRepliesForCaller 跨 target 拉取 caller 的全部待收回复（app 上线时统一收取）。
func (s *MailboxService) ListAllRepliesForCaller(callerFP string, after time.Time, limit int) ([]models.MailboxMessage, error) {
	if !agentFPRegex.MatchString(callerFP) {
		return nil, ErrMailboxInvalidFP
	}
	return s.listRepliesQuery(
		s.db.DB.Where(
			"direction = ? AND status = ? AND caller_fp = ? AND deleted_at IS NULL AND expires_at > ?",
			models.MailboxDirectionReply, models.MailboxStatusPending, callerFP, time.Now(),
		),
		after, limit,
	)
}

func (s *MailboxService) listRepliesQuery(q *gorm.DB, after time.Time, limit int) ([]models.MailboxMessage, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	if !after.IsZero() {
		q = q.Where("created_at > ?", after)
	}
	var rows []models.MailboxMessage
	err := q.Order("created_at ASC").Limit(limit).Find(&rows).Error
	return rows, err
}

// AckReplies caller 确认已持久化回复。
func (s *MailboxService) AckReplies(targetID, callerFP string, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if _, err := s.requireTarget("", targetID); err != nil {
		return 0, err
	}
	res := s.db.DB.Where(
		"target_id = ? AND direction = ? AND caller_fp = ? AND id IN ?",
		targetID, models.MailboxDirectionReply, callerFP, ids,
	).Delete(&models.MailboxMessage{})
	return res.RowsAffected, res.Error
}

// AckRepliesGlobal 跨 target 确认回复（配合 ListAllRepliesForCaller）。
func (s *MailboxService) AckRepliesGlobal(callerFP string, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if !agentFPRegex.MatchString(callerFP) {
		return 0, ErrMailboxInvalidFP
	}
	res := s.db.DB.Where(
		"direction = ? AND caller_fp = ? AND id IN ?",
		models.MailboxDirectionReply, callerFP, ids,
	).Delete(&models.MailboxMessage{})
	return res.RowsAffected, res.Error
}

// PendingCount 当前可领取的 inbound 数量。
func (s *MailboxService) PendingCount(targetID string) (int64, error) {
	s.recoverTimedOut(targetID)
	var n int64
	err := s.db.DB.Model(&models.MailboxMessage{}).
		Where(
			"target_id = ? AND direction = ? AND status = ? AND deleted_at IS NULL AND expires_at > ?",
			targetID, models.MailboxDirectionInbound, models.MailboxStatusPending, time.Now(),
		).Count(&n).Error
	return n, err
}

func (s *MailboxService) validateDeposit(targetType models.MailboxTargetType, targetID, callerFP, ciphertext string) error {
	if _, err := s.requireTarget(targetType, targetID); err != nil {
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

func normalizeTarget(targetType models.MailboxTargetType, targetID string) (models.MailboxTargetType, string) {
	if targetType == "" {
		targetType = models.MailboxTargetAgent
	}
	return targetType, targetID
}

func (s *MailboxService) requireTarget(targetType models.MailboxTargetType, targetID string) (models.MailboxTargetType, error) {
	if targetID == "" {
		return "", ErrMailboxInvalidTarget
	}
	if !mailboxTargetIDRegex.MatchString(targetID) {
		return "", ErrMailboxInvalidTarget
	}

	if targetType == "" {
		var agent models.Agent
		err := s.db.DB.Where("agent_id = ? AND deleted_at IS NULL", targetID).First(&agent).Error
		if err == nil {
			return models.MailboxTargetAgent, nil
		}
		if err != gorm.ErrRecordNotFound {
			return "", err
		}
		if isGroupTargetID(targetID) {
			return models.MailboxTargetGroup, nil
		}
		return "", ErrMailboxTargetNotFound
	}

	switch targetType {
	case models.MailboxTargetAgent:
		var agent models.Agent
		if err := s.db.DB.Where("agent_id = ? AND deleted_at IS NULL", targetID).First(&agent).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return "", ErrMailboxTargetNotFound
			}
			return "", err
		}
	case models.MailboxTargetGroup:
		if !isGroupTargetID(targetID) {
			return "", ErrMailboxInvalidTarget
		}
	default:
		return "", ErrMailboxInvalidTarget
	}
	return targetType, nil
}

func isGroupTargetID(id string) bool {
	return strings.HasPrefix(id, "psess_group_") || strings.HasPrefix(id, "group_")
}

func (s *MailboxService) ensureQuota(targetID string) error {
	if _, err := s.purgeExpiredForTarget(targetID); err != nil {
		return err
	}
	var n int64
	if err := s.db.DB.Model(&models.MailboxMessage{}).
		Where("target_id = ? AND deleted_at IS NULL AND expires_at > ?", targetID, time.Now()).
		Count(&n).Error; err != nil {
		return err
	}
	if n >= int64(models.MailboxMaxPerTarget) {
		return ErrMailboxFull
	}
	return nil
}

func (s *MailboxService) purgeExpiredForTarget(targetID string) (int64, error) {
	res := s.db.DB.Where("target_id = ? AND expires_at < ?", targetID, time.Now()).
		Delete(&models.MailboxMessage{})
	return res.RowsAffected, res.Error
}

// PurgeExpired 软删除全局过期行，使配额口径与可见窗口一致。
func (s *MailboxService) PurgeExpired() (int64, error) {
	res := s.db.DB.Where("expires_at < ?", time.Now()).Delete(&models.MailboxMessage{})
	return res.RowsAffected, res.Error
}

func (s *MailboxService) recoverTimedOut(targetID string) {
	cutoff := time.Now().Add(-models.MailboxVisibilityTimeout)
	_ = s.db.DB.Model(&models.MailboxMessage{}).
		Where(
			"target_id = ? AND direction = ? AND status = ? AND claimed_at < ? AND deleted_at IS NULL",
			targetID, models.MailboxDirectionInbound, models.MailboxStatusInFlight, cutoff,
		).
		Updates(map[string]interface{}{
			"status":     models.MailboxStatusPending,
			"claimed_at": nil,
			"updated_at": time.Now(),
		}).Error
}

// BackfillTargetColumns 迁移旧数据：把 agent_id 复制到 target_id。
func (s *MailboxService) BackfillTargetColumns() error {
	return s.db.DB.Exec(`
		UPDATE mailbox_messages
		SET target_id = agent_id, target_type = 'agent'
		WHERE (target_id = '' OR target_id IS NULL) AND agent_id != ''
	`).Error
}
