FROM golang:1.22-alpine AS builder

WORKDIR /app
RUN apk add --no-cache gcc musl-dev sqlite-dev

COPY go.mod go.sum ./
RUN GOPROXY=https://goproxy.cn,direct go mod download

COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -o channel-service ./pkg/cmd/

# ── 运行镜像 ──────────────────────────────────────────────────────────────────
FROM alpine:3.18
RUN apk --no-cache add ca-certificates tzdata sqlite-libs

WORKDIR /app
COPY --from=builder /app/channel-service .
COPY --from=builder /app/templates ./templates

EXPOSE 8080
# TCP/UDP 代理端口段（可在 docker run -p 或 compose 中指定范围）
EXPOSE 10000-20000

ENV PORT=8080 \
    BASE_URL=http://localhost:8080 \
    BASE_DOMAIN=localhost:8080 \
    DATABASE_URL=sqlite:./data/channel.db \
    TOKEN_TTL=15m \
    MAX_CHANNELS=5 \
    TCP_PORT_RANGE_START=10000 \
    TCP_PORT_RANGE_END=20000

VOLUME ["/app/data"]

CMD ["./channel-service"]
