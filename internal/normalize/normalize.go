// Package normalize turns a NormalizedJob into the column values `jobs` expects.
//
// Every heuristic here defaults to unknown rather than guessing. A wrong
// comp_model is worse than a blank one: blank prompts the user to look, wrong
// does not. AuthRequired is a flag that sorts a job lower, never a filter that
// removes it (INV-1).
//
// The patterns below were written with Python's (?x) verbose mode. Go's RE2 has
// no equivalent, so they are expanded here — every meaningful space in the
// originals was already an explicit \s, which makes the expansion mechanical.
package normalize

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// --------------------------------------------------------------------- comp model

var agnosticRe = regexp.MustCompile(`(?i)` +
	`location[\s-]*agnostic` +
	`|location[\s-]*independent` +
	`|same\s+(?:pay|salary|compensation)\s+(?:regardless|anywhere|no\s+matter)` +
	`|(?:pay|salary|compensation)\s+(?:is\s+)?(?:the\s+)?same\s+(?:regardless|anywhere|everywhere)` +
	`|(?:we\s+)?(?:do\s+not|don't)\s+adjust\s+(?:pay|salary|compensation)\s+(?:for|by)\s+location` +
	`|(?:not|non)[\s-]*adjusted\s+(?:for|by)\s+(?:location|geography)` +
	`|global\s+(?:salary|pay|compensation)\s+band`)

var geoRe = regexp.MustCompile(`(?i)` +
	`location[\s-]*adjusted` +
	`|geo[\s-]*(?:tiered|tier|adjusted|based)` +
	`|(?:pay|salary|compensation)\s+(?:tier|zone|band)s?\s+(?:by|based\s+on|depend)` +
	`|adjusted\s+(?:for|to)\s+(?:your\s+)?(?:location|geography|market|cost\s+of\s+living)` +
	`|(?:based|depends?|varies)\s+on\s+(?:your\s+)?(?:location|geography|geographic\s+tier)` +
	`|cost[\s-]*of[\s-]*living\s+adjust`)

var inrRe = regexp.MustCompile(`(?i)(?:₹|\bINR\b|\bRs\.?\s*\d|\d+\s*(?:LPA|lakhs?|lacs?)\b|\bcrores?\b)`)

// CompModel classifies how a posting sets pay against geography.
func CompModel(jdText, payCurrency string) string {
	if agnosticRe.MatchString(jdText) {
		return "location_agnostic"
	}
	if geoRe.MatchString(jdText) {
		return "geo_adjusted"
	}
	if strings.ToUpper(payCurrency) == "INR" || inrRe.MatchString(jdText) {
		return "local_market"
	}
	return "unknown"
}

// ------------------------------------------------------------------ auth required

// noSponsorRe is sponsorship explicitly refused. Checked first: "we do not
// sponsor visas" also matches the offering patterns below, and the refusal is
// the stronger signal.
//
// The final alternative requires "without sponsorship" to be qualified: bare
// use is nearly always the US export-license boilerplate, not an immigration
// statement.
var noSponsorRe = regexp.MustCompile(`(?i)` +
	`(?:do\s+not|don't|cannot|can't|unable\s+to|not\s+able\s+to|will\s+not|won't)` +
	`\s+(?:currently\s+|presently\s+)?(?:offer|provide|sponsor)\w*` +
	`(?:\s+\w+){0,3}?\s*(?:visa|sponsorship|immigration)` +
	`|no\s+(?:visa\s+|work\s+|employment\s+|immigration\s+)?sponsorship` +
	`|(?:visa\s+|work\s+|employment\s+)?sponsorship\s+is\s+` +
	`(?:not\s+(?:provided|offered|available|possible)|unavailable)` +
	`|without\s+(?:the\s+need\s+for\s+)?(?:visa|work|employment|immigration)\s+sponsorship`)

// exportBoilerplateRe matches the US export-control paragraphs that talk about
// "sponsorship for an export license" and "authorized to receive technology".
// Both trip the patterns above and have nothing to do with whether the company
// will hire someone who needs a visa.
var exportBoilerplateRe = regexp.MustCompile(`(?i)export\s+(?:license|control|law|administration|regulation)`)

var sponsorOKRe = regexp.MustCompile(`(?i)` +
	`(?:visa\s+)?sponsorship\s+(?:is\s+)?(?:available|offered|provided|possible)` +
	`|(?:we|company)\s+(?:can|will|do|are\s+happy\s+to|are\s+able\s+to)\s+sponsor` +
	`|(?:offer|provide)s?\s+(?:visa\s+)?sponsorship` +
	`|relocation\s+and\s+visa\s+support`)

var authRequiredRe = regexp.MustCompile(`(?i)` +
	`(?:must\s+be|are|is|be)\s+(?:legally\s+)?(?:authori[sz]ed|eligible|entitled)\s+to\s+work` +
	`\s+in\s+(?:the\s+)?(?:US|U\.S\.|USA|United\s+States|Canada|EU|European\s+Union|UK|United\s+Kingdom)` +
	`|(?:US|U\.S\.|USA|United\s+States|Canada|Canadian|EU|European|UK)\s+work\s+authori[sz]ation` +
	`\s*(?:is\s+)?(?:required|necessary|a\s+must)` +
	`|(?:requires?|require)\s+(?:US|U\.S\.|USA|United\s+States|Canada|EU|UK)\s+work\s+authori[sz]ation` +
	`|(?:US|U\.S\.|United\s+States|Canadian|EU|UK)\s+citizens?\s+or\s+permanent\s+residents?` +
	`|authori[sz]ed\s+to\s+work\s+in\s+(?:the\s+)?(?:US|U\.S\.|USA|United\s+States|Canada|EU|UK)`)

const boilerplateWindow = 200

// immigrationMatch reports whether the pattern matches anywhere outside an
// export-control paragraph. The window is measured in runes, matching Python's
// character-based slicing.
func immigrationMatch(pattern *regexp.Regexp, text string) bool {
	spans := pattern.FindAllStringIndex(text, -1)
	if spans == nil {
		return false
	}
	runes := []rune(text)
	// byteToRune lets the byte offsets RE2 reports address the rune slice.
	byteToRune := make(map[int]int, len(runes)+1)
	pos := 0
	for i, r := range runes {
		byteToRune[pos] = i
		pos += len(string(r))
	}
	byteToRune[pos] = len(runes)

	for _, span := range spans {
		start, ok1 := byteToRune[span[0]]
		end, ok2 := byteToRune[span[1]]
		if !ok1 || !ok2 {
			continue
		}
		lo := max(0, start-boilerplateWindow)
		hi := min(len(runes), end+boilerplateWindow)
		if exportBoilerplateRe.MatchString(string(runes[lo:hi])) {
			continue
		}
		return true
	}
	return false
}

// AuthRequired returns 1 when the posting demands US/CA/EU authorization the
// user does not have.
//
// A flag, not a filter — the caller sorts these lower and still shows them.
func AuthRequired(jdText string) int {
	if immigrationMatch(noSponsorRe, jdText) {
		return 1
	}
	if sponsorOKRe.MatchString(jdText) {
		return 0
	}
	if immigrationMatch(authRequiredRe, jdText) {
		return 1
	}
	return 0
}

// ----------------------------------------------------------------- hires in India

var indiaRe = regexp.MustCompile(`(?i)` +
	`\bindia\b|\bindian\b` +
	`|\bbangalore\b|\bbengaluru\b|\bmumbai\b|\bdelhi\b|\bncr\b|\bgurgaon\b` +
	`|\bgurugram\b|\bhyderabad\b|\bpune\b|\bchennai\b|\bnoida\b|\bkolkata\b` +
	`|\bahmedabad\b|\bjaipur\b|\bkochi\b|\bcoimbatore\b|\bindore\b`)

var globalRemoteRe = regexp.MustCompile(`(?i)` +
	`remote\s*[-–—,(]?\s*(?:global|worldwide|anywhere|international)` +
	`|(?:work\s+from|hire)\s+anywhere` +
	`|anywhere\s+in\s+the\s+world` +
	`|globally\s+remote|fully\s+distributed`)

// vagueLocationRe matches location strings that name no actual place, so the JD
// text gets to decide.
var vagueLocationRe = regexp.MustCompile(`(?i)^\s*(?:` +
	`remote|hybrid|in[\s-]*office|on[\s-]*site|flexible|global|worldwide` +
	`|anywhere|various|multiple(?:\s+locations)?|tbd|n/?a|-|\.` +
	`)\s*$`)

// HiresInIndia returns 1 for India or worldwide-remote, 0 for a named non-India
// location, and ok=false when it is unclear.
//
// The posting's own location field wins outright when it names a real place.
// Boilerplate in the body ("we are fully distributed", an India office in the
// footer) otherwise flags a Foster City role as India-friendly, and a field that
// says 1 for everything is worth less than one that says nothing.
//
// Unknown is a real answer here. Recording 0 for a JD that never mentioned
// geography at all would quietly bury roles that do in fact hire in India.
func HiresInIndia(jdText, location string) (int, bool) {
	loc := strings.TrimSpace(location)
	if loc != "" && !vagueLocationRe.MatchString(loc) {
		if indiaRe.MatchString(loc) {
			return 1, true
		}
		if globalRemoteRe.MatchString(loc) {
			return 1, true
		}
		return 0, true
	}

	if indiaRe.MatchString(jdText) || globalRemoteRe.MatchString(jdText) {
		return 1, true
	}
	return 0, false
}

// ------------------------------------------------------------------ dedupe keys

var (
	punctRe   = regexp.MustCompile(`[^a-z0-9 ]+`)
	suffixRe  = regexp.MustCompile(`(?i)\b(inc|llc|ltd|limited|pvt|private|corp|corporation|co|gmbh|plc|technologies|technology|labs|software|systems|solutions|india)\b`)
	noiseRe   = regexp.MustCompile(`(?i)\((?:remote|hybrid|onsite|on-site|contract|full[\s-]?time|part[\s-]?time)[^)]*\)`)
	trailerRe = regexp.MustCompile(`(?i)\s*[-–—|,]\s*(remote|hybrid|onsite|on-site|india|bangalore|bengaluru|mumbai|us|usa|emea|global)\b.*$`)
)

// collapse mirrors " ".join(text.split()): squeeze all whitespace runs to one
// space and trim.
func collapse(text string) string { return strings.Join(strings.Fields(text), " ") }

func NormCompany(name string) string {
	text := suffixRe.ReplaceAllString(strings.ToLower(name), " ")
	return collapse(punctRe.ReplaceAllString(text, " "))
}

func NormTitle(title string) string {
	text := noiseRe.ReplaceAllString(strings.ToLower(title), " ")
	text = trailerRe.ReplaceAllString(text, " ")
	return collapse(punctRe.ReplaceAllString(text, " "))
}

// PostedWeek returns an ISO year-week. Two boards listing the same role rarely
// agree on the exact timestamp but almost always land in the same week.
func PostedWeek(postedAt string) string {
	parsed, ok := ParseDT(postedAt)
	if !ok {
		return ""
	}
	year, week := parsed.ISOWeek()
	return fmt.Sprintf("%d-W%02d", year, week)
}

// dtLayouts covers the shapes fromisoformat accepted for these feeds, tried
// against the full string, its first 19 chars, then its first 10.
var dtLayouts = []string{
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// ParseDT parses a timestamp, defaulting a naive value to UTC.
func ParseDT(value string) (time.Time, bool) {
	text := strings.ReplaceAll(strings.TrimSpace(value), "Z", "+00:00")
	if text == "" {
		return time.Time{}, false
	}

	candidates := []string{text}
	if r := []rune(text); len(r) > 19 {
		candidates = append(candidates, string(r[:19]))
	}
	if r := []rune(text); len(r) > 10 {
		candidates = append(candidates, string(r[:10]))
	}

	for _, candidate := range candidates {
		for _, layout := range dtLayouts {
			if t, err := time.Parse(layout, candidate); err == nil {
				if t.Location() == time.UTC || hasZone(layout) {
					return t, true
				}
				return t.UTC(), true
			}
		}
	}
	return time.Time{}, false
}

func hasZone(layout string) bool { return strings.Contains(layout, "Z07:00") }
