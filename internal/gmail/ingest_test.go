package gmail

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Vaivaswat2244/job-tracker/internal/db"
)

var now = time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)

func seed(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Connect(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	for _, c := range []struct {
		id     int
		name   string
		domain string
	}{
		{1, "Stripe", "stripe.com"},
		{2, "Groww", "groww.in"},
		{3, "Stripe Capital", "stripecapital.example"}, // ambiguous prefix
	} {
		if _, err := conn.Exec(
			"INSERT INTO companies (id, name, domain) VALUES (?,?,?)",
			c.id, c.name, c.domain); err != nil {
			t.Fatalf("seed company: %v", err)
		}
	}
	for _, j := range []struct {
		id, company int
		title       string
	}{
		{10, 1, "Software Engineer, Intern"},
		{20, 2, "Backend Engineer"},
		{21, 2, "Frontend Engineer"},
	} {
		if _, err := conn.Exec(
			"INSERT INTO jobs (id, company_id, title, url, seen_at) VALUES (?,?,?,?,?)",
			j.id, j.company, j.title, fmt.Sprintf("https://x/%d", j.id),
			"2026-08-01T00:00:00+00:00"); err != nil {
			t.Fatalf("seed job: %v", err)
		}
	}
	return conn
}

func status(t *testing.T, conn *sql.DB, appID int64) string {
	t.Helper()
	var s string
	if err := conn.QueryRow("SELECT status FROM applications WHERE id = ?", appID).Scan(&s); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return s
}

func mustIngest(t *testing.T, conn *sql.DB, m Message) Action {
	t.Helper()
	a, err := Ingest(conn, m, now)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	return a
}

// The headline case: a confirmation for a company with exactly one role on file
// creates the application without being asked.
func TestConfirmationCreatesApplication(t *testing.T) {
	conn := seed(t)
	action := mustIngest(t, conn, Message{
		ID: "m1", From: "Stripe <no-reply@greenhouse.io>",
		Subject: "Thank you for applying to Stripe",
		Body:    "We have received your application.",
	})
	if action != ActionApplied {
		t.Fatalf("action = %q, want applied", action)
	}

	var jobID int64
	var st, due string
	if err := conn.QueryRow(
		"SELECT job_id, status, followup_due FROM applications").Scan(&jobID, &st, &due); err != nil {
		t.Fatalf("read application: %v", err)
	}
	if jobID != 10 || st != "applied" {
		t.Errorf("application = job %d, status %q; want job 10, applied", jobID, st)
	}
	// +5 business days from Tuesday 2026-08-11 is Tuesday 2026-08-18.
	if due != "2026-08-18" {
		t.Errorf("followup_due = %q, want 2026-08-18", due)
	}
}

// Two roles at one company: the ingest cannot know which was applied to, and a
// guess would put the follow-up ladder on the wrong role.
func TestAmbiguousCompanyIsQueuedNotGuessed(t *testing.T) {
	conn := seed(t)
	action := mustIngest(t, conn, Message{
		ID: "m2", From: "Groww <no-reply@hire.lever.co>",
		Subject: "Thank you for applying to Groww",
		Body:    "Application received.",
	})
	if action != ActionQueued {
		t.Fatalf("action = %q, want queued", action)
	}
	var n int
	if err := conn.QueryRow("SELECT count(*) FROM applications").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d application(s) created from an ambiguous message; want 0", n)
	}

	pending, err := PendingList(conn)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 || pending[0].GmailID != "m2" {
		t.Fatalf("pending = %+v, want the queued message", pending)
	}
	if !pending[0].Reason.Valid || pending[0].Reason.String == "" {
		t.Error("queued message carries no reason for the user to act on")
	}
}

// Resolving the queue does what the ingest would have done, and the message
// never comes back.
func TestResolveAppliesToTheNamedJob(t *testing.T) {
	conn := seed(t)
	mustIngest(t, conn, Message{
		ID: "m2", From: "Groww <no-reply@hire.lever.co>",
		Subject: "Thank you for applying to Groww", Body: "Application received.",
	})

	action, err := Resolve(conn, "m2", 21, now)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if action != ActionApplied {
		t.Errorf("action = %q, want applied", action)
	}
	var jobID int64
	if err := conn.QueryRow("SELECT job_id FROM applications").Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if jobID != 21 {
		t.Errorf("application on job %d, want 21", jobID)
	}
	pending, _ := PendingList(conn)
	if len(pending) != 0 {
		t.Errorf("%d message(s) still queued after resolution", len(pending))
	}
}

func TestDismissLeavesNoApplication(t *testing.T) {
	conn := seed(t)
	mustIngest(t, conn, Message{
		ID: "m2", From: "Groww <no-reply@hire.lever.co>",
		Subject: "Thank you for applying to Groww", Body: "Application received.",
	})
	if _, err := Resolve(conn, "m2", 0, now); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	var n int
	conn.QueryRow("SELECT count(*) FROM applications").Scan(&n)
	if n != 0 {
		t.Errorf("dismiss created %d application(s)", n)
	}
	if pending, _ := PendingList(conn); len(pending) != 0 {
		t.Error("dismissed message is still queued")
	}
}

func TestRejectionClosesTheApplication(t *testing.T) {
	conn := seed(t)
	mustIngest(t, conn, Message{
		ID: "m1", From: "no-reply@greenhouse.io",
		Subject: "Thank you for applying to Stripe", Body: "Received.",
	})

	action := mustIngest(t, conn, Message{
		ID: "m3", From: "no-reply@greenhouse.io",
		Subject: "Update on your application to Stripe",
		Body:    "We have decided to move forward with other candidates.",
	})
	if action != ActionRejected {
		t.Fatalf("action = %q, want rejected", action)
	}
	if got := status(t, conn, 1); got != "rejected" {
		t.Errorf("status = %q, want rejected", got)
	}
	var due sql.NullString
	conn.QueryRow("SELECT followup_due FROM applications WHERE id = 1").Scan(&due)
	if due.Valid {
		t.Errorf("followup_due = %q; a rejected application must not stay on the ladder", due.String)
	}
}

// A rejection for a company with nothing tracked has nothing to close, and
// inventing an application to reject would be worse than asking.
func TestRejectionWithNoApplicationIsQueued(t *testing.T) {
	conn := seed(t)
	action := mustIngest(t, conn, Message{
		ID: "m4", From: "no-reply@greenhouse.io",
		Subject: "Update on your application to Stripe",
		Body:    "We regret to inform you that we will not be progressing.",
	})
	if action != ActionQueued {
		t.Errorf("action = %q, want queued", action)
	}
}

// Re-polling the same window must not act twice — the second sighting of a
// rejection should not, for instance, re-close a reopened application.
func TestIngestIsIdempotent(t *testing.T) {
	conn := seed(t)
	msg := Message{
		ID: "m1", From: "no-reply@greenhouse.io",
		Subject: "Thank you for applying to Stripe", Body: "Received.",
	}
	mustIngest(t, conn, msg)

	_, err := Ingest(conn, msg, now)
	if !AlreadySeen(err) {
		t.Fatalf("second ingest returned %v, want the already-seen sentinel", err)
	}
	var n int
	conn.QueryRow("SELECT count(*) FROM applications").Scan(&n)
	if n != 1 {
		t.Errorf("%d applications after re-ingesting one message, want 1", n)
	}
}

// A confirmation arriving after an interview was logged must not drag the
// status backwards to "applied".
func TestConfirmationDoesNotRewindStatus(t *testing.T) {
	conn := seed(t)
	mustIngest(t, conn, Message{
		ID: "m1", From: "no-reply@greenhouse.io",
		Subject: "Thank you for applying to Stripe", Body: "Received.",
	})
	if _, err := conn.Exec("UPDATE applications SET status='in_process' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	mustIngest(t, conn, Message{
		ID: "m5", From: "no-reply@greenhouse.io",
		Subject: "Your application to Stripe", Body: "We have received your application.",
	})
	if got := status(t, conn, 1); got != "in_process" {
		t.Errorf("status = %q; a late confirmation rewound a live application", got)
	}
}

// A human reply on a thread the tracker knows must stop the ladder — this is
// the field nothing in the codebase wrote before.
func TestHumanReplyStopsTheLadder(t *testing.T) {
	conn := seed(t)
	mustIngest(t, conn, Message{
		ID: "m1", ThreadID: "t1", From: "no-reply@greenhouse.io",
		Subject: "Thank you for applying to Stripe", Body: "Received.",
	})
	if _, err := conn.Exec(
		"INSERT INTO contacts (id, company_id, name, title) VALUES (1,1,'Priya','EM')"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(
		"INSERT INTO outreach (id, contact_id, application_id, status) VALUES (1,1,1,'draft')"); err != nil {
		t.Fatal(err)
	}

	action := mustIngest(t, conn, Message{
		ID: "m6", ThreadID: "t1", From: "Priya <priya@stripe.com>",
		Subject: "Re: your application", Body: "Can you do a call Thursday?",
	})
	if action != ActionReplied {
		t.Fatalf("action = %q, want replied", action)
	}
	var replied sql.NullString
	conn.QueryRow("SELECT replied_at FROM outreach WHERE id = 1").Scan(&replied)
	if !replied.Valid {
		t.Error("outreach.replied_at still null; the ladder would keep nagging")
	}
}

// A reply with no outreach row still matters: record it on the application
// rather than losing the signal because the tracker did not draft the thread.
func TestReplyWithoutOutreachAdvancesTheApplication(t *testing.T) {
	conn := seed(t)
	mustIngest(t, conn, Message{
		ID: "m1", ThreadID: "t1", From: "no-reply@greenhouse.io",
		Subject: "Thank you for applying to Stripe", Body: "Received.",
	})
	mustIngest(t, conn, Message{
		ID: "m7", ThreadID: "t1", From: "Priya <priya@stripe.com>",
		Subject: "Re: your application", Body: "Are you free Thursday?",
	})
	if got := status(t, conn, 1); got != "in_process" {
		t.Errorf("status = %q, want in_process", got)
	}
}

// Mail from the employer's own domain should resolve without needing the
// subject line to name them.
func TestSenderDomainResolvesCompany(t *testing.T) {
	conn := seed(t)
	action := mustIngest(t, conn, Message{
		ID: "m8", From: "careers@stripe.com",
		Subject: "Application received", Body: "Thanks for applying.",
	})
	if action != ActionApplied {
		t.Errorf("action = %q, want applied via the sender domain", action)
	}
}

// "Stripe" prefix-matches "Stripe Capital" too; an exact name match has to win
// or every Stripe mail becomes ambiguous.
func TestExactNameBeatsPrefixCollision(t *testing.T) {
	conn := seed(t)
	id, guess, reason := ResolveCompany(conn, Message{
		From: "no-reply@greenhouse.io", Subject: "Thank you for applying to Stripe",
	})
	if reason != "" {
		t.Fatalf("reason = %q, want a confident match", reason)
	}
	if guess != "Stripe" || !id.Valid || id.Int64 != 1 {
		t.Errorf("resolved to %+v (guess %q), want company 1", id, guess)
	}
}

// An unknown employer is recorded, not dropped: INV-1 applies to mail too.
func TestUnknownCompanyIsRecordedForReview(t *testing.T) {
	conn := seed(t)
	action := mustIngest(t, conn, Message{
		ID: "m9", From: "no-reply@greenhouse.io",
		Subject: "Thank you for applying to Wingify", Body: "Received.",
	})
	if action != ActionQueued {
		t.Fatalf("action = %q, want queued", action)
	}
	var guess string
	if err := conn.QueryRow(
		"SELECT company_guess FROM mail_messages WHERE gmail_id='m9'").Scan(&guess); err != nil {
		t.Fatalf("read message: %v", err)
	}
	if guess != "Wingify" {
		t.Errorf("company_guess = %q, want Wingify preserved for the review queue", guess)
	}
}

// Ordinary mail is recorded and ignored, so a later poll does not re-examine it.
func TestUnrelatedMailIsRecordedAndIgnored(t *testing.T) {
	conn := seed(t)
	action := mustIngest(t, conn, Message{
		ID: "m10", From: "news@techcrunch.com",
		Subject: "Your daily briefing", Body: "Top stories.",
	})
	if action != ActionNone {
		t.Errorf("action = %q, want none", action)
	}
	var kind string
	conn.QueryRow("SELECT kind FROM mail_messages WHERE gmail_id='m10'").Scan(&kind)
	if kind != string(Other) {
		t.Errorf("kind = %q, want other", kind)
	}
	if pending, _ := PendingList(conn); len(pending) != 0 {
		t.Error("unrelated mail landed in the review queue")
	}
}
