package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"uniflow/models"
	"uniflow/utils"
)

// maxUploadSize 上传文件最大大小（500MB）
const maxUploadSize int64 = 500 << 20

var setupMu sync.Mutex

// restoreMu 保护备份恢复操作：恢复期间数据库连接会被关闭重建，
// 用 RWMutex 让正在进行的请求通过 TryLock 快速失败返回 503，
// 同时确保恢复操作串行化，避免与并发请求/定时任务冲突。
var restoreMu sync.RWMutex

// restoreInProgress 在恢复期间设为 true，供不经过 restoreMu 的代码路径
// （如 SetupCheckMiddleware）检测恢复状态并返回 503。
var restoreInProgress atomic.Bool

// ============ 通用辅助函数 ============

// execLog 执行 SQL 并记录错误（用于非关键 db.Exec 调用）
func execLog(db *sql.DB, query string, args ...interface{}) {
	if _, err := db.Exec(query, args...); err != nil {
		log.Printf("[DB] Exec error: %v, query: %s", err, query)
	}
}

func adminData(title, activeMenu, activeSubmenu, username string, db *sql.DB) gin.H {
	var foundedAt string
	if db != nil {
		db.QueryRow("SELECT value FROM system_settings WHERE key='site_founded_at'").Scan(&foundedAt)
	}
	return gin.H{
		"PageTitle":     title,
		"SiteTitle":     "UniFlow",
		"ActiveMenu":    activeMenu,
		"ActiveSubmenu": activeSubmenu,
		"Username":      username,
		"SiteFoundedAt": foundedAt,
	}
}

func getAdminUsername(c *gin.Context) string {
	v, _ := c.Get("admin_username")
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// ============ 登录 ============

func AdminLoginGet(c *gin.Context) {
	c.HTML(http.StatusOK, "admin/login.html", gin.H{"SiteTitle": "UniFlow"})
}

func AdminLoginPost(db *sql.DB) gin.HandlerFunc {
	// 预生成假 hash 用于时序攻击防护
	fakeHash, _ := bcrypt.GenerateFromPassword([]byte("dummy"), bcrypt.DefaultCost)

	return func(c *gin.Context) {
		username := c.PostForm("username")
		password := c.PostForm("password")
		ip := getClientIP(c)

		var user models.User
		err := db.QueryRow("SELECT id, username, password, role FROM users WHERE username = ?", username).
			Scan(&user.ID, &user.Username, &user.Password, &user.Role)

		if err != nil {
			// 用户不存在时也跑 bcrypt 抹平时序差异
			bcrypt.CompareHashAndPassword(fakeHash, []byte(password))
			db.Exec("INSERT INTO logs (log_type, operator, action, ip, user_agent, result) VALUES ('login', ?, ?, ?, ?, 'failure')",
				username, "login", ip, c.Request.UserAgent())
			c.HTML(http.StatusOK, "admin/login.html", gin.H{
				"SiteTitle": "UniFlow",
				"Error":     "用户名或密码错误",
			})
			return
		}

		if bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password)) != nil {
			db.Exec("INSERT INTO logs (log_type, operator, action, ip, user_agent, result) VALUES ('login', ?, ?, ?, ?, 'failure')",
				username, "login", ip, c.Request.UserAgent())
			c.HTML(http.StatusOK, "admin/login.html", gin.H{
				"SiteTitle": "UniFlow",
				"Error":     "用户名或密码错误",
			})
			return
		}

		// 记录登录日志
		db.Exec("INSERT INTO logs (log_type, operator, action, ip, user_agent, result) VALUES ('login', ?, ?, ?, ?, 'success')",
			username, "login", ip, c.Request.UserAgent())

		// 创建会话
		token := GenerateSession(username)
		signed := SignCookie(token)
		setSessionCookie(c, signed, 86400*7)

		c.Redirect(http.StatusFound, "/admin")
	}
}

func AdminLogout(c *gin.Context) {
	cookie, _ := c.Cookie("uniflow_session")
	if token, ok := VerifyCookie(cookie); ok {
		RevokeSession(token)
	}
	c.SetCookie("uniflow_session", "", -1, "/", "", false, true)
	c.Redirect(http.StatusFound, "/admin/login")
}

// ============ 仪表盘 ============

func AdminDashboard(db *sql.DB, version string) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("仪表盘", "dashboard", "", getAdminUsername(c), db)

		// 合并为单条 SQL，减少数据库查询次数
		var postCount, momentCount, commentCount, guestbookCount int
		db.QueryRow(
			"SELECT (SELECT COUNT(*) FROM posts), (SELECT COUNT(*) FROM moments), (SELECT COUNT(*) FROM comments), (SELECT COUNT(*) FROM guestbook)",
		).Scan(&postCount, &momentCount, &commentCount, &guestbookCount)
		data["Stats"] = map[string]int{
			"PostCount": postCount, "MomentCount": momentCount,
			"CommentCount": commentCount, "GuestbookCount": guestbookCount,
		}
		data["Now"] = time.Now().Format("2006-01-02 15:04")
		data["Version"] = version

		c.HTML(http.StatusOK, "admin/dashboard.html", data)
	}
}

// ============ 文章管理 ============

func AdminPostList(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("文章列表", "posts", "post_list", getAdminUsername(c), db)
		data["Mode"] = "list"

		// 搜索和筛选
		searchQuery := strings.TrimSpace(c.Query("q"))
		filterStatus := strings.TrimSpace(c.Query("status"))
		data["SearchQuery"] = searchQuery
		data["FilterStatus"] = filterStatus

		page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
		if page < 1 {
			page = 1
		}
		pageSize := 15
		offset := (page - 1) * pageSize

		// 构建 WHERE 条件
		where := "WHERE 1=1"
		var args []interface{}
		if searchQuery != "" {
			where += " AND p.title LIKE ?"
			args = append(args, "%"+searchQuery+"%")
		}
		if filterStatus != "" {
			where += " AND p.status = ?"
			args = append(args, filterStatus)
		}

		var total int
		db.QueryRow("SELECT COUNT(*) FROM posts p "+where, args...).Scan(&total)

		queryArgs := append(args, pageSize, offset)
		rows, _ := db.Query(
			"SELECT p.id, p.title, p.status, p.views, p.created_at, c.name FROM posts p LEFT JOIN categories c ON p.category_id = c.id "+where+" ORDER BY p.id DESC LIMIT ? OFFSET ?",
			queryArgs...,
		)
		var posts []gin.H
		if rows != nil {
			for rows.Next() {
				var p struct {
					ID           int64
					Title        string
					Status       string
					Views        int64
					CreatedAt    time.Time
					CategoryName sql.NullString
				}
				scanLog(rows.Scan(&p.ID, &p.Title, &p.Status, &p.Views, &p.CreatedAt, &p.CategoryName), "adminPosts")
				catName := ""
				if p.CategoryName.Valid {
					catName = p.CategoryName.String
				}
				posts = append(posts, gin.H{
					"ID": p.ID, "Title": p.Title, "Status": p.Status,
					"Views": p.Views, "CreatedAt": p.CreatedAt,
					"CategoryName": catName,
				})
			}
			rows.Close()
		}

		totalPages := (total + pageSize - 1) / pageSize
		data["Posts"] = posts
		data["Page"] = page
		data["TotalPages"] = totalPages
		c.HTML(http.StatusOK, "admin/post.html", data)
	}
}

func normalizePostPrivacy(privacy string) string {
	if privacy != "public" && privacy != "private" {
		return "public"
	}
	return privacy
}

func normalizePostStatus(status string) string {
	if status != "published" && status != "draft" && status != "scheduled" {
		return "draft"
	}
	return status
}

func AdminPostCreate(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("发布文章", "posts", "post_create", getAdminUsername(c), db)
		data["Mode"] = "create"
		data["Post"] = models.Post{} // 传入Post对象，避免模板nil
		data["PostContentJS"] = ""
		data["IsTop"] = false
		data["Privacy"] = "public"
		data["PublishAt"] = ""
		data["PostCover"] = ""
		data["SelectedTagIDs"] = []int64{}
		data["PostID"] = 0

		loadCategoriesAndTags(db, data)
		c.HTML(http.StatusOK, "admin/post.html", data)
	}
}

func AdminPostCreatePost(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		title := strings.TrimSpace(c.PostForm("title"))
		content := utils.SanitizeHTML(c.PostForm("content"))
		if title == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "标题不能为空"})
			return
		}
		categoryID, _ := strconv.ParseInt(c.PostForm("category_id"), 10, 64)
		isTop, _ := strconv.Atoi(c.PostForm("is_top"))
		privacy := normalizePostPrivacy(c.PostForm("privacy"))
		status := normalizePostStatus(c.PostForm("status"))
		publishAt := c.PostForm("publish_at")

		// 定时发布：如果 publish_at 有值且在未来，强制 status=scheduled
		if publishAt != "" {
			if t, err := time.ParseInLocation("2006-01-02T15:04", publishAt, time.Local); err == nil && t.After(time.Now()) {
				status = "scheduled"
			}
		}

		thumbURL := utils.SafeImageURL(c.PostForm("thumb_url"))
		// 处理封面图上传
		file, header, err := c.Request.FormFile("cover")
		if err == nil {
			defer file.Close()
			projectRoot := getProjectRoot(c)
			uploadsDir := filepath.Join(projectRoot, "uploads")
			os.MkdirAll(uploadsDir, 0755)
			tmpPath := filepath.Join(uploadsDir, "tmp_cover_"+uuid.New().String()+filepath.Ext(filepath.Base(header.Filename)))
			dst, err := os.Create(tmpPath)
			if err == nil {
				written, _ := io.Copy(dst, io.LimitReader(file, maxUploadSize+1))
				dst.Close()
				if written > maxUploadSize {
					os.Remove(tmpPath)
					c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "文件大小超过500MB限制"})
					return
				}
				filename, _, _ := utils.ProcessUploadedFile(tmpPath, uploadsDir)
				if filename != "" {
					thumbURL = "/uploads/" + filename
				}
			}
		}
		if thumbURL == "" {
			thumbURL = utils.SafeImageURL(c.PostForm("thumb_url"))
		}

	result, err := db.Exec(
		"INSERT INTO posts (title, content, thumb_url, author, category_id, is_top, privacy, status, publish_at) VALUES (?,?,?,?,?,?,?,?,?)",
		title, content, thumbURL, getAdminUsername(c), nilIfZero(categoryID), isTop, privacy, status, formatPublishAt(publishAt),
	)
			if err != nil {
				log.Printf("[AdminPostCreate] save error: %v", err)
				c.String(500, "保存失败，请检查输入或联系管理员")
				return
			}

		postID, _ := result.LastInsertId()

		// 保存标签关联
		tags := c.PostFormArray("tags")
		for _, tagIDStr := range tags {
			tagID, _ := strconv.ParseInt(tagIDStr, 10, 64)
			if tagID > 0 {
				execLog(db, "INSERT INTO post_tags (post_id, tag_id) VALUES (?, ?)", postID, tagID)
			}
		}

		username := getAdminUsername(c)
		logOperation(db, username, c, fmt.Sprintf("发布文章: %s", title))

		c.Redirect(http.StatusFound, "/admin/posts")
	}
}

func AdminPostEdit(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("编辑文章", "posts", "post_list", getAdminUsername(c), db)
		data["Mode"] = "edit"

		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if id <= 0 {
			c.Redirect(http.StatusFound, "/admin/posts")
			return
		}
		data["PostID"] = id

		var p models.Post
		err := db.QueryRow(
			"SELECT id, title, content, thumb_url, author, views, category_id, is_top, privacy, status, publish_at, created_at, updated_at FROM posts WHERE id = ?", id,
		).Scan(&p.ID, &p.Title, &p.Content, &p.ThumbURL, &p.Author, &p.Views, &p.CategoryID, &p.IsTop, &p.Privacy, &p.Status, &p.PublishAt, &p.CreatedAt, &p.UpdatedAt)
		if err != nil {
			c.String(404, "文章不存在")
			return
		}

		data["Post"] = p
		contentJSON, _ := json.Marshal(p.Content)
		data["PostContentJS"] = template.JS(contentJSON)
		data["IsTop"] = p.IsTop == 1
		data["Privacy"] = p.Privacy
		data["PostCover"] = p.ThumbURL
		if p.PublishAt != nil {
			data["PublishAt"] = p.PublishAt.In(time.Local).Format("2006-01-02T15:04")
		} else {
			data["PublishAt"] = ""
		}

		// 获取已选标签
		tagRows, _ := db.Query("SELECT tag_id FROM post_tags WHERE post_id = ?", id)
		var selectedTagIDs []int64
		if tagRows != nil {
			for tagRows.Next() {
				var tid int64
				scanLog(tagRows.Scan(&tid), "adminSelectedTags")
				selectedTagIDs = append(selectedTagIDs, tid)
			}
			tagRows.Close()
		}
		data["SelectedTagIDs"] = selectedTagIDs

		loadCategoriesAndTags(db, data)
		c.HTML(http.StatusOK, "admin/post.html", data)
	}
}

func AdminPostEditPost(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if id <= 0 {
			c.Redirect(http.StatusFound, "/admin/posts")
			return
		}
		title := strings.TrimSpace(c.PostForm("title"))
		if title == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "标题不能为空"})
			return
		}
		if len([]rune(title)) > 200 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "标题不能超过200个字符"})
			return
		}
		content := utils.SanitizeHTML(c.PostForm("content"))
		categoryID, _ := strconv.ParseInt(c.PostForm("category_id"), 10, 64)
		isTop, _ := strconv.Atoi(c.PostForm("is_top"))
		privacy := normalizePostPrivacy(c.PostForm("privacy"))
		status := normalizePostStatus(c.PostForm("status"))
		publishAt := c.PostForm("publish_at")

		// 定时发布：如果 publish_at 有值且在未来，强制 status=scheduled
		if publishAt != "" {
			if t, err := time.ParseInLocation("2006-01-02T15:04", publishAt, time.Local); err == nil && t.After(time.Now()) {
				status = "scheduled"
			}
		}

		var existingThumb string
		thumbURL := ""
		// 获取当前 thumb_url 作为默认
		db.QueryRow("SELECT thumb_url FROM posts WHERE id = ?", id).Scan(&existingThumb)

		// 处理封面图上传
		file, header, err := c.Request.FormFile("cover")
		if err == nil {
			defer file.Close()
			projectRoot := getProjectRoot(c)
			uploadsDir := filepath.Join(projectRoot, "uploads")
			os.MkdirAll(uploadsDir, 0755)
			tmpPath := filepath.Join(uploadsDir, "tmp_cover_"+uuid.New().String()+filepath.Ext(filepath.Base(header.Filename)))
			dst, err := os.Create(tmpPath)
			if err == nil {
				written, _ := io.Copy(dst, io.LimitReader(file, maxUploadSize+1))
				dst.Close()
				if written > maxUploadSize {
					os.Remove(tmpPath)
					c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "文件大小超过500MB限制"})
					return
				}
				filename, size, procErr := utils.ProcessUploadedFile(tmpPath, uploadsDir)
				log.Printf("[Cover] uploaded: tmp=%s filename=%s size=%d err=%v", tmpPath, filename, size, procErr)
				if filename != "" {
					thumbURL = "/uploads/" + filename
				}
			}
		} else {
			log.Printf("[Cover] no file uploaded: %v", err)
		}
		log.Printf("[Cover] thumbURL=%q existingThumb=%q", thumbURL, existingThumb)
		// 如果没有上传新封面，保留原有的
		if thumbURL == "" {
			thumbURL = existingThumb
		}

		if _, err := db.Exec(
			"UPDATE posts SET title=?, content=?, thumb_url=?, category_id=?, is_top=?, privacy=?, status=?, publish_at=?, updated_at=? WHERE id=?",
			title, content, thumbURL, nilIfZero(categoryID), isTop, privacy, status, formatPublishAt(publishAt), time.Now(), id,
		); err != nil {
				log.Printf("[AdminPostEdit] update error: %v", err)
				c.String(500, "更新失败，请检查输入或联系管理员")
				return
			}

		// 更新标签
		if _, err := db.Exec("DELETE FROM post_tags WHERE post_id = ?", id); err != nil {
			log.Printf("[Admin] delete post_tags for post %d failed: %v", id, err)
		}
		savePostTags(db, id, c.PostFormArray("tags"))

		username := getAdminUsername(c)
		logOperation(db, username, c, fmt.Sprintf("编辑文章 #%d: %s", id, title))

		c.Redirect(http.StatusFound, "/admin/posts")
	}
}

func AdminPostDelete(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if id <= 0 {
			c.Redirect(http.StatusFound, "/admin/posts")
			return
		}
		if _, err := db.Exec("DELETE FROM post_tags WHERE post_id = ?", id); err != nil {
			log.Printf("[Admin] delete post_tags for post %d failed: %v", id, err)
		}
		// 清理关联评论，避免孤儿数据残留在侧边栏"最新评论"中
		if _, err := db.Exec("DELETE FROM comments WHERE target_type='post' AND target_id = ?", id); err != nil {
			log.Printf("[Admin] delete comments for post %d failed: %v", id, err)
		}
		if _, err := db.Exec("DELETE FROM post_likes WHERE post_id = ?", id); err != nil {
			log.Printf("[Admin] delete post_likes for post %d failed: %v", id, err)
		}
		if _, err := db.Exec("DELETE FROM posts WHERE id = ?", id); err != nil {
			c.JSON(500, gin.H{"ok": false, "msg": "删除失败"})
			return
		}

		username := getAdminUsername(c)
		logOperation(db, username, c, fmt.Sprintf("删除文章 #%d", id))

		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// AdminPostExport 导出文章为 Markdown 文件
func AdminPostExport(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if id <= 0 {
			c.String(http.StatusBadRequest, "非法ID")
			return
		}
		var title, content string
		err := db.QueryRow("SELECT title, content FROM posts WHERE id = ?", id).Scan(&title, &content)
		if err != nil {
			c.String(http.StatusNotFound, "文章不存在")
			return
		}
		// 生成文件名：标题.md，去除不安全字符
		safeName := strings.Map(func(r rune) rune {
			if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' || r == '\n' || r == '\r' {
				return '_'
			}
			if unicode.IsControl(r) {
				return '_'
			}
			return r
		}, title)
		if safeName == "" {
			safeName = "untitled"
		}
		filename := safeName + ".md"
		c.Header("Content-Type", "text/markdown; charset=utf-8")
		c.Header("Content-Disposition", "attachment; filename=\""+filename+"\"")
		c.String(http.StatusOK, content)
	}
}

func loadCategoriesAndTags(db *sql.DB, data gin.H) {
	catRows, _ := db.Query("SELECT id, name, slug FROM categories ORDER BY name")
	var selectedCatID int64
	if post, ok := data["Post"]; ok {
		if p, ok := post.(models.Post); ok && p.CategoryID != nil {
			selectedCatID = *p.CategoryID
		}
	}
	var cats []gin.H
	if catRows != nil {
		for catRows.Next() {
			var cat models.Category
			scanLog(catRows.Scan(&cat.ID, &cat.Name, &cat.Slug), "adminCategories")
			cats = append(cats, gin.H{
				"ID": cat.ID, "Name": cat.Name, "Slug": cat.Slug,
				"Selected": cat.ID == selectedCatID,
			})
		}
		catRows.Close()
	}
	data["Categories"] = cats

	// 获取已选标签
	selectedIDs := map[int64]bool{}
	if ids, ok := data["SelectedTagIDs"]; ok {
		if arr, ok := ids.([]int64); ok {
			for _, id := range arr {
				selectedIDs[id] = true
			}
		}
	}

	tagRows, _ := db.Query("SELECT id, name FROM tags ORDER BY name")
	var tags []gin.H
	if tagRows != nil {
		for tagRows.Next() {
			var tag models.Tag
			scanLog(tagRows.Scan(&tag.ID, &tag.Name), "adminTags")
			tags = append(tags, gin.H{
				"ID": tag.ID, "Name": tag.Name,
				"Selected": selectedIDs[tag.ID],
			})
		}
		tagRows.Close()
	}
	data["Tags"] = tags
}

func savePostTags(db *sql.DB, postID int64, tagStrs []string) {
	for _, ts := range tagStrs {
		tid, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			continue
		}
		execLog(db, "INSERT OR IGNORE INTO post_tags (post_id, tag_id) VALUES (?, ?)", postID, tid)
	}
}

// ============ 分类管理 ============

func AdminCategories(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("分类管理", "posts", "categories", getAdminUsername(c), db)

		rows, _ := db.Query("SELECT c.id, c.name, c.slug, c.created_at, COUNT(p.id) FROM categories c LEFT JOIN posts p ON p.category_id = c.id GROUP BY c.id ORDER BY c.name")
		var cats []gin.H
		if rows != nil {
			for rows.Next() {
				var id int64
				var name, slug string
				var createdAt time.Time
				var count int
				scanLog(rows.Scan(&id, &name, &slug, &createdAt, &count), "adminCatList")
				cats = append(cats, gin.H{"ID": id, "Name": name, "Slug": slug, "PostCount": count})
			}
			rows.Close()
		}
		data["Categories"] = cats
		c.HTML(http.StatusOK, "admin/category.html", data)
	}
}

func AdminCategorySave(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := strings.TrimSpace(c.PostForm("name"))
		slug := strings.TrimSpace(c.PostForm("slug"))
		editIDStr := c.PostForm("edit_id")

		if name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "分类名不能为空"})
			return
		}
		if len([]rune(name)) > 50 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "分类名不能超过50个字符"})
			return
		}
		if len(slug) > 100 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "slug不能超过100个字符"})
			return
		}

		if editIDStr != "" {
			editID, err := strconv.ParseInt(editIDStr, 10, 64)
			if err != nil || editID <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "非法的分类ID"})
				return
			}
			if _, err := db.Exec("UPDATE categories SET name=?, slug=? WHERE id=?", name, slug, editID); err != nil {
				log.Printf("[Admin] update category %d failed: %v", editID, err)
			}
			logOperation(db, getAdminUsername(c), c, fmt.Sprintf("编辑分类: %s", name))
		} else {
			if _, err := db.Exec("INSERT INTO categories (name, slug) VALUES (?, ?)", name, slug); err != nil {
				log.Printf("[Admin] insert category failed: %v", err)
			}
			logOperation(db, getAdminUsername(c), c, fmt.Sprintf("新增分类: %s", name))
		}
		c.Redirect(http.StatusFound, "/admin/categories")
	}
}

func AdminCategoryDelete(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "非法ID"})
			return
		}
		if _, err := db.Exec("DELETE FROM categories WHERE id = ?", id); err != nil {
			log.Printf("[AdminCategoryDelete] error: %v", err)
		}
		logOperation(db, getAdminUsername(c), c, fmt.Sprintf("删除分类 #%d", id))
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// ============ 标签管理 ============

func AdminTags(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("标签管理", "posts", "tags", getAdminUsername(c), db)

		rows, _ := db.Query("SELECT t.id, t.name, COUNT(pt.post_id) FROM tags t LEFT JOIN post_tags pt ON t.id = pt.tag_id GROUP BY t.id ORDER BY t.name")
		var tags []gin.H
		if rows != nil {
			for rows.Next() {
				var id int64
				var name string
				var count int
				scanLog(rows.Scan(&id, &name, &count), "adminTagList")
				tags = append(tags, gin.H{"ID": id, "Name": name, "PostCount": count})
			}
			rows.Close()
		}
		data["Tags"] = tags
		c.HTML(http.StatusOK, "admin/tag.html", data)
	}
}

func AdminTagSave(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := strings.TrimSpace(c.PostForm("name"))
		if name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "标签名不能为空"})
			return
		}
		if len([]rune(name)) > 30 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "标签名不能超过30个字符"})
			return
		}
		if _, err := db.Exec("INSERT OR IGNORE INTO tags (name) VALUES (?)", name); err != nil {
			log.Printf("[AdminTagCreate] error: %v", err)
		}
		logOperation(db, getAdminUsername(c), c, fmt.Sprintf("新增标签: %s", name))
		c.Redirect(http.StatusFound, "/admin/tags")
	}
}

func AdminTagDelete(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "非法ID"})
			return
		}
		if _, err := db.Exec("DELETE FROM tags WHERE id = ?", id); err != nil {
			log.Printf("[AdminTagDelete] error: %v", err)
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// ============ 菜单管理 ============

func AdminMomentList(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("瞬间列表", "moments", "list", getAdminUsername(c), db)
		data["Mode"] = "list"

		searchQuery := strings.TrimSpace(c.Query("q"))
		filterStatus := strings.TrimSpace(c.Query("status"))
		data["SearchQuery"] = searchQuery
		data["FilterStatus"] = filterStatus

		query := "SELECT id, content, media_urls, likes, status, created_at FROM moments WHERE 1=1"
		args := []interface{}{}
		if filterStatus != "" {
			query += " AND status = ?"
			args = append(args, filterStatus)
		}
		if searchQuery != "" {
			query += " AND content LIKE ?"
			args = append(args, "%"+searchQuery+"%")
		}
		query += " ORDER BY id DESC"

		rows, _ := db.Query(query, args...)
		var moments []gin.H
		if rows != nil {
			for rows.Next() {
				var m struct {
					ID        int64; Content string; MediaURLs string; Likes int64
					Status    string; CreatedAt time.Time
				}
				scanLog(rows.Scan(&m.ID, &m.Content, &m.MediaURLs, &m.Likes, &m.Status, &m.CreatedAt), "adminMoments")
				var mediaList []string
				if m.MediaURLs != "" {
					mediaList = strings.Split(m.MediaURLs, ",")
				}
				moments = append(moments, gin.H{
					"ID": m.ID, "Content": m.Content, "MediaCount": len(mediaList),
					"MediaList": mediaList, "MediaURLs": m.MediaURLs,
					"Likes": m.Likes, "Status": m.Status, "CreatedAt": m.CreatedAt,
				})
			}
			rows.Close()
		}
		data["Moments"] = moments
		c.HTML(http.StatusOK, "admin/moment.html", data)
	}
}

func AdminMomentCreate(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("发布瞬间", "moments", "create", getAdminUsername(c), db)
		data["Mode"] = "create"
		data["Moment"] = models.Moment{}
		c.HTML(http.StatusOK, "admin/moment.html", data)
	}
}

func AdminMomentCreatePost(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		content := utils.SanitizeHTML(c.PostForm("content"))

		// 处理上传的媒体文件
		mediaURLs := ""
		form, _ := c.MultipartForm()
		if form != nil {
			files := form.File["media"]
			var urls []string
			for _, fh := range files {
				projectRoot := getProjectRoot(c)
				uploadsDir := filepath.Join(projectRoot, "uploads")
				os.MkdirAll(uploadsDir, 0755)
				file, err := fh.Open()
				if err != nil {
					continue
				}
				tmpPath := filepath.Join(uploadsDir, "tmp_"+uuid.New().String()+filepath.Ext(filepath.Base(fh.Filename)))
				dst, err := os.Create(tmpPath)
				if err != nil {
					file.Close()
					continue
				}
				written, _ := io.Copy(dst, io.LimitReader(file, maxUploadSize+1))
				dst.Close()
				file.Close()
				if written > maxUploadSize {
					os.Remove(tmpPath)
					continue
				}
				filename, _, _ := utils.ProcessUploadedFile(tmpPath, uploadsDir)
				if filename != "" {
					urls = append(urls, "/uploads/"+filename)
				}
			}
			if len(urls) > 0 {
				mediaURLs = strings.Join(urls, ",")
			}
		}

		createdAt := c.PostForm("created_at")
		status := c.PostForm("status")
		if status == "" { status = "published" }
		publishAt := c.PostForm("publish_at")

		// 定时发布：如果 publish_at 有值且在未来，覆盖 status 为 scheduled
		if publishAt != "" && status != "draft" {
			if t, err := time.Parse("2006-01-02T15:04", publishAt); err == nil && t.After(time.Now()) {
				status = "scheduled"
			}
		}

		if createdAt != "" {
			t, err := time.Parse("2006-01-02T15:04", createdAt)
			if err == nil {
				execLog(db, "INSERT INTO moments (content, media_urls, status, publish_at, created_at) VALUES (?, ?, ?, ?, ?)", content, mediaURLs, status, nil, t.Format("2006-01-02 15:04:05"))
			} else {
				execLog(db, "INSERT INTO moments (content, media_urls, status) VALUES (?, ?, ?)", content, mediaURLs, status)
			}
		} else {
			var pAt interface{}
			if publishAt != "" { pAt = publishAt } else { pAt = nil }
			execLog(db, "INSERT INTO moments (content, media_urls, status, publish_at) VALUES (?, ?, ?, ?)", content, mediaURLs, status, pAt)
		}
		if status == "draft" {
			logOperation(db, getAdminUsername(c), c, "保存瞬间草稿")
		} else {
			logOperation(db, getAdminUsername(c), c, "发布瞬间")
		}
		c.Redirect(http.StatusFound, "/admin/moments")
	}
}

func AdminMomentEdit(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("编辑瞬间", "moments", "list", getAdminUsername(c), db)
		data["Mode"] = "edit"

		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if id <= 0 {
			c.Redirect(http.StatusFound, "/admin/moments")
			return
		}
		var m models.Moment
		err := db.QueryRow("SELECT id, content, media_urls, likes, status, created_at FROM moments WHERE id = ?", id).
			Scan(&m.ID, &m.Content, &m.MediaURLs, &m.Likes, &m.Status, &m.CreatedAt)
		if err != nil {
			c.String(404, "瞬间不存在")
			return
		}
		data["Moment"] = m
		c.HTML(http.StatusOK, "admin/moment.html", data)
	}
}

func AdminMomentEditPost(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if id <= 0 {
			c.Redirect(http.StatusFound, "/admin/moments")
			return
		}

		content := utils.SanitizeHTML(c.PostForm("content"))

		// 获取现有 media_urls
		var existingMedia string
		db.QueryRow("SELECT media_urls FROM moments WHERE id = ?", id).Scan(&existingMedia)

		// 处理要删除的现有媒体
		deletedMedia := c.PostForm("deleted_media")
		if deletedMedia != "" && existingMedia != "" {
			deletedSet := make(map[string]bool)
			for _, d := range strings.Split(deletedMedia, ",") {
				d = strings.TrimSpace(d)
				if d != "" {
					deletedSet[d] = true
				}
			}
			// 从 existingMedia 中移除删除的 URL
			var kept []string
			for _, m := range strings.Split(existingMedia, ",") {
				m = strings.TrimSpace(m)
				if !deletedSet[m] {
					kept = append(kept, m)
				} else {
					// 从磁盘删除文件，仅允许删除 uploads 下的文件
					projectRoot := getProjectRoot(c)
					if strings.HasPrefix(m, "/uploads/") {
						if err := deleteMediaFile(projectRoot, strings.TrimPrefix(m, "/uploads/")); err != nil {
							log.Printf("[MomentEdit] failed to delete media %s: %v", m, err)
						}
					}
				}
			}
			existingMedia = strings.Join(kept, ",")
		}

		// 处理新上传的媒体文件
		newMedia := ""
		form, _ := c.MultipartForm()
		if form != nil {
			files := form.File["media"]
			var urls []string
			for _, fh := range files {
				projectRoot := getProjectRoot(c)
				uploadsDir := filepath.Join(projectRoot, "uploads")
				os.MkdirAll(uploadsDir, 0755)
				file, err := fh.Open()
				if err != nil {
					continue
				}
				tmpPath := filepath.Join(uploadsDir, "tmp_"+uuid.New().String()+filepath.Ext(filepath.Base(fh.Filename)))
				dst, err := os.Create(tmpPath)
				if err != nil {
					file.Close()
					continue
				}
				written, _ := io.Copy(dst, io.LimitReader(file, maxUploadSize+1))
				dst.Close()
				file.Close()
				if written > maxUploadSize {
					os.Remove(tmpPath)
					continue
				}
				filename, _, _ := utils.ProcessUploadedFile(tmpPath, uploadsDir)
				if filename != "" {
					urls = append(urls, "/uploads/"+filename)
				}
			}
			if len(urls) > 0 {
				newMedia = strings.Join(urls, ",")
			}
		}
		mediaURLs := existingMedia
		if newMedia != "" {
			if mediaURLs != "" {
				mediaURLs = mediaURLs + "," + newMedia
			} else {
				mediaURLs = newMedia
			}
		}

		createdAt := c.PostForm("created_at")
		if createdAt != "" {
			t, err := time.Parse("2006-01-02T15:04", createdAt)
			if err == nil {
				execLog(db, "UPDATE moments SET content=?, media_urls=?, created_at=?, updated_at=CURRENT_TIMESTAMP WHERE id=?", content, mediaURLs, t.Format("2006-01-02 15:04:05"), id)
			} else {
				execLog(db, "UPDATE moments SET content=?, media_urls=?, updated_at=CURRENT_TIMESTAMP WHERE id=?", content, mediaURLs, id)
			}
		} else {
			execLog(db, "UPDATE moments SET content=?, media_urls=?, updated_at=CURRENT_TIMESTAMP WHERE id=?", content, mediaURLs, id)
		}
		logOperation(db, getAdminUsername(c), c, fmt.Sprintf("编辑瞬间 #%d", id))
		c.Redirect(http.StatusFound, "/admin/moments")
	}
}

func AdminMomentDelete(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "非法ID"})
			return
		}
		// 清理关联的瞬间评论和点赞记录
		execLog(db, "DELETE FROM comments WHERE target_type='moment' AND target_id = ?", id)
		execLog(db, "DELETE FROM moment_likes WHERE moment_id = ?", id)
		if _, err := db.Exec("DELETE FROM moments WHERE id = ?", id); err != nil {
			log.Printf("[AdminMomentDelete] error: %v", err)
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// ============ 评论管理 ============

func AdminComments(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("评论管理", "messages", "comments", getAdminUsername(c), db)

		rows, _ := db.Query("SELECT c1.id, c1.target_type, c1.target_id, c1.author, c1.author_avatar, c1.content, c1.image_url, c1.likes, c1.created_at, c1.parent_id, COALESCE(c2.author, '') as reply_to FROM comments c1 LEFT JOIN comments c2 ON c1.parent_id = c2.id ORDER BY c1.id DESC")
		var comments []gin.H
		if rows != nil {
			for rows.Next() {
				var cm struct {
					ID           int64
					TargetType   string
					TargetID     int64
					Author       string
					AuthorAvatar string
					Content      string
					ImageURL     sql.NullString
					Likes        int64
					CreatedAt    time.Time
					ParentID     int64
					ReplyTo      string
				}
				scanLog(rows.Scan(&cm.ID, &cm.TargetType, &cm.TargetID, &cm.Author, &cm.AuthorAvatar, &cm.Content, &cm.ImageURL, &cm.Likes, &cm.CreatedAt, &cm.ParentID, &cm.ReplyTo), "adminComments")
				imgURL := ""
				if cm.ImageURL.Valid {
					imgURL = cm.ImageURL.String
				}
				comments = append(comments, gin.H{
					"ID": cm.ID, "TargetType": cm.TargetType, "TargetID": cm.TargetID,
					"Author": cm.Author, "AuthorAvatar": cm.AuthorAvatar, "Content": cm.Content, "ImageURL": imgURL,
					"CreatedAt": cm.CreatedAt, "ParentID": cm.ParentID, "ReplyTo": cm.ReplyTo,
				})
			}
			rows.Close()
		}
		data["Comments"] = comments
		data["Total"] = len(comments)
		c.HTML(http.StatusOK, "admin/comment.html", data)
	}
}

func AdminCommentDelete(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "非法ID"})
			return
		}
		// 级联删除子评论和关联的点赞记录
		execLog(db, "DELETE FROM comment_likes WHERE comment_id IN (SELECT id FROM comments WHERE parent_id = ?)", id)
		execLog(db, "DELETE FROM comment_likes WHERE comment_id = ?", id)
		execLog(db, "DELETE FROM comments WHERE parent_id = ?", id)
		if _, err := db.Exec("DELETE FROM comments WHERE id = ?", id); err != nil {
			c.JSON(500, gin.H{"ok": false, "msg": "删除失败"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func AdminCommentReply(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if id <= 0 {
			c.Redirect(http.StatusFound, "/admin/comments")
			return
		}
		content := utils.SanitizeHTML(c.PostForm("content"))

		if content == "" {
			c.Redirect(http.StatusFound, "/admin/comments")
			return
		}

		var targetType string
		var targetID int64
		err := db.QueryRow("SELECT target_type, target_id FROM comments WHERE id = ?", id).Scan(&targetType, &targetID)
		if err != nil {
			c.HTML(http.StatusNotFound, "admin/comment.html", gin.H{"Error": "评论不存在"})
			return
		}

		adminName := getAdminUsername(c)
		if _, err := db.Exec("INSERT INTO comments (target_type, target_id, author, content, parent_id) VALUES (?, ?, ?, ?, ?)",
			targetType, targetID, adminName, content, id); err != nil {
			log.Printf("[Admin] reply comment #%d failed: %v", id, err)
		}

		logOperation(db, adminName, c, fmt.Sprintf("回复评论 #%d", id))
		c.Redirect(http.StatusFound, "/admin/comments")
	}
}

// ============ 留言管理 ============

func AdminGuestbook(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("留言管理", "messages", "guestbook", getAdminUsername(c), db)

		rows, _ := db.Query("SELECT id, author, content, image_url, ip, admin_reply, replied_at, created_at FROM guestbook ORDER BY id DESC")
		var entries []gin.H
		if rows != nil {
			for rows.Next() {
				var g struct {
					ID         int64
					Author     string
					Content    string
					ImageURL   sql.NullString
					IP         sql.NullString
					AdminReply string
					RepliedAt  sql.NullTime
					CreatedAt  time.Time
				}
				scanLog(rows.Scan(&g.ID, &g.Author, &g.Content, &g.ImageURL, &g.IP, &g.AdminReply, &g.RepliedAt, &g.CreatedAt), "adminGuestbook")
				imgURL, ip := "", ""
				if g.ImageURL.Valid {
					imgURL = g.ImageURL.String
				}
				if g.IP.Valid {
					ip = g.IP.String
				}
				var repliedAt *time.Time
				if g.RepliedAt.Valid {
					repliedAt = &g.RepliedAt.Time
				}
				entries = append(entries, gin.H{
					"ID": g.ID, "Author": g.Author, "Content": g.Content,
					"ImageURL": imgURL, "IP": ip, "AdminReply": g.AdminReply,
					"RepliedAt": repliedAt, "CreatedAt": g.CreatedAt,
				})
			}
			rows.Close()
		}
		data["Guestbooks"] = entries
		data["Total"] = len(entries)
		c.HTML(http.StatusOK, "admin/guestbook.html", data)
	}
}

func AdminGuestbookReply(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		content := utils.SanitizeHTML(c.PostForm("content"))
		if id <= 0 || content == "" {
			c.Redirect(http.StatusFound, "/admin/guestbook")
			return
		}

		adminName := getAdminUsername(c)
		now := time.Now().Format("2006-01-02 15:04:05")
		if _, err := db.Exec("UPDATE guestbook SET admin_reply = ?, replied_at = ? WHERE id = ?", content, now, id); err != nil {
			log.Printf("[AdminGuestbookReply] reply guestbook #%d failed: %v", id, err)
		}

		logOperation(db, adminName, c, fmt.Sprintf("回复留言 #%d", id))
		c.Redirect(http.StatusFound, "/admin/guestbook")
	}
}

func AdminGuestbookDelete(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "非法ID"})
			return
		}
		if _, err := db.Exec("DELETE FROM guestbook WHERE id = ?", id); err != nil {
			log.Printf("[AdminGuestbookDelete] error: %v", err)
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// ============ 动态管理 ============

func AdminMedia(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("媒体管理", "media", c.DefaultQuery("type", "photos"), getAdminUsername(c), db)
		data["Type"] = c.DefaultQuery("type", "photos")

		uploadsDir := filepath.Join(getProjectRoot(c), "uploads")
		entries, err := os.ReadDir(uploadsDir)
		if err != nil {
			data["Files"] = []gin.H{}
			c.HTML(http.StatusOK, "admin/media.html", data)
			return
		}

		mediaType := c.DefaultQuery("type", "photos")
		var files []gin.H
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if mediaType == "photos" && !isImageExt(ext) {
				continue
			}
			if mediaType == "videos" && !isVideoExt(ext) {
				continue
			}

			info, _ := e.Info()
			files = append(files, gin.H{
				"Name":       e.Name(),
				"Size":       utils.FormatFileSize(info.Size()),
				"URL":        "/uploads/" + e.Name(),
				"URLEncoded": url.QueryEscape(e.Name()),
				"ModTime":    info.ModTime(),
			})
		}

		// 按上传时间倒序（最新在前）
		sort.Slice(files, func(i, j int) bool {
			mi, _ := files[i]["ModTime"].(time.Time)
			mj, _ := files[j]["ModTime"].(time.Time)
			return mi.After(mj)
		})

		data["Files"] = files
		c.HTML(http.StatusOK, "admin/media.html", data)
	}
}

func AdminUpload(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectRoot := getProjectRoot(c)
		uploadsDir := filepath.Join(projectRoot, "uploads")
		os.MkdirAll(uploadsDir, 0755)

		file, header, err := c.Request.FormFile("file")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "请选择文件"})
			return
		}
		defer file.Close()

		// 读取前 512 字节检测 MIME 类型
		buf := make([]byte, 512)
		n, _ := file.Read(buf)
		contentType := http.DetectContentType(buf[:n])
		allowedTypes := map[string]bool{
			"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true,
			"video/mp4": true, "video/webm": true,
		}
		if !allowedTypes[contentType] {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "不支持的文件类型，仅允许图片和视频"})
			return
		}

		// 重置 file reader 以便后续保存
		file.Seek(0, 0)

		tmpPath := filepath.Join(uploadsDir, "tmp_"+uuid.New().String()+filepath.Ext(filepath.Base(header.Filename)))
		// 保存临时文件
		dst, err := os.Create(tmpPath)
		if err != nil {
			c.JSON(500, gin.H{"ok": false, "msg": "保存失败"})
			return
		}
		written, _ := io.Copy(dst, io.LimitReader(file, maxUploadSize+1))
		dst.Close()
		if written > maxUploadSize {
			os.Remove(tmpPath)
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "文件大小超过500MB限制"})
			return
		}

		// 处理图片（压缩/转WebP）
		filename, size, err := utils.ProcessUploadedFile(tmpPath, uploadsDir)
		if err != nil {
			// 如果处理失败，保留原文件
			filename, _ = utils.SaveUploadedFile(tmpPath, uploadsDir)
			if stat, e := os.Stat(filepath.Join(uploadsDir, filename)); e == nil {
				size = stat.Size()
			}
		}

		logOperation(db, getAdminUsername(c), c, fmt.Sprintf("上传文件: %s (%s)", filename, utils.FormatFileSize(size)))

		c.JSON(http.StatusOK, gin.H{
			"ok":   true,
			"url":  "/uploads/" + filename,
			"name": filename,
			"size": utils.FormatFileSize(size),
		})
	}
}

func deleteMediaFile(projectRoot, name string) error {
	uploadsDir := filepath.Join(projectRoot, "uploads")
	baseName := filepath.Base(name)
	if baseName == "." || baseName == string(os.PathSeparator) || baseName == "" {
		return fmt.Errorf("非法文件名")
	}
	target := filepath.Join(uploadsDir, baseName)
	cleanTarget := filepath.Clean(target)
	cleanUploads := filepath.Clean(uploadsDir)
	if !strings.HasPrefix(cleanTarget, cleanUploads+string(os.PathSeparator)) {
		return fmt.Errorf("非法路径")
	}
	info, err := os.Stat(cleanTarget)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("不能删除目录")
	}
	return os.Remove(cleanTarget)
}

func AdminMediaDelete(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		// Gin 已自动 URL 解码 :name 参数，不需要再手动 unescape
		if name == "" {
			name = c.Query("url")
		}
		if name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "缺少文件名"})
			return
		}
		projectRoot := getProjectRoot(c)
		if err := deleteMediaFile(projectRoot, name); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func AdminMediaBatchDelete(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Names []string `json:"names"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || len(req.Names) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "请选择要删除的文件"})
			return
		}
		projectRoot := getProjectRoot(c)
		deleted := 0
		for _, name := range req.Names {
			if err := deleteMediaFile(projectRoot, name); err != nil {
				log.Printf("[Media] failed to delete %s: %v", name, err)
				continue
			}
			deleted++
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "deleted": deleted})
	}
}

// AdminMediaAPI 返回媒体文件 JSON 列表，供编辑器弹窗调用
func AdminMediaAPI(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		uploadsDir := filepath.Join(getProjectRoot(c), "uploads")
		entries, err := os.ReadDir(uploadsDir)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"ok": true, "files": []gin.H{}})
			return
		}

		mediaType := c.DefaultQuery("type", "all")
		var files []gin.H
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if mediaType == "photos" && !isImageExt(ext) {
				continue
			}
			if mediaType == "videos" && !isVideoExt(ext) {
				continue
			}

			info, _ := e.Info()
			files = append(files, gin.H{
				"name":    e.Name(),
				"size":    utils.FormatFileSize(info.Size()),
				"url":     "/uploads/" + e.Name(),
				"modTime": info.ModTime(),
			})
		}

		sort.Slice(files, func(i, j int) bool {
			mi, _ := files[i]["modTime"].(time.Time)
			mj, _ := files[j]["modTime"].(time.Time)
			return mi.After(mj)
		})

		c.JSON(http.StatusOK, gin.H{"ok": true, "files": files})
	}
}

func isImageExt(ext string) bool {
	return ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif" || ext == ".webp" || ext == ".bmp"
}

func isVideoExt(ext string) bool {
	return ext == ".mp4" || ext == ".webm" || ext == ".avi" || ext == ".mov"
}

// ============ 网站设置 ============

func AdminSettings(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("网站设置", "settings", "site", getAdminUsername(c), db)
		settings, _ := getSiteSettings(db)
		data["Settings"] = settings
		c.HTML(http.StatusOK, "admin/settings.html", data)
	}
}

// processBannerUpload 处理单路 Banner 上传/URL 输入的通用逻辑。
// 返回非 nil error 时表示文件超大，应由上层给用户错误提示。
func processBannerUpload(db *sql.DB, c *gin.Context, formFileName, formURLName, dbKey string) error {
	bannerURL := utils.SafeImageURL(c.PostForm(formURLName))
	file, header, err := c.Request.FormFile(formFileName)
	if err == nil {
		defer file.Close()
		projectRoot := getProjectRoot(c)
		uploadsDir := filepath.Join(projectRoot, "uploads")
		os.MkdirAll(uploadsDir, 0755)
		tmpPath := filepath.Join(uploadsDir, "tmp_banner_"+uuid.New().String()+filepath.Ext(filepath.Base(header.Filename)))
		dst, err := os.Create(tmpPath)
		if err == nil {
			written, _ := io.Copy(dst, io.LimitReader(file, maxUploadSize+1))
			dst.Close()
			if written > maxUploadSize {
				os.Remove(tmpPath)
				return fmt.Errorf("文件大小超过 500MB 限制")
			}
			filename, size, procErr := utils.ProcessUploadedFile(tmpPath, uploadsDir)
			log.Printf("[Banner] uploaded: tmp=%s filename=%s size=%d err=%v dbKey=%s", tmpPath, filename, size, procErr, dbKey)
			if filename != "" {
				bannerURL = "/uploads/" + filename
			}
		} else {
			log.Printf("[Banner] create tmp file failed: %v", err)
		}
	} else {
		log.Printf("[Banner] no file uploaded or error: %v dbKey=%s", err, dbKey)
	}
	log.Printf("[Banner] saving %s=%q", dbKey, bannerURL)
	execLog(db, "INSERT INTO system_settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", dbKey, bannerURL)
	return nil
}

func AdminSettingsPost(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		keys := []string{"site_title", "site_subtitle", "sensitive_words", "comment_limit_count", "comment_limit_minute", "site_founded_at", "site_notification", "amap_key", "amap_jscode"}
		for _, key := range keys {
			value := c.PostForm(key)
			execLog(db, "INSERT INTO system_settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
		}

			// 处理 4 路氛围 Banner
			for _, bp := range []struct{ f, u, k string }{
				{"banner_file", "banner_url", "banner_url"},
				{"banner_file_posts", "banner_url_posts", "banner_url_posts"},
				{"banner_file_moments", "banner_url_moments", "banner_url_moments"},
				{"banner_file_guestbook", "banner_url_guestbook", "banner_url_guestbook"},
			} {
				if err := processBannerUpload(db, c, bp.f, bp.u, bp.k); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": err.Error()})
					return
				}
			}

		// 处理 "所有页面使用首页 Banner" 开关
		// 注意：checkbox 未勾选时浏览器不发送该字段，c.PostForm 返回空字符串
		useHomepage := c.PostForm("banner_use_homepage")
		if useHomepage == "" {
			useHomepage = "false"
		}
		execLog(db, "INSERT INTO system_settings (key, value) VALUES ('banner_use_homepage', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", useHomepage)

		// Process QR code uploads
		qrFields := []struct {
			formName string
			dbKey    string
			prefix   string
		}{
			{"wechat_qr_file", "wechat_qr", "wechat_qr_"},
			{"alipay_qr_file", "alipay_qr", "alipay_qr_"},
		}
		for _, qf := range qrFields {
			qrURL := utils.SafeImageURL(c.PostForm(qf.dbKey))
			qrFile, qrHeader, qrErr := c.Request.FormFile(qf.formName)
			if qrErr == nil {
				defer qrFile.Close()
				projectRoot := getProjectRoot(c)
				uploadsDir := filepath.Join(projectRoot, "uploads")
				os.MkdirAll(uploadsDir, 0755)
				tmpPath := filepath.Join(uploadsDir, qf.prefix+uuid.New().String()+filepath.Ext(filepath.Base(qrHeader.Filename)))
				dst, dstErr := os.Create(tmpPath)
				if dstErr == nil {
					written, _ := io.Copy(dst, io.LimitReader(qrFile, maxUploadSize+1))
					dst.Close()
					if written > maxUploadSize {
						os.Remove(tmpPath)
					} else {
						filename, _, procErr := utils.ProcessUploadedFile(tmpPath, uploadsDir)
						log.Printf("[QR] uploaded: %s -> %s err=%v", qf.dbKey, filename, procErr)
						if filename != "" {
							qrURL = "/uploads/" + filename
						}
					}
				}
			}
			execLog(db, "INSERT INTO system_settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", qf.dbKey, qrURL)
		}

			InvalidateSiteSettingsCache()
			logOperation(db, getAdminUsername(c), c, "更新网站设置")
			c.Redirect(http.StatusFound, "/admin/settings")
	}
}

func AdminAbout(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("关于我", "settings", "about", getAdminUsername(c), db)

		var raw string
		db.QueryRow("SELECT value FROM system_settings WHERE key='about_me_json'").Scan(&raw)

		am := AboutMe{Name: "HuiNan", Bio: "热爱技术与生活"}
		if raw != "" {
			// 先尝试解析为单个对象
			if err := json.Unmarshal([]byte(raw), &am); err != nil {
				// 可能是数组格式 [{...}]，尝试从数组取第一个元素
				var arr []map[string]interface{}
				if json.Unmarshal([]byte(raw), &arr) == nil && len(arr) > 0 {
					if v, ok := arr[0]["name"]; ok {
						if s, ok := v.(string); ok && s != "" {
							am.Name = s
						}
					}
					if v, ok := arr[0]["avatar"]; ok {
						if s, ok := v.(string); ok && s != "" {
							am.Avatar = s
						}
					}
					if v, ok := arr[0]["bio"]; ok {
						if s, ok := v.(string); ok && s != "" {
							am.Bio = s
						}
					}
					if v, ok := arr[0]["location"]; ok {
						if s, ok := v.(string); ok && s != "" {
							am.Location = s
						}
					}
					if v, ok := arr[0]["one_word"]; ok {
						if s, ok := v.(string); ok && s != "" {
							am.OneWord = s
						}
					}
				}
			}
		}
		// 空值保护
		if am.Name == "" {
			am.Name = "HuiNan"
		}
		if am.Bio == "" {
			am.Bio = "热爱技术与生活"
		}
		// 兼容旧数据：把旧的 github/twitter/email 字段迁移到 social_links
		if len(am.SocialLinks) == 0 {
			// 尝试从旧格式读取
			var old map[string]string
			if raw != "" {
				json.Unmarshal([]byte(raw), &old)
			}
			if old != nil {
				if v, ok := old["github"]; ok && v != "" {
					am.SocialLinks = append(am.SocialLinks, SocialLink{Name: "GitHub", URL: v, Platform: "github"})
				}
				if v, ok := old["twitter"]; ok && v != "" {
					am.SocialLinks = append(am.SocialLinks, SocialLink{Name: "Twitter", URL: v, Platform: "twitter"})
				}
				if v, ok := old["email"]; ok && v != "" {
					am.SocialLinks = append(am.SocialLinks, SocialLink{Name: "邮箱", URL: "mailto:" + v, Platform: "email"})
				}
			}
		}
		am.SocialLinks = ensureDefaultSocialLinks(am.SocialLinks)

		data["AboutMe"] = am
		c.HTML(http.StatusOK, "admin/about.html", data)
	}
}

func AdminAboutPost(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		avatar := utils.SafeImageURL(c.PostForm("avatar"))
		// 优先用上传的文件
		file, err := c.FormFile("avatar_file")
		if err == nil {
			uploadsDir := filepath.Join(getProjectRoot(c), "uploads")
			os.MkdirAll(uploadsDir, 0755)
			tmpPath := filepath.Join(uploadsDir, "tmp_avatar_"+uuid.New().String()+filepath.Ext(filepath.Base(file.Filename)))
			if err := c.SaveUploadedFile(file, tmpPath); err == nil {
				filename, _, _ := utils.ProcessUploadedFile(tmpPath, uploadsDir)
				if filename != "" {
					avatar = "/uploads/" + filename
				}
			}
		}
		am := AboutMe{
			Name:        c.PostForm("name"),
			Avatar:      avatar,
			Bio:         c.PostForm("bio"),
			Location:    c.PostForm("location"),
			DetailIntro: c.PostForm("detail_intro"),
		}

		// 解析社交链接动态列表（安全取值，防止数组越界）
		names := c.PostFormArray("social_name")
		urls := c.PostFormArray("social_url")
		platforms := c.PostFormArray("social_platform")
		sorts := c.PostFormArray("social_sort")
		for i := range names {
			sName := names[i]
			sURL := ""
			if i < len(urls) {
				sURL = utils.SafeExternalURL(urls[i])
			}
			sPlatform := ""
			if i < len(platforms) {
				sPlatform = platforms[i]
			}
			sSort := (i + 1) * 10
			if i < len(sorts) {
				if parsed, err := strconv.Atoi(strings.TrimSpace(sorts[i])); err == nil && parsed > 0 {
					sSort = parsed
				}
			}
			if sName != "" || sURL != "" || sPlatform != "" {
				am.SocialLinks = append(am.SocialLinks, normalizeSocialLink(SocialLink{
					Name:     sName,
					URL:      sURL,
					Platform: sPlatform,
					Sort:     sSort,
				}))
			}
		}

		jsonBytes, _ := json.Marshal(am)
		execLog(db, "INSERT INTO system_settings (key, value) VALUES ('about_me_json', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", string(jsonBytes))

			InvalidateSiteSettingsCache()
			logOperation(db, getAdminUsername(c), c, "更新关于")
			c.Redirect(http.StatusFound, "/admin/about")
	}
}

func AdminSitemap(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectRoot := getProjectRoot(c)
		staticDir := filepath.Join(projectRoot, "static")
		os.MkdirAll(staticDir, 0755)

		// 从系统设置读取站点 URL，未配置则使用请求来源
		var siteURL string
		db.QueryRow("SELECT value FROM system_settings WHERE key='site_url'").Scan(&siteURL)
		if siteURL == "" {
			scheme := "http"
			if c.Request.TLS != nil {
				scheme = "https"
			} else if forwardedProto := c.GetHeader("X-Forwarded-Proto"); forwardedProto == "https" || forwardedProto == "http" {
				scheme = forwardedProto
			}
			host := c.Request.Host
			if host == "" {
				host = "localhost:9090"
			}
			siteURL = scheme + "://" + host
		}

			path, err := utils.GenerateSitemap(db, siteURL, staticDir)
			if err != nil {
				log.Printf("[AdminSitemap] generate error: %v", err)
				c.String(500, "Sitemap 生成失败，请检查服务器日志")
				return
			}

		logOperation(db, getAdminUsername(c), c, "生成站点地图")
		data := adminData("站点地图", "settings", "sitemap", getAdminUsername(c), db)
		data["SitemapPath"] = path
		c.HTML(http.StatusOK, "admin/sitemap.html", data)
	}
}

// ============ 用户管理 ============

func AdminUsers(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("用户管理", "system", "users", getAdminUsername(c), db)

		rows, _ := db.Query("SELECT id, username, role, avatar_url, created_at FROM users ORDER BY id")
		var users []gin.H
		if rows != nil {
			for rows.Next() {
				var u struct {
					ID        int64
					Username  string
					Role      string
					AvatarURL string
					CreatedAt time.Time
				}
				scanLog(rows.Scan(&u.ID, &u.Username, &u.Role, &u.AvatarURL, &u.CreatedAt), "adminUsers")
				users = append(users, gin.H{
					"ID": u.ID, "Username": u.Username, "Role": u.Role, "AvatarURL": u.AvatarURL,
					"CreatedAt": u.CreatedAt,
				})
			}
			rows.Close()
		}
		data["Users"] = users
		c.HTML(http.StatusOK, "admin/user.html", data)
	}
}

func AdminUserSave(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		username := strings.TrimSpace(c.PostForm("username"))
		password := c.PostForm("password")
		role := c.PostForm("role")
		if role == "" {
			role = "editor"
		}
		// 角色白名单校验
		if role != "admin" && role != "editor" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "非法角色"})
			return
		}
		// 用户名和密码长度校验
		if username == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "用户名不能为空"})
			return
		}
		if len([]rune(username)) > 30 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "用户名不能超过30个字符"})
			return
		}
		if len(password) < 6 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "密码至少6位"})
			return
		}
		if len(password) > 72 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "密码不能超过72个字符"})
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "密码加密失败"})
			return
		}

		result, err := db.Exec("INSERT INTO users (username, password, role) VALUES (?, ?, ?)", username, string(hash), role)
		if err != nil {
			log.Printf("[AdminUserCreate] error: %v", err)
			c.JSON(http.StatusConflict, gin.H{"ok": false, "msg": "用户名已存在"})
			return
		}
		if rows, _ := result.RowsAffected(); rows == 0 {
			c.JSON(http.StatusConflict, gin.H{"ok": false, "msg": "用户名已存在"})
			return
		}
		logOperation(db, getAdminUsername(c), c, fmt.Sprintf("新增用户: %s", username))
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func AdminUserUpdate(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "非法ID"})
			return
		}
		username := c.PostForm("username")
		role := c.PostForm("role")
		avatarURL := utils.SafeImageURL(c.PostForm("avatar_url"))
		// 优先用上传的文件
		file, fErr := c.FormFile("avatar_file")
		if fErr == nil {
			uploadsDir := filepath.Join(getProjectRoot(c), "uploads")
			os.MkdirAll(uploadsDir, 0755)
			tmpPath := filepath.Join(uploadsDir, "tmp_avatar_"+uuid.New().String()+filepath.Ext(filepath.Base(file.Filename)))
			if err := c.SaveUploadedFile(file, tmpPath); err == nil {
				filename, _, _ := utils.ProcessUploadedFile(tmpPath, uploadsDir)
				if filename != "" {
					avatarURL = "/uploads/" + filename
				}
			}
		}
		if username == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "用户名不能为空"})
			return
		}
		if role == "" {
			role = "editor"
		}
		// 角色白名单校验
		if role != "admin" && role != "editor" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "非法角色"})
			return
		}

		// 防止修改 id=1 的角色
		if id == 1 && role != "admin" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "超级管理员角色不可更改"})
			return
		}

		_, err := db.Exec("UPDATE users SET username = ?, role = ?, avatar_url = ? WHERE id = ?", username, role, avatarURL, id)
		if err != nil {
			log.Printf("[AdminUserUpdate] error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "更新失败"})
			return
		}
		logOperation(db, getAdminUsername(c), c, fmt.Sprintf("编辑用户 #%d: %s", id, username))
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func AdminUserResetPassword(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "非法ID"})
			return
		}
		password := c.PostForm("password")
		if password == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "密码不能为空"})
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "密码加密失败"})
			return
		}

		_, err = db.Exec("UPDATE users SET password = ? WHERE id = ?", string(hash), id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "更新失败"})
			return
		}

		logOperation(db, getAdminUsername(c), c, fmt.Sprintf("重置用户 #%d 密码", id))
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func AdminUserDelete(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "非法ID"})
			return
		}
		// 禁止删除 id=1 的超级管理员
		if id == 1 {
			c.JSON(http.StatusForbidden, gin.H{"ok": false, "msg": "不能删除超级管理员"})
			return
		}
		// 禁止删除自己
		currentUsername := getAdminUsername(c)
		var currentID int64
		db.QueryRow("SELECT id FROM users WHERE username = ?", currentUsername).Scan(&currentID)
		if id == currentID {
			c.JSON(http.StatusForbidden, gin.H{"ok": false, "msg": "不能删除自己"})
			return
		}
		// 禁止删除最后一个 admin
		var adminCount int
		db.QueryRow("SELECT COUNT(*) FROM users WHERE role = 'admin'").Scan(&adminCount)
		if adminCount <= 1 {
			var targetRole string
			db.QueryRow("SELECT role FROM users WHERE id = ?", id).Scan(&targetRole)
			if targetRole == "admin" {
				c.JSON(http.StatusForbidden, gin.H{"ok": false, "msg": "不能删除最后一个管理员"})
				return
			}
		}
		if _, err := db.Exec("DELETE FROM users WHERE id = ?", id); err != nil {
			log.Printf("[AdminUserDelete] error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "删除失败"})
			return
		}
		logOperation(db, currentUsername, c, fmt.Sprintf("删除用户 id=%d", id))
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// ============ 菜单管理 ============

func AdminMenus(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("菜单管理", "system", "menus", getAdminUsername(c), db)

		// 管理后台显示所有菜单（含隐藏的）
		rows, _ := db.Query("SELECT id, name, url, icon, parent_id, order_num, is_system, visible FROM menus ORDER BY order_num, id")
		var menus []gin.H
		if rows != nil {
			for rows.Next() {
				var m struct {
					ID       int64
					Name     string
					URL      string
					Icon     string
					ParentID sql.NullInt64
					OrderNum int
					IsSystem bool
					Visible  bool
				}
				scanLog(rows.Scan(&m.ID, &m.Name, &m.URL, &m.Icon, &m.ParentID, &m.OrderNum, &m.IsSystem, &m.Visible), "adminMenus")
				parentID := int64(0)
				if m.ParentID.Valid {
					parentID = m.ParentID.Int64
				}
				menus = append(menus, gin.H{
					"ID": m.ID, "Name": m.Name, "URL": m.URL, "Icon": m.Icon,
					"ParentID": parentID, "OrderNum": m.OrderNum,
					"IsSystem": m.IsSystem, "Visible": m.Visible,
				})
			}
			rows.Close()
		}
		data["Menus"] = menus
		c.HTML(http.StatusOK, "admin/menu.html", data)
	}
}

func AdminMenuSave(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := strings.TrimSpace(c.PostForm("name"))
		if name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "菜单名不能为空"})
			return
		}
		if len([]rune(name)) > 30 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "菜单名不能超过30个字符"})
			return
		}
		url := utils.SafeURL(c.PostForm("url"))
		icon := c.PostForm("icon")
		orderNum, _ := strconv.Atoi(c.PostForm("order_num"))
		parentID, _ := strconv.ParseInt(c.PostForm("parent_id"), 10, 64)

		if _, err := db.Exec("INSERT INTO menus (name, url, icon, parent_id, order_num, is_system, visible) VALUES (?,?,?,?,?,0,1)",
			name, url, icon, nilIfZero(parentID), orderNum); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "保存失败: " + err.Error()})
			return
		}

		logOperation(db, getAdminUsername(c), c, fmt.Sprintf("新增菜单: %s", name))
		c.Redirect(http.StatusFound, "/admin/menus")
	}
}

// AdminMenuUpdate 更新菜单（含系统菜单的名称/图标/排序/可见性）
func AdminMenuUpdate(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.PostForm("id"), 10, 64)
		name := strings.TrimSpace(c.PostForm("name"))
		icon := c.PostForm("icon")
		orderNum, _ := strconv.Atoi(c.PostForm("order_num"))
		visible := c.PostForm("visible") == "1"

		if id <= 0 || name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "参数错误"})
			return
		}

		db.Exec("UPDATE menus SET name=?, icon=?, order_num=?, visible=? WHERE id=?",
			name, icon, orderNum, visible, id)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func AdminMenuDelete(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "非法ID"})
			return
		}
		// 系统菜单不可删除
		var isSystem bool
		db.QueryRow("SELECT is_system FROM menus WHERE id=?", id).Scan(&isSystem)
		if isSystem {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "系统菜单不可删除"})
			return
		}
		if _, err := db.Exec("DELETE FROM menus WHERE id = ?", id); err != nil {
			log.Printf("[AdminMenuDelete] error: %v", err)
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// ============ 页面管理 ============

func AdminPages(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("页面管理", "system", "pages", getAdminUsername(c), db)
		data["Mode"] = "list"

		rows, _ := db.Query("SELECT id, title, slug, created_at FROM pages ORDER BY id")
		var pages []gin.H
		if rows != nil {
			for rows.Next() {
				var p struct {
					ID        int64
					Title     string
					Slug      string
					CreatedAt time.Time
				}
				scanLog(rows.Scan(&p.ID, &p.Title, &p.Slug, &p.CreatedAt), "adminPages")
				pages = append(pages, gin.H{
					"ID": p.ID, "Title": p.Title, "Slug": p.Slug,
					"CreatedAt": p.CreatedAt.Format("2006-01-02"),
				})
			}
			rows.Close()
		}
		data["Pages"] = pages
		c.HTML(http.StatusOK, "admin/page.html", data)
	}
}

func AdminPageCreate(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("新建页面", "system", "pages", getAdminUsername(c), db)
		data["Mode"] = "create"
		data["Page"] = models.Page{}
		c.HTML(http.StatusOK, "admin/page.html", data)
	}
}

func AdminPageCreatePost(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		title := strings.TrimSpace(c.PostForm("title"))
		slug := strings.TrimSpace(c.PostForm("slug"))
		content := utils.SanitizeHTML(c.PostForm("content"))
		if title == "" || slug == "" {
			c.String(http.StatusBadRequest, "标题和 slug 不能为空")
			return
		}
		if len([]rune(title)) > 200 {
			c.String(http.StatusBadRequest, "标题不能超过200个字符")
			return
		}
		if len(slug) > 100 {
			c.String(http.StatusBadRequest, "slug 不能超过100个字符")
			return
		}
		if _, err := db.Exec("INSERT INTO pages (title, slug, content) VALUES (?, ?, ?)", title, slug, content); err != nil {
				log.Printf("[AdminPageCreate] error: %v", err)
				c.String(500, "创建页面失败，请检查输入或联系管理员")
				return
			}
		logOperation(db, getAdminUsername(c), c, fmt.Sprintf("新建页面: %s", title))
		c.Redirect(http.StatusFound, "/admin/pages")
	}
}

func AdminPageEdit(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("编辑页面", "system", "pages", getAdminUsername(c), db)
		data["Mode"] = "edit"

		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if id <= 0 {
			c.Redirect(http.StatusFound, "/admin/pages")
			return
		}
		var p models.Page
		err := db.QueryRow("SELECT id, title, slug, content, created_at, COALESCE(updated_at, created_at) FROM pages WHERE id = ?", id).
			Scan(&p.ID, &p.Title, &p.Slug, &p.Content, &p.CreatedAt, &p.UpdatedAt)
		if err != nil {
			c.String(404, "页面不存在")
			return
		}
		data["Page"] = p
		c.HTML(http.StatusOK, "admin/page.html", data)
	}
}

func AdminPageEditPost(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if id <= 0 {
			c.Redirect(http.StatusFound, "/admin/pages")
			return
		}
		title := strings.TrimSpace(c.PostForm("title"))
		slug := strings.TrimSpace(c.PostForm("slug"))
		if title == "" || slug == "" {
			c.String(http.StatusBadRequest, "标题和 slug 不能为空")
			return
		}
		if len([]rune(title)) > 200 {
			c.String(http.StatusBadRequest, "标题不能超过200个字符")
			return
		}
		if len(slug) > 100 {
			c.String(http.StatusBadRequest, "slug 不能超过100个字符")
			return
		}
		if _, err := db.Exec("UPDATE pages SET title=?, slug=?, content=?, updated_at=? WHERE id=?",
				title, slug, utils.SanitizeHTML(c.PostForm("content")), time.Now(), id); err != nil {
				log.Printf("[AdminPageEdit] error: %v", err)
				c.String(500, "更新页面失败，请检查输入或联系管理员")
				return
			}
		logOperation(db, getAdminUsername(c), c, fmt.Sprintf("编辑页面 #%d", id))
		c.Redirect(http.StatusFound, "/admin/pages")
	}
}

func AdminPageDelete(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "非法ID"})
			return
		}
		if _, err := db.Exec("DELETE FROM pages WHERE id = ?", id); err != nil {
			log.Printf("[AdminPageDelete] error: %v", err)
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// ============ 备份恢复 ============

func AdminBackup(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("备份恢复", "system", "backup", getAdminUsername(c), db)

		projectRoot := getProjectRoot(c)
		backups, err := utils.ListBackups(filepath.Join(projectRoot, "backups"))
		if err != nil {
			backups = []string{}
		}

		var backupList []gin.H
		for _, b := range backups {
			name := filepath.Base(b)
			stat, _ := os.Stat(b)
			size := ""
			if stat != nil {
				size = utils.FormatFileSize(stat.Size())
			}
			backupList = append(backupList, gin.H{
				"Name": name, "URL": "/admin/backup/download/" + name, "Size": size,
			})
		}
		data["Backups"] = backupList
		c.HTML(http.StatusOK, "admin/backup.html", data)
	}
}

func AdminBackupCreate(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectRoot := getProjectRoot(c)
		cfg := utils.DefaultBackupConfig(projectRoot)
		backupPath, err := utils.CreateBackup(cfg)
		if err != nil {
			log.Printf("[BackupCreate] error: %v", err)
			c.JSON(500, gin.H{"ok": false, "msg": "备份创建失败，请检查服务器日志"})
			return
		}
		logOperation(db, getAdminUsername(c), c, "创建备份")
		c.JSON(http.StatusOK, gin.H{"ok": true, "file": filepath.Base(backupPath)})
	}
}

func resolveBackupPath(projectRoot, name string) (string, string, error) {
	baseName := filepath.Base(name)
	if baseName == "." || baseName == string(os.PathSeparator) || baseName == "" || !strings.HasSuffix(baseName, ".tar.gz") {
		return "", "", fmt.Errorf("非法备份文件")
	}
	backupDir := filepath.Join(projectRoot, "backups")
	backupPath := filepath.Clean(filepath.Join(backupDir, baseName))
	cleanBackupDir := filepath.Clean(backupDir)
	if !strings.HasPrefix(backupPath, cleanBackupDir+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("非法路径")
	}
	return backupPath, baseName, nil
}

func AdminBackupRestore(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 获取写锁，串行化恢复操作；持有期间其他持锁请求会通过 TryLock 快速失败
		restoreMu.Lock()
		defer restoreMu.Unlock()

		restoreInProgress.Store(true)
		defer restoreInProgress.Store(false)

		projectRoot := getProjectRoot(c)
		backupFile, _, err := resolveBackupPath(projectRoot, c.Param("name"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": err.Error()})
			return
		}

		log.Printf("[Restore] Starting restore from: %s", backupFile)

		// 先关闭数据库连接，避免覆盖正在使用的文件
		db.Close()

		err = utils.RestoreBackup(utils.DefaultBackupConfig(projectRoot), backupFile)
		if err != nil {
			log.Printf("[Restore] Failed: %v", err)
			// 尝试重新打开数据库
			if reopenErr := models.InitDB(filepath.Join(projectRoot, "uniflow.db")); reopenErr != nil {
				log.Printf("[Restore] Failed to reopen DB after restore failure: %v", reopenErr)
			}
			c.JSON(500, gin.H{"ok": false, "msg": "恢复失败，请检查备份文件或服务器日志"})
			return
		}

		// 重新打开数据库（注意：models.DB 被赋了新值，但所有路由闭包里的 db 仍指向旧连接）
		// 因此恢复成功后必须重启进程，否则后续请求会使用已关闭的旧连接。
		if reopenErr := models.InitDB(filepath.Join(projectRoot, "uniflow.db")); reopenErr != nil {
			log.Printf("[Restore] Failed to reopen DB after restore: %v", reopenErr)
			c.JSON(500, gin.H{"ok": false, "msg": "恢复成功但无法重新连接数据库，请手动重启服务"})
			return
		}

		log.Printf("[Restore] Success — database reopened, scheduling process restart")
		c.JSON(http.StatusOK, gin.H{"ok": true, "msg": "恢复成功，服务将在 2 秒后自动重启..."})

		// 异步重启进程：所有路由闭包捕获的 db 指向已关闭的旧连接，
		// 只有重启进程才能让所有 handler 使用新连接。
		go func() {
			time.Sleep(2 * time.Second)
			log.Println("[Restore] Restarting process after backup restore...")
			exe, _ := os.Executable()
			cmd := exec.Command(exe, os.Args[1:]...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.Stdin = os.Stdin
			// 继承环境变量（PORT, DB_PATH 等）
			cmd.Env = os.Environ()
			_ = cmd.Start()
			// 让子进程脱离父进程，然后退出
			os.Exit(0)
		}()
	}
}

func AdminBackupDelete(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectRoot := getProjectRoot(c)
		backupPath, _, err := resolveBackupPath(projectRoot, c.Param("name"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": err.Error()})
			return
		}
		if err := utils.DeleteBackup(backupPath); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "删除失败"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// AdminBackupDownload 需要认证的备份文件下载
func AdminBackupDownload(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectRoot := getProjectRoot(c)
		backupPath, backupName, err := resolveBackupPath(projectRoot, c.Param("name"))
		if err != nil {
			c.String(http.StatusBadRequest, "非法路径")
			return
		}

		info, err := os.Stat(backupPath)
		if err != nil || info.IsDir() {
			c.String(http.StatusNotFound, "文件不存在")
			return
		}

		c.FileAttachment(backupPath, backupName)
	}
}

// AdminBackupUpload 处理上传备份文件
func AdminBackupUpload(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		file, header, err := c.Request.FormFile("backup")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "请选择备份文件"})
			return
		}
		defer file.Close()

		// 检查文件扩展名
		if !strings.HasSuffix(strings.ToLower(header.Filename), ".tar.gz") {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "仅支持 .tar.gz 格式的备份文件"})
			return
		}

		// 清理文件名，防止路径穿越
		name := filepath.Base(header.Filename)
		if strings.Contains(name, "..") {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "非法文件名"})
			return
		}

		projectRoot := getProjectRoot(c)
		backupsDir := filepath.Join(projectRoot, "backups")
		if err := os.MkdirAll(backupsDir, 0755); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "创建备份目录失败"})
			return
		}

		destPath := filepath.Join(backupsDir, name)
		out, err := os.Create(destPath)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "创建文件失败"})
			return
		}
		defer out.Close()

		written, err := io.Copy(out, io.LimitReader(file, maxUploadSize))
		if err != nil {
			os.Remove(destPath)
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "写入文件失败"})
			return
		}

		log.Printf("[Backup] Uploaded: %s (%s)", name, utils.FormatFileSize(written))
		c.JSON(http.StatusOK, gin.H{
			"ok":   true,
			"msg":  "上传成功",
			"name": name,
			"size": utils.FormatFileSize(written),
		})
	}
}

// ============ 日志管理 ============

func AdminLogs(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("日志管理", "system", "logs", getAdminUsername(c), db)
		data["LogType"] = c.DefaultQuery("type", "login")

		logType := c.DefaultQuery("type", "login")
		page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
		if page < 1 {
			page = 1
		}
		pageSize := 20
		offset := (page - 1) * pageSize

		var total int
		db.QueryRow("SELECT COUNT(*) FROM logs WHERE log_type=?", logType).Scan(&total)

		rows, _ := db.Query("SELECT id, log_type, operator, action, ip, user_agent, result, created_at FROM logs WHERE log_type=? ORDER BY id DESC LIMIT ? OFFSET ?", logType, pageSize, offset)
		var logs []gin.H
		if rows != nil {
			for rows.Next() {
				var l struct {
					ID        int64
					LogType   string
					Operator  string
					Action    string
					IP        string
					UserAgent string
					Result    string
					CreatedAt time.Time
				}
				scanLog(rows.Scan(&l.ID, &l.LogType, &l.Operator, &l.Action, &l.IP, &l.UserAgent, &l.Result, &l.CreatedAt), "adminLogs")
				logs = append(logs, gin.H{
					"ID": l.ID, "LogType": l.LogType, "Operator": l.Operator, "Action": l.Action,
					"IP": l.IP, "UserAgent": truncate(l.UserAgent, 40), "Result": l.Result,
					"CreatedAt": l.CreatedAt.Format("2006-01-02 15:04:05"),
				})
			}
			rows.Close()
		}

		data["Logs"] = logs
		data["Page"] = page
		data["TotalPages"] = (total + pageSize - 1) / pageSize
		c.HTML(http.StatusOK, "admin/log.html", data)
	}
}

// ============ 辅助函数 ============

func logOperation(db *sql.DB, username string, c *gin.Context, action string) {
	ip := getClientIP(c)
	ua := c.Request.UserAgent()
	db.Exec("INSERT INTO logs (log_type, operator, action, ip, user_agent, result) VALUES ('operation', ?, ?, ?, ?, 'success')",
		username, action, ip, ua)
}

// formatIP 将 IPv6 localhost ::1 转为 IPv4 127.0.0.1 显示
func formatIP(ip string) string {
	if ip == "::1" {
		return "127.0.0.1"
	}
	// 去掉 IPv6 映射前缀 ::ffff:
	if strings.HasPrefix(ip, "::ffff:") {
		return strings.TrimPrefix(ip, "::ffff:")
	}
	return ip
}

func getClientIP(c *gin.Context) string {
	return formatIP(c.ClientIP())
}

func getProjectRoot(c *gin.Context) string {
	root, _ := c.Get("project_root")
	if r, ok := root.(string); ok {
		return r
	}
	return "."
}

func nilIfZero(n int64) interface{} {
	if n == 0 {
		return nil
	}
	return n
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// formatPublishAt 将表单中的 datetime-local 格式 "2006-01-02T15:04" 转换为
// SQLite 标准格式 "2006-01-02 15:04:05"，便于 Scan(*time.Time) 和字符串比较。
// 空字符串返回 nil。
func formatPublishAt(formVal string) interface{} {
	if formVal == "" {
		return nil
	}
	t, err := time.ParseInLocation("2006-01-02T15:04", formVal, time.Local)
	if err != nil {
		return nil
	}
	return t.Format("2006-01-02 15:04:05")
}

func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

// PublishScheduledPosts 发布定时文章（restoreMu.RLock 保护恢复期间的安全）
func PublishScheduledPosts(db *sql.DB) {
	restoreMu.RLock()
	defer restoreMu.RUnlock()
	now := time.Now().Format("2006-01-02 15:04:05")
	result, err := db.Exec("UPDATE posts SET status='published', updated_at=? WHERE status='scheduled' AND publish_at <= ? AND publish_at IS NOT NULL", now, now)
	if err != nil {
		return
	}
	affected, _ := result.RowsAffected()
	if affected > 0 {
		log.Printf("[Scheduler] Published %d scheduled post(s)", affected)
	}
}

// ============ 前台评论/留言 ============

// AuthCheck 前台检查登录状态（JS 可调用，解决 HttpOnly cookie 无法被 document.cookie 读取的问题）
func AuthCheck(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		cookie, err := c.Cookie("uniflow_session")
		if err != nil || cookie == "" {
			c.JSON(http.StatusOK, gin.H{"loggedIn": false})
			return
		}
		token, ok := VerifyCookie(cookie)
		if !ok {
			c.JSON(http.StatusOK, gin.H{"loggedIn": false})
			return
		}
		username, ok := ValidateSession(token)
		if !ok {
			c.JSON(http.StatusOK, gin.H{"loggedIn": false})
			return
		}
		var avatarURL string
		db.QueryRow("SELECT avatar_url FROM users WHERE username = ?", username).Scan(&avatarURL)
		c.JSON(http.StatusOK, gin.H{"loggedIn": true, "username": username, "avatar": avatarURL})
	}
}

func CommentSubmit(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 尝试从登录态获取用户信息，未登录也可评论
		var loginUsername string
		var avatarURL string
		session, err := c.Cookie("uniflow_session")
		if err == nil && session != "" {
			token, ok := VerifyCookie(session)
			if ok {
				username, ok2 := ValidateSession(token)
				if ok2 {
					loginUsername = username
					db.QueryRow("SELECT avatar_url FROM users WHERE username = ?", username).Scan(&avatarURL)
				}
			}
		}

		// 未登录用户需提供昵称
		author := strings.TrimSpace(c.PostForm("author"))
		if loginUsername == "" {
			if author == "" {
				c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "请输入昵称"})
				return
			}
			authorLen := len([]rune(author))
			if authorLen < 3 || authorLen > 10 {
				c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "昵称需要 3-10 个字"})
				return
			}
		} else {
			author = loginUsername
		}

		targetType := c.PostForm("target_type")
		targetID, _ := strconv.ParseInt(c.PostForm("target_id"), 10, 64)
		content := utils.SanitizeHTML(c.PostForm("content"))
		parentID, _ := strconv.ParseInt(c.PostForm("parent_id"), 10, 64)
		replyTo := utils.SanitizeHTML(c.PostForm("reply_to"))

		if content == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "内容不能为空"})
			return
		}
		if len([]rune(content)) > 500 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "内容不能超过500字"})
			return
		}

		// 登录用户可上传评论图片
		var imageURL string
		if loginUsername != "" {
			file, fh, fErr := c.Request.FormFile("image")
			if fErr == nil {
				defer file.Close()
				projectRoot := getProjectRoot(c)
				uploadsDir := filepath.Join(projectRoot, "uploads")
				os.MkdirAll(uploadsDir, 0755)
				tmpPath := filepath.Join(uploadsDir, "tmp_comment_"+uuid.New().String()+filepath.Ext(filepath.Base(fh.Filename)))
				if saveErr := c.SaveUploadedFile(fh, tmpPath); saveErr == nil {
					if filename, _, procErr := utils.ProcessUploadedFile(tmpPath, uploadsDir); procErr == nil && filename != "" {
						imageURL = "/uploads/" + filename
					}
				}
			}
		}

		// 验证目标是否存在
		if targetID <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "无效的目标"})
			return
		}
		if targetType == "post" {
			var exists int
			db.QueryRow("SELECT 1 FROM posts WHERE id = ?", targetID).Scan(&exists)
			if exists == 0 {
				c.JSON(http.StatusNotFound, gin.H{"ok": false, "msg": "文章不存在"})
				return
			}
		} else if targetType == "moment" {
			var exists int
			db.QueryRow("SELECT 1 FROM moments WHERE id = ?", targetID).Scan(&exists)
			if exists == 0 {
				c.JSON(http.StatusNotFound, gin.H{"ok": false, "msg": "瞬间不存在"})
				return
			}
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "无效的目标目录类型"})
			return
		}

		result, err := db.Exec("INSERT INTO comments (target_type, target_id, author, author_avatar, content, image_url, parent_id, reply_to) VALUES (?,?,?,?,?,?,?,?)",
			targetType, targetID, author, avatarURL, content, imageURL, parentID, replyTo)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "保存失败"})
			return
		}
		id, _ := result.LastInsertId()
		var createdAt time.Time
		var likes int64
		db.QueryRow("SELECT created_at, likes FROM comments WHERE id=?", id).Scan(&createdAt, &likes)
		c.JSON(http.StatusOK, gin.H{
			"ok": true, "msg": "评论成功",
			"id": id, "author": author, "author_avatar": avatarURL,
			"content": content, "image_url": imageURL,
			"created_at": createdAt.Format("2006-01-02 15:04"),
			"parent_id": parentID, "reply_to": replyTo,
			"likes": likes,
		})
	}
}

func GuestbookSubmit(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		author := strings.TrimSpace(c.PostForm("author"))
		content := utils.SanitizeHTML(c.PostForm("content"))

		if author == "" || content == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "昵称和内容不能为空"})
			return
		}
		authorLen := len([]rune(author))
		if authorLen < 3 || authorLen > 10 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "昵称需要 3-10 个字"})
			return
		}
		if len([]rune(content)) > 500 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "留言内容不能超过500个字"})
			return
		}

		now := time.Now().Format("2006-01-02 15:04:05")
		if _, err := db.Exec("INSERT INTO guestbook (author, content, ip, created_at) VALUES (?,?,?,?)",
			author, content, getClientIP(c), now); err != nil {
			log.Printf("[GuestbookSubmit] error: %v", err)
		}

		c.JSON(http.StatusOK, gin.H{"ok": true, "msg": "留言成功"})
	}
}

// PostLikeHandler 文章点赞（同设备同文章只能点赞一次，再次点击取消）
func PostLikeHandler(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false})
			return
		}
		// 校验文章存在且已发布
		var exists int
		db.QueryRow("SELECT 1 FROM posts WHERE id = ? AND status = 'published'", id).Scan(&exists)
		if exists == 0 {
			c.JSON(http.StatusNotFound, gin.H{"ok": false, "msg": "文章不存在"})
			return
		}

		// 获取或生成访客标识
		vid, _ := c.Cookie("visitor_id")
		if vid == "" {
			vid = uuid.New().String()[:8]
			c.SetCookie("visitor_id", vid, 365*24*3600, "/", "", false, true)
		}

		// 用事务保证点赞/取消与计数的一致性
		tx, txErr := db.Begin()
		if txErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "操作失败，请稍后重试"})
			return
		}
		defer tx.Rollback() //nolint:errcheck

		// 尝试插入点赞记录（利用 UNIQUE(post_id, visitor_id) 约束防重复）
		res, insErr := tx.Exec("INSERT OR IGNORE INTO post_likes (post_id, visitor_id, type) VALUES (?, ?, 'like')", id, vid)
		if insErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "操作失败，请稍后重试"})
			return
		}
		n, _ := res.RowsAffected()
		var (
			liked bool
			count int64
		)
		if n > 0 {
			// 新点赞：如果之前有 dislike 记录，INSERT OR IGNORE 会因为 UNIQUE 约束失败（n=0）
			// 所以 n>0 说明之前没有任何记录
			tx.QueryRow("UPDATE posts SET like_count = like_count + 1 WHERE id=? RETURNING like_count", id).Scan(&count)
			liked = true
		} else {
			// 已有记录，检查是 like 还是 dislike
			var existingType string
			tx.QueryRow("SELECT type FROM post_likes WHERE post_id=? AND visitor_id=?", id, vid).Scan(&existingType)
			if existingType == "like" {
				// 已点赞，取消
				tx.Exec("DELETE FROM post_likes WHERE post_id=? AND visitor_id=?", id, vid)
				tx.QueryRow("UPDATE posts SET like_count = MAX(0, like_count - 1) WHERE id=? RETURNING like_count", id).Scan(&count)
				liked = false
			} else {
				// 之前是 dislike，切换为 like
				tx.Exec("UPDATE post_likes SET type='like' WHERE post_id=? AND visitor_id=?", id, vid)
				tx.QueryRow("UPDATE posts SET like_count = like_count + 1, dislike_count = MAX(0, dislike_count - 1) WHERE id=? RETURNING like_count", id).Scan(&count)
				liked = true
			}
		}
		if err := tx.Commit(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "操作失败，请稍后重试"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "count": count, "liked": liked})
	}
}

// PostDislikeHandler 文章拍砖（同设备同文章只能拍砖一次，再次点击取消）
func PostDislikeHandler(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false})
			return
		}
		var exists int
		db.QueryRow("SELECT 1 FROM posts WHERE id = ? AND status = 'published'", id).Scan(&exists)
		if exists == 0 {
			c.JSON(http.StatusNotFound, gin.H{"ok": false, "msg": "文章不存在"})
			return
		}

		vid, _ := c.Cookie("visitor_id")
		if vid == "" {
			vid = uuid.New().String()[:8]
			c.SetCookie("visitor_id", vid, 365*24*3600, "/", "", false, true)
		}

		tx, txErr := db.Begin()
		if txErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "操作失败，请稍后重试"})
			return
		}
		defer tx.Rollback() //nolint:errcheck

		res, insErr := tx.Exec("INSERT OR IGNORE INTO post_likes (post_id, visitor_id, type) VALUES (?, ?, 'dislike')", id, vid)
		if insErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "操作失败，请稍后重试"})
			return
		}
		n, _ := res.RowsAffected()
		var (
			disliked bool
			count    int64
		)
		if n > 0 {
			// 新拍砖：之前没有任何记录
			tx.QueryRow("UPDATE posts SET dislike_count = dislike_count + 1 WHERE id=? RETURNING dislike_count", id).Scan(&count)
			disliked = true
		} else {
			// 已有记录，检查是 like 还是 dislike
			var existingType string
			tx.QueryRow("SELECT type FROM post_likes WHERE post_id=? AND visitor_id=?", id, vid).Scan(&existingType)
			if existingType == "dislike" {
				// 已拍砖，取消
				tx.Exec("DELETE FROM post_likes WHERE post_id=? AND visitor_id=?", id, vid)
				tx.QueryRow("UPDATE posts SET dislike_count = MAX(0, dislike_count - 1) WHERE id=? RETURNING dislike_count", id).Scan(&count)
				disliked = false
			} else {
				// 之前是 like，切换为 dislike
				tx.Exec("UPDATE post_likes SET type='dislike' WHERE post_id=? AND visitor_id=?", id, vid)
				tx.QueryRow("UPDATE posts SET dislike_count = dislike_count + 1, like_count = MAX(0, like_count - 1) WHERE id=? RETURNING dislike_count", id).Scan(&count)
				disliked = true
			}
		}
		if err := tx.Commit(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "操作失败，请稍后重试"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "count": count, "disliked": disliked})
	}
}

// ============ 首次部署引导 ============

// SetupCheckMiddleware 检查是否已完成初始化，未完成则跳转引导页
func SetupCheckMiddleware(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		if restoreInProgress.Load() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"ok": false, "msg": "系统正在恢复备份，请稍后再试"})
			c.Abort()
			return
		}
		if strings.HasPrefix(c.Request.URL.Path, "/setup") {
			c.Next()
			return
		}
		if strings.HasPrefix(c.Request.URL.Path, "/static") ||
			strings.HasPrefix(c.Request.URL.Path, "/uploads") {
			c.Next()
			return
		}
		var count int
		db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count)
		if count == 0 {
			c.Redirect(http.StatusFound, "/setup")
			c.Abort()
			return
		}
		c.Next()
	}
}

// SetupPage 首次部署引导页
func SetupPage(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var count int
		db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count)
		if count > 0 {
			c.Redirect(http.StatusFound, "/")
			return
		}
		c.HTML(http.StatusOK, "setup.html", gin.H{"Error": ""})
	}
}

// SetupPost 处理初始化提交
func SetupPost(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		setupMu.Lock()
		defer setupMu.Unlock()

		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil {
			c.HTML(http.StatusOK, "setup.html", gin.H{"Error": "初始化检查失败，请重试"})
			return
		}
		if count > 0 {
			c.Redirect(http.StatusFound, "/")
			return
		}

		siteTitle := c.PostForm("site_title")
		nickname := c.PostForm("nickname")
		username := c.PostForm("username")
		password := c.PostForm("password")

		if username == "" || password == "" || siteTitle == "" {
			c.HTML(http.StatusOK, "setup.html", gin.H{"Error": "所有字段都不能为空"})
			return
		}
		if len(password) < 6 {
			c.HTML(http.StatusOK, "setup.html", gin.H{"Error": "密码至少6位"})
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			c.HTML(http.StatusOK, "setup.html", gin.H{"Error": "密码加密失败，请重试"})
			return
		}

		// 创建管理员：setupMu 保证单进程内检查+插入原子化，避免首次部署并发创建多个管理员。
		if _, err := db.Exec("INSERT INTO users (username, password, role) VALUES (?, ?, 'admin')", username, string(hash)); err != nil {
			c.HTML(http.StatusOK, "setup.html", gin.H{"Error": "创建管理员失败，请重试"})
			return
		}

		// 设置站点名称
		db.Exec("INSERT INTO system_settings (key, value) VALUES ('site_title', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", siteTitle)

		// 设置关于我昵称
		aboutJSON, _ := json.Marshal(map[string]string{
			"name":     nickname,
			"avatar":   "",
			"bio":      "热爱技术与生活",
			"location": "",
			"github":   "",
			"twitter":  "",
			"email":    "",
		})
		db.Exec("INSERT INTO system_settings (key, value) VALUES ('about_me_json', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", string(aboutJSON))

		InvalidateSiteSettingsCache()

		// 自动登录
		sessionToken := GenerateSession(username)
		setSessionCookie(c, SignCookie(sessionToken), 86400*7)

		c.Redirect(http.StatusFound, "/admin")
	}
}

// ============ 版本检查 ============

type updateCheckCache struct {
	mu        sync.Mutex
	checkedAt time.Time
	payload   gin.H
}

var adminUpdateCache updateCheckCache

// AdminCheckUpdate 检查 GitHub 最新版本
func AdminCheckUpdate(currentVersion string) gin.HandlerFunc {
	return func(c *gin.Context) {
		adminUpdateCache.mu.Lock()
		if adminUpdateCache.payload != nil && time.Since(adminUpdateCache.checkedAt) < time.Hour {
			payload := gin.H{}
			for k, v := range adminUpdateCache.payload {
				payload[k] = v
			}
			adminUpdateCache.mu.Unlock()
			c.JSON(http.StatusOK, payload)
			return
		}
		adminUpdateCache.mu.Unlock()

		client := &http.Client{Timeout: 10 * time.Second}
		req, _ := http.NewRequest("GET", "https://api.github.com/repos/geekou/uniflow/releases/latest", nil)
		req.Header.Set("User-Agent", "UniFlow/"+currentVersion)
		resp, err := client.Do(req)
		if err != nil {
			payload := gin.H{"ok": true, "current": currentVersion, "latest": "", "hasUpdate": false, "error": "无法连接 GitHub，请稍后重试"}
			adminUpdateCache.mu.Lock()
			// 失败响应只缓存 1 分钟，避免网络恢复后仍返回错误
			adminUpdateCache.checkedAt = time.Now().Add(59 * time.Minute * -1)
			adminUpdateCache.payload = payload
			adminUpdateCache.mu.Unlock()
			c.JSON(http.StatusOK, payload)
			return
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		var release struct {
			TagName string `json:"tag_name"`
		}
		json.Unmarshal(body, &release)

		hasUpdate := release.TagName != "" && release.TagName != currentVersion
		payload := gin.H{
			"ok":        true,
			"current":   currentVersion,
			"latest":    release.TagName,
			"hasUpdate": hasUpdate,
		}
		adminUpdateCache.mu.Lock()
		adminUpdateCache.checkedAt = time.Now()
		adminUpdateCache.payload = payload
		adminUpdateCache.mu.Unlock()

		c.JSON(http.StatusOK, payload)
	}
}

// CommentLike 切换评论点赞状态（自动生成匿名 ID，同一设备同一评论只能点赞一次）
func CommentLike(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			CommentID int64 `json:"comment_id"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || req.CommentID <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "参数错误"})
			return
		}

		// 用匿名 cookie 标识设备
			anonID, err := c.Cookie("_aid")
			if err != nil || anonID == "" {
				anonID = uuid.New().String()
				secure := c.Request.TLS != nil && !strings.HasPrefix(c.Request.Host, "localhost") && !strings.HasPrefix(c.Request.Host, "127.0.0.1")
				http.SetCookie(c.Writer, &http.Cookie{
					Name:     "_aid",
					Value:    anonID,
					Path:     "/",
					MaxAge:   365 * 24 * 3600,
					HttpOnly: true,
					Secure:   secure,
					SameSite: http.SameSiteLaxMode,
				})
			}

			// 用事务保证点赞/取消与计数的一致性，避免竞态
			tx, txErr := db.Begin()
			if txErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "操作失败，请稍后重试"})
				return
			}
			defer tx.Rollback() //nolint:errcheck

			// 尝试插入点赞记录（利用 UNIQUE 约束防重复）
			res, insErr := tx.Exec("INSERT OR IGNORE INTO comment_likes (comment_id, anonymous_id) VALUES (?, ?)", req.CommentID, anonID)
			if insErr != nil {
				log.Printf("[CommentLike] insert error: %v", insErr)
				c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "操作失败，请稍后重试"})
				return
			}
			n, _ := res.RowsAffected()
			var (
				liked     bool
				finalLike int64
			)
			if n > 0 {
				// 新点赞
				tx.QueryRow("UPDATE comments SET likes = likes + 1 WHERE id=? RETURNING likes", req.CommentID).Scan(&finalLike)
				liked = true
			} else {
				// 已存在，取消点赞；仅当确实删除了一条记录时才 -1，防止重复取消导致计数错乱
				delRes, _ := tx.Exec("DELETE FROM comment_likes WHERE comment_id=? AND anonymous_id=?", req.CommentID, anonID)
				if dn, _ := delRes.RowsAffected(); dn > 0 {
					tx.QueryRow("UPDATE comments SET likes = MAX(0, likes - 1) WHERE id=? RETURNING likes", req.CommentID).Scan(&finalLike)
				} else {
					tx.QueryRow("SELECT likes FROM comments WHERE id=?", req.CommentID).Scan(&finalLike)
				}
				liked = false
			}
			if err := tx.Commit(); err != nil {
				log.Printf("[CommentLike] commit error: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "操作失败，请稍后重试"})
				return
			}

			c.JSON(http.StatusOK, gin.H{"ok": true, "likes": finalLike, "liked": liked})
	}
}

// ============ 设备管理 ============

func AdminDevices(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("设备管理", "settings", "devices", getAdminUsername(c), db)
		rows, _ := db.Query("SELECT id, name, image_url, info, order_num FROM devices ORDER BY order_num ASC, id ASC")
		var devices []models.Device
		if rows != nil {
			for rows.Next() {
				var d models.Device
				rows.Scan(&d.ID, &d.Name, &d.ImageURL, &d.Info, &d.OrderNum)
				devices = append(devices, d)
			}
			rows.Close()
		}
		data["Devices"] = devices
		c.HTML(http.StatusOK, "admin/devices.html", data)
	}
}

func AdminDeviceSave(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.PostForm("id"), 10, 64)
		name := strings.TrimSpace(c.PostForm("name"))
		imageURL := utils.SafeImageURL(c.PostForm("image_url"))
		info := strings.TrimSpace(c.PostForm("info"))
		orderNum, _ := strconv.Atoi(c.PostForm("order_num"))

		if name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "设备名不能为空"})
			return
		}

		// 处理本地上传图片（限制 5MB + 校验图片 MIME 类型）
		file, header, err := c.Request.FormFile("image_file")
		if err == nil {
			defer file.Close()
			if header.Size > 5<<20 {
				c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "图片不能超过 5MB"})
				return
			}
			projectRoot := getProjectRoot(c)
			uploadsDir := filepath.Join(projectRoot, "uploads")
			os.MkdirAll(uploadsDir, 0755)
			tmpPath := filepath.Join(uploadsDir, "tmp_device_"+uuid.New().String()+filepath.Ext(filepath.Base(header.Filename)))
			if saveErr := c.SaveUploadedFile(header, tmpPath); saveErr == nil {
				if filename, _, procErr := utils.ProcessUploadedFile(tmpPath, uploadsDir); procErr == nil && filename != "" {
					// ProcessUploadedFile 输出到 uploads/ 根目录，这里需要移动到 devices 子目录
					srcFile := filepath.Join(uploadsDir, filename)
					devDir := filepath.Join(projectRoot, "uploads", "devices")
					os.MkdirAll(devDir, 0755)
					finalName := "device_" + strconv.FormatInt(time.Now().UnixNano(), 10) + filepath.Ext(filename)
					finalPath := filepath.Join(devDir, finalName)
					if mvErr := os.Rename(srcFile, finalPath); mvErr == nil {
						imageURL = "/uploads/devices/" + finalName
					} else {
						os.Remove(srcFile)
					}
				} else {
					os.Remove(tmpPath)
				}
			}
		}
		// 如果没有本地上传则使用 URL 输入

		if id > 0 {
			db.Exec("UPDATE devices SET name=?, image_url=?, info=?, order_num=? WHERE id=?", name, imageURL, info, orderNum, id)
		} else {
			db.Exec("INSERT INTO devices (name, image_url, info, order_num) VALUES (?,?,?,?)", name, imageURL, info, orderNum)
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func AdminDeviceDelete(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		db.Exec("DELETE FROM devices WHERE id=?", id)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// ============ 足迹管理 ============

func AdminFootprints(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("足迹管理", "settings", "footprints", getAdminUsername(c), db)
		var raw string
		db.QueryRow("SELECT value FROM system_settings WHERE key='footprints_json'").Scan(&raw)
		var footprints []map[string]interface{}
		if raw != "" {
			json.Unmarshal([]byte(raw), &footprints)
		}
		data["Footprints"] = footprints
		c.HTML(http.StatusOK, "admin/footprints.html", data)
	}
}

func AdminFootprintSave(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := strings.TrimSpace(c.PostForm("name"))
		lat, _ := strconv.ParseFloat(c.PostForm("lat"), 64)
		lng, _ := strconv.ParseFloat(c.PostForm("lng"), 64)
		note := strings.TrimSpace(c.PostForm("note"))
		index, _ := strconv.Atoi(c.PostForm("index"))

		if name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "地名不能为空"})
			return
		}

		// 用事务串行化读-改-写，避免并发覆盖
		tx, err := db.Begin()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "操作失败"})
			return
		}
		defer tx.Rollback() //nolint:errcheck

		var list []map[string]interface{}
		var raw string
		tx.QueryRow("SELECT value FROM system_settings WHERE key='footprints_json'").Scan(&raw)
		if raw != "" {
			json.Unmarshal([]byte(raw), &list)
		}

		entry := map[string]interface{}{"name": name, "lat": lat, "lng": lng, "note": note}
		if index >= 0 && index < len(list) {
			list[index] = entry
		} else {
			list = append(list, entry)
		}

		newRaw, _ := json.Marshal(list)
		if _, err := tx.Exec("INSERT OR REPLACE INTO system_settings (key, value) VALUES ('footprints_json', ?)", string(newRaw)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "保存失败"})
			return
		}
		if err := tx.Commit(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "保存失败"})
			return
		}
		InvalidateSiteSettingsCache()
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func AdminFootprintDelete(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		index, _ := strconv.Atoi(c.Param("index"))
		if index < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "非法索引"})
			return
		}

		// 用事务串行化读-改-写，避免并发覆盖
		tx, err := db.Begin()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "操作失败"})
			return
		}
		defer tx.Rollback() //nolint:errcheck

		var list []map[string]interface{}
		var raw string
		tx.QueryRow("SELECT value FROM system_settings WHERE key='footprints_json'").Scan(&raw)
		if raw != "" {
			json.Unmarshal([]byte(raw), &list)
		}
		if index >= len(list) {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "索引超出范围"})
			return
		}
		list = append(list[:index], list[index+1:]...)

		newRaw, _ := json.Marshal(list)
		if _, err := tx.Exec("INSERT OR REPLACE INTO system_settings (key, value) VALUES ('footprints_json', ?)", string(newRaw)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "删除失败"})
			return
		}
		if err := tx.Commit(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "删除失败"})
			return
		}
		InvalidateSiteSettingsCache()
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}
