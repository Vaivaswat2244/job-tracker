// Row building, shared by every destination the data is exported to.
//
// The xlsx writer and the Google Sheets push both consume Tables from here.
// Keeping the SQL and the cell values in one place is the same argument
// jobs.Filter makes: two renderings of "the pipeline" that drift apart are
// worse than one that is merely plain.
package export

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/Vaivaswat2244/job-tracker/internal/dates"
	"github.com/Vaivaswat2244/job-tracker/internal/jobs"
)

// Table is one sheet's worth of data. A nil element in Rows is an empty cell,
// not a zero — SQL NULL and 0 mean different things to a reader.
type Table struct {
	Name    string
	Headers []string
	Rows    [][]any
}

// Col returns the 1-based index of a header, or 0 if absent.
func (t Table) Col(name string) int {
	for i, h := range t.Headers {
		if h == name {
			return i + 1
		}
	}
	return 0
}

var applicationHeaders = []string{
	"app_id", "job_id", "company", "title", "status", "applied_at",
	"followup_due", "days_since_applied", "source", "comp_model",
	"pay_min", "pay_max", "currency", "auth_required", "contacts", "url", "notes",
}

// applicationQuery orders NULL follow-up dates last, then by due date — the
// soonest follow-up is what the user opens the sheet to see.
const applicationQuery = `
SELECT a.id AS app_id, j.id AS job_id, c.name AS company, j.title, a.status,
       a.applied_at, a.followup_due, j.source, j.comp_model, j.pay_min, j.pay_max,
       j.pay_currency, j.auth_required, j.url, a.notes,
       (SELECT count(*) FROM contacts ct WHERE ct.company_id = c.id) AS contacts,
       CAST(julianday('now') - julianday(a.applied_at) AS INTEGER) AS days_since_applied
FROM applications a
JOIN jobs j ON j.id = a.job_id
JOIN companies c ON c.id = j.company_id
ORDER BY (a.followup_due IS NULL), a.followup_due, a.id
`

// Applications is what the user has actually applied to.
func Applications(conn *sql.DB) (Table, error) {
	t := Table{Name: "Applications", Headers: applicationHeaders}

	rows, err := conn.Query(applicationQuery)
	if err != nil {
		return t, fmt.Errorf("read applications: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			appID, jobID                int64
			company, title, status      string
			appliedAt, followupDue      sql.NullString
			source, compModel, currency sql.NullString
			payMin, payMax              sql.NullFloat64
			authRequired                sql.NullInt64
			url, notes                  sql.NullString
			contacts                    int
			daysSinceApplied            sql.NullInt64
		)
		if err := rows.Scan(&appID, &jobID, &company, &title, &status,
			&appliedAt, &followupDue, &source, &compModel, &payMin, &payMax,
			&currency, &authRequired, &url, &notes, &contacts, &daysSinceApplied); err != nil {
			return t, fmt.Errorf("scan application: %w", err)
		}
		t.Rows = append(t.Rows, []any{
			appID, jobID, company, title, status, nullAny(appliedAt), nullAny(followupDue),
			nullIntAny(daysSinceApplied), nullAny(source), nullAny(compModel),
			nullFloatAny(payMin), nullFloatAny(payMax), nullAny(currency),
			nullIntAny(authRequired), contacts, nullAny(url), nullAny(notes),
		})
	}
	return t, rows.Err()
}

var pipelineHeaders = []string{
	"job_id", "company", "title", "location", "seen", "source", "comp_model",
	"pay_min", "pay_max", "currency", "india", "auth_required", "flags", "url",
}

// Pipeline is everything poll ingested. The Applications table starts FROM
// applications, so before anything is applied to it is empty and says nothing
// about the thousands of roles actually collected.
//
// It shares jobs.Filter with the `jobs` command so the sheet and the terminal
// can never disagree about what is in the pipeline or how it is ranked.
func Pipeline(conn *sql.DB) (Table, error) {
	t := Table{Name: "Pipeline", Headers: pipelineHeaders}

	rows, err := jobs.List(conn, jobs.Filter{})
	if err != nil {
		return t, err
	}
	for _, r := range rows {
		// Date-only: the sheet is read by a human, and the clock time of an
		// overnight poll is noise next to the day the role appeared.
		seen := r.Seen
		if d := dates.ParseDay(seen); !d.IsZero() {
			seen = d.Format("2006-01-02")
		}
		t.Rows = append(t.Rows, []any{
			r.ID, r.Company, r.Title, nullAny(r.Location), seen, nullAny(r.Source),
			nullAny(r.CompModel), nullFloatAny(r.PayMin), nullFloatAny(r.PayMax),
			nullAny(r.PayCurrency), nullIntAny(r.HiresInIndia), nullIntAny(r.AuthRequired),
			nilIfEmpty(r.Flags()), r.URL,
		})
	}
	return t, nil
}

// NewWindow is what "posted today" means in practice. A calendar day would show
// almost nothing first thing in the morning, when the only poll since midnight
// ran an hour ago; a rolling day always answers "what appeared since I last
// looked".
const NewWindow = 24 * time.Hour

// NewRoles is everything first seen inside NewWindow — the daily read, as
// opposed to Pipeline's standing list of thousands.
//
// It keys on first_seen_at, not posted_at: a board that backdates its postings
// would otherwise hide a role that only became visible to us today, and what
// matters is when you could first have applied.
func NewRoles(conn *sql.DB, now time.Time) (Table, error) {
	t, err := Pipeline(conn)
	if err != nil {
		return t, err
	}
	full := t.Rows
	t.Name = "New (24h)"
	t.Rows = nil

	cutoff := now.Add(-NewWindow).UTC().Format("2006-01-02")
	seen := t.Col("seen") - 1
	for _, row := range full {
		if seen < 0 || seen >= len(row) {
			continue
		}
		day, _ := row[seen].(string)
		// Pipeline has already rendered seen as a day, so a string compare on
		// ISO dates is the same as a date compare.
		if day >= cutoff {
			t.Rows = append(t.Rows, row)
		}
	}
	return t, nil
}

var contactHeaders = []string{
	"id", "company", "name", "title", "email", "confidence", "linkedin", "source",
}

// Contacts holds other people's details, so it is deliberately not among the
// tables gsheet will push: see the allow-list there.
func Contacts(conn *sql.DB) (Table, error) {
	t := Table{Name: "Contacts", Headers: contactHeaders}

	rows, err := conn.Query(
		"SELECT ct.id, c.name AS company, ct.name, ct.title, ct.email, ct.email_confidence," +
			" ct.linkedin_url, ct.source FROM contacts ct" +
			" JOIN companies c ON c.id = ct.company_id ORDER BY c.name, ct.name")
	if err != nil {
		return t, fmt.Errorf("read contacts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id                   int64
			company, name, title string
			email, confidence    sql.NullString
			linkedin, source     sql.NullString
		)
		if err := rows.Scan(&id, &company, &name, &title, &email,
			&confidence, &linkedin, &source); err != nil {
			return t, fmt.Errorf("scan contact: %w", err)
		}

		// INV-2: an inferred address must never be pasteable as-is.
		shown := email.String
		if email.Valid && email.String != "" && confidence.String == "inferred" {
			shown = "[UNVERIFIED: " + email.String + "]"
		}
		t.Rows = append(t.Rows, []any{
			id, company, name, title, nullOr(email, shown),
			nullAny(confidence), nullAny(linkedin), nullAny(source),
		})
	}
	return t, rows.Err()
}

// nilIfEmpty keeps an empty flags cell blank rather than writing "".
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullAny and friends leave a SQL NULL as an empty cell rather than writing a
// zero value, matching what openpyxl did with None.
func nullAny(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}

func nullOr(v sql.NullString, replacement string) any {
	if !v.Valid {
		return nil
	}
	return replacement
}

func nullIntAny(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

func nullFloatAny(v sql.NullFloat64) any {
	if !v.Valid {
		return nil
	}
	return v.Float64
}
