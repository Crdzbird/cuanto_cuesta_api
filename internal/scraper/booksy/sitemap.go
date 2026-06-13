// Package booksy implements scraper.Source for booksy.com public pages.
//
// Discovery walks the published sitemap index (the politest crawl entry
// point) and collects business page URLs for one country. Parsing reads the
// schema.org JSON-LD block that Booksy embeds in every business page for
// SEO — structured data intended for crawlers, so no fragile CSS selectors.
package booksy

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
)

const siteRoot = "https://booksy.com"

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

// DiscoverURLs returns up to limit business page URLs for s.country, walking
// sitemap_business_* files from the sitemap index in order.
func (s *Source) DiscoverURLs(ctx context.Context, limit int) ([]string, error) {
	body, err := s.fetcher.Get(ctx, siteRoot+"/sitemap/sitemap_index.xml")
	if err != nil {
		return nil, fmt.Errorf("booksy: fetch sitemap index: %w", err)
	}
	var idx sitemapIndex
	if err := xml.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("booksy: parse sitemap index: %w", err)
	}

	countryPath := "/sitemap/" + s.country + "/"
	var urls []string
	for _, sm := range idx.Sitemaps {
		if len(urls) >= limit {
			break
		}
		if !strings.Contains(sm.Loc, countryPath) || !strings.Contains(sm.Loc, "sitemap_business_") {
			continue
		}
		body, err := s.fetcher.Get(ctx, sm.Loc)
		if err != nil {
			return nil, fmt.Errorf("booksy: fetch sitemap %s: %w", sm.Loc, err)
		}
		var set urlSet
		if err := xml.Unmarshal(body, &set); err != nil {
			return nil, fmt.Errorf("booksy: parse sitemap %s: %w", sm.Loc, err)
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
