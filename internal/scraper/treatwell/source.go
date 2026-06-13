// Package treatwell implements scraper.Source for treatwell.es public venue
// pages. Discovery walks the published sitemap index; venue pages embed
// schema.org JSON-LD inside an @graph wrapper, which the shared parser
// absorbs. Treatwell's robots.txt asks for Crawl-delay: 5 — the fetcher
// honors that automatically.
package treatwell

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
	"github.com/crdzbird/cuanto_cuesta/internal/match"
	"github.com/crdzbird/cuanto_cuesta/internal/scraper"
	"github.com/crdzbird/cuanto_cuesta/internal/scraper/schemaorg"
)

const (
	sourceName = "treatwell"
	siteRoot   = "https://www.treatwell.es"
)

// Source scrapes treatwell.es venue pages.
type Source struct {
	fetcher *scraper.Fetcher
	now     func() time.Time
}

// New builds a Treatwell source.
func New(fetcher *scraper.Fetcher) *Source {
	return &Source{fetcher: fetcher, now: time.Now}
}

// Name implements scraper.Source.
func (s *Source) Name() string { return sourceName }

type sitemapIndex struct {
	Sitemaps []struct {
		Loc string `xml:"loc"`
	} `xml:"sitemap"`
}

type urlSet struct {
	URLs []struct {
		Loc string `xml:"loc"`
	} `xml:"url"`
}

// DiscoverURLs implements scraper.Source, walking site-map-venue-details-*
// files from the sitemap index.
func (s *Source) DiscoverURLs(ctx context.Context, limit int) ([]string, error) {
	body, err := s.fetcher.Get(ctx, siteRoot+"/site-map-index.xml")
	if err != nil {
		return nil, fmt.Errorf("treatwell: fetch sitemap index: %w", err)
	}
	var idx sitemapIndex
	if err := xml.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("treatwell: parse sitemap index: %w", err)
	}
	var urls []string
	for _, sm := range idx.Sitemaps {
		if len(urls) >= limit {
			break
		}
		if !strings.Contains(sm.Loc, "site-map-venue-details") {
			continue
		}
		body, err := s.fetcher.Get(ctx, sm.Loc)
		if err != nil {
			return nil, fmt.Errorf("treatwell: fetch sitemap %s: %w", sm.Loc, err)
		}
		var set urlSet
		if err := xml.Unmarshal(body, &set); err != nil {
			return nil, fmt.Errorf("treatwell: parse sitemap %s: %w", sm.Loc, err)
		}
		for _, u := range set.URLs {
			if len(urls) >= limit {
				break
			}
			if u.Loc != "" {
				urls = append(urls, u.Loc)
			}
		}
	}
	return urls, nil
}

// FetchListing implements scraper.Source.
func (s *Source) FetchListing(ctx context.Context, pageURL string) (*domain.Listing, error) {
	body, err := s.fetcher.Get(ctx, pageURL)
	if err != nil {
		return nil, err
	}
	l, err := schemaorg.ParseListing(bytes.NewReader(body), pageURL, s.now())
	if err != nil {
		return nil, err
	}
	slug, err := venueSlug(pageURL)
	if err != nil {
		return nil, err
	}
	l.Source = sourceName
	l.SourceID = slug
	l.City = match.CityGuess(l.Address)
	return l, nil
}

// venueSlug extracts the venue slug from
// https://www.treatwell.es/establecimiento/<slug>/.
func venueSlug(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse venue url %q: %w", rawURL, err)
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) < 2 || segs[len(segs)-2] != "establecimiento" || segs[len(segs)-1] == "" {
		return "", fmt.Errorf("unexpected venue url shape: %q", rawURL)
	}
	return segs[len(segs)-1], nil
}

var _ scraper.Source = (*Source)(nil)
