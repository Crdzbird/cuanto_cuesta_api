// Package foursquare reads business data from the Foursquare Places API
// (the 2025 host places-api.foursquare.com). It assembles domain.External
// records with photos, ratings and price tiers — the Premium fields require
// account credits.
package foursquare

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
)

const (
	sourceName = "foursquare"
	apiBase    = "https://places-api.foursquare.com"
	apiVersion = "2025-06-17"
	pageLimit  = 50 // Foursquare max per page
)

// DefaultQueries target the services vertical (vets + dry-cleaning/laundry).
var DefaultQueries = []string{"veterinario", "tintorería", "lavandería"}

// Client calls the Foursquare Places API with a service key.
type Client struct {
	key      string
	location string
	http     *http.Client
}

// New builds a client. location is a Places "near" string, e.g.
// "Valencia, Spain".
func New(key, location string) *Client {
	if location == "" {
		location = "Valencia, Spain"
	}
	return &Client{key: key, location: location, http: &http.Client{Timeout: 30 * time.Second}}
}

type searchResponse struct {
	Results []placeJSON `json:"results"`
}

type placeJSON struct {
	ID       string   `json:"fsq_place_id"`
	Name     string   `json:"name"`
	Lat      *float64 `json:"latitude"`
	Lng      *float64 `json:"longitude"`
	Rating   *float64 `json:"rating"` // 0–10
	Price    *int     `json:"price"`  // 1–4
	Tel      string   `json:"tel"`
	Website  string   `json:"website"`
	Location struct {
		FormattedAddress string `json:"formatted_address"`
		Locality         string `json:"locality"`
	} `json:"location"`
	Categories []struct {
		Name string `json:"name"`
	} `json:"categories"`
	Photos []struct {
		Prefix string `json:"prefix"`
		Suffix string `json:"suffix"`
	} `json:"photos"`
}

// FetchExternals queries each term and returns deduplicated externals.
func (c *Client) FetchExternals(ctx context.Context, queries []string, limitPerQuery int) ([]domain.External, error) {
	if len(queries) == 0 {
		queries = DefaultQueries
	}
	seen := map[string]bool{}
	var out []domain.External
	for _, q := range queries {
		got, err := c.search(ctx, q, limitPerQuery)
		if err != nil {
			return nil, err
		}
		for _, p := range got {
			slug := categorySlug(p.Categories)
			if p.ID == "" || seen[p.ID] || slug == "" {
				continue // skip dupes and off-target categories
			}
			seen[p.ID] = true
			out = append(out, toExternal(p, slug))
		}
	}
	return out, nil
}

func (c *Client) search(ctx context.Context, query string, limit int) ([]placeJSON, error) {
	if limit <= 0 || limit > pageLimit {
		limit = pageLimit
	}
	q := url.Values{}
	q.Set("query", query)
	q.Set("near", c.location)
	q.Set("limit", fmt.Sprint(limit))
	q.Set("fields", "fsq_place_id,name,latitude,longitude,rating,price,tel,website,location,categories,photos")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/places/search?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("X-Places-Api-Version", apiVersion)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("foursquare search %q: %w", query, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("foursquare read %q: %w", query, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("foursquare: invalid service key (401)")
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("foursquare: out of API credits / rate limited (429)")
	default:
		return nil, fmt.Errorf("foursquare search %q: status %d: %s", query, resp.StatusCode, truncate(body))
	}
	var sr searchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("foursquare decode %q: %w", query, err)
	}
	return sr.Results, nil
}

func toExternal(p placeJSON, slug string) domain.External {
	e := domain.External{
		Source:       sourceName,
		PlaceID:      p.ID,
		Name:         strings.TrimSpace(p.Name),
		Category:     slug,
		Address:      p.Location.FormattedAddress,
		Neighborhood: p.Location.Locality,
		Latitude:     p.Lat,
		Longitude:    p.Lng,
		Currency:     "EUR",
	}
	if p.Rating != nil { // Foursquare rates 0–10; normalize to our 0–5
		v := *p.Rating / 2
		e.GoogleRating = &v
	}
	if p.Price != nil && *p.Price >= 1 && *p.Price <= 4 {
		e.PriceLevel = strings.Repeat("$", *p.Price)
		if f, t, ok := domain.PriceLevelRange(e.PriceLevel); ok {
			e.PriceFrom, e.PriceTo = &f, &t
		}
	}
	for _, ph := range p.Photos {
		if ph.Prefix != "" && ph.Suffix != "" {
			e.Images = append(e.Images, ph.Prefix+"original"+ph.Suffix)
		}
	}
	if len(e.Images) > 0 {
		e.ImageURL = e.Images[0]
	}
	return e
}

// categorySlug maps Foursquare category names to our internal slugs.
func categorySlug(cats []struct {
	Name string `json:"name"`
}) string {
	for _, c := range cats {
		n := strings.ToLower(c.Name)
		switch {
		case strings.Contains(n, "veterinar"):
			return "veterinario"
		case strings.Contains(n, "dry clean"):
			return "tintoreria"
		case strings.Contains(n, "laundr"):
			return "lavanderia"
		case strings.Contains(n, "barber"):
			return "barberia"
		case strings.Contains(n, "nail"):
			return "salon-de-unas"
		case strings.Contains(n, "hair") || strings.Contains(n, "salon"):
			return "peluqueria"
		}
	}
	return ""
}

func truncate(b []byte) string {
	const n = 200
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}
