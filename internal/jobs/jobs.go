// Package jobs is the read side of the ingested pipeline: everything `poll`
// collected, which `list` never shows because `list` is about applications.
//
// The filter lives here rather than in the CLI so the `jobs` command and the
// spreadsheet's Pipeline sheet cannot drift apart.
package jobs

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Row is one ingested job, joined to its company.
type Row struct {
	ID           int64
	Company      string
	Title        string
	Location     sql.NullString
	URL          string
	Source       sql.NullString
	CompModel    sql.NullString
	PayMin       sql.NullFloat64
	PayMax       sql.NullFloat64
	PayCurrency  sql.NullString
	AuthRequired sql.NullInt64
	HiresInIndia sql.NullInt64
	Seen         string // first_seen_at, or seen_at for hand-added rows
}

// Filter narrows the list. Every field is optional; the zero Filter is
// "everything, newest first".
type Filter struct {
	Company           string // substring, case-insensitive
	Title             string // substring, case-insensitive
	IndiaOnly         bool
	RemoteOnly        bool
	Since             time.Time // zero means no lower bound
	IncludeDuplicates bool      // canonical_id rows are hidden by default
	Limit             int       // 0 means no limit
}

// selectClause coalesces first_seen_at with seen_at so hand-added jobs, which
// only ever get seen_at, are not invisible in their own pipeline.
const selectClause = `
SELECT j.id, c.name, j.title, j.location, j.url, j.source, j.comp_model,
       j.pay_min, j.pay_max, j.pay_currency, j.auth_required, j.hires_in_india,
       COALESCE(j.first_seen_at, j.seen_at) AS seen
FROM jobs j
JOIN companies c ON c.id = j.company_id`

// orderClause: a flag sorts a role down, it never removes it (INV-1).
// Auth-required roles land last, India-friendly roles first.
//
// Both flags are COALESCEd because `NULL = 1` is NULL, not false, and SQLite
// sorts NULL last under DESC. Without it every job whose geography could not be
// determined — 257 of them on real data, plus every hand-added referral, which
// never gets the column set — would sink below known-not-India roles. Unknown
// is not the same as no.
const orderClause = `
ORDER BY COALESCE(j.auth_required, 0) ASC,
         (COALESCE(j.hires_in_india, 0) = 1) DESC,
         seen DESC, j.id DESC`

// SQL builds the statement and its arguments.
func (f Filter) SQL() (string, []any) {
	var (
		where []string
		args  []any
	)
	if !f.IncludeDuplicates {
		where = append(where, "j.canonical_id IS NULL")
	}
	if f.Company != "" {
		where = append(where, "lower(c.name) LIKE ?")
		args = append(args, "%"+strings.ToLower(f.Company)+"%")
	}
	if f.Title != "" {
		where = append(where, "lower(j.title) LIKE ?")
		args = append(args, "%"+strings.ToLower(f.Title)+"%")
	}
	if f.IndiaOnly {
		where = append(where, "j.hires_in_india = 1")
	}
	if f.RemoteOnly {
		where = append(where, "j.remote = 1")
	}
	if !f.Since.IsZero() {
		where = append(where, "COALESCE(j.first_seen_at, j.seen_at) >= ?")
		args = append(args, f.Since.UTC().Format("2006-01-02T15:04:05-07:00"))
	}

	query := selectClause
	if len(where) > 0 {
		query += "\nWHERE " + strings.Join(where, " AND ")
	}
	query += orderClause
	if f.Limit > 0 {
		query += fmt.Sprintf("\nLIMIT %d", f.Limit)
	}
	return query, args
}

// List runs the filter.
func List(conn *sql.DB, f Filter) ([]Row, error) {
	query, args := f.SQL()
	rows, err := conn.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("select jobs: %w", err)
	}
	defer rows.Close()

	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.Company, &r.Title, &r.Location, &r.URL,
			&r.Source, &r.CompModel, &r.PayMin, &r.PayMax, &r.PayCurrency,
			&r.AuthRequired, &r.HiresInIndia, &r.Seen); err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Count reports how many rows the filter matches, ignoring Limit, so the CLI
// can say "showing 50 of 2396".
func (f Filter) Count(conn *sql.DB) (int, error) {
	unlimited := f
	unlimited.Limit = 0
	query, args := unlimited.SQL()
	// Wrap rather than rebuild: one filter implementation, no second copy to
	// keep in step.
	var n int
	err := conn.QueryRow("SELECT count(*) FROM ("+query+")", args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count jobs: %w", err)
	}
	return n, nil
}

// Flags renders the sortable signals as a short column: India-friendly,
// authorization-gated, and a non-obvious comp model.
func (r Row) Flags() string {
	var out []string
	if r.HiresInIndia.Valid && r.HiresInIndia.Int64 == 1 {
		out = append(out, "IN")
	}
	if r.AuthRequired.Valid && r.AuthRequired.Int64 == 1 {
		out = append(out, "AUTH")
	}
	if r.CompModel.Valid && r.CompModel.String != "" && r.CompModel.String != "unknown" {
		out = append(out, r.CompModel.String)
	}
	return strings.Join(out, " ")
}
