// Heuristics. Each defaults to unknown; a wrong answer is worse than none.
package normalize_test

import (
	"testing"

	"github.com/Vaivaswat2244/job-tracker/internal/normalize"
)

// ----------------------------------------------------------------- comp model

func TestCompModel(t *testing.T) {
	tests := []struct {
		name, text, currency, want string
	}{
		{"agnostic/plain", "Our compensation is location agnostic.", "", "location_agnostic"},
		{"agnostic/same regardless", "We pay the same regardless of where you live.", "", "location_agnostic"},
		{"agnostic/hyphenated", "This is a location-independent salary band.", "", "location_agnostic"},
		{"agnostic/no adjustment", "We do not adjust salary for location.", "", "location_agnostic"},

		{"geo/adjusted", "Salary is location-adjusted.", "", "geo_adjusted"},
		{"geo/tiered", "We use geo-tiered compensation bands.", "", "geo_adjusted"},
		{"geo/adjusted for location", "Your offer is adjusted for your location.", "", "geo_adjusted"},
		{"geo/geographic tier", "Pay depends on your geographic tier.", "", "geo_adjusted"},

		{"inr/symbol", "Compensation: ₹40,00,000", "", "local_market"},
		{"inr/lpa", "Salary 25 LPA", "", "local_market"},
		{"inr/code", "INR 1800000", "", "local_market"},
		{"inr/currency field", "", "INR", "local_market"},

		{"unknown/empty", "", "", "unknown"},
		{"unknown/generic", "We offer competitive compensation.", "", "unknown"},
		{"unknown/benefits", "Great benefits.", "", "unknown"},

		// An explicit statement beats an INR figure: agnostic is checked first.
		{"explicit beats figure", "Location agnostic pay. Bengaluru band: ₹40L", "", "location_agnostic"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalize.CompModel(tc.text, tc.currency); got != tc.want {
				t.Errorf("CompModel(%q, %q) = %q, want %q", tc.text, tc.currency, got, tc.want)
			}
		})
	}
}

// --------------------------------------------------------------- auth required

func TestAuthRequired(t *testing.T) {
	tests := []struct {
		name, text string
		want       int
	}{
		{"authorized to work in US", "You must be legally authorized to work in the United States.", 1},
		{"work authorization required", "US work authorization is required.", 1},
		{"does not sponsor", "We do not sponsor visas.", 1},
		{"no visa sponsorship", "No visa sponsorship for this role.", 1},
		{"sponsorship not provided", "Visa sponsorship is NOT provided for this position.", 1},
		{"citizens or permanent residents", "Candidates must be US citizens or permanent residents.", 1},

		{"empty", "", 0},
		{"hires globally", "We hire globally.", 0},
		{"sponsorship available", "Visa sponsorship is available for this role.", 0},
		{"happy to sponsor", "We are happy to sponsor the right candidate.", 0},

		// The single biggest false positive in real Greenhouse JDs: US export-law
		// paragraphs mention "sponsorship for an export license", which has
		// nothing to do with whether the company will hire someone needing a visa.
		{"export control boilerplate",
			"Employee must be able to receive software or technology controlled under " +
				"U.S. export laws without sponsorship for an export license.", 0},

		{"offered sponsorship beats generic line",
			"You must be authorized to work in the US. Visa sponsorship is available.", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalize.AuthRequired(tc.text); got != tc.want {
				t.Errorf("AuthRequired(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}

// -------------------------------------------------------------- hires in India

func TestHiresInIndiaNamedLocations(t *testing.T) {
	for _, loc := range []string{"Bengaluru, India", "Remote India", "Mumbai", "Pune"} {
		t.Run(loc, func(t *testing.T) {
			got, ok := normalize.HiresInIndia("", loc)
			if !ok || got != 1 {
				t.Errorf("HiresInIndia(%q) = (%d, %v), want (1, true)", loc, got, ok)
			}
		})
	}
}

// A company with an India office mentions India in every JD footer. The
// posting's own location field is the authority.
func TestNamedNonIndiaLocationWinsOverBodyBoilerplate(t *testing.T) {
	body := "We are fully distributed with an office in Bangalore."
	for _, loc := range []string{"Foster City, CA", "United States", "Berlin, Germany"} {
		t.Run(loc, func(t *testing.T) {
			got, ok := normalize.HiresInIndia(body, loc)
			if !ok || got != 0 {
				t.Errorf("HiresInIndia(body, %q) = (%d, %v), want (0, true)", loc, got, ok)
			}
		})
	}
}

func TestVagueLocationDefersToTheDescription(t *testing.T) {
	for _, loc := range []string{"Remote", "", "Global", "In-Office"} {
		t.Run("loc="+loc, func(t *testing.T) {
			for _, body := range []string{
				"This role is open to candidates in India.",
				"Hire from anywhere in the world.",
			} {
				got, ok := normalize.HiresInIndia(body, loc)
				if !ok || got != 1 {
					t.Errorf("HiresInIndia(%q, %q) = (%d, %v), want (1, true)", body, loc, got, ok)
				}
			}
		})
	}
}

func TestUnknownWhenNothingIndicatesGeography(t *testing.T) {
	if _, ok := normalize.HiresInIndia("Great team, great mission.", "Remote"); ok {
		t.Error("expected unknown for a body with no geography")
	}
	if _, ok := normalize.HiresInIndia("", ""); ok {
		t.Error("expected unknown for empty body and location")
	}
}

// ---------------------------------------------------------------- dedupe keys

func TestNormCompanyStripsLegalSuffixes(t *testing.T) {
	if got, want := normalize.NormCompany("Acme Technologies Pvt. Ltd."), normalize.NormCompany("ACME"); got != want {
		t.Errorf("NormCompany mismatch: %q != %q", got, want)
	}
}

func TestNormTitleStripsLocationAndModeNoise(t *testing.T) {
	base := normalize.NormTitle("Backend Engineer")
	for _, variant := range []string{"Backend Engineer (Remote)", "Backend Engineer - India"} {
		if got := normalize.NormTitle(variant); got != base {
			t.Errorf("NormTitle(%q) = %q, want %q", variant, got, base)
		}
	}
}

func TestPostedWeekGroupsNearbyDays(t *testing.T) {
	if a, b := normalize.PostedWeek("2026-08-03T00:00:00Z"), normalize.PostedWeek("2026-08-06T23:00:00Z"); a != b {
		t.Errorf("same-week timestamps differ: %q != %q", a, b)
	}
	if a, b := normalize.PostedWeek("2026-08-03"), normalize.PostedWeek("2026-09-03"); a == b {
		t.Errorf("distant dates collided on %q", a)
	}
	if got := normalize.PostedWeek(""); got != "" {
		t.Errorf("PostedWeek(\"\") = %q, want \"\"", got)
	}
	if got := normalize.PostedWeek("garbage"); got != "" {
		t.Errorf("PostedWeek(\"garbage\") = %q, want \"\"", got)
	}
}
