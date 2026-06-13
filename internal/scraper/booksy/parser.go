package booksy

import (
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
	"github.com/crdzbird/cuanto_cuesta/internal/scraper/schemaorg"
)

// ParseListing extracts a domain.Listing from a Booksy business page: the
// shared schema.org parser does the heavy lifting, then Booksy's URL slug
// supplies the source ID, category, and city.
func ParseListing(pageHTML io.Reader, pageURL string, now time.Time) (*domain.Listing, error) {
	l, err := schemaorg.ParseListing(pageHTML, pageURL, now)
	if err != nil {
		return nil, err
	}
	id, category, city, err := parseBusinessURL(pageURL)
	if err != nil {
		return nil, err
	}
	l.Source = sourceName
	l.SourceID = id
	l.Category = category
	l.City = city
	return l, nil
}

// parseBusinessURL splits a Booksy business URL into its slug components.
// Shape: https://booksy.com/es-es/113550_forbici-mens-grooming-atelier_barberia_53009_madrid
// → id "113550", category "barberia", city "madrid".
// ASSUMES: '_' separates slug fields and never appears inside one (Booksy
// uses '-' inside fields); name may span several parts defensively.
func parseBusinessURL(rawURL string) (id, category, city string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", "", fmt.Errorf("parse business url %q: %w", rawURL, err)
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	last := segs[len(segs)-1]
	parts := strings.Split(last, "_")
	if len(parts) < 5 {
		return "", "", "", fmt.Errorf("unexpected business url shape: %q", rawURL)
	}
	id = parts[0]
	if _, convErr := strconv.Atoi(id); convErr != nil {
		return "", "", "", fmt.Errorf("non-numeric business id in url %q", rawURL)
	}
	return id, parts[len(parts)-3], parts[len(parts)-1], nil
}
