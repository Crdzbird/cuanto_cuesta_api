package domain

import (
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// MergeListings builds the canonical view of one business from all of its
// source listings. Policy ("avoid outdated data"):
//
//   - Listings are ranked newest-first; every scalar field takes the first
//     non-empty value, so a fresh source always overrides a stale one.
//   - Rating is aggregated across the newest listing of each source,
//     weighted by review count, so one stale review snapshot cannot skew it.
//   - Services and opening hours come from the newest listing that has them
//     (mixing service menus across sources would duplicate entries).
//   - SocialLinks are the deduplicated union.
//
// COMPLEXITY: Time O(n log n + total field sizes), n = len(listings).
func MergeListings(listings []Listing) Business {
	ls := slices.Clone(listings)
	sort.SliceStable(ls, func(i, j int) bool { return ls[i].ScrapedAt.After(ls[j].ScrapedAt) })

	var b Business
	sourceSet := map[string]bool{}
	linkSet := map[string]bool{}

	for _, l := range ls {
		if l.Source != "" && !sourceSet[l.Source] {
			sourceSet[l.Source] = true
			b.Sources = append(b.Sources, l.Source)
		}
		if l.ScrapedAt.After(b.LastVerified) {
			b.LastVerified = l.ScrapedAt
		}
		setIfEmpty(&b.Name, l.Name)
		setIfEmpty(&b.Category, l.Category)
		setIfEmpty(&b.SchemaType, l.SchemaType)
		setIfEmpty(&b.Description, l.Description)
		setIfEmpty(&b.City, l.City)
		setIfEmpty(&b.Address.Street, l.Address.Street)
		setIfEmpty(&b.Address.Locality, l.Address.Locality)
		setIfEmpty(&b.Address.PostalCode, l.Address.PostalCode)
		setIfEmpty(&b.Address.Country, strings.ToLower(l.Address.Country))
		setIfEmpty(&b.PriceRange, l.PriceRange)
		setIfEmpty(&b.Phone, l.Phone)
		setIfEmpty(&b.Email, l.Email)
		setIfEmpty(&b.Payment, l.Payment)
		setIfEmpty(&b.ImageURL, l.ImageURL)
		setIfEmpty(&b.LogoURL, l.LogoURL)
		if b.Latitude == nil && l.Latitude != nil && l.Longitude != nil {
			b.Latitude, b.Longitude = l.Latitude, l.Longitude
		}
		if len(b.Images) == 0 && len(l.Images) > 0 {
			b.Images = l.Images
		}
		if len(b.Services) == 0 && len(l.Services) > 0 {
			b.Services = l.Services
		}
		if len(b.OpeningHours) == 0 && len(l.OpeningHours) > 0 {
			b.OpeningHours = l.OpeningHours
		}
		for _, link := range l.SocialLinks {
			if link != "" && !linkSet[link] {
				linkSet[link] = true
				b.SocialLinks = append(b.SocialLinks, link)
			}
		}
	}
	slices.Sort(b.Sources)
	b.Rating = aggregateRating(ls)
	b.Reviews = collectReviews(ls)
	b.PriceFrom, b.PriceTo, b.PriceCurrency = priceSummary(b.Services)
	// Fallback: sources without itemized services may carry a price band
	// directly (e.g. a Yelp price level). Use the freshest such listing.
	if b.PriceFrom == nil {
		for _, l := range ls { // ls is sorted newest-first
			if l.PriceFrom != nil && l.PriceTo != nil {
				b.PriceFrom, b.PriceTo, b.PriceCurrency = l.PriceFrom, l.PriceTo, l.PriceCurrency
				break
			}
		}
	}
	// Sources without a published price range (Treatwell) get one derived
	// from the service menu, in the same shape Booksy publishes natively.
	if b.PriceRange == "" && b.PriceFrom != nil {
		b.PriceRange = formatPriceRange(*b.PriceFrom, *b.PriceTo, b.PriceCurrency)
	}
	return b
}

func formatPriceRange(from, to float64, currency string) string {
	s := formatPrice(from)
	if to != from {
		s += " - " + formatPrice(to)
	}
	if currency != "" {
		s = currency + " " + s
	}
	return s
}

func formatPrice(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// maxMergedReviews caps the review sample kept on the canonical record.
const maxMergedReviews = 10

// collectReviews gathers sample reviews from the newest listing of each
// source (older snapshots of the same source would duplicate them), tags
// provenance, and returns them newest-first.
func collectReviews(newestFirst []Listing) []Review {
	seen := map[string]bool{}
	var out []Review
	for _, l := range newestFirst {
		if seen[l.Source] || len(l.Reviews) == 0 {
			continue
		}
		seen[l.Source] = true
		for _, rv := range l.Reviews {
			rv.Source = l.Source
			out = append(out, rv)
		}
	}
	// ISO dates sort lexicographically; empty dates sink to the end.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	if len(out) > maxMergedReviews {
		out = out[:maxMergedReviews]
	}
	return out
}

// priceSummary derives a short, representative price band for list-card UX.
// Service menus often include a few outliers (multi-session bonos, premium
// add-ons) that blow the raw min–max out to something like 10–300; that's
// noise, not a useful "how much does it cost". So we trim ~10% off each end
// (when there are enough services) and round to whole units, yielding a tight
// band like 15–45. A currency is reported only when services agree on one.
func priceSummary(services []ServiceOffer) (from, to *float64, currency string) {
	var prices []float64
	mixed := false
	for _, svc := range services {
		if svc.Price != nil {
			prices = append(prices, *svc.Price)
		}
		if svc.Currency != "" {
			switch {
			case mixed:
			case currency == "":
				currency = svc.Currency
			case svc.Currency != currency:
				currency, mixed = "", true // mixed currencies: report none rather than lie
			}
		}
	}
	if len(prices) == 0 {
		return nil, nil, currency
	}
	sort.Float64s(prices)
	lo, hi := 0, len(prices)-1
	if len(prices) >= 4 {
		// Interquartile band (middle 50%): collapses outlier-driven spans
		// like 3–2100 to the typical cost, e.g. 20–45.
		lo = len(prices) / 4
		hi = len(prices) * 3 / 4
		if hi > len(prices)-1 {
			hi = len(prices) - 1
		}
	}
	f := math.Round(prices[lo])
	t := math.Round(prices[hi])
	return &f, &t, currency
}

// aggregateRating combines the newest rated listing of each source into a
// review-count-weighted average, so a source with 248 reviews outweighs one
// with 5, and an older snapshot of the same source never double-counts.
func aggregateRating(newestFirst []Listing) *Rating {
	seen := map[string]bool{}
	var weighted float64
	var reviews int
	for _, l := range newestFirst {
		if l.Rating == nil || seen[l.Source] {
			continue
		}
		seen[l.Source] = true
		w := max(l.Rating.ReviewCount, 1)
		weighted += l.Rating.Value * float64(w)
		reviews += w
	}
	if reviews == 0 {
		return nil
	}
	return &Rating{Value: weighted / float64(reviews), ReviewCount: reviews}
}

func setIfEmpty(dst *string, v string) {
	if *dst == "" && v != "" {
		*dst = strings.TrimSpace(v)
	}
}
