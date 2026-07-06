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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"uniflow/models"
	"uniflow/utils"
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
		c.Header("Cache-Control", "no-cache, must-revalidate")
		c.Header("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval' https://*.amap.com https://*.autonavi.com; style-src 'self' 'unsafe-inline'; img-src * data: blob:; font-src 'self' data:; connect-src *; worker-src blob:; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		c.Next()
	}
}

// ============ 管理员会话管理 ============

type sessionEntry struct {
	username     string
	createdAt    time.Time
	lastActiveAt time.Time
	lastDBSync   time.Time // 上次同步到数据库的时间，限频用
}

var (
	sessionStore = make(map[string]sessionEntry) // token -> session (内存缓存)
	sessionMu    sync.RWMutex
	hmacSecret   []byte
	hmacInitDone bool
)

// InitHMACSecret 从数据库加载 HMAC 密钥，如果数据库不可用则回退到随机生成。
// 必须在 models.InitDB 之后调用。密钥持久化在 system_settings 表中，
// 服务重启后保持一致，已签发的 Cookie 不会因重启而失效。
func InitHMACSecret() {
	if hmacInitDone {
		return
	}
	keyStr := models.GetSetting("hmac_secret")
	if keyStr != "" {
		if decoded, err := hex.DecodeString(keyStr); err == nil && len(decoded) == 32 {
			hmacSecret = decoded
			hmacInitDone = true
			log.Println("[Auth] HMAC secret loaded from database")
			return
		}
	}
	// 回退：数据库不可用时生成临时密钥（重启后 Cookie 失效，但不阻塞启动）
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		log.Fatalf("[FATAL] crypto/rand failed to generate HMAC key: %v", err)
	}
	hmacSecret = key
	hmacInitDone = true
	log.Println("[WARN] HMAC secret generated in-memory (DB unavailable, cookies will not survive restart)")
}

// setSessionCookie 设置带 SameSite=Strict 的会话 cookie
func setSessionCookie(c *gin.Context, value string, maxAge int) {
	secure := shouldUseSecureCookie(c)
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

// shouldUseSecureCookie 判断是否应标记 Secure。
// 支持 FORCE_SECURE_COOKIE 环境变量（反向代理场景强制开启）。
func shouldUseSecureCookie(c *gin.Context) bool {
	// 环境变量强制开启（用于 Nginx/Caddy 反向代理 TLS 终止场景）
	if os.Getenv("FORCE_SECURE_COOKIE") == "true" {
		return true
	}
	// 本地开发 HTTP 环境允许非 Secure
	if c.Request.TLS == nil {
		host := c.Request.Host
		if strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "127.0.0.1") {
			return false
		}
		// 非 localhost 的 HTTP 请求：如果部署在反向代理后面，
		// 仍然可能是 TLS，保守起见设为 true
		return true
	}
	return true
}

// GenerateSession 创建管理员会话，返回 token（同时持久化到数据库）
func GenerateSession(username string) string {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		log.Printf("[ERROR] crypto/rand failed for session token: %v", err)
		return ""
	}
	tokenStr := hex.EncodeToString(token)
	now := time.Now()

	sessionMu.Lock()
	sessionStore[tokenStr] = sessionEntry{username: username, createdAt: now, lastActiveAt: now, lastDBSync: now}
	sessionMu.Unlock()

	// 持久化到数据库，重启后仍可恢复
	if models.DB != nil {
		models.DB.Exec("INSERT OR REPLACE INTO sessions (token, username, created_at, last_active_at) VALUES (?, ?, ?, ?)",
			tokenStr, username, now.Format("2006-01-02 15:04:05"), now.Format("2006-01-02 15:04:05"))
	}

	return tokenStr
}

// ValidateSession 验证会话 token，返回用户名（带滑动窗口续期）
func ValidateSession(token string) (string, bool) {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	e, ok := sessionStore[token]
	if ok {
		// 内存命中
		if time.Since(e.lastActiveAt) > 7*24*time.Hour {
			delete(sessionStore, token)
			if models.DB != nil {
				models.DB.Exec("DELETE FROM sessions WHERE token = ?", token)
			}
			return "", false
		}
		now := time.Now()
		e.lastActiveAt = now
		sessionStore[token] = e
		// 限频异步更新数据库（每 5 分钟一次），避免 MaxOpenConns=1 下写压力
		if models.DB != nil && now.Sub(e.lastDBSync) > 5*time.Minute {
			e.lastDBSync = now
			sessionStore[token] = e
			go models.DB.Exec("UPDATE sessions SET last_active_at = ? WHERE token = ?",
				now.Format("2006-01-02 15:04:05"), token)
		}
		return e.username, true
	}

	// 内存未命中，尝试从数据库恢复（服务重启后）
	if models.DB != nil {
		var username string
		var lastActiveStr string
		err := models.DB.QueryRow("SELECT username, last_active_at FROM sessions WHERE token = ?", token).Scan(&username, &lastActiveStr)
		if err == nil {
			lastActive, parseErr := time.Parse("2006-01-02 15:04:05", lastActiveStr)
			if parseErr != nil {
				lastActive = time.Now()
			}
			if time.Since(lastActive) > 7*24*time.Hour {
				models.DB.Exec("DELETE FROM sessions WHERE token = ?", token)
				return "", false
			}
			// 恢复到内存缓存
			now := time.Now()
			sessionStore[token] = sessionEntry{username: username, createdAt: lastActive, lastActiveAt: now, lastDBSync: now}
			return username, true
		}
	}

	return "", false
}

// RevokeSession 撤销会话
func RevokeSession(token string) {
	sessionMu.Lock()
	delete(sessionStore, token)
	sessionMu.Unlock()
	if models.DB != nil {
		models.DB.Exec("DELETE FROM sessions WHERE token = ?", token)
	}
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
	likeLimitStore = make(map[string]*rateLimitEntry)
	likeLimitMu    sync.Mutex
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

			likeLimitMu.Lock()
			for ip, entry := range likeLimitStore {
				if now.After(entry.expireAt) {
					delete(likeLimitStore, ip)
				}
			}
			likeLimitMu.Unlock()

			// 清理过期 session（超过 7 天未活跃），避免内存泄漏
			sessionMu.Lock()
			for token, e := range sessionStore {
				if now.Sub(e.lastActiveAt) > 7*24*time.Hour {
					delete(sessionStore, token)
				}
			}
			sessionMu.Unlock()
			// 同步清理数据库中的过期 session
			if models.DB != nil {
				cutoff := now.Add(-7 * 24 * time.Hour).Format("2006-01-02 15:04:05")
				models.DB.Exec("DELETE FROM sessions WHERE last_active_at < ?", cutoff)
			}
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

// LikeRateLimitMiddleware 点赞/拍砖轻量限流，防止公开接口被脚本刷量。
func LikeRateLimitMiddleware() gin.HandlerFunc {
	const maxLikes = 30
	const window = time.Minute

	return func(c *gin.Context) {
		ip := getClientIP(c)
		now := time.Now()
		likeLimitMu.Lock()
		entry, exists := likeLimitStore[ip]
		if !exists || now.After(entry.expireAt) {
			likeLimitStore[ip] = &rateLimitEntry{count: 1, expireAt: now.Add(window)}
			likeLimitMu.Unlock()
			c.Next()
			return
		}
		if entry.count >= maxLikes {
			likeLimitMu.Unlock()
			c.JSON(http.StatusTooManyRequests, gin.H{"ok": false, "msg": "点赞过于频繁，请稍后再试"})
			c.Abort()
			return
		}
		entry.count++
		likeLimitMu.Unlock()
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
		// 同时对净化后的内容做检测，防止用 HTML 标签拆分敏感词绕过（如 <b>敏</b>感词）
		sanitized := utils.SanitizeHTML(content)
		for _, w := range wordList {
			if strings.Contains(content, w) || strings.Contains(author, w) || strings.Contains(sanitized, w) {
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

		origin := c.Request.Header.Get("Origin")
		referer := c.Request.Header.Get("Referer")
		host := c.Request.Host

		// 写操作必须携带 Origin 或 Referer，fetch 请求也必须同源，不能只依赖自定义头。
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
