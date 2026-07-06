package handlers

import (
	"database/sql"
	"encoding/xml"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
)

type RSS struct {
	XMLName xml.Name `xml:"rss"`
	Version string   `xml:"version,attr"`
	Channel RSSChannel
}

type RSSChannel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Language    string    `xml:"language"`
	LastBuild   string    `xml:"lastBuildDate"`
	Items       []RSSItem `xml:"item"`
}

type RSSItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
	GUID        string `xml:"guid"`
}

func RSSHandler(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		settings, _ := getSiteSettings(db)

		baseURL := "https://" + c.Request.Host
		if c.Request.TLS == nil {
			baseURL = "http://" + c.Request.Host
		}

		type feedItem struct {
			title     string
			link      string
			content   string
			createdAt time.Time
		}
		var items []feedItem

		// 文章
		pRows, err := db.Query(`
			SELECT id, title, content, created_at
			FROM posts
			WHERE status='published' AND privacy='public'
			ORDER BY created_at DESC
			LIMIT 20
		`)
		if err == nil && pRows != nil {
			defer pRows.Close()
			for pRows.Next() {
				var id int64
				var title, content string
				var t time.Time
				pRows.Scan(&id, &title, &content, &t)
				items = append(items, feedItem{
					title:     title,
					link:      baseURL + "/post/" + itoa(id),
					content:   truncateRunes(content, 200),
					createdAt: t,
				})
			}
		}

		// 瞬间
		mRows, err := db.Query(`
			SELECT id, content, created_at
			FROM moments
			ORDER BY created_at DESC
			LIMIT 20
		`)
		if err == nil && mRows != nil {
			defer mRows.Close()
			for mRows.Next() {
				var id int64
				var content string
				var t time.Time
				mRows.Scan(&id, &content, &t)
				items = append(items, feedItem{
					title:     "💬 瞬间",
					link:      baseURL + "/moments#" + itoa(id),
					content:   truncateRunes(content, 200),
					createdAt: t,
				})
			}
		}

		// 按时间降序排列
		sort.Slice(items, func(i, j int) bool {
			return items[i].createdAt.After(items[j].createdAt)
		})

		// 取前 30 条
		if len(items) > 30 {
			items = items[:30]
		}

		var rssItems []RSSItem
		for _, it := range items {
			rssItems = append(rssItems, RSSItem{
				Title:       it.title,
				Link:        it.link,
				Description: it.content,
				PubDate:     it.createdAt.Format(time.RFC1123Z),
				GUID:        it.link,
			})
		}

		rss := RSS{
			Version: "2.0",
			Channel: RSSChannel{
				Title:       settings["site_title"],
				Link:        baseURL,
				Description: settings["site_subtitle"],
				Language:    "zh-CN",
				LastBuild:   time.Now().Format(time.RFC1123Z),
				Items:       rssItems,
			},
		}

		c.Header("Content-Type", "application/xml; charset=utf-8")
		c.XML(http.StatusOK, rss)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "..."
	}
	return s
}
