package services

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupAccessTest(t *testing.T) (*AccessService, *AgentService, string, string) {
	t.Helper()
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.UserChannel{}, &models.Channel{}, &models.Agent{}, &models.AgentAccessGrant{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now()
	_ = db.Create(&models.User{ID: "u1", Email: "a@b.c", Name: "A", Provider: "email", ProviderID: "a@b.c"}).Error
	_ = db.Create(&models.Channel{ID: "ch1", Name: "hub", Type: "tunnel-http", Endpoint: "http://x", Secret: "sec", IsActive: true}).Error
	_ = db.Create(&models.UserChannel{ID: "uc1", UserID: "u1", ChannelID: "ch1"}).Error
	_ = db.Create(&models.Agent{
		AgentID: "acp_agent_aabbccdd", ChannelID: "ch1", UserID: "u1", Name: "Bot",
		IsPublic: true, Capacity: 5, AgentFP: "0123456789abcdef", AgentPubKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		LastSeenAt: now,
	}).Error

	dbs := &DatabaseService{DB: db}
	agentSvc := NewAgentService(dbs)
	return NewAccessService(dbs, agentSvc), agentSvc, "acp_agent_aabbccdd", "u1"
}

func mustKeyPair(t *testing.T) (fp, pubB64 string) {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8]), base64.StdEncoding.EncodeToString(raw)
}

func TestAccessRequestApproveSync(t *testing.T) {
	svc, _, agentID, owner := setupAccessTest(t)
	fp, pub := mustKeyPair(t)

	g, err := svc.Request(RequestAccessParams{
		AgentID: agentID, CallerFP: fp, CallerPubKey: pub, CallerName: "phone", Message: "hi",
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if g.Status != models.AccessPending {
		t.Fatalf("want pending got %s", g.Status)
	}

	approved, err := svc.Decide(owner, g.ID, models.AccessApproved)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.Status != models.AccessApproved {
		t.Fatalf("want approved")
	}

	sync, err := svc.ListForAgentSync(agentID, time.Time{})
	if err != nil || len(sync) != 1 {
		t.Fatalf("sync: %v len=%d", err, len(sync))
	}
	if sync[0].CallerPubKey != pub {
		t.Fatalf("pubkey mismatch")
	}

	mine, err := svc.GetMine(agentID, fp)
	if err != nil || mine.Status != models.AccessApproved {
		t.Fatalf("mine: %v %#v", err, mine)
	}

	if _, err := svc.Decide(owner, g.ID, models.AccessRevoked); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	sync2, _ := svc.ListForAgentSync(agentID, time.Time{})
	if len(sync2) != 1 || sync2[0].Status != models.AccessRevoked {
		t.Fatalf("want revoked in sync, got %#v", sync2)
	}
}

func TestAccessRejectsPrivateAgent(t *testing.T) {
	svc, agentSvc, _, _ := setupAccessTest(t)
	priv := false
	name := "x"
	_, _ = agentSvc.Update("u1", "acp_agent_aabbccdd", UpdateAgentParams{IsPublic: &priv, Name: &name})
	fp, pub := mustKeyPair(t)
	_, err := svc.Request(RequestAccessParams{AgentID: "acp_agent_aabbccdd", CallerFP: fp, CallerPubKey: pub})
	if err != ErrAccessNotPublic {
		t.Fatalf("want ErrAccessNotPublic got %v", err)
	}
}

func TestBuildAgentWSEndpoint(t *testing.T) {
	got := BuildAgentWSEndpoint("https://ch.example.com", "ch1", "/p/inst1/", "acp_agent_aabbccdd")
	want := "wss://ch.example.com/proxy/ch1/p/inst1/acp/ws?agentId=acp_agent_aabbccdd"
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	got2 := BuildAgentWSEndpoint("http://localhost:8080", "ch1", "", "acp_agent_aabbccdd")
	want2 := "ws://localhost:8080/proxy/ch1/acp/ws?agentId=acp_agent_aabbccdd"
	if got2 != want2 {
		t.Fatalf("got %s want %s", got2, want2)
	}
}
