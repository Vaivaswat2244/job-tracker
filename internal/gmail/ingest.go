package gmail

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/Vaivaswat2244/job-tracker/internal/dates"
	"github.com/Vaivaswat2244/job-tracker/internal/db"
)

// Action is what the ingest did about a message.
type Action string

const (
	ActionApplied  Action = "applied"  // created or advanced an application
	ActionRejected Action = "rejected" // closed one
	ActionReplied  Action = "replied"  // stopped the follow-up ladder
	ActionQueued   Action = "queued"   // needs a human decision
	ActionNone     Action = "none"     // classified, nothing to do
)

// Result summarises one ingest run.
type Result struct {
	Scanned  int
	Applied  int
	Rejected int
	Replied  int
	Queued   int
	Skipped  int // already processed
}

// Ingest classifies a message, applies what it can, and records the outcome.
// It is idempotent on gmail_id: a message already in mail_messages is skipped,
// so re-polling the same window costs nothing and can never double-act.
func Ingest(conn *sql.DB, m Message, now time.Time) (Action, error) {
	var seen string
	err := conn.QueryRow("SELECT seen_at FROM mail_messages WHERE gmail_id = ?", m.ID).Scan(&seen)
	if err == nil {
		return ActionNone, errAlreadySeen
	}
	if err != sql.ErrNoRows {
		return ActionNone, fmt.Errorf("check message: %w", err)
	}

	kind := Classify(m)
	record := mailRecord{
		msg:  m,
		kind: kind,
		now:  now.UTC().Format(db.ISO8601),
	}

	switch kind {
	case Other:
		record.action = ActionNone
		return record.action, record.save(conn)

	case Reply:
		// A reply only matters if it answers a thread we know about; anything
		// else is ordinary mail that happens to start with "Re:".
		appID, ok := ResolveThread(conn, m.ThreadID)
		if !ok {
			record.action = ActionNone
			record.reason = "reply on an unrecognised thread"
			return record.action, record.save(conn)
		}
		record.applicationID = appID
		if err := markReplied(conn, appID.Int64, record.now); err != nil {
			return ActionNone, err
		}
		record.action = ActionReplied
		return record.action, record.save(conn)
	}

	companyID, guess, reason := ResolveCompany(conn, m)
	record.companyGuess = guess
	if reason != "" {
		record.action = ActionQueued
		record.needsReview = true
		record.reason = reason
		return record.action, record.save(conn)
	}
	record.companyID = companyID

	match := ResolveApplication(conn, companyID.Int64, kind)
	record.jobID, record.applicationID = match.JobID, match.ApplicationID
	if !match.Confident() {
		record.action = ActionQueued
		record.needsReview = true
		record.reason = match.Reason
		return record.action, record.save(conn)
	}

	switch kind {
	case Confirmation:
		appID, err := ensureApplied(conn, match, now)
		if err != nil {
			return ActionNone, err
		}
		record.applicationID = appID
		record.action = ActionApplied
	case Rejection:
		if !match.ApplicationID.Valid {
			record.action = ActionQueued
			record.needsReview = true
			record.reason = "rejection with no application to close"
			break
		}
		if err := setStatus(conn, match.ApplicationID.Int64, "rejected", record.now); err != nil {
			return ActionNone, err
		}
		record.action = ActionRejected
	}
	return record.action, record.save(conn)
}

var errAlreadySeen = fmt.Errorf("message already processed")

// AlreadySeen reports whether Ingest skipped a message it had handled before.
func AlreadySeen(err error) bool { return err == errAlreadySeen }

// ensureApplied creates the application if the confirmation is the first the
// tracker hears of it, or leaves an existing one alone. It never moves an
// application backwards: a confirmation arriving after an interview was logged
// must not reset the status to "applied".
func ensureApplied(conn *sql.DB, match Match, now time.Time) (sql.NullInt64, error) {
	if match.ApplicationID.Valid {
		return match.ApplicationID, nil
	}
	if !match.JobID.Valid {
		return sql.NullInt64{}, fmt.Errorf("confirmation matched no job")
	}

	appliedAt := now.Format("2006-01-02")
	due := dates.AddBusinessDays(now, dates.FollowupBusinessDays).Format("2006-01-02")
	res, err := conn.Exec(
		"INSERT INTO applications (job_id, applied_at, status, followup_due, notes)"+
			" VALUES (?,?,'applied',?,?)",
		match.JobID.Int64, appliedAt, due, "created from the application confirmation email")
	if err != nil {
		return sql.NullInt64{}, fmt.Errorf("insert application: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return sql.NullInt64{}, fmt.Errorf("application id: %w", err)
	}
	return sql.NullInt64{Int64: id, Valid: true}, nil
}

func setStatus(conn *sql.DB, appID int64, status, now string) error {
	if _, err := conn.Exec(
		"UPDATE applications SET status = ?, followup_due = NULL WHERE id = ?",
		status, appID); err != nil {
		return fmt.Errorf("set status %s: %w", status, err)
	}
	return nil
}

// markReplied stops the ladder. The follow-up code reads outreach.replied_at,
// so a reply has to land there — until now nothing in the codebase wrote it.
func markReplied(conn *sql.DB, appID int64, now string) error {
	res, err := conn.Exec(
		"UPDATE outreach SET replied_at = COALESCE(replied_at, ?) WHERE application_id = ?",
		now, appID)
	if err != nil {
		return fmt.Errorf("mark replied: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// No outreach row: record the reply on the application instead, so the
		// signal is not lost just because the tracker did not draft the thread.
		if _, err := conn.Exec(
			"UPDATE applications SET status = 'in_process', followup_due = NULL"+
				" WHERE id = ? AND status IN ('applied','followed_up')", appID); err != nil {
			return fmt.Errorf("mark in process: %w", err)
		}
	}
	return nil
}

// mailRecord is the row written for every message, acted on or not.
type mailRecord struct {
	msg           Message
	kind          Kind
	action        Action
	companyID     sql.NullInt64
	jobID         sql.NullInt64
	applicationID sql.NullInt64
	companyGuess  string
	reason        string
	needsReview   bool
	now           string
}

func (r mailRecord) save(conn *sql.DB) error {
	review := 0
	if r.needsReview {
		review = 1
	}
	_, err := conn.Exec(
		"INSERT INTO mail_messages (gmail_id, thread_id, from_addr, from_domain, subject,"+
			" received_at, kind, company_id, job_id, application_id, action, needs_review,"+
			" reason, company_guess, seen_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		r.msg.ID, r.msg.ThreadID, Address(r.msg.From), Domain(r.msg.From), r.msg.Subject,
		r.msg.ReceivedAt, string(r.kind), nullInt(r.companyID), nullInt(r.jobID),
		nullInt(r.applicationID), string(r.action), review,
		nullStr(r.reason), nullStr(r.companyGuess), r.now)
	if err != nil {
		return fmt.Errorf("record message: %w", err)
	}
	return nil
}

func nullInt(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
