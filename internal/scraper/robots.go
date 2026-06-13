package scraper

import (
	"strconv"
	"strings"
)

// Robots is a minimal robots.txt evaluator.
//
// ASSUMES: simplified semantics — it honors the longest-match rule with
// Allow winning ties (Google's documented behavior), supports '*' wildcards
// and the '$' end anchor, and merges all groups whose User-agent matches
// either "*" or our agent token. Crawl-delay and sitemap directives are
// ignored here (rate limiting is handled by Fetcher).
type Robots struct {
	rules []robotsRule
	// CrawlDelay is the largest Crawl-delay (seconds) among groups that
	// apply to our agent; 0 when unspecified.
	CrawlDelay float64
}

type robotsRule struct {
	pattern string
	allow   bool
}

// ParseRobots parses robots.txt content, keeping the groups that apply to
// agentToken (matched case-insensitively as a prefix, per the RFC) or to "*".
func ParseRobots(content, agentToken string) *Robots {
	agentToken = strings.ToLower(agentToken)
	r := &Robots{}
	applies := false
	inAgentBlock := false // consecutive User-agent lines share one group
	for _, line := range strings.Split(content, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		switch key {
		case "user-agent":
			ua := strings.ToLower(val)
			if !inAgentBlock {
				applies = false
				inAgentBlock = true
			}
			if ua == "*" || strings.HasPrefix(agentToken, ua) {
				applies = true
			}
		case "allow", "disallow":
			inAgentBlock = false
			if !applies || val == "" {
				continue // empty Disallow means allow-all; no rule needed
			}
			r.rules = append(r.rules, robotsRule{pattern: val, allow: key == "allow"})
		case "crawl-delay":
			inAgentBlock = false
			if !applies {
				continue
			}
			if d, err := strconv.ParseFloat(val, 64); err == nil && d > r.CrawlDelay {
				r.CrawlDelay = d
			}
		default:
			inAgentBlock = false
		}
	}
	return r
}

// Allowed reports whether the URL path (with query, if any) may be fetched.
func (r *Robots) Allowed(path string) bool {
	if r == nil {
		return true
	}
	bestLen := -1
	allowed := true // no matching rule means allowed
	for _, rule := range r.rules {
		if !matchRobotsPattern(rule.pattern, path) {
			continue
		}
		l := len(rule.pattern)
		if l > bestLen || (l == bestLen && rule.allow && !allowed) {
			bestLen = l
			allowed = rule.allow
		}
	}
	return allowed
}

// matchRobotsPattern matches pattern against path. '*' matches any sequence,
// '$' at the end anchors the match; otherwise the pattern is a prefix match.
func matchRobotsPattern(pattern, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = strings.TrimSuffix(pattern, "$")
	}
	parts := strings.Split(pattern, "*")
	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		if i == 0 {
			if !strings.HasPrefix(path, part) {
				return false
			}
			pos = len(part)
			continue
		}
		idx := strings.Index(path[pos:], part)
		if idx < 0 {
			return false
		}
		pos += idx + len(part)
	}
	if anchored {
		// The last literal part must end exactly at the end of path.
		if parts[len(parts)-1] == "" { // pattern ended with '*': always anchors
			return true
		}
		return pos == len(path)
	}
	return true
}
