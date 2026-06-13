// Package domain holds the core entities, merge policy, and repository
// contracts. It imports nothing outside the standard library.
package domain

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrNotFound is returned by repositories when an entity does not exist.
var ErrNotFound = errors.New("not found")

// StaleAfter is how long scraped data is trusted before the API flags it
// stale and the scraper's refresh mode re-crawls it.
const StaleAfter = 30 * 24 * time.Hour

// Listing is one source's view of a business: the raw unit of scraping.
// Several listings from different sources can resolve to one Business.
type Listing struct {
	Source       string // source name, e.g. "booksy", "treatwell", "web"
	SourceID     string // stable ID within the source (site-local ID or URL)
	URL          string
	Name         string
	Category     string // source-local slug, e.g. "barberia"
	SchemaType   string // schema.org @type, e.g. "HairSalon"
	Description  string
	City         string // normalized slug, e.g. "madrid"
	Address      Address
	Latitude     *float64
	Longitude    *float64
	PriceRange   string // raw source string, e.g. "EUR 5 - 35"
	// PriceFrom/PriceTo carry a price band when the source gives one without
	// itemized services (e.g. a Yelp price level mapped to a range).
	PriceFrom     *float64
	PriceTo       *float64
	PriceCurrency string
	Rating       *Rating
	Phone        string
	Email        string
	Payment      string // accepted payment methods, raw source string
	ImageURL     string // primary image (first of Images)
	LogoURL      string
	Images       []string
	SocialLinks  []string
	OpeningHours []OpeningHours
	Services     []ServiceOffer
	Reviews      []Review
	ScrapedAt    time.Time
}

// Business is the canonical, merged view of one real-world establishment.
// All scalar fields come from MergeListings; Listings carries provenance.
type Business struct {
	ID           int64
	Name         string
	Category     string
	SchemaType   string
	Description  string
	City         string
	Address      Address
	Latitude     *float64
	Longitude    *float64
	PriceRange   string
	PriceFrom    *float64 // cheapest service price, derived from Services
	PriceTo      *float64 // priciest service price
	PriceCurrency string
	Rating       *Rating // aggregated across sources
	Phone        string
	Email        string
	Payment      string
	ImageURL     string
	LogoURL      string
	Images       []string
	SocialLinks  []string
	OpeningHours []OpeningHours
	Services     []ServiceOffer
	Reviews      []Review  // freshest sample reviews per source, newest first
	Sponsored    bool       // associated/partner business (demo: deterministic per ID)
	Verified     bool       // identity/legitimacy confirmed (demo: deterministic per ID)
	Unknown      bool       // present only in external sources (e.g. Supabase/Google Maps), never scraped by us
	Sources      []string   // distinct source names, sorted
	LastVerified time.Time  // newest ScrapedAt across listings
	Listings     []Listing  // populated on detail reads only
	Externals    []External // external-source records (Supabase/Google Maps); detail reads only
}

// External is a business record from an external dataset (Supabase, sourced
// from Google Maps): the business, its detail, and its price. Attached to a
// canonical business when matched; a business seen only here is Unknown.
type External struct {
	Source        string  // e.g. "supabase"
	PlaceID       string  // Google place id
	Name          string
	Category      string
	Address       string
	Neighborhood  string
	Latitude      *float64
	Longitude     *float64
	GoogleRating  *float64
	GoogleReviews int
	PriceLevel    string // raw "$"/"$$"/"$$$" when present, else ""
	// Estimated price band in whole-unit currency: from numeric estimates
	// when available, else derived from PriceLevel.
	PriceFrom *float64
	PriceTo   *float64
	Currency  string
	ImageURL  string   // primary photo (direct URL)
	Images    []string // all photos (direct URLs)
	Services  []ExternalService
}

// ExternalService is one priced service estimate from the external source.
type ExternalService struct {
	Name       string
	Slug       string
	PriceLow   *float64
	PriceHigh  *float64
	PriceEst   *float64
	Confidence float64
}

// PriceLevelRange maps a symbolic price level to an estimated numeric band by
// the count of currency symbols: 1 → 0–9, 2 → 10–99, 3 → 100–999, 4 →
// 1000–9999. Symbol-agnostic ($/€/£/¥) since sources localize it (Yelp ES
// returns "€€"). Non-price strings ("", "free") return ok=false.
func PriceLevelRange(level string) (from, to float64, ok bool) {
	level = strings.TrimSpace(level)
	if level == "" {
		return 0, 0, false
	}
	for _, r := range level {
		if !strings.ContainsRune("$€£¥", r) {
			return 0, 0, false
		}
	}
	switch len([]rune(level)) {
	case 1:
		return 0, 9, true
	case 2:
		return 10, 99, true
	case 3:
		return 100, 999, true
	case 4:
		return 1000, 9999, true
	default:
		return 0, 0, false
	}
}

// Verticals partition the catalog into independent business domains served by
// separate endpoints/dashboards.
const (
	VerticalGrooming = "grooming" // barbers, hair & beauty salons, nails…
	VerticalServices = "services" // vets, dry-cleaning & laundry
)

// categoryVertical maps a category slug to its vertical.
var categoryVertical = map[string]string{
	"barberia": VerticalGrooming, "peluqueria": VerticalGrooming,
	"salon-de-unas": VerticalGrooming, "cejas-y-pestanas": VerticalGrooming,
	"masajes": VerticalGrooming, "spa": VerticalGrooming,
	"cuidado-de-la-piel": VerticalGrooming, "depilacion": VerticalGrooming,
	"medicina-estetica": VerticalGrooming, "tienda-de-tatuajes": VerticalGrooming,
	"servicios-profesionales": VerticalGrooming, "otro": VerticalGrooming,
	"veterinario": VerticalServices, "veterinaria": VerticalServices,
	"tintoreria": VerticalServices, "lavanderia": VerticalServices,
}

// VerticalOf returns the vertical for a category slug, or "" if unknown.
func VerticalOf(category string) string { return categoryVertical[category] }

// externalSources are non-scraped, API-imported datasets. A business known
// only through these (never via our own crawlers) is "unknown".
var externalSources = map[string]bool{"supabase": true, "yelp": true}

// OnlyExternalSources reports whether every source is an external dataset
// (so we have no first-hand scraped record of the business).
func OnlyExternalSources(sources []string) bool {
	if len(sources) == 0 {
		return false
	}
	for _, s := range sources {
		if !externalSources[s] {
			return false
		}
	}
	return true
}

// Stale reports whether the canonical record is older than StaleAfter.
func (b *Business) Stale(now time.Time) bool {
	return now.Sub(b.LastVerified) > StaleAfter
}

// Address is a postal address; all fields optional.
type Address struct {
	Street     string
	Locality   string
	PostalCode string
	Country    string
}

// Rating aggregates review data. Absent entirely when unrated.
type Rating struct {
	Value       float64
	ReviewCount int
}

// OpeningHours is one opening interval applying to one or more weekdays.
type OpeningHours struct {
	Days   []string // schema.org day names: "Monday", ...
	Opens  string   // "11:00"
	Closes string   // "20:30"
}

// ServiceOffer is a single bookable service with its price.
type ServiceOffer struct {
	Name        string
	Description string
	Price       *float64
	// PriceMin/PriceMax are set when the source prices the service as a
	// range (schema.org minPrice/maxPrice) rather than a single value.
	PriceMin    *float64
	PriceMax    *float64
	Currency    string
	DurationMin *int // service duration in minutes, when published
	ImageURL    string
}

// Review is one customer review as published by a source.
type Review struct {
	Source string  // which source carried it (set during merge)
	Author string
	Rating *float64
	Body   string
	Date   string // ISO date as published, e.g. "2026-05-29"
}

// ListFilter narrows and pages a business listing query.
// Zero value means "everything, first page" (Limit is defaulted by the repo).
type ListFilter struct {
	Category  string
	City      string
	Query     string // case-insensitive substring on name
	MinRating float64
	Vertical  string // "grooming" | "services" | "" (all)
	Geo       *GeoFilter
	Limit     int
	Offset    int
}

// GeoFilter restricts results to a radius around a point.
type GeoFilter struct {
	Lat, Lng float64
	RadiusKm float64
}

// Facet is one value of a browsable dimension with its business count.
type Facet struct {
	Value string
	Count int
}

// Demand summarizes interest in a service type within a city, using review
// engagement as a real proxy for how sought-after businesses are. Search
// volume itself is modeled by the caller from these real signals.
type Demand struct {
	City              string
	Categories        []string
	Businesses        int
	TotalReviews      int
	AvgRating         float64
	ByCategory        []Facet      // business count per category slug
	ReviewsByCategory []Facet      // review total per category slug
	Neighborhoods     []DemandArea // engagement by neighborhood, reviews desc
	Top               []DemandBiz  // most-reviewed businesses, reviews desc
}

// DemandArea is engagement aggregated for one neighborhood.
type DemandArea struct {
	Name       string
	Businesses int
	Reviews    int
}

// DemandBiz is one business ranked by review engagement.
type DemandBiz struct {
	ID       int64
	Name     string
	Category string
	Reviews  int
	Rating   float64
}

// Stats is an aggregate snapshot of the catalog.
type Stats struct {
	Total         int     // all canonical businesses
	Unknown       int     // present only in external sources (sources == ["supabase"])
	WithExternals int     // businesses that have at least one external record
	Sponsored     int     // associated/partner businesses
	Verified      int     // identity-confirmed businesses
	WithGeo       int     // businesses with coordinates
	WithPrice     int     // businesses with a price band
	Rated         int     // businesses with a rating
	AvgRating     float64 // mean rating across rated businesses
	AvgPriceFrom  float64 // mean low price across priced businesses
	AvgPriceTo    float64 // mean high price across priced businesses
	BySource      []Facet // businesses whose sources include each source, count desc
	RatingDist    []Facet // count per whole star, Value "5".."1"
}

// BusinessRepository is the persistence contract, defined at the consumer.
type BusinessRepository interface {
	// UpsertListing stores one source listing, resolving it to an existing
	// canonical business (or creating one) and recomputing the merged view.
	// Returns the canonical business ID.
	UpsertListing(ctx context.Context, l *Listing) (int64, error)
	// SyncExternal stores one external (Supabase/Google Maps) record,
	// resolving it to a canonical business (or creating one) and attaching
	// the external detail. Returns the canonical business ID.
	SyncExternal(ctx context.Context, e *External) (int64, error)
	// GetByID returns the full business with listings, or ErrNotFound.
	GetByID(ctx context.Context, id int64) (*Business, error)
	// List returns a page of businesses and the total match count.
	List(ctx context.Context, f ListFilter) ([]Business, int, error)
	// ListStaleListings returns listings not scraped since the cutoff,
	// for refresh crawls.
	ListStaleListings(ctx context.Context, source string, cutoff time.Time, limit int) ([]Listing, error)
	// CategoryFacets and CityFacets list distinct values with counts,
	// most-populated first — the data behind browse screens. vertical ""
	// spans the whole catalog.
	CategoryFacets(ctx context.Context, vertical string) ([]Facet, error)
	CityFacets(ctx context.Context, vertical string) ([]Facet, error)
	// Stats returns aggregate catalog counts for a vertical ("" = all).
	Stats(ctx context.Context, vertical string) (Stats, error)
	// DemandStats aggregates review engagement for the given city and
	// categories (the real signal behind a demand/interest view).
	DemandStats(ctx context.Context, city string, categories []string) (Demand, error)
}
