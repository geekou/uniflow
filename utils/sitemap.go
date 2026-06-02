package utils

import (
	"bytes"
	"database/sql"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// GenerateSitemap 生成 sitemap.xml 站点地图
func GenerateSitemap(db *sql.DB, baseURL, staticDir string) (string, error) {
	// 查询所有已发布的公开文章
	rows, err := db.Query(
		"SELECT id, updated_at FROM posts WHERE status='published' AND privacy='public' ORDER BY updated_at DESC",
	)
	if err != nil {
		return "", fmt.Errorf("query posts: %w", err)
	}
	defer rows.Close()

	type urlEntry struct {
		Loc        string
		ChangeFreq string
		Priority   float64
		LastMod    string
	}

	var entries []urlEntry

	// 首页
	entries = append(entries, urlEntry{
		Loc:        baseURL + "/",
		ChangeFreq: "daily",
		Priority:   1.0,
		LastMod:    time.Now().Format("2006-01-02"),
	})

	// 文章页
	for rows.Next() {
		var id int64
		var updatedAt time.Time
		if err := rows.Scan(&id, &updatedAt); err != nil {
			continue
		}
		entries = append(entries, urlEntry{
			Loc:        fmt.Sprintf("%s/post/%d", baseURL, id),
			ChangeFreq: "weekly",
			Priority:   0.8,
			LastMod:    updatedAt.Format("2006-01-02"),
		})
	}

	// 自定义页面
	pageRows, err := db.Query("SELECT slug, COALESCE(updated_at, created_at) FROM pages ORDER BY updated_at DESC")
	if err != nil {
		return "", fmt.Errorf("query pages: %w", err)
	}
	defer pageRows.Close()
	for pageRows.Next() {
		var slug string
		var updatedAt time.Time
		if err := pageRows.Scan(&slug, &updatedAt); err != nil {
			continue
		}
		entries = append(entries, urlEntry{
			Loc:        fmt.Sprintf("%s/page/%s", baseURL, slug),
			ChangeFreq: "monthly",
			Priority:   0.6,
			LastMod:    updatedAt.Format("2006-01-02"),
		})
	}

	// 生成 XML（对 Loc 等字段做 XML 转义，防止特殊字符破坏 XML 结构）
	var buf []byte
	buf = append(buf, `<?xml version="1.0" encoding="UTF-8"?>`...)
	buf = append(buf, `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`...)
	for _, e := range entries {
		buf = append(buf, "<url><loc>"...)
		var locBuf bytes.Buffer
		xml.EscapeText(&locBuf, []byte(e.Loc))
		buf = append(buf, locBuf.Bytes()...)
		buf = append(buf, "</loc><lastmod>"...)
		buf = append(buf, e.LastMod...)
		buf = append(buf, "</lastmod><changefreq>"...)
		buf = append(buf, e.ChangeFreq...)
		buf = append(buf, fmt.Sprintf("</changefreq><priority>%.1f</priority></url>", e.Priority)...)
	}
	buf = append(buf, `</urlset>`...)

	// 保存文件
	sitemapPath := filepath.Join(staticDir, "sitemap.xml")
	if err := os.WriteFile(sitemapPath, buf, 0644); err != nil {
		return "", fmt.Errorf("write sitemap: %w", err)
	}

	return sitemapPath, nil
}
