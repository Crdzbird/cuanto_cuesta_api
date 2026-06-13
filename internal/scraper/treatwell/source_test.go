package treatwell

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crdzbird/cuanto_cuesta/internal/scraper/schemaorg"
)

func TestVenueSlug(t *testing.T) {
	t.Parallel()
	slug, err := venueSlug("https://www.treatwell.es/establecimiento/emi-beauty-studio/")
	if err != nil || slug != "emi-beauty-studio" {
		t.Errorf("venueSlug = %q, %v", slug, err)
	}
	if _, err := venueSlug("https://www.treatwell.es/otra/cosa/"); err == nil {
		t.Error("want error for non-venue URL")
	}
}

// TestParseLiveVenuePage runs the shared parser against a saved real page
// when present (testdata/treatwell_live.html is gitignored).
func TestParseLiveVenuePage(t *testing.T) {
	t.Parallel()
	path := filepath.Join("testdata", "treatwell_live.html")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no live fixture at %s", path)
	}
	pageURL := "https://www.treatwell.es/establecimiento/emi-beauty-studio/"
	l, err := schemaorg.ParseListing(bytes.NewReader(data), pageURL, time.Now())
	if err != nil {
		t.Fatalf("ParseListing(live): %v", err)
	}
	if l.Name == "" {
		t.Error("empty name")
	}
	if l.Latitude == nil || l.Longitude == nil {
		t.Error("missing geo coordinates")
	}
	if l.Rating == nil || l.Rating.ReviewCount == 0 {
		t.Errorf("rating not parsed: %+v", l.Rating)
	}
	// Treatwell nests services in hasOfferCatalog → OfferCatalog → Offer.
	if len(l.Services) == 0 {
		t.Error("no services extracted from hasOfferCatalog")
	}
	// Treatwell has no JSON-LD description: the meta-description fallback
	// must fill it.
	if l.Description == "" {
		t.Error("description not filled from meta description fallback")
	}
	if len(l.Images) < 2 {
		t.Errorf("Images len = %d, want the full gallery", len(l.Images))
	}
	if len(l.Reviews) == 0 {
		t.Error("no sample reviews extracted")
	}
	if l.Payment == "" {
		t.Error("paymentAccepted not extracted")
	}
	// Treatwell publishes structured durations and descriptions per service.
	withDuration, withDescription := 0, 0
	for _, svc := range l.Services {
		if svc.DurationMin != nil {
			withDuration++
		}
		if svc.Description != "" {
			withDescription++
		}
	}
	if withDuration == 0 {
		t.Error("no service durations extracted")
	}
	if withDescription == 0 {
		t.Error("no service descriptions extracted")
	}
}
