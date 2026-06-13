// Package domain holds the core entities, merge policy, and repository
// contracts. It imports nothing outside the standard library.
package domain

import (
	"context"
	"errors"
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
	Sponsored    bool      // associated/partner business (demo: deterministic per ID)
	Verified     bool      // identity/legitimacy confirmed (demo: deterministic per ID)
	Sources      []string  // distinct source names, sorted
	LastVerified time.Time // newest ScrapedAt across listings
	Listings     []Listing // populated on detail reads only
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

// BusinessRepository is the persistence contract, defined at the consumer.
type BusinessRepository interface {
	// UpsertListing stores one source listing, resolving it to an existing
	// canonical business (or creating one) and recomputing the merged view.
	// Returns the canonical business ID.
	UpsertListing(ctx context.Context, l *Listing) (int64, error)
	// GetByID returns the full business with listings, or ErrNotFound.
	GetByID(ctx context.Context, id int64) (*Business, error)
	// List returns a page of businesses and the total match count.
	List(ctx context.Context, f ListFilter) ([]Business, int, error)
	// ListStaleListings returns listings not scraped since the cutoff,
	// for refresh crawls.
	ListStaleListings(ctx context.Context, source string, cutoff time.Time, limit int) ([]Listing, error)
	// CategoryFacets and CityFacets list distinct values with counts,
	// most-populated first — the data behind browse screens.
	CategoryFacets(ctx context.Context) ([]Facet, error)
	CityFacets(ctx context.Context) ([]Facet, error)
}
