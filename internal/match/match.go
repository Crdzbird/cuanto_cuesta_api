// Package match implements entity resolution: deciding whether two listings
// from different sources describe the same real-world business.
package match

import (
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
	"github.com/crdzbird/cuanto_cuesta/internal/geo"
)

// Matching thresholds. Tuned conservatively: a false merge (two different
// businesses collapsed into one) is worse than a missed merge, which only
// costs a duplicate row.
const (
	// maxGeoDistanceKm is how far apart two coordinate pairs may be and
	// still describe the same venue (GPS + geocoding slack).
	maxGeoDistanceKm = 0.3
	// simWithGeo is the name-similarity floor when coordinates agree.
	simWithGeo = 0.5
	// simWithoutGeo is the floor when only city/postal code agree.
	simWithoutGeo = 0.85
)

// Fold lowercases s and removes diacritics ("Salón YöY" → "salon yoy").
// CONCURRENCY-CRITICAL: transform.Chain transformers carry internal state
// and are not safe for concurrent use — build one per call, never share a
// package-level instance.
func Fold(s string) string {
	folder := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	out, _, err := transform.String(folder, s)
	if err != nil {
		out = s // fall back to the raw string; matching degrades gracefully
	}
	return strings.ToLower(out)
}

// Slugify converts free text to a URL-ish slug ("Las Palmas" → "las-palmas").
func Slugify(s string) string {
	folded := Fold(s)
	var sb strings.Builder
	prevDash := true // suppress leading dash
	for _, r := range folded {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			sb.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				sb.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.TrimSuffix(sb.String(), "-")
}

// regions are autonomous-community / region names that sources put in
// addressLocality where a city is expected ("Comunidad de Madrid"). They are
// never the municipality, so CityGuess must not treat them as the city.
// ASSUMES: these multi-word/clearly-regional names don't collide with the
// municipality we want (single-word cities like "Madrid", "Murcia" are kept).
var regions = map[string]bool{
	"comunidad de madrid": true, "cataluna": true, "catalunya": true,
	"islas canarias": true, "andalucia": true, "comunidad valenciana": true,
	"pais vasco": true, "euskadi": true, "galicia": true,
	"castilla y leon": true, "castilla-la mancha": true, "castilla la mancha": true,
	"aragon": true, "region de murcia": true, "extremadura": true,
	"principado de asturias": true, "asturias": true, "cantabria": true,
	"la rioja": true, "comunidad foral de navarra": true, "navarra": true,
	"islas baleares": true, "illes balears": true,
}

// countries are trailing address segments to skip when locating the city.
var countries = map[string]bool{
	"espana": true, "spain": true, "es": true,
	"portugal": true, "france": true, "francia": true,
}

// CityGuess derives a municipality slug from a postal address. Spanish
// addresses anchor the city on the 5-digit postal code
// ("…, 28010, Madrid" or "…08028 Barcelona, España"), so CityGuess finds
// that anchor and takes the town beside it, skipping country and province
// tails. It falls back to the last townlike street segment, then to the
// locality — but only when the locality is an actual city, not a region.
// ASSUMES: a wrong guess only weakens matching/browse grouping; it never
// corrupts other merged fields.
func CityGuess(addr domain.Address) string {
	if c := cityFromStreet(addr.Street); c != "" {
		return c
	}
	if addr.Locality != "" && !regions[Fold(addr.Locality)] {
		return Slugify(addr.Locality)
	}
	return ""
}

func cityFromStreet(street string) string {
	if street == "" {
		return ""
	}
	var segs []string
	for _, s := range strings.Split(street, ",") {
		if s = strings.TrimSpace(s); s != "" {
			segs = append(segs, s)
		}
	}
	// Postal-code anchor: the segment whose first token is a 5-digit code.
	for i, seg := range segs {
		cp, after := leadingPostal(seg)
		if cp == "" {
			continue
		}
		if after != "" && !countries[Fold(after)] {
			return Slugify(after) // "08028 Barcelona" → "barcelona"
		}
		for j := i + 1; j < len(segs); j++ { // "28010", then "Madrid"
			if hasLetter(segs[j]) && !countries[Fold(segs[j])] {
				return Slugify(segs[j])
			}
		}
	}
	// No postal code: the last townlike segment that isn't a country, region,
	// or street line ("Calle Mayor" is not a city).
	for i := len(segs) - 1; i >= 0; i-- {
		s := segs[i]
		if hasLetter(s) && !countries[Fold(s)] && !regions[Fold(s)] && !looksLikeStreet(s) {
			return Slugify(s)
		}
	}
	return ""
}

// streetPrefixes start a street line rather than a city name. Includes
// Spanish and Catalan forms (carrer/avinguda/passeig/plaça).
var streetPrefixes = []string{
	"calle", "c.", "c/", "avenida", "av.", "avda", "avda.", "avinguda",
	"carrer", "carer", "plaza", "pl.", "placa", "passeig", "paseo", "pso",
	"camino", "ronda", "via", "travesia", "travessera", "callejon", "pasaje",
	"pje.", "rua", "carretera", "ctra", "ctra.", "gran via", "poligono",
	"urbanizacion", "local", "nave",
}

// looksLikeStreet reports whether a segment is a street line, not a city.
// A leading house number ("1 Avinguda de…") is stripped before matching, so
// number-first formats are recognized too.
func looksLikeStreet(seg string) bool {
	f := Fold(seg)
	if rest := strings.TrimLeft(f, "0123456789 "); rest != f {
		f = rest
	}
	for _, p := range streetPrefixes {
		if f == p || strings.HasPrefix(f, p+" ") {
			return true
		}
	}
	return false
}

// leadingPostal splits a segment beginning with a 5-digit Spanish postal
// code into the code and the remaining text ("08028 Barcelona" → "08028",
// "Barcelona"; "28010" → "28010", ""). Returns "","" when there is none.
func leadingPostal(seg string) (code, rest string) {
	if len(seg) < 5 {
		return "", ""
	}
	for i := 0; i < 5; i++ {
		if seg[i] < '0' || seg[i] > '9' {
			return "", ""
		}
	}
	if len(seg) > 5 && seg[5] >= '0' && seg[5] <= '9' {
		return "", "" // 6+ digits: not a postal code
	}
	return seg[:5], strings.TrimSpace(seg[5:])
}

func hasLetter(s string) bool {
	return strings.IndexFunc(s, unicode.IsLetter) >= 0
}

// NormalizeCity canonicalizes an already-known city value to a slug
// ("Madrid" → "madrid"); idempotent on slugs.
func NormalizeCity(city string) string {
	return Slugify(city)
}

// nameTokens normalizes a business name into a token set.
func nameTokens(name string) map[string]bool {
	folded := Fold(name)
	tokens := map[string]bool{}
	for _, tok := range strings.FieldsFunc(folded, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		tokens[tok] = true
	}
	return tokens
}

// NameSimilarity scores two business names in [0,1]: the larger of Jaccard
// overlap and containment (containment catches "Forbici" vs "Forbici Men's
// Grooming Atelier" — same business, shorter name on one source).
func NameSimilarity(a, b string) float64 {
	ta, tb := nameTokens(a), nameTokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	inter := 0
	for tok := range ta {
		if tb[tok] {
			inter++
		}
	}
	union := len(ta) + len(tb) - inter
	jaccard := float64(inter) / float64(union)
	containment := float64(inter) / float64(min(len(ta), len(tb)))
	return max(jaccard, containment)
}

// SameBusiness reports whether two listings describe the same establishment.
//
// Rules: with coordinates on both sides, venues within maxGeoDistanceKm and
// moderately similar names match. Without full coordinates, both a matching
// city or postal code AND a near-identical name are required.
func SameBusiness(a, b *domain.Listing) bool {
	sim := NameSimilarity(a.Name, b.Name)
	if sim == 0 {
		return false
	}
	aGeo := a.Latitude != nil && a.Longitude != nil
	bGeo := b.Latitude != nil && b.Longitude != nil
	if aGeo && bGeo {
		dist := geo.HaversineKm(*a.Latitude, *a.Longitude, *b.Latitude, *b.Longitude)
		return dist <= maxGeoDistanceKm && sim >= simWithGeo
	}
	sameLocale := (a.City != "" && Slugify(a.City) == Slugify(b.City)) ||
		(a.Address.PostalCode != "" && a.Address.PostalCode == b.Address.PostalCode)
	return sameLocale && sim >= simWithoutGeo
}
