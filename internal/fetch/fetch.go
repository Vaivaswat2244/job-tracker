// Package fetch does best-effort JD retrieval. A failure here must never cost
// us the row.
package fetch

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

const (
	UA      = "Mozilla/5.0 (X11; Linux x86_64) job-tracker/1.0"
	Timeout = 10 * time.Second
)

// atsPathCompany maps host -> path segment index holding the real company name,
// for ATS domains where the path carries it.
var atsPathCompany = map[string]int{
	"boards.greenhouse.io":       1,
	"job-boards.greenhouse.io":   1,
	"jobs.lever.co":              1,
	"jobs.ashbyhq.com":           1,
	"apply.workable.com":         1,
	"careers.smartrecruiters.com": 1,
}

var titleCaser = cases.Title(language.English)

func hostOf(rawURL string) (string, *url.URL) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", nil
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www."), u
}

// CompanyFromURL derives a display name from a posting URL.
func CompanyFromURL(rawURL string) string {
	host, u := hostOf(rawURL)
	if u == nil {
		return "Unknown"
	}

	var parts []string
	for _, p := range strings.Split(u.Path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}

	if idx, ok := atsPathCompany[host]; ok && len(parts) >= idx {
		seg := strings.NewReplacer("-", " ", "_", " ").Replace(parts[idx-1])
		return titleCaser.String(seg)
	}
	if host == "" {
		return "Unknown"
	}

	labels := strings.Split(host, ".")
	label := labels[0]
	switch label {
	case "jobs", "careers", "boards", "apply", "job-boards", "hire":
		if len(labels) > 1 {
			label = labels[1]
		}
	}
	return titleCaser.String(strings.ReplaceAll(label, "-", " "))
}

var atsHosts = func() map[string]struct{} {
	hosts := map[string]struct{}{
		"boards-api.greenhouse.io": {}, "api.lever.co": {}, "api.ashbyhq.com": {},
		"jobs.workable.com": {}, "www.linkedin.com": {}, "linkedin.com": {},
		"wellfound.com": {}, "remoteok.com": {}, "himalayas.app": {},
		"weworkremotely.com": {}, "jobicy.com": {}, "arbeitnow.com": {},
		"remotive.com": {}, "instahyre.com": {},
	}
	for h := range atsPathCompany {
		hosts[h] = struct{}{}
	}
	return hosts
}()

// DomainFromURL returns the company domain, or "" when the URL belongs to an
// ATS or aggregator.
//
// Guessing "jobs.lever.co" as the company domain would make the contact
// domain-collision guard reject every genuine address at that company.
func DomainFromURL(rawURL string) string {
	host, _ := hostOf(rawURL)
	if host == "" {
		return ""
	}
	if _, isATS := atsHosts[host]; isATS {
		return ""
	}
	return host
}

var (
	spaceRe = regexp.MustCompile(`[ \t]+`)
	nlRunRe = regexp.MustCompile(`\n{3,}`)
)

func clean(text string) string {
	return strings.TrimSpace(nlRunRe.ReplaceAllString(spaceRe.ReplaceAllString(text, " "), "\n\n"))
}

// JD returns (title, jdText, errMessage). It never fails hard: an empty JD
// beats a lost row, so every failure comes back as a message the caller logs.
func JD(rawURL string) (string, string, string) {
	client := &http.Client{Timeout: Timeout}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", fmt.Sprintf("BadRequest: %v", err)
	}
	req.Header.Set("User-Agent", UA)

	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Sprintf("RequestError: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", "", fmt.Sprintf("HTTPError: %d", resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return "", "", fmt.Sprintf("ParseError: %v", err)
	}

	title := extractTitle(doc)
	doc.Find("script, style, nav, footer, header, noscript").Remove()
	return title, clean(doc.Text()), ""
}

// extractTitle prefers og:title, then <title>, then the first <h1> — the same
// precedence the Python build used.
func extractTitle(doc *goquery.Document) string {
	if content, ok := doc.Find(`meta[property="og:title"]`).First().Attr("content"); ok {
		if t := strings.TrimSpace(content); t != "" {
			return t
		}
	}
	if t := strings.TrimSpace(doc.Find("title").First().Text()); t != "" {
		return t
	}
	return strings.TrimSpace(doc.Find("h1").First().Text())
}
