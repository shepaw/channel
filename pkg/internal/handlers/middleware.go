package handlers

import (
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/edenzou/channel-service/pkg/internal/models"
	"github.com/edenzou/channel-service/pkg/internal/services"
	"github.com/gin-gonic/gin"
)

// devSkipAuthUserID 是 DEV_SKIP_AUTH 模式下默认使用的 user_id
const devSkipAuthUserID = "dev-user-local"

// AuthMiddleware validates access tokens.
//
// 开发调试模式（仅本地使用，生产环境严禁开启）：
//
//	DEV_SKIP_AUTH=1              跳过 token 验证，使用默认 user_id "dev-user-local"
//	DEV_SKIP_AUTH=1 DEV_USER_ID=xxx  跳过验证，并指定自定义 user_id
func AuthMiddleware(authSvc *services.AuthService) gin.HandlerFunc {
	// 启动时检查一次，避免每次请求都读取环境变量
	devSkip := os.Getenv("DEV_SKIP_AUTH") == "1"
	devUserID := os.Getenv("DEV_USER_ID")
	if devUserID == "" {
		devUserID = devSkipAuthUserID
	}

	if devSkip {
		log.Printf("⚠️  DEV_SKIP_AUTH=1: auth middleware is DISABLED (user_id=%s). DO NOT use in production!", devUserID)
	}

	return func(c *gin.Context) {
		if devSkip {
			c.Set("user_id", devUserID)
			c.Set("user_email", "dev@local.test")
			c.Next()
			return
		}

		token := c.GetHeader("Authorization")
		if token == "" {
			token = c.Query("access_token")
		}

		if token == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authorization required"})
			c.Abort()
			return
		}

		// 去掉Bearer前缀
		if len(token) > 7 && token[:7] == "Bearer " {
			token = token[7:]
		}

		user, err := authSvc.ValidateToken(token)
		if err != nil {
			switch err {
			case services.ErrTokenExpired:
				c.JSON(http.StatusUnauthorized, gin.H{"error": "Token expired"})
			default:
				c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
			}
			c.Abort()
			return
		}

		c.Set("user_id", user.ID)
		c.Set("user_email", user.Email)
		c.Set("user", user)
		c.Next()
	}
}

// CORSMiddleware 限制跨域请求来源。
// 允许的 Origin 由 config.AllowedOrigins 控制，
// 未配置时默认仅允许同源（不添加 Access-Control-Allow-Origin）。
func CORSMiddleware(cfg *models.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			c.Next()
			return
		}

		allowed := false
		for _, o := range cfg.AllowedOrigins {
			if o == "*" || strings.EqualFold(o, origin) {
				allowed = true
				break
			}
		}

		if allowed {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Allow-Credentials", "true")
			c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Authorization,Content-Type")
			c.Header("Vary", "Origin")
		}

		if c.Request.Method == http.MethodOptions {
			if allowed {
				c.AbortWithStatus(http.StatusNoContent)
			} else {
				c.AbortWithStatus(http.StatusForbidden)
			}
			return
		}

		c.Next()
	}
}

// DomainMiddleware 校验请求的 Host 是否在允许的域名列表中。
// 支持通配符，例如 *.shepaw.com 匹配 api.shepaw.com、www.shepaw.com 等。
// AllowedDomains 为空时不启用域名过滤（允许所有域名）。
func DomainMiddleware(cfg *models.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 未配置则跳过过滤
		if len(cfg.AllowedDomains) == 0 {
			c.Next()
			return
		}

		// 从 Host header 中提取主机名（去掉端口）
		host := c.Request.Host
		if idx := strings.LastIndex(host, ":"); idx != -1 {
			host = host[:idx]
		}
		host = strings.ToLower(strings.TrimSpace(host))

		for _, pattern := range cfg.AllowedDomains {
			pattern = strings.ToLower(strings.TrimSpace(pattern))
			if pattern == "*" || matchDomain(pattern, host) {
				c.Next()
				return
			}
		}

		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Forbidden: domain not allowed"})
	}
}

// matchDomain 判断 host 是否匹配 pattern。
// 支持通配符 * 仅作为最左侧子域名通配符，例如:
//   - *.shepaw.com 匹配 api.shepaw.com，但不匹配 shepaw.com 或 a.b.shepaw.com
//   - shepaw.com 精确匹配 shepaw.com
func matchDomain(pattern, host string) bool {
	if !strings.HasPrefix(pattern, "*.") {
		return pattern == host
	}
	// 通配符模式: *.example.com
	suffix := pattern[1:] // 保留 ".example.com"
	if !strings.HasSuffix(host, suffix) {
		return false
	}
	// 确保通配符只匹配单层子域名，即 host 去掉 suffix 后不含 "."
	sub := host[:len(host)-len(suffix)]
	return len(sub) > 0 && !strings.Contains(sub, ".")
}

