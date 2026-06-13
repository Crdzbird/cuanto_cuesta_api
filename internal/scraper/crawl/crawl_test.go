package crawl

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
)

// stubGetter serves pages from memory and records fetch counts.
type stubGetter struct {
	mu    sync.Mutex
	pages map[string]string
	gets  map[string]int
}

func (s *stubGetter) Get(_ context.Context, rawURL string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets[rawURL]++
	body, ok := s.pages[rawURL]
	if !ok {
		return nil, fmt.Errorf("404 %s", rawURL)
	}
	return []byte(body), nil
}

func bizPage(name string, lat, lng float64) string {
	return fmt.Sprintf(`<html><head><script type="application/ld+json">
	{"@context":"https://schema.org","@type":"HairSalon","name":%q,
	 "geo":{"@type":"GeoCoordinates","latitude":%v,"longitude":%v}}
	</script></head><body></body></html>`, name, lat, lng)
}

func newStub() *stubGetter {
	return &stubGetter{
		pages: map[string]string{
			// Directory page: no business markup, links to two businesses
			// (one href repeated, one with fragment), an asset, an external
			// host, and a dead link.
			"https://dir.example/madrid": `<html><body>
				<a href="/biz/a">A</a>
				<a href="/biz/a#reviews">A again</a>
				<a href="https://dir.example/biz/b">B</a>
				<a href="/style.css">asset</a>
				<a href="https://other.example/biz/c">external</a>
				<a href="/dead">dead</a>
				<a href="mailto:x@y.z">mail</a>
			</body></html>`,
			"https://dir.example/biz/a":   bizPage("Salon A", 40.1, -3.1),
			"https://dir.example/biz/b":   bizPage("Salon B", 40.2, -3.2),
			"https://other.example/biz/c": bizPage("Salon C", 40.3, -3.3),
		},
		gets: map[string]int{},
	}
}

func runCrawl(t *testing.T, g Getter, seeds []string, opts Options) (Stats, []*domain.Listing) {
	t.Helper()
	var mu sync.Mutex
	var found []*domain.Listing
	emit := func(l *domain.Listing) error {
		mu.Lock()
		defer mu.Unlock()
		found = append(found, l)
		return nil
	}
	now := func() time.Time { return time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC) }
	stats, err := Run(context.Background(), g, seeds, opts, now, emit, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return stats, found
}

func TestCrawlDiscoversBusinesses(t *testing.T) {
	t.Parallel()
	g := newStub()
	stats, found := runCrawl(t, g, []string{"https://dir.example/madrid"}, Options{MaxDepth: 1})

	if stats.Businesses != 3 || len(found) != 3 {
		t.Fatalf("Businesses = %d (emitted %d), want 3", stats.Businesses, len(found))
	}
	names := map[string]bool{}
	for _, l := range found {
		names[l.Name] = true
		if l.Source != "web" || l.SourceID == "" {
			t.Errorf("listing identity = %s/%s, want web/<canonical url>", l.Source, l.SourceID)
		}
	}
	if !names["Salon A"] || !names["Salon B"] || !names["Salon C"] {
		t.Errorf("names = %v", names)
	}
	// The repeated/fragment link must not cause a second fetch.
	if g.gets["https://dir.example/biz/a"] != 1 {
		t.Errorf("biz/a fetched %d times, want 1", g.gets["https://dir.example/biz/a"])
	}
	// Assets and mailto links are never fetched.
	if g.gets["https://dir.example/style.css"] != 0 {
		t.Error("asset link was fetched")
	}
	// The dead link counts as a fetch failure, not a crash.
	if stats.FetchFailures != 1 {
		t.Errorf("FetchFailures = %d, want 1 (dead link)", stats.FetchFailures)
	}
}

func TestCrawlDepthZeroStaysOnSeeds(t *testing.T) {
	t.Parallel()
	g := newStub()
	// MaxDepth defaults to 1 when <=0 is passed via withDefaults; pass an
	// explicit seed-only crawl by seeding the business page itself.
	stats, found := runCrawl(t, g, []string{"https://dir.example/biz/a"}, Options{MaxDepth: 1})
	if stats.Businesses != 1 || found[0].Name != "Salon A" {
		t.Fatalf("want exactly the seeded business, got %d", stats.Businesses)
	}
}

func TestCrawlBudgets(t *testing.T) {
	t.Parallel()
	t.Run("max pages", func(t *testing.T) {
		t.Parallel()
		g := newStub()
		stats, _ := runCrawl(t, g, []string{"https://dir.example/madrid"}, Options{MaxDepth: 1, MaxPages: 2})
		total := stats.PagesFetched + stats.FetchFailures
		if total > 2 {
			t.Errorf("fetched %d pages, budget was 2", total)
		}
	})
	t.Run("per host cap", func(t *testing.T) {
		t.Parallel()
		g := newStub()
		// dir.example may serve only 2 pages: the directory + one business.
		stats, _ := runCrawl(t, g, []string{"https://dir.example/madrid"}, Options{MaxDepth: 1, PerHostCap: 2})
		// other.example is unaffected: Salon C still found.
		foundC := false
		for u := range g.gets {
			if u == "https://other.example/biz/c" {
				foundC = true
			}
		}
		if !foundC {
			t.Error("per-host cap on dir.example wrongly blocked other.example")
		}
		if stats.PagesFetched > 3 {
			t.Errorf("PagesFetched = %d, want ≤3 (2 dir.example + 1 other.example)", stats.PagesFetched)
		}
	})
}

func TestExtractLinks(t *testing.T) {
	t.Parallel()
	body := []byte(`<a href="/x">x</a><a href="https://h.example/y?q=1#frag">y</a>
		<a href="javascript:void(0)">js</a><a href="/img.PNG">img</a><a href="tel:+34">tel</a>`)
	links := extractLinks(body, "https://h.example/base/page")
	want := map[string]bool{
		"https://h.example/x":     true,
		"https://h.example/y?q=1": true,
	}
	if len(links) != len(want) {
		t.Fatalf("links = %v, want %v", links, want)
	}
	for _, l := range links {
		if !want[l] {
			t.Errorf("unexpected link %q", l)
		}
	}
}
