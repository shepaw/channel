# Channel Service

用 Go 实现的通道代理服务，打通任意两个本地应用。

## 核心功能

| 功能 | 说明 |
|------|------|
| 协议支持 | HTTP / HTTPS / WebSocket / TCP / UDP |
| 用户系统 | 微信扫码登录（中国）/ Google OAuth（海外） |
| Token 管理 | 临时 access token，默认 15 分钟，可自定义 |
| Channel 管理 | 每用户最多 5 个（可配置），永久地址 |
| 限流控制 | 每 channel 独立配置带宽 / 并发连接 / 请求速率 |

## 快速开始

### 本地运行（无需 Redis / Postgres）

```bash
cd /projects/channel
go build -o channel-service ./pkg/cmd/
./channel-service
```

访问 http://localhost:8080

### Docker Compose

```bash
cp .env.example .env   # 填入 OAuth 配置
docker-compose up -d
```

## 工作流程

1. **用户登录** → 微信扫码 或 Google OAuth
2. **获取 token** → `POST /api/v1/tokens` (有效期默认 15 分钟)
3. **注册 channel**：

```bash
curl -X POST http://localhost:8080/api/v1/channels \
  -H "Authorization: Bearer <your-token>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-app",
    "type": "http",
    "target": "http://127.0.0.1:3000"
  }'
```

返回：
```json
{
  "id": "xxxx-xxxx",
  "endpoint": "http://localhost:8080/proxy/xxxx-xxxx",
  "type": "http",
  ...
}
```

4. **其他应用访问** → `GET http://localhost:8080/proxy/xxxx-xxxx/path`

## API 参考

### 认证

| Method | Path | 说明 |
|--------|------|------|
| GET | /api/v1/auth/wechat/qrcode | 获取微信扫码 URL |
| GET | /api/v1/auth/wechat/status?scene_id=xxx | 轮询扫码状态 |
| GET | /api/v1/auth/google/initiate | 跳转 Google 登录 |
| POST | /api/v1/tokens | 生成 access token |
| DELETE | /api/v1/tokens/current | 吊销当前 token |

### Channel

| Method | Path | 说明 |
|--------|------|------|
| POST | /api/v1/channels | 创建 channel |
| GET | /api/v1/channels | 列出我的 channels |
| GET | /api/v1/channels/:id | 获取单个 |
| PUT | /api/v1/channels/:id | 更新 |
| DELETE | /api/v1/channels/:id | 删除（地址失效） |
| POST | /api/v1/channels/:id/rate-limits | 添加限流规则 |
| GET | /api/v1/channels/:id/rate-limits | 查看限流规则 |
| DELETE | /api/v1/channels/:id/rate-limits/:rule_id | 删除限流规则 |

### 限流规则示例

```json
{
  "rule_type": "bandwidth",
  "limit_value": 10.0,
  "time_window": "1s"
}
```

- `rule_type`: `bandwidth`（Mbps）/ `connections`（并发数）/ `requests`（rps）
- `limit_value`: 数值限制
- `time_window`: Go Duration，如 `1s` `1m` `1h`

## 配置（环境变量）

| 变量 | 默认值 | 说明 |
|------|--------|------|
| PORT | 8080 | HTTP 监听端口 |
| BASE_URL | http://localhost:8080 | 公开访问地址 |
| BASE_DOMAIN | localhost:8080 | 域名（用于生成 endpoint） |
| DATABASE_URL | sqlite:./channel.db | 数据库；支持 sqlite: 或 postgres:// |
| REDIS_ADDR | localhost:6379 | Redis 地址（不可用时自动降级内存模式） |
| TOKEN_TTL | 15m | 默认 token 有效期 |
| MAX_CHANNELS | 5 | 每用户最大 channel 数 |
| TCP_PORT_RANGE_START | 10000 | TCP/UDP 端口段起始 |
| TCP_PORT_RANGE_END | 20000 | TCP/UDP 端口段结束 |
| WECHAT_APP_ID | - | 微信开放平台 AppID |
| WECHAT_APP_SECRET | - | 微信开放平台 AppSecret |
| GOOGLE_CLIENT_ID | - | Google OAuth Client ID |
| GOOGLE_CLIENT_SECRET | - | Google OAuth Client Secret |

## 协议说明

- **HTTP/HTTPS/WebSocket**：通过 `/proxy/:channel_id/*path` 路由转发
- **TCP/UDP**：服务启动独立端口监听（端口在 endpoint 中返回），注册时立即开始监听

## 项目结构

```
/projects/channel/
├── pkg/
│   ├── cmd/main.go                        # 入口
│   └── internal/
│       ├── models/                        # 数据模型
│       ├── services/                      # 业务逻辑
│       │   ├── auth.go
│       │   ├── channel.go
│       │   ├── port_allocator.go          # TCP/UDP 端口分配
│       │   ├── rate_limit.go
│       │   ├── redis.go                   # Redis + Nop 降级
│       │   ├── database.go                # SQLite / Postgres
│       │   └── proxy/
│       │       ├── http.go                # HTTP 反向代理
│       │       └── websocket.go           # WebSocket 代理
│       └── handlers/
│           ├── auth.go
│           ├── channel.go
│           ├── proxy.go                   # TCP/UDP/HTTP/WS 分发
│           ├── oauth.go                   # 微信 + Google OAuth
│           └── middleware.go              # token 认证中间件
├── templates/                             # 前端页面（login/dashboard）
├── Dockerfile
├── docker-compose.yml
└── .env.example
```
