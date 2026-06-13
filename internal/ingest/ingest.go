// Package ingest is the scrape orchestrator shared by the CLI and the API:
// given a repository and a set of options, it builds the configured sources,
// drives discovery + a bounded fetch pool, and stores listings. Keeping this
// here (not in cmd/) lets the HTTP admin endpoint trigger exactly the same
// run the CLI does, with no duplicated logic.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
	"github.com/crdzbird/cuanto_cuesta/internal/external/foursquare"
	"github.com/crdzbird/cuanto_cuesta/internal/external/supabase"
	"github.com/crdzbird/cuanto_cuesta/internal/external/yelp"
	"github.com/crdzbird/cuanto_cuesta/internal/scraper"
	"github.com/crdzbird/cuanto_cuesta/internal/scraper/booksy"
	"github.com/crdzbird/cuanto_cuesta/internal/scraper/crawl"
	"github.com/crdzbird/cuanto_cuesta/internal/scraper/treatwell"
	"github.com/crdzbird/cuanto_cuesta/internal/scraper/web"
)

// UserAgent identifies the crawler to every host it contacts.
const UserAgent = "cuanto-cuesta-prototype/0.2 (personal MVP crawler; +mailto:luisalfonsocb83@gmail.com)"

// Options configures one ingest run. Seeds are passed in directly (the CLI
// reads them from a file; the API takes them from the request body).
type Options struct {
	Sources          []string      // booksy, treatwell, web, crawl
	Country          string        // Booksy sitemap country code
	City             string        // optional city slug filter (Booksy), e.g. "valencia"
	Limit            int           // max businesses per source
	Concurrency      int           // concurrent fetchers
	RPS              float64       // per-host requests/second budget
	RefreshOlderThan time.Duration // re-crawl stale listings instead of discovering
	Seeds            []string      // for web and crawl sources
	CrawlDepth       int
	MaxPages         int
	PerHostCap       int

	// Supabase (external "googlemaps" dataset) — used by the "supabase" source.
	SupabaseURL string
	SupabaseKey string

	// Yelp Fusion — used by the "yelp" source.
	YelpKey          string
	YelpLocation     string   // e.g. "Valencia, Spain"
	YelpCategories   []string // Fusion category aliases; default vets + dry-cleaning
	YelpDetailPhotos bool     // fetch up to 3 photos per business from the detail endpoint

	// Foursquare Places — used by the "foursquare" source.
	FoursquareKey      string
	FoursquareLocation string
	FoursquareQueries  []string
}

func (o Options) withDefaults() Options {
	if o.Country == "" {
		o.Country = "es"
	}
	if o.Limit <= 0 {
		o.Limit = 25
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 2
	}
	if o.RPS <= 0 {
		o.RPS = 0.5
	}
	return o
}

// SourceResult is per-source counters for one run.
type SourceResult struct {
	Saved  int `json:"saved"`
	Failed int `json:"failed"`
}

// Result aggregates a finished run.
type Result struct {
	BySource map[string]SourceResult `json:"by_source"`
	Saved    int                     `json:"saved"`
	Failed   int                     `json:"failed"`
}

// validSources guards against typos before any network work begins.
var validSources = map[string]bool{"booksy": true, "treatwell": true, "web": true, "crawl": true, "supabase": true, "yelp": true, "foursquare": true}

// Run executes one ingest according to opts, writing to repo. It is safe to
// cancel via ctx. Returns aggregated counts.
func Run(ctx context.Context, repo domain.BusinessRepository, opts Options, logger *slog.Logger) (Result, error) {
	opts = opts.withDefaults()
	res := Result{BySource: map[string]SourceResult{}}

	if len(opts.Sources) == 0 {
		return res, errors.New("no sources configured")
	}
	for _, s := range opts.Sources {
		if !validSources[s] {
			return res, fmt.Errorf("unknown source %q (want booksy, treatwell, web, crawl)", s)
		}
	}

	fetcher := scraper.NewFetcher(UserAgent, opts.RPS)

	for _, name := range opts.Sources {
		switch name {
		case "crawl":
			sr, err := runCrawler(ctx, repo, fetcher, opts, logger)
			if err != nil {
				return res, fmt.Errorf("crawl: %w", err)
			}
			res.record("crawl", sr)
		case "supabase":
			sr, err := runSupabase(ctx, repo, opts, logger)
			if err != nil {
				return res, fmt.Errorf("supabase: %w", err)
			}
			res.record("supabase", sr)
		case "yelp":
			sr, err := runYelp(ctx, repo, opts, logger)
			if err != nil {
				return res, fmt.Errorf("yelp: %w", err)
			}
			res.record("yelp", sr)
		case "foursquare":
			sr, err := runFoursquare(ctx, repo, opts, logger)
			if err != nil {
				return res, fmt.Errorf("foursquare: %w", err)
			}
			res.record("foursquare", sr)
		default:
			var src scraper.Source
			switch name {
			case "booksy":
				src = booksy.New(fetcher, opts.Country, opts.City)
			case "treatwell":
				src = treatwell.New(fetcher)
			case "web":
				if len(opts.Seeds) == 0 {
					return res, errors.New("source 'web' requires seed URLs")
				}
				src = web.New(fetcher, opts.Seeds)
			}
			urls, err := discover(ctx, repo, src, opts.Limit, opts.RefreshOlderThan, logger)
			if err != nil {
				return res, fmt.Errorf("%s: %w", name, err)
			}
			sr, err := runSource(ctx, repo, src, urls, opts.Concurrency, logger)
			if err != nil {
				return res, fmt.Errorf("%s: %w", name, err)
			}
			res.record(name, sr)
		}
	}
	return res, nil
}

func (r *Result) record(source string, sr SourceResult) {
	r.BySource[source] = sr
	r.Saved += sr.Saved
	r.Failed += sr.Failed
}

// discover picks the work list: stale stored listings in refresh mode,
// otherwise fresh URLs from the source's discovery (sitemaps, seed lists).
func discover(ctx context.Context, repo domain.BusinessRepository, src scraper.Source, limit int, refresh time.Duration, logger *slog.Logger) ([]string, error) {
	if refresh > 0 {
		stale, err := repo.ListStaleListings(ctx, src.Name(), time.Now().Add(-refresh), limit)
		if err != nil {
			return nil, err
		}
		urls := make([]string, 0, len(stale))
		for _, l := range stale {
			urls = append(urls, l.URL)
		}
		logger.Info("refreshing stale listings", "source", src.Name(), "count", len(urls))
		return urls, nil
	}
	logger.Info("discovering business urls", "source", src.Name(), "limit", limit)
	urls, err := src.DiscoverURLs(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("discover: %w", err)
	}
	logger.Info("discovered", "source", src.Name(), "count", len(urls))
	return urls, nil
}

// runSource fetches every URL through a bounded worker pool and stores
// listings. LEAK-SAFE: producer closes jobs; workers exit on channel close
// or ctx cancellation; errgroup owns and joins every goroutine.
func runSource(ctx context.Context, repo domain.BusinessRepository, src scraper.Source, urls []string, concurrency int, logger *slog.Logger) (SourceResult, error) {
	jobs := make(chan string)
	var saved, failed atomic.Int64

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		defer close(jobs)
		for _, u := range urls {
			select {
			case jobs <- u:
			case <-gctx.Done():
				return gctx.Err()
			}
		}
		return nil
	})
	for range concurrency {
		g.Go(func() error {
			for u := range jobs {
				listing, err := src.FetchListing(gctx, u)
				if err != nil {
					if gctx.Err() != nil {
						return gctx.Err()
					}
					failed.Add(1) // one bad page must not kill the crawl
					logger.Warn("fetch failed", "source", src.Name(), "url", u, "err", err)
					continue
				}
				businessID, err := repo.UpsertListing(gctx, listing)
				if err != nil {
					return fmt.Errorf("store %s/%s: %w", listing.Source, listing.SourceID, err)
				}
				saved.Add(1)
				logger.Info("saved", "source", src.Name(), "business_id", businessID,
					"name", listing.Name, "city", listing.City)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return SourceResult{}, err
	}
	sr := SourceResult{Saved: int(saved.Load()), Failed: int(failed.Load())}
	logger.Info("crawl complete", "source", src.Name(), "saved", sr.Saved, "failed", sr.Failed)
	return sr, nil
}

// runCrawler drives the focused BFS crawler, storing every business it finds.
func runCrawler(ctx context.Context, repo domain.BusinessRepository, fetcher *scraper.Fetcher, opts Options, logger *slog.Logger) (SourceResult, error) {
	if len(opts.Seeds) == 0 {
		return SourceResult{}, errors.New("source 'crawl' requires seed URLs")
	}
	var failed atomic.Int64
	stats, err := crawl.Run(ctx, fetcher, opts.Seeds, crawl.Options{
		MaxDepth:    opts.CrawlDepth,
		MaxPages:    opts.MaxPages,
		PerHostCap:  opts.PerHostCap,
		Concurrency: opts.Concurrency,
	}, time.Now, func(l *domain.Listing) error {
		businessID, err := repo.UpsertListing(ctx, l)
		if err != nil {
			return fmt.Errorf("store %s/%s: %w", l.Source, l.SourceID, err)
		}
		logger.Info("saved", "source", "crawl", "business_id", businessID, "name", l.Name, "city", l.City)
		return nil
	}, logger)
	if err != nil {
		return SourceResult{}, err
	}
	failed.Add(int64(stats.FetchFailures))
	logger.Info("crawl complete", "source", "crawl",
		"pages", stats.PagesFetched, "failures", stats.FetchFailures, "businesses", stats.Businesses)
	return SourceResult{Saved: stats.Businesses, Failed: stats.FetchFailures}, nil
}

// runSupabase pulls the external (Google Maps) dataset from Supabase and
// syncs each record: entity-resolved into the catalog with its external
// detail attached. Businesses found only here are flagged Unknown downstream.
func runSupabase(ctx context.Context, repo domain.BusinessRepository, opts Options, logger *slog.Logger) (SourceResult, error) {
	if opts.SupabaseURL == "" || opts.SupabaseKey == "" {
		return SourceResult{}, errors.New("source 'supabase' requires SupabaseURL and SupabaseKey")
	}
	client := supabase.New(opts.SupabaseURL, opts.SupabaseKey)
	externals, err := client.FetchExternals(ctx, opts.Limit)
	if err != nil {
		return SourceResult{}, err
	}
	logger.Info("fetched externals", "source", "supabase", "count", len(externals))

	var sr SourceResult
	for i := range externals {
		if err := ctx.Err(); err != nil {
			return sr, err
		}
		businessID, err := repo.SyncExternal(ctx, &externals[i])
		if err != nil {
			sr.Failed++
			logger.Warn("sync external failed", "name", externals[i].Name, "err", err)
			continue
		}
		sr.Saved++
		logger.Info("synced external", "source", "supabase", "business_id", businessID, "name", externals[i].Name)
	}
	return sr, nil
}

// runYelp pulls businesses from the Yelp Fusion API and syncs each as an
// external record (entity-resolved into the catalog, with detail attached).
func runYelp(ctx context.Context, repo domain.BusinessRepository, opts Options, logger *slog.Logger) (SourceResult, error) {
	if opts.YelpKey == "" {
		return SourceResult{}, errors.New("source 'yelp' requires YelpKey")
	}
	client := yelp.New(opts.YelpKey, opts.YelpLocation)
	externals, err := client.FetchExternals(ctx, opts.YelpCategories, opts.Limit, opts.YelpDetailPhotos)
	if err != nil {
		return SourceResult{}, err
	}
	logger.Info("fetched yelp businesses", "source", "yelp", "count", len(externals))

	var sr SourceResult
	for i := range externals {
		if err := ctx.Err(); err != nil {
			return sr, err
		}
		businessID, err := repo.SyncExternal(ctx, &externals[i])
		if err != nil {
			sr.Failed++
			logger.Warn("sync yelp business failed", "name", externals[i].Name, "err", err)
			continue
		}
		sr.Saved++
		logger.Info("synced yelp business", "source", "yelp", "business_id", businessID, "name", externals[i].Name)
	}
	return sr, nil
}

// runFoursquare pulls businesses from the Foursquare Places API and syncs
// each as an external record (entity-resolved into the catalog with photos,
// ratings and price tiers attached).
func runFoursquare(ctx context.Context, repo domain.BusinessRepository, opts Options, logger *slog.Logger) (SourceResult, error) {
	if opts.FoursquareKey == "" {
		return SourceResult{}, errors.New("source 'foursquare' requires FoursquareKey")
	}
	client := foursquare.New(opts.FoursquareKey, opts.FoursquareLocation)
	externals, err := client.FetchExternals(ctx, opts.FoursquareQueries, opts.Limit)
	if err != nil {
		return SourceResult{}, err
	}
	logger.Info("fetched foursquare businesses", "source", "foursquare", "count", len(externals))

	var sr SourceResult
	for i := range externals {
		if err := ctx.Err(); err != nil {
			return sr, err
		}
		businessID, err := repo.SyncExternal(ctx, &externals[i])
		if err != nil {
			sr.Failed++
			logger.Warn("sync foursquare business failed", "name", externals[i].Name, "err", err)
			continue
		}
		sr.Saved++
		logger.Info("synced foursquare business", "source", "foursquare", "business_id", businessID, "name", externals[i].Name)
	}
	return sr, nil
}

// SourceList parses a comma-separated source string into a clean slice.
func SourceList(s string) []string {
	var out []string
	for _, name := range strings.Split(s, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}
