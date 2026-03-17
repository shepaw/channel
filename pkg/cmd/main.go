package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
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
	adminHandler := handlers.NewAdminHandler(authSvc, redisSvc, cfg)
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
	r.Use(handlers.CORSMiddleware(cfg)) // 全局 CORS，必须在所有路由前注册
	r.LoadHTMLGlob("templates/*.html")
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
			// ⚠️ /login 已移除：OAuth 回调直接内部调用，不对外暴露 CreateOrGetUser
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

	// 隧道路由
	r.GET("/tunnel/connect", tunnelHandler.Connect)
	// /tunnel/status 需要认证，防止枚举 channel 在线状态
	r.GET("/tunnel/status/:channel_id", handlers.AuthMiddleware(authSvc), tunnelHandler.Status)

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

	// ── 管理后台 ───────────────────────────────────────────────────────────
	admin := r.Group("/admin")
	{
		// 公开页面 & OAuth
		admin.GET("/login", adminHandler.LoginPage)
		admin.GET("/auth/google/initiate", adminHandler.GoogleInitiate)
		admin.GET("/auth/google/callback", adminHandler.GoogleCallback)
		admin.GET("/logout", adminHandler.Logout)

		// 需要 admin session 的页面
		authedAdmin := admin.Group("")
		authedAdmin.Use(handlers.AdminAuthMiddleware(redisSvc, cfg))
		{
			authedAdmin.GET("/dashboard", adminHandler.DashboardPage)

			// Admin API
			adminAPI := authedAdmin.Group("/api")
			{
				adminAPI.GET("/stats", adminHandler.GetStats)
				adminAPI.GET("/users", adminHandler.ListUsers)
				adminAPI.GET("/channels", adminHandler.ListChannels)
				adminAPI.PUT("/channels/:id/toggle", adminHandler.ToggleChannel)
				adminAPI.DELETE("/channels/:id", adminHandler.DeleteChannel)
			}
		}
	}

	// ── 启动服务 ───────────────────────────────────────────────────────────

	// 进程重启时清除所有 channel 的残留连接计数，防止上次异常退出导致计数虚高
	go func() {
		var channels []models.Channel
		if err := dbSvc.DB.Find(&channels).Error; err == nil {
			for _, ch := range channels {
				rateLimitSvc.ResetConnections(ch.ID)
			}
			log.Printf("🔄 Reset connection counters for %d channels", len(channels))
		}
	}()
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
	envStr("ADMIN_EMAIL", &cfg.AdminEmail)
	// ALLOWED_ORIGINS: 逗号分隔，如 "https://app.example.com,https://admin.example.com"
	if origins := os.Getenv("ALLOWED_ORIGINS"); origins != "" {
		for _, o := range strings.Split(origins, ",") {
			o = strings.TrimSpace(o)
			if o != "" {
				cfg.AllowedOrigins = append(cfg.AllowedOrigins, o)
			}
		}
	}

	return &cfg
}
