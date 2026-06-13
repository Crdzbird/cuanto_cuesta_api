package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
)

func openTestRepo(t *testing.T) *Repo {
	t.Helper()
	repo, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func ptr[T any](v T) *T { return &v }

func sampleListing(source, sourceID, name, city string, lat, lng, rating float64, reviews int, at time.Time) *domain.Listing {
	return &domain.Listing{
		Source: source, SourceID: sourceID,
		URL:  "https://" + source + ".example/" + sourceID,
		Name: name, Category: "barberia", SchemaType: "HairSalon",
		Description: "desc from " + source, City: city,
		Address:  domain.Address{Street: "Calle X", PostalCode: "28006", Country: "es"},
		Latitude: ptr(lat), Longitude: ptr(lng),
		PriceRange: "EUR 5 - 35",
		Rating:     &domain.Rating{Value: rating, ReviewCount: reviews},
		SocialLinks: []string{"https://instagram.com/" + source},
		OpeningHours: []domain.OpeningHours{{Days: []string{"Monday"}, Opens: "09:00", Closes: "18:00"}},
		Services: []domain.ServiceOffer{
			{Name: "Corte " + source, Price: ptr(20.0), Currency: "EUR"},
		},
		ScrapedAt: at,
	}
}

func TestUpsertListingRoundTrip(t *testing.T) {
	t.Parallel()
	repo := openTestRepo(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)

	l := sampleListing("booksy", "113550", "Forbici", "madrid", 40.4363, -3.6775, 5, 22, at)
	id, err := repo.UpsertListing(ctx, l)
	if err != nil {
		t.Fatalf("UpsertListing: %v", err)
	}

	got, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "Forbici" || got.City != "madrid" {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if got.Rating == nil || got.Rating.Value != 5 || got.Rating.ReviewCount != 22 {
		t.Errorf("Rating = %+v", got.Rating)
	}
	if len(got.Services) != 1 || len(got.Listings) != 1 {
		t.Fatalf("services=%d listings=%d, want 1/1", len(got.Services), len(got.Listings))
	}
	if got.Listings[0].Source != "booksy" || !got.Listings[0].ScrapedAt.Equal(at) {
		t.Errorf("listing provenance = %+v", got.Listings[0])
	}
	if got.Sources[0] != "booksy" || !got.LastVerified.Equal(at) {
		t.Errorf("sources=%v last_verified=%v", got.Sources, got.LastVerified)
	}

	// Re-upsert same (source, source_id): update in place, same business.
	l.Name = "Forbici 2"
	id2, err := repo.UpsertListing(ctx, l)
	if err != nil {
		t.Fatalf("second UpsertListing: %v", err)
	}
	if id2 != id {
		t.Fatalf("re-upsert created new business: %d != %d", id2, id)
	}
	got, _ = repo.GetByID(ctx, id)
	if got.Name != "Forbici 2" || len(got.Listings) != 1 {
		t.Errorf("update not applied: name=%q listings=%d", got.Name, len(got.Listings))
	}
}

func TestCrossSourceEntityResolution(t *testing.T) {
	t.Parallel()
	repo := openTestRepo(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	fresh := time.Now().UTC().Truncate(time.Second)

	// Same venue seen by two sources: ~30 m apart, name variants.
	booksyID, err := repo.UpsertListing(ctx,
		sampleListing("booksy", "113550", "Forbici Men's Grooming Atelier", "madrid",
			40.43634, -3.67752, 5, 22, old))
	if err != nil {
		t.Fatalf("booksy upsert: %v", err)
	}
	twID, err := repo.UpsertListing(ctx,
		sampleListing("treatwell", "forbici", "Forbici", "madrid",
			40.43660, -3.67760, 4.8, 100, fresh))
	if err != nil {
		t.Fatalf("treatwell upsert: %v", err)
	}
	if booksyID != twID {
		t.Fatalf("same venue not merged: booksy=%d treatwell=%d", booksyID, twID)
	}

	got, err := repo.GetByID(ctx, booksyID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(got.Sources) != 2 || len(got.Listings) != 2 {
		t.Fatalf("sources=%v listings=%d, want 2 sources / 2 listings", got.Sources, len(got.Listings))
	}
	// Freshest listing (treatwell) wins the name.
	if got.Name != "Forbici" {
		t.Errorf("Name = %q, want freshest source's name", got.Name)
	}
	// Weighted rating: (5*22 + 4.8*100) / 122 ≈ 4.836.
	if got.Rating == nil || got.Rating.ReviewCount != 122 {
		t.Errorf("Rating = %+v, want 122 combined reviews", got.Rating)
	}
	if !got.LastVerified.Equal(fresh) {
		t.Errorf("LastVerified = %v, want %v", got.LastVerified, fresh)
	}

	// A different venue nearby must NOT merge.
	otherID, err := repo.UpsertListing(ctx,
		sampleListing("treatwell", "monaco", "Barbería Mónaco", "madrid",
			40.43700, -3.67800, 4.9, 50, fresh))
	if err != nil {
		t.Fatalf("other venue upsert: %v", err)
	}
	if otherID == booksyID {
		t.Error("different venue wrongly merged into existing business")
	}
}

func TestGetByIDNotFound(t *testing.T) {
	t.Parallel()
	repo := openTestRepo(t)
	if _, err := repo.GetByID(context.Background(), 99999); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want domain.ErrNotFound", err)
	}
}

func TestListFilters(t *testing.T) {
	t.Parallel()
	repo := openTestRepo(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)

	seed := []*domain.Listing{
		sampleListing("booksy", "1", "Forbici", "madrid", 40.4363, -3.6775, 5, 10, at),
		sampleListing("booksy", "2", "Bella Donna", "vigo", 42.2406, -8.7207, 4.2, 10, at),
		sampleListing("booksy", "3", "BCN Cuts", "barcelona", 41.3874, 2.1686, 3.8, 10, at),
	}
	seed[1].Category = "salon-de-unas"
	for _, l := range seed {
		if _, err := repo.UpsertListing(ctx, l); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	t.Run("category", func(t *testing.T) {
		_, total, err := repo.List(ctx, domain.ListFilter{Category: "barberia"})
		if err != nil {
			t.Fatal(err)
		}
		if total != 2 {
			t.Errorf("total=%d, want 2", total)
		}
	})

	t.Run("min rating orders by rating desc", func(t *testing.T) {
		items, _, err := repo.List(ctx, domain.ListFilter{MinRating: 4})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 2 || items[0].Name != "Forbici" || items[1].Name != "Bella Donna" {
			t.Errorf("unexpected items: %+v", items)
		}
	})

	t.Run("name substring", func(t *testing.T) {
		items, _, err := repo.List(ctx, domain.ListFilter{Query: "bella"})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].Name != "Bella Donna" {
			t.Errorf("unexpected items: %+v", items)
		}
	})

	t.Run("like wildcards escaped", func(t *testing.T) {
		_, total, err := repo.List(ctx, domain.ListFilter{Query: "%"})
		if err != nil {
			t.Fatal(err)
		}
		if total != 0 {
			t.Errorf("literal %% matched %d rows, want 0", total)
		}
	})

	t.Run("geo radius around madrid", func(t *testing.T) {
		items, total, err := repo.List(ctx, domain.ListFilter{
			Geo: &domain.GeoFilter{Lat: 40.4168, Lng: -3.7038, RadiusKm: 10},
		})
		if err != nil {
			t.Fatal(err)
		}
		if total != 1 || len(items) != 1 || items[0].Name != "Forbici" {
			t.Errorf("geo filter: total=%d items=%+v", total, items)
		}
	})

	t.Run("pagination", func(t *testing.T) {
		items, total, err := repo.List(ctx, domain.ListFilter{Limit: 2, Offset: 2})
		if err != nil {
			t.Fatal(err)
		}
		if total != 3 || len(items) != 1 {
			t.Errorf("total=%d len=%d, want 3/1", total, len(items))
		}
	})
}

func TestDemoFlags(t *testing.T) {
	t.Parallel()
	// Deterministic and non-degenerate for both flags.
	for _, tc := range []struct {
		name string
		fn   func(int64) bool
	}{
		{"sponsored", demoSponsored},
		{"verified", demoVerified},
	} {
		flagged := 0
		for id := int64(1); id <= 100; id++ {
			first := tc.fn(id)
			if tc.fn(id) != first {
				t.Fatalf("%s(%d) not deterministic", tc.name, id)
			}
			if first {
				flagged++
			}
		}
		if flagged == 0 || flagged == 100 {
			t.Fatalf("%s distribution degenerate: %d/100", tc.name, flagged)
		}
	}
	// The two flags are independent: they disagree for at least some ids.
	disagree := 0
	for id := int64(1); id <= 100; id++ {
		if demoSponsored(id) != demoVerified(id) {
			disagree++
		}
	}
	if disagree == 0 {
		t.Fatal("sponsored and verified are perfectly correlated; salt is ineffective")
	}
}

func TestSponsoredStoredAndOrderedFirst(t *testing.T) {
	t.Parallel()
	repo := openTestRepo(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)

	// Seed enough businesses that both sponsored and non-sponsored exist.
	for i := range 12 {
		l := sampleListing("booksy", "b"+strconv.Itoa(i), "Biz "+strconv.Itoa(i),
			"madrid", 40.0+float64(i)/1000, -3.0, 4.0, 5, at)
		if _, err := repo.UpsertListing(ctx, l); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	items, _, err := repo.List(ctx, domain.ListFilter{Limit: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// The stored flags must match the deterministic functions...
	for i := range items {
		if items[i].Sponsored != demoSponsored(items[i].ID) {
			t.Errorf("business %d sponsored=%v, want %v", items[i].ID, items[i].Sponsored, demoSponsored(items[i].ID))
		}
		if items[i].Verified != demoVerified(items[i].ID) {
			t.Errorf("business %d verified=%v, want %v", items[i].ID, items[i].Verified, demoVerified(items[i].ID))
		}
	}
	// ...and all sponsored businesses must sort ahead of non-sponsored ones.
	seenNonSponsored := false
	for _, b := range items {
		if !b.Sponsored {
			seenNonSponsored = true
		} else if seenNonSponsored {
			t.Fatalf("sponsored business %d appeared after a non-sponsored one", b.ID)
		}
	}
}

func TestFacets(t *testing.T) {
	t.Parallel()
	repo := openTestRepo(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)

	seed := []*domain.Listing{
		sampleListing("booksy", "1", "A", "madrid", 40.1, -3.1, 5, 5, at),
		sampleListing("booksy", "2", "B", "madrid", 40.2, -3.2, 5, 5, at),
		sampleListing("booksy", "3", "C", "vigo", 42.2, -8.7, 5, 5, at),
	}
	seed[2].Category = "salon-de-unas"
	for _, l := range seed {
		if _, err := repo.UpsertListing(ctx, l); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	cats, err := repo.CategoryFacets(ctx, "")
	if err != nil {
		t.Fatalf("CategoryFacets: %v", err)
	}
	if len(cats) != 2 || cats[0].Value != "barberia" || cats[0].Count != 2 {
		t.Errorf("categories = %+v", cats)
	}

	cities, err := repo.CityFacets(ctx, "")
	if err != nil {
		t.Fatalf("CityFacets: %v", err)
	}
	if len(cities) != 2 || cities[0].Value != "madrid" || cities[0].Count != 2 {
		t.Errorf("cities = %+v", cities)
	}
}

func TestListStaleListings(t *testing.T) {
	t.Parallel()
	repo := openTestRepo(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	fresh := time.Now().UTC().Truncate(time.Second)

	if _, err := repo.UpsertListing(ctx,
		sampleListing("booksy", "old", "Old Shop", "madrid", 40.1, -3.1, 4, 5, old)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpsertListing(ctx,
		sampleListing("booksy", "new", "New Shop", "vigo", 42.1, -8.1, 4, 5, fresh)); err != nil {
		t.Fatal(err)
	}

	stale, err := repo.ListStaleListings(ctx, "booksy", time.Now().UTC().Add(-30*24*time.Hour), 10)
	if err != nil {
		t.Fatalf("ListStaleListings: %v", err)
	}
	if len(stale) != 1 || stale[0].SourceID != "old" {
		t.Errorf("stale = %+v, want only the 40-day-old listing", stale)
	}
}

func TestConcurrentUpserts(t *testing.T) {
	t.Parallel()
	repo := openTestRepo(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	done := make(chan error, 8)
	for i := range 8 {
		go func() {
			l := sampleListing("booksy", "c", "Concurrent", "madrid", 40.0, -3.0, float64(i%5)+1, 5, at)
			_, err := repo.UpsertListing(ctx, l)
			done <- err
		}()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Errorf("concurrent UpsertListing: %v", err)
		}
	}
}
