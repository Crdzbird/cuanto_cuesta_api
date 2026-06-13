package schemaorg

import (
	"strings"
	"testing"
	"time"
)

func TestParseISODurationMinutes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want int
		ok   bool
	}{
		{"PT45M", 45, true},
		{"PT1H", 60, true},
		{"PT1H30M", 90, true},
		{"pt2h", 120, true},
		{"PT90S", 0, false}, // sub-minute only: not useful
		{"45", 0, false},
		{"PT", 0, false},
		{"P1D", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		got, ok := parseISODurationMinutes(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseISODurationMinutes(%q) = %d,%v want %d,%v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

// treatwellStyleHTML mirrors a Treatwell offer: @graph wrapper, itemOffered
// with description + Duration property, string price, and a price-range
// specification on the second offer.
const treatwellStyleHTML = `<html><head>
<meta name="description" content="Reserva online en Studio X.">
<script type="application/ld+json">
{"@context":"https://schema.org","@graph":[{
 "@type":"HealthAndBeautyBusiness",
 "name":"Studio X",
 "geo":{"@type":"GeoCoordinates","latitude":41.64,"longitude":-0.88},
 "hasOfferCatalog":{"@type":"OfferCatalog","itemListElement":[
   {"@type":"OfferCatalog","name":"Manicuras","itemListElement":[
     {"@type":"Offer","price":"19.00","priceCurrency":"EUR","itemOffered":{
       "@type":"Service","name":"Manicura expr&eacute;s",
       "description":"\n'''Incluye:'''\n\n* Limado de u&ntilde;as.\n* Esmalte.\n",
       "additionalProperty":{"@type":"PropertyValue","name":"Duration","value":"PT45M"}}},
     {"@type":"Offer","priceSpecification":{"@type":"PriceSpecification","minPrice":15,"maxPrice":25,"priceCurrency":"EUR"},
      "itemOffered":{"@type":"Service","name":"Pedicura",
       "additionalProperty":[{"@type":"PropertyValue","name":"Duration","value":"PT1H15M"}]}}
   ]}]}}]}
</script></head><body></body></html>`

func TestParseListingOfferDetails(t *testing.T) {
	t.Parallel()
	l, err := ParseListing(strings.NewReader(treatwellStyleHTML), "https://example.com/x", time.Now())
	if err != nil {
		t.Fatalf("ParseListing: %v", err)
	}
	if len(l.Services) != 2 {
		t.Fatalf("Services len = %d, want 2", len(l.Services))
	}

	mani := l.Services[0]
	if mani.Name != "Manicura exprés" {
		t.Errorf("Name = %q (entities not unescaped?)", mani.Name)
	}
	if mani.Description != "Incluye:\nLimado de uñas.\nEsmalte." {
		t.Errorf("Description = %q (markup not cleaned?)", mani.Description)
	}
	if mani.Price == nil || *mani.Price != 19 {
		t.Errorf("Price = %v, want 19", mani.Price)
	}
	if mani.DurationMin == nil || *mani.DurationMin != 45 {
		t.Errorf("DurationMin = %v, want 45", mani.DurationMin)
	}

	pedi := l.Services[1]
	if pedi.PriceMin == nil || *pedi.PriceMin != 15 || pedi.PriceMax == nil || *pedi.PriceMax != 25 {
		t.Errorf("price range = %v..%v, want 15..25", pedi.PriceMin, pedi.PriceMax)
	}
	if pedi.Currency != "EUR" {
		t.Errorf("Currency = %q, want EUR from priceSpecification", pedi.Currency)
	}
	if pedi.DurationMin == nil || *pedi.DurationMin != 75 {
		t.Errorf("DurationMin = %v, want 75 (array-form additionalProperty)", pedi.DurationMin)
	}
}
