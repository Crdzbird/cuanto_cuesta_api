package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
	"github.com/crdzbird/cuanto_cuesta/internal/match"
)

// SyncExternal implements domain.BusinessRepository. It first upserts a
// synthetic "supabase" listing so the external business participates in the
// catalog (entity resolution merges it with a scraped business when they're
// the same venue, or creates a new canonical business otherwise), then stores
// the rich external detail linked to that business.
func (r *Repo) SyncExternal(ctx context.Context, e *domain.External) (int64, error) {
	businessID, err := r.UpsertListing(ctx, externalToListing(e))
	if err != nil {
		return 0, fmt.Errorf("sync external %q: %w", e.Name, err)
	}

	servicesJSON, err := json.Marshal(e.Services)
	if err != nil {
		return 0, fmt.Errorf("marshal external services: %w", err)
	}
	placeID := e.PlaceID
	if placeID == "" {
		placeID = match.Slugify(e.Name) // fallback key when no Google place id
	}
	var lat, lng, rating, pFrom, pTo any
	if e.Latitude != nil && e.Longitude != nil {
		lat, lng = *e.Latitude, *e.Longitude
	}
	if e.GoogleRating != nil {
		rating = *e.GoogleRating
	}
	if e.PriceFrom != nil {
		pFrom = *e.PriceFrom
	}
	if e.PriceTo != nil {
		pTo = *e.PriceTo
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO externals (source, place_id, business_id, name, category, address,
			neighborhood, lat, lng, google_rating, google_reviews, price_level,
			price_from, price_to, currency, services)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(source, place_id) DO UPDATE SET
			business_id=excluded.business_id, name=excluded.name, category=excluded.category,
			address=excluded.address, neighborhood=excluded.neighborhood, lat=excluded.lat,
			lng=excluded.lng, google_rating=excluded.google_rating, google_reviews=excluded.google_reviews,
			price_level=excluded.price_level, price_from=excluded.price_from, price_to=excluded.price_to,
			currency=excluded.currency, services=excluded.services`,
		e.Source, placeID, businessID, e.Name, e.Category, e.Address,
		e.Neighborhood, lat, lng, rating, e.GoogleReviews, e.PriceLevel,
		pFrom, pTo, e.Currency, string(servicesJSON))
	if err != nil {
		return 0, fmt.Errorf("upsert external %s/%s: %w", e.Source, placeID, err)
	}
	return businessID, nil
}

// externalToListing builds the synthetic listing that lets an external
// business flow through the normal entity-resolution + merge pipeline. Its
// services carry the numeric price estimates so the catalog price band works.
func externalToListing(e *domain.External) *domain.Listing {
	l := &domain.Listing{
		Source:     e.Source,
		Name:       e.Name,
		Category:   e.Category,
		SchemaType: "LocalBusiness",
		Address:    domain.Address{Street: e.Address, Country: "es"},
		Latitude:   e.Latitude,
		Longitude:  e.Longitude,
		URL:        "", // external dataset has no canonical page
		ScrapedAt:  time.Now().UTC(),
	}
	l.SourceID = e.PlaceID
	if l.SourceID == "" {
		l.SourceID = match.Slugify(e.Name)
	}
	l.City = match.CityGuess(l.Address)
	if e.GoogleRating != nil {
		l.Rating = &domain.Rating{Value: *e.GoogleRating, ReviewCount: e.GoogleReviews}
	}
	for _, s := range e.Services {
		svc := domain.ServiceOffer{Name: s.Name, Currency: e.Currency}
		if s.PriceEst != nil {
			svc.Price = s.PriceEst
		}
		l.Services = append(l.Services, svc)
	}
	return l
}

// Reresolve rebuilds every canonical business from the stored per-source
// listings and externals, re-running entity resolution with the current
// matcher. Use after tuning matching rules to repair groupings (e.g. undo
// false merges) without re-scraping. Returns the rebuilt business count.
//
// The raw listing/external rows are the source of truth; only the business
// grouping is recomputed. Supabase listings are recreated by SyncExternal, so
// they are not re-inserted directly.
func (r *Repo) Reresolve(ctx context.Context) (int, error) {
	listings, err := r.allListings(ctx)
	if err != nil {
		return 0, err
	}
	externals, err := r.allExternals(ctx)
	if err != nil {
		return 0, err
	}

	// Wipe canonical data; FK cascade clears listings, services, externals.
	if _, err := r.db.ExecContext(ctx, `DELETE FROM businesses`); err != nil {
		return 0, fmt.Errorf("reresolve: clear businesses: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name='businesses'`); err != nil {
		return 0, fmt.Errorf("reresolve: reset ids: %w", err)
	}

	for i := range listings {
		if listings[i].Source == "supabase" {
			continue // recreated by SyncExternal below
		}
		if _, err := r.UpsertListing(ctx, &listings[i]); err != nil {
			return 0, fmt.Errorf("reresolve listing %s/%s: %w", listings[i].Source, listings[i].SourceID, err)
		}
	}
	for i := range externals {
		if _, err := r.SyncExternal(ctx, &externals[i]); err != nil {
			return 0, fmt.Errorf("reresolve external %q: %w", externals[i].Name, err)
		}
	}

	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM businesses`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (r *Repo) allListings(ctx context.Context) ([]domain.Listing, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+listingCols+` FROM listings ORDER BY scraped_at`)
	if err != nil {
		return nil, fmt.Errorf("load all listings: %w", err)
	}
	return scanListings(rows)
}

func (r *Repo) allExternals(ctx context.Context) ([]domain.External, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT business_id FROM externals`)
	if err != nil {
		return nil, fmt.Errorf("load external ids: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []domain.External
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		exts, err := loadExternals(ctx, r.db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, exts...)
	}
	return out, nil
}

func loadExternals(ctx context.Context, db *sql.DB, businessID int64) ([]domain.External, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT source, place_id, name, category, address, neighborhood, lat, lng,
			google_rating, google_reviews, price_level, price_from, price_to, currency, services
		FROM externals WHERE business_id = ? ORDER BY source, place_id`, businessID)
	if err != nil {
		return nil, fmt.Errorf("load externals for %d: %w", businessID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []domain.External
	for rows.Next() {
		var e domain.External
		var lat, lng, rating, pFrom, pTo sql.NullFloat64
		var reviews sql.NullInt64
		var servicesJSON string
		var placeID string
		if err := rows.Scan(&e.Source, &placeID, &e.Name, &e.Category, &e.Address,
			&e.Neighborhood, &lat, &lng, &rating, &reviews, &e.PriceLevel,
			&pFrom, &pTo, &e.Currency, &servicesJSON); err != nil {
			return nil, fmt.Errorf("scan external: %w", err)
		}
		e.PlaceID = placeID
		if lat.Valid && lng.Valid {
			e.Latitude, e.Longitude = &lat.Float64, &lng.Float64
		}
		if rating.Valid {
			v := rating.Float64
			e.GoogleRating = &v
			e.GoogleReviews = int(reviews.Int64)
		}
		if pFrom.Valid {
			e.PriceFrom = &pFrom.Float64
		}
		if pTo.Valid {
			e.PriceTo = &pTo.Float64
		}
		if err := json.Unmarshal([]byte(servicesJSON), &e.Services); err != nil {
			return nil, fmt.Errorf("decode external services: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate externals: %w", err)
	}
	return out, nil
}
