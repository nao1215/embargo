package detect

import "testing"

func TestValidITINGroupIntervalEdges(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"900500000", true},
		{"900650000", true},
		{"900700000", true},
		{"900880000", true},
		{"900900000", true},
		{"900920000", true},
		{"900940000", true},
		{"900990000", true},
		{"900490000", false},
		{"900660000", false},
		{"900690000", false},
		{"900890000", false},
		{"900930000", false},
		{"800500000", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Parallel()
			if got := validITIN(tc.value); got != tc.want {
				t.Errorf("validITIN(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestCountryMod11ResidueBelowTwoIsZero(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		value string
		want  int
	}{
		// The weighted sums are 0, 11 and 2 times the digit: 0, 0, 2 and 4.
		{"residue 0 checks as zero", "0", 0},
		{"residue 2 checks as nine", "1", 9},
		{"residue 4 checks as seven", "2", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := countryMod11(tc.value, 6); got != tc.want {
				t.Errorf("countryMod11(%q, 6) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}
