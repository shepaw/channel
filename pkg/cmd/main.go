package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/edenzou/channel-service/pkg/internal/handlers"
	"github.com/edenzou/channel-service/pkg/internal/models"
	"github.com/edenzou/channel-service/pkg/internal/services"
	"github.com/gin-gonic/gin"
)

func main() {
	cfg := loadConfig()

	// ── 数据库 ──────────────────────────────────────────────────────────────
	dbSvc, err := services.NewDatabaseService(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Database init failed: %v", err)
	}
	defer dbSvc.Close()

	// ── Redis（可选，不启动时用内存版降级） ──────────────────────────────────
	redisSvc, err := services.NewRedisService(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
	if err != nil {
		log.Printf("⚠️  Redis not available (%v), some features may be limited", err)
		redisSvc = services.NewNopRedisService()
	}
	defer redisSvc.Close()

	// ── 业务服务 ──────────────────────────────────────────────────────────
	authSvc := services.NewAuthService(dbSvc, redisSvc)
	emailSvc := services.NewEmailService(cfg)
	channelSvcV2 := services.NewChannelServiceV2(dbSvc, redisSvc, cfg)
	rateLimitSvc := services.NewRateLimitService(dbSvc, redisSvc)
	tunnelMgr := services.NewTunnelManager()

	// ── Handlers ──────────────────────────────────────────────────────────
	authHandler := handlers.NewAuthHandler(authSvc, cfg)
	emailAuthHandler := handlers.NewEmailAuthHandler(authSvc, emailSvc, cfg)
	channelHandler := handlers.NewChannelHandler(channelSvcV2.ChannelService, rateLimitSvc, cfg)
	proxyHandler := handlers.NewProxyHandler(channelSvcV2.ChannelService, rateLimitSvc, channelSvcV2)
	oauthHandler := handlers.NewOAuthHandler(authSvc, redisSvc, cfg)
	tunnelHandler := handlers.NewTunnelHandler(tunnelMgr, channelSvcV2.ChannelService, authSvc)

	// 注入 TunnelManager 到 ProxyHandler
	proxyHandler.SetTunnelManager(tunnelMgr)

	// 注入 V2 服务，让 channel handler 能启动 TCP/UDP 监听
	channelHandler.SetV2(channelSvcV2, proxyHandler)

	// ── 恢复已有 TCP/UDP channel 的监听器 ───────────────────────────────────
	if err := channelSvcV2.RestoreProxies(proxyHandler); err != nil {
		log.Printf("⚠️  Failed to restore some TCP/UDP proxies: %v", err)
	}

	// ── Gin 路由 ───────────────────────────────────────────────────────────
	r := gin.Default()
	r.LoadHTMLGlob("templates/*")
	r.Static("/static", "./web/static")

	// 页面
	r.GET("/", func(c *gin.Context) { c.HTML(200, "index.html", nil) })
	r.GET("/login", func(c *gin.Context) { c.HTML(200, "login.html", nil) })
	r.GET("/dashboard", func(c *gin.Context) { c.HTML(200, "dashboard.html", nil) })
	r.GET("/oauth-callback", func(c *gin.Context) { c.HTML(200, "oauth_callback.html", nil) })

	// OAuth 回调
	r.GET("/auth/wechat/callback", oauthHandler.WechatCallback)
	r.GET("/auth/google/callback", oauthHandler.GoogleCallback)

	// API v1
	api := r.Group("/api/v1")
	{
		// 无需认证
		auth := api.Group("/auth")
		{
			auth.POST("/login", authHandler.Login)
			auth.GET("/wechat/qrcode", oauthHandler.WechatQRCode)
			auth.GET("/wechat/status", oauthHandler.WechatStatus)
			auth.GET("/google/initiate", oauthHandler.GoogleInitiate)

			// 邮箱注册/登录
			email := auth.Group("/email")
			{
				email.POST("/send-code", emailAuthHandler.SendCode)
				email.POST("/register", emailAuthHandler.Register)
				email.POST("/login/password", emailAuthHandler.LoginPassword)
				email.POST("/login/code", emailAuthHandler.LoginCode)
			}
		}

		// 需要认证
		authed := api.Group("")
		authed.Use(handlers.AuthMiddleware(authSvc))
		{
			authed.POST("/tokens", authHandler.GenerateToken)
			authed.DELETE("/tokens/current", authHandler.RevokeToken)

			ch := authed.Group("/channels")
			{
				ch.POST("", channelHandler.Create)
				ch.GET("", channelHandler.GetAll)
				ch.GET("/:id", channelHandler.GetByID)
				ch.PUT("/:id", channelHandler.Update)
				ch.DELETE("/:id", channelHandler.Delete)
				ch.POST("/:id/rate-limits", channelHandler.AddRateLimitRule)
				ch.GET("/:id/rate-limits", channelHandler.GetRateLimitRules)
				ch.DELETE("/:id/rate-limits/:rule_id", channelHandler.DeleteRateLimitRule)
				ch.POST("/:id/rotate-secret", tunnelHandler.RotateSecret) // 重置 tunnel secret
			}
		}
	}

	// 隧道路由（本地 agent 连接 + 状态查询，无需登录认证）
	r.GET("/tunnel/connect", tunnelHandler.Connect)
	r.GET("/tunnel/status/:channel_id", tunnelHandler.Status)

	// 代理转发路由（HTTP/HTTPS/WebSocket/隧道）
	r.Any("/proxy/:channel_id", func(c *gin.Context) {
		proxyHandler.ServeHTTP(c.Writer, c.Request, c.Param("channel_id"))
	})
	r.Any("/proxy/:channel_id/*path", func(c *gin.Context) {
		proxyHandler.ServeHTTP(c.Writer, c.Request, c.Param("channel_id"))
	})

	// 健康检查
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "time": time.Now().Format(time.RFC3339)})
	})

	// ── 启动服务 ───────────────────────────────────────────────────────────
	addr := fmt.Sprintf(":%d", cfg.ServerPort)
	srv := &http.Server{Addr: addr, Handler: r}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("✅ Channel Service listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	<-quit
	log.Println("Shutting down...")
}

// loadConfig 从环境变量读取配置，未设置则用默认值
func loadConfig() *models.Config {
	cfg := models.DefaultConfig

	envInt := func(key string, target *int) {
		if v := os.Getenv(key); v != "" {
			fmt.Sscanf(v, "%d", target)
		}
	}
	envStr := func(key string, target *string) {
		if v := os.Getenv(key); v != "" {
			*target = v
		}
	}
	envDur := func(key string, target *time.Duration) {
		if v := os.Getenv(key); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				*target = d
			}
		}
	}

	envInt("PORT", &cfg.ServerPort)
	envStr("BASE_URL", &cfg.BaseURL)
	envStr("BASE_DOMAIN", &cfg.BaseDomain)
	envStr("DATABASE_URL", &cfg.DatabaseURL)
	envStr("REDIS_ADDR", &cfg.RedisAddr)
	envStr("REDIS_PASSWORD", &cfg.RedisPassword)
	envDur("TOKEN_TTL", &cfg.TokenTTL)
	envInt("MAX_CHANNELS", &cfg.MaxChannels)
	envInt("TCP_PORT_RANGE_START", &cfg.TCPPortRangeStart)
	envInt("TCP_PORT_RANGE_END", &cfg.TCPPortRangeEnd)
	envStr("WECHAT_APP_ID", &cfg.WechatAppID)
	envStr("WECHAT_APP_SECRET", &cfg.WechatAppSecret)
	envStr("GOOGLE_CLIENT_ID", &cfg.GoogleClientID)
	envStr("GOOGLE_CLIENT_SECRET", &cfg.GoogleClientSecret)
	envStr("SMTP_HOST", &cfg.SMTPHost)
	envInt("SMTP_PORT", &cfg.SMTPPort)
	envStr("SMTP_USERNAME", &cfg.SMTPUsername)
	envStr("SMTP_PASSWORD", &cfg.SMTPPassword)
	envStr("SMTP_FROM", &cfg.SMTPFrom)

	return &cfg
}
