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
	targetID := "acp_agent_aabbccdd"
	callerFP := "fedcba9876543210"

	msg, err := svc.DepositInbound(DepositInboundParams{
		TargetID:   targetID,
		CallerFP:   callerFP,
		MessageID:  "msg-1",
		RequestID:  "req-1",
		SessionID:  "sess-1",
		GroupID:    "psess_group_abc",
		Ciphertext: "dGVzdA==",
	})
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if msg.Status != models.MailboxStatusPending {
		t.Fatalf("expected pending, got %s", msg.Status)
	}
	if msg.RequestID != "req-1" || msg.GroupID != "psess_group_abc" {
		t.Fatalf("metadata not persisted")
	}

	again, err := svc.DepositInbound(DepositInboundParams{
		TargetID:   targetID,
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

	pending, err := svc.PendingCount(targetID)
	if err != nil {
		t.Fatalf("pending count: %v", err)
	}
	if pending != 1 {
		t.Fatalf("pending want 1 got %d", pending)
	}

	claimed, err := svc.ClaimPending(targetID, 5)
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
		TargetID:   targetID,
		CallerFP:   callerFP,
		ReplyTo:    "msg-1",
		RequestID:  "req-1",
		SessionID:  "sess-1",
		GroupID:    "psess_group_abc",
		MessageID:  "reply-1",
		Ciphertext: "cmVwbHk=",
	}); err != nil {
		t.Fatalf("deposit reply: %v", err)
	}

	if _, err := svc.AckInbound(targetID, []string{claimed[0].ID}); err != nil {
		t.Fatalf("ack inbound: %v", err)
	}

	replies, err := svc.ListReplies(targetID, callerFP, time.Time{}, 50)
	if err != nil {
		t.Fatalf("list replies: %v", err)
	}
	if len(replies) != 1 {
		t.Fatalf("replies want 1 got %d", len(replies))
	}

	all, err := svc.ListAllRepliesForCaller(callerFP, time.Time{}, 50)
	if err != nil {
		t.Fatalf("list all replies: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("all replies want 1 got %d", len(all))
	}

	if _, err := svc.AckReplies(targetID, callerFP, []string{replies[0].ID}); err != nil {
		t.Fatalf("ack replies: %v", err)
	}
	replies2, err := svc.ListReplies(targetID, callerFP, time.Time{}, 50)
	if err != nil {
		t.Fatalf("list after ack: %v", err)
	}
	if len(replies2) != 0 {
		t.Fatalf("after ack want 0 got %d", len(replies2))
	}
}

func TestMailboxGroupTargetWithoutRegistration(t *testing.T) {
	svc := setupMailboxTestDB(t)
	groupID := "psess_group_xyz12345"
	callerFP := "fedcba9876543210"

	msg, err := svc.DepositInbound(DepositInboundParams{
		TargetType: models.MailboxTargetGroup,
		TargetID:   groupID,
		CallerFP:   callerFP,
		MessageID:  "msg-g1",
		SessionID:  "sess-g1",
		Ciphertext: "dGVzdA==",
	})
	if err != nil {
		t.Fatalf("group deposit: %v", err)
	}
	if msg.TargetType != models.MailboxTargetGroup {
		t.Fatalf("expected group target type")
	}

	n, err := svc.PendingCount(groupID)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if n != 1 {
		t.Fatalf("pending want 1 got %d", n)
	}
}

func TestMailboxRejectsInvalidCallerFP(t *testing.T) {
	svc := setupMailboxTestDB(t)
	_, err := svc.DepositInbound(DepositInboundParams{
		TargetID:   "acp_agent_aabbccdd",
		CallerFP:   "not-hex",
		MessageID:  "m-bad",
		Ciphertext: "dGVzdA==",
	})
	if err != ErrMailboxInvalidFP {
		t.Fatalf("want ErrMailboxInvalidFP got %v", err)
	}
}

func TestMailboxExpiredRowsDoNotFillQuota(t *testing.T) {
	svc := setupMailboxTestDB(t)
	targetID := "acp_agent_aabbccdd"
	callerFP := "fedcba9876543210"

	expired := &models.MailboxMessage{
		ID:         "expired-1",
		TargetType: models.MailboxTargetAgent,
		TargetID:   targetID,
		AgentID:    targetID,
		Direction:  models.MailboxDirectionInbound,
		Status:     models.MailboxStatusPending,
		Kind:       models.MailboxKindChat,
		CallerFP:   callerFP,
		MessageID:  "old-msg",
		SessionID:  "sess-old",
		Ciphertext: "dGVzdA==",
		SizeBytes:  6,
		ExpiresAt:  time.Now().Add(-time.Hour),
	}
	if err := svc.db.DB.Create(expired).Error; err != nil {
		t.Fatalf("seed expired: %v", err)
	}

	n, err := svc.PurgeExpired()
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("purge want 1 got %d", n)
	}

	msg, err := svc.DepositInbound(DepositInboundParams{
		TargetID:   targetID,
		CallerFP:   callerFP,
		MessageID:  "msg-fresh",
		SessionID:  "sess-fresh",
		Ciphertext: "dGVzdA==",
	})
	if err != nil {
		t.Fatalf("deposit after purge: %v", err)
	}
	if msg.MessageID != "msg-fresh" {
		t.Fatalf("unexpected message %s", msg.MessageID)
	}
}
