package gmail

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/Vaivaswat2244/job-tracker/internal/dates"
	"github.com/Vaivaswat2244/job-tracker/internal/db"
)

// Pending is one queued message awaiting a decision.
type Pending struct {
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
		"SELECT gmail_id, subject, from_addr, received_at, kind, company_guess, reason" +
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
		if err := rows.Scan(&p.GmailID, &p.Subject, &p.From, &p.ReceivedAt,
			&kind, &p.CompanyGuess, &p.Reason); err != nil {
			return nil, fmt.Errorf("scan pending mail: %w", err)
		}
		p.Kind = Kind(kind)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Resolve applies a queued message to a job the user names, doing what the
// ingest would have done had it been sure.
//
// Passing jobID = 0 dismisses the message instead: it stays on file with a
// decided_at, so it never reappears and the record of the decision survives.
func Resolve(conn *sql.DB, gmailID string, jobID int64, now time.Time) (Action, error) {
	var kind, action string
	var appID sql.NullInt64
	err := conn.QueryRow(
		"SELECT kind, action, application_id FROM mail_messages"+
			" WHERE gmail_id = ? AND needs_review = 1 AND decided_at IS NULL",
		gmailID).Scan(&kind, &action, &appID)
	if err == sql.ErrNoRows {
		return ActionNone, fmt.Errorf("no message queued for review with id %s", gmailID)
	}
	if err != nil {
		return ActionNone, fmt.Errorf("look up queued message: %w", err)
	}

	stamp := now.UTC().Format(db.ISO8601)
	if jobID == 0 {
		if err := decide(conn, gmailID, ActionNone, sql.NullInt64{}, sql.NullInt64{}, stamp); err != nil {
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

	if err := decide(conn, gmailID, resolved, appID,
		sql.NullInt64{Int64: jobID, Valid: true}, stamp); err != nil {
		return ActionNone, err
	}
	return resolved, nil
}

func decide(conn *sql.DB, gmailID string, action Action, appID, jobID sql.NullInt64, stamp string) error {
	_, err := conn.Exec(
		"UPDATE mail_messages SET action = ?, application_id = COALESCE(?, application_id),"+
			" job_id = COALESCE(?, job_id), needs_review = 0, decided_at = ?"+
			" WHERE gmail_id = ?",
		string(action), nullInt(appID), nullInt(jobID), stamp, gmailID)
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
