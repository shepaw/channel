# Channel Service

shepaw 与 agent-bridge 之间的 **云端收件箱** + **NAT 隧道传输层**。

## 核心定位

| 层级 | 能力 | 说明 |
|------|------|------|
| **收件箱（主）** | 异步密文信箱 | App/agent 离线仍可留言与收取；E2E seal-box，服务端只见密文 |
| **传输层（辅）** | HTTP/WS tunnel | 让 agent-bridge 在 NAT 后保持实时可达 |
| **发现/接入** | Agent 目录 + 接入审批 | 公开 agent 名片、Noise 白名单中介 |

架构详情见 [docs/INBOX_ARCHITECTURE.md](docs/INBOX_ARCHITECTURE.md)。

## 收件箱串联键

每条消息记录 `target_id`（agent 或 group）、`session_id`、`request_id`、`message_id`/`reply_to`，保证异步回复能串回正确的会话与 inflight turn。

## 快速开始

### 本地运行

```bash
cd channel
go build -o channel-service ./pkg/cmd/
./channel-service
```

访问 http://localhost:8080

### Docker Compose

```bash
cp .env.example .env
docker-compose up -d
```

## 收件箱 API（节选）

### Caller（shepaw，免登录）

```bash
# 留言（agent 忙或离线）
curl -X POST http://localhost:8080/api/v1/mailbox/acp_agent_xxx/messages \
  -H "Content-Type: application/json" \
  -d '{
    "caller_fp": "0123456789abcdef",
    "message_id": "msg-uuid",
    "request_id": "req-uuid",
    "session_id": "sess-uuid",
    "group_id": "psess_group_abc",
    "ciphertext": "<base64-sealed>"
  }'

# App 上线：跨 agent 统一收取
curl "http://localhost:8080/api/v1/inbox/replies?caller_fp=0123456789abcdef"
```

### Agent（agent-bridge，HMAC）

```bash
# claim 待处理留言 → 处理后 POST /replies 回投密文
curl "http://localhost:8080/api/v1/mailbox/acp_agent_xxx/pending?limit=5&timestamp=...&nonce=...&signature=..."
```

## 其他能力

| 功能 | 说明 |
|------|------|
| 用户系统 | 微信 / Google / 邮箱登录 |
| Tunnel | `/tunnel/connect` WebSocket 长连接 |
| 代理转发 | `/proxy/:channel_id/*`（传输层，非身份） |
| Agent 发现 | `/api/v1/discovery/agents` |
| 版本管理 | `/api/v1/check-update` |

完整 API 与配置见各 `docs/` 文档。

## 项目结构

```
channel/
├── pkg/
│   ├── cmd/main.go
│   └── internal/
│       ├── models/          # User, Agent, MailboxMessage, Channel, …
│       ├── services/        # mailbox, agent, tunnel, channel, …
│       └── handlers/
├── docs/
│   └── INBOX_ARCHITECTURE.md
└── templates/
```

## 配置（环境变量）

| 变量 | 默认值 | 说明 |
|------|--------|------|
| PORT | 8080 | HTTP 监听端口 |
| BASE_URL | http://localhost:8080 | 公开访问地址 |
| DATABASE_URL | sqlite:./channel.db | SQLite 或 postgres:// |
| REDIS_ADDR | localhost:6379 | 可选，降级内存模式 |
| MAX_CHANNELS | 5 | 每用户最大 tunnel 数 |

完整列表见 `.env.example`。
