// Package api exposes the business catalog over REST/JSON.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
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
	Category    string        `json:"category,omitempty"` // raw source slug (used by ?category= filter)
	Label       string        `json:"label,omitempty"`    // clean service-type label, e.g. "Barbería"
	SchemaType  string        `json:"schema_type,omitempty"`
	Description string        `json:"description,omitempty"`
	City        string        `json:"city,omitempty"`
	Address     *addressJSON  `json:"address,omitempty"`
	Location    *locationJSON `json:"location,omitempty"`
	Rating      *ratingJSON   `json:"rating,omitempty"` // aggregated; absent when unrated
	// Tight, outlier-trimmed price band in whole units (the raw, often very
	// wide, source range is intentionally not surfaced here).
	PriceFrom   *int   `json:"price_from,omitempty"`
	PriceTo     *int   `json:"price_to,omitempty"`
	PriceCurrency string `json:"price_currency,omitempty"`
	DistanceKm  *float64      `json:"distance_km,omitempty"` // present on geo-filtered lists
	ImageURL    string        `json:"image_url,omitempty"`
	LogoURL     string        `json:"logo_url,omitempty"`

	Sponsored bool `json:"sponsored"` // associated/partner business
	Verified  bool `json:"verified"`  // identity/legitimacy confirmed
	Unknown   bool `json:"unknown"`   // present only in external sources (Supabase/Google Maps)

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
	Reviews      []reviewJSON   `json:"reviews,omitempty"`  // freshest sample per source
	Listings     []listingJSON  `json:"listings,omitempty"` // per-source provenance
	Externals    []externalJSON `json:"externals,omitempty"` // external (Supabase/Google Maps) records
}

// externalJSON is one external record: the business, its detail, and price.
type externalJSON struct {
	Source        string                `json:"source"`
	PlaceID       string                `json:"place_id,omitempty"`
	Name          string                `json:"name"`
	Label         string                `json:"label,omitempty"`
	Address       string                `json:"address,omitempty"`
	Neighborhood  string                `json:"neighborhood,omitempty"`
	Location      *locationJSON         `json:"location,omitempty"`
	GoogleRating  *int                  `json:"google_rating,omitempty"`
	GoogleReviews int                   `json:"google_reviews,omitempty"`
	PriceLevel    string                `json:"price_level,omitempty"`
	PriceFrom     *int                  `json:"price_from,omitempty"`
	PriceTo       *int                  `json:"price_to,omitempty"`
	PriceCurrency string                `json:"price_currency,omitempty"`
	Services      []externalServiceJSON `json:"services,omitempty"`
}

type externalServiceJSON struct {
	Name     string   `json:"name"`
	PriceLow *float64 `json:"price_low,omitempty"`
	PriceHigh *float64 `json:"price_high,omitempty"`
	Price    *float64 `json:"price_estimate,omitempty"`
}

func toExternalJSON(e domain.External) externalJSON {
	out := externalJSON{
		Source: e.Source, PlaceID: e.PlaceID, Name: e.Name,
		Label:         categoryLabel(e.Category, ""),
		Address:       e.Address,
		Neighborhood:  e.Neighborhood,
		GoogleReviews: e.GoogleReviews,
		PriceLevel:    e.PriceLevel,
		PriceFrom:     roundPtr(e.PriceFrom),
		PriceTo:       roundPtr(e.PriceTo),
		PriceCurrency: e.Currency,
	}
	if e.Latitude != nil && e.Longitude != nil {
		out.Location = &locationJSON{Lat: *e.Latitude, Lng: *e.Longitude}
	}
	if e.GoogleRating != nil {
		out.GoogleRating = roundPtr(e.GoogleRating)
	}
	for _, s := range e.Services {
		out.Services = append(out.Services, externalServiceJSON{
			Name: s.Name, PriceLow: s.PriceLow, PriceHigh: s.PriceHigh, Price: s.PriceEst,
		})
	}
	return out
}

type reviewJSON struct {
	Source string `json:"source"`
	Author string `json:"author,omitempty"`
	Rating *int   `json:"rating,omitempty"` // whole stars, 0–5
	Body   string `json:"body,omitempty"`
	Date   string `json:"date,omitempty"`
}

func toReviewJSON(rv domain.Review) reviewJSON {
	return reviewJSON{
		Source: rv.Source, Author: rv.Author, Body: rv.Body, Date: rv.Date,
		Rating: roundPtr(rv.Rating),
	}
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
	Value       int `json:"value"` // whole stars, 0–5
	ReviewCount int `json:"review_count"`
}

// ratingStars rounds an aggregate rating to a whole star value, clamped 0–5.
func ratingStars(v float64) int {
	r := int(math.Round(v))
	if r < 0 {
		return 0
	}
	if r > 5 {
		return 5
	}
	return r
}

func roundPtr(v *float64) *int {
	if v == nil {
		return nil
	}
	n := int(math.Round(*v))
	return &n
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

// categoryLabels maps Booksy category slugs to a clean, human-readable label
// naming the service itself — no generic container word ("tienda de…").
var categoryLabels = map[string]string{
	"barberia":                "Barbería",
	"peluqueria":              "Peluquería",
	"salon-de-unas":           "Uñas",
	"cejas-y-pestanas":        "Cejas y pestañas",
	"masajes":                 "Masajes",
	"cuidado-de-la-piel":      "Cuidado de la piel",
	"tienda-de-tatuajes":      "Tatuajes",
	"medicina-estetica":       "Medicina estética",
	"depilacion":              "Depilación",
	"spa":                     "Spa",
	"deporte-y-salud":         "Deporte y salud",
	"cuidado-dental":          "Cuidado dental",
	"servicios-profesionales": "Servicios profesionales",
	"servicios-para-mascotas": "Mascotas",
	"otro":                    "Otro",
	// dry-cleaning & vets
	"tintoreria":  "Tintorería",
	"lavanderia":  "Lavandería",
	"veterinario": "Veterinario",
	"veterinaria": "Veterinario",
}

// schemaTypeLabels is the fallback when a listing has no category slug
// (crawl / Treatwell rows), mapping the schema.org @type to a service label.
var schemaTypeLabels = map[string]string{
	"HairSalon":               "Peluquería",
	"NailSalon":               "Uñas",
	"BeautySalon":             "Salón de belleza",
	"DaySpa":                  "Spa",
	"HealthAndBeautyBusiness": "Salud y belleza",
	"VeterinaryCare":          "Veterinario",
	"DryCleaningOrLaundry":    "Tintorería",
}

// categoryLabel returns a clean service label for a business, preferring the
// curated category name, then the schema type, then a prettified slug.
func categoryLabel(category, schemaType string) string {
	if l, ok := categoryLabels[category]; ok {
		return l
	}
	if l, ok := schemaTypeLabels[schemaType]; ok {
		return l
	}
	return prettifyCategory(category)
}

// prettifyCategory de-slugifies an unknown category and strips a leading
// generic container word ("tienda[ de] tatuajes" → "Tatuajes"), so new
// categories still produce a service-focused label.
func prettifyCategory(slug string) string {
	if slug == "" {
		return ""
	}
	s := strings.ReplaceAll(slug, "-", " ")
	for _, p := range []string{"tienda de ", "tienda ", "centro de ", "estudio de ", "salon de "} {
		if rest := strings.TrimPrefix(s, p); rest != s {
			s = rest
			break
		}
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func toBusinessJSON(b *domain.Business, detail bool, now time.Time) businessJSON {
	out := businessJSON{
		ID:           b.ID,
		Name:         b.Name,
		Category:     b.Category,
		Label:        categoryLabel(b.Category, b.SchemaType),
		SchemaType:   b.SchemaType,
		Description:  b.Description,
		City:          b.City,
		PriceFrom:     roundPtr(b.PriceFrom),
		PriceTo:       roundPtr(b.PriceTo),
		PriceCurrency: b.PriceCurrency,
		ImageURL:      b.ImageURL,
		LogoURL:       b.LogoURL,
		Sponsored:     b.Sponsored,
		Verified:      b.Verified,
		Unknown:       b.Unknown,
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
		out.Rating = &ratingJSON{Value: ratingStars(b.Rating.Value), ReviewCount: b.Rating.ReviewCount}
	}
	if detail {
		out.Phone = b.Phone
		out.Email = b.Email
		out.Payment = b.Payment
		out.Images = b.Images
		out.SocialLinks = b.SocialLinks
		for _, rv := range b.Reviews {
			out.Reviews = append(out.Reviews, toReviewJSON(rv))
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
				lj.Rating = &ratingJSON{Value: ratingStars(l.Rating.Value), ReviewCount: l.Rating.ReviewCount}
			}
			out.Listings = append(out.Listings, lj)
		}
		for _, e := range b.Externals {
			out.Externals = append(out.Externals, toExternalJSON(e))
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

// requestVertical resolves the vertical from the path (/v1/grooming/* →
// grooming) or the ?vertical query, defaulting to services (the catalog the
// main endpoints have served since the pivot). "all" disables the filter.
func requestVertical(r *http.Request) string {
	if strings.Contains(r.URL.Path, "/grooming/") {
		return domain.VerticalGrooming
	}
	switch v := r.URL.Query().Get("vertical"); v {
	case domain.VerticalGrooming, domain.VerticalServices:
		return v
	case "all":
		return ""
	default:
		return domain.VerticalServices
	}
}

// GET /v1/businesses
func (h *handlers) listBusinesses(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := domain.ListFilter{
		Category: q.Get("category"),
		City:     q.Get("city"),
		Query:    q.Get("q"),
		Vertical: requestVertical(r),
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
		items = append(items, toReviewJSON(rv))
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
	load func(context.Context, string) ([]domain.Facet, error)) {
	facets, err := load(r.Context(), requestVertical(r))
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

// countPct is a count with its share of the total, as a percentage.
type countPct struct {
	Count   int     `json:"count"`
	Percent float64 `json:"percent"`
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return math.Round(float64(n)/float64(total)*1000) / 10 // 1 decimal place
}

// GET /v1/stats
func (h *handlers) stats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	vert := requestVertical(r)
	s, err := h.repo.Stats(ctx, vert)
	if err != nil {
		h.logger.Error("stats", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	cats, err := h.repo.CategoryFacets(ctx, vert)
	if err != nil {
		h.logger.Error("stats categories", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	cities, err := h.repo.CityFacets(ctx, vert)
	if err != nil {
		h.logger.Error("stats cities", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	known := s.Total - s.Unknown
	srcPct := func(f domain.Facet) map[string]any {
		return map[string]any{"source": f.Value, "count": f.Count, "percent": pct(f.Count, s.Total)}
	}
	bySource := make([]map[string]any, 0, len(s.BySource))
	for _, f := range s.BySource {
		bySource = append(bySource, srcPct(f))
	}
	// Categories with a clean label; cities top 12.
	byCategory := make([]map[string]any, 0, len(cats))
	for _, f := range cats {
		byCategory = append(byCategory, map[string]any{
			"value": f.Value, "label": categoryLabel(f.Value, ""),
			"count": f.Count, "percent": pct(f.Count, s.Total),
		})
	}
	byCity := make([]map[string]any, 0, len(cities))
	for i, f := range cities {
		if i >= 12 {
			break
		}
		byCity = append(byCity, map[string]any{"value": f.Value, "count": f.Count, "percent": pct(f.Count, s.Total)})
	}
	ratingDist := make([]map[string]any, 0, len(s.RatingDist))
	for _, f := range s.RatingDist {
		ratingDist = append(ratingDist, map[string]any{"stars": f.Value, "count": f.Count})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total":          s.Total,
		"unknown":        countPct{Count: s.Unknown, Percent: pct(s.Unknown, s.Total)},
		"known":          countPct{Count: known, Percent: pct(known, s.Total)},
		"with_externals": countPct{Count: s.WithExternals, Percent: pct(s.WithExternals, s.Total)},
		"sponsored":      countPct{Count: s.Sponsored, Percent: pct(s.Sponsored, s.Total)},
		"verified":       countPct{Count: s.Verified, Percent: pct(s.Verified, s.Total)},
		"with_geo":       countPct{Count: s.WithGeo, Percent: pct(s.WithGeo, s.Total)},
		"with_price":     countPct{Count: s.WithPrice, Percent: pct(s.WithPrice, s.Total)},
		"rated":          countPct{Count: s.Rated, Percent: pct(s.Rated, s.Total)},
		"avg_rating":     math.Round(s.AvgRating*10) / 10,
		"avg_price": map[string]any{
			"from": math.Round(s.AvgPriceFrom), "to": math.Round(s.AvgPriceTo), "currency": "EUR",
		},
		"rating_distribution": ratingDist,
		"by_source":           bySource,
		"by_category":         byCategory,
		"by_city":             byCity,
	})
}

// searchesPerReview is the modeling factor turning observed review
// engagement into an illustrative monthly-search estimate. It is a rough
// heuristic, NOT measured search volume — the page labels it as modeled.
const searchesPerReview = 6.0

// monthlySeasonality is an illustrative 12-month interest curve (Jan..Dec):
// a mild dip in deep winter, a climb toward summer, a December uptick.
// Normalized to average 1.0 so it reshapes a total without changing it.
var monthlySeasonality = [12]float64{0.85, 0.88, 0.97, 1.02, 1.10, 1.18, 1.12, 0.90, 1.05, 1.04, 1.00, 1.29}

var monthNames = [12]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}

// priceIntentShare is the modeled fraction of searches phrased as a price
// question ("how much does it cost"). Illustrative, like the search estimate.
const priceIntentShare = 0.18

// priceQueries are the localized price-question phrases and their modeled
// share of price-intent searches in Valencia (Spanish-dominant, with English
// tourists/expats and a notable Ukrainian community). Weights sum to 1.
var priceQueries = []struct {
	Phrase, Lang, Language string
	Weight                 float64
}{
	{"¿Cuánto cuesta?", "es", "Spanish", 0.70},
	{"How much does it cost?", "en", "English", 0.22},
	{"Скільки це коштує?", "uk", "Ukrainian", 0.08},
}

// serviceIntentShare is the modeled fraction of searches that express a
// service/symptom intent (vaccine, sick pet, …) rather than a price question.
const serviceIntentShare = 0.30

type intentTerm struct {
	Term, Lang, Language string
	Weight               float64
}

// vetIntentTerms are service/symptom search terms for the services vertical.
var vetIntentTerms = []intentTerm{
	{"vacuna", "es", "Spanish", 0.18},
	{"vacuna perro", "es", "Spanish", 0.14},
	{"veterinario", "es", "Spanish", 0.14},
	{"medicina", "es", "Spanish", 0.11},
	{"perro enfermo", "es", "Spanish", 0.11},
	{"inyección", "es", "Spanish", 0.08},
	{"vaccine", "en", "English", 0.07},
	{"dog is sick", "en", "English", 0.06},
	{"vet near me", "en", "English", 0.06},
	{"urgencias veterinario", "es", "Spanish", 0.05},
}

// groomingIntentTerms are service search terms for the grooming vertical.
var groomingIntentTerms = []intentTerm{
	{"corte de pelo", "es", "Spanish", 0.20},
	{"barbería cerca", "es", "Spanish", 0.15},
	{"corte de barba", "es", "Spanish", 0.13},
	{"mechas", "es", "Spanish", 0.11},
	{"tinte de pelo", "es", "Spanish", 0.10},
	{"peluquería", "es", "Spanish", 0.10},
	{"haircut near me", "en", "English", 0.08},
	{"fade", "en", "English", 0.07},
	{"corte mujer", "es", "Spanish", 0.06},
}

func intentTermsFor(vertical string) []intentTerm {
	if vertical == domain.VerticalGrooming {
		return groomingIntentTerms
	}
	return vetIntentTerms
}

// demandCategories returns the default category set for a vertical.
func demandCategories(vertical string) []string {
	if vertical == domain.VerticalGrooming {
		return []string{"barberia", "peluqueria", "salon-de-unas"}
	}
	return []string{"tintoreria", "lavanderia", "veterinario", "veterinaria"}
}

// GET /v1/demand?city=valencia&categories=barberia,peluqueria
func (h *handlers) demand(w http.ResponseWriter, r *http.Request) {
	city := r.URL.Query().Get("city")
	if city == "" {
		city = "valencia"
	}
	categories := demandCategories(requestVertical(r))
	if c := r.URL.Query().Get("categories"); c != "" {
		categories = strings.Split(c, ",")
	}

	d, err := h.repo.DemandStats(r.Context(), city, categories)
	if err != nil {
		h.logger.Error("demand", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Modeled search interest derived from real review engagement.
	estMonthly := int(math.Round(float64(d.TotalReviews) * searchesPerReview))
	trend := make([]map[string]any, 12)
	for i := 0; i < 12; i++ {
		trend[i] = map[string]any{
			"month":             monthNames[i],
			"estimated_searches": int(math.Round(float64(estMonthly) * monthlySeasonality[i])),
		}
	}
	labelFacets := func(fs []domain.Facet) []map[string]any {
		out := make([]map[string]any, 0, len(fs))
		for _, f := range fs {
			out = append(out, map[string]any{"value": f.Value, "label": categoryLabel(f.Value, ""), "count": f.Count})
		}
		return out
	}
	neigh := make([]map[string]any, 0, len(d.Neighborhoods))
	for _, a := range d.Neighborhoods {
		neigh = append(neigh, map[string]any{"name": a.Name, "businesses": a.Businesses, "reviews": a.Reviews})
	}
	top := make([]map[string]any, 0, len(d.Top))
	for _, b := range d.Top {
		top = append(top, map[string]any{
			"id": b.ID, "name": b.Name, "label": categoryLabel(b.Category, ""),
			"reviews": b.Reviews, "rating": ratingStars(b.Rating),
		})
	}

	resp := map[string]any{
		"city":       d.City,
		"categories": d.Categories,
		"supply": map[string]any{
			"businesses":  d.Businesses,
			"by_category": labelFacets(d.ByCategory),
		},
		"engagement": map[string]any{ // REAL signals
			"total_reviews":       d.TotalReviews,
			"avg_rating":          math.Round(d.AvgRating*10) / 10,
			"reviews_by_category": labelFacets(d.ReviewsByCategory),
			"by_neighborhood":     neigh,
			"top_businesses":      top,
		},
		"interest": map[string]any{ // MODELED from engagement, not measured
			"estimated_monthly_searches": estMonthly,
			"model":                      fmt.Sprintf("~%.0f searches per review (illustrative)", searchesPerReview),
			"monthly_trend":              trend,
		},
	}

	// query=true adds the price-question breakdown across languages.
	if q := r.URL.Query().Get("query"); q == "true" || q == "1" {
		totalPI := int(math.Round(float64(estMonthly) * priceIntentShare))
		phrases := make([]map[string]any, 0, len(priceQueries))
		for _, pq := range priceQueries {
			phrases = append(phrases, map[string]any{
				"phrase":             pq.Phrase,
				"lang":               pq.Lang,
				"language":           pq.Language,
				"estimated_searches": int(math.Round(float64(totalPI) * pq.Weight)),
				"percent":            math.Round(pq.Weight * 1000) / 10,
			})
		}
		totalSI := int(math.Round(float64(estMonthly) * serviceIntentShare))
		vertTerms := intentTermsFor(requestVertical(r))
		terms := make([]map[string]any, 0, len(vertTerms))
		for _, it := range vertTerms {
			terms = append(terms, map[string]any{
				"term":               it.Term,
				"lang":               it.Lang,
				"language":           it.Language,
				"estimated_searches": int(math.Round(float64(totalSI) * it.Weight)),
				"percent":            math.Round(it.Weight * 1000) / 10,
			})
		}
		resp["queries"] = map[string]any{
			"price_intent_share":            priceIntentShare,
			"total_price_intent_searches":   totalPI,
			"model":                         fmt.Sprintf("~%.0f%% price questions, ~%.0f%% service/symptom terms (illustrative)", priceIntentShare*100, serviceIntentShare*100),
			"phrases":                       phrases,
			"service_intent_share":          serviceIntentShare,
			"total_service_intent_searches": totalSI,
			"intent_terms":                  terms,
		}
	}

	writeJSON(w, http.StatusOK, resp)
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
