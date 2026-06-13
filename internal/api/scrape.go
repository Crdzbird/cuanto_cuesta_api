package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/crdzbird/cuanto_cuesta/internal/ingest"
)

// ScrapeRunner executes one ingest run. Injected so the API package stays
// decoupled from storage wiring and so tests can stub it.
type ScrapeRunner func(ctx context.Context, opts ingest.Options) (ingest.Result, error)

// jobStatus values.
const (
	statusRunning   = "running"
	statusCompleted = "completed"
	statusFailed    = "failed"
)

// job is one scrape run's lifecycle record. All fields are read/written only
// under jobManager.mu, so snapshots handed to callers never race the worker.
type job struct {
	ID         string          `json:"id"`
	Status     string          `json:"status"`
	Options    ingest.Options  `json:"options"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	Result     *ingest.Result  `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
}

// jobManager runs at most one scrape at a time (single-flight): overlapping
// crawls would multiply load on the target sites and break politeness.
type jobManager struct {
	run    ScrapeRunner
	now    func() time.Time
	logger *slog.Logger

	mu      sync.Mutex
	seq     int64
	current *job // most recent run (running or finished)
}

func newJobManager(run ScrapeRunner, logger *slog.Logger) *jobManager {
	return &jobManager{run: run, now: time.Now, logger: logger}
}

// errBusy is returned by start when a scrape is already in progress.
var errBusy = fmt.Errorf("a scrape is already running")

// start launches a scrape unless one is running. Returns a snapshot of the
// new (or already-running) job. LEAK-SAFE: the worker goroutine is owned by
// the manager, terminates when run returns, and records its outcome under mu.
func (m *jobManager) start(opts ingest.Options) (job, error) {
	m.mu.Lock()
	if m.current != nil && m.current.Status == statusRunning {
		snap := *m.current
		m.mu.Unlock()
		return snap, errBusy
	}
	m.seq++
	j := &job{
		ID:        fmt.Sprintf("scrape-%d", m.seq),
		Status:    statusRunning,
		Options:   opts,
		StartedAt: m.now(),
	}
	m.current = j
	snap := *j
	m.mu.Unlock()

	go func() {
		// Detached from any request context so the job outlives the HTTP
		// call; bounded by the run itself.
		res, err := m.run(context.Background(), opts)
		m.mu.Lock()
		defer m.mu.Unlock()
		fin := m.now()
		j.FinishedAt = &fin
		if err != nil {
			j.Status = statusFailed
			j.Error = err.Error()
			m.logger.Error("scrape job failed", "id", j.ID, "err", err)
		} else {
			j.Status = statusCompleted
			j.Result = &res
			m.logger.Info("scrape job complete", "id", j.ID, "saved", res.Saved, "failed", res.Failed)
		}
	}()
	return snap, nil
}

// status returns a snapshot of the most recent job, or false if none has run.
func (m *jobManager) status() (job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		return job{}, false
	}
	return *m.current, true
}

// ---- HTTP layer --------------------------------------------------------------

// scrapeRequest is the POST body; all fields optional (ingest fills defaults).
type scrapeRequest struct {
	Sources          []string `json:"sources"`
	Country          string   `json:"country"`
	City             string   `json:"city"`
	Limit            int      `json:"limit"`
	Concurrency      int      `json:"concurrency"`
	RPS              float64  `json:"rps"`
	RefreshOlderHrs  float64  `json:"refresh_older_than_hours"`
	Seeds            []string `json:"seeds"`
	CrawlDepth       int      `json:"crawl_depth"`
	MaxPages         int      `json:"max_pages"`
	PerHostCap       int      `json:"per_host_cap"`
}

func (r scrapeRequest) toOptions() ingest.Options {
	return ingest.Options{
		Sources:          r.Sources,
		Country:          r.Country,
		City:             r.City,
		Limit:            r.Limit,
		Concurrency:      r.Concurrency,
		RPS:              r.RPS,
		RefreshOlderThan: time.Duration(r.RefreshOlderHrs * float64(time.Hour)),
		Seeds:            r.Seeds,
		CrawlDepth:       r.CrawlDepth,
		MaxPages:         r.MaxPages,
		PerHostCap:       r.PerHostCap,
	}
}

// requireToken enforces the bearer token on admin endpoints. With no token
// configured the endpoints are disabled outright — we never expose an
// unauthenticated scrape trigger, least of all on a public tunnel.
func (h *handlers) requireToken(w http.ResponseWriter, r *http.Request) bool {
	if h.scrapeToken == "" || h.jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "scrape endpoints are disabled (no token configured)")
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(h.scrapeToken)) == 1 {
		return true
	}
	writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
	return false
}

// POST /v1/admin/scrape
func (h *handlers) startScrape(w http.ResponseWriter, r *http.Request) {
	if !h.requireToken(w, r) {
		return
	}
	var req scrapeRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}
	if len(req.Sources) == 0 {
		req.Sources = []string{"booksy", "treatwell"}
	}
	j, err := h.jobs.start(req.toOptions())
	if err == errBusy {
		writeJSON(w, http.StatusConflict, j) // 409 + the in-flight job
		return
	}
	writeJSON(w, http.StatusAccepted, j) // 202 Accepted, job running
}

// GET /v1/admin/scrape
func (h *handlers) scrapeStatus(w http.ResponseWriter, r *http.Request) {
	if !h.requireToken(w, r) {
		return
	}
	j, ok := h.jobs.status()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"status": "idle"})
		return
	}
	writeJSON(w, http.StatusOK, j)
}
