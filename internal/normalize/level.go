package normalize

import (
	"regexp"
	"strconv"
	"strings"
)

// Seniority levels, coarse on purpose. The question this answers is "could a
// final-year student plausibly get this?", not "what band is it internally".
const (
	LevelIntern  = "intern"  // internship, trainee, apprentice, co-op
	LevelJunior  = "junior"  // new grad, associate, I/II, <=2 years asked
	LevelMid     = "mid"     // 3-5 years, or III
	LevelSenior  = "senior"  // senior/staff/principal/lead/manager and up
	LevelUnknown = "unknown" // nothing said either way
)

// internRe is the strongest and least ambiguous signal. "Internal" and
// "International" are excluded by the word boundary plus the negative cases
// below — `intern` alone would match both, which is exactly the mistake the
// naive title filter made.
var internRe = regexp.MustCompile(`(?i)\b(intern|internship|interns)\b|\b(trainee|apprentice|co-?op\s+(?:program|student))\b`)

// notInternRe catches the words that merely start the same way.
var notInternRe = regexp.MustCompile(`(?i)\b(internal|international|internally|internationally)\b`)

// juniorRe is the explicitly early-career vocabulary.
var juniorRe = regexp.MustCompile(`(?i)\b(new\s?grad(?:uate)?|graduate\s+(?:program|engineer|scheme)|campus|fresher|entry[\s-]level|junior|jr\.?)\b`)

// associateRe is separated from juniorRe because "Associate" is genuinely
// ambiguous: "Associate Engineer" is early career, "Associate Director" is not.
// It only counts as junior when no senior word appears alongside it.
var associateRe = regexp.MustCompile(`(?i)\bassociates?\b`)

// seniorRe vetoes everything else. A senior title beats a low
// years-of-experience number: "Senior Software Engineer ... 2+ years" is a
// senior role with a modest floor, not a junior one.
var seniorRe = regexp.MustCompile(`(?i)\b(senior|sr\.?|staff|principal|lead|leader|head\s+of|director|vp|vice\s+president|manager|chief|architect|distinguished|fellow|expert|experienced)\b`)

// levelSuffixRe reads the numeric level companies append to engineering titles:
// "Software Engineer II", "SDE-3", "Engineer L4". Roman and arabic both appear.
var levelSuffixRe = regexp.MustCompile(`(?i)(?:\b|[-\s])(?:L|level\s*)?([IVX]{1,4}|[1-6])\s*$`)

// yoeRe pulls the minimum years of experience a posting asks for. The first
// number in "3-5 years of experience" is the floor, which is the only part that
// decides whether it is worth applying to.
var yoeRe = regexp.MustCompile(`(?i)(\d{1,2})\s*(?:\+|\s*(?:-|–|to)\s*\d{1,2})?\s*\+?\s*years?[^.\n]{0,40}?(?:experience|exp\b)`)

// romanValues maps the level suffixes worth reading.
var romanValues = map[string]int{"I": 1, "II": 2, "III": 3, "IV": 4, "V": 5, "VI": 6}

// MinYears returns the lowest years-of-experience the text asks for, and
// whether it found one at all.
func MinYears(text string) (int, bool) {
	m := yoeRe.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n > 30 {
		return 0, false
	}
	return n, true
}

// TitleLevel reads the numeric suffix on a title, if any. Returns 0 when the
// title carries no level.
func TitleLevel(title string) int {
	m := levelSuffixRe.FindStringSubmatch(strings.TrimSpace(title))
	if m == nil {
		return 0
	}
	token := strings.ToUpper(m[1])
	if v, ok := romanValues[token]; ok {
		return v
	}
	if v, err := strconv.Atoi(token); err == nil && v >= 1 && v <= 6 {
		return v
	}
	return 0
}

// Level classifies how senior a posting is, from three independent signals.
//
// None of them works alone. employment_type is exact but almost never set;
// titles miss roles that are open to juniors without saying so; and years of
// experience alone reads "Senior Engineer, 2+ years" as junior. So: the ATS
// type wins when present, an explicit senior title vetoes, and experience is
// the fallback that supplies recall.
func Level(title, employmentType, jdText string) string {
	// 1. The ATS said so outright.
	if t := strings.ToLower(strings.TrimSpace(employmentType)); t != "" {
		if strings.HasPrefix(t, "intern") && !strings.HasPrefix(t, "internal") {
			return LevelIntern
		}
	}

	// 2. Internship in the title, unless it is "internal" or "international".
	cleaned := notInternRe.ReplaceAllString(title, " ")
	if internRe.MatchString(cleaned) {
		return LevelIntern
	}

	senior := seniorRe.MatchString(title)

	// 3. Explicitly early-career vocabulary, as long as nothing senior is also
	// present — "Senior Associate" is not a junior role.
	if !senior {
		if juniorRe.MatchString(title) {
			return LevelJunior
		}
		if associateRe.MatchString(title) {
			return LevelJunior
		}
		if n := TitleLevel(title); n > 0 {
			level := LevelSenior
			switch {
			case n <= 2:
				level = LevelJunior
			case n == 3:
				level = LevelMid
			}
			// Reconcile against what the posting actually asks for. Numeric
			// levels are company-specific: "Engineer II" asking for 4 years is
			// not an early-career role whatever the numeral suggests, and
			// trusting the numeral alone puts roles you cannot get on the list.
			if years, ok := MinYears(jdText); ok {
				switch {
				case years > 5:
					level = LevelSenior
				case years > 2 && level == LevelJunior:
					level = LevelMid
				}
			}
			return level
		}
	}

	if senior {
		return LevelSenior
	}

	// 4. Nothing in the title. Fall back to what the description asks for.
	if years, ok := MinYears(jdText); ok {
		switch {
		case years <= 2:
			return LevelJunior
		case years <= 5:
			return LevelMid
		default:
			return LevelSenior
		}
	}
	return LevelUnknown
}

// EarlyCareer reports whether a level is one a final-year student can
// realistically apply to. Unknown counts: an unclassified role is one to look
// at, not one to hide (INV-1 — uncertainty must not silently exclude).
func EarlyCareer(level string) bool {
	switch level {
	case LevelIntern, LevelJunior, LevelUnknown, "":
		return true
	default:
		return false
	}
}
