package booksy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureHTML mirrors the structure observed on live Booksy pages on
// 2026-06-12: the JSON-LD script tag carries data-* attributes before type,
// and a BreadcrumbList block follows the business block.
const fixtureHTML = `<!DOCTYPE html><html><head>
<script data-n-head="ssr" data-hid="ld-json-0" type="application/ld+json">
{
 "@context": "https://schema.org",
 "@type": "HairSalon",
 "name": "Forbici Men’s Grooming Atelier",
 "description": "Atelier privado de grooming masculino.",
 "url": "https://booksy.com/es-es/113550_forbici-mens-grooming-atelier_barberia_53009_madrid",
 "image": "https://cdn.example/biz.jpeg",
 "logo": "https://cdn.example/logo.jpeg",
 "address": {"@type": "PostalAddress", "streetAddress": "Calle del Gral. Pardiñas, 114, 28006, Madrid",
   "addressCountry": "es", "addressLocality": "Comunidad de Madrid", "postalCode": "28006"},
 "priceRange": "EUR 5 - 35",
 "sameAs": ["https://www.instagram.com/example/"],
 "geo": {"@type": "GeoCoordinates", "latitude": 40.43634816325855, "longitude": -3.677526824176311},
 "openingHoursSpecification": [
   {"@type": "OpeningHoursSpecification", "dayOfWeek": ["Monday","Tuesday"], "opens": "11:00", "closes": "20:30"},
   {"@type": "OpeningHoursSpecification", "dayOfWeek": "Saturday", "opens": "10:00", "closes": "18:00"}],
 "aggregateRating": {"@type": "AggregateRating", "ratingValue": 5, "reviewCount": 22},
 "makesOffer": [
   {"@type": "Offer", "name": "Corte caballero", "priceCurrency": "EUR", "price": 20},
   {"@type": "Offer", "name": "Rapado de pelo", "priceCurrency": "EUR", "price": "18"}]
}
</script>
<script data-n-head="ssr" data-hid="ld-json-1" type="application/ld+json">
{"@context":"https://schema.org","@type":"BreadcrumbList","itemListElement":[]}
</script>
</head><body></body></html>`

const fixtureURL = "https://booksy.com/es-es/113550_forbici-mens-grooming-atelier_barberia_53009_madrid"

func TestParseListing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	l, err := ParseListing(strings.NewReader(fixtureHTML), fixtureURL, now)
	if err != nil {
		t.Fatalf("ParseListing: %v", err)
	}

	if l.Source != "booksy" || l.SourceID != "113550" {
		t.Errorf("Source/SourceID = %q/%q, want booksy/113550", l.Source, l.SourceID)
	}
	if l.Name != "Forbici Men’s Grooming Atelier" {
		t.Errorf("Name = %q", l.Name)
	}
	if l.Category != "barberia" || l.City != "madrid" {
		t.Errorf("Category/City = %q/%q, want barberia/madrid", l.Category, l.City)
	}
	if l.SchemaType != "HairSalon" {
		t.Errorf("SchemaType = %q", l.SchemaType)
	}
	if l.Latitude == nil || *l.Latitude != 40.43634816325855 {
		t.Errorf("Latitude = %v", l.Latitude)
	}
	if l.Rating == nil || l.Rating.Value != 5 || l.Rating.ReviewCount != 22 {
		t.Errorf("Rating = %+v", l.Rating)
	}
	if l.Address.PostalCode != "28006" {
		t.Errorf("PostalCode = %q", l.Address.PostalCode)
	}
	if len(l.OpeningHours) != 2 {
		t.Fatalf("OpeningHours len = %d, want 2", len(l.OpeningHours))
	}
	// Single-string dayOfWeek must be absorbed into a one-element slice.
	if len(l.OpeningHours[1].Days) != 1 || l.OpeningHours[1].Days[0] != "Saturday" {
		t.Errorf("OpeningHours[1].Days = %v", l.OpeningHours[1].Days)
	}
	if len(l.Services) != 2 {
		t.Fatalf("Services len = %d, want 2", len(l.Services))
	}
	// Numeric-string price must parse via the flexible float.
	if l.Services[1].Price == nil || *l.Services[1].Price != 18 {
		t.Errorf("Services[1].Price = %v", l.Services[1].Price)
	}
	if !l.ScrapedAt.Equal(now) {
		t.Errorf("ScrapedAt = %v, want %v", l.ScrapedAt, now)
	}
}

// TestParseListingLivePage runs the parser against a real saved page when
// present (testdata/booksy_live.html is gitignored, refreshed manually).
func TestParseListingLivePage(t *testing.T) {
	t.Parallel()
	path := filepath.Join("testdata", "booksy_live.html")
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("no live fixture at %s", path)
	}
	defer func() { _ = f.Close() }()
	l, err := ParseListing(f, fixtureURL, time.Now())
	if err != nil {
		t.Fatalf("ParseListing(live): %v", err)
	}
	if l.Name == "" || l.Latitude == nil || len(l.Services) == 0 {
		t.Errorf("live parse incomplete: name=%q lat=%v services=%d", l.Name, l.Latitude, len(l.Services))
	}
}

func TestParseListingNoJSONLD(t *testing.T) {
	t.Parallel()
	_, err := ParseListing(strings.NewReader("<html><body>nope</body></html>"), fixtureURL, time.Now())
	if err == nil {
		t.Fatal("want error for page without JSON-LD")
	}
}

func TestParseBusinessURLShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		url      string
		wantID   string
		wantCat  string
		wantCity string
		wantErr  bool
	}{
		{"canonical", fixtureURL, "113550", "barberia", "madrid", false},
		{"name with extra underscore", "https://booksy.com/es-es/99_a_b_salon-de-unas_123_vigo", "99", "salon-de-unas", "vigo", false},
		{"too few parts", "https://booksy.com/es-es/badpage", "", "", "", true},
		{"non-numeric id", "https://booksy.com/es-es/abc_x_cat_1_city", "", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			id, cat, city, err := parseBusinessURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if id != tt.wantID || cat != tt.wantCat || city != tt.wantCity {
				t.Errorf("got (%q,%q,%q), want (%q,%q,%q)", id, cat, city, tt.wantID, tt.wantCat, tt.wantCity)
			}
		})
	}
}
