package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
)

// Config holds optional server dependencies beyond the core repository.
type Config struct {
	// ScrapeToken gates the admin scrape endpoints; empty disables them.
	ScrapeToken string
	// ScrapeRunner executes a scrape; required to enable the admin endpoints.
	ScrapeRunner ScrapeRunner
}

// NewServer wires the routes and returns an http.Server ready to listen.
func NewServer(addr string, repo domain.BusinessRepository, logger *slog.Logger, cfg Config) *http.Server {
	h := &handlers{repo: repo, logger: logger, now: time.Now, scrapeToken: cfg.ScrapeToken}
	if cfg.ScrapeRunner != nil {
		h.jobs = newJobManager(cfg.ScrapeRunner, logger)
	}

	mux := http.NewServeMux()
	// NB: /healthz is reserved by the Cloud Run frontend (it never reaches
	// the container), so the liveness route lives at /health.
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("GET /v1/businesses", h.listBusinesses)
	mux.HandleFunc("GET /v1/businesses/{id}", h.getBusiness)
	mux.HandleFunc("GET /v1/businesses/{id}/services", h.getBusinessServices)
	mux.HandleFunc("GET /v1/businesses/{id}/reviews", h.getBusinessReviews)
	mux.HandleFunc("GET /v1/categories", h.listCategories)
	mux.HandleFunc("GET /v1/cities", h.listCities)
	mux.HandleFunc("POST /v1/admin/scrape", h.startScrape)
	mux.HandleFunc("GET /v1/admin/scrape", h.scrapeStatus)
	mux.HandleFunc("GET /openapi.yaml", h.openapiYAML)
	mux.HandleFunc("GET /docs", h.docs)

	return &http.Server{
		Addr:              addr,
		Handler:           requestLog(logger, mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// requestLog logs method, path, status and latency for every request.
func requestLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
