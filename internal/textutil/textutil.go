// Package textutil holds the text coercion shared by the adapters. Every helper
// returns a value for any input, including nil — adapters must not fail on a
// field a provider omitted.
package textutil

import (
	"html"
	"regexp"
	"strconv"
	"strings"
)

var (
	tagRe      = regexp.MustCompile(`<[^>]+>`)
	blockEndRe = regexp.MustCompile(`(?i)</(p|div|li|tr|td|th|h[1-6]|section|ul|ol|table)\s*>`)
	brRe       = regexp.MustCompile(`(?i)<br\s*/?>`)
	spaceRe    = regexp.MustCompile(`[ \t]+`)
	nlPadRe    = regexp.MustCompile(` *\n *`)
	nlRunRe    = regexp.MustCompile(`\n{3,}`)
)

// StripHTML turns markup — possibly entity-escaped, as Greenhouse sends it —
// into readable text.
//
// Unescape first: Greenhouse double-encodes, so the body arrives as "&lt;p&gt;…"
// and a naive tag strip would leave the markup intact.
func StripHTML(value string) string {
	if value == "" {
		return ""
	}
	text := value
	for range 2 {
		unescaped := html.UnescapeString(text)
		if unescaped == text {
			break
		}
		text = unescaped
	}
	text = brRe.ReplaceAllString(text, "\n")
	text = blockEndRe.ReplaceAllString(text, "\n")
	// Dropped, not spaced: inline markup lands mid-word ("V<b>isa</b>"), and a
	// space there turns "Visa sponsorship" into "V isa sponsorship", which no
	// amount of pattern-writing downstream can recover.
	text = tagRe.ReplaceAllString(text, "")
	text = html.UnescapeString(text)
	return Clean(text)
}

func Clean(text string) string {
	text = strings.ReplaceAll(text, " ", " ")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = spaceRe.ReplaceAllString(text, " ")
	text = nlPadRe.ReplaceAllString(text, "\n")
	return strings.TrimSpace(nlRunRe.ReplaceAllString(text, "\n\n"))
}

// AsString coerces a decoded-JSON value to a non-empty string, or "" when the
// value is absent, empty, or a container. The bool reports whether a value was
// present, so callers can tell "" apart from SQL NULL.
func AsString(value any) (string, bool) {
	switch v := value.(type) {
	case nil:
		return "", false
	case map[string]any, []any:
		return "", false
	case string:
		s := strings.TrimSpace(v)
		return s, s != ""
	case bool:
		if v {
			return "True", true
		}
		return "False", true
	case float64:
		// JSON numbers decode as float64; render integers without a ".0" tail
		// so an external_id of 42 does not become "42.0".
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10), true
		}
		return strconv.FormatFloat(v, 'g', -1, 64), true
	default:
		return "", false
	}
}

// AsFloat mirrors Python's float() over the shapes JSON produces, returning
// ok=false where float() would have raised.
func AsFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case nil:
		return 0, false
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// Dig walks payload["a"]["b"] without caring whether either level exists.
func Dig(payload any, keys ...string) any {
	node := payload
	for _, key := range keys {
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = m[key]
	}
	return node
}
