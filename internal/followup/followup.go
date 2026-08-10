// Package followup implements the escalation ladder.
//
// One notification per (application, stage), recorded in followup_notices so a
// re-run on the same day is silent. `ghosted` at 30 days is the only automatic
// status transition in the system; every other move is the user's.
package followup

import (
	"database/sql"
	"fmt"
	"io"
	"time"

	"github.com/Vaivaswat2244/job-tracker/internal/dates"
	"github.com/Vaivaswat2244/job-tracker/internal/db"
	"github.com/Vaivaswat2244/job-tracker/internal/notify"
)

// GhostAfterDays is measured from applied_at, not from followup_due: the
// question it answers is "how long has this application been silent".
const GhostAfterDays = 30

// Stages of the ladder, by how many days the follow-up is overdue.
const (
	StageDue   = "due"   // 0-2 days over
	StageNudge = "nudge" // 3-6 days over
	StageStale = "stale" // 7+ days over
)

// Notifier is injected so tests can capture notifications instead of firing them.
type Notifier func(title, body, urgency string)

// Options controls one run.
type Options struct {
	Now    time.Time
	DryRun bool
	Notify Notifier
	Stdout io.Writer
}

const openApps = `
SELECT a.id, a.status, a.applied_at, a.followup_due, j.title, c.name AS company,
       (SELECT ct.name || ' (' || ct.title || ')' FROM contacts ct
         WHERE ct.company_id = c.id ORDER BY ct.id LIMIT 1) AS contact,
       (SELECT max(o.replied_at) FROM outreach o WHERE o.application_id = a.id) AS replied_at
FROM applications a
JOIN jobs j ON j.id = a.job_id
JOIN companies c ON c.id = j.company_id
WHERE a.status IN ('applied', 'followed_up')
ORDER BY a.followup_due`

type openApp struct {
	id          int64
	status      string
	appliedAt   sql.NullString
	followupDue sql.NullString
	title       string
	company     string
	contact     sql.NullString
	repliedAt   sql.NullString
}

// StageFor maps overdue days onto a ladder rung, or "" when nothing is due.
func StageFor(daysOver int) string {
	switch {
	case daysOver >= 7:
		return StageStale
	case daysOver >= 3:
		return StageNudge
	case daysOver >= 0:
		return StageDue
	default:
		return ""
	}
}

func alreadyNotified(conn *sql.DB, appID int64, stage string) (bool, error) {
	var one int
	err := conn.QueryRow(
		"SELECT 1 FROM followup_notices WHERE application_id = ? AND stage = ?",
		appID, stage).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check notice for app %d: %w", appID, err)
	}
	return true, nil
}

func markNotified(conn *sql.DB, appID int64, stage string) error {
	_, err := conn.Exec(
		"INSERT OR IGNORE INTO followup_notices (application_id, stage, notified_at)"+
			" VALUES (?,?,?)", appID, stage, db.Now())
	if err != nil {
		return fmt.Errorf("record notice for app %d: %w", appID, err)
	}
	return nil
}

// Run walks every open application and returns how many notifications fired.
func Run(conn *sql.DB, opts Options) (int, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.Notify == nil {
		opts.Notify = notify.Send
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	today := time.Date(opts.Now.Year(), opts.Now.Month(), opts.Now.Day(), 0, 0, 0, 0, time.UTC)

	rows, err := conn.Query(openApps)
	if err != nil {
		return 0, fmt.Errorf("select open applications: %w", err)
	}
	var apps []openApp
	for rows.Next() {
		var a openApp
		if err := rows.Scan(&a.id, &a.status, &a.appliedAt, &a.followupDue,
			&a.title, &a.company, &a.contact, &a.repliedAt); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan application: %w", err)
		}
		apps = append(apps, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterate applications: %w", err)
	}
	rows.Close()

	sent := 0
	for _, a := range apps {
		elapsed := 0
		if applied := dates.ParseDay(a.appliedAt.String); !applied.IsZero() {
			elapsed = int(today.Sub(applied).Hours() / 24)
		}

		// The one permitted auto-transition.
		if elapsed >= GhostAfterDays && !a.repliedAt.Valid {
			if !opts.DryRun {
				if _, err := conn.Exec(
					"UPDATE applications SET status='ghosted', followup_due=NULL WHERE id = ?",
					a.id); err != nil {
					return sent, fmt.Errorf("mark app %d ghosted: %w", a.id, err)
				}
			}
			opts.Notify(
				fmt.Sprintf("Ghosted: %s", a.company),
				fmt.Sprintf("%s — %d days since applying, no reply. Marked ghosted (app %d).",
					a.title, elapsed, a.id),
				"normal")
			sent++
			continue
		}

		due := dates.ParseDay(a.followupDue.String)
		if due.IsZero() {
			continue
		}
		stage := StageFor(int(today.Sub(due).Hours() / 24))
		if stage == "" {
			continue
		}
		seen, err := alreadyNotified(conn, a.id, stage)
		if err != nil {
			return sent, err
		}
		if seen {
			continue
		}

		contact := a.contact.String
		if contact == "" {
			contact = "no contact yet — `contact add <company_id>`"
		}
		suffix := map[string]string{
			StageDue:   "Follow-up due today.",
			StageNudge: "Still no movement 3 days after due.",
			StageStale: fmt.Sprintf("7+ days past due — consider `status %d ghosted`.", a.id),
		}[stage]
		urgency := "normal"
		if stage == StageStale {
			urgency = "critical"
		}

		opts.Notify(
			fmt.Sprintf("Follow up: %s", a.company),
			fmt.Sprintf("%s\n%s\n%d days since applying (app %d). %s",
				a.title, contact, elapsed, a.id, suffix),
			urgency)
		if !opts.DryRun {
			if err := markNotified(conn, a.id, stage); err != nil {
				return sent, err
			}
		}
		sent++
	}

	fmt.Fprintf(opts.Stdout, "%s: %d notification(s)\n", today.Format("2006-01-02"), sent)
	return sent, nil
}
