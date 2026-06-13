package scraper

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ErrDisallowed is returned when robots.txt forbids fetching a URL.
var ErrDisallowed = errors.New("disallowed by robots.txt")

// maxBodyBytes caps a single response body. UNTRUSTED: a hostile or broken
// server must not be able to exhaust memory.
const maxBodyBytes = 10 << 20 // 10 MiB

// Fetcher is a polite multi-host HTTP client: identified User-Agent,
// per-host robots.txt (loaded lazily, cached), per-host rate limits that
// honor Crawl-delay, bounded retries with jittered backoff, and transparent
// gunzip of .gz payloads. Safe for concurrent use.
type Fetcher struct {
	client     *http.Client
	userAgent  string
	defaultRPS float64
	maxRetries int

	mu    sync.Mutex
	hosts map[string]*hostState
}

// hostState carries one host's politeness state. robots loading is guarded
// by once so concurrent first fetches to a host trigger a single load.
type hostState struct {
	once    sync.Once
	robots  *Robots // nil if robots.txt was unreachable → allow-all
	limiter *rate.Limiter
}

// NewFetcher builds a Fetcher. rps is the default sustained per-host request
// budget (burst 1: politeness over throughput); a host's Crawl-delay lowers
// its budget further but never raises it.
func NewFetcher(userAgent string, rps float64) *Fetcher {
	return &Fetcher{
		client:     &http.Client{Timeout: 30 * time.Second},
		userAgent:  userAgent,
		defaultRPS: rps,
		maxRetries: 3,
		hosts:      map[string]*hostState{},
	}
}

// Get fetches rawURL, enforcing the host's robots.txt and rate limit, and
// returns the (gunzipped, size-capped) body.
func (f *Fetcher) Get(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url %q: %w", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("unsupported url %q", rawURL)
	}
	st := f.hostState(ctx, u)
	pathQuery := u.Path
	if u.RawQuery != "" {
		pathQuery += "?" + u.RawQuery
	}
	if !st.robots.Allowed(pathQuery) {
		return nil, fmt.Errorf("%q: %w", rawURL, ErrDisallowed)
	}
	return f.get(ctx, rawURL, st.limiter)
}

// hostState returns (creating and initializing if needed) the politeness
// state for the URL's host. The robots fetch itself runs outside the map
// lock and is rate-limited like any request to the host.
func (f *Fetcher) hostState(ctx context.Context, u *url.URL) *hostState {
	f.mu.Lock()
	st, ok := f.hosts[u.Host]
	if !ok {
		st = &hostState{limiter: rate.NewLimiter(rate.Limit(f.defaultRPS), 1)}
		f.hosts[u.Host] = st
	}
	f.mu.Unlock()

	st.once.Do(func() {
		robotsURL := u.Scheme + "://" + u.Host + "/robots.txt"
		body, err := f.get(ctx, robotsURL, st.limiter)
		if err != nil {
			// Unreachable robots.txt (404, network) → allow-all, keep default
			// rate. A 404 is the common, legitimate "no policy" case.
			return
		}
		st.robots = ParseRobots(string(body), f.userAgent)
		if d := st.robots.CrawlDelay; d > 0 {
			hostRPS := 1 / d
			if hostRPS < f.defaultRPS {
				st.limiter.SetLimit(rate.Limit(hostRPS))
			}
		}
	})
	return st
}

func (f *Fetcher) get(ctx context.Context, rawURL string, limiter *rate.Limiter) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= f.maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff with full jitter: 0.5s, 1s, 2s base.
			base := 500 * time.Millisecond << (attempt - 1)
			delay := time.Duration(rand.Int64N(int64(base))) + base/2
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		if err := limiter.Wait(ctx); err != nil {
			return nil, err
		}
		body, retryable, err := f.doOnce(ctx, rawURL)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}
	}
	return nil, fmt.Errorf("get %s: retries exhausted: %w", rawURL, lastErr)
}

func (f *Fetcher) doOnce(ctx context.Context, rawURL string) (body []byte, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, true, err // network errors are retryable
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) //nolint:errcheck // drain for keep-alive
		return nil, true, fmt.Errorf("get %s: status %d", rawURL, resp.StatusCode)
	default:
		return nil, false, fmt.Errorf("get %s: status %d", rawURL, resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, true, fmt.Errorf("read %s: %w", rawURL, err)
	}
	if len(data) > maxBodyBytes {
		return nil, false, fmt.Errorf("read %s: body exceeds %d bytes", rawURL, maxBodyBytes)
	}
	// Sitemap files are often served as literal .gz objects; detect by magic.
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, false, fmt.Errorf("gunzip %s: %w", rawURL, err)
		}
		defer func() { _ = zr.Close() }()
		data, err = io.ReadAll(io.LimitReader(zr, maxBodyBytes+1))
		if err != nil || len(data) > maxBodyBytes {
			return nil, false, fmt.Errorf("gunzip %s: oversized or corrupt: %w", rawURL, err)
		}
	}
	return data, false, nil
}
