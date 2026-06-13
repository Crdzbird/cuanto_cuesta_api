package booksy

import (
	"bytes"
	"context"
	"time"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
	"github.com/crdzbird/cuanto_cuesta/internal/scraper"
)

const sourceName = "booksy"

// Source scrapes booksy.com business pages for one country market.
type Source struct {
	fetcher *scraper.Fetcher
	country string // sitemap country code, e.g. "es"
	now     func() time.Time
}

// New builds a Booksy source. Robots enforcement is handled per-host by the
// fetcher on first contact.
func New(fetcher *scraper.Fetcher, country string) *Source {
	return &Source{fetcher: fetcher, country: country, now: time.Now}
}

// Name implements scraper.Source.
func (s *Source) Name() string { return sourceName }

// FetchListing implements scraper.Source.
func (s *Source) FetchListing(ctx context.Context, pageURL string) (*domain.Listing, error) {
	body, err := s.fetcher.Get(ctx, pageURL)
	if err != nil {
		return nil, err
	}
	return ParseListing(bytes.NewReader(body), pageURL, s.now())
}

var _ scraper.Source = (*Source)(nil)
