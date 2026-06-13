package crawl

import (
	"bytes"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// maxLinksPerPage caps link extraction so one pathological page (an
// "all cities" index with tens of thousands of anchors) cannot flood the
// frontier.
const maxLinksPerPage = 200

// skippedExtensions are link targets that are never HTML pages.
var skippedExtensions = []string{
	".jpg", ".jpeg", ".png", ".gif", ".webp", ".svg", ".ico",
	".css", ".js", ".json", ".xml", ".pdf", ".zip", ".gz",
	".mp4", ".mp3", ".webm", ".woff", ".woff2", ".ttf",
}

// extractLinks returns normalized absolute URLs of every followable <a href>
// on the page. UNTRUSTED: the page decides nothing — budgets and robots are
// enforced by the caller and the fetcher.
func extractLinks(body []byte, pageURL string) []string {
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil
	}
	z := html.NewTokenizer(bytes.NewReader(body))
	var out []string
	seen := map[string]bool{}
	for len(out) < maxLinksPerPage {
		tt := z.Next()
		if tt == html.ErrorToken {
			break // EOF or malformed tail: keep what we have
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		name, hasAttr := z.TagName()
		if !bytes.Equal(name, []byte("a")) || !hasAttr {
			continue
		}
		var href string
		for {
			k, v, more := z.TagAttr()
			if bytes.Equal(k, []byte("href")) {
				href = string(v)
			}
			if !more {
				break
			}
		}
		link := resolveLink(base, href)
		if link != "" && !seen[link] {
			seen[link] = true
			out = append(out, link)
		}
	}
	return out
}

// resolveLink turns an href into a normalized absolute URL, or "" when the
// target is not a followable HTML page.
func resolveLink(base *url.URL, href string) string {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") {
		return ""
	}
	u, err := base.Parse(href)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	lowerPath := strings.ToLower(u.Path)
	for _, ext := range skippedExtensions {
		if strings.HasSuffix(lowerPath, ext) {
			return ""
		}
	}
	u.Fragment = ""
	return u.String()
}
