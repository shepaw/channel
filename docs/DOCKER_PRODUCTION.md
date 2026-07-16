# Channel — Docker 生产清单

仓库根目录已有 `Dockerfile` 与 `docker-compose.yml`（含 Redis）。公开或半公开部署前请核对：

## 必做

1. **TLS 终止**：前面加 Caddy / Nginx / Cloudflare；`BASE_URL` / `BASE_DOMAIN` 使用 `https://...`
2. **禁止** `DEV_SKIP_AUTH=1`
3. 设置 `ADMIN_EMAIL`、OAuth（微信 / Google）或邮件登录所需密钥
4. 生产使用 **Postgres + Redis**（勿依赖单文件 SQLite 多副本）
5. 配置 `ALLOWED_ORIGINS` / `ALLOWED_DOMAINS`
6. 为 `/proxy` 与 `/tunnel/connect` 准备反向代理层限流（应用内 tunnel 路径默认无限流）

## 推荐 compose 片段

```yaml
environment:
  PORT: 8080
  BASE_URL: https://channel.example.com
  BASE_DOMAIN: channel.example.com
  DATABASE_URL: postgres://user:pass@db:5432/channel?sslmode=disable
  REDIS_ADDR: redis:6379
  # DEV_SKIP_AUTH:  # 切勿设置
  ADMIN_EMAIL: you@example.com
  ALLOWED_ORIGINS: https://channel.example.com
```

## ShePaw / Hub 对接

在 Hub：

```bash
shepaw-hub gateway-set-channel \
  --server https://channel.example.com \
  --channel-id <id> \
  --secret <ch_sec_...>
shepaw-hub gateway-start
```

手机侧依赖 ACP / Peer 端到端加密；Channel 只负责到达本机端口。保管好 `channelId` / 短 alias（知 URL 即可打到在线 agent）。

## 健康检查

```bash
curl -fsS https://channel.example.com/health
```
