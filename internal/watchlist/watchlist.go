// Package watchlist owns watchlist.yaml: the hand-curated list of companies
// whose boards we poll.
//
// The YAML file is the source of truth for *membership*; the DB holds the
// operational state (last_polled_at, etag, funding window). Sync pushes the
// former into the latter and never the other way round, so the user can always
// read and edit the list in git.
package watchlist

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Vaivaswat2244/job-tracker/internal/db"
)

// CadenceHours is the poll cadence by effective priority.
var CadenceHours = map[string]int{"high": 3, "normal": 6}

const FundingWindowDays = 60

func Path() string {
	if p := os.Getenv("TRACKER_WATCHLIST"); p != "" {
		return p
	}
	return "watchlist.yaml"
}

// Entry is one watchlist.yaml company. Field order here is the order Save
// writes them in, and omitempty reproduces Python's "skip keys that are None".
type Entry struct {
	Name       string `yaml:"name,omitempty"`
	Domain     string `yaml:"domain,omitempty"`
	ATS        string `yaml:"ats,omitempty"`
	Slug       string `yaml:"slug,omitempty"`
	CareersURL string `yaml:"careers_url,omitempty"`
	Source     string `yaml:"source,omitempty"`
	OSSRepo    string `yaml:"oss_repo,omitempty"`
	Priority   string `yaml:"priority,omitempty"`
	// Enabled is a pointer so an explicit `enabled: false` survives a
	// load/save round trip instead of being dropped as a zero value.
	Enabled *bool `yaml:"enabled,omitempty"`
}

func (e Entry) IsEnabled() bool { return e.Enabled == nil || *e.Enabled }

type file struct {
	Companies []Entry `yaml:"companies"`
}

// Load reads the watchlist, skipping entries with no name. A missing file means
// an empty watchlist, not an error.
func Load(path string) ([]Entry, error) {
	if path == "" {
		path = Path()
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var doc file
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	out := make([]Entry, 0, len(doc.Companies))
	for _, c := range doc.Companies {
		if strings.TrimSpace(c.Name) != "" {
			out = append(out, c)
		}
	}
	return out, nil
}

const header = "# Hand-curated watchlist. `tracker watchlist add <careers_url>` appends here.\n" +
	"# ats: greenhouse | lever | ashby | unknown   priority: normal | high\n"

func Save(companies []Entry, path string) error {
	if path == "" {
		path = Path()
	}
	body, err := yaml.Marshal(file{Companies: companies})
	if err != nil {
		return fmt.Errorf("encode watchlist: %w", err)
	}
	if err := os.WriteFile(path, append([]byte(header), body...), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func Append(entry Entry, path string) error {
	companies, err := Load(path)
	if err != nil {
		return err
	}
	return Save(append(companies, entry), path)
}

// Find matches on domain first, then name — both case-insensitively.
func Find(companies []Entry, name, domain string) (Entry, bool) {
	for _, c := range companies {
		if domain != "" && strings.EqualFold(c.Domain, domain) {
			return c, true
		}
		if name != "" && strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return Entry{}, false
}

// ------------------------------------------------------------------------- sync

// Sync upserts watchlist entries into `companies`, returning (added, updated).
//
// Membership only — operational columns (last_polled_at, etag, funding state)
// are never touched here.
func Sync(conn *sql.DB, path string) (int, int, error) {
	entries, err := Load(path)
	if err != nil {
		return 0, 0, err
	}

	added, updated := 0, 0
	for _, entry := range entries {
		name := strings.TrimSpace(entry.Name)
		domain := strings.ToLower(strings.TrimSpace(entry.Domain))

		var id int64
		found := false
		if domain != "" {
			err := conn.QueryRow(
				"SELECT id FROM companies WHERE lower(domain) = ?", domain).Scan(&id)
			if err == nil {
				found = true
			} else if !errors.Is(err, sql.ErrNoRows) {
				return added, updated, fmt.Errorf("look up company by domain %q: %w", domain, err)
			}
		}
		if !found {
			err := conn.QueryRow(
				"SELECT id FROM companies WHERE lower(name) = lower(?)", name).Scan(&id)
			if err == nil {
				found = true
			} else if !errors.Is(err, sql.ErrNoRows) {
				return added, updated, fmt.Errorf("look up company by name %q: %w", name, err)
			}
		}

		ats := entry.ATS
		if ats == "" {
			ats = "unknown"
		}
		priority := entry.Priority
		if priority == "" {
			priority = "normal"
		}
		enabled := 1
		if !entry.IsEnabled() {
			enabled = 0
		}

		args := []any{
			name, nilIfEmpty(domain), nilIfEmpty(entry.CareersURL), ats,
			nilIfEmpty(entry.Slug), priority, enabled,
			nilIfEmpty(entry.OSSRepo), nilIfEmpty(entry.Source),
		}

		if found {
			// COALESCE on the optional columns so a YAML entry that omits a
			// field does not erase a value already in the DB.
			_, err = conn.Exec(
				"UPDATE companies SET name=?, domain=COALESCE(?, domain),"+
					" careers_url=COALESCE(?, careers_url), ats=?, slug=?, priority=?,"+
					" watchlist_enabled=?, oss_repo=COALESCE(?, oss_repo),"+
					" discovery_source=COALESCE(?, discovery_source) WHERE id=?",
				append(args, id)...,
			)
			if err != nil {
				return added, updated, fmt.Errorf("update company %q: %w", name, err)
			}
			updated++
		} else {
			_, err = conn.Exec(
				"INSERT INTO companies (name, domain, careers_url, ats, slug, priority,"+
					" watchlist_enabled, oss_repo, discovery_source) VALUES (?,?,?,?,?,?,?,?,?)",
				args...,
			)
			if err != nil {
				return added, updated, fmt.Errorf("insert company %q: %w", name, err)
			}
			added++
		}
	}
	return added, updated, nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// --------------------------------------------------------------------- cadence

// EffectivePriority is the baseline priority from the YAML, raised to "high"
// while a funding window is open.
//
// Computed rather than stored, so the window decays on its own the moment it
// expires — there is no cron step that can fail to run.
func EffectivePriority(c db.Company, now time.Time) string {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if c.PriorityOr() == "high" {
		return "high"
	}
	if until, ok := parseTime(c.PriorityUntil); ok && now.Before(until) {
		return "high"
	}
	return "normal"
}

// parseTime accepts the ISO shapes the DB stores, defaulting a naive value to UTC.
func parseTime(v sql.NullString) (time.Time, bool) {
	if !v.Valid || v.String == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		db.ISO8601,
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, v.String); err == nil {
			if strings.Contains(layout, "07:00") {
				return t, true
			}
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func DueForPoll(c db.Company, now time.Time) bool {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	last, ok := parseTime(c.LastPolledAt)
	if !ok {
		return true
	}
	hours := CadenceHours[EffectivePriority(c, now)]
	return now.Sub(last) >= time.Duration(hours)*time.Hour
}

// Pollable lists watchlist companies with a usable board. "unknown" providers
// stay in the list and stay visible; they are simply not pollable until a slug
// is known.
func Pollable(conn *sql.DB) ([]db.Company, error) {
	rows, err := conn.Query(
		"SELECT " + db.CompanyColumns + " FROM companies WHERE watchlist_enabled = 1" +
			" AND ats IN ('greenhouse','lever','ashby') AND slug IS NOT NULL AND slug != ''" +
			" ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("read pollable companies: %w", err)
	}
	defer rows.Close()

	var out []db.Company
	for rows.Next() {
		c, err := db.ScanCompany(rows)
		if err != nil {
			return nil, fmt.Errorf("scan company: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func MarkPolled(conn *sql.DB, companyID int64, etag, lastModified string) error {
	_, err := conn.Exec(
		"UPDATE companies SET last_polled_at=?, poll_etag=?, poll_last_modified=? WHERE id=?",
		db.Now(), nilIfEmpty(etag), nilIfEmpty(lastModified), companyID,
	)
	if err != nil {
		return fmt.Errorf("mark company %d polled: %w", companyID, err)
	}
	return nil
}
