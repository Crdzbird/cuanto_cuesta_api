# cuanto_cuesta

MVP prototype of a service-offering catalog (salons, barbers, spas, nail
studios, ...) in the style of Booksy/Wallapop — built as a **multi-source
aggregator**: polite scrapers ingest per-source *listings*, entity resolution
matches listings that describe the same real-world business, and a freshness
merge builds one canonical record per venue with full provenance.

> Prototype only. Scraped content belongs to the businesses and the source
> sites; do not resell or republish it.

## Architecture

```
sources (booksy, treatwell, web) ──> listings ──> entity resolution ──> canonical businesses ──> REST API
          ▲                          (source,      name similarity +     freshest field wins,
          └── crawl (focused BFS      source_id)   geo proximity         weighted ratings
              web discovery)
```

```
cmd/api                  HTTP server (wiring only)
cmd/scraper              multi-source crawler CLI (wiring only)
internal/domain          entities, merge policy, repository contract (stdlib only)
internal/match           entity resolution: diacritic folding, name similarity, geo rules
internal/geo             haversine
internal/scraper         multi-host polite fetcher, robots.txt + Crawl-delay, Source interface
internal/scraper/schemaorg  shared schema.org JSON-LD parser (flat, array, @graph shapes)
internal/scraper/booksy     Booksy sitemap discovery + slug overlay
internal/scraper/treatwell  Treatwell sitemap discovery + @graph venue pages
internal/scraper/web        generic source: any page with LocalBusiness JSON-LD (seed URLs)
internal/scraper/crawl      focused BFS crawler: discovers business pages from seed pages
internal/storage/sqlite     listings + canonical recompute on pure-Go SQLite (no CGO)
internal/api                REST handlers, JSON contract with provenance
```

## Run

```sh
# Crawl both marketplaces (25 businesses each)
go run ./cmd/scraper -db cuanto_cuesta.db -sources booksy,treatwell -limit 25

# Ingest arbitrary business pages from the rest of the web (known URLs)
go run ./cmd/scraper -db cuanto_cuesta.db -sources web -urls seeds.txt

# Discover business pages by crawling outward from seed pages
# (directories, category/city pages, a business's own site)
go run ./cmd/scraper -db cuanto_cuesta.db -sources crawl -urls seeds.txt \
  -crawl-depth 1 -max-pages 50 -per-host-cap 25

# Re-crawl anything older than 30 days (freshness maintenance)
go run ./cmd/scraper -db cuanto_cuesta.db -refresh-older-than 720h

# Recompute all stored businesses with current rules (e.g. city normalization)
go run ./cmd/scraper -db cuanto_cuesta.db -renormalize

# Serve the catalog
go run ./cmd/api -db cuanto_cuesta.db -addr :8080
```

## API (v1)

| Endpoint | Description |
|---|---|
| `GET /healthz` | liveness |
| `GET /v1/businesses` | paged list; filters below |
| `GET /v1/businesses/{id}` | full detail: services + prices, hours, social links, reviews, per-source listings |
| `GET /v1/businesses/{id}/services` | just the menu — for partial refreshes |
| `GET /v1/businesses/{id}/reviews` | just the review sample |
| `GET /v1/categories` | browse index: distinct categories with counts |
| `GET /v1/cities` | browse index: distinct cities with counts |
| `GET /docs` | Swagger UI |
| `GET /openapi.yaml` | OpenAPI 3 contract (embedded in the binary) |

Interactive API docs are served at `/docs` (Swagger UI) against the embedded
`/openapi.yaml` spec.

Detail and subresource responses carry `Last-Modified` (from
`last_verified`) and an `ETag`, honoring `If-Modified-Since` and
`If-None-Match` — a client whose copy is current gets a body-less `304`,
so mobile re-visits cost nothing.

List filters: `category`, `city`, `q` (name substring), `min_rating`,
`lat`+`lng`+`radius_km` (geo radius, sorted by distance, items carry
`distance_km`), `limit` (≤100), `offset`.

List cards carry everything a results screen needs:

```json
{
  "id": 13,
  "name": "Emi Beauty Studio",
  "description": "Lee las opiniones de los clientes y reserva...",
  "price_from": 10, "price_to": 300, "price_currency": "EUR",
  "rating": {"value": 5, "review_count": 248},
  "image_url": "https://...",
  "sources": ["treatwell"],
  "last_verified": "2026-06-12T22:30:00Z",
  "stale": false
}
```

Descriptions fall back to the page's meta description when a source's
JSON-LD lacks one, and `price_range` is derived from the service menu when
the source doesn't publish one. Detail responses add `phone`, `email`,
`payment_accepted`, the full `images[]` gallery, `opening_hours[]`,
`social_links[]`, `reviews[]` (freshest sample per source, with
author/rating/date), `listings[]` — which source said what, and when — and
`services[]`, each with `description`, `price` (or `price_range` when the
source prices a span via minPrice/maxPrice), `currency`, `duration_min`
(parsed from ISO-8601 durations), and `image_url`.

## How data stays fresh and safe

- **Freshest field wins**: the canonical record is recomputed from all
  listings on every write; a new crawl of any source overrides stale values.
- **Weighted ratings**: only the newest snapshot per source counts (no
  double-counting), weighted by review volume across sources.
- **`stale` flag + refresh mode**: anything unverified for 30 days is
  flagged in the API and re-crawlable via `-refresh-older-than`.
- **Conservative matching**: merging requires name similarity plus ~300 m
  geo agreement (or same city + near-identical name). A missed merge costs a
  duplicate row; a false merge corrupts data — thresholds favor the former.

## How the scrapers stay polite

- Discovery walks each site's **published sitemaps**; no search scraping,
  no disallowed `/api/` endpoints.
- Every fetch is checked against the **host's robots.txt** (cached per host),
  rate-limited per host (default 0.5 req/s, lowered further by `Crawl-delay`
  — Treatwell's 5 s is honored automatically), retried with jittered backoff,
  and sent with an identifying User-Agent.
- Parsing reads **schema.org JSON-LD** — structured data the sites publish
  for crawlers — so there are no fragile CSS selectors anywhere.
- The focused crawler adds its own bounds on top: an overall page budget
  (`-max-pages`), a per-host cap so one big site cannot monopolize a crawl,
  a depth limit, and a per-page link cap. Pages without business markup are
  used only as link sources; crawl-discovered listings share the `web`
  source identity, so re-crawls update instead of duplicating.

## Development

```sh
go test -race ./...
golangci-lint run ./...
```

`internal/scraper/*/testdata/*_live.html` (gitignored) are saved live pages;
when present, parser tests also run against them. Refresh with `curl -A
'Mozilla/5.0' <url> -o <path>`.
