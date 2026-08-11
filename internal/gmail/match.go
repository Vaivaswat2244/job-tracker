package gmail

import (
	"database/sql"
	"fmt"
	"strings"
)

// Match is what a message resolved to, and how confidently.
type Match struct {
	CompanyID     sql.NullInt64
	JobID         sql.NullInt64
	ApplicationID sql.NullInt64
	CompanyGuess  string // what the subject claimed, kept even when unmatched
	Reason        string // why review is needed; empty when confident
}

// Confident reports whether the match is safe to act on without asking.
func (m Match) Confident() bool { return m.Reason == "" }

// ResolveCompany finds the company a message is about.
//
// The sender's domain is the strong signal, but the common case defeats it:
// most application mail comes from greenhouse.io or lever.co on behalf of the
// employer, so for those the company has to come out of the subject line.
func ResolveCompany(conn *sql.DB, m Message) (sql.NullInt64, string, string) {
	guess := CompanyFromSubject(m.Subject)

	if !IsATS(m.From) {
		if domain := Domain(m.From); domain != "" {
			var id int64
			err := conn.QueryRow(
				"SELECT id FROM companies WHERE lower(domain) = ? OR lower(domain) = ?",
				domain, stripSubdomain(domain)).Scan(&id)
			if err == nil {
				return sql.NullInt64{Int64: id, Valid: true}, guess, ""
			}
		}
	}

	if guess == "" {
		return sql.NullInt64{}, "", "no company in the subject and the sender is a recruiting platform"
	}

	rows, err := conn.Query(
		"SELECT id, name FROM companies WHERE lower(name) = ? OR lower(name) LIKE ?",
		strings.ToLower(guess), strings.ToLower(guess)+"%")
	if err != nil {
		return sql.NullInt64{}, guess, fmt.Sprintf("company lookup failed: %v", err)
	}
	defer rows.Close()

	var ids []int64
	var names []string
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return sql.NullInt64{}, guess, fmt.Sprintf("company scan failed: %v", err)
		}
		// An exact name match ends it; prefixes are only a fallback.
		if strings.EqualFold(name, guess) {
			return sql.NullInt64{Int64: id, Valid: true}, guess, ""
		}
		ids = append(ids, id)
		names = append(names, name)
	}
	switch len(ids) {
	case 0:
		return sql.NullInt64{}, guess, fmt.Sprintf("no company named %q on file", guess)
	case 1:
		return sql.NullInt64{Int64: ids[0], Valid: true}, guess, ""
	default:
		return sql.NullInt64{}, guess, fmt.Sprintf(
			"%q matches %d companies (%s)", guess, len(ids), strings.Join(names, ", "))
	}
}

// stripSubdomain turns careers.example.com into example.com so mail from a
// recruiting subdomain still matches the company on file.
func stripSubdomain(domain string) string {
	parts := strings.Split(domain, ".")
	if len(parts) <= 2 {
		return domain
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

// ResolveApplication ties a message to one application at a company.
//
// A rejection has to name an application that exists; a confirmation may be the
// first the tracker hears of it, in which case the job is what matters.
func ResolveApplication(conn *sql.DB, companyID int64, kind Kind) Match {
	m := Match{CompanyID: sql.NullInt64{Int64: companyID, Valid: true}}

	rows, err := conn.Query(
		"SELECT a.id, a.job_id, a.status FROM applications a"+
			" JOIN jobs j ON j.id = a.job_id"+
			" WHERE j.company_id = ? AND a.status NOT IN ('rejected','ghosted')"+
			" ORDER BY a.applied_at DESC", companyID)
	if err != nil {
		m.Reason = fmt.Sprintf("application lookup failed: %v", err)
		return m
	}
	defer rows.Close()

	type app struct {
		id, jobID int64
	}
	var apps []app
	for rows.Next() {
		var a app
		var status string
		if err := rows.Scan(&a.id, &a.jobID, &status); err != nil {
			m.Reason = fmt.Sprintf("application scan failed: %v", err)
			return m
		}
		apps = append(apps, a)
	}

	switch len(apps) {
	case 1:
		m.ApplicationID = sql.NullInt64{Int64: apps[0].id, Valid: true}
		m.JobID = sql.NullInt64{Int64: apps[0].jobID, Valid: true}
		return m
	case 0:
		if kind == Confirmation {
			// Nothing tracked yet — normal for a confirmation. Pick the job
			// only if the company has exactly one on file, otherwise ask.
			return resolveSingleJob(conn, companyID, m)
		}
		m.Reason = "no open application at this company to act on"
		return m
	default:
		m.Reason = fmt.Sprintf("%d open applications at this company; which one?", len(apps))
		return m
	}
}

func resolveSingleJob(conn *sql.DB, companyID int64, m Match) Match {
	var n int
	if err := conn.QueryRow(
		"SELECT count(*) FROM jobs WHERE company_id = ? AND canonical_id IS NULL",
		companyID).Scan(&n); err != nil {
		m.Reason = fmt.Sprintf("job count failed: %v", err)
		return m
	}
	if n != 1 {
		m.Reason = fmt.Sprintf("company has %d roles on file; which did you apply to?", n)
		return m
	}
	var jobID int64
	if err := conn.QueryRow(
		"SELECT id FROM jobs WHERE company_id = ? AND canonical_id IS NULL",
		companyID).Scan(&jobID); err != nil {
		m.Reason = fmt.Sprintf("job lookup failed: %v", err)
		return m
	}
	m.JobID = sql.NullInt64{Int64: jobID, Valid: true}
	return m
}

// ResolveThread finds the application a reply belongs to by its thread, which
// is exact when the tracker drafted the outreach that started it.
//
// Thread ids are per-mailbox, so the account has to be part of the lookup:
// without it a thread in one account could claim an application matched in the
// other.
func ResolveThread(conn *sql.DB, account, threadID string) (sql.NullInt64, bool) {
	if threadID == "" {
		return sql.NullInt64{}, false
	}
	var appID sql.NullInt64
	err := conn.QueryRow(
		"SELECT application_id FROM mail_messages"+
			" WHERE account = ? AND thread_id = ? AND application_id IS NOT NULL"+
			" ORDER BY received_at LIMIT 1", account, threadID).Scan(&appID)
	if err != nil || !appID.Valid {
		return sql.NullInt64{}, false
	}
	return appID, true
}
