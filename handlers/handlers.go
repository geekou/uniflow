package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"

	"uniflow/models"
	"uniflow/utils"

	"github.com/gin-gonic/gin"
)

// ============ 通用模板辅助函数 ============

// runeLen 截取字符串的前 n 个 rune（UTF-8 安全）
func runeTruncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

// SocialLink 社交链接
type SocialLink struct {
	Name string `json:"name"` // 显示名称，如 GitHub、微信、B站
	URL  string `json:"url"`  // 链接地址
	Icon string `json:"icon"` // Font Awesome 图标类名，如 fa-brands fa-github
}

// AboutMe 关于我 JSON 结构
type AboutMe struct {
	Name        string       `json:"name"`
	Avatar      string       `json:"avatar"`
	Bio         string       `json:"bio"`
	Location    string       `json:"location"`
	OneWord     string       `json:"one_word"`     // 一言
	SocialLinks []SocialLink `json:"social_links"` // 社交链接列表
}

// SiteStats 站点统计
type SiteStats struct {
	PostCount     int `json:"post_count"`
	CategoryCount int `json:"category_count"`
	TagCount      int `json:"tag_count"`
	CommentCount  int `json:"comment_count"`
	TotalViews    int `json:"total_views"`
}

// IndexData 首页模板数据
type IndexData struct {
	SiteTitle         string
	SiteSubtitle      string
	BannerURL         string
	AboutMe           AboutMe
	Menus             []models.Menu
	Stats             SiteStats
	TopPost           *models.PostWithMeta
	Posts             []models.PostWithMeta
	Categories        []models.Category
	Moments           []models.MomentView
	MomentMediaMap    map[string][]string // moment ID -> []media URL
	Tags              []models.Tag
	HotPosts          []models.Post
	RecentComments    []models.Comment
	MarqueeGuestbooks []models.Guestbook
	Page              int
	TotalPages        int
	PageRange         []int // 分页数字序列, -1 代表省略号
	CurrentCategory   string
	SearchQuery       string
	SiteFoundedAt     string
	SiteNotification  string
	Guestbooks        []models.Guestbook
}

// PageData 自定义页面模板数据
type PageData struct {
	SiteTitle        string
	SiteSubtitle     string
	Menus            []models.Menu
	PageData         models.PageView
	SiteFoundedAt    string
	SiteNotification string
}

// ============ 获取公共数据 ============

func getSiteSettings(db *sql.DB) (map[string]string, error) {
	rows, err := db.Query("SELECT key, value FROM system_settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	settings := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			continue
		}
		settings[k] = v
	}
	return settings, nil
}

func getAboutMe(settings map[string]string) AboutMe {
	am := AboutMe{Name: "HuiNan", Bio: "热爱技术与生活"}
	raw, ok := settings["about_me_json"]
	if !ok || raw == "" {
		return am
	}
	// 尝试解析为单个对象
	if err := json.Unmarshal([]byte(raw), &am); err != nil {
		// 可能是数组格式 [{...}]，取第一个元素
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
			if v, ok := arr[0]["social_links"]; ok {
				if links, ok := v.([]interface{}); ok {
					for _, link := range links {
						if m, ok := link.(map[string]interface{}); ok {
							sl := SocialLink{}
							if v, ok := m["name"]; ok {
								if s, ok := v.(string); ok {
									sl.Name = s
								}
							}
							if v, ok := m["url"]; ok {
								if s, ok := v.(string); ok {
									sl.URL = s
								}
							}
							if v, ok := m["icon"]; ok {
								if s, ok := v.(string); ok {
									sl.Icon = s
								}
							}
							am.SocialLinks = append(am.SocialLinks, sl)
						}
					}
				}
			}
		}
	}
	// 空值保护：不要用空字符串覆盖默认值
	if am.Name == "" {
		am.Name = "HuiNan"
	}
	if am.Bio == "" {
		am.Bio = "热爱技术与生活"
	}
	return am
}

func getMenus(db *sql.DB) ([]models.Menu, error) {
	rows, err := db.Query("SELECT id, name, url, icon, parent_id, order_num FROM menus WHERE parent_id = 0 OR parent_id IS NULL ORDER BY order_num ASC, id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var menus []models.Menu
	for rows.Next() {
		var m models.Menu
		if err := rows.Scan(&m.ID, &m.Name, &m.URL, &m.Icon, &m.ParentID, &m.OrderNum); err != nil {
			continue
		}
		menus = append(menus, m)
	}
	return menus, nil
}

func getStats(db *sql.DB) SiteStats {
	s := SiteStats{}
	db.QueryRow("SELECT COUNT(*) FROM posts WHERE status='published' AND privacy='public'").Scan(&s.PostCount)
	db.QueryRow("SELECT COUNT(*) FROM categories").Scan(&s.CategoryCount)
	db.QueryRow("SELECT COUNT(*) FROM tags").Scan(&s.TagCount)
	db.QueryRow("SELECT COUNT(*) FROM comments").Scan(&s.CommentCount)
	db.QueryRow("SELECT COALESCE(SUM(views),0) FROM posts").Scan(&s.TotalViews)
	return s
}

// getTopPost 获取置顶文章（Banner 用）
func getTopPost(db *sql.DB) *models.PostWithMeta {
	var tp models.PostWithMeta
	err := db.QueryRow(
		"SELECT p.id, p.title, p.content, p.thumb_url, p.author, p.views, p.category_id, p.is_top, p.privacy, p.status, p.publish_at, p.created_at, p.updated_at, COALESCE(c.name,''), COALESCE(c.slug,''), (SELECT COUNT(*) FROM comments WHERE target_type='post' AND target_id=p.id) as comment_count FROM posts p LEFT JOIN categories c ON p.category_id = c.id WHERE p.is_top=1 AND p.privacy='public' AND p.status='published' ORDER BY p.created_at DESC LIMIT 1",
	).Scan(
		&tp.ID, &tp.Title, &tp.Content, &tp.ThumbURL,
		&tp.Author, &tp.Views, &tp.CategoryID, &tp.IsTop,
		&tp.Privacy, &tp.Status, &tp.PublishAt,
		&tp.CreatedAt, &tp.UpdatedAt, &tp.CategoryName, &tp.CategorySlug, &tp.CommentCount,
	)
	if err != nil {
		return nil
	}
	return &tp
}

// buildInPlaceholders 构建 IN 子句的占位符和参数列表
// ids: int64 切片 → 返回 ("?,?,?", []interface{}{1,2,3})
func buildInPlaceholders(ids []int64) (string, []interface{}) {
	placeholders := strings.Repeat("?,", len(ids)-1) + "?"
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return placeholders, args
}

// scanLog 记录 Scan 错误（不中断流程，仅记录日志）
func scanLog(err error, context string) {
	if err != nil {
		log.Printf("[Scan] %s: %v", context, err)
	}
}

// ============ 公共数据查询 ============

func getHotPosts(db *sql.DB) []models.Post {
	rows, _ := db.Query("SELECT id, title, content, thumb_url, author, views, category_id, is_top, privacy, status, publish_at, created_at, updated_at FROM posts WHERE status='published' AND privacy='public' ORDER BY views DESC LIMIT 5")
	var posts []models.Post
	if rows != nil {
		for rows.Next() {
			var p models.Post
			scanLog(rows.Scan(&p.ID, &p.Title, &p.Content, &p.ThumbURL, &p.Author, &p.Views, &p.CategoryID, &p.IsTop, &p.Privacy, &p.Status, &p.PublishAt, &p.CreatedAt, &p.UpdatedAt), "hotPosts")
			posts = append(posts, p)
		}
		rows.Close()
	}
	return posts
}

func getTags(db *sql.DB) []models.Tag {
	rows, _ := db.Query("SELECT t.id, t.name, t.created_at, COUNT(pt.post_id) as post_count FROM tags t LEFT JOIN post_tags pt ON t.id = pt.tag_id GROUP BY t.id ORDER BY post_count DESC LIMIT 20")
	var tags []models.Tag
	if rows != nil {
		for rows.Next() {
			var t models.Tag
			scanLog(rows.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.PostCount), "tags")
			tags = append(tags, t)
		}
		rows.Close()
	}
	return tags
}

func getRecentComments(db *sql.DB) []models.Comment {
	rows, _ := db.Query("SELECT id, target_type, target_id, author, author_avatar, content, image_url, created_at, parent_id FROM comments ORDER BY created_at DESC LIMIT 5")
	var comments []models.Comment
	if rows != nil {
		for rows.Next() {
			var c models.Comment
			scanLog(rows.Scan(&c.ID, &c.TargetType, &c.TargetID, &c.Author, &c.AuthorAvatar, &c.Content, &c.ImageURL, &c.CreatedAt, &c.ParentID), "recentComments")
			comments = append(comments, c)
		}
		rows.Close()
	}
	return comments
}

// ============ 首页处理器 ============

func IndexHandler(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		settings, _ := getSiteSettings(db)
		menus, _ := getMenus(db)

		// 分页参数
		page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
		if page < 1 {
			page = 1
		}
		pageSize := 10
		offset := (page - 1) * pageSize

		// 分类筛选
		categorySlug := c.Query("category")

		// 构建文章查询
		where := "WHERE p.status='published' AND p.privacy='public'"
		args := []interface{}{}
		if categorySlug != "" {
			where += " AND c.slug = ?"
			args = append(args, categorySlug)
		}

		// 查询总数
		var total int
		db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM posts p LEFT JOIN categories c ON p.category_id = c.id %s", where), args...).Scan(&total)
		totalPages := (total + pageSize - 1) / pageSize
		if totalPages < 1 {
			totalPages = 1
		}
		if page > totalPages {
			page = totalPages
		}
		offset = (page - 1) * pageSize

		// 置顶文章
		topPost := getTopPost(db)

		// 文章列表（附带分类名和评论数）
		query := fmt.Sprintf(`
			SELECT p.id, p.title, p.content, p.thumb_url, p.author, p.views, p.category_id,
			       p.is_top, p.privacy, p.status, p.publish_at, p.created_at, p.updated_at,
			       COALESCE(c.name, '') as category_name, COALESCE(c.slug,'') as category_slug,
			       (SELECT COUNT(*) FROM comments WHERE target_type='post' AND target_id=p.id) as comment_count
			FROM posts p
			LEFT JOIN categories c ON p.category_id = c.id
			%s
			ORDER BY p.is_top DESC, p.created_at DESC
			LIMIT ? OFFSET ?
		`, where)
		allArgs := append(args, pageSize, offset)

		rows, err := db.Query(query, allArgs...)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "index.html", gin.H{"error": "查询失败"})
			return
		}

		var posts []models.PostWithMeta
		for rows.Next() {
			var p models.PostWithMeta
			scanLog(rows.Scan(
				&p.ID, &p.Title, &p.Content, &p.ThumbURL,
				&p.Author, &p.Views, &p.CategoryID, &p.IsTop,
				&p.Privacy, &p.Status, &p.PublishAt,
				&p.CreatedAt, &p.UpdatedAt, &p.CategoryName, &p.CategorySlug, &p.CommentCount,
			), "indexPosts")
			// 截取摘要（前200字）
			p.Content = runeTruncate(p.Content, 200)
			posts = append(posts, p)
		}
		rows.Close() // 立即释放连接，避免 SetMaxOpenConns(1) 死锁

		// 分类列表（筛选栏）
		catRows, _ := db.Query("SELECT id, name, slug, created_at FROM categories ORDER BY name ASC")
		var categories []models.Category
		if catRows != nil {
			for catRows.Next() {
				var cat models.Category
				scanLog(catRows.Scan(&cat.ID, &cat.Name, &cat.Slug, &cat.CreatedAt), "indexCategories")
				categories = append(categories, cat)
			}
			catRows.Close()
		}

		// 瞬间流（最近10条）
		momentRows, _ := db.Query("SELECT id, content, media_urls, likes, created_at FROM moments ORDER BY created_at DESC LIMIT 10")
		var moments []models.MomentView
		mediaMap := make(map[string][]string)
		if momentRows != nil {
			for momentRows.Next() {
				var m models.MomentView
				scanLog(momentRows.Scan(&m.ID, &m.Content, &m.MediaURLs, &m.Likes, &m.CreatedAt), "indexMoments")
				m.IDStr = strconv.FormatInt(m.ID, 10)
				if m.MediaURLs != "" {
					mediaMap[m.IDStr] = strings.Split(m.MediaURLs, ",")
				}
				moments = append(moments, m)
			}
			momentRows.Close()
		}

		hotPosts := getHotPosts(db)
		tags := getTags(db)
		recentComments := getRecentComments(db)
		pageRange := buildPageRange(page, totalPages)

		data := IndexData{
			SiteTitle:        settings["site_title"],
			SiteSubtitle:     settings["site_subtitle"],
			SiteFoundedAt:    settings["site_founded_at"],
			SiteNotification: settings["site_notification"],
			BannerURL:        settings["banner_url"],
			AboutMe:          getAboutMe(settings),
			Menus:            menus,
			Stats:            getStats(db),
			TopPost:          topPost,
			Posts:            posts,
			Categories:       categories,
			Moments:          moments,
			MomentMediaMap:   mediaMap,
			Tags:             tags,
			HotPosts:         hotPosts,
			RecentComments:   recentComments,
			Page:             page,
			TotalPages:       totalPages,
			PageRange:        pageRange,
			CurrentCategory:  categorySlug,
		}

		c.HTML(http.StatusOK, "index.html", data)
	}
}

// ============ 文章列表页处理器 ============

func PostsListHandler(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		settings, _ := getSiteSettings(db)
		menus, _ := getMenus(db)

		// 置顶文章（用于 Banner）
		topPost := getTopPost(db)

		page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
		if page < 1 {
			page = 1
		}
		pageSize := 10
		offset := (page - 1) * pageSize

		categorySlug := c.Query("category")
		where := "WHERE p.status='published' AND p.privacy='public'"
		args := []interface{}{}
		if categorySlug != "" {
			where += " AND c.slug = ?"
			args = append(args, categorySlug)
		}

		var total int
		db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM posts p LEFT JOIN categories c ON p.category_id = c.id %s", where), args...).Scan(&total)
		totalPages := (total + pageSize - 1) / pageSize
		if totalPages < 1 {
			totalPages = 1
		}
		if page > totalPages {
			page = totalPages
		}
		offset = (page - 1) * pageSize

		query := fmt.Sprintf(`
			SELECT p.id, p.title, p.content, p.thumb_url, p.author, p.views, p.category_id,
			       p.is_top, p.privacy, p.status, p.publish_at, p.created_at, p.updated_at,
			       COALESCE(c.name, '') as category_name, COALESCE(c.slug,'') as category_slug,
			       (SELECT COUNT(*) FROM comments WHERE target_type='post' AND target_id=p.id) as comment_count
			FROM posts p
			LEFT JOIN categories c ON p.category_id = c.id
			%s
			ORDER BY p.is_top DESC, p.created_at DESC
			LIMIT ? OFFSET ?
		`, where)
		allArgs := append(args, pageSize, offset)

		rows, err := db.Query(query, allArgs...)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "posts.html", gin.H{"error": "查询失败"})
			return
		}

		var posts []models.PostWithMeta
		for rows.Next() {
			var p models.PostWithMeta
			scanLog(rows.Scan(
				&p.ID, &p.Title, &p.Content, &p.ThumbURL,
				&p.Author, &p.Views, &p.CategoryID, &p.IsTop,
				&p.Privacy, &p.Status, &p.PublishAt,
				&p.CreatedAt, &p.UpdatedAt, &p.CategoryName, &p.CategorySlug, &p.CommentCount,
			), "postsList")
			if len(p.Content) > 200 {
				p.Content = runeTruncate(p.Content, 200)
			}
			posts = append(posts, p)
		}
		rows.Close() // 立即释放连接

		// 分类列表
		catRows, _ := db.Query("SELECT id, name, slug, created_at FROM categories ORDER BY name ASC")
		var categories []models.Category
		if catRows != nil {
			for catRows.Next() {
				var cat models.Category
				scanLog(catRows.Scan(&cat.ID, &cat.Name, &cat.Slug, &cat.CreatedAt), "postsCategories")
				categories = append(categories, cat)
			}
			catRows.Close()
		}

		hotPosts := getHotPosts(db)
		tags := getTags(db)
		recentComments := getRecentComments(db)
		pageRange := buildPageRange(page, totalPages)

		data := IndexData{
			SiteTitle:        settings["site_title"],
			SiteSubtitle:     settings["site_subtitle"],
			SiteFoundedAt:    settings["site_founded_at"],
			SiteNotification: settings["site_notification"],
			BannerURL:        settings["banner_url"],
			TopPost:          topPost,
			AboutMe:          getAboutMe(settings),
			Menus:            menus,
			Stats:            getStats(db),
			Posts:            posts,
			Categories:       categories,
			MomentMediaMap:   map[string][]string{},
			Tags:             tags,
			HotPosts:         hotPosts,
			RecentComments:   recentComments,
			Page:             page,
			TotalPages:       totalPages,
			PageRange:        pageRange,
			CurrentCategory:  categorySlug,
			SearchQuery:      c.Query("q"),
		}
		c.HTML(http.StatusOK, "posts.html", data)
	}
}

// ============ 瞬间列表页处理器 ============

func MomentsListHandler(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		settings, _ := getSiteSettings(db)
		menus, _ := getMenus(db)

		// 置顶文章（用于 Banner）
		topPost := getTopPost(db)

		page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
		if page < 1 {
			page = 1
		}
		pageSize := 10
		offset := (page - 1) * pageSize

		var total int
		db.QueryRow("SELECT COUNT(*) FROM moments").Scan(&total)
		totalPages := (total + pageSize - 1) / pageSize
		if totalPages < 1 {
			totalPages = 1
		}
		if page > totalPages {
			page = totalPages
		}
		offset = (page - 1) * pageSize

		momentRows, err := db.Query("SELECT id, content, media_urls, likes, created_at FROM moments ORDER BY created_at DESC LIMIT ? OFFSET ?", pageSize, offset)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "moments.html", gin.H{"error": "查询失败"})
			return
		}
		defer momentRows.Close()

		var moments []models.MomentView
		mediaMap := make(map[string][]string)
		for momentRows.Next() {
			var m models.MomentView
			scanLog(momentRows.Scan(&m.ID, &m.Content, &m.MediaURLs, &m.Likes, &m.CreatedAt), "momentsList")
			m.IDStr = strconv.FormatInt(m.ID, 10)
			if m.MediaURLs != "" {
				mediaMap[m.IDStr] = strings.Split(m.MediaURLs, ",")
			}
			moments = append(moments, m)
		}
		momentRows.Close()

		// 批量加载所有当前页瞬间的评论（避免 SetMaxOpenConns=1 死锁）
		if len(moments) > 0 {
			momentIDs := make([]int64, len(moments))
			for i, m := range moments {
				momentIDs[i] = m.ID
			}
			commentMap := make(map[int64][]models.Comment)
			ph, phArgs := buildInPlaceholders(momentIDs)
			commentRows, err := db.Query("SELECT id, target_type, target_id, author, author_avatar, content, image_url, created_at, parent_id FROM comments WHERE target_type='moment' AND target_id IN ("+ph+") ORDER BY created_at ASC", phArgs...)
			if err == nil {
				for commentRows.Next() {
					var cm models.Comment
					scanLog(commentRows.Scan(&cm.ID, &cm.TargetType, &cm.TargetID, &cm.Author, &cm.AuthorAvatar, &cm.Content, &cm.ImageURL, &cm.CreatedAt, &cm.ParentID), "momentComments")
					commentMap[cm.TargetID] = append(commentMap[cm.TargetID], cm)
				}
				commentRows.Close()
			}
			// 填充 ReplyTo：根据 parent_id 查找父评论作者
			parentIDs := make(map[int64]bool)
			for _, ms := range commentMap {
				for _, cm := range ms {
					if cm.ParentID > 0 {
						parentIDs[cm.ParentID] = true
					}
				}
			}
			if len(parentIDs) > 0 {
				pidSlice := make([]int64, 0, len(parentIDs))
				for pid := range parentIDs {
					pidSlice = append(pidSlice, pid)
				}
				parentAuthors := make(map[int64]string)
				pPh, pArgs := buildInPlaceholders(pidSlice)
				pRows, pErr := db.Query("SELECT id, author FROM comments WHERE id IN ("+pPh+")", pArgs...)
				if pErr == nil {
					for pRows.Next() {
						var pid int64
						var pauthor string
						scanLog(pRows.Scan(&pid, &pauthor), "momentParentAuthors")
						parentAuthors[pid] = pauthor
					}
					pRows.Close()
				}
				for i, ms := range commentMap {
					for j := range ms {
						if ms[j].ParentID > 0 {
							commentMap[i][j].ReplyTo = parentAuthors[ms[j].ParentID]
						}
					}
				}
			}
			for i := range moments {
				moments[i].Comments = commentMap[moments[i].ID]
				moments[i].CommentCount = len(moments[i].Comments)
			}
		}

		hotPosts := getHotPosts(db)
		tags := getTags(db)
		recentComments := getRecentComments(db)
		pageRange := buildPageRange(page, totalPages)

		data := IndexData{
			SiteTitle:        settings["site_title"],
			SiteSubtitle:     settings["site_subtitle"],
			SiteFoundedAt:    settings["site_founded_at"],
			SiteNotification: settings["site_notification"],
			BannerURL:        settings["banner_url"],
			AboutMe:          getAboutMe(settings),
			Menus:            menus,
			Stats:            getStats(db),
			TopPost:          topPost,
			Moments:          moments,
			MomentMediaMap:   mediaMap,
			Tags:             tags,
			HotPosts:         hotPosts,
			RecentComments:   recentComments,
			Page:             page,
			TotalPages:       totalPages,
			PageRange:        pageRange,
		}
		c.HTML(http.StatusOK, "moments.html", data)
	}
}

// PostDetailData 文章详情页模板数据
type PostDetailData struct {
	SiteTitle        string
	SiteSubtitle     string
	BannerURL        string
	AboutMe          AboutMe
	Menus            []models.Menu
	Stats            SiteStats
	TopPost          *models.PostWithMeta
	Post             models.PostWithMeta
	Tags             []models.Tag
	HotPosts         []models.Post
	RecentComments   []models.Comment
	RelatedPosts     []models.PostWithMeta
	PostComments     []models.Comment
	PrevPost         *models.PostWithMeta
	NextPost         *models.PostWithMeta
	SiteFoundedAt    string
	SiteNotification string
	WechatQR         string
	AlipayQR         string
	ActiveMenu       string
}

// ============ 文章详情处理器 ============

func PostDetailHandler(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		idStr := c.Param("id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			c.HTML(http.StatusNotFound, "post_detail.html", PostDetailData{
				SiteTitle: "UniFlow",
				Menus:     []models.Menu{},
			})
			return
		}

		settings, _ := getSiteSettings(db)
		menus, _ := getMenus(db)

		// 置顶文章（用于 Banner）
		topPost := getTopPost(db)

		// 查询文章详情
		var post models.PostWithMeta
		err = db.QueryRow(`
			SELECT p.id, p.title, p.content, p.thumb_url, p.author, p.views, p.category_id,
			       p.is_top, p.privacy, p.status, p.publish_at, p.created_at, p.updated_at,
			       COALESCE(c.name, '') as category_name, COALESCE(c.slug, '') as category_slug,
			       COALESCE(p.like_count, 0), COALESCE(p.dislike_count, 0),
			       (SELECT COUNT(*) FROM comments WHERE target_type='post' AND target_id=p.id) as comment_count
			FROM posts p
			LEFT JOIN categories c ON p.category_id = c.id
			WHERE p.id = ? AND p.status='published' AND p.privacy='public'
		`, id).Scan(
			&post.ID, &post.Title, &post.Content, &post.ThumbURL,
			&post.Author, &post.Views, &post.CategoryID, &post.IsTop,
			&post.Privacy, &post.Status, &post.PublishAt,
			&post.CreatedAt, &post.UpdatedAt, &post.CategoryName, &post.CategorySlug,
			&post.LikeCount, &post.DislikeCount, &post.CommentCount,
		)

		if err != nil {
			c.HTML(http.StatusNotFound, "post_detail.html", PostDetailData{
				SiteTitle:        settings["site_title"],
				SiteSubtitle:     settings["site_subtitle"],
				SiteFoundedAt:    settings["site_founded_at"],
				SiteNotification: settings["site_notification"],
				BannerURL:        settings["banner_url"],
				AboutMe:          getAboutMe(settings),
				Menus:            menus,
				Stats:            getStats(db),
				TopPost:          topPost,
				WechatQR:         settings["wechat_qr"],
				AlipayQR:         settings["alipay_qr"],
			})
			return
		}

		// 浏览量 +1
		db.Exec("UPDATE posts SET views = views + 1 WHERE id = ?", id)
		post.Views++

		// 查询文章标签
		var tags []models.Tag
		tagRows, _ := db.Query(`
			SELECT t.id, t.name, t.created_at, 0 as post_count
			FROM tags t
			INNER JOIN post_tags pt ON t.id = pt.tag_id
			WHERE pt.post_id = ?
		`, id)
		if tagRows != nil {
			for tagRows.Next() {
				var t models.Tag
				scanLog(tagRows.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.PostCount), "postTags")
				tags = append(tags, t)
			}
			tagRows.Close()
		}

		// 热门文章（Top 5）
		hotRows, _ := db.Query("SELECT id, title, content, thumb_url, author, views, category_id, is_top, privacy, status, publish_at, created_at, updated_at FROM posts WHERE status='published' AND privacy='public' AND id != ? ORDER BY views DESC LIMIT 5", id)
		var hotPosts []models.Post
		if hotRows != nil {
			for hotRows.Next() {
				var p models.Post
				scanLog(hotRows.Scan(&p.ID, &p.Title, &p.Content, &p.ThumbURL, &p.Author, &p.Views, &p.CategoryID, &p.IsTop, &p.Privacy, &p.Status, &p.PublishAt, &p.CreatedAt, &p.UpdatedAt), "hotPosts")
				hotPosts = append(hotPosts, p)
			}
			hotRows.Close()
		}

		// 相关文章（同分类，排除当前文章，Top 5）
		var relatedPosts []models.PostWithMeta
		if post.CategoryID != nil && *post.CategoryID > 0 {
			relRows, _ := db.Query(`
				SELECT p.id, p.title, p.content, p.thumb_url, p.author, p.views, p.category_id,
				       p.is_top, p.privacy, p.status, p.publish_at, p.created_at, p.updated_at,
				       COALESCE(c.name, '') as category_name, COALESCE(c.slug,'') as category_slug,
				       (SELECT COUNT(*) FROM comments WHERE target_type='post' AND target_id=p.id) as comment_count
				FROM posts p
				LEFT JOIN categories c ON p.category_id = c.id
				WHERE p.category_id = ? AND p.id != ? AND p.status='published' AND p.privacy='public'
				ORDER BY p.created_at DESC LIMIT 5
			`, post.CategoryID, id)
			if relRows != nil {
				for relRows.Next() {
					var rp models.PostWithMeta
					scanLog(relRows.Scan(
						&rp.ID, &rp.Title, &rp.Content, &rp.ThumbURL,
						&rp.Author, &rp.Views, &rp.CategoryID, &rp.IsTop,
						&rp.Privacy, &rp.Status, &rp.PublishAt,
						&rp.CreatedAt, &rp.UpdatedAt, &rp.CategoryName, &rp.CategorySlug, &rp.CommentCount,
					), "relatedPosts")
					if len(rp.Content) > 100 {
						rp.Content = runeTruncate(rp.Content, 100)
					}
					relatedPosts = append(relatedPosts, rp)
				}
				relRows.Close()
			}
		}

		// 最新评论
		commentRows, _ := db.Query("SELECT id, target_type, target_id, author, author_avatar, content, image_url, created_at, parent_id FROM comments ORDER BY created_at DESC LIMIT 5")
		var recentComments []models.Comment
		if commentRows != nil {
			for commentRows.Next() {
				var cm models.Comment
				scanLog(commentRows.Scan(&cm.ID, &cm.TargetType, &cm.TargetID, &cm.Author, &cm.AuthorAvatar, &cm.Content, &cm.ImageURL, &cm.CreatedAt, &cm.ParentID), "recentComments")
				recentComments = append(recentComments, cm)
			}
			commentRows.Close()
		}

		// 文章评论
		var postComments []models.Comment
		pcRows, _ := db.Query("SELECT id, target_type, target_id, author, author_avatar, content, image_url, created_at, parent_id FROM comments WHERE target_type='post' AND target_id = ? ORDER BY created_at ASC", id)
		if pcRows != nil {
			for pcRows.Next() {
				var cm models.Comment
				scanLog(pcRows.Scan(&cm.ID, &cm.TargetType, &cm.TargetID, &cm.Author, &cm.AuthorAvatar, &cm.Content, &cm.ImageURL, &cm.CreatedAt, &cm.ParentID), "postComments")
				postComments = append(postComments, cm)
			}
			pcRows.Close()
		}

		// 填充文章评论的 ReplyTo
		parentIDs := make(map[int64]bool)
		for _, cm := range postComments {
			if cm.ParentID > 0 {
				parentIDs[cm.ParentID] = true
			}
		}
		if len(parentIDs) > 0 {
			pidSlice := make([]int64, 0, len(parentIDs))
			for pid := range parentIDs {
				pidSlice = append(pidSlice, pid)
			}
			parentAuthors := make(map[int64]string)
			pPh, pArgs := buildInPlaceholders(pidSlice)
			pRows, pErr := db.Query("SELECT id, author FROM comments WHERE id IN ("+pPh+")", pArgs...)
			if pErr == nil {
				for pRows.Next() {
					var pid int64
					var pauthor string
					scanLog(pRows.Scan(&pid, &pauthor), "postParentAuthors")
					parentAuthors[pid] = pauthor
				}
				pRows.Close()
			}
			for i := range postComments {
				if postComments[i].ParentID > 0 {
					postComments[i].ReplyTo = parentAuthors[postComments[i].ParentID]
				}
			}
		}

		// 上下篇导航
		var prevPost, nextPost *models.PostWithMeta
		prevRow := db.QueryRow(`
			SELECT p.id, p.title FROM posts p
			WHERE p.id < ? AND p.status='published' AND p.privacy='public'
			ORDER BY p.id DESC LIMIT 1
		`, id)
		var pp models.PostWithMeta
		if err := prevRow.Scan(&pp.ID, &pp.Title); err == nil {
			prevPost = &pp
		}
		nextRow := db.QueryRow(`
			SELECT p.id, p.title FROM posts p
			WHERE p.id > ? AND p.status='published' AND p.privacy='public'
			ORDER BY p.id ASC LIMIT 1
		`, id)
		var np models.PostWithMeta
		if err := nextRow.Scan(&np.ID, &np.Title); err == nil {
			nextPost = &np
		}

		c.HTML(http.StatusOK, "post_detail.html", PostDetailData{
			SiteTitle:        settings["site_title"],
			SiteSubtitle:     settings["site_subtitle"],
			SiteFoundedAt:    settings["site_founded_at"],
			SiteNotification: settings["site_notification"],
			BannerURL:        settings["banner_url"],
			AboutMe:          getAboutMe(settings),
			Menus:            menus,
			Stats:            getStats(db),
			TopPost:          topPost,
			Post:             post,
			Tags:             tags,
			HotPosts:         hotPosts,
			RecentComments:   recentComments,
			RelatedPosts:     relatedPosts,
			PostComments:     postComments,
			PrevPost:         prevPost,
			NextPost:         nextPost,
			WechatQR:         settings["wechat_qr"],
			AlipayQR:         settings["alipay_qr"],
			ActiveMenu:       "/posts",
		})
	}
}

// ============ 自定义页面处理器 ============

func CustomPageHandler(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		slug := c.Param("slug")
		settings, _ := getSiteSettings(db)
		menus, _ := getMenus(db)

		var pv models.PageView
		err := db.QueryRow(
			"SELECT id, title, slug, content, created_at FROM pages WHERE slug = ?", slug,
		).Scan(&pv.ID, &pv.Title, &pv.Slug, &pv.HTMLContent, &pv.CreatedAt)

		if err != nil {
			c.HTML(http.StatusNotFound, "page.html", PageData{
				SiteTitle:        settings["site_title"],
				SiteSubtitle:     settings["site_subtitle"],
				SiteFoundedAt:    settings["site_founded_at"],
				SiteNotification: settings["site_notification"],
				Menus:            menus,
				PageData: models.PageView{
					Title:       "页面未找到",
					HTMLContent: template.HTML(`<div class="text-center py-16"><i class="fa-regular fa-face-frown text-5xl text-gray-300 mb-4 block"></i><p class="text-gray-400">你访问的页面不存在或已被删除。</p><a href="/" class="inline-block mt-4 text-indigo-500 hover:underline">返回首页</a></div>`),
				},
			})
			return
		}

	// 统一净化 HTML 内容，防止 XSS
	content := template.HTML(utils.SanitizeHTML(string(pv.HTMLContent)))
	pv.HTMLContent = content

		c.HTML(http.StatusOK, "page.html", PageData{
			SiteTitle:        settings["site_title"],
			SiteSubtitle:     settings["site_subtitle"],
			SiteFoundedAt:    settings["site_founded_at"],
			SiteNotification: settings["site_notification"],
			Menus:            menus,
			PageData:         pv,
		})
	}
}

// ============ 瞬间点赞 API ============

func MomentLikeHandler(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		idStr := c.Param("id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "msg": "无效的 ID"})
			return
		}

		// 点赞数 +1
		result, err := db.Exec("UPDATE moments SET likes = likes + 1 WHERE id = ?", id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "操作失败"})
			return
		}

		rowsAffected, _ := result.RowsAffected()
		if rowsAffected == 0 {
			c.JSON(http.StatusNotFound, gin.H{"ok": false, "msg": "瞬间不存在"})
			return
		}

		// 返回最新点赞数
		var likes int
		db.QueryRow("SELECT likes FROM moments WHERE id = ?", id).Scan(&likes)

		c.JSON(http.StatusOK, gin.H{"ok": true, "likes": likes})
	}
}

// ============ 搜索页处理器 ============

func SearchHandler(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		settings, _ := getSiteSettings(db)
		menus, _ := getMenus(db)
		query := strings.TrimSpace(c.Query("q"))

		var posts []models.PostWithMeta
		if query != "" {
			likeQuery := "%" + query + "%"
			rows, err := db.Query(`
				SELECT p.id, p.title, p.content, p.thumb_url, p.author, p.views, p.category_id,
				       p.is_top, p.privacy, p.status, p.publish_at, p.created_at, p.updated_at,
				       COALESCE(c.name, '') as category_name, COALESCE(c.slug,'') as category_slug,
				       (SELECT COUNT(*) FROM comments WHERE target_type='post' AND target_id=p.id) as comment_count
				FROM posts p
				LEFT JOIN categories c ON p.category_id = c.id
				WHERE p.status='published' AND p.privacy='public'
				  AND (p.title LIKE ? OR p.content LIKE ?)
				ORDER BY p.created_at DESC
				LIMIT 20
			`, likeQuery, likeQuery)
			if err == nil && rows != nil {
				for rows.Next() {
					var p models.PostWithMeta
					scanLog(rows.Scan(
						&p.ID, &p.Title, &p.Content, &p.ThumbURL,
						&p.Author, &p.Views, &p.CategoryID, &p.IsTop,
						&p.Privacy, &p.Status, &p.PublishAt,
						&p.CreatedAt, &p.UpdatedAt, &p.CategoryName, &p.CategorySlug, &p.CommentCount,
					), "searchResults")
					p.Content = runeTruncate(p.Content, 200)
					posts = append(posts, p)
				}
				rows.Close()
			}
		}

		hotPosts := getHotPosts(db)
		tags := getTags(db)
		recentComments := getRecentComments(db)

		data := IndexData{
			SiteTitle:        settings["site_title"],
			SiteSubtitle:     settings["site_subtitle"],
			SiteFoundedAt:    settings["site_founded_at"],
			SiteNotification: settings["site_notification"],
			AboutMe:          getAboutMe(settings),
			Menus:            menus,
			Stats:            getStats(db),
			Posts:            posts,
			MomentMediaMap:   map[string][]string{},
			Tags:             tags,
			HotPosts:         hotPosts,
			RecentComments:   recentComments,
			SearchQuery:      query,
			Page:             1,
			TotalPages:       1,
			PageRange:        []int{1},
		}
		c.HTML(http.StatusOK, "posts.html", data)
	}
}

// ============ 留言板处理器 ============

func GuestbookHandler(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		settings, _ := getSiteSettings(db)
		menus, _ := getMenus(db)

		// 置顶文章（用于 Banner）
		topPost := getTopPost(db)

		page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
		if page < 1 {
			page = 1
		}
		pageSize := 20
		offset := (page - 1) * pageSize

		var total int
		db.QueryRow("SELECT COUNT(*) FROM guestbook").Scan(&total)
		totalPages := (total + pageSize - 1) / pageSize
		if totalPages < 1 {
			totalPages = 1
		}
		if page > totalPages {
			page = totalPages
		}
		offset = (page - 1) * pageSize

		rows, err := db.Query("SELECT id, author, content, image_url, ip, admin_reply, replied_at, created_at FROM guestbook ORDER BY created_at DESC LIMIT ? OFFSET ?", pageSize, offset)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "guestbook.html", gin.H{"error": "查询失败"})
			return
		}
		defer rows.Close()

		var entries []models.Guestbook
		for rows.Next() {
			var g models.Guestbook
			scanLog(rows.Scan(&g.ID, &g.Author, &g.Content, &g.ImageURL, &g.IP, &g.AdminReply, &g.RepliedAt, &g.CreatedAt), "guestbook")
			entries = append(entries, g)
		}

		marqueeRows, err := db.Query("SELECT id, author, content, image_url, ip, admin_reply, replied_at, created_at FROM guestbook ORDER BY created_at DESC LIMIT 20")
		if err != nil {
			log.Printf("[guestbookMarquee] error: %v", err)
		}
		var marqueeEntries []models.Guestbook
		if marqueeRows != nil {
			defer marqueeRows.Close()
			for marqueeRows.Next() {
				var g models.Guestbook
				scanLog(marqueeRows.Scan(&g.ID, &g.Author, &g.Content, &g.ImageURL, &g.IP, &g.AdminReply, &g.RepliedAt, &g.CreatedAt), "guestbookMarquee")
				marqueeEntries = append(marqueeEntries, g)
			}
		}

		hotPosts := getHotPosts(db)
		tags := getTags(db)
		recentComments := getRecentComments(db)
		pageRange := buildPageRange(page, totalPages)

		data := IndexData{
			SiteTitle:         settings["site_title"],
			SiteSubtitle:      settings["site_subtitle"],
			SiteFoundedAt:     settings["site_founded_at"],
			SiteNotification:  settings["site_notification"],
			BannerURL:         settings["banner_url"],
			AboutMe:           getAboutMe(settings),
			Menus:             menus,
			Stats:             getStats(db),
			TopPost:           topPost,
			Tags:              tags,
			HotPosts:          hotPosts,
			RecentComments:    recentComments,
			MarqueeGuestbooks: marqueeEntries,
			Guestbooks:        entries,
			Page:              page,
			TotalPages:        totalPages,
			PageRange:         pageRange,
		}
		c.HTML(http.StatusOK, "guestbook.html", data)
	}
}

// ============ 辅助函数 ============

// buildPageRange 构建分页数字序列，超过范围用 -1 表示省略号
func buildPageRange(page, totalPages int) []int {
	if totalPages <= 7 {
		result := make([]int, totalPages)
		for i := 0; i < totalPages; i++ {
			result[i] = i + 1
		}
		return result
	}

	var result []int
	result = append(result, 1)

	if page > 3 {
		result = append(result, -1) // 省略号
	}

	start := page - 1
	if start < 2 {
		start = 2
	}
	end := page + 1
	if end > totalPages-1 {
		end = totalPages - 1
	}

	for i := start; i <= end; i++ {
		result = append(result, i)
	}

	if page < totalPages-2 {
		result = append(result, -1)
	}

	result = append(result, totalPages)
	return result
}
