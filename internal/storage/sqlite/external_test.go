package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
)

func sampleExternal(placeID, name string, lat, lng, rating float64) *domain.External {
	return &domain.External{
		Source: "supabase", PlaceID: placeID, Name: name,
		Category: "barberia", Address: "C/ Test, 1, 46004 València, Valencia, Spain",
		Neighborhood: "Ruzafa",
		Latitude:     ptr(lat), Longitude: ptr(lng),
		GoogleRating: ptr(rating), GoogleReviews: 200,
		PriceFrom: ptr(9.0), PriceTo: ptr(13.0), Currency: "EUR",
		Services: []domain.ExternalService{
			{Name: "Corte de pelo hombre", Slug: "corte_hombre", PriceLow: ptr(9.0), PriceHigh: ptr(13.0), PriceEst: ptr(11.0)},
		},
	}
}

func TestSyncExternalOnlyIsUnknown(t *testing.T) {
	t.Parallel()
	repo := openTestRepo(t)
	ctx := context.Background()

	id, err := repo.SyncExternal(ctx, sampleExternal("place-A", "Fresh Fade Factory", 39.4644, -0.3731, 4.8))
	if err != nil {
		t.Fatalf("SyncExternal: %v", err)
	}
	b, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !b.Unknown {
		t.Errorf("external-only business should be Unknown, sources=%v", b.Sources)
	}
	if len(b.Externals) != 1 || b.Externals[0].PlaceID != "place-A" {
		t.Fatalf("externals = %+v", b.Externals)
	}
	if b.Externals[0].GoogleRating == nil || *b.Externals[0].GoogleRating != 4.8 {
		t.Errorf("google rating = %+v", b.Externals[0].GoogleRating)
	}
	if b.City != "valencia" {
		t.Errorf("city = %q, want valencia (parsed from address)", b.City)
	}
	// Price band comes from the external estimate.
	if b.PriceFrom == nil || b.PriceTo == nil {
		t.Errorf("price band not set from external services: %v..%v", b.PriceFrom, b.PriceTo)
	}
}

func TestSyncExternalMatchesScrapedBusiness(t *testing.T) {
	t.Parallel()
	repo := openTestRepo(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)

	// A scraped Booksy barber in Valencia.
	scrapedID, err := repo.UpsertListing(ctx,
		sampleListing("booksy", "999", "Fresh Fade Factory Ruzafa", "valencia", 39.4644, -0.3731, 4.7, 100, at))
	if err != nil {
		t.Fatal(err)
	}

	// Same venue from Supabase (~same coords, similar name) must merge, not duplicate.
	extID, err := repo.SyncExternal(ctx,
		sampleExternal("place-B", "Fresh Fade Factory", 39.46445, -0.37315, 4.8))
	if err != nil {
		t.Fatalf("SyncExternal: %v", err)
	}
	if extID != scrapedID {
		t.Fatalf("external did not merge with scraped business: %d != %d", extID, scrapedID)
	}
	b, err := repo.GetByID(ctx, scrapedID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Unknown {
		t.Error("business known from booksy must not be Unknown")
	}
	if len(b.Sources) != 2 {
		t.Errorf("sources = %v, want booksy + supabase", b.Sources)
	}
	if len(b.Externals) != 1 {
		t.Errorf("expected the external attached, got %d", len(b.Externals))
	}
}

func TestSyncExternalReupsertNoDuplicate(t *testing.T) {
	t.Parallel()
	repo := openTestRepo(t)
	ctx := context.Background()
	e := sampleExternal("place-C", "Barbería X", 39.47, -0.38, 4.5)
	id1, err := repo.SyncExternal(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := repo.SyncExternal(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("re-sync created a new business: %d != %d", id1, id2)
	}
	b, _ := repo.GetByID(ctx, id1)
	if len(b.Externals) != 1 {
		t.Errorf("re-sync duplicated externals: %d", len(b.Externals))
	}
}
