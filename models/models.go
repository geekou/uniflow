package models

import (
	"html/template"
	"time"
)

// Post 文章模型
type Post struct {
	ID         int64      `json:"id"`
	Title      string     `json:"title"`
	Content    string     `json:"content"`
	ThumbURL   string     `json:"thumb_url"`
	Author     string     `json:"author"`
	Views      int64      `json:"views"`
	CategoryID *int64     `json:"category_id"`
	IsTop      int        `json:"is_top"`
	Privacy    string     `json:"privacy"` // public / private
	Status     string     `json:"status"`  // draft / published / scheduled
	PublishAt  *time.Time `json:"publish_at"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	LikeCount    int64 `json:"like_count"`
	DislikeCount int64 `json:"dislike_count"`
	// 联表字段（非数据库字段）
	CategoryName string `json:"category_name,omitempty"`
	Tags         []Tag  `json:"tags,omitempty"`
}

// Moment 瞬间模型
type Moment struct {
	ID        int64     `json:"id"`
	Content   string    `json:"content"`
	MediaURLs string    `json:"media_urls"` // 逗号分隔的图片/视频URL
	Likes     int64     `json:"likes"`
	CreatedAt time.Time `json:"created_at"`
}

// Comment 评论模型
type Comment struct {
	ID           int64     `json:"id"`
	TargetType   string    `json:"target_type"` // post / moment
	TargetID     int64     `json:"target_id"`
	Author       string    `json:"author"`
	AuthorAvatar string    `json:"author_avatar"`
	Content      string    `json:"content"`
	ImageURL     string    `json:"image_url"`
	ParentID     int64     `json:"parent_id"` // 回复的父评论ID，0表示顶级评论
	ReplyTo      string    `json:"reply_to"`  // 回复的目标作者名（模板渲染用）
	CreatedAt    time.Time `json:"created_at"`
}

// Category 分类模型
type Category struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"created_at"`
	// 统计字段
	PostCount int64 `json:"post_count,omitempty"`
}

// Tag 标签模型
type Tag struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// 统计字段
	PostCount int64 `json:"post_count,omitempty"`
}

// PostTag 文章-标签关联模型
type PostTag struct {
	PostID int64 `json:"post_id"`
	TagID  int64 `json:"tag_id"`
}

// Menu 菜单模型
type Menu struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Icon     string `json:"icon"` // FontAwesome 类名
	ParentID *int64 `json:"parent_id"`
	OrderNum int    `json:"order_num"`
	// 子菜单
	Children []Menu `json:"children,omitempty"`
}

// Page 独立自定义页面模型
type Page struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Slug      string    `json:"slug"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SystemSetting 网站设置模型
type SystemSetting struct {
	ID    int64  `json:"id"`
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Log 安全审计日志模型
type Log struct {
	ID        int64     `json:"id"`
	LogType   string    `json:"log_type"` // login / operation
	Operator  string    `json:"operator"`
	Action    string    `json:"action"`
	IP        string    `json:"ip"`
	UserAgent string    `json:"user_agent"`
	Result    string    `json:"result"` // success / failure
	CreatedAt time.Time `json:"created_at"`
}

// PostWithMeta 文章+分类名+评论数（用于列表展示）
type PostWithMeta struct {
	Post
	CategoryName string `json:"category_name,omitempty"`
	CategorySlug string `json:"category_slug,omitempty"`
	CommentCount int    `json:"comment_count"`
}

// MomentView 瞬间视图模型（模板渲染用）
type MomentView struct {
	ID           int64     `json:"id"`
	IDStr        string    `json:"id_str"`
	Content      string    `json:"content"`
	MediaURLs    string    `json:"media_urls"`
	Likes        int64     `json:"likes"`
	CreatedAt    time.Time `json:"created_at"`
	Comments     []Comment `json:"comments"`
	CommentCount int       `json:"comment_count"`
}

// PageView 自定义页面视图模型（带 HTMLContent）
type PageView struct {
	ID          int64         `json:"id"`
	Title       string        `json:"title"`
	Slug        string        `json:"slug"`
	HTMLContent template.HTML `json:"html_content"`
	CreatedAt   time.Time     `json:"created_at"`
}

// User 管理员用户模型
type User struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	Password  string    `json:"password,omitempty"` // 密码哈希
	Role      string    `json:"role"`
	AvatarURL string    `json:"avatar_url"`
	CreatedAt time.Time `json:"created_at"`
}

// Guestbook 留言模型
type Guestbook struct {
	ID         int64      `json:"id"`
	Author     string     `json:"author"`
	Content    string     `json:"content"`
	ImageURL   string     `json:"image_url"`
	IP         string     `json:"ip"`
	AdminReply string     `json:"admin_reply"`
	RepliedAt  *time.Time `json:"replied_at"`
	CreatedAt  time.Time  `json:"created_at"`
}
