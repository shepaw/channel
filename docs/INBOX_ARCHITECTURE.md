# Channel 云端收件箱架构

## 定位

Channel 是 **shepaw（App）** 与 **agent-bridge（本地 Agent Hub）** 之间的 **云端信箱**，不是代理身份，也不是对话参与方。

| 角色 | 职责 |
|------|------|
| **shepaw** | 在线时走实时通道（tunnel/peer）；离线或 agent 忙时把密文投入收件箱；上线后从收件箱收取回复 |
| **agent-bridge** | 在线时处理实时请求；离线期间从收件箱 claim 留言、处理后回投密文回复 |
| **channel** | 只存 **密文 + 路由元数据**，E2E 加密（seal-box），服务端看不到明文 |

隧道（`/proxy/…`、`/tunnel/connect`）是 **传输层**：让 agent-bridge 在 NAT 后保持可达。收件箱是 **业务层**：保证异步消息不丢、可串联。

## 串联键（Correlation Keys）

每条收件箱消息携带以下元数据，用于把异步回复串回正确的会话与 inflight turn：

| 字段 | 说明 |
|------|------|
| `target_type` | `agent` 或 `group` |
| `target_id` | agent_id（如 `acp_agent_…`）或 group_id（如 `psess_group_…`） |
| `session_id` | 一次会话标识；App 再次 `agent.chat` 时携带以续聊 |
| `request_id` | 一次对话 turn 标识；与 peer inflight / ACP task 对应 |
| `message_id` | 客户端生成的消息 id；回复通过 `reply_to` 关联原留言 |
| `group_id` | 可选；agent 目标下的群上下文 |
| `caller_fp` | 留言方 Noise 公钥指纹（16 hex），回复路由回正确用户 |
| `kind` | 内容类型：`chat`（默认）、`article`（预留，公开文章投递） |

## 数据流

### 留言（caller → inbox → agent）

```
shepaw                          channel                         agent-bridge
  │  POST /mailbox/:target_id/messages                           │
  │  { caller_fp, message_id, request_id, session_id,            │
  │    group_id, ciphertext }                                      │
  ├──────────────────────────────►  pending inbound               │
  │                                (TTL 7d, at-least-once)        │
  │                                mail_waiting (tunnel, 可选) ──►│
  │                                                               │ GET /pending (HMAC)
  │                                                               │ decrypt → onChat
  │                                                               │ POST /replies (sealed)
  │                               ◄──────────────────────────────┤
  │                               ack inbound                     │
```

### 收取回复（agent → inbox → caller）

```
shepaw 进聊天页 / busy 留言后
  │  WS /inbox/subscribe?caller_fp=…  （mail_reply 推送）
  │  GET /inbox/replies?caller_fp=…     （拉密文，push 触发或 5s 兜底）
  ├──────────────────────────────►  pending replies
  │◄──────────────────────────────  [{ reply_to, request_id, session_id, ciphertext }]
  │  decrypt → 写入本地 DB → POST …/replies/ack
```

## API 概览

### Caller（免登录，IP 限流）

| Method | Path | 说明 |
|--------|------|------|
| POST | `/api/v1/mailbox/:target_id/messages` | 投递留言 |
| GET | `/api/v1/mailbox/:target_id/replies` | 拉取指定 target 的回复 |
| POST | `/api/v1/mailbox/:target_id/replies/ack` | 确认已落本地 |
| GET | `/api/v1/inbox/replies` | **跨 target 统一拉取**（App 上线） |
| POST | `/api/v1/inbox/replies/ack` | 跨 target 确认 |
| GET | `/api/v1/inbox/subscribe` | **WebSocket**：`mail_reply` 推送（caller_fp） |

### Agent handler（channel secret HMAC）

| Method | Path | 说明 |
|--------|------|------|
| GET | `/api/v1/mailbox/:target_id/pending` | claim 待处理留言 |
| POST | `/api/v1/mailbox/:target_id/ack` | 确认已处理 inbound |
| POST | `/api/v1/mailbox/:target_id/replies` | 回投回复密文 |

HMAC 签名串：`{channel_id}\n{target_id}\n{timestamp}\n{nonce}`

## 与隧道的关系

```
┌─────────────┐     实时 (WS/tunnel)      ┌──────────────┐
│   shepaw    │◄────────────────────────►│ agent-bridge │
└──────┬──────┘                          └──────┬───────┘
       │                                         │
       │         异步 (REST inbox)               │
       └──────────────► channel ◄─────────────────┘
                        密文 + 元数据
```

- **在线**：优先走 tunnel 实时对话；agent 忙时 shepaw 可 fallback 到收件箱留言。
- **离线**：shepaw 留言/agent 回复均暂存 channel；任一方上线后 pull + ack。
- **tunnel 不是身份**：channel 上的 tunnel 只是 NAT 穿透；agent 身份由 `agent_id` + Noise 白名单决定。

## 扩展：公开文章投递

预留 `kind=article`：

- 投递方可为系统或公开 agent，target 可为订阅频道 id。
- 密文格式与 chat 相同；shepaw 按 `kind` 渲染为文章而非聊天气泡。
- 鉴权与配额策略后续单独定义（不在 v1 范围）。

## 相关代码

| 仓库 | 路径 |
|------|------|
| channel | `pkg/internal/models/mailbox.go`, `services/mailbox.go`, `handlers/mailbox.go` |
| shepaw | `lib/services/mailbox/channel_mailbox_service.dart`, `inbox_subscribe_service.dart`, `mailbox_inbox_poller.dart` |
| agent-bridge | `sdks/shepaw-acp-sdk-typescript/src/mailbox.ts`, `server.ts` (drainMailbox) |
