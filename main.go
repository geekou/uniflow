package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"uniflow/handlers"
	"uniflow/models"
	"uniflow/utils"

	"github.com/gin-gonic/gin"
)

// Version 当前版本号
const Version = "v1.6.1"

// splitWords 按字母/数字连续段统计英文单词。
func splitWords(s string) []string {
	var words []string
	var cur []rune
	for _, r := range s {
		if r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			cur = append(cur, r)
			continue
		}
		if len(cur) > 0 {
			words = append(words, string(cur))
			cur = nil
		}
	}
	if len(cur) > 0 {
		words = append(words, string(cur))
	}
	return words
}

func main() {
	// 设置时区为中国标准时间
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		time.Local = loc
	} else {
		log.Printf("[WARN] load Asia/Shanghai timezone failed: %v, using UTC", err)
	}

	// 获取项目根目录
	projectRoot, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to get working directory: %v", err)
	}

	// 初始化数据库
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = filepath.Join(projectRoot, "uniflow.db")
	}
	if err := models.InitDB(dbPath); err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer models.CloseDB()

	// 从数据库加载持久化的 HMAC 密钥（必须在 InitDB 之后）
	handlers.InitHMACSecret()

	// 设置 Gin 模式（生产环境默认 release）
	mode := os.Getenv("GIN_MODE")
	if mode == "" {
		mode = gin.ReleaseMode
	}
	gin.SetMode(mode)

	r := gin.Default()
	_ = r.SetTrustedProxies(nil) // 禁用代理头，防止 IP 伪造绕过限流

	// ============ 中间件：安全响应头 ============
	r.Use(handlers.SecurityHeadersMiddleware())

	// ============ 中间件：全局请求体大小限制 ============
	r.Use(func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 500<<20)
		c.Next()
	})

	// ============ 中间件：检查是否已初始化 ============
	r.Use(handlers.SetupCheckMiddleware(models.DB))
	r.MaxMultipartMemory = 128 << 20 // 128MB

	// ============ 中间件：注入 project_root ============
	r.Use(func(c *gin.Context) {
		c.Set("project_root", projectRoot)
		c.Next()
	})

	// ============ 首次部署引导（无需认证，使用基础 CSRF 防护） ============
	r.GET("/setup", handlers.SetupPage(models.DB))
	r.POST("/setup", handlers.CSRFMiddleware(), handlers.SetupPost(models.DB))

	// ============ 静态资源 ============
	r.Static("/static", filepath.Join(projectRoot, "static"))
	r.Static("/uploads", filepath.Join(projectRoot, "uploads"))
	// 注意: /backups 不作为公开静态目录暴露，防止数据库备份被任意下载
	// 后台管理通过 admin handlers 提供备份下载

	// ============ 加载模板（注册自定义函数） ============
	funcMap := template.FuncMap{
		"firstChar": func(s string) string {
			if s == "" {
				return "?"
			}
			r := []rune(s)
			return string(r[0:1])
		},
		"defaultName": func(s string) string {
			if s == "" {
				return "user"
			}
			return s
		},
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
		"mul": func(a, b int) int { return a * b },
		"eq":  func(a, b interface{}) bool { return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b) },
		"ne":  func(a, b interface{}) bool { return fmt.Sprintf("%v", a) != fmt.Sprintf("%v", b) },
		"gt":  func(a, b int) bool { return a > b },
		"lt":  func(a, b int) bool { return a < b },
		"truncate": func(s string, n int) string {
			runes := []rune(s)
			if len(runes) <= n {
				return s
			}
			return string(runes[:n]) + "..."
		},
		"stripHTML":       utils.StripHTML,
		"safeURL":         utils.SafeURL,
		"safeImageURL":    utils.SafeImageURL,
		"safeMenuURL":     utils.SafeMenuURL,
		"safeExternalURL": utils.SafeExternalURL,
		"json": func(v interface{}) (template.JS, error) {
			b, err := json.Marshal(v)
			if err != nil {
				return template.JS("{}"), err
			}
			return template.JS(b), nil
		},
		"dict": func(keysAndValues ...interface{}) (map[string]interface{}, error) {
			if len(keysAndValues)%2 != 0 {
				return nil, fmt.Errorf("dict requires even number of arguments")
			}
			m := make(map[string]interface{}, len(keysAndValues)/2)
			for i := 0; i < len(keysAndValues); i += 2 {
				key, ok := keysAndValues[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict key must be string, got %T", keysAndValues[i])
				}
				m[key] = keysAndValues[i+1]
			}
			return m, nil
		},
		"safeHTML": func(s string) template.HTML {
			return template.HTML(utils.SanitizeHTML(s))
		},
		"split": func(s, sep string) []string { return strings.Split(s, sep) },
		"isVideo": func(s string) bool {
			ext := strings.ToLower(s)
			for _, v := range []string{".mp4", ".webm", ".mov", ".avi"} {
				if strings.HasSuffix(ext, v) {
					return true
				}
			}
			return false
		},
		"formatTime": func(t time.Time) string {
			return t.In(time.Local).Format("2006-01-02 15:04")
		},
		"wordCount": func(htmlContent string) int {
			text := utils.StripHTML(htmlContent)
			count := 0
			for _, r := range text {
				if unicode.Is(unicode.Han, r) {
					count++
				}
			}
			for _, w := range splitWords(text) {
				if w != "" {
					count++
				}
			}
			return count
		},
		"readingTime": func(wordCount int) int {
			// 中文阅读速度约 400 字/分钟，至少 1 分钟
			t := (wordCount + 399) / 400
			if t < 1 {
				t = 1
			}
			return t
		},
		"formatDate": func(t time.Time) string {
			return t.In(time.Local).Format("2006-01-02")
		},
		"relativeTime": func(t time.Time) string {
			now := time.Now()
			local := t.In(time.Local)
			diff := now.Sub(local)
			if diff < time.Minute {
				return "刚刚"
			} else if diff < time.Hour {
				return fmt.Sprintf("%d分钟前", int(diff.Minutes()))
			} else if diff < 24*time.Hour {
				return fmt.Sprintf("%d小时前", int(diff.Hours()))
			} else if diff < 48*time.Hour {
				return fmt.Sprintf("昨天 %s", local.Format("15:04"))
			} else if diff < 7*24*time.Hour {
				return fmt.Sprintf("%d天前", int(diff.Hours()/24))
			}
			return local.Format("01-02 15:04")
		},
		"formatDatetimeLocal": func(t time.Time) string {
			return t.In(time.Local).Format("2006-01-02T15:04")
		},
		"buildPageRange": func(page, totalPages int) []int {
			if totalPages <= 7 {
				r := make([]int, totalPages)
				for i := 0; i < totalPages; i++ {
					r[i] = i + 1
				}
				return r
			}
			var result []int
			result = append(result, 1)
			if page > 3 {
				result = append(result, -1)
			}
			s, e := page-1, page+1
			if s < 2 {
				s = 2
			}
			if e > totalPages-1 {
				e = totalPages - 1
			}
			for i := s; i <= e; i++ {
				result = append(result, i)
			}
			if page < totalPages-2 {
				result = append(result, -1)
			}
			result = append(result, totalPages)
			return result
		},
	}

	tmpl := template.New("").Funcs(funcMap)
	templateDir := filepath.Join(projectRoot, "templates")

	filepath.Walk(templateDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(path), ".html") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// 计算相对于 templateDir 的路径作为模板名
		rel, _ := filepath.Rel(templateDir, path)
		name := filepath.ToSlash(rel) // "admin/login.html" or "index.html"

		_, err = tmpl.New(name).Parse(string(data))
		if err != nil {
			log.Printf("[Template] Parse error %s: %v", name, err)
			return nil
		}
		return nil
	})

	r.SetHTMLTemplate(tmpl)

	// ============ 数据库引用 ============
	db := models.DB

	// ===============================================
	//   前台路由
	// ===============================================
	r.GET("/", handlers.IndexHandler(db))
	r.GET("/posts", handlers.PostsListHandler(db))
	r.GET("/moments", handlers.MomentsListHandler(db))
	r.GET("/post/:id", handlers.PostDetailHandler(db))
	r.GET("/page/:slug", handlers.CustomPageHandler(db))
	r.GET("/search", handlers.SearchHandler(db))
	r.GET("/rss", handlers.RSSHandler(db))
	r.GET("/guestbook", handlers.GuestbookHandler(db))
	r.GET("/about", handlers.AboutHandler(db))
	r.GET("/maptest", handlers.MapTestHandler)

	// ===============================================
	//   前台 API - 只读（无频率限制）
	// ===============================================
	r.GET("/api/stats/heatmap", handlers.StatsHeatmapHandler(db))
	likeAPI := r.Group("/api")
	likeAPI.Use(handlers.LikeRateLimitMiddleware())
	{
		likeAPI.POST("/post/like/:id", handlers.PostLikeHandler(db))
		likeAPI.POST("/post/dislike/:id", handlers.PostDislikeHandler(db))
	}
	r.GET("/api/auth/check", handlers.AuthCheck(db))

	// ===============================================
	//   前台 API - 写操作（带频率限制和敏感词过滤）
	// ===============================================
	api := r.Group("/api")
	api.Use(handlers.RateLimitMiddleware(db))
	api.Use(handlers.SensitiveWordFilter(db))
	{
		api.POST("/comment", handlers.CommentSubmit(db))
		api.POST("/comment/like", handlers.CommentLike(db))
		api.GET("/footprints", handlers.FootprintsAPI(db))
		api.POST("/guestbook", handlers.GuestbookSubmit(db))
	}

	// 瞬间点赞（轻量限流，防刷量）
	momentLikeAPI := r.Group("/api")
	momentLikeAPI.Use(handlers.LikeRateLimitMiddleware())
	{
		momentLikeAPI.POST("/moment/like/:id", handlers.MomentLikeHandler(db))
	}

	// ===============================================
	//   后台管理路由
	// ===============================================
	admin := r.Group("/admin")

	// 登录页（无需认证，带频率限制）
	admin.GET("/login", handlers.AdminLoginGet)
	admin.POST("/login", handlers.LoginRateLimitMiddleware(), handlers.AdminLoginPost(db))

	// 退出登录
	admin.GET("/logout", handlers.AdminLogout)

	// 需要认证 + CSRF 防护的后台路由
	auth := admin.Group("", handlers.AuthMiddleware(), handlers.CSRFMiddleware())
	{
		// 仪表盘
		auth.GET("/", handlers.AdminDashboard(db, Version))
		auth.GET("/api/version/check", handlers.AdminCheckUpdate(Version))

		// 文章管理
		auth.GET("/posts", handlers.AdminPostList(db))
		auth.GET("/posts/create", handlers.AdminPostCreate(db))
		auth.POST("/posts/create", handlers.AdminPostCreatePost(db))
		auth.GET("/posts/edit/:id", handlers.AdminPostEdit(db))
		auth.POST("/posts/edit/:id", handlers.AdminPostEditPost(db))
		auth.POST("/posts/delete/:id", handlers.AdminPostDelete(db))
		auth.GET("/posts/export/:id", handlers.AdminPostExport(db))

		// 分类管理
		auth.GET("/categories", handlers.AdminCategories(db))
		auth.POST("/categories/save", handlers.AdminCategorySave(db))
		auth.POST("/categories/delete/:id", handlers.AdminCategoryDelete(db))

		// 标签管理
		auth.GET("/tags", handlers.AdminTags(db))
		auth.POST("/tags/save", handlers.AdminTagSave(db))
		auth.POST("/tags/delete/:id", handlers.AdminTagDelete(db))

		// 瞬间管理
		auth.GET("/moments", handlers.AdminMomentList(db))
		auth.GET("/moments/create", handlers.AdminMomentCreate(db))
		auth.POST("/moments/create", handlers.AdminMomentCreatePost(db))
		auth.GET("/moments/edit/:id", handlers.AdminMomentEdit(db))
		auth.POST("/moments/edit/:id", handlers.AdminMomentEditPost(db))
		auth.POST("/moments/delete/:id", handlers.AdminMomentDelete(db))

		// 消息管理
		auth.GET("/comments", handlers.AdminComments(db))
		auth.POST("/comments/delete/:id", handlers.AdminCommentDelete(db))
		auth.POST("/comments/reply/:id", handlers.AdminCommentReply(db))
		auth.GET("/guestbook", handlers.AdminGuestbook(db))
		auth.POST("/guestbook/reply/:id", handlers.AdminGuestbookReply(db))
		auth.POST("/guestbook/delete/:id", handlers.AdminGuestbookDelete(db))

		// 媒体管理
		auth.GET("/photos", func(c *gin.Context) { c.Redirect(http.StatusFound, "/admin/media?type=photos") })
		auth.GET("/videos", func(c *gin.Context) { c.Redirect(http.StatusFound, "/admin/media?type=videos") })
		auth.GET("/media", handlers.AdminMedia(db))
		auth.GET("/media/api", handlers.AdminMediaAPI(db))
		auth.POST("/upload", handlers.AdminUpload(db))
		auth.POST("/media/delete/:name", handlers.AdminMediaDelete(db))
		auth.POST("/media/batch-delete", handlers.AdminMediaBatchDelete(db))

		// 网站设置（仅 admin）
		adminOnly := auth.Group("", handlers.RequireAdminMiddleware())
		adminOnly.GET("/settings", handlers.AdminSettings(db))
		adminOnly.POST("/settings", handlers.AdminSettingsPost(db))
		adminOnly.GET("/about", handlers.AdminAbout(db))
		adminOnly.POST("/about", handlers.AdminAboutPost(db))

		// 设备管理（API）
		adminOnly.POST("/devices/save", handlers.AdminDeviceSave(db))
		adminOnly.POST("/devices/delete/:id", handlers.AdminDeviceDelete(db))

		// 足迹管理（API）
		adminOnly.POST("/footprints/save", handlers.AdminFootprintSave(db))
		adminOnly.POST("/footprints/delete/:index", handlers.AdminFootprintDelete(db))
		adminOnly.GET("/sitemap", handlers.AdminSitemap(db))

		// 系统管理（仅 admin）
		adminOnly.GET("/users", handlers.AdminUsers(db))
		adminOnly.POST("/users/save", handlers.AdminUserSave(db))
		adminOnly.POST("/users/update/:id", handlers.AdminUserUpdate(db))
		adminOnly.POST("/users/delete/:id", handlers.AdminUserDelete(db))
		adminOnly.POST("/users/reset-password/:id", handlers.AdminUserResetPassword(db))

		adminOnly.GET("/menus", handlers.AdminMenus(db))
		adminOnly.POST("/menus/save", handlers.AdminMenuSave(db))
		adminOnly.POST("/menus/update", handlers.AdminMenuUpdate(db))
		adminOnly.POST("/menus/delete/:id", handlers.AdminMenuDelete(db))

		adminOnly.GET("/pages", handlers.AdminPages(db))
		adminOnly.GET("/pages/create", handlers.AdminPageCreate(db))
		adminOnly.POST("/pages/create", handlers.AdminPageCreatePost(db))
		adminOnly.GET("/pages/edit/:id", handlers.AdminPageEdit(db))
		adminOnly.POST("/pages/edit/:id", handlers.AdminPageEditPost(db))
		adminOnly.POST("/pages/delete/:id", handlers.AdminPageDelete(db))

		adminOnly.GET("/backup", handlers.AdminBackup(db))
		adminOnly.POST("/backup/create", handlers.AdminBackupCreate(db))
		adminOnly.POST("/backup/restore/:name", handlers.AdminBackupRestore(db))
		adminOnly.POST("/backup/upload", handlers.AdminBackupUpload(db))
		adminOnly.POST("/backup/delete/:name", handlers.AdminBackupDelete(db))
		adminOnly.GET("/backup/download/:name", handlers.AdminBackupDownload(db))

		adminOnly.GET("/logs", handlers.AdminLogs(db))
	}

	// ===============================================
	//   404
	// ===============================================
	r.NoRoute(func(c *gin.Context) {
		// 加载热门文章作为推荐
		type hotLink struct {
			ID    int64
			Title string
		}
		var hotPosts []hotLink
		hotRows, _ := db.Query("SELECT id, title FROM posts WHERE status='published' AND privacy='public' ORDER BY views DESC LIMIT 4")
		if hotRows != nil {
			for hotRows.Next() {
				var hp hotLink
				if err := hotRows.Scan(&hp.ID, &hp.Title); err == nil {
					hotPosts = append(hotPosts, hp)
				}
			}
			hotRows.Close()
		}

		hotHTML := ""
		if len(hotPosts) > 0 {
			hotHTML = `<div class="mt-8 grid grid-cols-1 sm:grid-cols-2 gap-3 max-w-lg mx-auto">`
			for _, hp := range hotPosts {
				hotHTML += fmt.Sprintf(
					`<a href="/post/%d" class="block p-3 bg-white rounded-xl border border-gray-100 hover:border-indigo-200 hover:shadow-sm transition text-left text-sm text-gray-700 hover:text-indigo-500 truncate">%s</a>`,
					hp.ID, html.EscapeString(hp.Title),
				)
			}
			hotHTML += `</div>`
		}

		c.HTML(http.StatusNotFound, "page.html", gin.H{
			"SiteTitle":    "UniFlow",
			"SiteSubtitle": "",
			"Menus":        []models.Menu{},
			"PageData": gin.H{
				"Title": "页面未找到",
				"HTMLContent": template.HTML(fmt.Sprintf(`<div class="text-center py-8 sm:py-12">
<div class="text-8xl font-extrabold bg-gradient-to-br from-indigo-400 via-purple-400 to-pink-400 bg-clip-text text-transparent select-none mb-4">404</div>
<p class="text-gray-500 text-sm mb-8">抱歉，你要找的页面走丢啦～</p>
<div class="flex flex-col sm:flex-row gap-3 justify-center">
    <a href="/" class="inline-flex items-center gap-2 px-5 py-2.5 bg-indigo-500 text-white text-sm font-medium rounded-xl hover:bg-indigo-600 active:scale-95 transition">
        <span class="iconify" data-icon="mdi:home-outline" data-width="18"></span> 返回首页
    </a>
</div>
<p class="text-xs text-gray-300 mt-8">或者看看下面的热门文章</p>
%s</div>`, hotHTML)),
			},
		})
	})

	// ===============================================
	//   定时发布调度器（每分钟检查）
	// ===============================================
	schedulerCtx, schedulerCancel := context.WithCancel(context.Background())
	defer schedulerCancel()
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				handlers.PublishScheduledPosts(db)
			case <-schedulerCtx.Done():
				return
			}
		}
	}()

	// ============ 启动服务 ============
	port := os.Getenv("PORT")
	if port == "" {
		port = "9090"
	}

	log.Printf("========================================")
	log.Printf("  UniFlow is running at http://localhost:%s", port)
	log.Printf("  Admin panel:  http://localhost:%s/admin", port)
	log.Printf("  Setup: http://localhost:%s/setup", port)
	log.Printf("  Project root: %s", projectRoot)
	log.Printf("========================================")

	if err := r.Run(":" + port); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
