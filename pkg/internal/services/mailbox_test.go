package services

import (
	"testing"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupMailboxTestDB(t *testing.T) *MailboxService {
	t.Helper()
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.Agent{}, &models.MailboxMessage{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now()
	agent := &models.Agent{
		AgentID:    "acp_agent_aabbccdd",
		ChannelID:  "ch-test-1",
		UserID:     "u-1",
		Name:       "Test",
		Capacity:   5,
		AgentFP:    "0123456789abcdef",
		LastSeenAt: now,
	}
	if err := db.Create(agent).Error; err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return NewMailboxService(&DatabaseService{DB: db})
}

func TestMailboxDepositClaimReplyAck(t *testing.T) {
	svc := setupMailboxTestDB(t)
	agentID := "acp_agent_aabbccdd"
	callerFP := "fedcba9876543210"

	msg, err := svc.DepositInbound(DepositInboundParams{
		AgentID:    agentID,
		CallerFP:   callerFP,
		MessageID:  "msg-1",
		SessionID:  "sess-1",
		Ciphertext: "dGVzdA==",
	})
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if msg.Status != models.MailboxStatusPending {
		t.Fatalf("expected pending, got %s", msg.Status)
	}

	// idempotent
	again, err := svc.DepositInbound(DepositInboundParams{
		AgentID:    agentID,
		CallerFP:   callerFP,
		MessageID:  "msg-1",
		SessionID:  "sess-1",
		Ciphertext: "dGVzdA==",
	})
	if err != nil {
		t.Fatalf("deposit idempotent: %v", err)
	}
	if again.ID != msg.ID {
		t.Fatalf("idempotent should return same id")
	}

	pending, err := svc.PendingCount(agentID)
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if pending != 1 {
		t.Fatalf("pending want 1 got %d", pending)
	}

	claimed, err := svc.ClaimPending(agentID, 5)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claim want 1 got %d", len(claimed))
	}
	if claimed[0].Status != models.MailboxStatusInFlight {
		t.Fatalf("expected in_flight after claim")
	}

	if _, err := svc.DepositReply(DepositReplyParams{
		AgentID:    agentID,
		CallerFP:   callerFP,
		ReplyTo:    "msg-1",
		SessionID:  "sess-1",
		MessageID:  "reply-1",
		Ciphertext: "cmVwbHk=",
	}); err != nil {
		t.Fatalf("deposit reply: %v", err)
	}

	if _, err := svc.AckInbound(agentID, []string{claimed[0].ID}); err != nil {
		t.Fatalf("ack inbound: %v", err)
	}

	replies, err := svc.ListReplies(agentID, callerFP, time.Time{}, 50)
	if err != nil {
		t.Fatalf("list replies: %v", err)
	}
	if len(replies) != 1 {
		t.Fatalf("replies want 1 got %d", len(replies))
	}

	if _, err := svc.AckReplies(agentID, callerFP, []string{replies[0].ID}); err != nil {
		t.Fatalf("ack replies: %v", err)
	}
	replies2, err := svc.ListReplies(agentID, callerFP, time.Time{}, 50)
	if err != nil {
		t.Fatalf("list after ack: %v", err)
	}
	if len(replies2) != 0 {
		t.Fatalf("after ack want 0 got %d", len(replies2))
	}
}

func TestMailboxRejectsInvalidCallerFP(t *testing.T) {
	svc := setupMailboxTestDB(t)
	_, err := svc.DepositInbound(DepositInboundParams{
		AgentID:    "acp_agent_aabbccdd",
		CallerFP:   "not-hex",
		MessageID:  "m-bad",
		Ciphertext: "dGVzdA==",
	})
	if err != ErrMailboxInvalidFP {
		t.Fatalf("want ErrMailboxInvalidFP got %v", err)
	}
}
