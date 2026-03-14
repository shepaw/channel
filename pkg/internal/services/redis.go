package services

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisService 封装 Redis 操作，支持 Nop 降级（Redis 不可用时用内存 map 保底）
type RedisService struct {
	client *redis.Client
	ctx    context.Context

	// nop 模式下的内存 kv（TTL 不强制，仅用于基本功能）
	mu      sync.RWMutex
	memKV   map[string]string
	memInc  map[string]int64
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

// NewNopRedisService 返回内存降级版，Redis 不可用时使用
func NewNopRedisService() *RedisService {
	return &RedisService{
		ctx:    context.Background(),
		memKV:  make(map[string]string),
		memInc: make(map[string]int64),
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
		r.mu.Lock()
		r.memKV[key] = val
		r.mu.Unlock()
		return nil
	}
	return r.client.Set(r.ctx, key, val, ttl).Err()
}

func (r *RedisService) Get(key string) (string, error) {
	if r.isNop() {
		r.mu.RLock()
		v, ok := r.memKV[key]
		r.mu.RUnlock()
		if !ok {
			return "", fmt.Errorf("key not found: %s", key)
		}
		return v, nil
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
		r.memInc[key] += value
		v := r.memInc[key]
		r.mu.Unlock()
		return v, nil
	}
	return r.client.IncrBy(r.ctx, key, value).Result()
}

func (r *RedisService) Expire(key string, ttl time.Duration) error {
	if r.isNop() {
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
