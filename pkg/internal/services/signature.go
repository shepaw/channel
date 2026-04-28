package services

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"
)

const (
	// SignatureTimestampTolerance 签名时间戳允许的偏差范围（±5分钟）
	SignatureTimestampTolerance = 5 * time.Minute

	// NonceExpiry nonce 过期时间（略大于 timestamp 窗口，确保窗口内的 nonce 都能被检测到）
	NonceExpiry = 6 * time.Minute
)

// NonceCache 防重放 nonce 缓存（内存实现，适合单实例部署）
// 如果需要多实例部署，可替换为 Redis 实现
type NonceCache struct {
	mu      sync.Mutex
	entries map[string]time.Time // nonce -> expiry time
}

func NewNonceCache() *NonceCache {
	nc := &NonceCache{
		entries: make(map[string]time.Time),
	}
	go nc.cleanupLoop()
	return nc
}

// CheckAndStore 检查 nonce 是否已使用，未使用则存入缓存
// 返回 true 表示 nonce 有效（首次出现），false 表示已被使用（重放）
func (nc *NonceCache) CheckAndStore(nonce string) bool {
	nc.mu.Lock()
	defer nc.mu.Unlock()

	if _, exists := nc.entries[nonce]; exists {
		return false // 已使用，拒绝
	}
	nc.entries[nonce] = time.Now().Add(NonceExpiry)
	return true
}

// cleanupLoop 定期清理过期的 nonce
func (nc *NonceCache) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		nc.mu.Lock()
		now := time.Now()
		for k, expiry := range nc.entries {
			if now.After(expiry) {
				delete(nc.entries, k)
			}
		}
		nc.mu.Unlock()
	}
}

// ── 签名工具函数 ────────────────────────────────────────────────────────────────

// ComputeSignature 使用 HMAC-SHA256 计算签名
// signingString 格式: "{channel_id}\n{timestamp}\n{nonce}"
func ComputeSignature(secret, channelID, timestamp, nonce string) string {
	signingString := fmt.Sprintf("%s\n%s\n%s", channelID, timestamp, nonce)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingString))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature 校验签名是否正确
func VerifySignature(secret, channelID, timestamp, nonce, signature string) bool {
	expected := ComputeSignature(secret, channelID, timestamp, nonce)
	return hmac.Equal([]byte(expected), []byte(signature))
}

// ValidateTimestamp 校验时间戳是否在允许的窗口内
func ValidateTimestamp(timestampStr string) error {
	ts, err := strconv.ParseInt(timestampStr, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid timestamp format")
	}

	now := time.Now().Unix()
	diff := time.Duration(math.Abs(float64(now-ts))) * time.Second

	if diff > SignatureTimestampTolerance {
		return fmt.Errorf("timestamp expired, drift=%s, tolerance=%s", diff, SignatureTimestampTolerance)
	}
	return nil
}

// GenerateNonce 生成一个随机 nonce（16字节 hex，32字符）
func GenerateNonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
