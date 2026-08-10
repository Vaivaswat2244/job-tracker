package ats

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/Vaivaswat2244/job-tracker/internal/httpx"
)

// Detection is best-effort by design. `watchlist add` must never refuse to add
// a company because detection failed (INV-1): an entry sitting at ats=unknown
// is visible and fixable, a company the user believed they added is not.

// reserved holds slugs that are actually route segments, not boards.
var reserved = map[string]struct{}{
	"embed": {}, "job_board": {}, "api": {}, "v0": {}, "v1": {},
	"jobs": {}, "job": {}, "boards": {}, "static": {},
}

// patterns is ordered: the Greenhouse embed form must win over the bare path
// form, since "boards.greenhouse.io/embed/job_board?for=acme" matches both and
// only the first one yields the real slug.
var patterns = []struct {
	provider string
	re       *regexp.Regexp
}{
	{"greenhouse", regexp.MustCompile(
		`(?:boards|job-boards)\.greenhouse\.io/embed/job_board[^"'\s<>]*?[?&]for=([A-Za-z0-9_.-]+)`)},
	{"greenhouse", regexp.MustCompile(
		`(?:boards|job-boards)\.greenhouse\.io/([A-Za-z0-9_.-]+)`)},
	{"lever", regexp.MustCompile(`jobs\.lever\.co/([A-Za-z0-9_.-]+)`)},
	{"ashby", regexp.MustCompile(`jobs\.ashbyhq\.com/([A-Za-z0-9_.-]+)`)},
}

var iframeRe = regexp.MustCompile(`(?i)<iframe[^>]+src=["']([^"']+)["']`)

type Detection struct {
	ATS      string // "unknown" when nothing matched
	Slug     string
	Evidence string // where the match was found
	Error    string
}

func (d Detection) Found() bool { return d.ATS != "unknown" && d.ATS != "" && d.Slug != "" }

func unknown() Detection { return Detection{ATS: "unknown"} }

// MatchText returns the first (provider, slug) found in any URL-shaped string.
func MatchText(text string) (string, string, bool) {
	if text == "" {
		return "", "", false
	}
	for _, p := range patterns {
		for _, m := range p.re.FindAllStringSubmatch(text, -1) {
			slug := strings.Trim(m[1], "/.")
			if slug == "" {
				continue
			}
			if _, isReserved := reserved[strings.ToLower(slug)]; isReserved {
				continue
			}
			return p.provider, slug, true
		}
	}
	return "", "", false
}

// Detect checks the URL itself, then the page it redirects to, then its body,
// then one level of iframe. Anything deeper is a scraper, which is a non-goal.
func Detect(rawURL string) Detection { return detect(rawURL, 0) }

func detect(rawURL string, depth int) Detection {
	if provider, slug, ok := MatchText(rawURL); ok {
		return Detection{ATS: provider, Slug: slug, Evidence: "url"}
	}

	resp := httpx.Get(rawURL, httpx.Options{})
	if !resp.OK() {
		msg := resp.Err
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.Status)
		}
		d := unknown()
		d.Error = msg
		return d
	}

	if resp.FinalURL != "" && resp.FinalURL != rawURL {
		if provider, slug, ok := MatchText(resp.FinalURL); ok {
			return Detection{ATS: provider, Slug: slug, Evidence: "redirect"}
		}
	}

	if provider, slug, ok := MatchText(resp.Body); ok {
		return Detection{ATS: provider, Slug: slug, Evidence: "page body"}
	}

	if depth == 0 {
		base := resp.FinalURL
		if base == "" {
			base = rawURL
		}
		for _, m := range iframeRe.FindAllStringSubmatch(resp.Body, -1) {
			nested := detect(resolveRef(base, m[1]), 1)
			if nested.Found() {
				nested.Evidence = "iframe -> " + nested.Evidence
				return nested
			}
		}
	}

	d := unknown()
	d.Error = "no greenhouse/lever/ashby board found on the page"
	return d
}

// resolveRef is urljoin: an absolute ref replaces the base, a relative one
// resolves against it.
func resolveRef(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}
