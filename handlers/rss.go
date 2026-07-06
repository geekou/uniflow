package handlers

import (
	"database/sql"
	"encoding/xml"
	"net/http"
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

		rows, err := db.Query(`
			SELECT id, title, content, created_at
			FROM posts
			WHERE status='published' AND privacy='public'
			ORDER BY created_at DESC
			LIMIT 20
		`)
		if err != nil {
			c.String(http.StatusInternalServerError, "error")
			return
		}
		defer rows.Close()

		baseURL := "https://" + c.Request.Host
		if c.Request.TLS == nil {
			baseURL = "http://" + c.Request.Host
		}

		var items []RSSItem
		for rows.Next() {
			var id int64
			var title, content string
			var createdAt time.Time
			rows.Scan(&id, &title, &content, &createdAt)

			desc := content
			if len([]rune(desc)) > 300 {
				desc = string([]rune(desc)[:300]) + "..."
			}

			items = append(items, RSSItem{
				Title:       title,
				Link:        baseURL + "/post/" + itoa(id),
				Description: desc,
				PubDate:     createdAt.Format(time.RFC1123Z),
				GUID:        baseURL + "/post/" + itoa(id),
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
				Items:       items,
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
