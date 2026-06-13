// Package api exposes the business catalog over REST/JSON.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
	"github.com/crdzbird/cuanto_cuesta/internal/geo"
)

// ---- JSON contract -----------------------------------------------------------
// CONTRACT: these shapes are the public v1 API. Additive changes only;
// removing or renaming a field requires /v2.

type businessJSON struct {
	ID          int64         `json:"id"`
	Name        string        `json:"name"`
	Category    string        `json:"category,omitempty"`
	SchemaType  string        `json:"schema_type,omitempty"`
	Description string        `json:"description,omitempty"`
	City        string        `json:"city,omitempty"`
	Address     *addressJSON  `json:"address,omitempty"`
	Location    *locationJSON `json:"location,omitempty"`
	Rating      *ratingJSON   `json:"rating,omitempty"` // aggregated; absent when unrated
	PriceRange  string        `json:"price_range,omitempty"`
	PriceFrom   *float64      `json:"price_from,omitempty"` // cheapest service
	PriceTo     *float64      `json:"price_to,omitempty"`   // priciest service
	PriceCurrency string      `json:"price_currency,omitempty"`
	DistanceKm  *float64      `json:"distance_km,omitempty"` // present on geo-filtered lists
	ImageURL    string        `json:"image_url,omitempty"`
	LogoURL     string        `json:"logo_url,omitempty"`

	Sponsored bool `json:"sponsored"` // associated/partner business
	Verified  bool `json:"verified"`  // identity/legitimacy confirmed

	// Provenance & freshness.
	Sources      []string  `json:"sources"`
	LastVerified time.Time `json:"last_verified"`
	Stale        bool      `json:"stale"` // true when last_verified is older than 30 days

	// Detail-only fields (omitted from list responses).
	Phone        string        `json:"phone,omitempty"`
	Email        string        `json:"email,omitempty"`
	Payment      string        `json:"payment_accepted,omitempty"`
	Images       []string      `json:"images,omitempty"`
	SocialLinks  []string      `json:"social_links,omitempty"`
	OpeningHours []hoursJSON   `json:"opening_hours,omitempty"`
	Services     []serviceJSON `json:"services,omitempty"`
	Reviews      []reviewJSON  `json:"reviews,omitempty"` // freshest sample per source
	Listings     []listingJSON `json:"listings,omitempty"` // per-source provenance
}

type reviewJSON struct {
	Source string   `json:"source"`
	Author string   `json:"author,omitempty"`
	Rating *float64 `json:"rating,omitempty"`
	Body   string   `json:"body,omitempty"`
	Date   string   `json:"date,omitempty"`
}

type addressJSON struct {
	Street     string `json:"street,omitempty"`
	Locality   string `json:"locality,omitempty"`
	PostalCode string `json:"postal_code,omitempty"`
	Country    string `json:"country,omitempty"`
}

type locationJSON struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

type ratingJSON struct {
	Value       float64 `json:"value"`
	ReviewCount int     `json:"review_count"`
}

type hoursJSON struct {
	Days   []string `json:"days"`
	Opens  string   `json:"opens"`
	Closes string   `json:"closes"`
}

type serviceJSON struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Price       *float64 `json:"price,omitempty"`
	PriceRange  string   `json:"price_range,omitempty"` // "15 - 25" when the source prices a range
	Currency    string   `json:"currency,omitempty"`
	DurationMin *int     `json:"duration_min,omitempty"` // service length in minutes
	ImageURL    string   `json:"image_url,omitempty"`
}

func toServiceJSON(svc domain.ServiceOffer) serviceJSON {
	out := serviceJSON{
		Name:        svc.Name,
		Description: svc.Description,
		Price:       svc.Price,
		Currency:    svc.Currency,
		DurationMin: svc.DurationMin,
		ImageURL:    svc.ImageURL,
	}
	if svc.PriceMin != nil && svc.PriceMax != nil {
		out.PriceRange = formatNum(*svc.PriceMin) + " - " + formatNum(*svc.PriceMax)
	}
	return out
}

func formatNum(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// listingJSON is the per-source provenance view: enough to see where each
// fact came from and how fresh it is, without duplicating the whole record.
type listingJSON struct {
	Source     string      `json:"source"`
	URL        string      `json:"url"`
	Rating     *ratingJSON `json:"rating,omitempty"`
	PriceRange string      `json:"price_range,omitempty"`
	Services   int         `json:"services"`
	ScrapedAt  time.Time   `json:"scraped_at"`
}

type listResponseJSON struct {
	Items  []businessJSON `json:"items"`
	Total  int            `json:"total"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

type errorJSON struct {
	Error string `json:"error"`
}

func toBusinessJSON(b *domain.Business, detail bool, now time.Time) businessJSON {
	out := businessJSON{
		ID:           b.ID,
		Name:         b.Name,
		Category:     b.Category,
		SchemaType:   b.SchemaType,
		Description:  b.Description,
		City:          b.City,
		PriceRange:    b.PriceRange,
		PriceFrom:     b.PriceFrom,
		PriceTo:       b.PriceTo,
		PriceCurrency: b.PriceCurrency,
		ImageURL:      b.ImageURL,
		LogoURL:       b.LogoURL,
		Sponsored:     b.Sponsored,
		Verified:      b.Verified,
		Sources:       b.Sources,
		LastVerified:  b.LastVerified,
		Stale:         b.Stale(now),
	}
	if out.Sources == nil {
		out.Sources = []string{}
	}
	if b.Address != (domain.Address{}) {
		out.Address = &addressJSON{
			Street: b.Address.Street, Locality: b.Address.Locality,
			PostalCode: b.Address.PostalCode, Country: b.Address.Country,
		}
	}
	if b.Latitude != nil && b.Longitude != nil {
		out.Location = &locationJSON{Lat: *b.Latitude, Lng: *b.Longitude}
	}
	if b.Rating != nil {
		out.Rating = &ratingJSON{Value: b.Rating.Value, ReviewCount: b.Rating.ReviewCount}
	}
	if detail {
		out.Phone = b.Phone
		out.Email = b.Email
		out.Payment = b.Payment
		out.Images = b.Images
		out.SocialLinks = b.SocialLinks
		for _, rv := range b.Reviews {
			out.Reviews = append(out.Reviews, reviewJSON(rv))
		}
		for _, oh := range b.OpeningHours {
			out.OpeningHours = append(out.OpeningHours, hoursJSON(oh))
		}
		for _, svc := range b.Services {
			out.Services = append(out.Services, toServiceJSON(svc))
		}
		for _, l := range b.Listings {
			lj := listingJSON{
				Source:     l.Source,
				URL:        l.URL,
				PriceRange: l.PriceRange,
				Services:   len(l.Services),
				ScrapedAt:  l.ScrapedAt,
			}
			if l.Rating != nil {
				lj.Rating = &ratingJSON{Value: l.Rating.Value, ReviewCount: l.Rating.ReviewCount}
			}
			out.Listings = append(out.Listings, lj)
		}
	}
	return out
}

// ---- handlers ------------------------------------------------------------------

type handlers struct {
	repo        domain.BusinessRepository
	logger      *slog.Logger
	now         func() time.Time
	scrapeToken string
	jobs        *jobManager // nil when scrape endpoints are disabled
}

// GET /v1/businesses
func (h *handlers) listBusinesses(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := domain.ListFilter{
		Category: q.Get("category"),
		City:     q.Get("city"),
		Query:    q.Get("q"),
	}
	var err error
	if f.MinRating, err = parseFloatParam(q.Get("min_rating"), 0); err != nil {
		writeError(w, http.StatusBadRequest, "min_rating must be a number")
		return
	}
	if f.Limit, err = parseIntParam(q.Get("limit"), 20); err != nil {
		writeError(w, http.StatusBadRequest, "limit must be an integer")
		return
	}
	if f.Offset, err = parseIntParam(q.Get("offset"), 0); err != nil {
		writeError(w, http.StatusBadRequest, "offset must be an integer")
		return
	}
	if f.Limit > 100 {
		f.Limit = 100
	}

	latS, lngS, radS := q.Get("lat"), q.Get("lng"), q.Get("radius_km")
	if latS != "" || lngS != "" || radS != "" {
		if latS == "" || lngS == "" || radS == "" {
			writeError(w, http.StatusBadRequest, "geo filter requires lat, lng and radius_km together")
			return
		}
		lat, err1 := strconv.ParseFloat(latS, 64)
		lng, err2 := strconv.ParseFloat(lngS, 64)
		rad, err3 := strconv.ParseFloat(radS, 64)
		if err1 != nil || err2 != nil || err3 != nil ||
			lat < -90 || lat > 90 || lng < -180 || lng > 180 || rad <= 0 || rad > 500 {
			writeError(w, http.StatusBadRequest, "invalid lat/lng/radius_km (radius up to 500 km)")
			return
		}
		f.Geo = &domain.GeoFilter{Lat: lat, Lng: lng, RadiusKm: rad}
	}

	items, total, err := h.repo.List(r.Context(), f)
	if err != nil {
		h.logger.Error("list businesses", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	now := h.now()
	resp := listResponseJSON{
		Items:  make([]businessJSON, 0, len(items)),
		Total:  total,
		Limit:  f.Limit,
		Offset: f.Offset,
	}
	for i := range items {
		bj := toBusinessJSON(&items[i], false, now)
		if f.Geo != nil && items[i].Latitude != nil && items[i].Longitude != nil {
			d := geo.HaversineKm(f.Geo.Lat, f.Geo.Lng, *items[i].Latitude, *items[i].Longitude)
			bj.DistanceKm = &d
		}
		resp.Items = append(resp.Items, bj)
	}
	writeJSON(w, http.StatusOK, resp)
}

// loadBusiness parses {id}, loads the business, sets Last-Modified from
// last_verified, and handles conditional requests (If-Modified-Since → 304).
// A nil return means the response has already been written.
func (h *handlers) loadBusiness(w http.ResponseWriter, r *http.Request) *domain.Business {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "id must be an integer")
		return nil
	}
	b, err := h.repo.GetByID(r.Context(), id)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "business not found")
		return nil
	}
	if err != nil {
		h.logger.Error("get business", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return nil
	}
	lastModified := b.LastVerified.UTC().Truncate(time.Second)
	etag := fmt.Sprintf("\"%d-%d\"", b.ID, lastModified.Unix())
	w.Header().Set("Last-Modified", lastModified.Format(http.TimeFormat))
	w.Header().Set("ETag", etag)
	// If-None-Match wins over If-Modified-Since (RFC 9110 §13.1.3).
	if match := r.Header.Get("If-None-Match"); match != "" {
		if etagMatches(match, etag) {
			w.WriteHeader(http.StatusNotModified)
			return nil
		}
		return b
	}
	if since, err := http.ParseTime(r.Header.Get("If-Modified-Since")); err == nil &&
		!lastModified.After(since) {
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	return b
}

// etagMatches reports whether an If-None-Match header value matches etag,
// honoring "*", comma-separated lists, and weak validators (W/ prefix).
func etagMatches(header, etag string) bool {
	if strings.TrimSpace(header) == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return true
		}
	}
	return false
}

// GET /v1/businesses/{id}
func (h *handlers) getBusiness(w http.ResponseWriter, r *http.Request) {
	if b := h.loadBusiness(w, r); b != nil {
		writeJSON(w, http.StatusOK, toBusinessJSON(b, true, h.now()))
	}
}

// GET /v1/businesses/{id}/services — just the menu, for partial refreshes.
func (h *handlers) getBusinessServices(w http.ResponseWriter, r *http.Request) {
	b := h.loadBusiness(w, r)
	if b == nil {
		return
	}
	items := make([]serviceJSON, 0, len(b.Services))
	for _, svc := range b.Services {
		items = append(items, toServiceJSON(svc))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"business_id": b.ID,
		"items":       items,
		"total":       len(items),
	})
}

// GET /v1/businesses/{id}/reviews — just the review sample.
func (h *handlers) getBusinessReviews(w http.ResponseWriter, r *http.Request) {
	b := h.loadBusiness(w, r)
	if b == nil {
		return
	}
	items := make([]reviewJSON, 0, len(b.Reviews))
	for _, rv := range b.Reviews {
		items = append(items, reviewJSON(rv))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"business_id": b.ID,
		"items":       items,
		"total":       len(items),
	})
}

// facetJSON is one entry of a browse index.
type facetJSON struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// GET /v1/categories
func (h *handlers) listCategories(w http.ResponseWriter, r *http.Request) {
	h.writeFacets(w, r, "categories", h.repo.CategoryFacets)
}

// GET /v1/cities
func (h *handlers) listCities(w http.ResponseWriter, r *http.Request) {
	h.writeFacets(w, r, "cities", h.repo.CityFacets)
}

func (h *handlers) writeFacets(w http.ResponseWriter, r *http.Request, name string,
	load func(context.Context) ([]domain.Facet, error)) {
	facets, err := load(r.Context())
	if err != nil {
		h.logger.Error("list "+name, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	items := make([]facetJSON, 0, len(facets))
	for _, f := range facets {
		items = append(items, facetJSON(f))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

// GET /healthz
func (h *handlers) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // headers already sent; nothing useful to do on error
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorJSON{Error: msg})
}

func parseFloatParam(s string, def float64) (float64, error) {
	if s == "" {
		return def, nil
	}
	return strconv.ParseFloat(s, 64)
}

func parseIntParam(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	return strconv.Atoi(s)
}
