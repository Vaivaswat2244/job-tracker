package funding

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/Vaivaswat2244/job-tracker/internal/db"
	"github.com/Vaivaswat2244/job-tracker/internal/httpx"
)

// Resolution attaches a funded company to a domain, and matches it on that
// domain alone.
//
// Company names collide constantly. Matching "Atlas" the Bangalore fintech to
// "Atlas" the US logistics company corrupts the watchlist, and every downstream
// consequence — priority, ATS slug, eventually a draft addressed to a stranger —
// inherits the error. So:
//
//  1. resolve to a domain, or do not match at all;
//  2. match on domain only, never on name;
//  3. a name that looks right without domain confirmation is a candidate for
//     human review, never an automatic write to `companies`.

// neverACompany holds publishers, socials, CDNs and shorteners. A link to any
// of these tells us nothing about who raised the money.
var neverACompany = map[string]struct{}{
	"entrackr.com": {}, "inc42.com": {}, "vccircle.com": {}, "yourstory.com": {},
	"moneycontrol.com": {}, "economictimes.indiatimes.com": {}, "indiatimes.com": {},
	"livemint.com": {}, "business-standard.com": {}, "techcrunch.com": {},
	"reuters.com": {}, "bloomberg.com": {}, "forbes.com": {}, "medium.com": {},
	"twitter.com": {}, "x.com": {}, "facebook.com": {}, "linkedin.com": {},
	"instagram.com": {}, "youtube.com": {}, "whatsapp.com": {}, "t.me": {},
	"telegram.me": {}, "threads.net": {}, "reddit.com": {}, "pinterest.com": {},
	"google.com": {}, "gstatic.com": {}, "googleapis.com": {}, "doubleclick.net": {},
	"gravatar.com": {}, "wordpress.com": {}, "wp.com": {}, "cloudflare.com": {},
	"amazonaws.com": {}, "publive.online": {}, "bit.ly": {}, "tinyurl.com": {},
	"crunchbase.com": {}, "tracxn.com": {}, "wikipedia.org": {}, "apple.com": {},
	"play.google.com": {}, "sharechat.com": {}, "koo.app": {},
}

var stopwords = map[string]struct{}{
	"the": {}, "and": {}, "labs": {}, "technologies": {}, "technology": {},
	"systems": {}, "solutions": {}, "ventures": {}, "capital": {}, "group": {},
	"india": {}, "global": {}, "inc": {}, "ltd": {}, "pvt": {}, "limited": {},
	"private": {}, "company": {}, "digital": {}, "online": {}, "app": {}, "ai": {},
}

var (
	linkRe     = regexp.MustCompile(`(?i)<a[^>]+href=["']([^"']+)["']`)
	nonAlnumRe = regexp.MustCompile(`[^a-z0-9]+`)
)

// Registrable is a close-enough registrable domain. Good enough to compare two
// hostnames.
func Registrable(host string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(host)), "www.")
}

func isPublisher(host string) bool {
	for bad := range neverACompany {
		if host == bad || strings.HasSuffix(host, "."+bad) {
			return true
		}
	}
	return false
}

// NameTokens are the meaningful words of a company name, for *comparing* names.
func NameTokens(name string) []string {
	var out []string
	for _, w := range nonAlnumRe.Split(strings.ToLower(name), -1) {
		if len(w) >= 3 {
			if _, stop := stopwords[w]; !stop {
				out = append(out, w)
			}
		}
	}
	return out
}

// DomainMatchesName asks whether a hostname plausibly belongs to a company.
//
// Deliberately strict. An outbound link on a funding article is as likely to be
// an investor's site as the company's, and a wrong domain here is worse than no
// domain: no domain sends the item to review, a wrong one confirms a match
// against the wrong company.
func DomainMatchesName(domain, name string) bool {
	label := strings.ReplaceAll(strings.Split(Registrable(domain), ".")[0], "-", "")
	tokens := NameTokens(name)
	if label == "" || len(tokens) == 0 {
		return false
	}

	joined := strings.Join(tokens, "")
	if label == joined || label == tokens[0] {
		return true
	}
	// "River Mobility" -> rivermobility.com, rivermobility.in
	if strings.HasPrefix(joined, label) && len(label) >= 5 {
		return true
	}
	return strings.HasPrefix(label, joined) && len(joined) >= 5
}

// CandidateDomains lists the outbound hosts of an article, in first-seen order.
func CandidateDomains(html string) []string {
	var seen []string
	for _, m := range linkRe.FindAllStringSubmatch(html, -1) {
		parsed, err := url.Parse(m[1])
		if err != nil {
			continue
		}
		host := Registrable(parsed.Hostname())
		if host == "" || !strings.Contains(host, ".") || isPublisher(host) {
			continue
		}
		if !contains(seen, host) {
			seen = append(seen, host)
		}
	}
	return seen
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

var (
	TLDs       = []string{"com", "in", "co", "io", "ai"}
	Prefixes   = []string{"", "get", "try"}
	MaxGuesses = 7
)

var parkedRe = regexp.MustCompile(`(?i)(?:domain\s+(?:is\s+)?for\s+sale|buy\s+this\s+domain` +
	`|parked\s+(?:free\s+)?(?:at|by|domain)|this\s+domain\s+(?:may\s+be|is)\s+for\s+sale` +
	`|godaddy\s+domain|under\s+construction|coming\s+soon)`)

// legalOnly holds only legal-form suffixes. Unlike NameTokens (used for
// comparing names), domain generation must keep words like "labs" and
// "technologies": the domain for "InRisk Labs" is far more likely to be
// inrisklabs.com than inrisk.com.
var legalOnly = map[string]struct{}{
	"inc": {}, "ltd": {}, "pvt": {}, "limited": {}, "private": {},
	"llp": {}, "corp": {}, "co": {},
}

func DomainTokens(name string) []string {
	var out []string
	for _, w := range nonAlnumRe.Split(strings.ToLower(name), -1) {
		if w == "" {
			continue
		}
		if _, legal := legalOnly[w]; !legal {
			out = append(out, w)
		}
	}
	return out
}

// GuessDomains returns plausible domains for a company name — candidates to
// verify, not answers.
//
// Only for names with two or more meaningful tokens. A single-token name is
// precisely the collision the guard exists for: "Atlas" would resolve to
// atlas.com, whose content mentions "Atlas", and the verification below would
// happily confirm the wrong company. Those go to human review instead.
func GuessDomains(name string) []string {
	tokens := DomainTokens(name)
	if len(tokens) < 2 {
		return nil
	}
	joined := strings.Join(tokens, "")
	if !isAlnum(joined) || len(joined) < 4 {
		return nil
	}

	var out []string
	for _, prefix := range Prefixes {
		for _, tld := range TLDs {
			candidate := prefix + joined + "." + tld
			if !contains(out, candidate) {
				out = append(out, candidate)
			}
		}
	}
	if len(out) > MaxGuesses {
		out = out[:MaxGuesses]
	}
	return out
}

func isAlnum(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// VerifyDomain asks whether a real site lives here, and whether it says it is
// this company.
func VerifyDomain(domain, name string) bool {
	resp := httpx.Get("https://"+domain, httpx.Options{})
	if !resp.OK() || resp.Body == "" {
		return false
	}
	body := strings.ToLower(head(resp.Body, 20000))
	if parkedRe.MatchString(body) {
		return false
	}
	// The site must name the company, not merely resolve.
	for _, token := range NameTokens(name) {
		if !strings.Contains(body, token) {
			return false
		}
	}
	return true
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ResolveDomain returns (domain, reason). The article is fetched only to read
// its outbound links; the body is never stored, per the no-article-text rule.
//
// Two paths, in order of trustworthiness: a link in the article, then a
// name-derived domain that is fetched and confirmed to describe the company.
// Failing both, the answer is empty — which sends the item to review rather than
// attaching it to a company it might not be.
func ResolveDomain(articleURL, companyName string, allowGuessing bool) (string, string) {
	if articleURL == "" || companyName == "" {
		return "", "no article url or company name"
	}

	if !httpx.Allowed(articleURL) {
		return "", "article disallowed by robots.txt"
	}

	resp := httpx.Get(articleURL, httpx.Options{})
	if !resp.OK() {
		detail := resp.Err
		if detail == "" {
			detail = fmt.Sprintf("%d", resp.Status)
		}
		return "", fmt.Sprintf("could not fetch article (%s)", detail)
	}
	for _, host := range CandidateDomains(resp.Body) {
		if DomainMatchesName(host, companyName) {
			return host, fmt.Sprintf("confirmed by outbound link in the article (%s)", host)
		}
	}

	if !allowGuessing {
		return "", "no outbound link matched the company name"
	}

	guesses := GuessDomains(companyName)
	if len(guesses) == 0 {
		return "", "no outbound link matched, and the name is a single word — " +
			"too collision-prone to resolve by guessing"
	}
	for _, domain := range guesses {
		if VerifyDomain(domain, companyName) {
			return domain, fmt.Sprintf("no article link; verified %s names the company", domain)
		}
	}
	return "", fmt.Sprintf(
		"no outbound link matched and none of %d candidate domains verified", len(guesses))
}

// ------------------------------------------------------------------- matching

// MatchOnDomain is the only way a funding item is allowed to attach to a company.
func MatchOnDomain(conn *sql.DB, domain string) (db.Company, bool, error) {
	if domain == "" {
		return db.Company{}, false, nil
	}
	row := conn.QueryRow(
		"SELECT "+db.CompanyColumns+" FROM companies WHERE lower(domain) = lower(?)",
		Registrable(domain))
	c, err := db.ScanCompany(row)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Company{}, false, nil
	}
	if err != nil {
		return db.Company{}, false, fmt.Errorf("match domain %q: %w", domain, err)
	}
	return c, true, nil
}

// Collision is a watchlist company whose name resembles a funded one.
type Collision struct {
	ID     int64
	Name   string
	Domain sql.NullString
}

// NameCollisions lists companies whose name looks like this one. Reported,
// never matched — this function exists to explain a near-miss to the user, not
// to resolve it.
func NameCollisions(conn *sql.DB, name string) ([]Collision, error) {
	tokens := NameTokens(name)
	if len(tokens) == 0 {
		return nil, nil
	}

	rows, err := conn.Query("SELECT id, name, domain FROM companies")
	if err != nil {
		return nil, fmt.Errorf("read companies for collision check: %w", err)
	}
	defer rows.Close()

	var hits []Collision
	for rows.Next() {
		var c Collision
		if err := rows.Scan(&c.ID, &c.Name, &c.Domain); err != nil {
			return nil, fmt.Errorf("scan company: %w", err)
		}
		other := NameTokens(c.Name)
		if len(other) == 0 {
			continue
		}
		if equalTokens(other, tokens) || (tokens[0] == other[0] && len(tokens[0]) >= 4) {
			hits = append(hits, c)
		}
	}
	return hits, rows.Err()
}

func equalTokens(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
