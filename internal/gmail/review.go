package gmail

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Vaivaswat2244/job-tracker/internal/dates"
	"github.com/Vaivaswat2244/job-tracker/internal/db"
)

// Pending is one queued message awaiting a decision.
type Pending struct {
	Account      string
	AccountEmail sql.NullString
	GmailID      string
	Subject      string
	From         string
	ReceivedAt   string
	Kind         Kind
	CompanyGuess sql.NullString
	Reason       sql.NullString
}

// PendingList returns everything queued, oldest first, because the oldest is
// the one whose application is closest to being wrongly ghosted.
func PendingList(conn *sql.DB) ([]Pending, error) {
	rows, err := conn.Query(
		"SELECT account, account_email, gmail_id, subject, from_addr, received_at," +
			" kind, company_guess, reason" +
			" FROM mail_messages WHERE needs_review = 1 AND decided_at IS NULL" +
			" ORDER BY received_at")
	if err != nil {
		return nil, fmt.Errorf("list pending mail: %w", err)
	}
	defer rows.Close()

	var out []Pending
	for rows.Next() {
		var p Pending
		var kind string
		if err := rows.Scan(&p.Account, &p.AccountEmail, &p.GmailID, &p.Subject,
			&p.From, &p.ReceivedAt, &kind, &p.CompanyGuess, &p.Reason); err != nil {
			return nil, fmt.Errorf("scan pending mail: %w", err)
		}
		p.Kind = Kind(kind)
		out = append(out, p)
	}
	return out, rows.Err()
}

// RecordNew creates the company and job a queued message refers to, then
// applies it. Most application mail is from companies the watchlist does not
// track — 26 of the first 86 queued messages — and requiring an existing job id
// left those impossible to record at all. The application is the thing worth
// keeping; the job row exists to hang it on.
func RecordNew(conn *sql.DB, gmailID, company, title string, now time.Time) (Action, error) {
	if strings.TrimSpace(company) == "" {
		return ActionNone, fmt.Errorf("a company name is required")
	}
	if strings.TrimSpace(title) == "" {
		title = "(role not stated in the email)"
	}

	var companyID int64
	err := conn.QueryRow("SELECT id FROM companies WHERE lower(name) = ?",
		strings.ToLower(company)).Scan(&companyID)
	if err == sql.ErrNoRows {
		res, err := conn.Exec(
			"INSERT INTO companies (name, discovery_source) VALUES (?, 'mail')", company)
		if err != nil {
			return ActionNone, fmt.Errorf("create company %q: %w", company, err)
		}
		if companyID, err = res.LastInsertId(); err != nil {
			return ActionNone, err
		}
	} else if err != nil {
		return ActionNone, fmt.Errorf("look up company: %w", err)
	}

	stamp := now.UTC().Format(db.ISO8601)
	res, err := conn.Exec(
		"INSERT INTO jobs (company_id, title, url, source, seen_at, first_seen_at)"+
			" VALUES (?,?,?, 'mail', ?, ?)",
		companyID, title, "mail:"+gmailID, stamp, stamp)
	if err != nil {
		return ActionNone, fmt.Errorf("create job: %w", err)
	}
	jobID, err := res.LastInsertId()
	if err != nil {
		return ActionNone, err
	}
	return Resolve(conn, gmailID, jobID, now)
}

// Resolve applies a queued message to a job the user names, doing what the
// ingest would have done had it been sure.
//
// Passing jobID = 0 dismisses the message instead: it stays on file with a
// decided_at, so it never reappears and the record of the decision survives.
func Resolve(conn *sql.DB, gmailID string, jobID int64, now time.Time) (Action, error) {
	var account, kind, action string
	var appID sql.NullInt64
	err := conn.QueryRow(
		"SELECT account, kind, action, application_id FROM mail_messages"+
			" WHERE gmail_id = ? AND needs_review = 1 AND decided_at IS NULL",
		gmailID).Scan(&account, &kind, &action, &appID)
	if err == sql.ErrNoRows {
		return ActionNone, fmt.Errorf("no message queued for review with id %s", gmailID)
	}
	if err != nil {
		return ActionNone, fmt.Errorf("look up queued message: %w", err)
	}

	stamp := now.UTC().Format(db.ISO8601)
	if jobID == 0 {
		if err := decide(conn, account, gmailID, ActionNone, sql.NullInt64{}, sql.NullInt64{}, stamp); err != nil {
			return ActionNone, err
		}
		return ActionNone, nil
	}

	var companyID int64
	if err := conn.QueryRow("SELECT company_id FROM jobs WHERE id = ?", jobID).Scan(&companyID); err != nil {
		if err == sql.ErrNoRows {
			return ActionNone, fmt.Errorf("no job with id %d", jobID)
		}
		return ActionNone, fmt.Errorf("look up job: %w", err)
	}

	var resolved Action
	switch Kind(kind) {
	case Confirmation:
		id, err := applicationFor(conn, jobID, now)
		if err != nil {
			return ActionNone, err
		}
		appID, resolved = id, ActionApplied
	case Rejection:
		id, err := existingApplication(conn, jobID)
		if err != nil {
			return ActionNone, err
		}
		if err := setStatus(conn, id.Int64, "rejected", stamp); err != nil {
			return ActionNone, err
		}
		appID, resolved = id, ActionRejected
	default:
		return ActionNone, fmt.Errorf("message %s is a %s; nothing to apply", gmailID, kind)
	}

	if err := decide(conn, account, gmailID, resolved, appID,
		sql.NullInt64{Int64: jobID, Valid: true}, stamp); err != nil {
		return ActionNone, err
	}
	return resolved, nil
}

// decide scopes the update by account as well as id. Gmail ids are unique per
// mailbox, so with two accounts connected an id-only UPDATE could in principle
// resolve a queued message in the other mailbox at the same time.
func decide(conn *sql.DB, account, gmailID string, action Action,
	appID, jobID sql.NullInt64, stamp string) error {

	_, err := conn.Exec(
		"UPDATE mail_messages SET action = ?, application_id = COALESCE(?, application_id),"+
			" job_id = COALESCE(?, job_id), needs_review = 0, decided_at = ?"+
			" WHERE account = ? AND gmail_id = ?",
		string(action), nullInt(appID), nullInt(jobID), stamp, account, gmailID)
	if err != nil {
		return fmt.Errorf("record decision: %w", err)
	}
	return nil
}

// applicationFor returns the application for a job, creating it if this is the
// first the tracker hears of it.
func applicationFor(conn *sql.DB, jobID int64, now time.Time) (sql.NullInt64, error) {
	var id int64
	err := conn.QueryRow("SELECT id FROM applications WHERE job_id = ?", jobID).Scan(&id)
	if err == nil {
		return sql.NullInt64{Int64: id, Valid: true}, nil
	}
	if err != sql.ErrNoRows {
		return sql.NullInt64{}, fmt.Errorf("look up application: %w", err)
	}

	applied := now.Format("2006-01-02")
	due := dates.AddBusinessDays(now, dates.FollowupBusinessDays).Format("2006-01-02")
	res, err := conn.Exec(
		"INSERT INTO applications (job_id, applied_at, status, followup_due, notes)"+
			" VALUES (?,?,'applied',?,?)",
		jobID, applied, due, "confirmed from email during review")
	if err != nil {
		return sql.NullInt64{}, fmt.Errorf("insert application: %w", err)
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return sql.NullInt64{}, fmt.Errorf("application id: %w", err)
	}
	return sql.NullInt64{Int64: newID, Valid: true}, nil
}

func existingApplication(conn *sql.DB, jobID int64) (sql.NullInt64, error) {
	var id int64
	err := conn.QueryRow("SELECT id FROM applications WHERE job_id = ?", jobID).Scan(&id)
	if err == sql.ErrNoRows {
		return sql.NullInt64{}, fmt.Errorf(
			"job %d has no application to reject — `tracker apply %d` first if you did apply",
			jobID, jobID)
	}
	if err != nil {
		return sql.NullInt64{}, fmt.Errorf("look up application: %w", err)
	}
	return sql.NullInt64{Int64: id, Valid: true}, nil
}
