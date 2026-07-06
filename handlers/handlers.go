package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"uniflow/models"
	"uniflow/utils"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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
	Name     string `json:"name"`      // 显示名称，如 GitHub、微信、B站
	URL      string `json:"url"`       // 链接地址
	Icon     string `json:"icon"`      // Font Awesome 图标类名，如 fa-brands fa-github
	Platform string `json:"platform"`  // 平台标识，如 github、wechat、bilibili
	Badge    string `json:"badge"`     // 无法使用品牌图标时的文字兜底
	Color    string `json:"color"`     // 前台 hover/text 颜色类
	Sort     int    `json:"sort"`      // 展示顺序，数字越小越靠前
}

var socialPlatformPresets = map[string]SocialLink{
	"github":     {Name: "GitHub", Platform: "github", Icon: "fa-brands fa-github", Badge: "GH", Color: "hover:text-gray-900"},
	"twitter":    {Name: "Twitter", Platform: "twitter", Icon: "fa-brands fa-twitter", Badge: "X", Color: "hover:text-sky-500"},
	"email":      {Name: "邮箱", Platform: "email", Icon: "fa-solid fa-envelope", Badge: "@", Color: "hover:text-indigo-600"},
	"qq":         {Name: "QQ", Platform: "qq", Icon: "fa-brands fa-qq", Badge: "QQ", Color: "hover:text-sky-500"},
	"wechat":     {Name: "微信", Platform: "wechat", Icon: "fa-brands fa-weixin", Badge: "微", Color: "hover:text-green-500"},
	"douyin":     {Name: "抖音", Platform: "douyin", Icon: "fa-brands fa-tiktok", Badge: "抖", Color: "hover:text-gray-900"},
	"bilibili":   {Name: "哔哩哔哩", Platform: "bilibili", Icon: "", Badge: "B站", Color: "hover:text-sky-500"},
	"xiaohongshu": {Name: "小红书", Platform: "xiaohongshu", Icon: "", Badge: "红", Color: "hover:text-rose-500"},
}

var socialPlatformNames = map[string]string{
	"GitHub": "github",
	"Twitter": "twitter",
	"邮箱": "email",
	"Email": "email",
	"QQ": "qq",
	"微信": "wechat",
	"Wechat": "wechat",
	"WeChat": "wechat",
	"抖音": "douyin",
	"Douyin": "douyin",
	"哔哩哔哩": "bilibili",
	"B站": "bilibili",
	"Bilibili": "bilibili",
	"小红书": "xiaohongshu",
	"Xiaohongshu": "xiaohongshu",
}

func normalizeSocialLink(link SocialLink) SocialLink {
	platform := strings.TrimSpace(link.Platform)
	if platform == "" {
		platform = socialPlatformNames[strings.TrimSpace(link.Name)]
	}
	if preset, ok := socialPlatformPresets[platform]; ok {
		preset.URL = strings.TrimSpace(link.URL)
		preset.Sort = link.Sort
		if strings.TrimSpace(link.Name) != "" {
			preset.Name = strings.TrimSpace(link.Name)
		}
		return preset
	}
	link.Name = strings.TrimSpace(link.Name)
	link.URL = strings.TrimSpace(link.URL)
	link.Icon = "fa-solid fa-link"
	link.Platform = "custom"
	link.Badge = "链"
	link.Color = "hover:text-indigo-600"
	return link
}

func ensureDefaultSocialLinks(links []SocialLink) []SocialLink {
	seen := make(map[string]bool)
	result := make([]SocialLink, 0, len(links)+5)
	for i, link := range links {
		normalized := normalizeSocialLink(link)
		if normalized.Name == "" {
			continue
		}
		if normalized.Sort <= 0 {
			normalized.Sort = (i + 1) * 10
		}
		result = append(result, normalized)
		if normalized.Platform != "" && normalized.Platform != "custom" {
			seen[normalized.Platform] = true
		}
	}
	defaultSort := len(result) * 10
	for _, platform := range []string{"qq", "wechat", "douyin", "bilibili", "xiaohongshu"} {
		if seen[platform] {
			continue
		}
		defaultSort += 10
		link := socialPlatformPresets[platform]
		link.Sort = defaultSort
		result = append(result, link)
	}
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].Sort < result[j].Sort
	})
	return result
}

// AboutMe 关于我 JSON 结构
type AboutMe struct {
	Name        string       `json:"name"`
	Avatar      string       `json:"avatar"`
	Bio         string       `json:"bio"`
	Location    string       `json:"location"`
	OneWord     string       `json:"one_word"`
	DetailIntro string       `json:"detail_intro"` // 个人简介长文本
	SocialLinks []SocialLink `json:"social_links"`
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

// resolveBannerURL 根据 banner_use_homepage 开关解析各页面的 Banner 图
// pageKey: banner_url_posts / banner_url_moments / banner_url_guestbook / "" (首页直接用 banner_url)
func resolveBannerURL(settings map[string]string, pageKey string) string {
	if settings["banner_use_homepage"] == "true" || settings["banner_use_homepage"] == "" {
		return settings["banner_url"]
	}
	if pageKey == "" {
		return settings["banner_url"]
	}
	if val := settings[pageKey]; val != "" {
		return val
	}
	return settings["banner_url"]
}

// PageData 自定义页面模板数据
type PageData struct {
	SiteTitle        string
	SiteSubtitle     string
	Menus            []models.Menu
	PageData         models.PageView
	SiteFoundedAt    string
	SiteNotification string
	SearchQuery      string
}

// ============ 获取公共数据 ============

// siteSettingsCache 站点设置内存缓存（30 秒），避免每个前台请求都全表扫描。
// 仅缓存读取结果；写入设置（AdminSettingsPost 等）后最多 30 秒生效。
var (
	siteSettingsCache     map[string]string
	siteSettingsCachedAt  time.Time
	siteSettingsCacheMu   sync.RWMutex
	siteSettingsCacheTTL  = 30 * time.Second
)

// InvalidateSiteSettingsCache 使站点设置缓存失效，供后台修改设置后调用。
func InvalidateSiteSettingsCache() {
	siteSettingsCacheMu.Lock()
	siteSettingsCache = nil
	siteSettingsCacheMu.Unlock()
}

func getSiteSettings(db *sql.DB) (map[string]string, error) {
	siteSettingsCacheMu.RLock()
	if siteSettingsCache != nil && time.Since(siteSettingsCachedAt) < siteSettingsCacheTTL {
		// 返回副本，避免调用方污染缓存
		cp := make(map[string]string, len(siteSettingsCache))
		for k, v := range siteSettingsCache {
			cp[k] = v
		}
		siteSettingsCacheMu.RUnlock()
		return cp, nil
	}
	siteSettingsCacheMu.RUnlock()

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

	siteSettingsCacheMu.Lock()
	siteSettingsCache = settings
	siteSettingsCachedAt = time.Now()
	siteSettingsCacheMu.Unlock()

	// 返回副本
	cp := make(map[string]string, len(settings))
	for k, v := range settings {
		cp[k] = v
	}
	return cp, nil
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
	am.SocialLinks = ensureDefaultSocialLinks(am.SocialLinks)
	return am
}

func getMenus(db *sql.DB) ([]models.Menu, error) {
	rows, err := db.Query("SELECT id, name, url, icon, parent_id, order_num, is_system, visible FROM menus WHERE visible = 1 ORDER BY order_num ASC, id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var menus []models.Menu
	for rows.Next() {
		var m models.Menu
		var isSystem int
		var visible int
		if err := rows.Scan(&m.ID, &m.Name, &m.URL, &m.Icon, &m.ParentID, &m.OrderNum, &isSystem, &visible); err != nil {
			continue
		}
		m.IsSystem = isSystem != 0
		m.Visible = visible != 0
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
	db.QueryRow("SELECT COALESCE(SUM(views),0) FROM posts WHERE status='published' AND privacy='public'").Scan(&s.TotalViews)
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
	if len(ids) == 0 {
		return "", nil
	}
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

func queryLog(context string, rows *sql.Rows, err error) *sql.Rows {
	if err != nil {
		log.Printf("[Query] %s: %v", context, err)
		return nil
	}
	return rows
}

// ============ 公共数据查询 ============

func getHotPosts(db *sql.DB) []models.Post {
	rows, err := db.Query("SELECT id, title, content, thumb_url, author, views, category_id, is_top, privacy, status, publish_at, created_at, updated_at FROM posts WHERE status='published' AND privacy='public' ORDER BY views DESC LIMIT 5")
	rows = queryLog("hotPosts", rows, err)
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
	rows, err := db.Query("SELECT t.id, t.name, t.created_at, COUNT(pt.post_id) as post_count FROM tags t LEFT JOIN post_tags pt ON t.id = pt.tag_id GROUP BY t.id ORDER BY post_count DESC LIMIT 20")
	rows = queryLog("tags", rows, err)
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
	rows, err := db.Query("SELECT id, target_type, target_id, author, author_avatar, content, image_url, likes, reply_to, created_at, parent_id FROM comments ORDER BY created_at DESC LIMIT 5")
	rows = queryLog("recentComments", rows, err)
	var comments []models.Comment
	if rows != nil {
		for rows.Next() {
			var c models.Comment
			scanLog(rows.Scan(&c.ID, &c.TargetType, &c.TargetID, &c.Author, &c.AuthorAvatar, &c.Content, &c.ImageURL, &c.Likes, &c.ReplyTo, &c.CreatedAt, &c.ParentID), "recentComments")
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

		// 文章列表（附带分类名和评论数），首页随机排序，置顶靠前
		query := fmt.Sprintf(`
			SELECT p.id, p.title, p.content, p.thumb_url, p.author, p.views, p.category_id,
			       p.is_top, p.privacy, p.status, p.publish_at, p.created_at, p.updated_at,
			       COALESCE(c.name, '') as category_name, COALESCE(c.slug,'') as category_slug,
			       COUNT(cm.id) as comment_count
			FROM posts p
			LEFT JOIN categories c ON p.category_id = c.id
			LEFT JOIN comments cm ON cm.target_type='post' AND cm.target_id=p.id
			%s
			GROUP BY p.id
			ORDER BY p.is_top DESC, RANDOM()
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

		// 瞬间流（最近10条，随机排序）
		momentRows, _ := db.Query("SELECT id, content, media_urls, likes, created_at FROM moments ORDER BY RANDOM() LIMIT 10")
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
			       COUNT(cm.id) as comment_count
			FROM posts p
			LEFT JOIN categories c ON p.category_id = c.id
			LEFT JOIN comments cm ON cm.target_type='post' AND cm.target_id=p.id
			%s
			GROUP BY p.id
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
			BannerURL:        resolveBannerURL(settings, "banner_url_posts"),
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
			commentRows, err := db.Query("SELECT id, target_type, target_id, author, author_avatar, content, image_url, likes, created_at, parent_id FROM comments WHERE target_type='moment' AND target_id IN ("+ph+") ORDER BY created_at DESC", phArgs...)
			if err == nil {
				for commentRows.Next() {
					var cm models.Comment
					scanLog(commentRows.Scan(&cm.ID, &cm.TargetType, &cm.TargetID, &cm.Author, &cm.AuthorAvatar, &cm.Content, &cm.ImageURL, &cm.Likes, &cm.CreatedAt, &cm.ParentID), "momentComments")
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
			BannerURL:        resolveBannerURL(settings, "banner_url_moments"),
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
	SearchQuery      string
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

		// 浏览量 +1（同一访客同篇文章每天只计一次）
		vid, _ := c.Cookie("visitor_id")
		if vid == "" {
			vid = uuid.New().String()[:8]
			c.SetCookie("visitor_id", vid, 365*24*3600, "/", "", false, true)
		}
		today := time.Now().Format("2006-01-02")
		res, _ := db.Exec("INSERT OR IGNORE INTO post_visits (post_id, visitor_id, visit_date) VALUES (?, ?, ?)", id, vid, today)
		if n, _ := res.RowsAffected(); n > 0 {
			db.Exec("UPDATE posts SET views = views + 1 WHERE id = ?", id)
		}
		// 重新读取最新浏览量用于展示
		db.QueryRow("SELECT views FROM posts WHERE id = ?", id).Scan(&post.Views)

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
		commentRows, err := db.Query("SELECT id, target_type, target_id, author, author_avatar, content, image_url, likes, reply_to, created_at, parent_id FROM comments ORDER BY created_at DESC LIMIT 5")
		commentRows = queryLog("postDetailRecentComments", commentRows, err)
		var recentComments []models.Comment
		if commentRows != nil {
			for commentRows.Next() {
				var cm models.Comment
				scanLog(commentRows.Scan(&cm.ID, &cm.TargetType, &cm.TargetID, &cm.Author, &cm.AuthorAvatar, &cm.Content, &cm.ImageURL, &cm.Likes, &cm.ReplyTo, &cm.CreatedAt, &cm.ParentID), "recentComments")
				recentComments = append(recentComments, cm)
			}
			commentRows.Close()
		}

		// 文章评论
		var postComments []models.Comment
		pcRows, err := db.Query("SELECT id, target_type, target_id, author, author_avatar, content, image_url, likes, created_at, parent_id FROM comments WHERE target_type='post' AND target_id = ? ORDER BY created_at DESC", id)
		pcRows = queryLog("postComments", pcRows, err)
		if pcRows != nil {
			for pcRows.Next() {
				var cm models.Comment
				scanLog(pcRows.Scan(&cm.ID, &cm.TargetType, &cm.TargetID, &cm.Author, &cm.AuthorAvatar, &cm.Content, &cm.ImageURL, &cm.Likes, &cm.CreatedAt, &cm.ParentID), "postComments")
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

		// 获取或生成访客标识
		vid, _ := c.Cookie("visitor_id")
		if vid == "" {
			vid = uuid.New().String()[:8]
			c.SetCookie("visitor_id", vid, 365*24*3600, "/", "", false, true)
		}

		// 用事务保证点赞/取消与计数的一致性，避免竞态
		tx, txErr := db.Begin()
		if txErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "操作失败，请稍后重试"})
			return
		}
		defer tx.Rollback() //nolint:errcheck

		// 尝试插入点赞记录（利用 UNIQUE 约束防重复）
		res, insErr := tx.Exec("INSERT OR IGNORE INTO moment_likes (moment_id, visitor_id) VALUES (?, ?)", id, vid)
		if insErr != nil {
			log.Printf("[MomentLike] insert error: %v", insErr)
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
			tx.QueryRow("UPDATE moments SET likes = likes + 1 WHERE id=? RETURNING likes", id).Scan(&finalLike)
			liked = true
		} else {
			// 已存在，取消点赞；仅当确实删除了一条记录时才 -1
			delRes, _ := tx.Exec("DELETE FROM moment_likes WHERE moment_id=? AND visitor_id=?", id, vid)
			if dn, _ := delRes.RowsAffected(); dn > 0 {
				tx.QueryRow("UPDATE moments SET likes = MAX(0, likes - 1) WHERE id=? RETURNING likes", id).Scan(&finalLike)
			} else {
				tx.QueryRow("SELECT likes FROM moments WHERE id=?", id).Scan(&finalLike)
			}
			liked = false
		}
		if err := tx.Commit(); err != nil {
			log.Printf("[MomentLike] commit error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "操作失败，请稍后重试"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"ok": true, "likes": finalLike, "liked": liked})
	}
}

// ============ 搜索页处理器 ============

func SearchHandler(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		settings, _ := getSiteSettings(db)
		menus, _ := getMenus(db)
		query := strings.TrimSpace(c.Query("q"))

		var posts []models.PostWithMeta
		var moments []models.MomentView
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

			// 搜索瞬间
			mRows, mErr := db.Query("SELECT id, content, media_urls, likes, created_at FROM moments WHERE content LIKE ? ORDER BY created_at DESC LIMIT 20", likeQuery)
			if mErr == nil && mRows != nil {
				for mRows.Next() {
					var m models.MomentView
					scanLog(mRows.Scan(&m.ID, &m.Content, &m.MediaURLs, &m.Likes, &m.CreatedAt), "searchMoments")
					m.IDStr = strconv.FormatInt(m.ID, 10)
					moments = append(moments, m)
				}
				mRows.Close()
			}
		}

		hotPosts := getHotPosts(db)
		tags := getTags(db)
		recentComments := getRecentComments(db)
		topPost := getTopPost(db)

		data := IndexData{
			SiteTitle:        settings["site_title"],
			SiteSubtitle:     settings["site_subtitle"],
			SiteFoundedAt:    settings["site_founded_at"],
			SiteNotification: settings["site_notification"],
			AboutMe:          getAboutMe(settings),
			Menus:            menus,
			Stats:            getStats(db),
			Posts:            posts,
			Moments:          moments,
			MomentMediaMap:   map[string][]string{},
			Tags:             tags,
			HotPosts:         hotPosts,
			RecentComments:   recentComments,
			SearchQuery:      query,
			Page:             1,
			TotalPages:       1,
			PageRange:        []int{1},
		}
		c.HTML(http.StatusOK, "posts.html", gin.H{
			"SiteTitle":        data.SiteTitle,
			"SiteSubtitle":     data.SiteSubtitle,
			"SiteFoundedAt":    data.SiteFoundedAt,
			"SiteNotification": data.SiteNotification,
			"AboutMe":          data.AboutMe,
			"Menus":            data.Menus,
			"Stats":            data.Stats,
			"BannerURL":        settings["banner_url"],
			"TopPost":          topPost,
			"Posts":            data.Posts,
			"Tags":             data.Tags,
			"HotPosts":         data.HotPosts,
			"RecentComments":   data.RecentComments,
			"SearchQuery":      data.SearchQuery,
			"Page":             data.Page,
			"TotalPages":       data.TotalPages,
			"PageRange":        data.PageRange,
			"Moments":          data.Moments,
			"MomentMediaMap":   data.MomentMediaMap,
		})
	}
}

// ============ 关于我处理器 ============

func AboutHandler(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		settings, _ := getSiteSettings(db)
		menus, _ := getMenus(db)
		hotPosts := getHotPosts(db)
		tags := getTags(db)
		recentComments := getRecentComments(db)
		aboutMe := getAboutMe(settings)

		// 设备列表
		dRows, _ := db.Query("SELECT id, name, image_url, info, order_num FROM devices ORDER BY order_num ASC, id ASC")
		var devices []models.Device
		if dRows != nil {
			for dRows.Next() {
				var d models.Device
				dRows.Scan(&d.ID, &d.Name, &d.ImageURL, &d.Info, &d.OrderNum)
				devices = append(devices, d)
			}
			dRows.Close()
		}

		data := IndexData{
			SiteTitle:        settings["site_title"],
			SiteSubtitle:     settings["site_subtitle"],
			SiteFoundedAt:    settings["site_founded_at"],
			SiteNotification: settings["site_notification"],
			AboutMe:          aboutMe,
			Menus:            menus,
			Tags:             tags,
			HotPosts:         hotPosts,
			RecentComments:   recentComments,
		}
		// 额外数据用 gin.H 包装到模板
		c.HTML(http.StatusOK, "about.html", gin.H{
			"SiteTitle":        data.SiteTitle,
			"SiteSubtitle":     data.SiteSubtitle,
			"SiteFoundedAt":    data.SiteFoundedAt,
			"SiteNotification": data.SiteNotification,
			"AboutMe":          data.AboutMe,
			"Menus":            data.Menus,
			"Stats":            getStats(db),
			"TopPost":          getTopPost(db),
			"BannerURL":        settings["banner_url"],
			"Tags":             data.Tags,
			"HotPosts":         data.HotPosts,
			"RecentComments":   data.RecentComments,
			"Devices":          devices,
			"DetailIntro":      template.HTML(utils.SanitizeHTML(aboutMe.DetailIntro)),
			"AmapKey":          settings["amap_key"],
			"AmapJsCode":       settings["amap_jscode"],
		})
	}
}

// ============ 足迹 API ============

func FootprintsAPI(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var raw string
		db.QueryRow("SELECT value FROM system_settings WHERE key='footprints_json'").Scan(&raw)
		if raw == "" {
			raw = "[]"
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", []byte(raw))
	}
}

// MapTest 地图测试页
func MapTestHandler(c *gin.Context) {
	c.HTML(http.StatusOK, "maptest.html", nil)
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
			BannerURL:         resolveBannerURL(settings, "banner_url_guestbook"),
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
