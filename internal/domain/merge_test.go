package domain

import (
	"testing"
	"time"
)

func ptr(v float64) *float64 { return &v }

func TestMergeListingsFreshestWins(t *testing.T) {
	t.Parallel()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fresh := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	stale := Listing{
		Source: "booksy", Name: "Forbici (old name)", City: "madrid",
		Description: "old description", PriceRange: "EUR 5 - 30",
		Rating:    &Rating{Value: 4.0, ReviewCount: 10},
		Services:  []ServiceOffer{{Name: "Corte viejo", Price: ptr(15), Currency: "EUR"}},
		ScrapedAt: old,
	}
	current := Listing{
		Source: "treatwell", Name: "Forbici", City: "",
		PriceRange: "EUR 5 - 35",
		Latitude:   ptr(40.43), Longitude: ptr(-3.67),
		Rating:    &Rating{Value: 5.0, ReviewCount: 40},
		Services:  []ServiceOffer{{Name: "Corte nuevo", Price: ptr(20), Currency: "EUR"}},
		ScrapedAt: fresh,
	}

	b := MergeListings([]Listing{stale, current})

	if b.Name != "Forbici" {
		t.Errorf("Name = %q, want freshest", b.Name)
	}
	if b.PriceRange != "EUR 5 - 35" {
		t.Errorf("PriceRange = %q, want freshest", b.PriceRange)
	}
	// Fields empty on the fresh listing backfill from the stale one.
	if b.City != "madrid" || b.Description != "old description" {
		t.Errorf("backfill failed: city=%q desc=%q", b.City, b.Description)
	}
	// Services come from the freshest listing that has any — never mixed.
	if len(b.Services) != 1 || b.Services[0].Name != "Corte nuevo" {
		t.Errorf("Services = %+v", b.Services)
	}
	if !b.LastVerified.Equal(fresh) {
		t.Errorf("LastVerified = %v", b.LastVerified)
	}
	if len(b.Sources) != 2 || b.Sources[0] != "booksy" || b.Sources[1] != "treatwell" {
		t.Errorf("Sources = %v", b.Sources)
	}
	// Weighted rating: (4.0*10 + 5.0*40) / 50 = 4.8.
	if b.Rating == nil || b.Rating.Value != 4.8 || b.Rating.ReviewCount != 50 {
		t.Errorf("Rating = %+v, want 4.8/50", b.Rating)
	}
}

func TestMergeListingsSameSourceNoDoubleCount(t *testing.T) {
	t.Parallel()
	older := Listing{Source: "booksy", Name: "X", Rating: &Rating{Value: 3, ReviewCount: 100},
		ScrapedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	newer := Listing{Source: "booksy", Name: "X", Rating: &Rating{Value: 5, ReviewCount: 120},
		ScrapedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}

	b := MergeListings([]Listing{older, newer})
	// Only the newest snapshot per source counts.
	if b.Rating == nil || b.Rating.Value != 5 || b.Rating.ReviewCount != 120 {
		t.Errorf("Rating = %+v, want 5/120", b.Rating)
	}
}

func TestMergeListingsReviewsAndPrices(t *testing.T) {
	t.Parallel()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fresh := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	booksyOld := Listing{Source: "booksy", Name: "X", ScrapedAt: old,
		Reviews: []Review{{Author: "Stale", Date: "2025-12-01"}}}
	booksyNew := Listing{Source: "booksy", Name: "X", ScrapedAt: fresh,
		Reviews: []Review{{Author: "Lucas", Date: "2026-05-29", Body: "Excelente"}},
		Services: []ServiceOffer{
			{Name: "Corte", Price: ptr(20), Currency: "EUR"},
			{Name: "Barba", Price: ptr(10), Currency: "EUR"},
			{Name: "Consulta", Currency: "EUR"}, // no price: ignored in summary
		}}
	treatwell := Listing{Source: "treatwell", Name: "X", ScrapedAt: fresh,
		Reviews: []Review{{Author: "Mihaela", Date: "2026-06-10", Body: "Experta!"}}}

	b := MergeListings([]Listing{booksyOld, booksyNew, treatwell})

	// Only the newest snapshot per source contributes reviews; newest first.
	if len(b.Reviews) != 2 {
		t.Fatalf("Reviews len = %d, want 2 (stale snapshot excluded)", len(b.Reviews))
	}
	if b.Reviews[0].Author != "Mihaela" || b.Reviews[0].Source != "treatwell" {
		t.Errorf("Reviews[0] = %+v, want newest with source tag", b.Reviews[0])
	}
	if b.PriceFrom == nil || *b.PriceFrom != 10 || b.PriceTo == nil || *b.PriceTo != 20 {
		t.Errorf("price summary = %v..%v, want 10..20", b.PriceFrom, b.PriceTo)
	}
	if b.PriceCurrency != "EUR" {
		t.Errorf("PriceCurrency = %q, want EUR", b.PriceCurrency)
	}
	// No source published a price range: derive one from the menu.
	if b.PriceRange != "EUR 10 - 20" {
		t.Errorf("PriceRange = %q, want derived \"EUR 10 - 20\"", b.PriceRange)
	}
}

func TestMergeListingsKeepsSourcePriceRange(t *testing.T) {
	t.Parallel()
	b := MergeListings([]Listing{{
		Source: "booksy", Name: "X", PriceRange: "EUR 5 - 35",
		Services:  []ServiceOffer{{Name: "Corte", Price: ptr(20), Currency: "EUR"}},
		ScrapedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}})
	// A source-published range is authoritative over the derived one.
	if b.PriceRange != "EUR 5 - 35" {
		t.Errorf("PriceRange = %q, want source value preserved", b.PriceRange)
	}
}

func TestPriceSummaryMixedCurrencies(t *testing.T) {
	t.Parallel()
	from, to, cur := priceSummary([]ServiceOffer{
		{Name: "A", Price: ptr(10), Currency: "EUR"},
		{Name: "B", Price: ptr(30), Currency: "USD"},
		{Name: "C", Price: ptr(20), Currency: "EUR"},
	})
	if *from != 10 || *to != 30 {
		t.Errorf("range = %v..%v, want 10..30", *from, *to)
	}
	if cur != "" {
		t.Errorf("currency = %q, want empty for mixed currencies", cur)
	}
}

func TestMergeListingsUnrated(t *testing.T) {
	t.Parallel()
	b := MergeListings([]Listing{{Source: "web", Name: "X", ScrapedAt: time.Now()}})
	if b.Rating != nil {
		t.Errorf("Rating = %+v, want nil", b.Rating)
	}
}

func TestBusinessStale(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)
	fresh := Business{LastVerified: now.Add(-StaleAfter + time.Hour)}
	stale := Business{LastVerified: now.Add(-StaleAfter - time.Hour)}
	if fresh.Stale(now) {
		t.Error("fresh business reported stale")
	}
	if !stale.Stale(now) {
		t.Error("stale business reported fresh")
	}
}
