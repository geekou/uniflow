package utils

import (
	"github.com/microcosm-cc/bluemonday"
)

// htmlSanitizer 使用 bluemonday UGCPolicy 过滤用户生成的 HTML
var htmlSanitizer = bluemonday.UGCPolicy().
	AllowAttrs("class").OnElements("pre", "code", "span", "div", "table", "th", "td", "tr", "thead", "tbody").
	AllowAttrs("id", "target").OnElements("h1", "h2", "h3", "h4", "h5", "h6").
	AllowAttrs("style").OnElements("span")

// SanitizeHTML 净化 HTML 内容，移除 XSS 向量
func SanitizeHTML(html string) string {
	return htmlSanitizer.Sanitize(html)
}
