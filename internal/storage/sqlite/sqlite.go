// Package sqlite implements domain.BusinessRepository on SQLite using the
// pure-Go modernc.org/sqlite driver (no CGO).
//
// Data model: per-source listings are the unit of ingestion; each resolves
// (via internal/match) to one canonical business row, which is recomputed
// from all of its listings on every write (domain.MergeListings).
package sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
	"github.com/crdzbird/cuanto_cuesta/internal/geo"
	"github.com/crdzbird/cuanto_cuesta/internal/match"
)

const schema = `
CREATE TABLE IF NOT EXISTS businesses (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	name          TEXT NOT NULL,
	category      TEXT NOT NULL DEFAULT '',
	schema_type   TEXT NOT NULL DEFAULT '',
	description   TEXT NOT NULL DEFAULT '',
	city          TEXT NOT NULL DEFAULT '',
	street        TEXT NOT NULL DEFAULT '',
	locality      TEXT NOT NULL DEFAULT '',
	postal_code   TEXT NOT NULL DEFAULT '',
	country       TEXT NOT NULL DEFAULT '',
	lat           REAL,
	lng           REAL,
	price_range   TEXT NOT NULL DEFAULT '',
	price_from    REAL,
	price_to      REAL,
	price_currency TEXT NOT NULL DEFAULT '',
	rating        REAL,
	review_count  INTEGER,
	phone         TEXT NOT NULL DEFAULT '',
	email         TEXT NOT NULL DEFAULT '',
	payment       TEXT NOT NULL DEFAULT '',
	image_url     TEXT NOT NULL DEFAULT '',
	logo_url      TEXT NOT NULL DEFAULT '',
	images        TEXT NOT NULL DEFAULT '[]',
	social_links  TEXT NOT NULL DEFAULT '[]',
	opening_hours TEXT NOT NULL DEFAULT '[]',
	reviews       TEXT NOT NULL DEFAULT '[]',
	sponsored     INTEGER NOT NULL DEFAULT 0,
	verified      INTEGER NOT NULL DEFAULT 0,
	sources       TEXT NOT NULL DEFAULT '[]',
	last_verified TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS listings (
	source        TEXT NOT NULL,
	source_id     TEXT NOT NULL,
	business_id   INTEGER NOT NULL REFERENCES businesses(id) ON DELETE CASCADE,
	url           TEXT NOT NULL,
	name          TEXT NOT NULL,
	category      TEXT NOT NULL DEFAULT '',
	schema_type   TEXT NOT NULL DEFAULT '',
	description   TEXT NOT NULL DEFAULT '',
	city          TEXT NOT NULL DEFAULT '',
	street        TEXT NOT NULL DEFAULT '',
	locality      TEXT NOT NULL DEFAULT '',
	postal_code   TEXT NOT NULL DEFAULT '',
	country       TEXT NOT NULL DEFAULT '',
	lat           REAL,
	lng           REAL,
	price_range   TEXT NOT NULL DEFAULT '',
	rating        REAL,
	review_count  INTEGER,
	phone         TEXT NOT NULL DEFAULT '',
	email         TEXT NOT NULL DEFAULT '',
	payment       TEXT NOT NULL DEFAULT '',
	image_url     TEXT NOT NULL DEFAULT '',
	logo_url      TEXT NOT NULL DEFAULT '',
	images        TEXT NOT NULL DEFAULT '[]',
	social_links  TEXT NOT NULL DEFAULT '[]',
	opening_hours TEXT NOT NULL DEFAULT '[]',
	services      TEXT NOT NULL DEFAULT '[]',
	reviews       TEXT NOT NULL DEFAULT '[]',
	scraped_at    TEXT NOT NULL,
	PRIMARY KEY (source, source_id)
);
CREATE TABLE IF NOT EXISTS services (
	business_id  INTEGER NOT NULL REFERENCES businesses(id) ON DELETE CASCADE,
	name         TEXT NOT NULL,
	description  TEXT NOT NULL DEFAULT '',
	price        REAL,
	price_min    REAL,
	price_max    REAL,
	currency     TEXT NOT NULL DEFAULT '',
	duration_min INTEGER,
	image_url    TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (business_id, name)
);
CREATE INDEX IF NOT EXISTS idx_businesses_category  ON businesses(category);
CREATE INDEX IF NOT EXISTS idx_businesses_city      ON businesses(city);
CREATE INDEX IF NOT EXISTS idx_businesses_rating    ON businesses(rating);
CREATE INDEX IF NOT EXISTS idx_listings_business    ON listings(business_id);
CREATE INDEX IF NOT EXISTS idx_listings_scraped_at  ON listings(scraped_at);
`

// Repo is a SQLite-backed domain.BusinessRepository. Safe for concurrent use.
type Repo struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies the schema.
// The DSN pragmas configure WAL (concurrent reader-friendly), a busy timeout
// so the API and scraper processes can share the file, and FK enforcement.
func Open(ctx context.Context, path string) (*Repo, error) {
	// _txlock=immediate makes every transaction take the write lock at BEGIN,
	// so concurrent writers queue on busy_timeout instead of failing with
	// SQLITE_BUSY when a read transaction tries to upgrade mid-flight
	// (UpsertListing reads candidates before writing).
	dsn := "file:" + path + "?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// SQLite allows one writer; a small pool avoids lock churn.
	db.SetMaxOpenConns(4)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// Additive migrations for databases created before these columns existed.
	// SQLite has no ADD COLUMN IF NOT EXISTS, so a duplicate is expected and
	// ignored; anything else is a real failure.
	for _, col := range []string{"sponsored", "verified"} {
		if _, err := db.ExecContext(ctx,
			`ALTER TABLE businesses ADD COLUMN `+col+` INTEGER NOT NULL DEFAULT 0`); err != nil &&
			!strings.Contains(err.Error(), "duplicate column") {
			_ = db.Close()
			return nil, fmt.Errorf("migrate %s column: %w", col, err)
		}
	}
	return &Repo{db: db}, nil
}

// Close releases the underlying pool.
func (r *Repo) Close() error { return r.db.Close() }

// UpsertListing implements domain.BusinessRepository: store the listing,
// resolve it to a canonical business, recompute the merged view — all in one
// transaction so readers never observe a half-merged business.
func (r *Repo) UpsertListing(ctx context.Context, l *domain.Listing) (int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	businessID, err := r.resolveBusinessID(ctx, tx, l)
	if err != nil {
		return 0, err
	}
	if err := insertListing(ctx, tx, businessID, l); err != nil {
		return 0, err
	}
	if err := recomputeCanonical(ctx, tx, businessID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit listing %s/%s: %w", l.Source, l.SourceID, err)
	}
	return businessID, nil
}

// resolveBusinessID finds the canonical business this listing belongs to:
// the fast path is the (source, source_id) key from a previous crawl; the
// slow path entity-matches against candidate businesses in the same city or
// geographic neighborhood, creating a new business when nothing matches.
func (r *Repo) resolveBusinessID(ctx context.Context, tx *sql.Tx, l *domain.Listing) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		`SELECT business_id FROM listings WHERE source = ? AND source_id = ?`,
		l.Source, l.SourceID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("lookup listing %s/%s: %w", l.Source, l.SourceID, err)
	}

	candidates, err := candidateListings(ctx, tx, l)
	if err != nil {
		return 0, err
	}
	for i := range candidates {
		if match.SameBusiness(l, &candidates[i]) {
			var bid int64
			if err := tx.QueryRowContext(ctx,
				`SELECT business_id FROM listings WHERE source = ? AND source_id = ?`,
				candidates[i].Source, candidates[i].SourceID).Scan(&bid); err != nil {
				return 0, fmt.Errorf("candidate business id: %w", err)
			}
			return bid, nil
		}
	}

	// No match: create the canonical row (recomputeCanonical fills it in).
	res, err := tx.ExecContext(ctx,
		`INSERT INTO businesses (name, last_verified) VALUES (?, ?)`,
		l.Name, l.ScrapedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("create business for %s/%s: %w", l.Source, l.SourceID, err)
	}
	return res.LastInsertId()
}

// candidateListings returns listings that could plausibly be the same venue:
// same city slug, or within a ~600 m bounding box when coordinates exist.
// Matching against listings (not canonical rows) compares like with like.
func candidateListings(ctx context.Context, tx *sql.Tx, l *domain.Listing) ([]domain.Listing, error) {
	const sel = `SELECT ` + listingCols + ` FROM listings`
	var rows *sql.Rows
	var err error
	switch {
	case l.Latitude != nil && l.Longitude != nil:
		const delta = 0.006 // ~600 m latitude; wider in longitude, fine for candidates
		rows, err = tx.QueryContext(ctx, sel+
			` WHERE (lat BETWEEN ? AND ? AND lng BETWEEN ? AND ?) OR (city != '' AND city = ?) LIMIT 500`,
			*l.Latitude-delta, *l.Latitude+delta,
			*l.Longitude-delta, *l.Longitude+delta, l.City)
	case l.City != "":
		rows, err = tx.QueryContext(ctx, sel+` WHERE city = ? LIMIT 500`, l.City)
	default:
		return nil, nil // nothing to match on; a new business will be created
	}
	if err != nil {
		return nil, fmt.Errorf("candidate listings: %w", err)
	}
	return scanListings(rows)
}

func insertListing(ctx context.Context, tx *sql.Tx, businessID int64, l *domain.Listing) error {
	j, err := marshalListingJSON(l)
	if err != nil {
		return err
	}
	var rating, reviewCount any
	if l.Rating != nil {
		rating, reviewCount = l.Rating.Value, l.Rating.ReviewCount
	}
	var lat, lng any
	if l.Latitude != nil && l.Longitude != nil {
		lat, lng = *l.Latitude, *l.Longitude
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO listings (source, source_id, business_id, url, name, category,
			schema_type, description, city, street, locality, postal_code, country,
			lat, lng, price_range, rating, review_count, phone, email, payment,
			image_url, logo_url, images, social_links, opening_hours, services,
			reviews, scraped_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(source, source_id) DO UPDATE SET
			business_id=excluded.business_id, url=excluded.url, name=excluded.name,
			category=excluded.category, schema_type=excluded.schema_type,
			description=excluded.description, city=excluded.city, street=excluded.street,
			locality=excluded.locality, postal_code=excluded.postal_code, country=excluded.country,
			lat=excluded.lat, lng=excluded.lng, price_range=excluded.price_range,
			rating=excluded.rating, review_count=excluded.review_count,
			phone=excluded.phone, email=excluded.email, payment=excluded.payment,
			image_url=excluded.image_url, logo_url=excluded.logo_url, images=excluded.images,
			social_links=excluded.social_links, opening_hours=excluded.opening_hours,
			services=excluded.services, reviews=excluded.reviews, scraped_at=excluded.scraped_at`,
		l.Source, l.SourceID, businessID, l.URL, l.Name, l.Category,
		l.SchemaType, l.Description, l.City, l.Address.Street, l.Address.Locality,
		l.Address.PostalCode, l.Address.Country, lat, lng, l.PriceRange,
		rating, reviewCount, l.Phone, l.Email, l.Payment, l.ImageURL, l.LogoURL,
		j.images, j.social, j.hours, j.services, j.reviews,
		l.ScrapedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("upsert listing %s/%s: %w", l.Source, l.SourceID, err)
	}
	return nil
}

// recomputeCanonical rebuilds the merged business row and its services from
// all listings currently attached to it.
func recomputeCanonical(ctx context.Context, tx *sql.Tx, businessID int64) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+listingCols+` FROM listings WHERE business_id = ?`, businessID)
	if err != nil {
		return fmt.Errorf("load listings for %d: %w", businessID, err)
	}
	listings, err := scanListings(rows)
	if err != nil {
		return err
	}
	if len(listings) == 0 {
		return fmt.Errorf("business %d has no listings", businessID)
	}
	// Re-derive each listing's city from its address so the canonical city is
	// a real municipality regardless of which source (or which generation of
	// the parser) produced the row — this is what makes /v1/cities and the
	// ?city= filter consistent across booksy slugs, treatwell, and crawls.
	for i := range listings {
		listings[i].City = canonicalCity(&listings[i])
	}
	b := domain.MergeListings(listings)

	socialJSON, err := json.Marshal(b.SocialLinks)
	if err != nil {
		return fmt.Errorf("marshal social links: %w", err)
	}
	hoursJSON, err := json.Marshal(b.OpeningHours)
	if err != nil {
		return fmt.Errorf("marshal opening hours: %w", err)
	}
	sourcesJSON, err := json.Marshal(b.Sources)
	if err != nil {
		return fmt.Errorf("marshal sources: %w", err)
	}
	imagesJSON, err := json.Marshal(b.Images)
	if err != nil {
		return fmt.Errorf("marshal images: %w", err)
	}
	reviewsJSON, err := json.Marshal(b.Reviews)
	if err != nil {
		return fmt.Errorf("marshal reviews: %w", err)
	}
	var rating, reviewCount any
	if b.Rating != nil {
		rating, reviewCount = b.Rating.Value, b.Rating.ReviewCount
	}
	var lat, lng any
	if b.Latitude != nil && b.Longitude != nil {
		lat, lng = *b.Latitude, *b.Longitude
	}
	var priceFrom, priceTo any
	if b.PriceFrom != nil {
		priceFrom = *b.PriceFrom
	}
	if b.PriceTo != nil {
		priceTo = *b.PriceTo
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE businesses SET name=?, category=?, schema_type=?, description=?,
			city=?, street=?, locality=?, postal_code=?, country=?, lat=?, lng=?,
			price_range=?, price_from=?, price_to=?, price_currency=?,
			rating=?, review_count=?, phone=?, email=?, payment=?,
			image_url=?, logo_url=?, images=?, social_links=?, opening_hours=?,
			reviews=?, sponsored=?, verified=?, sources=?, last_verified=?
		WHERE id=?`,
		b.Name, b.Category, b.SchemaType, b.Description, b.City,
		b.Address.Street, b.Address.Locality, b.Address.PostalCode, b.Address.Country,
		lat, lng, b.PriceRange, priceFrom, priceTo, b.PriceCurrency,
		rating, reviewCount, b.Phone, b.Email, b.Payment,
		b.ImageURL, b.LogoURL, string(imagesJSON), string(socialJSON), string(hoursJSON),
		string(reviewsJSON), boolToInt(demoSponsored(businessID)),
		boolToInt(demoVerified(businessID)), string(sourcesJSON),
		b.LastVerified.UTC().Format(time.RFC3339), businessID)
	if err != nil {
		return fmt.Errorf("update business %d: %w", businessID, err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM services WHERE business_id = ?`, businessID); err != nil {
		return fmt.Errorf("clear services for %d: %w", businessID, err)
	}
	for _, svc := range b.Services {
		var price, priceMin, priceMax, duration any
		if svc.Price != nil {
			price = *svc.Price
		}
		if svc.PriceMin != nil {
			priceMin = *svc.PriceMin
		}
		if svc.PriceMax != nil {
			priceMax = *svc.PriceMax
		}
		if svc.DurationMin != nil {
			duration = *svc.DurationMin
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO services
			 (business_id, name, description, price, price_min, price_max, currency, duration_min, image_url)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			businessID, svc.Name, svc.Description, price, priceMin, priceMax,
			svc.Currency, duration, svc.ImageURL); err != nil {
			return fmt.Errorf("insert service %q for %d: %w", svc.Name, businessID, err)
		}
	}
	return nil
}

// GetByID implements domain.BusinessRepository.
func (r *Repo) GetByID(ctx context.Context, id int64) (*domain.Business, error) {
	row := r.db.QueryRowContext(ctx, selectBusinessCols+` FROM businesses WHERE id = ?`, id)
	b, err := scanBusiness(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get business %d: %w", id, err)
	}

	svcRows, err := r.db.QueryContext(ctx,
		`SELECT name, description, price, price_min, price_max, currency, duration_min, image_url
		 FROM services WHERE business_id = ? ORDER BY name`, id)
	if err != nil {
		return nil, fmt.Errorf("get services for %d: %w", id, err)
	}
	b.Services, err = scanServices(svcRows)
	if err != nil {
		return nil, err
	}

	lRows, err := r.db.QueryContext(ctx,
		`SELECT `+listingCols+` FROM listings WHERE business_id = ? ORDER BY scraped_at DESC`, id)
	if err != nil {
		return nil, fmt.Errorf("get listings for %d: %w", id, err)
	}
	b.Listings, err = scanListings(lRows)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// ListStaleListings implements domain.BusinessRepository.
func (r *Repo) ListStaleListings(ctx context.Context, source string, cutoff time.Time, limit int) ([]domain.Listing, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+listingCols+` FROM listings
		 WHERE source = ? AND scraped_at < ? ORDER BY scraped_at LIMIT ?`,
		source, cutoff.UTC().Format(time.RFC3339), limit)
	if err != nil {
		return nil, fmt.Errorf("list stale listings: %w", err)
	}
	return scanListings(rows)
}

// List implements domain.BusinessRepository.
//
// Geo filtering uses a latitude/longitude bounding-box prefilter in SQL and a
// haversine refinement in Go (SQLite has no trig functions without
// extensions); geo result sets are then sorted by distance.
// COMPLEXITY: Time O(rows in bounding box) for geo queries, O(page) otherwise.
func (r *Repo) List(ctx context.Context, f domain.ListFilter) ([]domain.Business, int, error) {
	if f.Limit <= 0 || f.Limit > 100 {
		f.Limit = 20
	}
	if f.Offset < 0 {
		f.Offset = 0
	}

	var where []string
	var args []any
	if f.Category != "" {
		where = append(where, "category = ?")
		args = append(args, f.Category)
	}
	if f.City != "" {
		where = append(where, "city = ?")
		args = append(args, f.City)
	}
	if f.Query != "" {
		where = append(where, "name LIKE ? ESCAPE '\\'")
		args = append(args, "%"+escapeLike(f.Query)+"%")
	}
	if f.MinRating > 0 {
		where = append(where, "rating >= ?")
		args = append(args, f.MinRating)
	}
	if f.Geo != nil {
		latDelta := f.Geo.RadiusKm / 111.0 // ~km per degree latitude
		lngDelta := latDelta / math.Max(math.Cos(f.Geo.Lat*math.Pi/180), 0.01)
		where = append(where, "lat BETWEEN ? AND ?", "lng BETWEEN ? AND ?")
		args = append(args,
			f.Geo.Lat-latDelta, f.Geo.Lat+latDelta,
			f.Geo.Lng-lngDelta, f.Geo.Lng+lngDelta)
	}
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}

	if f.Geo != nil {
		// Fetch the whole bounding box, refine by true distance, page in Go.
		rows, err := r.db.QueryContext(ctx, selectBusinessCols+` FROM businesses`+cond, args...)
		if err != nil {
			return nil, 0, fmt.Errorf("list businesses (geo): %w", err)
		}
		all, err := scanBusinesses(rows)
		if err != nil {
			return nil, 0, err
		}
		var inRadius []domain.Business
		for _, b := range all {
			if b.Latitude == nil || b.Longitude == nil {
				continue
			}
			if geo.HaversineKm(f.Geo.Lat, f.Geo.Lng, *b.Latitude, *b.Longitude) <= f.Geo.RadiusKm {
				inRadius = append(inRadius, b)
			}
		}
		sort.Slice(inRadius, func(i, j int) bool {
			di := geo.HaversineKm(f.Geo.Lat, f.Geo.Lng, *inRadius[i].Latitude, *inRadius[i].Longitude)
			dj := geo.HaversineKm(f.Geo.Lat, f.Geo.Lng, *inRadius[j].Latitude, *inRadius[j].Longitude)
			return di < dj
		})
		total := len(inRadius)
		start := min(f.Offset, total)
		end := min(start+f.Limit, total)
		return inRadius[start:end], total, nil
	}

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM businesses`+cond, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count businesses: %w", err)
	}
	rows, err := r.db.QueryContext(ctx,
		selectBusinessCols+` FROM businesses`+cond+
			` ORDER BY sponsored DESC, rating DESC NULLS LAST, review_count DESC, name LIMIT ? OFFSET ?`,
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("list businesses: %w", err)
	}
	items, err := scanBusinesses(rows)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// demoFlag returns a stable pseudo-random boolean derived from the business
// ID and a salt, true for roughly pctOutOf10 in 10 businesses. Deterministic
// so a business keeps the same flags across requests; the salt makes
// independent flags (sponsored vs verified) uncorrelated.
func demoFlag(id int64, salt byte, pctOutOf10 uint32) bool {
	var b [9]byte
	binary.LittleEndian.PutUint64(b[:8], uint64(id))
	b[8] = salt
	h := fnv.New32a()
	_, _ = h.Write(b[:])
	return h.Sum32()%10 < pctOutOf10
}

// demoSponsored marks ~30% of businesses as associated partners; demoVerified
// marks ~60% as identity-confirmed. Replace both with real tables for
// production (a sponsorships join and a verification status).
func demoSponsored(id int64) bool { return demoFlag(id, 's', 3) }
func demoVerified(id int64) bool  { return demoFlag(id, 'v', 6) }

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// canonicalCity prefers the municipality parsed from the address, falling
// back to the listing's own city value (e.g. a Booksy URL slug) normalized.
func canonicalCity(l *domain.Listing) string {
	if c := match.CityGuess(l.Address); c != "" {
		return c
	}
	return match.NormalizeCity(l.City)
}

// RenormalizeAll recomputes every canonical business from its listings,
// applying the current parsing/merge rules (notably city normalization) to
// data stored by an earlier version. Returns the number of businesses fixed.
func (r *Repo) RenormalizeAll(ctx context.Context) (int, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM businesses ORDER BY id`)
	if err != nil {
		return 0, fmt.Errorf("list business ids: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan business id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("iterate business ids: %w", err)
	}
	_ = rows.Close()

	for _, id := range ids {
		if err := r.renormalizeOne(ctx, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

func (r *Repo) renormalizeOne(ctx context.Context, id int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if err := recomputeCanonical(ctx, tx, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit renormalize %d: %w", id, err)
	}
	return nil
}

// CategoryFacets implements domain.BusinessRepository.
func (r *Repo) CategoryFacets(ctx context.Context) ([]domain.Facet, error) {
	return r.facets(ctx, "category")
}

// CityFacets implements domain.BusinessRepository.
func (r *Repo) CityFacets(ctx context.Context) ([]domain.Facet, error) {
	return r.facets(ctx, "city")
}

// facets aggregates one column of the canonical businesses table. The column
// name is compile-time constant at both call sites, never user input.
func (r *Repo) facets(ctx context.Context, column string) ([]domain.Facet, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+column+`, COUNT(*) AS n FROM businesses
		 WHERE `+column+` != '' GROUP BY `+column+` ORDER BY n DESC, `+column)
	if err != nil {
		return nil, fmt.Errorf("facets %s: %w", column, err)
	}
	defer func() { _ = rows.Close() }()
	var out []domain.Facet
	for rows.Next() {
		var f domain.Facet
		if err := rows.Scan(&f.Value, &f.Count); err != nil {
			return nil, fmt.Errorf("scan facet: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate facets: %w", err)
	}
	return out, nil
}

var _ domain.BusinessRepository = (*Repo)(nil)
