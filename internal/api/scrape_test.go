package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crdzbird/cuanto_cuesta/internal/ingest"
)

// newScrapeServer builds a server exposing only the admin scrape endpoints,
// with the given token and runner.
func newScrapeServer(t *testing.T, token string, runner ScrapeRunner) *httptest.Server {
	t.Helper()
	h := &handlers{logger: slog.New(slog.DiscardHandler), now: time.Now, scrapeToken: token}
	if runner != nil {
		h.jobs = newJobManager(runner, h.logger)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/admin/scrape", h.startScrape)
	mux.HandleFunc("GET /v1/admin/scrape", h.scrapeStatus)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestScrapeDisabledWithoutToken(t *testing.T) {
	t.Parallel()
	srv := newScrapeServer(t, "", nil)
	resp := post(t, srv.URL+"/v1/admin/scrape", "", "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when no token configured", resp.StatusCode)
	}
}

func TestScrapeRequiresToken(t *testing.T) {
	t.Parallel()
	runner := func(context.Context, ingest.Options) (ingest.Result, error) { return ingest.Result{}, nil }
	srv := newScrapeServer(t, "secret", runner)

	if resp := post(t, srv.URL+"/v1/admin/scrape", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", resp.StatusCode)
	}
	if resp := post(t, srv.URL+"/v1/admin/scrape", "wrong", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", resp.StatusCode)
	}
}

func TestScrapeStartStatusAndSingleFlight(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var calls int32
	runner := func(_ context.Context, opts ingest.Options) (ingest.Result, error) {
		atomic.AddInt32(&calls, 1)
		<-release // hold the job "running" until the test allows it to finish
		return ingest.Result{Saved: 7, BySource: map[string]ingest.SourceResult{"booksy": {Saved: 7}}}, nil
	}
	srv := newScrapeServer(t, "secret", runner)

	// Start a job.
	start := post(t, srv.URL+"/v1/admin/scrape", "secret", `{"sources":["booksy"],"limit":5}`)
	if start.StatusCode != http.StatusAccepted {
		t.Fatalf("start status = %d, want 202", start.StatusCode)
	}
	var started job
	if err := json.NewDecoder(start.Body).Decode(&started); err != nil {
		t.Fatal(err)
	}
	if started.Status != statusRunning || started.ID == "" {
		t.Fatalf("started job = %+v", started)
	}

	// Second start while running → 409, no second invocation.
	busy := post(t, srv.URL+"/v1/admin/scrape", "secret", "")
	if busy.StatusCode != http.StatusConflict {
		t.Errorf("concurrent start status = %d, want 409", busy.StatusCode)
	}

	// Status reports running.
	if st := getScrapeStatus(t, srv); st.Status != statusRunning {
		t.Errorf("status = %q, want running", st.Status)
	}

	// Let it finish and wait for completion.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	var final job
	for time.Now().Before(deadline) {
		final = getScrapeStatus(t, srv)
		if final.Status == statusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if final.Status != statusCompleted {
		t.Fatalf("final status = %q, want completed", final.Status)
	}
	if final.Result == nil || final.Result.Saved != 7 || final.FinishedAt == nil {
		t.Errorf("final job = %+v", final)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("runner invoked %d times, want exactly 1 (single-flight)", got)
	}
}

func TestScrapeRunnerError(t *testing.T) {
	t.Parallel()
	runner := func(context.Context, ingest.Options) (ingest.Result, error) {
		return ingest.Result{}, errors.New("boom")
	}
	srv := newScrapeServer(t, "secret", runner)
	post(t, srv.URL+"/v1/admin/scrape", "secret", `{"sources":["booksy"]}`)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st := getScrapeStatus(t, srv); st.Status == statusFailed {
			if st.Error == "" {
				t.Error("failed job has no error message")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job never reached failed status")
}

func getScrapeStatus(t *testing.T, srv *httptest.Server) job {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/admin/scrape", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var j job
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
		t.Fatal(err)
	}
	return j
}
