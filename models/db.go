package models

import (
	"database/sql"
	"fmt"
	"log"

	_ "modernc.org/sqlite"

	"golang.org/x/crypto/bcrypt"
)

var DB *sql.DB

// InitDB 初始化 SQLite 数据库连接并建表
func InitDB(dbPath string) error {
	var err error
	// 启用 WAL 模式以提升并发性能
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000", dbPath)
	DB, err = sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	// 连接池配置
	DB.SetMaxOpenConns(1) // SQLite 单写模式
	DB.SetMaxIdleConns(1)

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

	// 修复旧数据库：comments 表曾有错误的 FK 约束（target_id -> posts.id），
	// 导致瞬间评论无法插入。先尝试插入测试记录，失败则重建表。
	if err = fixCommentsTable(); err != nil {
		return fmt.Errorf("failed to fix comments table: %w", err)
	}

	// 预设默认配置
	if err = seedSystemSettings(); err != nil {
		return fmt.Errorf("failed to seed settings: %w", err)
	}

	// 不再自动创建默认管理员，由首次部署引导页处理
	log.Println("[DB] All tables created and seeded successfully")
	return nil
}

// CloseDB 关闭数据库连接
func CloseDB() {
	if DB != nil {
		_ = DB.Close()
		log.Println("[DB] Database connection closed")
	}
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
	}

	// 创建索引
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_posts_category ON posts(category_id)`,
		`CREATE INDEX IF NOT EXISTS idx_posts_status ON posts(status)`,
		`CREATE INDEX IF NOT EXISTS idx_posts_publish_at ON posts(publish_at)`,
		`CREATE INDEX IF NOT EXISTS idx_posts_is_top ON posts(is_top)`,
		`CREATE INDEX IF NOT EXISTS idx_comments_target ON comments(target_type, target_id)`,
		`CREATE INDEX IF NOT EXISTS idx_moments_created ON moments(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_type ON logs(log_type)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_created ON logs(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_users_username ON users(username)`,
		`CREATE INDEX IF NOT EXISTS idx_guestbook_created ON guestbook(created_at)`,
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

// fixCommentsTable 修复旧数据库中 comments 表的错误外键约束
// 旧版表结构: FOREIGN KEY (target_id) REFERENCES posts(id) 导致瞬间评论无法插入
func fixCommentsTable() error {
	// 尝试插入一条瞬间评论测试（瞬间表通常 id=1 存在）
	_, err := DB.Exec("INSERT INTO comments (target_type, target_id, author, content) VALUES ('moment', 1, '__fix__', '__fix__')")
	if err == nil {
		// 插入成功，删除测试记录，说明表结构正常
		DB.Exec("DELETE FROM comments WHERE author='__fix__'")
		log.Println("[DB] comments table schema OK")
		return nil
	}

	// 插入失败，说明有旧的外键约束，重建 comments 表
	log.Printf("[DB] comments table has old schema (FK constraint), rebuilding: %v\n", err)
	if _, err := DB.Exec("DROP TABLE IF EXISTS comments"); err != nil {
		log.Printf("[DB] WARNING: failed to drop comments table: %v\n", err)
	}
	// 用 db.Exec 直接执行建表（不用驱动包），绕过 IF NOT EXISTS
	if _, err := DB.Exec(`CREATE TABLE comments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		target_type TEXT NOT NULL CHECK(target_type IN ('post','moment')),
		target_id INTEGER NOT NULL,
		author TEXT NOT NULL DEFAULT '',
		content TEXT NOT NULL DEFAULT '',
		image_url TEXT DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		log.Printf("[DB] CRITICAL: failed to recreate comments table: %v\n", err)
		return err
	}
	log.Println("[DB] comments table rebuilt without FK constraint")

	// 验证修复后能否插入
	_, err = DB.Exec("INSERT INTO comments (target_type, target_id, author, content) VALUES ('moment', 1, '__verify__', '__verify__')")
	if err != nil {
		log.Printf("[DB] CRITICAL: comments table rebuild FAILED: %v\n", err)
	} else {
		if _, err := DB.Exec("DELETE FROM comments WHERE author='__verify__'"); err != nil {
			log.Printf("[DB] WARNING: failed to clean up verify row: %v\n", err)
		}
		log.Println("[DB] comments table verified OK after rebuild")
	}
	return nil
}

// seedSystemSettings 预设网站默认配置
func seedSystemSettings() error {
	defaultSettings := map[string]string{
		"site_title":           "UniFlow",
		"site_subtitle":        "记录生活，分享思考",
		"site_url":             "",
		"banner_url":           "",
		"sensitive_words":      "",
		"comment_limit_count":  "2",
		"comment_limit_minute": "1",
		"about_me_json": `{
	"name": "HuiNan",
	"avatar": "/static/default-avatar.png",
	"bio": "热爱技术与生活",
	"location": "",
	"github": "",
	"twitter": "",
	"email": ""
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

// seedAdminUser 预设默认管理员（admin / admin123）
func seedAdminUser() error {
	hash, err := bcrypt.GenerateFromPassword([]byte("admin123"), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("bcrypt hash failed: %w", err)
	}

	_, err = DB.Exec(
		`INSERT OR IGNORE INTO users (username, password, role) VALUES (?, ?, 'admin')`,
		"admin", string(hash),
	)
	if err != nil {
		return fmt.Errorf("seed admin failed: %w", err)
	}

	log.Println("[DB] Default admin user seeded (admin/admin123)")
	return nil
}
