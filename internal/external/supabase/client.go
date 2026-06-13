// Package supabase reads the external business + price dataset (sourced from
// Google Maps) from a Supabase project via its PostgREST API, and assembles
// domain.External records. It is read-only: we never write to Supabase.
package supabase

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
)

const sourceName = "supabase"

// maxBodyBytes caps a PostgREST response. UNTRUSTED: external data.
const maxBodyBytes = 16 << 20

// Client talks to a Supabase project's REST API.
type Client struct {
	baseURL string
	key     string
	http    *http.Client
}

// New builds a client. baseURL is the project URL
// (https://<ref>.supabase.co); key is the service_role or anon key.
func New(baseURL, key string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		key:     key,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// get fetches one PostgREST table query and decodes the JSON array into dst.
func (c *Client) get(ctx context.Context, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/rest/v1/"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("apikey", c.key)
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("supabase get %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("supabase read %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("supabase get %s: status %d: %s", path, resp.StatusCode, truncate(body))
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("supabase decode %s: %w", path, err)
	}
	return nil
}

// ---- table row shapes (only the columns we use) -----------------------------

type businessRow struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Lat           *float64 `json:"lat"`
	Lng           *float64 `json:"lng"`
	Neighborhood  string   `json:"neighborhood"`
	Category      string   `json:"category"`
	GooglePlaceID string   `json:"google_place_id"`
	Address       string   `json:"address"`
	GoogleRating  *float64 `json:"google_rating"`
	GoogleReviews int      `json:"google_reviews"`
	PriceLevel    string   `json:"google_price_level"`
}

type estimateRow struct {
	BusinessID  string   `json:"business_id"`
	ServiceSlug string   `json:"service_slug"`
	ServiceName string   `json:"service_name"`
	PriceLow    *float64 `json:"price_low"`
	PriceHigh   *float64 `json:"price_high"`
	PriceEst    *float64 `json:"price_estimate"`
	Confidence  float64  `json:"confidence"`
}

// googleCategoryToSlug aligns Google/Supabase category names with our slugs so
// externals share the catalog's category filter and labels.
var googleCategoryToSlug = map[string]string{
	"barber":          "barberia",
	"hair_salon":      "peluqueria",
	"hairdresser":     "peluqueria",
	"nail_salon":      "salon-de-unas",
	"spa":             "spa",
	"beauty_salon":    "peluqueria",
	"veterinary_care": "veterinario",
	"vet":             "veterinario",
	"dry_cleaner":     "tintoreria",
	"laundry":         "lavanderia",
}

// FetchExternals returns every external business with its price estimates
// folded in. limit<=0 fetches all.
func (c *Client) FetchExternals(ctx context.Context, limit int) ([]domain.External, error) {
	bizPath := "businesses?select=*&order=name"
	if limit > 0 {
		bizPath += "&limit=" + url.QueryEscape(fmt.Sprint(limit))
	}
	var bizs []businessRow
	if err := c.get(ctx, bizPath, &bizs); err != nil {
		return nil, err
	}
	var ests []estimateRow
	if err := c.get(ctx, "price_estimate?select=business_id,service_slug,service_name,price_low,price_high,price_estimate,confidence", &ests); err != nil {
		return nil, err
	}

	estByBiz := map[string][]estimateRow{}
	for _, e := range ests {
		estByBiz[e.BusinessID] = append(estByBiz[e.BusinessID], e)
	}

	out := make([]domain.External, 0, len(bizs))
	for _, b := range bizs {
		ext := domain.External{
			Source:        sourceName,
			PlaceID:       b.GooglePlaceID,
			Name:          strings.TrimSpace(b.Name),
			Category:      categorySlug(b.Category),
			Address:       b.Address,
			Neighborhood:  b.Neighborhood,
			Latitude:      b.Lat,
			Longitude:     b.Lng,
			GoogleRating:  b.GoogleRating,
			GoogleReviews: b.GoogleReviews,
			PriceLevel:    b.PriceLevel,
			Currency:      "EUR",
		}
		ext.Services = dedupeServices(estByBiz[b.ID])
		ext.PriceFrom, ext.PriceTo = priceBand(ext.Services, b.PriceLevel)
		out = append(out, ext)
	}
	return out, nil
}

func categorySlug(google string) string {
	if s, ok := googleCategoryToSlug[strings.ToLower(strings.TrimSpace(google))]; ok {
		return s
	}
	return strings.ToLower(strings.TrimSpace(google))
}

// dedupeServices collapses the multiple estimate rows per service into one
// per service slug, keeping the highest-confidence estimate.
func dedupeServices(rows []estimateRow) []domain.ExternalService {
	best := map[string]estimateRow{}
	for _, r := range rows {
		cur, ok := best[r.ServiceSlug]
		if !ok || r.Confidence > cur.Confidence {
			best[r.ServiceSlug] = r
		}
	}
	out := make([]domain.ExternalService, 0, len(best))
	for _, r := range best {
		out = append(out, domain.ExternalService{
			Name: r.ServiceName, Slug: r.ServiceSlug,
			PriceLow: r.PriceLow, PriceHigh: r.PriceHigh,
			PriceEst: r.PriceEst, Confidence: r.Confidence,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// priceBand derives a numeric price band: the span of per-service estimates
// when present, otherwise the Google price-level mapping.
func priceBand(services []domain.ExternalService, level string) (from, to *float64) {
	var lo, hi *float64
	for _, s := range services {
		if s.PriceLow != nil && (lo == nil || *s.PriceLow < *lo) {
			v := *s.PriceLow
			lo = &v
		}
		if s.PriceHigh != nil && (hi == nil || *s.PriceHigh > *hi) {
			v := *s.PriceHigh
			hi = &v
		}
	}
	if lo != nil && hi != nil {
		return lo, hi
	}
	if f, t, ok := domain.PriceLevelRange(level); ok {
		return &f, &t
	}
	return nil, nil
}

func truncate(b []byte) string {
	const n = 200
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}
