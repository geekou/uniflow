package models

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var DB *sql.DB

// InitDB 初始化 SQLite 数据库连接并建表
func InitDB(dbPath string) error {
	var err error
	// 启用 WAL 模式以提升并发性能。
	// 用 url.URL 构造 DSN，避免 DB_PATH 中的 ?/& 污染 SQLite 连接参数。
	// Windows 盘符路径（如 E:/cc/uniflow/uniflow.db）必须放在 Opaque 中，否则会被解析成 URI authority。
	dsnURL := url.URL{Scheme: "file", Opaque: dbPath}
	q := dsnURL.Query()
	q.Set("_journal_mode", "WAL")
	q.Set("_busy_timeout", "5000")
	dsnURL.RawQuery = q.Encode()
	dsn := dsnURL.String()
	DB, err = sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	// 连接池配置
	DB.SetMaxOpenConns(3) // SQLite WAL 模式允许有限并发读
	DB.SetMaxIdleConns(2)

	// 验证连接
	if err = DB.Ping(); err != nil {
		return fmt.Errorf("failed to ping database: %w", err)
	}

	// 设置 WAL 模式
	if _, err = DB.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return fmt.Errorf("failed to set WAL mode: %w", err)
	}
	// 启用外键约束
	if _, err = DB.Exec("PRAGMA foreign_keys=ON"); err != nil {
		return fmt.Errorf("failed to enable foreign keys: %w", err)
	}

	log.Println("[DB] SQLite connected, WAL mode enabled")

	// 建表
	if err = createTables(); err != nil {
		return fmt.Errorf("failed to create tables: %w", err)
	}

	// 迁移：comments 表添加 likes 列
	if err = migrateCommentLikes(); err != nil {
		return fmt.Errorf("failed to migrate comment likes: %w", err)
	}

	// 迁移：comments 表添加 parent_id / reply_to / author_avatar 列
	if err = migrateCommentReply(); err != nil {
		return fmt.Errorf("failed to migrate comment reply fields: %w", err)
	}

	// 修复旧数据库：comments 表曾有错误的 FK 约束（target_id -> posts.id），
	// 导致瞬间评论无法插入。通过 PRAGMA 精确检测并迁移，避免误删评论。
	// 必须在 migrateCommentReply 之后执行，因为迁移 SQL 引用了这些列。
	if err = fixCommentsTable(); err != nil {
		return fmt.Errorf("failed to fix comments table: %w", err)
	}

	// 迁移：menus 表添加 is_system / visible 列
	if err = migrateMenuFlags(); err != nil {
		return fmt.Errorf("failed to migrate menu flags: %w", err)
	}

	// 迁移：post_likes 表 UNIQUE 约束修正
	if err = migratePostLikesUnique(); err != nil {
		return fmt.Errorf("failed to migrate post_likes unique: %w", err)
	}

	// 预设默认配置
	if err = seedSystemSettings(); err != nil {
		return fmt.Errorf("failed to seed settings: %w", err)
	}

	// 确保 HMAC 密钥存在（持久化到 system_settings，重启后保持一致）
	var hmacKey string
	DB.QueryRow("SELECT value FROM system_settings WHERE key='hmac_secret'").Scan(&hmacKey)
	if hmacKey == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return fmt.Errorf("failed to generate HMAC key: %w", err)
		}
		hmacKey = hex.EncodeToString(key)
		DB.Exec("INSERT INTO system_settings (key, value) VALUES ('hmac_secret', ?)", hmacKey)
		log.Println("[DB] Generated and stored HMAC secret key")
	}

	// 不再自动创建默认管理员，由首次部署引导页处理
	// 默认菜单由后台管理，仅首次初始化时创建
	var menuCount int
	DB.QueryRow("SELECT COUNT(*) FROM menus").Scan(&menuCount)
	if menuCount == 0 {
		seedDefaultMenus(DB)
	}

	// 建站时间（如果未设置则默认当天）
	var founded string
	DB.QueryRow("SELECT value FROM system_settings WHERE key='site_founded_at'").Scan(&founded)
	if founded == "" {
		DB.Exec("INSERT INTO system_settings (key, value) VALUES ('site_founded_at', ?)",
			time.Now().Format("2006-01-02T15:04"))
	}

	log.Println("[DB] All tables created and seeded successfully")
	return nil
}

// seedDefaultMenus 仅在首次初始化（菜单表为空）时写入默认菜单
func seedDefaultMenus(db *sql.DB) {
	menus := []struct {
		name, url, icon string
		orderNum        int
	}{
		{"首页", "/", "fa-solid fa-house", 1},
		{"文章", "/posts", "fa-solid fa-book", 2},
		{"瞬间", "/moments", "fa-solid fa-bolt", 3},
		{"留言板", "/guestbook", "fa-regular fa-envelope", 4},
		{"关于我", "/about", "fa-solid fa-user", 5},
	}
	for _, m := range menus {
		db.Exec("INSERT INTO menus (name, url, icon, order_num, is_system, visible, parent_id) VALUES (?, ?, ?, ?, 1, 1, 0)",
			m.name, m.url, m.icon, m.orderNum)
	}
	log.Println("[DB] Default menus seeded")
}

// CloseDB 关闭数据库连接
func CloseDB() {
	if DB != nil {
		_ = DB.Close()
		log.Println("[DB] Database connection closed")
	}
}

// GetSetting 读取单个系统设置值，不存在返回空字符串
func GetSetting(key string) string {
	var val string
	DB.QueryRow("SELECT value FROM system_settings WHERE key=?", key).Scan(&val)
	return val
}

func createTables() error {
	tables := []string{
		// 文章表
		`CREATE TABLE IF NOT EXISTS posts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			title TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL DEFAULT '',
			thumb_url TEXT DEFAULT '',
			author TEXT NOT NULL DEFAULT 'HuiNan',
			views INTEGER NOT NULL DEFAULT 0,
			category_id INTEGER,
			is_top INTEGER NOT NULL DEFAULT 0,
			privacy TEXT NOT NULL DEFAULT 'public' CHECK(privacy IN ('public','private')),
			status TEXT NOT NULL DEFAULT 'draft' CHECK(status IN ('draft','published','scheduled')),
			publish_at DATETIME,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (category_id) REFERENCES categories(id) ON DELETE SET NULL
		)`,
		// 瞬间表
		`CREATE TABLE IF NOT EXISTS moments (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			content TEXT NOT NULL DEFAULT '',
			media_urls TEXT DEFAULT '',
			likes INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		// 评论表
		// Bug 3 修复：移除不合理的 foreign key 约束。
		// target_id 的含义取决于 target_type（post 或 moment），SQLite 不支持条件外键，
		// 故在 CommentSubmit handler 中通过应用层验证目标存在性，不再依赖 DB 层外键约束。
		`CREATE TABLE IF NOT EXISTS comments (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			target_type TEXT NOT NULL CHECK(target_type IN ('post','moment')),
			target_id INTEGER NOT NULL,
			author TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL DEFAULT '',
			image_url TEXT DEFAULT '',
			likes INTEGER NOT NULL DEFAULT 0,
			parent_id INTEGER NOT NULL DEFAULT 0,
			reply_to TEXT NOT NULL DEFAULT '',
			author_avatar TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		// 评论点赞记录表
		`CREATE TABLE IF NOT EXISTS comment_likes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			comment_id INTEGER NOT NULL,
			anonymous_id TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(comment_id, anonymous_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_comment_likes_comment ON comment_likes(comment_id)`,
		// 设备展示表
		`CREATE TABLE IF NOT EXISTS devices (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL DEFAULT '',
			image_url TEXT NOT NULL DEFAULT '',
			info TEXT NOT NULL DEFAULT '',
			order_num INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		// 分类表
		`CREATE TABLE IF NOT EXISTS categories (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			slug TEXT NOT NULL UNIQUE,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		// 标签表
		`CREATE TABLE IF NOT EXISTS tags (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		// 文章-标签关联表
		`CREATE TABLE IF NOT EXISTS post_tags (
			post_id INTEGER NOT NULL,
			tag_id INTEGER NOT NULL,
			PRIMARY KEY (post_id, tag_id),
			FOREIGN KEY (post_id) REFERENCES posts(id) ON DELETE CASCADE,
			FOREIGN KEY (tag_id) REFERENCES tags(id) ON DELETE CASCADE
		)`,
		// 左侧栏菜单表
		`CREATE TABLE IF NOT EXISTS menus (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			url TEXT DEFAULT '',
			icon TEXT DEFAULT '',
			parent_id INTEGER DEFAULT 0,
			order_num INTEGER NOT NULL DEFAULT 0,
			is_system INTEGER NOT NULL DEFAULT 0,
			visible INTEGER NOT NULL DEFAULT 1,
			FOREIGN KEY (parent_id) REFERENCES menus(id) ON DELETE SET NULL
		)`,
		// 独立自定义页面表
		`CREATE TABLE IF NOT EXISTS pages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			title TEXT NOT NULL,
			slug TEXT NOT NULL UNIQUE,
			content TEXT DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		// 网站核心设置表
		`CREATE TABLE IF NOT EXISTS system_settings (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			key TEXT NOT NULL UNIQUE,
			value TEXT DEFAULT ''
		)`,
		// 安全与审计日志表
		`CREATE TABLE IF NOT EXISTS logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			log_type TEXT NOT NULL CHECK(log_type IN ('login','operation')),
			operator TEXT DEFAULT '',
			action TEXT DEFAULT '',
			ip TEXT DEFAULT '',
			user_agent TEXT DEFAULT '',
			result TEXT DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		// 管理员用户表
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL UNIQUE,
			password TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'admin',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		// 留言表
		`CREATE TABLE IF NOT EXISTS guestbook (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			author TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL DEFAULT '',
			image_url TEXT DEFAULT '',
			ip TEXT DEFAULT '',
			admin_reply TEXT NOT NULL DEFAULT '',
			replied_at DATETIME,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS moment_likes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		moment_id INTEGER NOT NULL,
		visitor_id TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(moment_id, visitor_id)
	)`,
		// 文章点赞/拍砖去重表
		`CREATE TABLE IF NOT EXISTS post_likes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		post_id INTEGER NOT NULL,
		visitor_id TEXT NOT NULL DEFAULT '',
		type TEXT NOT NULL DEFAULT 'like' CHECK(type IN ('like','dislike')),
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(post_id, visitor_id)
	)`,
		// 文章访问去重表（同一访客同篇文章每天只计一次）
		`CREATE TABLE IF NOT EXISTS post_visits (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		post_id INTEGER NOT NULL,
		visitor_id TEXT NOT NULL DEFAULT '',
		visit_date TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(post_id, visitor_id, visit_date)
	)`,
		// 管理员会话持久化表（服务重启后仍可恢复登录态）
		`CREATE TABLE IF NOT EXISTS sessions (
		token TEXT PRIMARY KEY,
		username TEXT NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		last_active_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_last_active ON sessions(last_active_at)`,
	}

	// 创建索引
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_posts_category ON posts(category_id)`,
		`CREATE INDEX IF NOT EXISTS idx_posts_status ON posts(status)`,
		`CREATE INDEX IF NOT EXISTS idx_posts_publish_at ON posts(publish_at)`,
		`CREATE INDEX IF NOT EXISTS idx_posts_is_top ON posts(is_top)`,
		`CREATE INDEX IF NOT EXISTS idx_posts_public_list ON posts(status, privacy, is_top DESC, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_posts_public_views ON posts(status, privacy, views DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_posts_scheduled ON posts(status, publish_at)`,
		`CREATE INDEX IF NOT EXISTS idx_comments_target ON comments(target_type, target_id)`,
		`CREATE INDEX IF NOT EXISTS idx_comments_created ON comments(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_moments_created ON moments(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_type ON logs(log_type)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_type_id ON logs(log_type, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_users_username ON users(username)`,
		`CREATE INDEX IF NOT EXISTS idx_guestbook_created ON guestbook(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_menus_order ON menus(parent_id, order_num, id)`,
		`CREATE INDEX IF NOT EXISTS idx_post_tags_tag ON post_tags(tag_id, post_id)`,
	}

	for _, ddl := range tables {
		if _, err := DB.Exec(ddl); err != nil {
			return fmt.Errorf("create table failed: %w", err)
		}
	}

	for _, idx := range indexes {
		if _, err := DB.Exec(idx); err != nil {
			return fmt.Errorf("create index failed: %w", err)
		}
	}

	log.Println("[DB] All 12 tables and indexes created")

	// 迁移：为已有 pages 表添加 updated_at 列（如缺失）
	var hasUpdatedAt bool
	colRows, err := DB.Query("PRAGMA table_info(pages)")
	if err == nil {
		for colRows.Next() {
			var cid int
			var name, ctype string
			var notnull int
			var dfltValue interface{}
			var pk int
			if err := colRows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
				log.Printf("[DB] scan pages table info failed: %v", err)
				continue
			}
			if name == "updated_at" {
				hasUpdatedAt = true
			}
		}
		colRows.Close()
	}
	if !hasUpdatedAt {
		_, _ = DB.Exec("ALTER TABLE pages ADD COLUMN updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP")
		log.Println("[DB] Migration: added updated_at column to pages table")
	}

	// moments 表 updated_at 字段迁移
	colRows2, err := DB.Query("PRAGMA table_info(moments)")
	if err == nil {
		hasMomentUpdatedAt := false
		for colRows2.Next() {
			var cid int
			var cname, ctype string
			var notnull int
			var dfltValue interface{}
			var pk int
			_ = colRows2.Scan(&cid, &cname, &ctype, &notnull, &dfltValue, &pk)
			if cname == "updated_at" {
				hasMomentUpdatedAt = true
			}
		}
		colRows2.Close()
		if !hasMomentUpdatedAt {
			_, _ = DB.Exec("ALTER TABLE moments ADD COLUMN updated_at DATETIME")
			log.Println("[DB] Migration: added updated_at column to moments table")
		}
	}

	// guestbook 表回复字段迁移
	colRowsGuestbook, err := DB.Query("PRAGMA table_info(guestbook)")
	if err == nil {
		hasAdminReply := false
		hasRepliedAt := false
		for colRowsGuestbook.Next() {
			var cid int
			var cname, ctype string
			var notnull int
			var dfltValue interface{}
			var pk int
			_ = colRowsGuestbook.Scan(&cid, &cname, &ctype, &notnull, &dfltValue, &pk)
			switch cname {
			case "admin_reply":
				hasAdminReply = true
			case "replied_at":
				hasRepliedAt = true
			}
		}
		colRowsGuestbook.Close()
		if !hasAdminReply {
			_, _ = DB.Exec("ALTER TABLE guestbook ADD COLUMN admin_reply TEXT NOT NULL DEFAULT ''")
			log.Println("[DB] Migration: added admin_reply column to guestbook table")
		}
		if !hasRepliedAt {
			_, _ = DB.Exec("ALTER TABLE guestbook ADD COLUMN replied_at DATETIME")
			log.Println("[DB] Migration: added replied_at column to guestbook table")
		}
	}

	// comments 表 parent_id 字段迁移
	colRows3, err := DB.Query("PRAGMA table_info(comments)")
	if err == nil {
		hasParentID := false
		hasAuthorAvatar := false
		for colRows3.Next() {
			var cid int
			var cname, ctype string
			var notnull int
			var dfltValue interface{}
			var pk int
			_ = colRows3.Scan(&cid, &cname, &ctype, &notnull, &dfltValue, &pk)
			if cname == "parent_id" {
				hasParentID = true
			}
			if cname == "author_avatar" {
				hasAuthorAvatar = true
			}
		}
		colRows3.Close()
		if !hasParentID {
			_, _ = DB.Exec("ALTER TABLE comments ADD COLUMN parent_id INTEGER NOT NULL DEFAULT 0")
			log.Println("[DB] Migration: added parent_id column to comments table")
		}
		if !hasAuthorAvatar {
			_, _ = DB.Exec("ALTER TABLE comments ADD COLUMN author_avatar TEXT NOT NULL DEFAULT ''")
			log.Println("[DB] Migration: added author_avatar column to comments table")
		}
	}

	// users 表 avatar_url 字段迁移
	colRows4, err := DB.Query("PRAGMA table_info(users)")
	if err == nil {
		hasAvatarURL := false
		for colRows4.Next() {
			var cid int
			var cname, ctype string
			var notnull int
			var dfltValue interface{}
			var pk int
			_ = colRows4.Scan(&cid, &cname, &ctype, &notnull, &dfltValue, &pk)
			if cname == "avatar_url" {
				hasAvatarURL = true
			}
		}
		colRows4.Close()
		if !hasAvatarURL {
			_, _ = DB.Exec("ALTER TABLE users ADD COLUMN avatar_url TEXT NOT NULL DEFAULT ''")
			log.Println("[DB] Migration: added avatar_url column to users table")
		}
	}

	// posts 表 like_count / dislike_count 字段迁移
	colRows5, err := DB.Query("PRAGMA table_info(posts)")
	if err == nil {
		hasLikes, hasDislikes := false, false
		for colRows5.Next() {
			var cid int
			var cname, ctype string
			var notnull int
			var dfltValue interface{}
			var pk int
			_ = colRows5.Scan(&cid, &cname, &ctype, &notnull, &dfltValue, &pk)
			if cname == "like_count" {
				hasLikes = true
			}
			if cname == "dislike_count" {
				hasDislikes = true
			}
		}
		colRows5.Close()
		if !hasLikes {
			_, _ = DB.Exec("ALTER TABLE posts ADD COLUMN like_count INTEGER NOT NULL DEFAULT 0")
			log.Println("[DB] Migration: added like_count column to posts table")
		}
		if !hasDislikes {
			_, _ = DB.Exec("ALTER TABLE posts ADD COLUMN dislike_count INTEGER NOT NULL DEFAULT 0")
			log.Println("[DB] Migration: added dislike_count column to posts table")
		}
	}

	return nil
}

// fixCommentsTable 修复旧数据库中 comments 表的错误外键约束。
// 旧版表结构: FOREIGN KEY (target_id) REFERENCES posts(id) 导致瞬间评论无法插入。
func fixCommentsTable() error {
	fkRows, err := DB.Query("PRAGMA foreign_key_list(comments)")
	if err != nil {
		return fmt.Errorf("failed to inspect comments foreign keys: %w", err)
	}
	defer fkRows.Close()

	hasOldPostFK := false
	for fkRows.Next() {
		var id, seq int
		var tableName, fromCol, toCol, onUpdate, onDelete, match string
		if err := fkRows.Scan(&id, &seq, &tableName, &fromCol, &toCol, &onUpdate, &onDelete, &match); err != nil {
			return fmt.Errorf("failed to scan comments foreign key: %w", err)
		}
		if tableName == "posts" && fromCol == "target_id" && toCol == "id" {
			hasOldPostFK = true
			break
		}
	}
	if err := fkRows.Err(); err != nil {
		return fmt.Errorf("failed to iterate comments foreign keys: %w", err)
	}
	if !hasOldPostFK {
		log.Println("[DB] comments table schema OK")
		return nil
	}

	log.Println("[DB] comments table has old post FK; migrating without data loss")
	if _, err := DB.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		return fmt.Errorf("failed to disable foreign keys: %w", err)
	}
	defer DB.Exec("PRAGMA foreign_keys=ON")

	tx, err := DB.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin comments migration: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`CREATE TABLE comments_new (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		target_type TEXT NOT NULL CHECK(target_type IN ('post','moment')),
		target_id INTEGER NOT NULL,
		author TEXT NOT NULL DEFAULT '',
		author_avatar TEXT NOT NULL DEFAULT '',
		content TEXT NOT NULL DEFAULT '',
		image_url TEXT DEFAULT '',
		likes INTEGER NOT NULL DEFAULT 0,
		reply_to TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		parent_id INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		return fmt.Errorf("failed to create comments_new: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO comments_new (id, target_type, target_id, author, author_avatar, content, image_url, likes, reply_to, created_at, parent_id)
		SELECT id, target_type, target_id, author,
			COALESCE(author_avatar, ''), content, COALESCE(image_url, ''), COALESCE(likes, 0), COALESCE(reply_to, ''), created_at, COALESCE(parent_id, 0)
		FROM comments`); err != nil {
		return fmt.Errorf("failed to copy comments data: %w", err)
	}
	if _, err := tx.Exec("DROP TABLE comments"); err != nil {
		return fmt.Errorf("failed to drop old comments table: %w", err)
	}
	if _, err := tx.Exec("ALTER TABLE comments_new RENAME TO comments"); err != nil {
		return fmt.Errorf("failed to rename comments_new: %w", err)
	}
	if _, err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_comments_target ON comments(target_type, target_id)"); err != nil {
		return fmt.Errorf("failed to recreate comments target index: %w", err)
	}
	if _, err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_comments_created ON comments(created_at DESC)"); err != nil {
		return fmt.Errorf("failed to recreate comments created index: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit comments migration: %w", err)
	}
	log.Println("[DB] comments table migrated without old FK")
	return nil
}

// migrateCommentLikes 为 comments 表添加 likes 列（迁移旧数据库）
func migrateCommentLikes() error {
	rows, err := DB.Query("PRAGMA table_info(comments)")
	if err != nil {
		return err
	}
	defer rows.Close()
	hasLikes := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var defVal interface{}
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &defVal, &pk); err != nil {
			continue
		}
		if name == "likes" {
			hasLikes = true
			break
		}
	}
	if !hasLikes {
		_, err := DB.Exec("ALTER TABLE comments ADD COLUMN likes INTEGER NOT NULL DEFAULT 0")
		if err != nil {
			return fmt.Errorf("failed to add likes column: %w", err)
		}
		log.Println("[DB] Migration: added likes column to comments table")
	}
	return nil
}

// migrateCommentReply 为 comments 表添加 parent_id / reply_to / author_avatar 列
func migrateCommentReply() error {
	rows, err := DB.Query("PRAGMA table_info(comments)")
	if err != nil {
		return err
	}
	defer rows.Close()
	hasParentID, hasReplyTo, hasAvatar := false, false, false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var defVal interface{}
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &defVal, &pk); err != nil {
			continue
		}
		switch name {
		case "parent_id":
			hasParentID = true
		case "reply_to":
			hasReplyTo = true
		case "author_avatar":
			hasAvatar = true
		}
	}
	if !hasParentID {
		_, _ = DB.Exec("ALTER TABLE comments ADD COLUMN parent_id INTEGER NOT NULL DEFAULT 0")
		log.Println("[DB] Migration: added parent_id column to comments table")
	}
	if !hasReplyTo {
		_, _ = DB.Exec("ALTER TABLE comments ADD COLUMN reply_to TEXT NOT NULL DEFAULT ''")
		log.Println("[DB] Migration: added reply_to column to comments table")
	}
	if !hasAvatar {
		_, _ = DB.Exec("ALTER TABLE comments ADD COLUMN author_avatar TEXT NOT NULL DEFAULT ''")
		log.Println("[DB] Migration: added author_avatar column to comments table")
	}
	return nil
}

// migrateMenuFlags 为 menus 表添加 is_system / visible 列
func migrateMenuFlags() error {
	colRows, err := DB.Query("PRAGMA table_info(menus)")
	if err != nil {
		return err
	}
	hasSystem := false
	hasVisible := false
	for colRows.Next() {
		var cid int
		var cname, ctype string
		var notnull int
		var dfltValue interface{}
		var pk int
		_ = colRows.Scan(&cid, &cname, &ctype, &notnull, &dfltValue, &pk)
		if cname == "is_system" {
			hasSystem = true
		}
		if cname == "visible" {
			hasVisible = true
		}
	}
	colRows.Close()
	if !hasSystem {
		if _, err := DB.Exec("ALTER TABLE menus ADD COLUMN is_system INTEGER NOT NULL DEFAULT 0"); err != nil {
			return fmt.Errorf("add is_system: %w", err)
		}
		log.Println("[DB] Migration: added is_system column to menus table")
	}
	if !hasVisible {
		if _, err := DB.Exec("ALTER TABLE menus ADD COLUMN visible INTEGER NOT NULL DEFAULT 1"); err != nil {
			return fmt.Errorf("add visible: %w", err)
		}
		log.Println("[DB] Migration: added visible column to menus table")
	}
	return nil
}

// migratePostLikesUnique 迁移 post_likes 表的 UNIQUE 约束
// 旧约束为 UNIQUE(post_id, visitor_id, type)，允许同一访客同时 like + dislike
// 新约束为 UNIQUE(post_id, visitor_id)，同一访客只能有一条记录
func migratePostLikesUnique() error {
	// 检查当前表的 UNIQUE 约束是否包含 type
	// 通过尝试插入冲突数据来检测，或通过 PRAGMA index_list 检查
	// 更简单的方式：直接检查 sqlite_master 中的 CREATE TABLE 语句
	var createSQL string
	err := DB.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='post_likes'").Scan(&createSQL)
	if err != nil {
		return nil // 表不存在，createTables 会创建正确的版本
	}

	// 如果旧表包含 UNIQUE(post_id, visitor_id, type)，需要迁移
	if !strings.Contains(createSQL, "UNIQUE(post_id, visitor_id, type)") {
		return nil // 已经是新 schema
	}

	log.Println("[DB] Migrating post_likes table: UNIQUE(post_id, visitor_id, type) -> UNIQUE(post_id, visitor_id)")

	tx, err := DB.Begin()
	if err != nil {
		return fmt.Errorf("begin post_likes migration: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`CREATE TABLE post_likes_new (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		post_id INTEGER NOT NULL,
		visitor_id TEXT NOT NULL DEFAULT '',
		type TEXT NOT NULL DEFAULT 'like' CHECK(type IN ('like','dislike')),
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(post_id, visitor_id)
	)`); err != nil {
		return fmt.Errorf("create post_likes_new: %w", err)
	}

	// 迁移数据：如果同一访客同时有 like 和 dislike，保留 like
	if _, err := tx.Exec(`INSERT OR IGNORE INTO post_likes_new (id, post_id, visitor_id, type, created_at)
		SELECT id, post_id, visitor_id, type, created_at FROM post_likes
		WHERE type = 'like'
		ORDER BY created_at DESC`); err != nil {
		return fmt.Errorf("migrate like records: %w", err)
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO post_likes_new (id, post_id, visitor_id, type, created_at)
		SELECT id, post_id, visitor_id, type, created_at FROM post_likes
		WHERE type = 'dislike'
		ORDER BY created_at DESC`); err != nil {
		return fmt.Errorf("migrate dislike records: %w", err)
	}

	if _, err := tx.Exec("DROP TABLE post_likes"); err != nil {
		return fmt.Errorf("drop old post_likes: %w", err)
	}
	if _, err := tx.Exec("ALTER TABLE post_likes_new RENAME TO post_likes"); err != nil {
		return fmt.Errorf("rename post_likes_new: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit post_likes migration: %w", err)
	}

	log.Println("[DB] post_likes table migrated successfully")
	return nil
}

// seedSystemSettings 预设网站默认配置
func seedSystemSettings() error {
	defaultSettings := map[string]string{
		"site_title":           "UniFlow",
		"site_subtitle":        "记录生活，分享思考",
		"site_url":             "",
		"banner_url":           "",
		"banner_url_posts":     "",
		"banner_url_moments":   "",
		"banner_url_guestbook": "",
		"banner_use_homepage":  "true",
		"sensitive_words":      "",
		"comment_limit_count":  "2",
		"comment_limit_minute": "1",
		"about_me_json": `{
	"name": "HuiNan",
	"avatar": "/static/default-avatar.png",
	"bio": "热爱技术与生活",
	"location": "",
	"social_links": [
		{"name": "QQ", "url": "", "platform": "qq"},
		{"name": "微信", "url": "", "platform": "wechat"},
		{"name": "抖音", "url": "", "platform": "douyin"},
		{"name": "哔哩哔哩", "url": "", "platform": "bilibili"},
		{"name": "小红书", "url": "", "platform": "xiaohongshu"}
	]
}`,
	}

	for key, value := range defaultSettings {
		// INSERT OR IGNORE 避免重复插入
		_, err := DB.Exec(
			`INSERT OR IGNORE INTO system_settings (key, value) VALUES (?, ?)`,
			key, value,
		)
		if err != nil {
			return fmt.Errorf("seed setting '%s' failed: %w", key, err)
		}
	}

	log.Println("[DB] System settings seeded with defaults")
	return nil
}
