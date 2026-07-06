package utils

import (
	"net/url"
	"regexp"
	"strings"

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

// StripHTML 移除 HTML 标签并折叠空白，用于摘要、字数统计和模板纯文本展示。
func StripHTML(s string) string {
	re := regexp.MustCompile(`<[^>]*>`)
	s = re.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	re2 := regexp.MustCompile(`\s+`)
	return re2.ReplaceAllString(s, " ")
}

// SafeURL 校验前台可渲染到 href/src 的 URL。
// 允许站内相对路径、根路径、http/https 绝对地址；拒绝 javascript:、data: 等危险协议。
func SafeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "\x00\r\n\t") {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		return ""
	}
	if strings.HasPrefix(raw, "/") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme == "" && u.Host == "" && !strings.HasPrefix(raw, ".") {
		return raw
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme == "http" || scheme == "https") && u.Host != "" {
		return raw
	}
	return ""
}

// SafeImageURL 校验前台可渲染到 img/video src 的 URL。
// 仅允许站内上传/静态资源，或 https 外链图片；拒绝 data:、javascript:、file: 等危险协议。
func SafeImageURL(raw string) string {
	safe := SafeURL(raw)
	if safe == "" {
		return ""
	}
	if strings.HasPrefix(safe, "/uploads/") || strings.HasPrefix(safe, "/static/") {
		return safe
	}
	u, err := url.Parse(safe)
	if err != nil {
		return ""
	}
	if strings.EqualFold(u.Scheme, "https") && u.Host != "" {
		return safe
	}
	return ""
}

// SafeMenuURL 返回可用于菜单跳转的 URL，非法或空值回退到首页。
func SafeMenuURL(raw string) string {
	if safe := SafeURL(raw); safe != "" {
		return safe
	}
	return "/"
}

// SafeExternalURL 返回适合外链 href 的 URL；非法时返回空字符串。
// 社交链接额外允许 mailto:，但仍拒绝控制字符和空地址。
func SafeExternalURL(raw string) string {
	if safe := SafeURL(raw); safe != "" {
		return safe
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "\x00\r\n	") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if strings.EqualFold(u.Scheme, "mailto") && u.Opaque != "" {
		return raw
	}
	return ""
}
