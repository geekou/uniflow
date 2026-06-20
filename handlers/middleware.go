package handlers

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"uniflow/models"
)

// ============ 安全响应头中间件 ============

// SecurityHeadersMiddleware 添加安全相关的 HTTP 响应头
func SecurityHeadersMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-XSS-Protection", "1; mode=block")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		c.Header("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval' https://cdn.jsdelivr.net https://cdnjs.cloudflare.com https://cdn.quilljs.com; style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net https://cdnjs.cloudflare.com; img-src 'self' data: https:; media-src 'self'; font-src 'self' https://cdnjs.cloudflare.com; connect-src 'self'")
		c.Next()
	}
}

// ============ 管理员会话管理 ============

type sessionEntry struct {
	username  string
	createdAt time.Time
}

var (
	sessionStore = make(map[string]sessionEntry) // token -> session
	sessionMu    sync.RWMutex
	hmacSecret   []byte
)

func init() {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		log.Fatalf("[FATAL] crypto/rand failed to generate HMAC key: %v", err)
	}
	hmacSecret = key
}

// setSessionCookie 设置带 SameSite=Strict 的会话 cookie
func setSessionCookie(c *gin.Context, value string, maxAge int) {
	secure := true
	if c.Request.TLS == nil && (c.Request.Host == "localhost:9090" || strings.HasPrefix(c.Request.Host, "127.0.0.1") || strings.HasPrefix(c.Request.Host, "localhost")) {
		secure = false // 本地开发允许 HTTP
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     "uniflow_session",
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

// GenerateSession 创建管理员会话，返回 token
func GenerateSession(username string) string {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		log.Printf("[ERROR] crypto/rand failed for session token: %v", err)
		return ""
	}
	tokenStr := hex.EncodeToString(token)

	sessionMu.Lock()
	sessionStore[tokenStr] = sessionEntry{username: username, createdAt: time.Now()}
	sessionMu.Unlock()

	return tokenStr
}

// ValidateSession 验证会话 token，返回用户名
func ValidateSession(token string) (string, bool) {
	sessionMu.RLock()
	defer sessionMu.RUnlock()
	e, ok := sessionStore[token]
	if !ok {
		return "", false
	}
	// 会话超过 7 天自动过期
	if time.Since(e.createdAt) > 7*24*time.Hour {
		return "", false
	}
	return e.username, true
}

// RevokeSession 撤销会话
func RevokeSession(token string) {
	sessionMu.Lock()
	delete(sessionStore, token)
	sessionMu.Unlock()
}

// SignCookie 签名 cookie 值
func SignCookie(value string) string {
	mac := hmac.New(sha256.New, hmacSecret)
	mac.Write([]byte(value))
	return value + "|" + hex.EncodeToString(mac.Sum(nil))
}

// VerifyCookie 验证签名 cookie
func VerifyCookie(signed string) (string, bool) {
	parts := strings.SplitN(signed, "|", 2)
	if len(parts) != 2 {
		return "", false
	}
	value, sig := parts[0], parts[1]
	mac := hmac.New(sha256.New, hmacSecret)
	mac.Write([]byte(value))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return "", false
	}
	return value, true
}

// AuthMiddleware 管理员登录认证中间件
func AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		cookie, err := c.Cookie("uniflow_session")
		if err != nil {
			c.Redirect(http.StatusFound, "/admin/login")
			c.Abort()
			return
		}

		token, ok := VerifyCookie(cookie)
		if !ok {
			c.Redirect(http.StatusFound, "/admin/login")
			c.Abort()
			return
		}

		username, ok := ValidateSession(token)
		if !ok {
			c.Redirect(http.StatusFound, "/admin/login")
			c.Abort()
			return
		}

		c.Set("admin_username", username)

		// 查询用户角色
		var role string
		models.DB.QueryRow("SELECT role FROM users WHERE username = ?", username).Scan(&role)
		c.Set("admin_role", role)

		c.Next()
	}
}

// RequireAdminMiddleware 要求 admin 角色才能访问
func RequireAdminMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		role, exists := c.Get("admin_role")
		if !exists || role != "admin" {
			c.JSON(http.StatusForbidden, gin.H{"ok": false, "msg": "需要管理员权限"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// ============ 评论频率限制 ============

type rateLimitEntry struct {
	count    int
	expireAt time.Time
}

var (
	rateLimitStore = make(map[string]*rateLimitEntry)
	rateLimitMu    sync.Mutex
)

// 缓存限流配置，避免每个请求都查数据库
var (
	cachedLimitCount  int
	cachedLimitMinute int
	cachedLimitAt     time.Time
	cacheMu           sync.RWMutex

	sensitiveWordsCache struct {
		words    []string
		loadedAt time.Time
	}
	sensitiveCacheMu sync.RWMutex
)

// init 启动限流记录清理协程，每 5 分钟清理过期条目
func init() {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			rateLimitMu.Lock()
			for ip, entry := range rateLimitStore {
				if now.After(entry.expireAt) {
					delete(rateLimitStore, ip)
				}
			}
			rateLimitMu.Unlock()
		}
	}()
}

// getRateLimitConfig 获取限流配置（内存缓存 5 分钟）
func getRateLimitConfig(db *sql.DB) (int, int) {
	cacheMu.RLock()
	if time.Since(cachedLimitAt) < 5*time.Minute {
		count, minute := cachedLimitCount, cachedLimitMinute
		cacheMu.RUnlock()
		return count, minute
	}
	cacheMu.RUnlock()

	var limitCount, limitMinute int
	db.QueryRow("SELECT value FROM system_settings WHERE key='comment_limit_count'").Scan(&limitCount)
	db.QueryRow("SELECT value FROM system_settings WHERE key='comment_limit_minute'").Scan(&limitMinute)
	if limitCount <= 0 {
		limitCount = 2
	}
	if limitMinute <= 0 {
		limitMinute = 1
	}

	cacheMu.Lock()
	cachedLimitCount = limitCount
	cachedLimitMinute = limitMinute
	cachedLimitAt = time.Now()
	cacheMu.Unlock()
	return limitCount, limitMinute
}

// RateLimitMiddleware 评论频率限制中间件
func RateLimitMiddleware(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := getClientIP(c)
		limitCount, limitMinute := getRateLimitConfig(db)

		rateLimitMu.Lock()
		entry, exists := rateLimitStore[ip]

		if !exists || time.Now().After(entry.expireAt) {
			rateLimitStore[ip] = &rateLimitEntry{
				count:    1,
				expireAt: time.Now().Add(time.Duration(limitMinute) * time.Minute),
			}
			rateLimitMu.Unlock()
			c.Next()
			return
		}

		if entry.count >= limitCount {
			rateLimitMu.Unlock()
			c.JSON(http.StatusTooManyRequests, gin.H{
				"ok":  false,
				"msg": fmt.Sprintf("发言过于频繁，请 %d 分钟后再试", limitMinute),
			})
			c.Abort()
			return
		}

		entry.count++
		rateLimitMu.Unlock()
		c.Next()
	}
}

// ============ 敏感词过滤 ============

// getSensitiveWords 获取敏感词列表（内存缓存 5 分钟）
func getSensitiveWords(db *sql.DB) []string {
	sensitiveCacheMu.RLock()
	if time.Since(sensitiveWordsCache.loadedAt) < 5*time.Minute && sensitiveWordsCache.words != nil {
		words := sensitiveWordsCache.words
		sensitiveCacheMu.RUnlock()
		return words
	}
	sensitiveCacheMu.RUnlock()

	var words string
	db.QueryRow("SELECT value FROM system_settings WHERE key='sensitive_words'").Scan(&words)
	var result []string
	if words != "" {
		// 支持逗号和换行分隔
		for _, w := range strings.FieldsFunc(words, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
			w = strings.TrimSpace(w)
			if w != "" {
				result = append(result, w)
			}
		}
	}

	sensitiveCacheMu.Lock()
	sensitiveWordsCache.words = result
	sensitiveWordsCache.loadedAt = time.Now()
	sensitiveCacheMu.Unlock()
	return result
}

// SensitiveWordFilter 敏感词过滤中间件
func SensitiveWordFilter(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 只拦截评论和留言提交
		if c.Request.Method != "POST" {
			c.Next()
			return
		}
		path := c.Request.URL.Path
		if !strings.Contains(path, "/api/comment") && !strings.Contains(path, "/api/guestbook") {
			c.Next()
			return
		}

		wordList := getSensitiveWords(db)
		if len(wordList) == 0 {
			c.Next()
			return
		}

		content := c.PostForm("content")
		author := c.PostForm("author")
		for _, w := range wordList {
			if strings.Contains(content, w) || strings.Contains(author, w) {
				c.JSON(http.StatusForbidden, gin.H{
					"ok":  false,
					"msg": "内容包含敏感词汇，请修改后重新提交",
				})
				c.Abort()
				return
			}
		}

		c.Next()
	}
}

// ============ CSRF 防护 ============

// CSRFMiddleware 验证管理员 POST 请求的 Origin/Referer 防止跨站请求伪造
func CSRFMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == "GET" || c.Request.Method == "HEAD" || c.Request.Method == "OPTIONS" {
			c.Next()
			return
		}
		// 带 X-Requested-With 的 fetch 请求天然防 CSRF，直接放行
		if c.GetHeader("X-Requested-With") == "fetch" {
			c.Next()
			return
		}
		origin := c.Request.Header.Get("Origin")
		referer := c.Request.Header.Get("Referer")
		host := c.Request.Host
		// 浏览器原生表单提交必须有 Origin 或 Referer
		if origin == "" && referer == "" {
			c.JSON(http.StatusForbidden, gin.H{"ok": false, "msg": "CSRF 验证失败：缺少 Origin/Referer"})
			c.Abort()
			return
		}
		checkSameOrigin := func(rawURL string) bool {
			rawURL = strings.TrimPrefix(rawURL, "https://")
			rawURL = strings.TrimPrefix(rawURL, "http://")
			if idx := strings.Index(rawURL, "/"); idx >= 0 {
				rawURL = rawURL[:idx]
			}
			return rawURL == host
		}
		if (origin != "" && !checkSameOrigin(origin)) || (referer != "" && !checkSameOrigin(referer)) {
			c.JSON(http.StatusForbidden, gin.H{"ok": false, "msg": "CSRF 验证失败"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// ============ 登录频率限制 ============

var (
	loginAttemptStore = make(map[string]*loginAttemptEntry)
	loginAttemptMu    sync.Mutex
)

type loginAttemptEntry struct {
	count    int
	expireAt time.Time
}

func init() {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			loginAttemptMu.Lock()
			for ip, entry := range loginAttemptStore {
				if now.After(entry.expireAt) {
					delete(loginAttemptStore, ip)
				}
			}
			loginAttemptMu.Unlock()
		}
	}()
}

// LoginRateLimitMiddleware 登录频率限制（每 IP 每分钟最多 5 次尝试）
func LoginRateLimitMiddleware() gin.HandlerFunc {
	const maxAttempts = 5
	const windowMinutes = 1

	return func(c *gin.Context) {
		if c.Request.Method != "POST" || !strings.Contains(c.Request.URL.Path, "/admin/login") {
			c.Next()
			return
		}
		ip := getClientIP(c)
		loginAttemptMu.Lock()
		entry, exists := loginAttemptStore[ip]
		now := time.Now()
		if !exists || now.After(entry.expireAt) {
			loginAttemptStore[ip] = &loginAttemptEntry{count: 1, expireAt: now.Add(time.Duration(windowMinutes) * time.Minute)}
			loginAttemptMu.Unlock()
			c.Next()
			return
		}
		if entry.count >= maxAttempts {
			loginAttemptMu.Unlock()
			c.JSON(http.StatusTooManyRequests, gin.H{"ok": false, "msg": "登录尝试过于频繁，请1分钟后再试"})
			c.Abort()
			return
		}
		entry.count++
		loginAttemptMu.Unlock()
		c.Next()
	}
}
