package services

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// AccessService 接入申请中介：caller 申请 → owner 审批 → agent 拉取同步白名单。
type AccessService struct {
	db       *DatabaseService
	agentSvc *AgentService
}

func NewAccessService(db *DatabaseService, agentSvc *AgentService) *AccessService {
	return &AccessService{db: db, agentSvc: agentSvc}
}

var (
	ErrAccessNotFound      = errors.New("access request not found")
	ErrAccessNotOwner      = errors.New("not agent owner")
	ErrAccessInvalidPubKey = errors.New("invalid caller_pubkey")
	ErrAccessFPMismatch    = errors.New("caller_fp does not match caller_pubkey")
	ErrAccessNotPublic     = errors.New("agent is not public")
	ErrAccessBadStatus     = errors.New("invalid status transition")
)

// RequestAccessParams caller 发起申请
type RequestAccessParams struct {
	AgentID      string
	CallerFP     string
	CallerPubKey string // base64
	CallerName   string
	Message      string
}

// Request 对公开 agent 发起/重提申请。已 approved 则原样返回；rejected/revoked 可重新变 pending。
func (s *AccessService) Request(p RequestAccessParams) (*models.AgentAccessGrant, error) {
	if !agentFPRegex.MatchString(p.CallerFP) {
		return nil, ErrMailboxInvalidFP
	}
	if err := validatePubKeyMatchesFP(p.CallerPubKey, p.CallerFP); err != nil {
		return nil, err
	}

	agent, err := s.agentSvc.GetByID(p.AgentID)
	if err != nil {
		return nil, ErrAgentNotFound
	}
	if !agent.IsPublic {
		return nil, ErrAccessNotPublic
	}

	var existing models.AgentAccessGrant
	err = s.db.DB.Where(
		"agent_id = ? AND caller_fp = ? AND deleted_at IS NULL",
		p.AgentID, p.CallerFP,
	).First(&existing).Error
	if err == nil {
		switch existing.Status {
		case models.AccessApproved:
			return &existing, nil
		case models.AccessPending:
			// 刷新留言/名字
			if p.CallerName != "" {
				existing.CallerName = p.CallerName
			}
			if p.Message != "" {
				existing.Message = p.Message
			}
			existing.CallerPubKey = p.CallerPubKey
			if err := s.db.DB.Save(&existing).Error; err != nil {
				return nil, err
			}
			return &existing, nil
		case models.AccessRejected, models.AccessRevoked:
			existing.Status = models.AccessPending
			existing.CallerPubKey = p.CallerPubKey
			existing.CallerName = p.CallerName
			existing.Message = p.Message
			existing.DecidedAt = nil
			if err := s.db.DB.Save(&existing).Error; err != nil {
				return nil, err
			}
			return &existing, nil
		}
	}
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, err
	}

	grant := &models.AgentAccessGrant{
		ID:           uuid.New().String(),
		AgentID:      p.AgentID,
		CallerFP:     p.CallerFP,
		CallerPubKey: p.CallerPubKey,
		CallerName:   p.CallerName,
		Message:      p.Message,
		Status:       models.AccessPending,
	}
	if err := s.db.DB.Create(grant).Error; err != nil {
		return nil, err
	}
	return grant, nil
}

// ListByAgent owner 查看某 agent 的申请列表
func (s *AccessService) ListByAgent(userID, agentID, status string) ([]models.AgentAccessGrant, error) {
	agent, err := s.agentSvc.GetByID(agentID)
	if err != nil {
		return nil, err
	}
	if agent.UserID != userID {
		return nil, ErrAccessNotOwner
	}
	q := s.db.DB.Where("agent_id = ? AND deleted_at IS NULL", agentID)
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var rows []models.AgentAccessGrant
	err = q.Order("created_at DESC").Limit(200).Find(&rows).Error
	return rows, err
}

// Decide 批准 / 拒绝 / 撤销
func (s *AccessService) Decide(userID, grantID string, next models.AccessGrantStatus) (*models.AgentAccessGrant, error) {
	grant, err := s.getByID(grantID)
	if err != nil {
		return nil, err
	}
	agent, err := s.agentSvc.GetByID(grant.AgentID)
	if err != nil {
		return nil, err
	}
	if agent.UserID != userID {
		return nil, ErrAccessNotOwner
	}

	switch next {
	case models.AccessApproved:
		if grant.Status != models.AccessPending && grant.Status != models.AccessRejected && grant.Status != models.AccessRevoked {
			if grant.Status == models.AccessApproved {
				return grant, nil
			}
			return nil, ErrAccessBadStatus
		}
	case models.AccessRejected:
		if grant.Status != models.AccessPending {
			return nil, ErrAccessBadStatus
		}
	case models.AccessRevoked:
		if grant.Status != models.AccessApproved {
			return nil, ErrAccessBadStatus
		}
	default:
		return nil, ErrAccessBadStatus
	}

	now := time.Now()
	grant.Status = next
	grant.DecidedAt = &now
	if err := s.db.DB.Save(grant).Error; err != nil {
		return nil, err
	}
	return grant, nil
}

// GetMine caller 查询自己对某 agent 的申请状态
func (s *AccessService) GetMine(agentID, callerFP string) (*models.AgentAccessGrant, error) {
	if !agentFPRegex.MatchString(callerFP) {
		return nil, ErrMailboxInvalidFP
	}
	var grant models.AgentAccessGrant
	err := s.db.DB.Where(
		"agent_id = ? AND caller_fp = ? AND deleted_at IS NULL",
		agentID, callerFP,
	).First(&grant).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrAccessNotFound
		}
		return nil, err
	}
	return &grant, nil
}

// ListForAgentSync agent 拉取需同步到白名单的授权变更（approved + revoked）
func (s *AccessService) ListForAgentSync(agentID string, since time.Time) ([]models.AgentAccessGrant, error) {
	if _, err := s.agentSvc.GetByID(agentID); err != nil {
		return nil, err
	}
	q := s.db.DB.Where(
		"agent_id = ? AND status IN ? AND deleted_at IS NULL",
		agentID,
		[]models.AccessGrantStatus{models.AccessApproved, models.AccessRevoked},
	)
	if !since.IsZero() {
		q = q.Where("updated_at > ?", since)
	}
	var rows []models.AgentAccessGrant
	err := q.Order("updated_at ASC").Limit(500).Find(&rows).Error
	return rows, err
}

func (s *AccessService) getByID(id string) (*models.AgentAccessGrant, error) {
	var grant models.AgentAccessGrant
	if err := s.db.DB.Where("id = ? AND deleted_at IS NULL", id).First(&grant).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrAccessNotFound
		}
		return nil, err
	}
	return &grant, nil
}

// validatePubKeyMatchesFP 校验 base64 公钥为 32 字节且指纹匹配
func validatePubKeyMatchesFP(pubB64, fp string) error {
	raw, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(raw) != 32 {
		// try raw URL encoding without padding
		raw, err = base64.RawStdEncoding.DecodeString(pubB64)
		if err != nil || len(raw) != 32 {
			return ErrAccessInvalidPubKey
		}
	}
	sum := sha256.Sum256(raw)
	derived := hex.EncodeToString(sum[:8])
	if derived != fp {
		return ErrAccessFPMismatch
	}
	return nil
}

// BuildAgentWSEndpoint 批准后给 caller 的连接地址（不含 secret；鉴权靠 Noise 白名单）
func BuildAgentWSEndpoint(baseURL, channelID, pathPrefix, agentID string) string {
	u := baseURL
	if len(u) >= 8 && u[:8] == "https://" {
		u = "wss://" + u[8:]
	} else if len(u) >= 7 && u[:7] == "http://" {
		u = "ws://" + u[7:]
	}
	for len(u) > 0 && u[len(u)-1] == '/' {
		u = u[:len(u)-1]
	}
	prefix := pathPrefix
	if prefix != "" && prefix[0] != '/' {
		prefix = "/" + prefix
	}
	if prefix != "" && prefix[len(prefix)-1] != '/' {
		prefix += "/"
	}
	if prefix == "" {
		prefix = "/"
	}
	return u + "/proxy/" + channelID + prefix + "acp/ws?agentId=" + agentID
}
