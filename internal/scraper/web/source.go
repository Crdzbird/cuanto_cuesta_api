// Package web is the generic "rest of the internet" source: given seed URLs
// (a business's own website, a directory page, any page carrying schema.org
// LocalBusiness markup), it extracts a listing with no site-specific code.
// Each host's robots.txt and crawl-delay are enforced by the fetcher.
package web

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
	"github.com/crdzbird/cuanto_cuesta/internal/match"
	"github.com/crdzbird/cuanto_cuesta/internal/scraper"
	"github.com/crdzbird/cuanto_cuesta/internal/scraper/schemaorg"
)

const sourceName = "web"

// Source scrapes arbitrary seed URLs for schema.org business markup.
type Source struct {
	fetcher *scraper.Fetcher
	seeds   []string
	now     func() time.Time
}

// New builds a web source over a seed URL list (one business page per URL).
func New(fetcher *scraper.Fetcher, seeds []string) *Source {
	return &Source{fetcher: fetcher, seeds: seeds, now: time.Now}
}

// Name implements scraper.Source.
func (s *Source) Name() string { return sourceName }

// DiscoverURLs implements scraper.Source: the seed list, capped at limit.
func (s *Source) DiscoverURLs(_ context.Context, limit int) ([]string, error) {
	if len(s.seeds) <= limit {
		return s.seeds, nil
	}
	return s.seeds[:limit], nil
}

// FetchListing implements scraper.Source.
func (s *Source) FetchListing(ctx context.Context, pageURL string) (*domain.Listing, error) {
	body, err := s.fetcher.Get(ctx, pageURL)
	if err != nil {
		return nil, err
	}
	return ParsePage(body, pageURL, s.now())
}

// ParsePage converts a fetched page into a "web" listing. It is shared with
// the focused crawler so discovered pages get identical source identity
// (re-crawls update rather than duplicate).
func ParsePage(body []byte, pageURL string, now time.Time) (*domain.Listing, error) {
	l, err := schemaorg.ParseListing(bytes.NewReader(body), pageURL, now)
	if err != nil {
		return nil, err
	}
	id, err := canonicalID(pageURL)
	if err != nil {
		return nil, err
	}
	l.Source = sourceName
	l.SourceID = id
	l.City = match.CityGuess(l.Address)
	return l, nil
}

// canonicalID normalizes a URL into a stable source ID, so re-crawls of
// "https://example.com/page/" and "http://example.com/page" update the same
// listing instead of duplicating it.
func canonicalID(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse seed url %q: %w", rawURL, err)
	}
	host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	if host == "" {
		return "", fmt.Errorf("seed url missing host: %q", rawURL)
	}
	return host + strings.TrimSuffix(u.Path, "/"), nil
}

var _ scraper.Source = (*Source)(nil)
