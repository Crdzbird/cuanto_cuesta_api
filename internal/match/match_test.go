package match

import (
	"testing"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
)

func ptr(v float64) *float64 { return &v }

func TestFoldAndSlugify(t *testing.T) {
	t.Parallel()
	if got := Fold("Salón YöY"); got != "salon yoy" {
		t.Errorf("Fold = %q", got)
	}
	if got := Slugify("Las Palmas de Gran Canaria"); got != "las-palmas-de-gran-canaria" {
		t.Errorf("Slugify = %q", got)
	}
	if got := Slugify("  Barbería Mónaco!! "); got != "barberia-monaco" {
		t.Errorf("Slugify = %q", got)
	}
}

func TestCityGuess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		addr domain.Address
		want string
	}{
		// Postal code as its own segment: city is the next segment.
		{"cp then city", domain.Address{
			Street:   "Calle del Gral. Pardiñas, 114, 28006, Madrid",
			Locality: "Comunidad de Madrid",
		}, "madrid"},
		// Postal code glued to city, with a country tail.
		{"cp+city, country tail", domain.Address{
			Street: "Carrer de Joan Güell, 52, Sants-Montjuïc, 08028 Barcelona, España",
		}, "barcelona"},
		// City beside postal, province + country after must be skipped.
		{"cp+city, province+country tail", domain.Address{
			Street: "C. Domine, 1, 18600 Motril, Granada, España",
		}, "motril"},
		// No postal code: last townlike segment.
		{"no postal", domain.Address{Street: "C. de Mariano Royo, 2, Zaragoza"}, "zaragoza"},
		// Region in locality must NOT become the city.
		{"region locality ignored", domain.Address{Locality: "Comunidad de Madrid"}, ""},
		// Genuine city in locality is accepted as a fallback.
		{"city locality fallback", domain.Address{Locality: "Vigo"}, "vigo"},
		{"trailing number is not a city", domain.Address{Street: "Calle Mayor, 14"}, ""},
		{"empty", domain.Address{}, ""},
	}
	for _, tt := range tests {
		if got := CityGuess(tt.addr); got != tt.want {
			t.Errorf("%s: CityGuess = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestLeadingPostal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in         string
		code, rest string
	}{
		{"28010", "28010", ""},
		{"08028 Barcelona", "08028", "Barcelona"},
		{"Calle Mayor", "", ""},
		{"123456 X", "", ""}, // six digits: not a postal code
		{"114", "", ""},
	}
	for _, tt := range tests {
		code, rest := leadingPostal(tt.in)
		if code != tt.code || rest != tt.rest {
			t.Errorf("leadingPostal(%q) = %q,%q want %q,%q", tt.in, code, rest, tt.code, tt.rest)
		}
	}
}

func TestSameBusiness(t *testing.T) {
	t.Parallel()
	base := domain.Listing{
		Source: "booksy", Name: "Forbici Men's Grooming Atelier",
		City: "madrid", Latitude: ptr(40.43634), Longitude: ptr(-3.67752),
	}
	tests := []struct {
		name  string
		other domain.Listing
		want  bool
	}{
		{
			"same venue, shorter name, ~30m away",
			domain.Listing{Source: "treatwell", Name: "Forbici", City: "madrid",
				Latitude: ptr(40.43660), Longitude: ptr(-3.67760)},
			true,
		},
		{
			"different venue 5km away, similar generic name",
			domain.Listing{Source: "treatwell", Name: "Forbici Grooming", City: "madrid",
				Latitude: ptr(40.48), Longitude: ptr(-3.70)},
			false,
		},
		{
			"no coords, same city, near-identical name with accents",
			domain.Listing{Source: "web", Name: "Fórbici Men's Grooming Atelier", City: "Madrid"},
			true,
		},
		{
			"no coords, same city, different business",
			domain.Listing{Source: "web", Name: "Barbería Mónaco", City: "madrid"},
			false,
		},
		{
			"no coords, different city, identical name",
			domain.Listing{Source: "web", Name: "Forbici Men's Grooming Atelier", City: "barcelona"},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := base
			if got := SameBusiness(&b, &tt.other); got != tt.want {
				t.Errorf("SameBusiness = %v, want %v (sim=%.2f)",
					got, tt.want, NameSimilarity(b.Name, tt.other.Name))
			}
		})
	}
}
