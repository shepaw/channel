package services

import (
	"testing"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupAgentTest(t *testing.T) *AgentService {
	t.Helper()
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.UserChannel{}, &models.Channel{}, &models.Agent{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = db.Create(&models.User{ID: "u1", Email: "a@b.c", Name: "A", Provider: "email", ProviderID: "a@b.c"}).Error
	_ = db.Create(&models.User{ID: "u2", Email: "c@d.e", Name: "B", Provider: "email", ProviderID: "c@d.e"}).Error
	_ = db.Create(&models.Channel{ID: "ch-old", Name: "old", Type: "tunnel-http", Endpoint: "https://x/proxy/ch-old", Secret: "sec-old", IsActive: true}).Error
	_ = db.Create(&models.Channel{ID: "ch-new", Name: "new", Type: "tunnel-http", Endpoint: "https://x/proxy/ch-new", Secret: "sec-new", IsActive: true}).Error
	_ = db.Create(&models.Channel{ID: "ch-other", Name: "other", Type: "tunnel-http", Endpoint: "https://x/proxy/ch-other", Secret: "sec-o", IsActive: true}).Error
	_ = db.Create(&models.UserChannel{ID: "uc1", UserID: "u1", ChannelID: "ch-old"}).Error
	_ = db.Create(&models.UserChannel{ID: "uc2", UserID: "u1", ChannelID: "ch-new"}).Error
	_ = db.Create(&models.UserChannel{ID: "uc3", UserID: "u2", ChannelID: "ch-other"}).Error
	return NewAgentService(&DatabaseService{DB: db})
}

func TestRegisterSameOwnerRebindsChannel(t *testing.T) {
	svc := setupAgentTest(t)
	first, err := svc.Register(RegisterAgentParams{
		AgentID:   "acp_agent_aabbccdd",
		ChannelID: "ch-old",
		Name:      "Bot",
		AgentFP:   "0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("first register: %v", err)
	}
	if first.ChannelID != "ch-old" {
		t.Fatalf("want ch-old, got %s", first.ChannelID)
	}

	moved, err := svc.Register(RegisterAgentParams{
		AgentID:   "acp_agent_aabbccdd",
		ChannelID: "ch-new",
		Name:      "Bot",
	})
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if moved.ChannelID != "ch-new" {
		t.Fatalf("want ch-new, got %s", moved.ChannelID)
	}

	listed, err := svc.ListByChannel("ch-new")
	if err != nil || len(listed) != 1 {
		t.Fatalf("new channel list: %v len=%d", err, len(listed))
	}
	oldList, err := svc.ListByChannel("ch-old")
	if err != nil || len(oldList) != 0 {
		t.Fatalf("old channel should be empty, len=%d err=%v", len(oldList), err)
	}
}

func TestRegisterOtherOwnerRejected(t *testing.T) {
	svc := setupAgentTest(t)
	if _, err := svc.Register(RegisterAgentParams{
		AgentID: "acp_agent_aabbccdd", ChannelID: "ch-old", Name: "Bot",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := svc.Register(RegisterAgentParams{
		AgentID: "acp_agent_aabbccdd", ChannelID: "ch-other", Name: "Bot",
	})
	if err != ErrAgentChannelBound {
		t.Fatalf("want ErrAgentChannelBound, got %v", err)
	}
}

func TestAgentReachableRequiresHeartbeatAndTunnel(t *testing.T) {
	fresh := &models.Agent{ChannelID: "ch-new", LastSeenAt: time.Now()}
	stale := &models.Agent{ChannelID: "ch-new", LastSeenAt: time.Now().Add(-10 * time.Minute)}

	if AgentReachable(nil, nil) {
		t.Fatal("nil agent should be offline")
	}
	if !AgentReachable(fresh, nil) {
		t.Fatal("fresh heartbeat without tunnel mgr should be online")
	}
	if AgentReachable(stale, nil) {
		t.Fatal("stale heartbeat should be offline")
	}

	tm := NewTunnelManager()
	if AgentReachable(fresh, tm) {
		t.Fatal("fresh heartbeat but no tunnel should be offline")
	}
	tm.tunnels.Store("ch-new", &connSet{conns: map[string]*TunnelConn{
		"dev": {closed: false},
	}})
	if !AgentReachable(fresh, tm) {
		t.Fatal("fresh heartbeat + live tunnel should be online")
	}
	if AgentReachable(stale, tm) {
		t.Fatal("stale heartbeat + live tunnel should still be offline")
	}
}
