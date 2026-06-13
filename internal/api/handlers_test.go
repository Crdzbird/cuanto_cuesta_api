package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
)

// fakeRepo serves a fixed business set without a database.
type fakeRepo struct {
	businesses map[int64]*domain.Business
}

func (f *fakeRepo) UpsertListing(context.Context, *domain.Listing) (int64, error) {
	panic("not used in API tests")
}

func (f *fakeRepo) SyncExternal(context.Context, *domain.External) (int64, error) {
	panic("not used in API tests")
}

func (f *fakeRepo) GetByID(_ context.Context, id int64) (*domain.Business, error) {
	b, ok := f.businesses[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return b, nil
}

func (f *fakeRepo) List(_ context.Context, _ domain.ListFilter) ([]domain.Business, int, error) {
	var out []domain.Business
	for _, b := range f.businesses {
		out = append(out, *b)
	}
	return out, len(out), nil
}

func (f *fakeRepo) ListStaleListings(context.Context, string, time.Time, int) ([]domain.Listing, error) {
	return nil, nil
}

func (f *fakeRepo) CategoryFacets(context.Context, string) ([]domain.Facet, error) {
	return []domain.Facet{{Value: "barberia", Count: 2}, {Value: "salon-de-unas", Count: 1}}, nil
}

func (f *fakeRepo) CityFacets(context.Context, string) ([]domain.Facet, error) {
	return []domain.Facet{{Value: "madrid", Count: 3}}, nil
}

func (f *fakeRepo) DemandStats(context.Context, string, []string) (domain.Demand, error) {
	return domain.Demand{
		City: "valencia", Categories: []string{"barberia", "peluqueria"},
		Businesses: 80, TotalReviews: 12000, AvgRating: 4.9,
		ByCategory:        []domain.Facet{{Value: "barberia", Count: 69}, {Value: "peluqueria", Count: 11}},
		ReviewsByCategory: []domain.Facet{{Value: "barberia", Count: 9000}, {Value: "peluqueria", Count: 3000}},
		Neighborhoods:     []domain.DemandArea{{Name: "Ruzafa", Businesses: 25, Reviews: 8000}},
		Top:               []domain.DemandBiz{{ID: 1, Name: "Fresh Fade", Category: "barberia", Reviews: 660, Rating: 5}},
	}, nil
}

func (f *fakeRepo) Stats(context.Context, string) (domain.Stats, error) {
	return domain.Stats{
		Total: 160, Unknown: 23, WithExternals: 18, Sponsored: 48, Verified: 96,
		BySource: []domain.Facet{{Value: "booksy", Count: 90}, {Value: "supabase", Count: 25}},
	}, nil
}

var testNow = time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	price := 20.0
	repo := &fakeRepo{businesses: map[int64]*domain.Business{
		1: {
			ID: 1, Name: "Forbici", Category: "barberia", City: "madrid",
			Latitude: ptr(40.43), Longitude: ptr(-3.67),
			Rating:       &domain.Rating{Value: 5, ReviewCount: 22},
			Sponsored:    true,
			Verified:     true,
			Sources:      []string{"booksy"},
			LastVerified: testNow.Add(-time.Hour),
			Services: []domain.ServiceOffer{
				{Name: "Corte", Price: &price, Currency: "EUR", DurationMin: ptr(30)},
			},
			Reviews:  []domain.Review{{Source: "booksy", Author: "Lucas", Body: "Excelente", Date: "2026-05-29"}},
			Listings: []domain.Listing{{Source: "booksy", URL: "https://x", ScrapedAt: testNow.Add(-time.Hour)}},
		},
	}}
	h := &handlers{repo: repo, logger: slog.New(slog.DiscardHandler), now: func() time.Time { return testNow }}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/businesses", h.listBusinesses)
	mux.HandleFunc("GET /v1/businesses/{id}", h.getBusiness)
	mux.HandleFunc("GET /v1/businesses/{id}/services", h.getBusinessServices)
	mux.HandleFunc("GET /v1/businesses/{id}/reviews", h.getBusinessReviews)
	mux.HandleFunc("GET /v1/categories", h.listCategories)
	mux.HandleFunc("GET /v1/cities", h.listCities)
	mux.HandleFunc("GET /v1/stats", h.stats)
	mux.HandleFunc("POST /v1/admin/scrape", h.startScrape)
	mux.HandleFunc("GET /v1/admin/scrape", h.scrapeStatus)
	mux.HandleFunc("GET /openapi.yaml", h.openapiYAML)
	mux.HandleFunc("GET /docs", h.docs)
	mux.HandleFunc("GET /dashboard", h.dashboard)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string, header map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestGetBusinessDetail(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	resp := get(t, srv.URL+"/v1/businesses/1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("Last-Modified") == "" {
		t.Error("missing Last-Modified header")
	}
	var body struct {
		Name     string `json:"name"`
		Stale    bool   `json:"stale"`
		Services []struct {
			DurationMin *int `json:"duration_min"`
		} `json:"services"`
		Listings []struct {
			Source string `json:"source"`
		} `json:"listings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Name != "Forbici" || body.Stale {
		t.Errorf("body = %+v", body)
	}
	if len(body.Services) != 1 || body.Services[0].DurationMin == nil || *body.Services[0].DurationMin != 30 {
		t.Errorf("services = %+v", body.Services)
	}
	if len(body.Listings) != 1 || body.Listings[0].Source != "booksy" {
		t.Errorf("listings = %+v", body.Listings)
	}
}

func TestCategoryLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		category, schemaType, want string
	}{
		{"barberia", "HairSalon", "Barbería"},
		{"tienda-de-tatuajes", "LocalBusiness", "Tatuajes"}, // drops "tienda de"
		{"salon-de-unas", "NailSalon", "Uñas"},
		{"", "HairSalon", "Peluquería"},        // no slug → schema-type fallback
		{"", "NailSalon", "Uñas"},              // no slug → schema-type fallback
		{"futuro-servicio", "", "Futuro servicio"}, // unknown → prettified
		{"centro-de-yoga", "", "Yoga"},         // unknown with generic prefix stripped
		{"", "", ""},                            // nothing to label
	}
	for _, tt := range tests {
		if got := categoryLabel(tt.category, tt.schemaType); got != tt.want {
			t.Errorf("categoryLabel(%q,%q) = %q, want %q", tt.category, tt.schemaType, got, tt.want)
		}
	}
}

func TestTrustFlagsSerialized(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	resp := get(t, srv.URL+"/v1/businesses/1", nil)
	var body struct {
		Sponsored bool `json:"sponsored"`
		Verified  bool `json:"verified"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Sponsored || !body.Verified { // fixture business 1 has both true
		t.Errorf("trust flags not serialized: sponsored=%v verified=%v", body.Sponsored, body.Verified)
	}
}

func TestGetBusinessNotFoundAndBadID(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	if resp := get(t, srv.URL+"/v1/businesses/99", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing id: status = %d, want 404", resp.StatusCode)
	}
	if resp := get(t, srv.URL+"/v1/businesses/abc", nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad id: status = %d, want 400", resp.StatusCode)
	}
}

func TestConditionalGet(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	first := get(t, srv.URL+"/v1/businesses/1", nil)
	lastModified := first.Header.Get("Last-Modified")

	// Re-validation with the served timestamp: 304, no body.
	resp := get(t, srv.URL+"/v1/businesses/1", map[string]string{"If-Modified-Since": lastModified})
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation: status = %d, want 304", resp.StatusCode)
	}

	// A client snapshot older than last_verified: full 200.
	stale := testNow.Add(-48 * time.Hour).Format(http.TimeFormat)
	resp = get(t, srv.URL+"/v1/businesses/1", map[string]string{"If-Modified-Since": stale})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("stale client: status = %d, want 200", resp.StatusCode)
	}
}

func TestETagRevalidation(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	first := get(t, srv.URL+"/v1/businesses/1", nil)
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag header")
	}

	tests := []struct {
		name   string
		header string
		want   int
	}{
		{"exact match", etag, http.StatusNotModified},
		{"weak validator", "W/" + etag, http.StatusNotModified},
		{"list with match", `"other", ` + etag, http.StatusNotModified},
		{"star", "*", http.StatusNotModified},
		{"mismatch", `"stale-etag"`, http.StatusOK},
	}
	for _, tt := range tests {
		resp := get(t, srv.URL+"/v1/businesses/1", map[string]string{"If-None-Match": tt.header})
		if resp.StatusCode != tt.want {
			t.Errorf("%s: status = %d, want %d", tt.name, resp.StatusCode, tt.want)
		}
	}

	// If-None-Match mismatch must win over a matching If-Modified-Since.
	resp := get(t, srv.URL+"/v1/businesses/1", map[string]string{
		"If-None-Match":     `"stale-etag"`,
		"If-Modified-Since": first.Header.Get("Last-Modified"),
	})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("If-None-Match precedence: status = %d, want 200", resp.StatusCode)
	}
}

func TestStatsEndpoint(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	resp := get(t, srv.URL+"/v1/stats", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats status = %d", resp.StatusCode)
	}
	var body struct {
		Total   int `json:"total"`
		Unknown struct {
			Count   int     `json:"count"`
			Percent float64 `json:"percent"`
		} `json:"unknown"`
		Known struct {
			Count   int     `json:"count"`
			Percent float64 `json:"percent"`
		} `json:"known"`
		BySource []struct {
			Source  string  `json:"source"`
			Percent float64 `json:"percent"`
		} `json:"by_source"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 160 || body.Unknown.Count != 23 || body.Known.Count != 137 {
		t.Errorf("counts = total %d unknown %d known %d", body.Total, body.Unknown.Count, body.Known.Count)
	}
	// 23/160 = 14.4%, 137/160 = 85.6%; the two shares add to 100.
	if body.Unknown.Percent != 14.4 || body.Known.Percent != 85.6 {
		t.Errorf("percentages = unknown %.1f known %.1f", body.Unknown.Percent, body.Known.Percent)
	}
	if len(body.BySource) == 0 {
		t.Error("by_source is empty")
	}
}

func TestFacetEndpoints(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	var cats struct {
		Total int         `json:"total"`
		Items []facetJSON `json:"items"`
	}
	resp := get(t, srv.URL+"/v1/categories", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("categories status = %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&cats); err != nil {
		t.Fatal(err)
	}
	if cats.Total != 2 || cats.Items[0].Value != "barberia" || cats.Items[0].Count != 2 {
		t.Errorf("categories = %+v", cats)
	}

	resp = get(t, srv.URL+"/v1/cities", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cities status = %d", resp.StatusCode)
	}
}

func TestDocsAndSpec(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	spec := get(t, srv.URL+"/openapi.yaml", nil)
	if spec.StatusCode != http.StatusOK {
		t.Fatalf("openapi.yaml status = %d", spec.StatusCode)
	}
	if ct := spec.Header.Get("Content-Type"); ct != "application/yaml; charset=utf-8" {
		t.Errorf("spec content-type = %q", ct)
	}

	docs := get(t, srv.URL+"/docs", nil)
	if docs.StatusCode != http.StatusOK {
		t.Errorf("docs status = %d", docs.StatusCode)
	}

	dash := get(t, srv.URL+"/dashboard", nil)
	if dash.StatusCode != http.StatusOK {
		t.Errorf("dashboard status = %d", dash.StatusCode)
	}
	if ct := dash.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("dashboard content-type = %q", ct)
	}
}

func TestServicesAndReviewsSubresources(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	var services struct {
		BusinessID int64 `json:"business_id"`
		Total      int   `json:"total"`
		Items      []struct {
			Name  string   `json:"name"`
			Price *float64 `json:"price"`
		} `json:"items"`
	}
	resp := get(t, srv.URL+"/v1/businesses/1/services", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("services status = %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&services); err != nil {
		t.Fatal(err)
	}
	if services.BusinessID != 1 || services.Total != 1 || services.Items[0].Name != "Corte" {
		t.Errorf("services = %+v", services)
	}

	var reviews struct {
		Total int `json:"total"`
		Items []struct {
			Author string `json:"author"`
			Source string `json:"source"`
		} `json:"items"`
	}
	resp = get(t, srv.URL+"/v1/businesses/1/reviews", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reviews status = %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&reviews); err != nil {
		t.Fatal(err)
	}
	if reviews.Total != 1 || reviews.Items[0].Author != "Lucas" || reviews.Items[0].Source != "booksy" {
		t.Errorf("reviews = %+v", reviews)
	}
}
