// Package health does feed-death detection.
//
// A wrong slug returns 404 forever. A renamed board returns [] forever. Both
// look exactly like "this company isn't hiring", and both silently remove a
// company from the pipeline — the precise failure INV-1 exists to prevent. So
// every poll is recorded, and a feed that stops producing shouts.
//
// Shared by the ATS watchlist (Task A) and the funding sources (Task B): the
// failure mode is identical, so the detector is too.
package health

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/Vaivaswat2244/job-tracker/internal/db"
	"github.com/Vaivaswat2244/job-tracker/internal/notify"
)

const (
	FailStreak  = 3 // consecutive non-2xx/non-304 polls
	EmptyStreak = 3 // consecutive 0-item polls after the feed had produced items
	StaleFeed   = "stale_feed"
)

// PollRow is one poll_log row. HTTPStatus and ItemCount are nullable and the
// null is meaningful: a null item count means "the request never delivered a
// list", which is different from a list of length zero.
type PollRow struct {
	OK         bool
	HTTPStatus sql.NullInt64
	ItemCount  sql.NullInt64
	Error      string
}

// Poll captures one poll outcome for recording.
type Poll struct {
	HTTPStatus sql.NullInt64
	ItemCount  sql.NullInt64
	OK         bool
	Error      string
	Meta       string
}

// Int wraps a present integer for the nullable fields above.
func Int(v int) sql.NullInt64 { return sql.NullInt64{Int64: int64(v), Valid: true} }

// Null is the absent value, for a request that never completed.
var Null = sql.NullInt64{}

func RecordPoll(conn *sql.DB, targetType, targetID string, p Poll) error {
	okFlag := 0
	if p.OK {
		okFlag = 1
	}
	_, err := conn.Exec(
		"INSERT INTO poll_log (target_type, target_id, polled_at, http_status, item_count,"+
			" ok, error, meta) VALUES (?,?,?,?,?,?,?,?)",
		targetType, targetID, db.Now(), nullable(p.HTTPStatus), nullable(p.ItemCount),
		okFlag, nullString(p.Error), nullString(p.Meta),
	)
	if err != nil {
		return fmt.Errorf("record poll for %s/%s: %w", targetType, targetID, err)
	}
	return nil
}

func nullable(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func recent(conn *sql.DB, targetType, targetID string, limit int) ([]PollRow, error) {
	rows, err := conn.Query(
		"SELECT ok, http_status, item_count, error FROM poll_log"+
			" WHERE target_type = ? AND target_id = ? ORDER BY id DESC LIMIT ?",
		targetType, targetID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("read poll history for %s/%s: %w", targetType, targetID, err)
	}
	defer rows.Close()

	var out []PollRow
	for rows.Next() {
		var (
			okFlag int
			r      PollRow
			errMsg sql.NullString
		)
		if err := rows.Scan(&okFlag, &r.HTTPStatus, &r.ItemCount, &errMsg); err != nil {
			return nil, fmt.Errorf("scan poll row: %w", err)
		}
		r.OK = okFlag != 0
		r.Error = errMsg.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// Diagnose returns (reason, detail) for the most recent polls, newest first.
// Pure function so the streak arithmetic is testable without a database.
func Diagnose(rows []PollRow) (string, string) {
	fails := 0
	for _, row := range rows {
		if row.OK {
			break
		}
		fails++
	}
	if fails >= FailStreak {
		last := rows[0]
		status := "no response"
		if last.HTTPStatus.Valid {
			status = fmt.Sprintf("%d", last.HTTPStatus.Int64)
		}
		detail := fmt.Sprintf("%d consecutive failed polls (latest: %s", fails, status)
		if last.Error != "" {
			detail += "; " + last.Error
		}
		return "failing", detail + ")"
	}

	// 304 means "unchanged", which is neither a zero nor a non-zero result: it
	// neither extends nor breaks the empty streak.
	empties := 0
	producedBefore := false
	for _, row := range rows {
		if !row.OK {
			break
		}
		if row.HTTPStatus.Valid && row.HTTPStatus.Int64 == 304 {
			continue
		}
		if row.ItemCount.Valid && row.ItemCount.Int64 == 0 {
			empties++
			continue
		}
		producedBefore = true
		break
	}
	if empties >= EmptyStreak && producedBefore {
		return "empty", fmt.Sprintf(
			"%d consecutive polls returned 0 items after previously returning results"+
				" — slug or board URL has probably changed", empties)
	}
	return "", ""
}

// RaiseAlert opens an alert if one is not already open, reporting whether it was
// newly raised.
func RaiseAlert(conn *sql.DB, targetType, targetID, label, message, detail, kind string) (bool, error) {
	if kind == "" {
		kind = StaleFeed
	}
	res, err := conn.Exec(
		"INSERT OR IGNORE INTO alerts (kind, target_type, target_id, message, detail, raised_at)"+
			" VALUES (?,?,?,?,?,?)",
		kind, targetType, targetID, message, nullString(detail), db.Now(),
	)
	if err != nil {
		return false, fmt.Errorf("raise alert for %s/%s: %w", targetType, targetID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil || affected == 0 {
		return false, nil
	}

	notify.Send("Feed stale: "+label, strings.TrimSpace(message+"\n"+detail), "critical")
	if _, err := conn.Exec(
		"UPDATE alerts SET notified_at = ? WHERE kind = ? AND target_type = ?"+
			" AND target_id = ? AND resolved_at IS NULL",
		db.Now(), kind, targetType, targetID,
	); err != nil {
		return true, fmt.Errorf("mark alert notified: %w", err)
	}
	return true, nil
}

func ResolveAlert(conn *sql.DB, targetType, targetID, kind string) error {
	if kind == "" {
		kind = StaleFeed
	}
	_, err := conn.Exec(
		"UPDATE alerts SET resolved_at = ? WHERE kind = ? AND target_type = ?"+
			" AND target_id = ? AND resolved_at IS NULL",
		db.Now(), kind, targetType, targetID,
	)
	if err != nil {
		return fmt.Errorf("resolve alert for %s/%s: %w", targetType, targetID, err)
	}
	return nil
}

// Check evaluates the recorded history and raises or clears the stale_feed alert.
//
// The target is never disabled. An alerting company keeps its watchlist entry
// and keeps being polled — auto-disabling is how a company disappears quietly.
func Check(conn *sql.DB, targetType, targetID, label string) (string, error) {
	rows, err := recent(conn, targetType, targetID, 30)
	if err != nil {
		return "", err
	}
	reason, detail := Diagnose(rows)
	if reason == "" {
		return "", ResolveAlert(conn, targetType, targetID, StaleFeed)
	}

	message := label + ": board went empty"
	if reason == "failing" {
		message = label + ": board is returning errors"
	}
	if _, err := RaiseAlert(conn, targetType, targetID, label, message, detail, StaleFeed); err != nil {
		return reason, err
	}
	return reason, nil
}

// Alert is one open operational alert.
type Alert struct {
	ID         int64
	Kind       string
	TargetType string
	TargetID   string
	Message    string
	Detail     string
	RaisedAt   string
}

func OpenAlerts(conn *sql.DB) ([]Alert, error) {
	rows, err := conn.Query(
		"SELECT id, kind, target_type, target_id, message, detail, raised_at" +
			" FROM alerts WHERE resolved_at IS NULL ORDER BY raised_at")
	if err != nil {
		return nil, fmt.Errorf("read open alerts: %w", err)
	}
	defer rows.Close()

	var out []Alert
	for rows.Next() {
		var (
			a      Alert
			detail sql.NullString
		)
		if err := rows.Scan(&a.ID, &a.Kind, &a.TargetType, &a.TargetID,
			&a.Message, &detail, &a.RaisedAt); err != nil {
			return nil, fmt.Errorf("scan alert: %w", err)
		}
		a.Detail = detail.String
		out = append(out, a)
	}
	return out, rows.Err()
}

// PollOutcome is the transport summary of one company poll. It mirrors the
// fields of an ats.FetchResult without importing it, so health stays usable by
// anything that polls.
//
// JobsPresent distinguishes "no list came back" from "the list was empty" —
// the whole basis of the empty-streak check.
type PollOutcome struct {
	JobsPresent bool
	JobCount    int
	NotModified bool
	Status      int
	OK          bool
	Error       string
}

// PollHealth records one company poll and evaluates feed health.
func PollHealth(conn *sql.DB, companyID int64, outcome PollOutcome, label string) (string, error) {
	target := fmt.Sprintf("%d", companyID)

	itemCount := Null
	if outcome.JobsPresent && !outcome.NotModified {
		itemCount = Int(outcome.JobCount)
	}
	status := Null
	if outcome.Status != 0 {
		status = Int(outcome.Status)
	}

	if err := RecordPoll(conn, "company", target, Poll{
		HTTPStatus: status,
		ItemCount:  itemCount,
		OK:         outcome.OK || outcome.NotModified,
		Error:      outcome.Error,
	}); err != nil {
		return "", err
	}

	if label == "" {
		var name sql.NullString
		err := conn.QueryRow("SELECT name FROM companies WHERE id = ?", companyID).Scan(&name)
		if err == nil && name.Valid {
			label = name.String
		} else {
			label = "company " + target
		}
	}
	return Check(conn, "company", target, label)
}
