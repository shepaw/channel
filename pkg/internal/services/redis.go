package services

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// memEntry nop 模式下带 TTL 的内存条目
type memEntry struct {
	value     string
	expiresAt time.Time // zero value 表示永不过期
}

func (e memEntry) expired() bool {
	return !e.expiresAt.IsZero() && time.Now().After(e.expiresAt)
}

// RedisService 封装 Redis 操作，支持 Nop 降级（Redis 不可用时用内存 map 保底）
type RedisService struct {
	client *redis.Client
	ctx    context.Context

	// nop 模式
	mu      sync.RWMutex
	memKV   map[string]memEntry
	memInc  map[string]int64
	memIncTTL map[string]time.Time // incr key 的过期时间
}

func NewRedisService(addr, password string, db int) (*RedisService, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})

	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to redis: %v", err)
	}

	return &RedisService{client: client, ctx: ctx}, nil
}

// NewNopRedisService 返回内存降级版（支持 TTL + 定期 GC）
func NewNopRedisService() *RedisService {
	svc := &RedisService{
		ctx:       context.Background(),
		memKV:     make(map[string]memEntry),
		memInc:    make(map[string]int64),
		memIncTTL: make(map[string]time.Time),
	}
	// 后台 GC：每 5 分钟清理过期 key
	go svc.gcLoop()
	return svc
}

func (r *RedisService) gcLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		if r.client != nil {
			return // 切换到真实 Redis 后不需要 GC
		}
		r.mu.Lock()
		now := time.Now()
		for k, e := range r.memKV {
			if !e.expiresAt.IsZero() && now.After(e.expiresAt) {
				delete(r.memKV, k)
			}
		}
		for k, exp := range r.memIncTTL {
			if now.After(exp) {
				delete(r.memInc, k)
				delete(r.memIncTTL, k)
			}
		}
		r.mu.Unlock()
	}
}

func (r *RedisService) isNop() bool { return r.client == nil }

func (r *RedisService) Set(key string, value interface{}, ttl time.Duration) error {
	var val string
	switch v := value.(type) {
	case string:
		val = v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		val = string(data)
	}

	if r.isNop() {
		entry := memEntry{value: val}
		if ttl > 0 {
			entry.expiresAt = time.Now().Add(ttl)
		}
		r.mu.Lock()
		r.memKV[key] = entry
		r.mu.Unlock()
		return nil
	}
	return r.client.Set(r.ctx, key, val, ttl).Err()
}

func (r *RedisService) Get(key string) (string, error) {
	if r.isNop() {
		r.mu.RLock()
		e, ok := r.memKV[key]
		r.mu.RUnlock()
		if !ok || e.expired() {
			if ok {
				// 惰性删除
				r.mu.Lock()
				delete(r.memKV, key)
				r.mu.Unlock()
			}
			return "", fmt.Errorf("key not found: %s", key)
		}
		return e.value, nil
	}
	val, err := r.client.Get(r.ctx, key).Result()
	if err == redis.Nil {
		return "", fmt.Errorf("key not found: %s", key)
	}
	return val, err
}

func (r *RedisService) GetStruct(key string, dest interface{}) error {
	val, err := r.Get(key)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(val), dest)
}

func (r *RedisService) Delete(key string) error {
	if r.isNop() {
		r.mu.Lock()
		delete(r.memKV, key)
		delete(r.memInc, key)
		delete(r.memIncTTL, key)
		r.mu.Unlock()
		return nil
	}
	return r.client.Del(r.ctx, key).Err()
}

func (r *RedisService) Incr(key string) (int64, error) {
	return r.IncrBy(key, 1)
}

func (r *RedisService) IncrBy(key string, value int64) (int64, error) {
	if r.isNop() {
		r.mu.Lock()
		// 惰性过期检查
		if exp, ok := r.memIncTTL[key]; ok && time.Now().After(exp) {
			delete(r.memInc, key)
			delete(r.memIncTTL, key)
		}
		r.memInc[key] += value
		v := r.memInc[key]
		r.mu.Unlock()
		return v, nil
	}
	return r.client.IncrBy(r.ctx, key, value).Result()
}

func (r *RedisService) Expire(key string, ttl time.Duration) error {
	if r.isNop() {
		r.mu.Lock()
		// 同时处理 KV 和 Inc key
		if e, ok := r.memKV[key]; ok {
			e.expiresAt = time.Now().Add(ttl)
			r.memKV[key] = e
		}
		if _, ok := r.memInc[key]; ok {
			r.memIncTTL[key] = time.Now().Add(ttl)
		}
		r.mu.Unlock()
		return nil
	}
	return r.client.Expire(r.ctx, key, ttl).Err()
}

func (r *RedisService) Close() error {
	if r.isNop() {
		return nil
	}
	return r.client.Close()
}
