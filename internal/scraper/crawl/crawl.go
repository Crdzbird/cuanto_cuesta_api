// Package crawl is the discovery arm of the "rest of the internet" source:
// a focused breadth-first crawler. Starting from seed pages (directories,
// category/city listings, a business's own site), it follows links level by
// level and ingests every page that turns out to carry schema.org business
// markup — pages that don't are just link sources.
//
// Politeness is inherited from the Fetcher (per-host robots.txt, rate
// limits, crawl-delay); the crawler adds its own bounds: an overall page
// budget, a per-host cap so one big site cannot monopolize the crawl, and a
// depth limit.
package crawl

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
	"github.com/crdzbird/cuanto_cuesta/internal/scraper"
	"github.com/crdzbird/cuanto_cuesta/internal/scraper/web"
)

// Getter fetches one URL. *scraper.Fetcher satisfies it; tests use a stub.
type Getter interface {
	Get(ctx context.Context, rawURL string) ([]byte, error)
}

// Options bound a crawl. Zero values get safe defaults.
type Options struct {
	MaxDepth    int // link-following depth from the seeds (0 = seeds only)
	MaxPages    int // overall fetch budget
	PerHostCap  int // max pages fetched from any single host
	Concurrency int // concurrent fetches (per-host politeness still applies)
}

func (o Options) withDefaults() Options {
	if o.MaxDepth <= 0 {
		o.MaxDepth = 1
	}
	if o.MaxPages <= 0 {
		o.MaxPages = 50
	}
	if o.PerHostCap <= 0 {
		o.PerHostCap = 25
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 2
	}
	return o
}

// Stats summarizes a finished crawl.
type Stats struct {
	PagesFetched  int
	FetchFailures int
	Businesses    int
}

// Run crawls from seeds and calls emit for every business listing found.
// emit may be called concurrently and must be safe for that.
// LEAK-SAFE: each BFS level is an errgroup that is fully joined before the
// next level starts; no goroutine outlives Run.
func Run(ctx context.Context, g Getter, seeds []string, opts Options,
	now func() time.Time, emit func(*domain.Listing) error, logger *slog.Logger) (Stats, error) {

	opts = opts.withDefaults()
	var stats Stats
	visited := map[string]bool{}
	hostCount := map[string]int{}

	level := normalizeAll(seeds)
	for depth := 0; depth <= opts.MaxDepth && len(level) > 0; depth++ {
		// Select this level's batch single-threaded: dedup + budgets.
		var batch []string
		for _, u := range level {
			if stats.PagesFetched+len(batch) >= opts.MaxPages {
				break
			}
			host := hostOf(u)
			if u == "" || visited[u] || host == "" || hostCount[host] >= opts.PerHostCap {
				continue
			}
			visited[u] = true
			hostCount[host]++
			batch = append(batch, u)
		}
		if len(batch) == 0 {
			break
		}
		logger.Info("crawl level", "depth", depth, "pages", len(batch))

		var mu sync.Mutex // guards next + stats counters below
		var next []string
		gr, gctx := errgroup.WithContext(ctx)
		gr.SetLimit(opts.Concurrency)
		for _, pageURL := range batch {
			gr.Go(func() error {
				body, err := g.Get(gctx, pageURL)
				if err != nil {
					if gctx.Err() != nil {
						return gctx.Err()
					}
					mu.Lock()
					stats.FetchFailures++
					mu.Unlock()
					lvl := slog.LevelWarn
					if errors.Is(err, scraper.ErrDisallowed) {
						lvl = slog.LevelDebug // robots said no: expected, not a failure
					}
					logger.Log(gctx, lvl, "crawl fetch failed", "url", pageURL, "err", err)
					return nil
				}
				mu.Lock()
				stats.PagesFetched++
				mu.Unlock()

				if l, err := web.ParsePage(body, pageURL, now()); err == nil {
					if err := emit(l); err != nil {
						return err // storage failure is fatal
					}
					mu.Lock()
					stats.Businesses++
					mu.Unlock()
					logger.Info("crawl found business", "name", l.Name, "url", pageURL)
				}

				if depth < opts.MaxDepth {
					links := extractLinks(body, pageURL)
					mu.Lock()
					next = append(next, links...)
					mu.Unlock()
				}
				return nil
			})
		}
		if err := gr.Wait(); err != nil {
			return stats, err
		}
		level = next
	}
	return stats, nil
}

func normalizeAll(urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		out = append(out, normalizeURL(u))
	}
	return out
}

// normalizeURL canonicalizes a URL for the visited set: https/http only,
// fragment stripped. Returns "" for anything not crawlable.
func normalizeURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	u.Fragment = ""
	return u.String()
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Host
}
