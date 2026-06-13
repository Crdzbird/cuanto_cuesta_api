// Package yelp reads business data from the Yelp Fusion API (the official,
// ToS-compliant interface — Yelp's HTML is robots-disallowed). It assembles
// domain.External records for categories like veterinarians and dry cleaning.
package yelp

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
	sourceName = "yelp"
	apiBase    = "https://api.yelp.com/v3"
	pageSize   = 50  // Fusion max per page
	maxOffset  = 240 // Fusion paging cap
)

// DefaultCategories are the Yelp Fusion category aliases for this domain.
// NB: the vet alias is "vet" (not "veterinarians"); an invalid alias makes
// Fusion silently ignore the filter and return everything.
var DefaultCategories = []string{"vet", "drycleaning", "laundryservices"}

// yelpToSlug maps Fusion category aliases to our internal category slugs.
var yelpToSlug = map[string]string{
	"veterinarians":   "veterinario",
	"vet":             "veterinario",
	"drycleaning":     "tintoreria",
	"laundryservices": "lavanderia",
	"laundromat":      "lavanderia",
	// grooming vertical
	"barbers":    "barberia",
	"hair":       "peluqueria",
	"hairsalons": "peluqueria",
	"othersalons": "peluqueria",
	"nailsalons": "salon-de-unas",
}

// Client calls the Yelp Fusion API with a bearer key.
type Client struct {
	key      string
	location string
	http     *http.Client
}

// New builds a client. location is a Fusion location string, e.g.
// "Valencia, Spain".
func New(key, location string) *Client {
	if location == "" {
		location = "Valencia, Spain"
	}
	return &Client{key: key, location: location, http: &http.Client{Timeout: 30 * time.Second}}
}

type searchResponse struct {
	Total      int            `json:"total"`
	Businesses []businessJSON `json:"businesses"`
}

type businessJSON struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Rating      *float64 `json:"rating"`
	ReviewCount int      `json:"review_count"`
	Price       string   `json:"price"`     // "$".."$$$$"
	ImageURL    string   `json:"image_url"` // direct photo URL
	Phone       string   `json:"phone"`
	Coordinates struct {
		Latitude  *float64 `json:"latitude"`
		Longitude *float64 `json:"longitude"`
	} `json:"coordinates"`
	Location struct {
		DisplayAddress []string `json:"display_address"`
		City           string   `json:"city"`
	} `json:"location"`
	Categories []struct {
		Alias string `json:"alias"`
		Title string `json:"title"`
	} `json:"categories"`
}

// FetchExternals queries each category and returns deduplicated externals.
// limitPerCategory<=0 fetches up to the Fusion cap. When detailPhotos is set,
// each business is enriched with up to 3 photos from the detail endpoint
// (one extra call per business — within the free tier for a city's worth).
func (c *Client) FetchExternals(ctx context.Context, categories []string, limitPerCategory int, detailPhotos bool) ([]domain.External, error) {
	if len(categories) == 0 {
		categories = DefaultCategories
	}
	seen := map[string]bool{}
	var out []domain.External
	for _, cat := range categories {
		got, err := c.searchCategory(ctx, cat, limitPerCategory)
		if err != nil {
			return nil, err
		}
		for _, b := range got {
			if b.ID == "" || seen[b.ID] || !relevant(b) {
				continue // skip dupes and anything outside our target categories
			}
			seen[b.ID] = true
			out = append(out, toExternal(b))
		}
	}
	if detailPhotos {
		for i := range out {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			photos, err := c.businessPhotos(ctx, out[i].PlaceID)
			if err != nil || len(photos) == 0 {
				continue // keep the single search photo on any failure
			}
			out[i].Images = photos
			out[i].ImageURL = photos[0]
		}
	}
	return out, nil
}

// businessPhotos fetches up to 3 photo URLs from the business detail endpoint.
func (c *Client) businessPhotos(ctx context.Context, id string) ([]string, error) {
	if id == "" {
		return nil, nil
	}
	var resp struct {
		Photos []string `json:"photos"`
	}
	if err := c.get(ctx, "/businesses/"+url.PathEscape(id), &resp); err != nil {
		return nil, err
	}
	return resp.Photos, nil
}

func (c *Client) searchCategory(ctx context.Context, category string, limit int) ([]businessJSON, error) {
	var all []businessJSON
	for offset := 0; offset <= maxOffset; offset += pageSize {
		if limit > 0 && len(all) >= limit {
			break
		}
		q := url.Values{}
		q.Set("location", c.location)
		q.Set("categories", category)
		q.Set("limit", fmt.Sprint(pageSize))
		q.Set("offset", fmt.Sprint(offset))
		q.Set("sort_by", "review_count")

		var resp searchResponse
		if err := c.get(ctx, "/businesses/search?"+q.Encode(), &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Businesses...)
		if len(resp.Businesses) < pageSize || offset+pageSize >= resp.Total {
			break
		}
	}
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (c *Client) get(ctx context.Context, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("yelp get %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("yelp read %s: %w", path, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return fmt.Errorf("yelp: invalid API key (401)")
	case http.StatusTooManyRequests:
		return fmt.Errorf("yelp: rate limited (429)")
	default:
		return fmt.Errorf("yelp get %s: status %d: %s", path, resp.StatusCode, truncate(body))
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("yelp decode %s: %w", path, err)
	}
	return nil
}

func toExternal(b businessJSON) domain.External {
	e := domain.External{
		Source:        sourceName,
		PlaceID:       b.ID,
		Name:          strings.TrimSpace(b.Name),
		Category:      categorySlug(b.Categories),
		Address:       strings.Join(b.Location.DisplayAddress, ", "),
		Neighborhood:  b.Location.City,
		Latitude:      b.Coordinates.Latitude,
		Longitude:     b.Coordinates.Longitude,
		GoogleRating:  b.Rating, // external rating (Yelp stars, 0–5)
		GoogleReviews: b.ReviewCount,
		PriceLevel:    b.Price,
		Currency:      "EUR",
		ImageURL:      b.ImageURL,
	}
	if b.ImageURL != "" {
		e.Images = []string{b.ImageURL}
	}
	if f, t, ok := domain.PriceLevelRange(b.Price); ok {
		e.PriceFrom, e.PriceTo = &f, &t
	}
	return e
}

// relevant reports whether a business belongs to one of our target
// categories — a guard so a wrong/ignored Fusion alias (which makes Yelp
// return unrelated businesses) can never pollute the catalog.
func relevant(b businessJSON) bool {
	for _, c := range b.Categories {
		if _, ok := yelpToSlug[c.Alias]; ok {
			return true
		}
	}
	return false
}

// categorySlug picks our internal slug from a business's Yelp categories,
// preferring a known mapping, else the first category alias.
func categorySlug(cats []struct {
	Alias string `json:"alias"`
	Title string `json:"title"`
}) string {
	for _, c := range cats {
		if slug, ok := yelpToSlug[c.Alias]; ok {
			return slug
		}
	}
	if len(cats) > 0 {
		return cats[0].Alias
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
