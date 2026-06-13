package domain

import "testing"

func TestPriceLevelRange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		level    string
		from, to float64
		ok       bool
	}{
		{"$", 0, 9, true},
		{"$$", 10, 99, true},
		{"$$$", 100, 999, true},
		{"$$$$", 1000, 9999, true},
		{"€", 0, 9, true},     // Yelp ES localizes the symbol
		{"€€", 10, 99, true},
		{"€€€", 100, 999, true},
		{"", 0, 0, false},
		{"free", 0, 0, false}, // non-symbol string rejected
	}
	for _, tt := range tests {
		from, to, ok := PriceLevelRange(tt.level)
		if from != tt.from || to != tt.to || ok != tt.ok {
			t.Errorf("PriceLevelRange(%q) = %v,%v,%v want %v,%v,%v",
				tt.level, from, to, ok, tt.from, tt.to, tt.ok)
		}
	}
}
