package booksy

import "testing"

func TestCityMatches(t *testing.T) {
	t.Parallel()
	const valenciaBiz = "https://booksy.com/es-es/117527_adriana-martinez_peluqueria_58087_valencia"
	tests := []struct {
		name string
		loc  string
		city string
		want bool
	}{
		{"no filter matches all", valenciaBiz, "", true},
		{"exact city", valenciaBiz, "valencia", true},
		{"hair salon in city", "https://booksy.com/es-es/1_x_peluqueria_58087_valencia", "valencia", true},
		{"other city excluded", "https://booksy.com/es-es/1_x_barberia_53009_madrid", "valencia", false},
		{"substring town not matched", "https://booksy.com/es-es/1_x_barberia_99_nueva-valencia", "valencia", false},
	}
	for _, tt := range tests {
		if got := cityMatches(tt.loc, tt.city); got != tt.want {
			t.Errorf("%s: cityMatches(%q,%q) = %v, want %v", tt.name, tt.loc, tt.city, got, tt.want)
		}
	}
}
