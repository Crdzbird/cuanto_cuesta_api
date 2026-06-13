package scraper

import "testing"

// Rules drawn from booksy.com/robots.txt as observed on 2026-06-12.
const booksyRobots = `User-agent: *
Disallow: /biz-app/
Disallow: /pro/*/onboarding/Login?*
Allow: /pro/*/onboarding/Login
Disallow: /pro/
Disallow: /search/
Disallow: /*?address=
Disallow: /en-gb/s/*/940155_crawley*
Disallow: /api/

User-agent: evilbot
Disallow: /
`

func TestRobotsAllowed(t *testing.T) {
	t.Parallel()
	r := ParseRobots(booksyRobots, "cuanto-cuesta-prototype")
	tests := []struct {
		path string
		want bool
	}{
		{"/es-es/113550_forbici_barberia_53009_madrid", true},
		{"/sitemap/sitemap_index.xml", true},
		{"/search/anything", false},
		{"/api/v2/whatever", false},
		{"/biz-app/x", false},
		{"/es-es/page?address=foo", false},
		{"/en-gb/s/hair/940155_crawley-west", false},
		{"/pro/123/onboarding/Login", true},          // Allow beats Disallow on specificity
		{"/pro/123/onboarding/Login?next=x", false},  // query variant disallowed (longer match)
		{"/pro/123/dashboard", false},
		{"/", true},
	}
	for _, tt := range tests {
		if got := r.Allowed(tt.path); got != tt.want {
			t.Errorf("Allowed(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestRobotsAgentGroups(t *testing.T) {
	t.Parallel()
	// The evilbot group must not apply to us...
	r := ParseRobots(booksyRobots, "cuanto-cuesta-prototype")
	if !r.Allowed("/es-es/ok") {
		t.Error("wildcard group wrongly blocked an allowed path")
	}
	// ...but it must apply to evilbot.
	evil := ParseRobots(booksyRobots, "evilbot")
	if evil.Allowed("/anything") {
		t.Error("evilbot group not applied to evilbot agent")
	}
}

func TestRobotsCrawlDelay(t *testing.T) {
	t.Parallel()
	// Treatwell-style: delay declared in the wildcard group.
	r := ParseRobots("User-agent: *\nCrawl-delay: 5\nDisallow: /m/\n", "cuanto-cuesta-prototype")
	if r.CrawlDelay != 5 {
		t.Errorf("CrawlDelay = %v, want 5", r.CrawlDelay)
	}
	// A delay in a group for someone else must not apply.
	other := ParseRobots("User-agent: evilbot\nCrawl-delay: 60\n", "cuanto-cuesta-prototype")
	if other.CrawlDelay != 0 {
		t.Errorf("CrawlDelay = %v, want 0", other.CrawlDelay)
	}
}

func TestRobotsNilAllowsAll(t *testing.T) {
	t.Parallel()
	var r *Robots
	if !r.Allowed("/anything") {
		t.Error("nil Robots must allow all (robots not yet loaded)")
	}
}

func TestMatchRobotsPatternAnchor(t *testing.T) {
	t.Parallel()
	if !matchRobotsPattern("/foo$", "/foo") {
		t.Error("anchored exact match failed")
	}
	if matchRobotsPattern("/foo$", "/foobar") {
		t.Error("anchored pattern matched longer path")
	}
	if !matchRobotsPattern("/foo*$", "/foo/bar/baz") {
		t.Error("trailing wildcard with anchor should match any suffix")
	}
}
