# Tunnel Agent Registration - Complete Index

## 📋 Documentation Files

This analysis consists of three comprehensive documents:

1. **TUNNEL_ANALYSIS.md** - Complete deep dive (16 sections, 350+ lines)
2. **TUNNEL_QUICK_REFERENCE.md** - Quick lookup by component
3. **TUNNEL_ARCHITECTURE.txt** - Visual ASCII diagrams

## 📂 File Organization by Topic

### Core Authentication Files

| File | Lines | Topic |
|------|-------|-------|
| `pkg/internal/handlers/tunnel.go` | 38-97 | Agent registration endpoint |
| `pkg/internal/handlers/tunnel.go` | 110-145 | Secret rotation endpoint |
| `pkg/internal/services/channel.go` | 258-263 | Secret generation |
| `pkg/internal/services/channel.go` | 265-272 | Secret lookup |
| `pkg/internal/services/channel.go` | 274-288 | Secret rotation service |

### Tunnel Connection Management

| File | Lines | Topic |
|------|-------|-------|
| `pkg/internal/services/tunnel.go` | 17-30 | Message type constants |
| `pkg/internal/services/tunnel.go` | 32-43 | TunnelMessage struct |
| `pkg/internal/services/tunnel.go` | 45-56 | TunnelConn struct |
| `pkg/internal/services/tunnel.go` | 58-61 | TunnelManager struct |
| `pkg/internal/services/tunnel.go` | 65-91 | Register connection |
| `pkg/internal/services/tunnel.go` | 100-109 | Kick connection |
| `pkg/internal/services/tunnel.go` | 126-183 | HTTP forwarding |
| `pkg/internal/services/tunnel.go` | 192-270 | WebSocket proxying |
| `pkg/internal/services/tunnel.go` | 297-352 | Message reader loop |
| `pkg/internal/services/tunnel.go` | 354-371 | Keepalive loop |

### Data Models

| File | Lines | Topic |
|------|-------|-------|
| `pkg/internal/models/channel.go` | 71-85 | Channel model |
| `pkg/internal/models/channel.go` | 49-55 | UserChannel (ownership) |
| `pkg/internal/models/user.go` | 10-22 | User model |
| `pkg/internal/models/user.go` | 24-35 | EmailVerification model |
| `pkg/internal/models/user.go` | 38-46 | AccessToken model |
| `pkg/internal/models/config.go` | 1-78 | Config struct |

### Agent Client Implementation

| File | Lines | Topic |
|------|-------|-------|
| `cmd/agent/main.go` | 56-73 | Agent struct |
| `cmd/agent/main.go` | 86-113 | Reconnection loop |
| `cmd/agent/main.go` | 115-144 | WebSocket connection |
| `cmd/agent/main.go` | 147-198 | Message handler loop |
| `cmd/agent/main.go` | 201-266 | HTTP request forwarding |
| `cmd/agent/main.go` | 284-353 | WebSocket proxy |
| `cmd/agent/main.go` | 365-378 | Message transmission |
| `cmd/agent/main.go` | 410-445 | Entry point & CLI |

### Infrastructure

| File | Lines | Topic |
|------|-------|-------|
| `pkg/cmd/main.go` | 48 | TunnelManager initialization |
| `pkg/cmd/main.go` | 57 | TunnelHandler initialization |
| `pkg/cmd/main.go` | 138-141 | /tunnel/connect route |
| `pkg/cmd/main.go` | 143 | /tunnel/status route |
| `pkg/cmd/main.go` | 130 | /rotate-secret route |
| `pkg/internal/services/database.go` | 44-52 | Database auto-migration |
| `pkg/internal/handlers/middleware.go` | 13-48 | Auth middleware |

## 🔐 Security Model

### Authentication Layers

```
Layer 1: Channel Secret (Permanent)
├─ Format: ch_sec_<32hex>
├─ Storage: Plaintext in DB (indexed)
├─ Lookup: Direct DB query
├─ Transmission: Query param or Bearer header
└─ Used for: Agent registration

Layer 2: User Token (Temporary)
├─ Format: Base64
├─ Storage: SHA-256 hash in DB
├─ Expiration: 15 minutes (default)
├─ Transmission: Authorization header
└─ Used for: Channel management, secret rotation

Layer 3: Channel Ownership
├─ Verified via: UserChannel join
├─ Required for: Secret rotation, channel deletion
└─ Checked at: rotate-secret endpoint
```

### Secret Format Validation

```go
// Secret must start with "ch_sec_"
if !strings.HasPrefix(secret, "ch_sec_") {
    return 401 "invalid secret format"
}

// Generate: ch_sec_ + 16 random bytes (hex encoded = 32 chars)
func generateChannelSecret() string {
    b := make([]byte, 16)
    rand.Read(b)  // crypto/rand
    return "ch_sec_" + hex.EncodeToString(b)
}
```

## 🔄 Flow Sequences

### Registration Flow (Phase 1-2)

**User creates tunnel channel:**
```
POST /api/v1/channels {type: "tunnel-http"}
  ↓ [User token auth]
  ↓ generateChannelSecret()
  ↓ Save to DB with indexed secret
  ↓ Return: {channel_id, secret, setup_command}
```

**Agent connects:**
```
GET /tunnel/connect?channel_id=xxx&timestamp=T&nonce=N&signature=HMAC(...)
  ↓ [Timestamp & nonce validation]
  ↓ [DB lookup by channel_id]
  ↓ [HMAC-SHA256 signature verification]
  ↓ [Type & active check]
  ↓ [WebSocket upgrade]
  ↓ tunnelMgr.Register() [evicts old agent if exists]
  ✓ Connection established
```

### Secret Rotation Flow (Phase 4)

**User rotates secret:**
```
POST /api/v1/channels/{id}/rotate-secret
  ↓ [User token auth + ownership check]
  ↓ generateChannelSecret() → new secret
  ↓ Update DB
  ↓ tunnelMgr.KickIfSecret(old_secret)
  ↓ Close old agent's connection
  ✓ Old agent gets 401 on reconnect
```

### HTTP Request Flow

**Client → Server:**
```
GET /proxy/{channel_id}/path
  ↓ ProxyHandler
  ├─ tunnelMgr.ForwardHTTP()
  ├─ Allocate streamID
  ├─ Create response channel
  └─ Send TunnelMessage("request")
```

**Server → Agent (over tunnel):**
```
Receive MsgRequest
  ↓ handleRequest()
  ├─ Forward to http://localhost:3000/path
  ├─ Read response
  └─ Send MsgResponse with streamID
```

**Agent → Server (over tunnel):**
```
TunnelManager.readLoop()
  ↓ Unmarshal MsgResponse
  ├─ Look up streams[streamID]
  └─ Send to channel → ForwardHTTP() returns
```

**Server → Client:**
```
ProxyHandler gets response
  ├─ Decode body
  └─ Write HTTP response
```

### Reconnection Flow (Phase 3)

**Agent disconnects:**
```
loop() exits → Run() catches
  ↓ Backoff = 2s
  ↓ Wait 2s
  ↓ Try connect() again
  ├─ Success: Backoff reset to 2s
  └─ Failure: Backoff *= 2 (max 60s)
```

## 📊 Data Structures

### TunnelConn (In-Memory)
```go
type TunnelConn struct {
    channelID  string                               // e.g. "abc-123"
    secret     string                               // e.g. "ch_sec_..."
    conn       *websocket.Conn                      // TCP connection
    streams    map[int64]chan *TunnelMessage        // Per-request channels
    lastPingAt time.Time                            // Keepalive tracking
    closed     bool                                 // Closed flag
    closeCh    chan struct{}                        // Close signal
}
```

### TunnelMessage (JSON over WebSocket)
```json
{
  "type": "request|response|ping|pong|ws_connect|ws_data|ws_close",
  "stream_id": 12345,
  "method": "GET|POST",
  "path": "/api/path?query=value",
  "headers": {"key": "value"},
  "status": 200,
  "body": "base64encodedData",
  "error": "error message",
  "ws_msg_type": 1
}
```

### Channel (Database)
```sql
CREATE TABLE channels (
  id          VARCHAR(36) PRIMARY KEY,
  type        VARCHAR(32),              -- tunnel-http|tunnel-tcp|tunnel-ws
  secret      VARCHAR(64) INDEXED,      -- ch_sec_<32hex>
  is_active   BOOLEAN,
  target      VARCHAR(255),             -- Optional for tunnel types
  created_at  TIMESTAMP,
  updated_at  TIMESTAMP,
  deleted_at  TIMESTAMP                 -- Soft delete
);
```

## ⏱️ Timeouts & Intervals

| Parameter | Value | Location |
|-----------|-------|----------|
| Ping interval | 20s | tunnel.go:355 |
| Pong timeout | 60s | tunnel.go:362 |
| HTTP request | 30s | tunnel.go:178 |
| Agent request | 25s | agent/main.go:231 |
| WebSocket handshake | 10s | agent/main.go:132 |
| Reconnect backoff | 2s-60s | agent/main.go:87-110 |

## 🔍 Key Functions Index

### Handler Layer
- `TunnelHandler.Connect()` - Agent registration
- `TunnelHandler.RotateSecret()` - Secret rotation
- `TunnelHandler.Status()` - Tunnel status check

### Manager Layer
- `TunnelManager.Register()` - Register connection
- `TunnelManager.Get()` - Get connection
- `TunnelManager.IsOnline()` - Check status
- `TunnelManager.KickIfSecret()` - Disconnect by secret
- `TunnelManager.ForwardHTTP()` - Forward HTTP request
- `TunnelManager.ForwardWS()` - Forward WebSocket
- `TunnelConn.readLoop()` - Message reader
- `TunnelConn.pingLoop()` - Keepalive sender

### Service Layer
- `ChannelService.generateChannelSecret()` - Generate secret
- `ChannelService.GetChannelBySecret()` - Look up channel
- `ChannelService.RotateSecret()` - Rotate secret
- `AuthService.ValidateToken()` - Validate user token
- `AuthService.hashToken()` - Hash token for storage

### Agent Layer
- `Agent.Run()` - Main loop with reconnect
- `Agent.connect()` - WebSocket dial
- `Agent.loop()` - Message handler
- `Agent.handleRequest()` - HTTP forwarding
- `Agent.handleWsProxy()` - WebSocket proxy
- `Agent.send()` - Thread-safe transmission

## 📈 Concurrency Model

### Server Side
```
Main goroutines per tunnel:
  - readLoop()     → Reads messages, routes to streams
  - pingLoop()     → Sends keepalive pings
  - ForwardHTTP()  → Waits on stream channel (30s timeout)
  - ForwardWS()    → Bidirectional proxy (two goroutines)

Synchronization:
  - TunnelManager.tunnels: sync.Map (thread-safe)
  - TunnelConn.streams: sync.RWMutex
  - Per-stream channels: buffered or blocking
```

### Client Side
```
Main goroutines per agent:
  - Run()          → Reconnection loop
  - loop()         → Message handler
  - handleRequest() → HTTP forwarding (concurrent)
  - handleWsProxy() → WebSocket proxy (two goroutines)

Synchronization:
  - Agent.conn: sync.Mutex
  - Agent.streams: sync.RWMutex
```

## 🛡️ Security Gaps & Recommendations

### Gaps Identified
- ❌ No rate limiting on secret guessing
- ❌ No brute-force protection
- ❌ No automatic secret rotation
- ❌ No connection attempt logging
- ❌ No HMAC/signature verification
- ❌ No mutual TLS authentication
- ❌ No IP whitelist support

### Strengths
- ✅ Secret format validation
- ✅ Database verification
- ✅ Ownership checks
- ✅ Single tunnel enforcement
- ✅ Immediate rotation capability
- ✅ TLS encryption (WSS/HTTPS)

## 📚 Related Files (Not Core)

| File | Purpose |
|------|---------|
| `pkg/internal/handlers/channel.go` | Channel CRUD |
| `pkg/internal/handlers/proxy.go` | HTTP proxy dispatch |
| `pkg/internal/handlers/middleware.go` | Auth middleware |
| `pkg/internal/handlers/auth.go` | User auth |
| `pkg/internal/services/auth.go` | Token & email auth |
| `pkg/internal/services/database.go` | DB connection |
| `pkg/internal/services/redis.go` | Caching |

## 🧪 Testing Checklist

### Agent Registration
- [ ] Connect with valid secret → Success
- [ ] Connect with invalid secret → 401
- [ ] Connect with wrong channel_id → 401
- [ ] Secret format validation → Works
- [ ] Database lookup → Works
- [ ] Channel type validation → Enforced
- [ ] Active status check → Enforced

### Tunnel Operations
- [ ] Single tunnel per channel enforced
- [ ] New agent evicts old agent
- [ ] HTTP request forwarding → Works
- [ ] WebSocket proxying → Works
- [ ] Request multiplexing → Works
- [ ] Concurrent requests → Works

### Secret Rotation
- [ ] Generate new secret → Works
- [ ] Old agent disconnected → Yes
- [ ] Old secret fails → 401
- [ ] New secret works → Yes

### Keepalive
- [ ] Ping every 20s → Works
- [ ] Pong received → Timeout reset
- [ ] 60s no pong → Disconnect
- [ ] Connection timeout → Graceful close

### Reconnection
- [ ] Backoff starts 2s → Works
- [ ] Backoff doubles → Works
- [ ] Max 60s → Enforced
- [ ] Reset on success → Works

---

**Last Updated**: 2026-04-26  
**Total Files Analyzed**: 11  
**Total Lines Covered**: 1,000+  
**Documentation Pages**: 4 (this index + 3 detailed docs)
