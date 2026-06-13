// Package schemaorg parses schema.org JSON-LD business markup out of HTML
// pages. It is the shared engine behind every source: marketplaces embed it
// for SEO, and most business websites and directories carry LocalBusiness
// markup too — which is what lets the crawler ingest "the rest of the web"
// without per-site CSS selectors.
//
// UNTRUSTED: everything here is external input. The parser absorbs the
// shape variance seen in the wild — flat objects, arrays, @graph wrappers,
// string-or-number scalars, string-or-array fields — and never trusts a
// single malformed block to fail a page.
package schemaorg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
)

// ---- flexible JSON scalars -------------------------------------------------

// FlexFloat decodes a JSON number or a numeric string.
type FlexFloat float64

func (f *FlexFloat) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return fmt.Errorf("flex float: %q: %w", s, err)
		}
		*f = FlexFloat(v)
		return nil
	}
	var v float64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*f = FlexFloat(v)
	return nil
}

// FlexString decodes a JSON string or takes the first element of an array.
type FlexString string

func (f *FlexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '[' {
		var arr []string
		if err := json.Unmarshal(b, &arr); err != nil {
			return err
		}
		if len(arr) > 0 {
			*f = FlexString(arr[0])
		}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*f = FlexString(s)
	return nil
}

// FlexStrings decodes a JSON string or an array of strings.
type FlexStrings []string

func (f *FlexStrings) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '[' {
		return json.Unmarshal(b, (*[]string)(f))
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*f = FlexStrings{s}
	return nil
}

// ---- JSON-LD node shape ------------------------------------------------------

type ldNode struct {
	Type        FlexStrings `json:"@type"`
	Graph       []ldNode    `json:"@graph"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	URL         string      `json:"url"`
	Image       FlexStrings `json:"image"`
	Logo        FlexString  `json:"logo"`
	PriceRange  string      `json:"priceRange"`
	Telephone   FlexString  `json:"telephone"`
	Email       FlexString  `json:"email"`
	Payment     FlexStrings `json:"paymentAccepted"`
	SameAs      FlexStrings `json:"sameAs"`
	Address     *struct {
		StreetAddress   string `json:"streetAddress"`
		AddressLocality string `json:"addressLocality"`
		PostalCode      string `json:"postalCode"`
		AddressCountry  string `json:"addressCountry"`
	} `json:"address"`
	Geo *struct {
		Latitude  FlexFloat `json:"latitude"`
		Longitude FlexFloat `json:"longitude"`
	} `json:"geo"`
	AggregateRating *struct {
		RatingValue FlexFloat `json:"ratingValue"`
		ReviewCount FlexFloat `json:"reviewCount"`
	} `json:"aggregateRating"`
	OpeningHoursSpecification []struct {
		DayOfWeek FlexStrings `json:"dayOfWeek"`
		Opens     string      `json:"opens"`
		Closes    string      `json:"closes"`
	} `json:"openingHoursSpecification"`
	MakesOffer      json.RawMessage `json:"makesOffer"`
	HasOfferCatalog json.RawMessage `json:"hasOfferCatalog"`
	Review          json.RawMessage `json:"review"`
}

// nonBusinessTypes are @type values that never describe an establishment.
var nonBusinessTypes = map[string]bool{
	"BreadcrumbList": true, "WebSite": true, "WebPage": true,
	"SearchAction": true, "ItemList": true, "FAQPage": true,
	"Organization": false, // an Organization with an address is acceptable
}

// pageData is what one tokenizer pass extracts from an HTML document.
type pageData struct {
	blocks   []string // raw JSON-LD block contents
	metaDesc string   // <meta name="description"> falling back to og:description
	ogDesc   string
}

// ExtractBlocks returns the raw contents of every
// <script type="application/ld+json"> block in the document.
func ExtractBlocks(r io.Reader) ([]string, error) {
	p, err := extractPage(r)
	return p.blocks, err
}

func extractPage(r io.Reader) (pageData, error) {
	z := html.NewTokenizer(r)
	var p pageData
	for {
		switch z.Next() {
		case html.ErrorToken:
			if z.Err() == io.EOF {
				return p, nil
			}
			return p, fmt.Errorf("tokenize html: %w", z.Err())
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if !hasAttr {
				continue
			}
			switch string(name) {
			case "script":
				isLD := false
				for {
					k, v, more := z.TagAttr()
					if bytes.Equal(k, []byte("type")) && bytes.Equal(v, []byte("application/ld+json")) {
						isLD = true
					}
					if !more {
						break
					}
				}
				if isLD && z.Next() == html.TextToken {
					p.blocks = append(p.blocks, string(z.Text()))
				}
			case "meta":
				var metaName, content string
				for {
					k, v, more := z.TagAttr()
					switch string(k) {
					case "name", "property":
						metaName = string(v)
					case "content":
						content = string(v)
					}
					if !more {
						break
					}
				}
				switch metaName {
				case "description":
					p.metaDesc = content
				case "og:description":
					p.ogDesc = content
				}
			}
		}
	}
}

// ParseListing scans an HTML document for the first business-like JSON-LD
// node and converts it. Source/SourceID/Category/City are left for the
// caller to overlay — they are source-specific. The caller passes now so the
// parser stays deterministic under test.
func ParseListing(doc io.Reader, pageURL string, now time.Time) (*domain.Listing, error) {
	page, err := extractPage(doc)
	if err != nil {
		return nil, err
	}
	for _, block := range page.blocks {
		for _, node := range decodeNodes([]byte(block)) {
			l := nodeToListing(node, pageURL, now)
			if l == nil {
				continue
			}
			// Many sites (Treatwell among them) omit description from
			// JSON-LD but publish a meta description for search engines.
			if l.Description == "" {
				l.Description = clean(firstNonEmpty(page.metaDesc, page.ogDesc))
			}
			return l, nil
		}
	}
	return nil, fmt.Errorf("parse %s: no business JSON-LD found", pageURL)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// decodeNodes absorbs the three top-level shapes: object, array, @graph.
func decodeNodes(block []byte) []ldNode {
	block = bytes.TrimSpace(block)
	var nodes []ldNode
	if len(block) > 0 && block[0] == '[' {
		_ = json.Unmarshal(block, &nodes) // UNTRUSTED: malformed → empty
	} else {
		var n ldNode
		if json.Unmarshal(block, &n) == nil {
			nodes = []ldNode{n}
		}
	}
	var out []ldNode
	for _, n := range nodes {
		if len(n.Graph) > 0 {
			out = append(out, n.Graph...)
		} else {
			out = append(out, n)
		}
	}
	return out
}

// nodeToListing converts one node, or returns nil if it isn't a business.
func nodeToListing(n ldNode, pageURL string, now time.Time) *domain.Listing {
	typ := ""
	if len(n.Type) > 0 {
		typ = n.Type[0]
	}
	if nonBusinessTypes[typ] || strings.TrimSpace(n.Name) == "" {
		return nil
	}
	// A business node must be locatable or rated; this rejects authors,
	// articles, and products.
	if n.Address == nil && n.Geo == nil && n.AggregateRating == nil {
		return nil
	}

	l := &domain.Listing{
		Name:        clean(n.Name),
		SchemaType:  typ,
		Description: clean(n.Description),
		URL:         pageURL,
		PriceRange:  n.PriceRange,
		Phone:       clean(string(n.Telephone)),
		Email:       clean(string(n.Email)),
		Payment:     strings.Join(n.Payment, ", "),
		Images:      n.Image,
		LogoURL:     string(n.Logo),
		SocialLinks: n.SameAs,
		ScrapedAt:   now.UTC(),
	}
	if len(l.Images) > 0 {
		l.ImageURL = l.Images[0]
	}
	if n.Address != nil {
		l.Address = domain.Address{
			Street:     clean(n.Address.StreetAddress),
			Locality:   clean(n.Address.AddressLocality),
			PostalCode: n.Address.PostalCode,
			Country:    n.Address.AddressCountry,
		}
	}
	if n.Geo != nil {
		lat, lng := float64(n.Geo.Latitude), float64(n.Geo.Longitude)
		if lat >= -90 && lat <= 90 && lng >= -180 && lng <= 180 && (lat != 0 || lng != 0) {
			l.Latitude, l.Longitude = &lat, &lng
		}
	}
	if n.AggregateRating != nil {
		v := float64(n.AggregateRating.RatingValue)
		if v >= 0 && v <= 5 {
			l.Rating = &domain.Rating{Value: v, ReviewCount: int(float64(n.AggregateRating.ReviewCount))}
		}
	}
	for _, oh := range n.OpeningHoursSpecification {
		l.OpeningHours = append(l.OpeningHours, domain.OpeningHours{
			Days: oh.DayOfWeek, Opens: oh.Opens, Closes: oh.Closes,
		})
	}
	l.Services = append(collectOffers(n.MakesOffer), collectOffers(n.HasOfferCatalog)...)
	l.Reviews = collectReviews(n.Review)
	return l
}

// collectReviews extracts sample reviews from a review field, which may be a
// single Review node or an array of them.
func collectReviews(raw json.RawMessage) []domain.Review {
	if len(raw) == 0 {
		return nil
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil
	}
	nodes, ok := root.([]any)
	if !ok {
		nodes = []any{root}
	}
	var out []domain.Review
	for _, node := range nodes {
		m, ok := node.(map[string]any)
		if !ok {
			continue
		}
		rv := domain.Review{
			Body: clean(str(m["reviewBody"])),
			Date: str(m["datePublished"]),
		}
		switch author := m["author"].(type) {
		case string:
			rv.Author = clean(author)
		case map[string]any:
			rv.Author = clean(str(author["name"]))
		}
		if rating, ok := m["reviewRating"].(map[string]any); ok {
			if v, ok := asFloat(rating["ratingValue"]); ok && v >= 0 && v <= 5 {
				rv.Rating = &v
			}
		}
		if rv.Body == "" && rv.Author == "" {
			continue
		}
		out = append(out, rv)
	}
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// collectOffers walks any offer container — a makesOffer array or an
// arbitrarily nested hasOfferCatalog tree — and extracts every Offer node.
func collectOffers(raw json.RawMessage) []domain.ServiceOffer {
	if len(raw) == 0 {
		return nil
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil
	}
	var out []domain.ServiceOffer
	seen := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch node := v.(type) {
		case []any:
			for _, item := range node {
				walk(item)
			}
		case map[string]any:
			if t, _ := node["@type"].(string); t == "Offer" {
				if svc, ok := offerToService(node); ok && !seen[svc.Name] {
					seen[svc.Name] = true
					out = append(out, svc)
				}
				return
			}
			for _, child := range node {
				walk(child)
			}
		}
	}
	walk(root)
	return out
}

// offerToService extracts a service from an Offer node, looking in the offer
// itself, its itemOffered (name, description, duration, image), and its
// priceSpecification (single price or min/max range).
func offerToService(offer map[string]any) (domain.ServiceOffer, bool) {
	var svc domain.ServiceOffer
	svc.Name = clean(str(offer["name"]))
	svc.Description = cleanOfferText(str(offer["description"]))
	svc.ImageURL = str(offer["image"])

	if item, ok := offer["itemOffered"].(map[string]any); ok {
		if svc.Name == "" {
			svc.Name = clean(str(item["name"]))
		}
		if svc.Description == "" {
			svc.Description = cleanOfferText(str(item["description"]))
		}
		if svc.ImageURL == "" {
			svc.ImageURL = str(item["image"])
		}
		svc.DurationMin = durationProperty(item["additionalProperty"])
	}
	if svc.Name == "" {
		return domain.ServiceOffer{}, false
	}
	if cur, ok := offer["priceCurrency"].(string); ok {
		svc.Currency = cur
	}
	if p, ok := asFloat(offer["price"]); ok {
		svc.Price = &p
	}
	if spec, ok := offer["priceSpecification"].(map[string]any); ok {
		if svc.Price == nil {
			if p, ok := asFloat(spec["price"]); ok {
				svc.Price = &p
			}
		}
		if svc.Currency == "" {
			if cur, ok := spec["priceCurrency"].(string); ok {
				svc.Currency = cur
			}
		}
		if p, ok := asFloat(spec["minPrice"]); ok {
			svc.PriceMin = &p
		}
		if p, ok := asFloat(spec["maxPrice"]); ok {
			svc.PriceMax = &p
		}
	}
	return svc, true
}

// durationProperty finds a "Duration" PropertyValue (single node or array)
// and parses its ISO-8601 value ("PT45M", "PT1H30M") into minutes.
func durationProperty(v any) *int {
	props, ok := v.([]any)
	if !ok {
		if m, ok := v.(map[string]any); ok {
			props = []any{m}
		}
	}
	for _, p := range props {
		m, ok := p.(map[string]any)
		if !ok || !strings.EqualFold(str(m["name"]), "duration") {
			continue
		}
		if min, ok := parseISODurationMinutes(str(m["value"])); ok {
			return &min
		}
	}
	return nil
}

// parseISODurationMinutes parses the time part of an ISO-8601 duration
// ("PT45M", "PT1H", "PT1H30M") into whole minutes.
func parseISODurationMinutes(s string) (int, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	rest, ok := strings.CutPrefix(s, "PT")
	if !ok || rest == "" {
		return 0, false
	}
	total := 0
	num := ""
	for _, r := range rest {
		switch {
		case r >= '0' && r <= '9':
			num += string(r)
		case r == 'H' || r == 'M' || r == 'S':
			if num == "" {
				return 0, false
			}
			n, err := strconv.Atoi(num)
			if err != nil {
				return 0, false
			}
			switch r {
			case 'H':
				total += n * 60
			case 'M':
				total += n
			case 'S': // sub-minute precision is noise for a service menu
			}
			num = ""
		default:
			return 0, false
		}
	}
	return total, num == "" && total > 0
}

// cleanOfferText normalizes offer descriptions: HTML entities, the light
// wiki markup some sources embed ('''bold''', * bullets), and whitespace.
func cleanOfferText(s string) string {
	s = clean(s)
	s = strings.ReplaceAll(s, "'''", "")
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, line := range lines {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "* "))
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// clean unescapes HTML entities ("m&aacute;s" → "más") and trims whitespace.
func clean(s string) string {
	return strings.TrimSpace(html.UnescapeString(s))
}
