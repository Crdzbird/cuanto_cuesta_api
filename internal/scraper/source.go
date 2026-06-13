// Package scraper provides the crawling contract and the polite HTTP
// machinery shared by all site-specific sources.
package scraper

import (
	"context"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
)

// Source is a site-specific scraper. Implementations discover business page
// URLs and turn a single page into a per-source domain.Listing; entity
// resolution into canonical businesses happens downstream in storage.
type Source interface {
	// Name is the stable source identifier stored with every listing.
	Name() string
	// DiscoverURLs returns up to limit business page URLs.
	DiscoverURLs(ctx context.Context, limit int) ([]string, error)
	// FetchListing downloads and parses one business page.
	// UNTRUSTED: everything parsed from the page is external input.
	FetchListing(ctx context.Context, url string) (*domain.Listing, error)
}
