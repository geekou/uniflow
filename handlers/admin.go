package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"uniflow/models"
	"uniflow/utils"
)

// maxUploadSize 上传文件最大大小（500MB）
const maxUploadSize int64 = 500 << 20

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
		data := adminData("文章列表", "posts", "list", getAdminUsername(c), db)
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

func AdminPostCreate(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		data := adminData("发布文章", "posts", "create", getAdminUsername(c), db)
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
		content := c.PostForm("content")
		if title == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "标题不能为空"})
			return
		}
		categoryID, _ := strconv.ParseInt(c.PostForm("category_id"), 10, 64)
		isTop, _ := strconv.Atoi(c.PostForm("is_top"))
		privacy := c.PostForm("privacy")
		if privacy != "public" && privacy != "private" {
			privacy = "public"
		}
		status := c.PostForm("status")
		if status != "published" && status != "draft" && status != "scheduled" {
			status = "draft"
		}
		publishAt := c.PostForm("publish_at")

		// 定时发布：如果 publish_at 有值且在未来，强制 status=scheduled
		if publishAt != "" {
			if t, err := time.ParseInLocation("2006-01-02T15:04", publishAt, time.Local); err == nil && t.After(time.Now()) {
				status = "scheduled"
			}
		}

		thumbURL := c.PostForm("thumb_url")
		// 处理封面图上传
		file, header, err := c.Request.FormFile("cover")
		if err == nil {
			defer file.Close()
			projectRoot := getProjectRoot(c)
			uploadsDir := filepath.Join(projectRoot, "uploads")
			os.MkdirAll(uploadsDir, 0755)
			tmpPath := filepath.Join(uploadsDir, "tmp_cover_"+header.Filename)
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
			thumbURL = c.PostForm("thumb_url")
		}

		result, err := db.Exec(
			"INSERT INTO posts (title, content, thumb_url, author, category_id, is_top, privacy, status, publish_at) VALUES (?,?,?,?,?,?,?,?,?)",
			title, content, thumbURL, getAdminUsername(c), nilIfZero(categoryID), isTop, privacy, status, nilIfEmpty(publishAt),
		)
		if err != nil {
			c.String(500, "保存失败: %v", err)
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
		data := adminData("编辑文章", "posts", "list", getAdminUsername(c), db)
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
		title := c.PostForm("title")
		content := c.PostForm("content")
		categoryID, _ := strconv.ParseInt(c.PostForm("category_id"), 10, 64)
		isTop, _ := strconv.Atoi(c.PostForm("is_top"))
		privacy := c.PostForm("privacy")
		status := c.PostForm("status")
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
			tmpPath := filepath.Join(uploadsDir, "tmp_cover_"+header.Filename)
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
			title, content, thumbURL, nilIfZero(categoryID), isTop, privacy, status, nilIfEmpty(publishAt), time.Now(), id,
		); err != nil {
			c.String(500, "更新失败: %v", err)
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
		name := c.PostForm("name")
		slug := c.PostForm("slug")
		editIDStr := c.PostForm("edit_id")

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
		name := c.PostForm("name")
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

		rows, _ := db.Query("SELECT id, content, media_urls, likes, created_at FROM moments ORDER BY id DESC")
		var moments []gin.H
		if rows != nil {
			for rows.Next() {
				var m struct {
					ID        int64
					Content   string
					MediaURLs string
					Likes     int64
					CreatedAt time.Time
				}
				scanLog(rows.Scan(&m.ID, &m.Content, &m.MediaURLs, &m.Likes, &m.CreatedAt), "adminMoments")
				var mediaList []string
				if m.MediaURLs != "" {
					mediaList = strings.Split(m.MediaURLs, ",")
				}
				moments = append(moments, gin.H{
					"ID": m.ID, "Content": m.Content, "MediaCount": len(mediaList),
					"MediaList": mediaList, "MediaURLs": m.MediaURLs,
					"Likes": m.Likes, "CreatedAt": m.CreatedAt,
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
		content := c.PostForm("content")

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
				tmpPath := filepath.Join(uploadsDir, "tmp_"+fh.Filename)
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

		if createdAt != "" {
			// 解析用户提供的日期时间
			t, err := time.Parse("2006-01-02T15:04", createdAt)
			if err == nil {
				execLog(db, "INSERT INTO moments (content, media_urls, created_at) VALUES (?, ?, ?)", content, mediaURLs, t.Format("2006-01-02 15:04:05"))
			} else {
				execLog(db, "INSERT INTO moments (content, media_urls) VALUES (?, ?)", content, mediaURLs)
			}
		} else {
			execLog(db, "INSERT INTO moments (content, media_urls) VALUES (?, ?)", content, mediaURLs)
		}
		logOperation(db, getAdminUsername(c), c, "发布瞬间")
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
		err := db.QueryRow("SELECT id, content, media_urls, likes, created_at FROM moments WHERE id = ?", id).
			Scan(&m.ID, &m.Content, &m.MediaURLs, &m.Likes, &m.CreatedAt)
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
					// 从磁盘删除文件
					projectRoot := getProjectRoot(c)
					filePath := filepath.Join(projectRoot, m)
					os.Remove(filePath)
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
				tmpPath := filepath.Join(uploadsDir, "tmp_"+fh.Filename)
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
				execLog(db, "UPDATE moments SET content=?, media_urls=?, created_at=?, updated_at=CURRENT_TIMESTAMP WHERE id=?", c.PostForm("content"), mediaURLs, t.Format("2006-01-02 15:04:05"), id)
			} else {
				execLog(db, "UPDATE moments SET content=?, media_urls=?, updated_at=CURRENT_TIMESTAMP WHERE id=?", c.PostForm("content"), mediaURLs, id)
			}
		} else {
			execLog(db, "UPDATE moments SET content=?, media_urls=?, updated_at=CURRENT_TIMESTAMP WHERE id=?", c.PostForm("content"), mediaURLs, id)
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

		rows, _ := db.Query("SELECT c1.id, c1.target_type, c1.target_id, c1.author, c1.author_avatar, c1.content, c1.image_url, c1.created_at, c1.parent_id, COALESCE(c2.author, '') as reply_to FROM comments c1 LEFT JOIN comments c2 ON c1.parent_id = c2.id ORDER BY c1.id DESC")
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
					CreatedAt    time.Time
					ParentID     int64
					ReplyTo      string
				}
				scanLog(rows.Scan(&cm.ID, &cm.TargetType, &cm.TargetID, &cm.Author, &cm.AuthorAvatar, &cm.Content, &cm.ImageURL, &cm.CreatedAt, &cm.ParentID, &cm.ReplyTo), "adminComments")
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
		content := c.PostForm("content")

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
		content := strings.TrimSpace(c.PostForm("content"))
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
			})
		}

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
			"image/jpeg": true, "image/png": true, "image/gif": true,
			"image/webp": true, "image/svg+xml": true, "video/mp4": true,
			"video/webm": true,
		}
		if !allowedTypes[contentType] {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "不支持的文件类型，仅允许图片和视频"})
			return
		}

		// 重置 file reader 以便后续保存
		file.Seek(0, 0)

		tmpPath := filepath.Join(uploadsDir, "tmp_"+header.Filename)
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
	if strings.Contains(name, "..") {
		return fmt.Errorf("非法路径")
	}
	return os.Remove(filepath.Join(projectRoot, "uploads", name))
}

func AdminMediaDelete(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 支持路径参数和 query parameter 两种方式
		name := c.Param("name")
		if name == "" {
			name = c.Query("url")
		}
		name, _ = url.QueryUnescape(name)
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

func isImageExt(ext string) bool {
	return ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif" || ext == ".webp" || ext == ".bmp" || ext == ".svg"
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

func AdminSettingsPost(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		keys := []string{"site_title", "site_subtitle", "sensitive_words", "comment_limit_count", "comment_limit_minute", "site_founded_at", "site_notification"}
		for _, key := range keys {
			value := c.PostForm(key)
			execLog(db, "INSERT INTO system_settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
		}

		// 处理 Banner 上传
		bannerURL := c.PostForm("banner_url")
		file, header, err := c.Request.FormFile("banner_file")
		if err == nil {
			defer file.Close()
			projectRoot := getProjectRoot(c)
			uploadsDir := filepath.Join(projectRoot, "uploads")
			os.MkdirAll(uploadsDir, 0755)
			tmpPath := filepath.Join(uploadsDir, "tmp_banner_"+header.Filename)
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
				log.Printf("[Banner] uploaded: tmp=%s filename=%s size=%d err=%v", tmpPath, filename, size, procErr)
				if filename != "" {
					bannerURL = "/uploads/" + filename
				}
			} else {
				log.Printf("[Banner] create tmp file failed: %v", err)
			}
		} else {
			log.Printf("[Banner] no file uploaded or error: %v", err)
		}
		log.Printf("[Banner] saving banner_url=%q", bannerURL)
		execLog(db, "INSERT INTO system_settings (key, value) VALUES ('banner_url', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", bannerURL)

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
			qrURL := c.PostForm(qf.dbKey)
			qrFile, qrHeader, qrErr := c.Request.FormFile(qf.formName)
			if qrErr == nil {
				defer qrFile.Close()
				projectRoot := getProjectRoot(c)
				uploadsDir := filepath.Join(projectRoot, "uploads")
				os.MkdirAll(uploadsDir, 0755)
				tmpPath := filepath.Join(uploadsDir, qf.prefix+qrHeader.Filename)
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
					am.SocialLinks = append(am.SocialLinks, SocialLink{Name: "GitHub", URL: v, Icon: "fa-brands fa-github"})
				}
				if v, ok := old["twitter"]; ok && v != "" {
					am.SocialLinks = append(am.SocialLinks, SocialLink{Name: "Twitter", URL: v, Icon: "fa-brands fa-twitter"})
				}
				if v, ok := old["email"]; ok && v != "" {
					am.SocialLinks = append(am.SocialLinks, SocialLink{Name: "邮箱", URL: "mailto:" + v, Icon: "fa-solid fa-envelope"})
				}
			}
		}

		data["AboutMe"] = am
		c.HTML(http.StatusOK, "admin/about.html", data)
	}
}

func AdminAboutPost(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		avatar := c.PostForm("avatar")
		// 优先用上传的文件
		file, err := c.FormFile("avatar_file")
		if err == nil {
			uploadsDir := filepath.Join(getProjectRoot(c), "uploads")
			os.MkdirAll(uploadsDir, 0755)
			tmpPath := filepath.Join(uploadsDir, "tmp_avatar_"+file.Filename)
			if err := c.SaveUploadedFile(file, tmpPath); err == nil {
				filename, _, _ := utils.ProcessUploadedFile(tmpPath, uploadsDir)
				if filename != "" {
					avatar = "/uploads/" + filename
				}
			}
		}
		am := AboutMe{
			Name:     c.PostForm("name"),
			Avatar:   avatar,
			Bio:      c.PostForm("bio"),
			Location: c.PostForm("location"),
		}

		// 解析社交链接动态列表（安全取值，防止数组越界）
		names := c.PostFormArray("social_name")
		urls := c.PostFormArray("social_url")
		icons := c.PostFormArray("social_icon")
		for i := range names {
			sName := names[i]
			sURL := ""
			if i < len(urls) {
				sURL = urls[i]
			}
			sIcon := ""
			if i < len(icons) {
				sIcon = icons[i]
			}
			if sName != "" || sURL != "" {
				am.SocialLinks = append(am.SocialLinks, SocialLink{
					Name: sName,
					URL:  sURL,
					Icon: sIcon,
				})
			}
		}

		jsonBytes, _ := json.Marshal(am)
		execLog(db, "INSERT INTO system_settings (key, value) VALUES ('about_me_json', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", string(jsonBytes))

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
			siteURL = c.Request.URL.Scheme + "://" + c.Request.Host
			if siteURL == "://" {
				siteURL = "http://localhost:8080"
			}
		}

		path, err := utils.GenerateSitemap(db, siteURL, staticDir)
		if err != nil {
			c.String(500, "生成失败: %v", err)
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
		username := c.PostForm("username")
		password := c.PostForm("password")
		role := c.PostForm("role")
		if role == "" {
			role = "editor"
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
		avatarURL := c.PostForm("avatar_url")
		// 优先用上传的文件
		file, fErr := c.FormFile("avatar_file")
		if fErr == nil {
			uploadsDir := filepath.Join(getProjectRoot(c), "uploads")
			os.MkdirAll(uploadsDir, 0755)
			tmpPath := filepath.Join(uploadsDir, "tmp_avatar_"+file.Filename)
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

		rows, _ := db.Query("SELECT id, name, url, icon, parent_id, order_num FROM menus ORDER BY order_num, id")
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
				}
				scanLog(rows.Scan(&m.ID, &m.Name, &m.URL, &m.Icon, &m.ParentID, &m.OrderNum), "adminMenus")
				parentID := int64(0)
				if m.ParentID.Valid {
					parentID = m.ParentID.Int64
				}
				menus = append(menus, gin.H{
					"ID": m.ID, "Name": m.Name, "URL": m.URL, "Icon": m.Icon,
					"ParentID": parentID, "OrderNum": m.OrderNum,
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
		name := c.PostForm("name")
		url := c.PostForm("url")
		icon := c.PostForm("icon")
		orderNum, _ := strconv.Atoi(c.PostForm("order_num"))
		parentID, _ := strconv.ParseInt(c.PostForm("parent_id"), 10, 64)

		execLog(db, "INSERT INTO menus (name, url, icon, parent_id, order_num) VALUES (?,?,?,?,?)",
			name, url, icon, nilIfZero(parentID), orderNum)

		logOperation(db, getAdminUsername(c), c, fmt.Sprintf("新增菜单: %s", name))
		c.Redirect(http.StatusFound, "/admin/menus")
	}
}

func AdminMenuDelete(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		if id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "非法ID"})
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
		title := c.PostForm("title")
		slug := c.PostForm("slug")
		content := c.PostForm("content")
		if _, err := db.Exec("INSERT INTO pages (title, slug, content) VALUES (?, ?, ?)", title, slug, content); err != nil {
			c.String(500, "创建页面失败: %v", err)
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
		if _, err := db.Exec("UPDATE pages SET title=?, slug=?, content=?, updated_at=? WHERE id=?",
			c.PostForm("title"), c.PostForm("slug"), c.PostForm("content"), time.Now(), id); err != nil {
			c.String(500, "更新页面失败: %v", err)
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
			c.JSON(500, gin.H{"ok": false, "msg": err.Error()})
			return
		}
		logOperation(db, getAdminUsername(c), c, "创建备份")
		c.JSON(http.StatusOK, gin.H{"ok": true, "file": filepath.Base(backupPath)})
	}
}

func AdminBackupRestore(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		// 安全检查：防止路径穿越
		if strings.Contains(name, "..") {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "非法路径"})
			return
		}
		projectRoot := getProjectRoot(c)
		backupFile := filepath.Join(projectRoot, "backups", name)

		log.Printf("[Restore] Starting restore from: %s", backupFile)

		// 先关闭数据库连接，避免覆盖正在使用的文件
		db.Close()

		err := utils.RestoreBackup(utils.DefaultBackupConfig(projectRoot), backupFile)
		if err != nil {
			log.Printf("[Restore] Failed: %v", err)
			c.JSON(500, gin.H{"ok": false, "msg": "恢复失败: " + err.Error()})
			return
		}

		log.Printf("[Restore] Success, restarting service...")
		c.JSON(http.StatusOK, gin.H{"ok": true, "msg": "恢复成功，服务将重启"})

		// 恢复成功后重启服务，让新数据库生效
		go func() {
			time.Sleep(500 * time.Millisecond)
			execPath, _ := os.Executable()
			cmd := exec.Command(execPath)
			cmd.Dir = filepath.Dir(execPath)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.Start()
			os.Exit(0)
		}()
	}
}

func AdminBackupDelete(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		// 安全检查：防止路径穿越
		if strings.Contains(name, "..") {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "非法路径"})
			return
		}
		projectRoot := getProjectRoot(c)
		utils.DeleteBackup(filepath.Join(projectRoot, "backups", name))
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// AdminBackupDownload 需要认证的备份文件下载
func AdminBackupDownload(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		// 安全检查：防止路径穿越
		if strings.Contains(name, "..") {
			c.String(http.StatusBadRequest, "非法路径")
			return
		}

		projectRoot := getProjectRoot(c)
		backupPath := filepath.Join(projectRoot, "backups", name)

		if _, err := os.Stat(backupPath); os.IsNotExist(err) {
			c.String(http.StatusNotFound, "文件不存在")
			return
		}

		c.FileAttachment(backupPath, name)
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

func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

// PublishScheduledPosts 发布定时文章
func PublishScheduledPosts(db *sql.DB) {
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
		author := c.PostForm("author")
		if loginUsername == "" {
			if author == "" {
				c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "请输入昵称"})
				return
			}
			if len(author) > 30 {
				c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "昵称最多30个字符"})
				return
			}
		} else {
			author = loginUsername
		}

		targetType := c.PostForm("target_type")
		targetID, _ := strconv.ParseInt(c.PostForm("target_id"), 10, 64)
		content := c.PostForm("content")

		if content == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "内容不能为空"})
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
				tmpPath := filepath.Join(uploadsDir, "tmp_comment_"+fh.Filename)
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

		result, err := db.Exec("INSERT INTO comments (target_type, target_id, author, author_avatar, content, image_url) VALUES (?,?,?,?,?,?)",
			targetType, targetID, author, avatarURL, content, imageURL)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "保存失败"})
			return
		}
		rowsAff, _ := result.RowsAffected()
		c.JSON(http.StatusOK, gin.H{"ok": true, "msg": "评论成功", "rowsAffected": rowsAff, "author": author, "author_avatar": avatarURL, "image_url": imageURL})
	}
}

func GuestbookSubmit(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		author := c.PostForm("author")
		content := c.PostForm("content")

		if author == "" || content == "" {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "昵称和内容不能为空"})
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

// PostLikeHandler 文章点赞
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
		db.Exec("UPDATE posts SET like_count = like_count + 1 WHERE id = ?", id)
		var count int64
		db.QueryRow("SELECT like_count FROM posts WHERE id = ?", id).Scan(&count)
		c.JSON(http.StatusOK, gin.H{"ok": true, "count": count})
	}
}

// PostDislikeHandler 文章拍砖
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
		db.Exec("UPDATE posts SET dislike_count = dislike_count + 1 WHERE id = ?", id)
		var count int64
		db.QueryRow("SELECT dislike_count FROM posts WHERE id = ?", id).Scan(&count)
		c.JSON(http.StatusOK, gin.H{"ok": true, "count": count})
	}
}

// ============ 首次部署引导 ============

// SetupCheckMiddleware 检查是否已完成初始化，未完成则跳转引导页
func SetupCheckMiddleware(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
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
		var count int
		db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count)
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

		// 创管理员
		db.Exec("INSERT INTO users (username, password, role) VALUES (?, ?, 'admin')", username, string(hash))

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

		// 自动登录
		sessionToken := GenerateSession(username)
		setSessionCookie(c, SignCookie(sessionToken), 86400)

		c.Redirect(http.StatusFound, "/admin")
	}
}

// ============ 版本检查 ============

// AdminCheckUpdate 检查 GitHub 最新版本
func AdminCheckUpdate(currentVersion string) gin.HandlerFunc {
	return func(c *gin.Context) {
		client := &http.Client{Timeout: 10 * time.Second}
		req, _ := http.NewRequest("GET", "https://api.github.com/repos/geekou/uniflow/releases/latest", nil)
		req.Header.Set("User-Agent", "UniFlow/"+currentVersion)
		resp, err := client.Do(req)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"ok": true, "current": currentVersion, "latest": "", "hasUpdate": false, "error": "无法连接 GitHub，请稍后重试"})
			return
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		var release struct {
			TagName string `json:"tag_name"`
		}
		json.Unmarshal(body, &release)

		hasUpdate := release.TagName != "" && release.TagName != currentVersion

		c.JSON(http.StatusOK, gin.H{
			"ok":        true,
			"current":   currentVersion,
			"latest":    release.TagName,
			"hasUpdate": hasUpdate,
		})
	}
}
