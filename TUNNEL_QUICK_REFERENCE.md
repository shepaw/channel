# Tunnel Agent Registration - Quick Reference

## Core Files with Line Numbers

### 1. Handlers
**File**: `pkg/internal/handlers/tunnel.go`
- Lines 38-97: `Connect()` - Agent registration endpoint
- Lines 110-145: `RotateSecret()` - Secret rotation (force agent disconnect)
- Lines 99-108: `Status()` - Public tunnel status check
- Lines 148-154: `extractBearerToken()` - Parse Authorization header

### 2. Services
**File**: `pkg/internal/services/tunnel.go`
- Lines 17-30: Message type constants
- Lines 32-43: TunnelMessage struct
- Lines 45-56: TunnelConn struct
- Lines 58-61: TunnelManager struct
- Lines 65-91: `Register()` - Tunnel connection registration
- Lines 100-109: `KickIfSecret()` - Disconnect agent using old secret
- Lines 111-118: `Get()` - Get tunnel connection
- Lines 120-124: `IsOnline()` - Check if tunnel connected
- Lines 126-183: `ForwardHTTP()` - Forward HTTP requests through tunnel
- Lines 192-270: `ForwardWS()` - WebSocket proxying
- Lines 297-352: `readLoop()` - Message reader
- Lines 354-371: `pingLoop()` - Keepalive mechanism

**File**: `pkg/internal/services/channel.go`
- Lines 258-263: `generateChannelSecret()` - Create ch_sec_* format secret
- Lines 265-272: `GetChannelBySecret()` - Lookup channel by secret
- Lines 274-288: `RotateSecret()` - Generate new secret for channel

### 3. Models
**File**: `pkg/internal/models/channel.go`
- Lines 71-85: Channel struct (includes Secret field)
- Lines 49-55: UserChannel struct (channel ownership)

**File**: `pkg/internal/models/user.go`
- Lines 10-22: User struct
- Lines 24-35: EmailVerification struct
- Lines 38-46: AccessToken struct

### 4. Agent Client
**File**: `cmd/agent/main.go`
- Lines 56-73: Agent struct
- Lines 115-144: `connect()` - WebSocket connection to server
- Lines 86-113: `Run()` - Reconnection loop with exponential backoff
- Lines 147-198: `loop()` - Main message handler
- Lines 201-266: `handleRequest()` - HTTP request forwarding
- Lines 284-353: `handleWsProxy()` - WebSocket proxying
- Lines 365-378: `send()` - Thread-safe message transmission

### 5. Routes & Setup
**File**: `pkg/cmd/main.go`
- Lines 48: TunnelManager initialization
- Lines 57: TunnelHandler initialization
- Lines 138-141: `/tunnel/connect` route (no auth middleware)
- Lines 143: `/tunnel/status/:channel_id` route (with auth)
- Lines 130: `/rotate-secret` route (with auth)

### 6. Database
**File**: `pkg/internal/services/database.go`
- Lines 44-52: Auto-migration (creates Channel table with Secret field)

### 7. Middleware
**File**: `pkg/internal/handlers/middleware.go`
- Lines 13-48: AuthMiddleware (token validation, NOT used for tunnel/connect)

---

## Secret Generation

```
Format: ch_sec_<32_hex_chars>
Source: crypto/rand (16 random bytes)
Storage: Indexed in Channel.Secret column (varchar(64))
Lookup: Direct DB query by secret value
```

---

## Authentication Flow

### Agent Registration
1. Agent calls: `GET /tunnel/connect?channel_id=xxx&timestamp=T&nonce=N&signature=HMAC(...)`
2. Server validates:
   - Timestamp within ±5min window (anti-replay)
   - Nonce not previously used (anti-replay)
   - Channel exists by ID, has secret configured
   - HMAC-SHA256 signature matches (secret never transmitted)
   - Channel.Type in {tunnel-http, tunnel-tcp, tunnel-ws}
   - Channel.IsActive == true
3. WebSocket upgrade & register with TunnelManager

### Secret Rotation
1. User calls: `POST /api/v1/channels/:id/rotate-secret`
2. Server validates: User owns channel (via AuthMiddleware token)
3. Generate new secret
4. Update database
5. Kick old agent: `KickIfSecret(channelID, oldSecret)`
6. Return new secret

---

## Message Types

| Type | Direction | Purpose |
|------|-----------|---------|
| `request` | Server→Agent | Forward HTTP request |
| `response` | Agent→Server | HTTP response |
| `ping` | Server→Agent | Keepalive (every 20s) |
| `pong` | Agent→Server | Keepalive response |
| `ws_connect` | Server→Agent | Establish WS to local server |
| `ws_data` | Bidirectional | WebSocket frame data |
| `ws_close` | Bidirectional | WebSocket close |

---

## Reconnection Strategy

```
Backoff: 2s → 4s → 8s → 16s → 32s → 60s (capped)
Reset on success
Max: 60 seconds
```

---

## Keepalive Mechanism

```
Server sends ping: every 20 seconds
Agent responds: pong immediately
Timeout: 60s without pong → close connection
```

---

## Key Data Structures

### TunnelConn
```go
{
    channelID: "uuid",
    secret: "ch_sec_abc123...",
    conn: *websocket.Conn,
    streams: map[int64]chan *TunnelMessage,  // Per-request channels
    lastPingAt: time.Time,
    closed: bool,
    closeCh: chan struct{},
}
```

### TunnelMessage
```go
{
    type: "request|response|ping|pong|ws_connect|ws_data|ws_close",
    stream_id: int64,
    method: "GET|POST|...",
    path: "/path?query=value",
    headers: map[string]string,
    status: int,
    body: "base64encodedData",
    error: "error message",
    ws_msg_type: 1|2,  // websocket frame type
}
```

---

## HTTP Request Timeout
- Client request: 30 seconds
- Agent→local request: 25 seconds
- Tunnel keepalive: 60 seconds (no pong)

---

## Stream ID Allocation
```go
Global atomic counter: globalStreamID
Each request/websocket gets unique ID
Scoped to single tunnel connection
Per-stream response channels for multiplexing
```

---

## Single Tunnel per Channel
- TunnelManager stores: `channelID → TunnelConn`
- Only one active tunnel per channel
- New connection evicts old (LoadAndDelete pattern)
- Prevents duplicate agents

---

## Security Controls

✅ **Implemented**
- Secret format validation
- Database lookup verification
- Ownership check (channel_id matches secret)
- Type validation (tunnel-* only)
- Active status check
- Single tunnel enforcement
- Secret rotation capability

⚠️ **Not Implemented**
- Rate limiting on secret guessing
- Request signing/HMAC
- Mutual TLS authentication
- Automatic secret expiration
- Connection attempt logging
- Brute-force protection

---

## Environment Variables

See `pkg/cmd/main.go` loadConfig():
- `PORT`: Server port
- `BASE_URL`: Base URL for setup command
- `BASE_DOMAIN`: Domain for endpoint generation
- `DATABASE_URL`: DB connection string
- `REDIS_ADDR`: Redis connection
- `ALLOWED_ORIGINS`: CORS origins
- `ALLOWED_DOMAINS`: Hostname whitelist

---

## Testing Checklist

- [ ] Channel creation generates secret with `ch_sec_` prefix
- [ ] Agent connects with valid secret
- [ ] Connection lookup works (TunnelManager.Get)
- [ ] New agent evicts old agent
- [ ] Secret rotation kicks old agent
- [ ] Old secret fails after rotation
- [ ] Ping/pong keepalive works
- [ ] HTTP requests forward correctly
- [ ] WebSocket proxying works
- [ ] Reconnection backoff works
- [ ] Connection timeout (60s no pong) works

