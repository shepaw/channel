package services

import (
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/models"
)

// PortAllocator 管理 TCP/UDP 端口分配
type PortAllocator struct {
	mu        sync.Mutex
	startPort int
	endPort   int
	usedPorts map[int]string // port -> channelID
}

func NewPortAllocator(start, end int) *PortAllocator {
	return &PortAllocator{
		startPort: start,
		endPort:   end,
		usedPorts: make(map[int]string),
	}
}

// Allocate 随机分配一个空闲端口
func (p *PortAllocator) Allocate(channelID string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for attempts := 0; attempts < 200; attempts++ {
		port := p.startPort + r.Intn(p.endPort-p.startPort)
		if _, used := p.usedPorts[port]; !used {
			p.usedPorts[port] = channelID
			return port, nil
		}
	}
	return 0, fmt.Errorf("no available ports in range %d-%d", p.startPort, p.endPort)
}

// Reserve 预留指定端口（恢复已有 channel 时使用）
func (p *PortAllocator) Reserve(port int, channelID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.usedPorts[port] = channelID
}

// Release 释放端口
func (p *PortAllocator) Release(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.usedPorts, port)
}

// GetChannelPort 根据 channelID 查找已分配的端口
func (p *PortAllocator) GetChannelPort(channelID string) (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for port, chID := range p.usedPorts {
		if chID == channelID {
			return port, true
		}
	}
	return 0, false
}

// ──────────────────────────────────────────────────────────────────────────────

// TCPUDPStarter 是 ProxyHandler 需要实现的接口，用于启动 TCP/UDP 监听
type TCPUDPStarter interface {
	StartTCPProxy(channelID, target string, listenPort int) error
	StartUDPProxy(channelID, target string, listenPort int) error
}

// ChannelServiceV2 在 ChannelService 基础上增加端口分配能力
type ChannelServiceV2 struct {
	*ChannelService
	portAllocator *PortAllocator
}

func NewChannelServiceV2(db *DatabaseService, redis *RedisService, config *models.Config) *ChannelServiceV2 {
	allocator := NewPortAllocator(config.TCPPortRangeStart, config.TCPPortRangeEnd)
	return &ChannelServiceV2{
		ChannelService: NewChannelService(db, redis, config),
		portAllocator:  allocator,
	}
}

// CreateChannelWithPort 创建 channel，TCP/UDP 额外分配端口
func (v *ChannelServiceV2) CreateChannelWithPort(userID, name, description, channelType, target string, config map[string]interface{}) (*models.Channel, int, error) {
	var allocatedPort int

	if channelType == "tcp" || channelType == "udp" {
		port, err := v.portAllocator.Allocate("")
		if err != nil {
			return nil, 0, err
		}
		allocatedPort = port
	}

	channel, err := v.ChannelService.CreateChannel(userID, name, description, channelType, target, config)
	if err != nil {
		if allocatedPort > 0 {
			v.portAllocator.Release(allocatedPort)
		}
		return nil, 0, err
	}

	// TCP/UDP 更新 endpoint 为真实端口
	if allocatedPort > 0 {
		v.portAllocator.Reserve(allocatedPort, channel.ID)
		endpoint := fmt.Sprintf("%s://%s:%d", channelType, v.ChannelService.config.BaseDomain, allocatedPort)
		v.ChannelService.db.DB.Model(channel).Update("endpoint", endpoint)
		channel.Endpoint = endpoint
	}

	return channel, allocatedPort, nil
}

// GetPortAllocator 暴露 port allocator（供 proxy handler 使用）
func (v *ChannelServiceV2) GetPortAllocator() *PortAllocator {
	return v.portAllocator
}

// RestoreProxies 服务重启后，恢复已有的 TCP/UDP channel 监听器
func (v *ChannelServiceV2) RestoreProxies(starter TCPUDPStarter) error {
	var channels []models.Channel
	if err := v.ChannelService.db.DB.
		Where("(type = 'tcp' OR type = 'udp') AND is_active = true AND deleted_at IS NULL").
		Find(&channels).Error; err != nil {
		return err
	}

	for _, ch := range channels {
		ch := ch // 避免闭包捕获
		var port int
		fmt.Sscanf(ch.Endpoint, "%*[^:]://%*[^:]:%d", &port)
		if port == 0 {
			log.Printf("⚠️  Cannot restore %s channel %s: no port in endpoint %s", ch.Type, ch.ID, ch.Endpoint)
			continue
		}

		v.portAllocator.Reserve(port, ch.ID)

		var err error
		if ch.Type == "tcp" {
			err = starter.StartTCPProxy(ch.ID, ch.Target, port)
		} else {
			err = starter.StartUDPProxy(ch.ID, ch.Target, port)
		}
		if err != nil {
			log.Printf("⚠️  Failed to restore %s proxy for channel %s on port %d: %v", ch.Type, ch.ID, port, err)
		} else {
			log.Printf("✅ Restored %s proxy for channel %s on port %d", ch.Type, ch.ID, port)
		}
	}

	return nil
}
