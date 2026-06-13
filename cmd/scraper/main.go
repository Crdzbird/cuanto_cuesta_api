// Command scraper crawls business pages from the configured sources and
// stores per-source listings; storage resolves them into canonical
// businesses. Wiring only; orchestration lives in internal/ingest.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/crdzbird/cuanto_cuesta/internal/ingest"
	"github.com/crdzbird/cuanto_cuesta/internal/storage/sqlite"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("scraper failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dbPath      = flag.String("db", "cuanto_cuesta.db", "SQLite database path")
		sourcesFlag = flag.String("sources", "booksy,treatwell", "comma-separated sources: booksy, treatwell, web, crawl")
		country     = flag.String("country", "es", "Booksy sitemap country code")
		city        = flag.String("city", "", "restrict Booksy discovery to one city slug, e.g. valencia (empty = whole country)")
		urlsFile    = flag.String("urls", "", "seed URL file for the 'web'/'crawl' sources (one URL per line)")
		limit       = flag.Int("limit", 25, "max businesses to crawl per source this run")
		concurrency = flag.Int("concurrency", 2, "concurrent page fetchers per source")
		rps         = flag.Float64("rps", 0.5, "per-host sustained requests/second (lowered further by Crawl-delay)")
		refresh     = flag.Duration("refresh-older-than", 0, "re-crawl stored listings older than this (e.g. 720h) instead of discovering new ones")
		crawlDepth  = flag.Int("crawl-depth", 1, "link-following depth for the 'crawl' source")
		maxPages    = flag.Int("max-pages", 50, "overall page budget for the 'crawl' source")
		perHostCap  = flag.Int("per-host-cap", 25, "max pages per host for the 'crawl' source")
		renormalize = flag.Bool("renormalize", false, "recompute all stored businesses with current rules (city normalization) and exit")
		reresolve   = flag.Bool("reresolve", false, "rebuild canonical businesses from stored listings/externals with the current matcher (repairs groupings) and exit")
		supabaseURL = flag.String("supabase-url", os.Getenv("SUPABASE_URL"), "Supabase project URL for the 'supabase' source")
		supabaseKey = flag.String("supabase-key", os.Getenv("SUPABASE_KEY"), "Supabase API key for the 'supabase' source")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	repo, err := sqlite.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()

	if *renormalize {
		n, err := repo.RenormalizeAll(ctx)
		if err != nil {
			return fmt.Errorf("renormalize: %w", err)
		}
		logger.Info("renormalized businesses", "count", n)
		return nil
	}

	if *reresolve {
		n, err := repo.Reresolve(ctx)
		if err != nil {
			return fmt.Errorf("reresolve: %w", err)
		}
		logger.Info("reresolved businesses", "count", n)
		return nil
	}

	opts := ingest.Options{
		Sources:          ingest.SourceList(*sourcesFlag),
		Country:          *country,
		City:             *city,
		Limit:            *limit,
		Concurrency:      *concurrency,
		RPS:              *rps,
		RefreshOlderThan: *refresh,
		CrawlDepth:       *crawlDepth,
		MaxPages:         *maxPages,
		PerHostCap:       *perHostCap,
		SupabaseURL:      *supabaseURL,
		SupabaseKey:      *supabaseKey,
	}
	if needsSeeds(opts.Sources) {
		seeds, err := readSeedURLs(*urlsFile)
		if err != nil {
			return err
		}
		opts.Seeds = seeds
	}

	_, err = ingest.Run(ctx, repo, opts, logger)
	return err
}

func needsSeeds(sources []string) bool {
	return slices.Contains(sources, "web") || slices.Contains(sources, "crawl")
}

func readSeedURLs(path string) ([]string, error) {
	if path == "" {
		return nil, errors.New("sources 'web'/'crawl' require -urls <file>")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open seed file: %w", err)
	}
	defer func() { _ = f.Close() }()
	var urls []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			urls = append(urls, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read seed file: %w", err)
	}
	return urls, nil
}
