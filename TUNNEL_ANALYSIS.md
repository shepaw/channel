# Tunnel Agent Registration Flow Analysis

**Directory**: `/Users/edenzou/workspace/shepaw/channel`

---

## Executive Summary

The tunnel agent registration flow is a **permanent channel secret-based authentication system** that allows local agents to establish persistent reverse-tunnel WebSocket connections to the channel service. The flow uses:

1. **Permanent Channel Secrets** (`ch_sec_*` format, 32 hex chars) - generated at tunnel channel creation
2. **No temporary tokens** - the secret is reused for all connections
3. **Secret rotation capability** - immediately kicks out old agents
4. **Stream-based multiplexing** - multiple HTTP/WebSocket streams over single tunnel connection
5. **Automatic reconnection** - agents auto-reconnect with exponential backoff on disconnect

---

## 1. TUNNEL HANDLER LAYER

**File**: `/Users/edenzou/workspace/shepaw/channel/pkg/internal/handlers/tunnel.go`

### Handler Type
```go
type TunnelHandler struct {
    tunnelMgr  *services.TunnelManager
    channelSvc *services.ChannelService
    authSvc    *services.AuthService
}
```

### Key Endpoint: `Connect` (Lines 38-97)

**Route**: `GET /tunnel/connect?channel_id=xxx&timestamp=T&nonce=N&signature=HMAC-SHA256(...)`

**Authentication Method**: HMAC-SHA256 Signature (secret never transmitted)

**Flow**:
1. Extract `channel_id` from query param
2. Extract `secret` from:
   - Query param: `?secret=...`
   - OR Authorization header: `Bearer ch_sec_...`
3. Validate secret format: Must start with `ch_sec_` (prevents token misuse)
4. Lookup channel by secret: `channelSvc.GetChannelBySecret(secret)`
5. Verify ownership: `channel.ID == channelID`
6. Verify channel type: Must be `tunnel-http`, `tunnel-tcp`, or `tunnel-ws`
7. Verify active status: `channel.IsActive == true`
8. WebSocket upgrade using gorilla/websocket
9. Register tunnel: `tunnelMgr.Register(channelID, secret, conn)`

**Security Controls**:
- Secret format validation (ch_sec_ prefix)
- Ownership verification (channel_id + secret match)
- Channel type validation
- Active status check
- WebSocket origin not restricted (agents can come from any origin)

### Key Endpoint: `RotateSecret` (Lines 110-145)

**Route**: `POST /api/v1/channels/:id/rotate-secret`

**Authentication**: User token (temporary access token)

**Flow**:
1. Require user authentication via token
2. Fetch old channel
3. Generate new secret: `channelSvc.RotateSecret(userID, channelID)`
4. Kick out old agent: `tunnelMgr.KickIfSecret(channelID, oldSecret)`
5. Return new secret

**Key Feature**: Old agent using old secret is IMMEDIATELY disconnected

### Key Endpoint: `Status` (Lines 99-108)

**Route**: `GET /tunnel/status/:channel_id`

**Authentication**: None (public endpoint)

**Function**: Returns `{channel_id, online: bool}`

---

## 2. TUNNEL SERVICE/MANAGER LAYER

**File**: `/Users/edenzou/workspace/shepaw/channel/pkg/internal/services/tunnel.go`

### Core Data Structures

#### TunnelMessageType (Lines 17-30)
```go
const (
    TunnelMsgRequest   = "request"
    TunnelMsgResponse  = "response"
    TunnelMsgPing      = "ping"
    TunnelMsgPong      = "pong"
    TunnelMsgData      = "data"
    TunnelMsgClose     = "close"
    TunnelMsgWsConnect = "ws_connect"  // server→agent
    TunnelMsgWsData    = "ws_data"     // bidirectional
    TunnelMsgWsClose   = "ws_close"    // bidirectional
)
```

#### TunnelMessage (Lines 32-43)
```go
type TunnelMessage struct {
    Type      TunnelMessageType `json:"type"`
    StreamID  int64             `json:"stream_id,omitempty"`
    Method    string            `json:"method,omitempty"`     // HTTP method
    Path      string            `json:"path,omitempty"`       // Full path + query
    Headers   map[string]string `json:"headers,omitempty"`
    Status    int               `json:"status,omitempty"`     // HTTP status
    Body      string            `json:"body,omitempty"`       // base64 encoded
    Error     string            `json:"error,omitempty"`
    WsMsgType int               `json:"ws_msg_type,omitempty"` // websocket frame type
}
```

#### TunnelConn (Lines 45-56)
```go
type TunnelConn struct {
    channelID  string
    secret     string              // Connection's secret (for rotation check)
    conn       *websocket.Conn
    mu         sync.Mutex
    streams    map[int64]chan *TunnelMessage  // stream_id → response channel
    streamsMu  sync.RWMutex
    lastPingAt time.Time           // For keepalive timeout
    closed     bool
    closeCh    chan struct{}
}
```

#### TunnelManager (Lines 58-61)
```go
type TunnelManager struct {
    tunnels sync.Map // channelID → *TunnelConn
}
```

### Manager Methods

#### Register (Lines 69-91)
```go
func (tm *TunnelManager) Register(channelID, secret string, conn *websocket.Conn) *TunnelConn
```
- Creates new TunnelConn with provided secret
- **Kicks out old connection** if exists (LoadAndDelete pattern)
- Starts readLoop goroutine
- Starts pingLoop goroutine

**Key Behavior**: Only ONE tunnel per channelID; new connection evicts old

#### KickIfSecret (Lines 100-109)
```go
func (tm *TunnelManager) KickIfSecret(channelID, oldSecret string)
```
- Checks if current tunnel's secret matches oldSecret
- If yes: deletes from map and closes connection
- If no: does nothing (new agent already connected with new secret)

**Used by**: Secret rotation flow

#### Get (Lines 111-118)
```go
func (tm *TunnelManager) Get(channelID string) (*TunnelConn, bool)
```
- Returns tunnel connection if exists

#### IsOnline (Lines 120-124)
```go
func (tm *TunnelManager) IsOnline(channelID string) bool
```
- Simple check: tunnel exists in map

#### ForwardHTTP (Lines 126-183)
```go
func (tm *TunnelManager) ForwardHTTP(channelID string, r *http.Request) (*TunnelMessage, error)
```

**Flow**:
1. Get tunnel connection
2. Read request body (max 32MB)
3. Build path with query string
4. Simplify headers (first value only)
5. Allocate new streamID (atomic counter)
6. Create response channel
7. Send TunnelMsgRequest to agent
8. Wait for response (30s timeout)

**Timeouts**:
- HTTP request timeout: 30 seconds
- If tunnel closes: returns error

#### ForwardWS (Lines 192-270)
```go
func (tm *TunnelManager) ForwardWS(channelID string, streamID int64, originalPath string, headers map[string]string, clientConn *websocket.Conn) error
```

**Flow**:
1. Get tunnel connection
2. Create bidirectional data channel (buffered)
3. Send TunnelMsgWsConnect to agent with path + headers
4. Start client→agent goroutine (reads from client, sends WsData frames)
5. Start agent→client direction (receives WsData/WsClose from dataCh)
6. Explicitly send Close frame to client on disconnect (prevents "loading" UX bug)

### Internal Methods

#### readLoop (Lines 297-352)
- Continuously reads from WebSocket
- Routes messages by type:
  - **Pong**: Updates lastPingAt
  - **Response**: Sends to stream's response channel
  - **WsData/WsClose**: Routes to stream's data channel

#### pingLoop (Lines 354-371)
- Sends ping every 20 seconds
- **Timeout**: If no pong received for 60 seconds, closes connection

---

## 3. CHANNEL SERVICE LAYER

**File**: `/Users/edenzou/workspace/shepaw/channel/pkg/internal/services/channel.go`

### Secret Generation (Lines 258-263)

```go
func generateChannelSecret() string {
    b := make([]byte, 16)
    rand.Read(b)
    return "ch_sec_" + hex.EncodeToString(b)
}
```

**Format**: `ch_sec_` + 32 hex characters (from 16 random bytes)

**Generation Point**: When creating tunnel-type channels (Line 58-60)

```go
var secret string
if strings.HasPrefix(channelType, "tunnel-") {
    secret = generateChannelSecret()
}
```

### GetChannelBySecret (Lines 265-272)

```go
func (c *ChannelService) GetChannelBySecret(secret string) (*models.Channel, error) {
    var channel models.Channel
    if err := c.db.DB.Where("secret = ? AND deleted_at IS NULL", secret).First(&channel).Error; err != nil {
        return nil, err
    }
    return &channel, nil
}
```

**Database Query**: Direct lookup by secret field

### RotateSecret (Lines 274-288)

```go
func (c *ChannelService) RotateSecret(userID, channelID string) (string, error) {
    if !c.IsOwner(userID, channelID) {
        return "", ErrNotChannelOwner
    }
    newSecret := generateChannelSecret()
    if err := c.db.DB.Model(&models.Channel{}).
        Where("id = ?", channelID).
        Update("secret", newSecret).Error; err != nil {
        return "", err
    }
    c.redis.Delete(fmt.Sprintf("channel:%s", channelID))
    return newSecret, nil
}
```

**Steps**:
1. Verify user owns channel
2. Generate new secret
3. Update in database
4. Clear Redis cache

**Coordination**: Caller (TunnelHandler) calls `KickIfSecret` after this

---

## 4. CHANNEL MODEL

**File**: `/Users/edenzou/workspace/shepaw/channel/pkg/internal/models/channel.go`

### Channel Structure (Lines 71-85)

```go
type Channel struct {
    ID          string         `gorm:"primaryKey;type:varchar(36)"  json:"id"`
    Name        string         `json:"name"`
    Description string         `json:"description"`
    Type        string         `json:"type"`     // tunnel-http | tunnel-tcp | tunnel-ws
    Target      string         `json:"target"`   // For tunnel types, optional (set by agent)
    Endpoint    string         `gorm:"uniqueIndex;not null" json:"endpoint"`
    Secret      string         `gorm:"type:varchar(64);index" json:"secret,omitempty"` // Tunnel secret
    IsActive    bool           `json:"is_active"`
    Config      JSONMap        `gorm:"type:text"    json:"config"`
    Stats       ChannelStats   `gorm:"type:text"    json:"stats"`
    CreatedAt   time.Time      `json:"created_at"`
    UpdatedAt   time.Time      `json:"updated_at"`
    DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`
}
```

**Key Fields**:
- `Secret`: Indexed for fast lookup
- `Type`: Must be one of tunnel-* types
- `IsActive`: Must be true for tunnel connect
- `Target`: Optional for tunnel types (can be set by agent via config)

### UserChannel (Lines 49-55)
```go
type UserChannel struct {
    ID        string         `gorm:"primaryKey;type:varchar(36)" json:"id"`
    UserID    string         `gorm:"type:varchar(36);index"      json:"user_id"`
    ChannelID string         `gorm:"type:varchar(36);index"      json:"channel_id"`
    CreatedAt time.Time      `json:"created_at"`
    DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}
```

**Purpose**: Many-to-many relationship (users can share channels)

---

## 5. DATABASE & SCHEMA

**File**: `/Users/edenzou/workspace/shepaw/channel/pkg/internal/services/database.go`

### Auto-Migration (Lines 44-52)
```go
err = db.AutoMigrate(
    &models.User{},
    &models.AccessToken{},
    &models.Channel{},          // Includes Secret field
    &models.UserChannel{},
    &models.RateLimitRule{},
    &models.EmailVerification{},
    &models.AppVersion{},
)
```

**Index on Channel.Secret** (from model):
```go
Secret string `gorm:"type:varchar(64);index" json:"secret,omitempty"`
```

---

## 6. AUTHENTICATION & MIDDLEWARE

**File**: `/Users/edenzou/workspace/shepaw/channel/pkg/internal/handlers/middleware.go`

### AuthMiddleware (Lines 13-48)
- Validates temporary access tokens (SHA-256 hashes)
- NOT used for tunnel/connect endpoint
- Used for rotate-secret, channel management, etc.

### Key Point
- `/tunnel/connect` does NOT use AuthMiddleware
- Instead, it uses channel secret directly

---

## 7. ROUTES & SETUP

**File**: `/Users/edenzou/workspace/shepaw/channel/pkg/cmd/main.go`

### Tunnel Routes (Lines 135-156)

```go
// Raw tunnel connection (no auth middleware)
r.GET("/tunnel/connect", func(c *gin.Context) {
    c.Abort() // Prevent Gin from writing response after hijack
    tunnelHandler.Connect(c)
})

// Tunnel status (protected with auth middleware)
r.GET("/tunnel/status/:channel_id", 
    handlers.AuthMiddleware(authSvc), 
    tunnelHandler.Status)

// Secret rotation (protected)
ch.POST("/:id/rotate-secret", tunnelHandler.RotateSecret)
```

### Manager Initialization (Lines 48, 57, 61)

```go
tunnelMgr := services.NewTunnelManager()
tunnelHandler := handlers.NewTunnelHandler(tunnelMgr, channelSvcV2.ChannelService, authSvc)
proxyHandler.SetTunnelManager(tunnelMgr)
```

---

## 8. AGENT REGISTRATION FLOW

**File**: `/Users/edenzou/workspace/shepaw/channel/cmd/agent/main.go`

### Agent Structure (Lines 56-73)

```go
type Agent struct {
    server    string        // https://channel.example.com
    channelID string
    secret    string        // Permanent channel secret (ch_sec_xxx)
    target    string        // http://localhost:3000
    
    conn   *websocket.Conn
    mu     sync.Mutex
    closed bool
    
    streams   map[int64]chan *Message  // Per-stream bidirectional channels
    streamsMu sync.RWMutex
    
    totalRequests int64
    totalBytes    int64
}
```

### Connect Flow (Lines 115-144)

```go
func (a *Agent) connect() error {
    // Build WebSocket URL
    base := strings.Replace(a.server, "https://", "wss://", 1)
    u := url.Parse(base + "/tunnel/connect")
    
    // Add query parameters
    q := u.Query()
    q.Set("channel_id", a.channelID)
    q.Set("secret", a.secret)  // Permanent secret!
    u.RawQuery = q.Encode()
    
    // Dial WebSocket
    dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
    conn, _, err := dialer.Dial(u.String(), nil)
    
    a.mu.Lock()
    a.conn = conn
    a.mu.Unlock()
    return nil
}
```

### Reconnection with Exponential Backoff (Lines 86-113)

```go
func (a *Agent) Run() {
    backoff := 2 * time.Second
    maxBackoff := 60 * time.Second
    
    for {
        log.Printf("🔌 Connecting to %s (channel: %s)...", a.server, a.channelID)
        
        err := a.connect()
        if err != nil {
            log.Printf("❌ Connection failed: %v", err)
        } else {
            backoff = 2 * time.Second  // Reset on success
            a.loop()
            log.Printf("⚠️  Connection closed")
        }
        
        if a.closed {
            return
        }
        
        log.Printf("↩️  Reconnecting in %s...", backoff)
        time.Sleep(backoff)
        backoff *= 2
        if backoff > maxBackoff {
            backoff = maxBackoff
        }
    }
}
```

**Backoff Pattern**:
- Start: 2s
- Double each retry: 2s → 4s → 8s → 16s → 32s → 60s (capped)

### Message Loop (Lines 147-198)

```go
func (a *Agent) loop() {
    defer func() { /* close connection */ }()
    
    for {
        _, data, err := a.conn.ReadMessage()
        if err != nil {
            return
        }
        
        var msg Message
        json.Unmarshal(data, &msg)
        
        switch msg.Type {
        case MsgPing:
            a.send(&Message{Type: MsgPong})
        
        case MsgRequest:
            go a.handleRequest(&msg)  // Concurrent handling
        
        case MsgWsConnect:
            go a.handleWsProxy(&msg)
        
        case MsgWsData, MsgWsClose:
            // Route to per-stream channel
            ch := a.streams[msg.StreamID]
            ch <- &msg
        
        case MsgClose:
            log.Printf("⚠️  Server closed (secret rotated?)")
            return
        }
    }
}
```

### HTTP Request Handling (Lines 201-266)

```go
func (a *Agent) handleRequest(req *Message) {
    // Decode body
    bodyBytes, _ := base64.StdEncoding.DecodeString(req.Body)
    
    // Build target URL
    targetURL := a.target + req.Path
    
    // Create HTTP request
    httpReq, _ := http.NewRequest(req.Method, targetURL, 
        strings.NewReader(string(bodyBytes)))
    
    // Forward headers (skip Host)
    for k, v := range req.Headers {
        if strings.ToLower(k) != "host" {
            httpReq.Header.Set(k, v)
        }
    }
    
    // Execute (25s timeout)
    client := &http.Client{Timeout: 25 * time.Second}
    resp, _ := client.Do(httpReq)
    
    // Send response
    a.send(&Message{
        Type:     MsgResponse,
        StreamID: req.StreamID,
        Status:   resp.StatusCode,
        Headers:  respHeaders,
        Body:     base64.StdEncoding.EncodeToString(bodyBytes),
    })
}
```

### WebSocket Proxy (Lines 284-353)

**Two-way proxy between tunnel and local ACP Server**:

1. **Incoming**: `MsgWsConnect` from server → dial local ACP Server
2. **Local→Tunnel**: Read from local WS → send `MsgWsData` to server
3. **Tunnel→Local**: Receive `MsgWsData` from stream channel → write to local WS

---

## 9. COMPLETE REGISTRATION FLOW SEQUENCE

### Phase 1: Channel Creation (User Dashboard)

```
User (Dashboard)
    ↓ POST /api/v1/channels {type: "tunnel-http", ...}
    ↓ [AuthMiddleware validates token]
Channel Service
    ↓ generateChannelSecret() → "ch_sec_<32hex>"
    ↓ Save Channel{id, secret, type, ...}
    ↓
Response: {channel_id, secret, setup_command}
    ↓
User copies secret
```

**Secret Format**: `ch_sec_` + 16 random bytes as hex = 32 hex chars total

### Phase 2: Agent Registration (First Connection)

```
Local Agent
    $ channel-agent --server https://channel.example.com \
                    --channel-id abc-123 \
                    --secret ch_sec_abc123... \
                    --target http://localhost:3000
    
    ↓ Connect WebSocket
    ↓ GET /tunnel/connect?channel_id=abc-123&timestamp=T&nonce=N&signature=HMAC(...)
    ↓
TunnelHandler.Connect()
    ├─ Extract timestamp, nonce, signature from query
    ├─ Validate timestamp within ±5min window
    ├─ Validate nonce not reused (NonceCache)
    ├─ channelSvc.GetChannelByID(channelID) → Channel
    ├─ Verify HMAC-SHA256 signature matches
    ├─ Verify channel.Type in {tunnel-http, tunnel-tcp, tunnel-ws}
    ├─ Verify channel.IsActive == true
    └─ tunnelMgr.Register(channelID, secret, conn)
    
    ├─ TunnelManager.Register():
    │  ├─ Create TunnelConn{channelID, secret, conn, streams}
    │  ├─ If old tunnel exists: close it (kick old agent)
    │  ├─ Store in tunnels map[channelID]
    │  └─ Start readLoop & pingLoop goroutines
    │
    └─ return (WebSocket upgraded successfully)

Agent.loop() starts:
    ├─ Receive MsgPing every 20s
    ├─ Send MsgPong back
    └─ Wait for MsgRequest / MsgWsConnect
```

### Phase 3: Agent Reconnection (Auto-Reconnect on Disconnect)

```
If connection drops:
    ├─ Backoff = 2s
    ├─ Wait 2s
    ├─ Try connect() again
    ├─ If success: reset backoff to 2s
    ├─ If fail: backoff *= 2 (4s, 8s, 16s, 32s, 60s, capped)
    └─ Repeat until success
```

### Phase 4: Secret Rotation (Force Agent Reconnect)

```
User (Dashboard)
    ↓ POST /api/v1/channels/abc-123/rotate-secret
    ↓ [AuthMiddleware validates token]

TunnelHandler.RotateSecret()
    ├─ RotateSecret(userID, channelID)
    │  ├─ Verify ownership
    │  ├─ newSecret = generateChannelSecret()
    │  └─ Update in DB, clear cache
    │
    ├─ tunnelMgr.KickIfSecret(channelID, oldSecret)
    │  ├─ Get tunnel for channelID
    │  ├─ If tunnel.secret == oldSecret:
    │  │  ├─ Remove from tunnels map
    │  │  ├─ conn.Close() → triggers error in readLoop
    │  │  └─ Agent receives disconnect
    │  └─ Else: do nothing (new agent already connected)
    │
    └─ return {new_secret, message: "Old agent kicked"}

Old Agent:
    ├─ conn.Close() error detected in loop()
    ├─ Return from loop()
    ├─ Run() catches disconnect
    ├─ Backoff & retry with OLD secret
    ├─ New attempt: GET /tunnel/connect?...&signature=HMAC(old_secret,...)
    ├─ TunnelHandler.Connect():
    │  ├─ channelSvc.GetChannelByID(channelID) → Channel (with new secret)
    │  ├─ VerifySignature(new_secret, ..., old_signature) → MISMATCH
    │  └─ Return 401 "invalid signature"
    │
    ├─ Agent receives 401, loop() exits
    ├─ Logs: "⚠️  Server closed the tunnel (secret may have been rotated)"
    └─ Can restart with new secret OR manual intervention
```

---

## 10. SECURITY ANALYSIS

### Strengths

1. **Permanent Secret Storage**: Secret persists in DB, no expiration (unlike temporary tokens)
2. **Secret Format Validation**: Enforces `ch_sec_` prefix to prevent token reuse
3. **Ownership Verification**: Confirms `channel_id` matches secret at connect time
4. **Channel Type Validation**: Only tunnel types can connect via secret
5. **Single Tunnel per Channel**: Old agent automatically evicted on new connection
6. **Secret Rotation Support**: Can immediately force reconnection with new secret
7. **Keyed by Random Bytes**: 16 random bytes = 128 bits of entropy
8. **Index on Secret**: Database indexed for fast lookup

### Potential Weaknesses

1. **No Signature/HMAC**: Secret is plaintext, transmitted over HTTPS/WSS only
2. **No Rate Limiting**: No built-in protection against brute-force secret guessing
3. **Query Parameter Exposure**: Secret visible in HTTP/WebSocket URL (mitigated by HTTPS/WSS)
4. **Shared Secrets**: If channel URL leaked, anyone with secret can connect
5. **No Client Certificates**: No mutual TLS authentication
6. **Secret Never Rotates Automatically**: Requires manual user action
7. **No Audit Trail**: No logging of who connects, when, or failed attempts
8. **No TTL/Expiration**: Secret never expires unless manually rotated

### Transmission Security

- **Protocol**: WebSocket over TLS (wss://)
- **Secret in URL**: Encrypted by TLS
- **No additional HMAC/Signature**: Relies entirely on TLS encryption

---

## 11. CRYPTO/UTILITY FUNCTIONS

### Token Hashing (auth.go, Lines 78-83)

```go
func hashToken(raw string) string {
    h := sha256.Sum256([]byte(raw))
    return hex.EncodeToString(h[:])
}
```

**Used for**: Temporary access tokens (NOT for channel secrets)

### Channel Secret Generation (channel.go, Lines 258-263)

```go
func generateChannelSecret() string {
    b := make([]byte, 16)
    rand.Read(b)
    return "ch_sec_" + hex.EncodeToString(b)
}
```

**Uses**: `crypto/rand` for 16 random bytes → 32 hex chars

### Email Code Generation (auth.go, Lines 337-348)

```go
func generateNumericCode(n int) (string, error) {
    digits := make([]byte, n)
    for i := range digits {
        v, err := rand.Int(rand.Reader, big.NewInt(10))
        if err != nil {
            return "", err
        }
        digits[i] = byte('0') + byte(v.Int64())
    }
    return string(digits), nil
}
```

**Used for**: 6-digit email verification codes

**No HMAC/Signature code found** - Security relies on TLS + secret format validation

---

## 12. FILE STRUCTURE SUMMARY

```
/Users/edenzou/workspace/shepaw/channel/
├── cmd/agent/main.go                    # Agent client (registration point)
├── pkg/cmd/main.go                      # Server entry point (routes setup)
├── pkg/internal/
│   ├── handlers/
│   │   ├── tunnel.go                    # Connect, RotateSecret endpoints
│   │   ├── channel.go                   # Channel CRUD
│   │   └── middleware.go                # Auth middleware
│   │
│   ├── services/
│   │   ├── tunnel.go                    # TunnelManager, message protocol
│   │   ├── channel.go                   # Secret generation, GetBySecret
│   │   ├── auth.go                      # Token validation
│   │   └── database.go                  # Schema migration
│   │
│   └── models/
│       ├── channel.go                   # Channel struct with Secret field
│       ├── user.go                      # User, UserChannel structs
│       └── config.go                    # Config struct
```

---

## 13. MESSAGE PROTOCOL (JSON over WebSocket)

### Request from Server → Agent
```json
{
  "type": "request",
  "stream_id": 12345,
  "method": "GET",
  "path": "/api/endpoint?param=value",
  "headers": {
    "Authorization": "Bearer token",
    "User-Agent": "..."
  },
  "body": "base64encodedData"
}
```

### Response from Agent → Server
```json
{
  "type": "response",
  "stream_id": 12345,
  "status": 200,
  "headers": {
    "Content-Type": "application/json"
  },
  "body": "base64encodedData"
}
```

### WebSocket Connect (Server → Agent)
```json
{
  "type": "ws_connect",
  "stream_id": 67890,
  "path": "/acp/ws?agentId=xyz",
  "headers": {
    "Authorization": "Bearer token"
  }
}
```

### Keepalive
```json
// Server → Agent
{"type": "ping"}

// Agent → Server  
{"type": "pong"}
```

---

## 14. KEY LINE NUMBERS REFERENCE

| Component | File | Lines | Function |
|-----------|------|-------|----------|
| Handler | tunnel.go | 38-97 | Connect() |
| Handler | tunnel.go | 110-145 | RotateSecret() |
| Manager | tunnel.go | 65-91 | Register() |
| Manager | tunnel.go | 100-109 | KickIfSecret() |
| Service | channel.go | 258-263 | generateChannelSecret() |
| Service | channel.go | 265-272 | GetChannelBySecret() |
| Service | channel.go | 274-288 | RotateSecret() |
| Agent | agent/main.go | 115-144 | connect() |
| Agent | agent/main.go | 86-113 | Run() (reconnect loop) |
| Agent | agent/main.go | 147-198 | loop() (message handler) |
| Routes | main.go | 138-141 | /tunnel/connect route |
| Routes | main.go | 143 | /tunnel/status route |
| Routes | main.go | 130 | /rotate-secret route |

---

## 15. COMPARISON: SECRET vs TOKEN AUTHENTICATION

| Aspect | Channel Secret | User Token |
|--------|----------------|-----------|
| Format | `ch_sec_` + 32 hex | Base64 (48 chars) |
| Storage | Plaintext in DB | SHA-256 hash in DB |
| Expiration | No | 15 mins (default) |
| Rotation | Manual | N/A |
| Endpoint | `/tunnel/connect` | API endpoints |
| User Auth | Channel ownership | Email/OAuth |
| Transmission | Query param or header | Authorization header |
| Validation | Database lookup | SHA-256 lookup + TTL check |

---

## 16. SUMMARY TABLE

| Layer | Component | Key Files | Authentication |
|-------|-----------|-----------|-----------------|
| **Handler** | TunnelHandler | tunnel.go | Channel secret |
| **Manager** | TunnelManager | tunnel.go | N/A (internal) |
| **Service** | ChannelService | channel.go | User token (for rotation only) |
| **Model** | Channel | channel.go | N/A (data structure) |
| **Agent** | Agent | cmd/agent/main.go | Channel secret |
| **Routes** | Routes | cmd/main.go | Hybrid (secret + token) |

