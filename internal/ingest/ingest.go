// Package ingest is append-only: upsert by (source, external_id), link
// duplicates, never delete.
//
// INV-1 lives here. The two rules that matter:
//   - re-polling the same board must not create a second row, and
//   - discovering the same role on a second board must not remove the first.
package ingest

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Vaivaswat2244/job-tracker/internal/ats"
	"github.com/Vaivaswat2244/job-tracker/internal/db"
	"github.com/Vaivaswat2244/job-tracker/internal/normalize"
)

const maxRawJSON = 200_000

func GetOrCreateCompany(conn *sql.DB, name, domain string) (int64, error) {
	if domain != "" {
		var id int64
		err := conn.QueryRow(
			"SELECT id FROM companies WHERE lower(domain) = lower(?)", domain).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("look up company by domain %q: %w", domain, err)
		}
	}

	var (
		id       int64
		existing sql.NullString
	)
	err := conn.QueryRow(
		"SELECT id, domain FROM companies WHERE lower(name) = lower(?)", name).Scan(&id, &existing)
	switch {
	case err == nil:
		// Backfill a domain we have now but did not have when the row was created.
		if domain != "" && !existing.Valid {
			if _, err := conn.Exec(
				"UPDATE companies SET domain = ? WHERE id = ?", domain, id); err != nil {
				return 0, fmt.Errorf("backfill domain for company %d: %w", id, err)
			}
		}
		return id, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("look up company by name %q: %w", name, err)
	}

	res, err := conn.Exec("INSERT INTO companies (name, domain) VALUES (?,?)",
		name, nilIfEmpty(domain))
	if err != nil {
		return 0, fmt.Errorf("insert company %q: %w", name, err)
	}
	return res.LastInsertId()
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func DedupeKey(companyName, title, postedAt string) string {
	return strings.Join([]string{
		normalize.NormCompany(companyName),
		normalize.NormTitle(title),
		normalize.PostedWeek(postedAt),
	}, "|")
}

// appendSourceURL records every URL a role was seen at, on the canonical row.
// It never replaces.
func appendSourceURL(conn *sql.DB, jobID int64, url string) error {
	if url == "" {
		return nil
	}
	var stored sql.NullString
	if err := conn.QueryRow(
		"SELECT source_urls FROM jobs WHERE id = ?", jobID).Scan(&stored); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("read source_urls for job %d: %w", jobID, err)
	}

	var urls []string
	if stored.Valid && stored.String != "" {
		// A malformed value is replaced rather than fatal: the column is a
		// convenience index, not the record itself.
		if err := json.Unmarshal([]byte(stored.String), &urls); err != nil {
			urls = nil
		}
	}
	for _, u := range urls {
		if u == url {
			return nil
		}
	}

	encoded, err := json.Marshal(append(urls, url))
	if err != nil {
		return fmt.Errorf("encode source_urls for job %d: %w", jobID, err)
	}
	if _, err := conn.Exec(
		"UPDATE jobs SET source_urls = ? WHERE id = ?", string(encoded), jobID); err != nil {
		return fmt.Errorf("update source_urls for job %d: %w", jobID, err)
	}
	return nil
}

// LinkDuplicate points this row at the oldest row from a *different* source
// sharing its dedupe key, returning that row's id and whether one was found.
//
// Cross-source only. Within one board, two postings with distinct external_ids
// are distinct postings — the ATS says so. Collapsing them would hide the role a
// company opened in six cities behind a single row and lose five real
// application URLs, which is the loss INV-1 forbids.
//
// Deleting the newer row would lose a URL too, so the row stays and only gains
// a pointer.
func LinkDuplicate(conn *sql.DB, jobID int64, key, url, source string) (int64, bool, error) {
	if key == "" || strings.Count(key, "|") != 2 || strings.Trim(key, "|") == "" {
		return 0, false, nil
	}

	var canonical int64
	err := conn.QueryRow(
		"SELECT id FROM jobs WHERE dedupe_key = ? AND id != ? AND canonical_id IS NULL"+
			" AND ifnull(source,'') != ifnull(?,'') ORDER BY id LIMIT 1",
		key, jobID, nilIfEmpty(source),
	).Scan(&canonical)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("find canonical for job %d: %w", jobID, err)
	}

	if _, err := conn.Exec(
		"UPDATE jobs SET canonical_id = ? WHERE id = ?", canonical, jobID); err != nil {
		return 0, false, fmt.Errorf("link job %d to canonical %d: %w", jobID, canonical, err)
	}
	if err := appendSourceURL(conn, canonical, url); err != nil {
		return 0, false, err
	}
	return canonical, true, nil
}

const updateSQL = `
UPDATE jobs SET
    company_id = ?, title = ?, url = ?, posted_at = ?, seen_at = ?,
    jd_text = CASE WHEN ? != '' THEN ? ELSE jd_text END,
    location = ?, employment_type = ?, remote = ?,
    pay_min = ?, pay_max = ?, pay_currency = ?,
    comp_model = ?, hires_in_india = ?, auth_required = ?,
    dedupe_key = ?, raw_json = ?
WHERE id = ?
`

const insertSQL = `
INSERT INTO jobs (company_id, title, url, source, external_id, posted_at, seen_at,
    first_seen_at, jd_text, location, employment_type, remote, pay_min, pay_max,
    pay_currency, comp_model, hires_in_india, auth_required, dedupe_key, raw_json,
    source_urls)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
`

// Upsert inserts or refreshes one posting, returning (jobID, "new"|"updated").
func Upsert(conn *sql.DB, companyID int64, companyName string, job ats.NormalizedJob) (int64, string, error) {
	now := db.Now()
	postedAt := deref(job.PostedAt)
	key := DedupeKey(companyName, job.Title, postedAt)

	compModel := normalize.CompModel(job.JDText, deref(job.PayCurrency))
	auth := normalize.AuthRequired(job.JDText)

	var india any
	if v, ok := normalize.HiresInIndia(job.JDText, deref(job.Location)); ok {
		india = v
	}

	var remote any
	if job.Remote != nil {
		remote = boolToInt(*job.Remote)
	}

	raw := rawJSON(job.Raw)

	var existing int64
	err := conn.QueryRow(
		"SELECT id FROM jobs WHERE source = ? AND external_id = ?",
		job.Source, job.ExternalID).Scan(&existing)

	switch {
	case err == nil:
		if _, err := conn.Exec(updateSQL,
			companyID, job.Title, job.URL, nilIfEmpty(postedAt), now,
			job.JDText, job.JDText,
			ptrArg(job.Location), ptrArg(job.EmploymentType), remote,
			ptrArg(job.PayMin), ptrArg(job.PayMax), ptrArg(job.PayCurrency),
			compModel, india, auth, key, raw, existing,
		); err != nil {
			return 0, "", fmt.Errorf("update job %d: %w", existing, err)
		}
		if err := appendSourceURL(conn, existing, job.URL); err != nil {
			return 0, "", err
		}
		return existing, "updated", nil

	case !errors.Is(err, sql.ErrNoRows):
		return 0, "", fmt.Errorf("look up job %s/%s: %w", job.Source, job.ExternalID, err)
	}

	sourceURLs := "[]"
	if job.URL != "" {
		encoded, _ := json.Marshal([]string{job.URL})
		sourceURLs = string(encoded)
	}

	res, err := conn.Exec(insertSQL,
		companyID, job.Title, job.URL, job.Source, job.ExternalID, nilIfEmpty(postedAt),
		now, now, job.JDText, ptrArg(job.Location), ptrArg(job.EmploymentType), remote,
		ptrArg(job.PayMin), ptrArg(job.PayMax), ptrArg(job.PayCurrency),
		compModel, india, auth, key, raw, sourceURLs,
	)
	if err != nil {
		return 0, "", fmt.Errorf("insert job %s/%s: %w", job.Source, job.ExternalID, err)
	}
	jobID, err := res.LastInsertId()
	if err != nil {
		return 0, "", err
	}
	if _, _, err := LinkDuplicate(conn, jobID, key, job.URL, job.Source); err != nil {
		return 0, "", err
	}
	return jobID, "new", nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ptrArg passes a nil pointer through as SQL NULL rather than a zero value.
func ptrArg[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// rawJSON serialises the provider payload, truncated. It returns nil for an
// empty payload so the column stays NULL.
func rawJSON(raw map[string]any) any {
	if len(raw) == 0 {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	if len(encoded) > maxRawJSON {
		return string(encoded[:maxRawJSON])
	}
	return string(encoded)
}

// Renormalize re-runs the heuristics over stored rows.
//
// Needed whenever comp_model/auth_required/hires_in_india change: a poll only
// refreshes a row the board still lists, so without this an improved rule would
// never reach the archive. Touches derived columns only.
func Renormalize(conn *sql.DB) (int, error) {
	rows, err := conn.Query("SELECT id, jd_text, location, pay_currency FROM jobs")
	if err != nil {
		return 0, fmt.Errorf("read jobs for renormalize: %w", err)
	}

	type record struct {
		id                            int64
		jdText, location, payCurrency sql.NullString
	}
	var records []record
	for rows.Next() {
		var r record
		if err := rows.Scan(&r.id, &r.jdText, &r.location, &r.payCurrency); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan job: %w", err)
		}
		records = append(records, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, r := range records {
		var india any
		if v, ok := normalize.HiresInIndia(r.jdText.String, r.location.String); ok {
			india = v
		}
		if _, err := conn.Exec(
			"UPDATE jobs SET comp_model = ?, auth_required = ?, hires_in_india = ? WHERE id = ?",
			normalize.CompModel(r.jdText.String, r.payCurrency.String),
			normalize.AuthRequired(r.jdText.String),
			india, r.id,
		); err != nil {
			return 0, fmt.Errorf("renormalize job %d: %w", r.id, err)
		}
	}
	return len(records), nil
}

// Stats is the per-board ingest tally.
type Stats struct {
	New     int
	Updated int
	Skipped int
}

func Ingest(conn *sql.DB, companyID int64, companyName string, jobs []ats.NormalizedJob) (Stats, error) {
	var stats Stats
	for _, job := range jobs {
		if job.ExternalID == "" || job.Title == "" {
			if err := db.LogExclusion(conn, truncRaw(job.Raw),
				"posting had no usable id or title", "ingest.incomplete_posting"); err != nil {
				return stats, err
			}
			stats.Skipped++
			continue
		}

		_, outcome, err := Upsert(conn, companyID, companyName, job)
		if err != nil {
			// One malformed posting must not abort the other 40 on the board.
			if logErr := db.LogExclusion(conn, truncRaw(job.Raw),
				fmt.Sprintf("upsert failed: %v", err), "ingest.upsert_error"); logErr != nil {
				return stats, logErr
			}
			stats.Skipped++
			continue
		}
		if outcome == "new" {
			stats.New++
		} else {
			stats.Updated++
		}
	}
	return stats, nil
}

func truncRaw(raw map[string]any) string {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return "{}"
	}
	if len(encoded) > 5000 {
		return string(encoded[:5000])
	}
	return string(encoded)
}
