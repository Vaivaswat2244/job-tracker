package followup

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Vaivaswat2244/job-tracker/internal/db"
)

type note struct{ title, body, urgency string }

// seed builds one applied application whose follow-up is due on dueDay.
func seed(t *testing.T, appliedAt, dueDay string) *sql.DB {
	t.Helper()
	conn, err := db.Connect(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	if _, err := conn.Exec(
		"INSERT INTO companies (id, name, domain) VALUES (1, 'Cockroach Labs', 'cockroachlabs.com')",
	); err != nil {
		t.Fatalf("seed company: %v", err)
	}
	if _, err := conn.Exec(
		"INSERT INTO jobs (id, company_id, title, url, seen_at)"+
			" VALUES (1, 1, 'Senior Backend Engineer', 'https://x/1', ?)", db.Now(),
	); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := conn.Exec(
		"INSERT INTO applications (id, job_id, applied_at, status, followup_due)"+
			" VALUES (1, 1, ?, 'applied', ?)", appliedAt, dueDay,
	); err != nil {
		t.Fatalf("seed application: %v", err)
	}
	return conn
}

func day(t *testing.T, value string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return d
}

func runOn(t *testing.T, conn *sql.DB, when time.Time, dryRun bool) []note {
	t.Helper()
	var got []note
	if _, err := Run(conn, Options{
		Now:    when,
		DryRun: dryRun,
		Notify: func(title, body, urgency string) {
			got = append(got, note{title, body, urgency})
		},
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	return got
}

// The ladder fires once per rung and stays silent in between, which is the
// whole point: a daily timer must not renotify every morning.
func TestLadderFiresOncePerStage(t *testing.T) {
	conn := seed(t, "2026-08-05", "2026-08-12")
	due := day(t, "2026-08-12")

	for _, tc := range []struct {
		offset int
		want   int
	}{
		{-1, 0}, {0, 1}, {1, 0}, {2, 0}, {3, 1}, {4, 0}, {7, 1}, {8, 0},
	} {
		got := runOn(t, conn, due.AddDate(0, 0, tc.offset), false)
		if len(got) != tc.want {
			t.Errorf("due%+d: got %d notification(s), want %d", tc.offset, len(got), tc.want)
		}
	}

	rows, err := conn.Query("SELECT stage FROM followup_notices ORDER BY notified_at, stage")
	if err != nil {
		t.Fatalf("query notices: %v", err)
	}
	defer rows.Close()
	var stages []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		stages = append(stages, s)
	}
	want := []string{StageDue, StageNudge, StageStale}
	if len(stages) != len(want) {
		t.Fatalf("stages recorded = %v, want %v", stages, want)
	}
	for i := range want {
		if stages[i] != want[i] {
			t.Errorf("stage[%d] = %q, want %q", i, stages[i], want[i])
		}
	}
}

func TestStaleStageIsCriticalAndSuggestsGhosted(t *testing.T) {
	conn := seed(t, "2026-08-05", "2026-08-12")
	got := runOn(t, conn, day(t, "2026-08-19"), false)
	if len(got) != 1 {
		t.Fatalf("got %d notifications, want 1", len(got))
	}
	if got[0].urgency != "critical" {
		t.Errorf("urgency = %q, want critical", got[0].urgency)
	}
	if want := "consider `status 1 ghosted`"; !strings.Contains(got[0].body, want) {
		t.Errorf("body %q does not mention %q", got[0].body, want)
	}
}

// Ghosting at 30 days is the only automatic transition in the system.
func TestGhostsAtThirtyDays(t *testing.T) {
	conn := seed(t, "2026-08-05", "2026-08-12")
	got := runOn(t, conn, day(t, "2026-09-04"), false)

	if len(got) != 1 || got[0].title != "Ghosted: Cockroach Labs" {
		t.Fatalf("notifications = %+v, want one ghost notice", got)
	}
	var status string
	var due sql.NullString
	if err := conn.QueryRow(
		"SELECT status, followup_due FROM applications WHERE id = 1").Scan(&status, &due); err != nil {
		t.Fatalf("read application: %v", err)
	}
	if status != "ghosted" {
		t.Errorf("status = %q, want ghosted", status)
	}
	if due.Valid {
		t.Errorf("followup_due = %q, want NULL", due.String)
	}
}

// A reply is the signal that the application is alive, so the 30-day
// auto-ghost must not fire on it.
func TestReplyPreventsGhosting(t *testing.T) {
	conn := seed(t, "2026-08-05", "2026-08-12")
	if _, err := conn.Exec(
		"INSERT INTO contacts (id, company_id, name, title) VALUES (1, 1, 'Ana', 'EM')",
	); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	if _, err := conn.Exec(
		"INSERT INTO outreach (contact_id, application_id, status, replied_at)" +
			" VALUES (1, 1, 'sent', '2026-08-20')",
	); err != nil {
		t.Fatalf("seed outreach: %v", err)
	}

	runOn(t, conn, day(t, "2026-09-04"), false)
	var status string
	if err := conn.QueryRow(
		"SELECT status FROM applications WHERE id = 1").Scan(&status); err != nil {
		t.Fatalf("read application: %v", err)
	}
	if status != "applied" {
		t.Errorf("status = %q, want applied", status)
	}
}

func TestDryRunNotifiesButRecordsNothing(t *testing.T) {
	conn := seed(t, "2026-08-05", "2026-08-12")
	if got := runOn(t, conn, day(t, "2026-08-12"), true); len(got) != 1 {
		t.Fatalf("got %d notifications, want 1", len(got))
	}
	var notices int
	if err := conn.QueryRow(
		"SELECT count(*) FROM followup_notices").Scan(&notices); err != nil {
		t.Fatalf("count notices: %v", err)
	}
	if notices != 0 {
		t.Errorf("recorded %d notices, want 0", notices)
	}
	// And so it fires again tomorrow rather than being silently consumed.
	if got := runOn(t, conn, day(t, "2026-08-12"), true); len(got) != 1 {
		t.Errorf("second dry run fired %d, want 1", len(got))
	}
}

func TestClosedApplicationsAreNotChased(t *testing.T) {
	conn := seed(t, "2026-08-05", "2026-08-12")
	if _, err := conn.Exec(
		"UPDATE applications SET status = 'rejected' WHERE id = 1"); err != nil {
		t.Fatalf("close application: %v", err)
	}
	if got := runOn(t, conn, day(t, "2026-08-19"), false); len(got) != 0 {
		t.Errorf("got %d notifications for a rejected application, want 0", len(got))
	}
}

func TestNotificationBodyCarriesEnoughToAct(t *testing.T) {
	conn := seed(t, "2026-08-05", "2026-08-12")
	if _, err := conn.Exec(
		"INSERT INTO contacts (company_id, name, title) VALUES (1, 'Ana Reyes', 'EM')",
	); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	got := runOn(t, conn, day(t, "2026-08-12"), false)
	if len(got) != 1 {
		t.Fatalf("got %d notifications, want 1", len(got))
	}
	for _, want := range []string{
		"Senior Backend Engineer", "Ana Reyes (EM)", "7 days since applying", "app 1",
	} {
		if !strings.Contains(got[0].body, want) {
			t.Errorf("body %q missing %q", got[0].body, want)
		}
	}
}

func TestMissingContactStillActionable(t *testing.T) {
	conn := seed(t, "2026-08-05", "2026-08-12")
	got := runOn(t, conn, day(t, "2026-08-12"), false)
	if len(got) != 1 || !strings.Contains(got[0].body, "no contact yet") {
		t.Errorf("body = %q, want the no-contact hint", got[0].body)
	}
}

func TestStageFor(t *testing.T) {
	for _, tc := range []struct {
		daysOver int
		want     string
	}{
		{-1, ""}, {0, StageDue}, {2, StageDue}, {3, StageNudge},
		{6, StageNudge}, {7, StageStale}, {30, StageStale},
	} {
		if got := StageFor(tc.daysOver); got != tc.want {
			t.Errorf("StageFor(%d) = %q, want %q", tc.daysOver, got, tc.want)
		}
	}
}
