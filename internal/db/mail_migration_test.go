package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// The shape mail_messages shipped with before multi-account support.
const oldMailTable = `
CREATE TABLE mail_messages (
    gmail_id TEXT PRIMARY KEY,
    thread_id TEXT,
    from_addr TEXT,
    from_domain TEXT,
    subject TEXT,
    received_at TEXT,
    kind TEXT NOT NULL,
    company_id INTEGER,
    job_id INTEGER,
    application_id INTEGER,
    action TEXT NOT NULL,
    needs_review INTEGER NOT NULL DEFAULT 0,
    reason TEXT,
    company_guess TEXT,
    decided_at TEXT,
    seen_at TEXT NOT NULL
);`

// A database written by the single-account version must come forward without
// losing rows, and must end up keyed so two mailboxes cannot collide.
func TestMailMigrationPreservesRowsAndRekeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := raw.Exec(oldMailTable); err != nil {
		t.Fatalf("create old table: %v", err)
	}
	if _, err := raw.Exec(
		"INSERT INTO mail_messages (gmail_id, subject, kind, action, needs_review, seen_at)"+
			" VALUES (?,?,?,?,?,?)",
		"abc", "Thank you for applying to Stripe", "confirmation", "queued", 1,
		"2026-08-11T09:00:00+00:00"); err != nil {
		t.Fatalf("seed old row: %v", err)
	}
	raw.Close()

	conn, err := Connect(path)
	if err != nil {
		t.Fatalf("connect (which migrates): %v", err)
	}
	defer conn.Close()

	var account, subject string
	var needsReview int
	if err := conn.QueryRow(
		"SELECT account, subject, needs_review FROM mail_messages WHERE gmail_id = 'abc'").
		Scan(&account, &subject, &needsReview); err != nil {
		t.Fatalf("row did not survive the migration: %v", err)
	}
	if account != "default" {
		t.Errorf("account = %q, want default for a pre-existing row", account)
	}
	if subject != "Thank you for applying to Stripe" || needsReview != 1 {
		t.Errorf("row came across altered: subject=%q needs_review=%d", subject, needsReview)
	}

	// The point of the rebuild: the same id under a second account is now a
	// separate row rather than a primary-key conflict.
	if _, err := conn.Exec(
		"INSERT INTO mail_messages (account, gmail_id, kind, action, seen_at)" +
			" VALUES ('college','abc','confirmation','none','2026-08-11T09:00:00+00:00')",
	); err != nil {
		t.Fatalf("second account still collides after migration: %v", err)
	}

	var n int
	if err := conn.QueryRow(
		"SELECT count(*) FROM mail_messages WHERE gmail_id = 'abc'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("%d rows for the shared id, want 2", n)
	}

	// The scratch table must not be left behind.
	var leftover int
	conn.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='mail_messages_old'").
		Scan(&leftover)
	if leftover != 0 {
		t.Error("mail_messages_old was left behind after the migration")
	}
}

// Migrate runs on every Connect, so it has to be safe to run repeatedly.
func TestMailMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	for i := 0; i < 3; i++ {
		conn, err := Connect(path)
		if err != nil {
			t.Fatalf("connect %d: %v", i, err)
		}
		if err := Migrate(conn); err != nil {
			t.Fatalf("re-migrate %d: %v", i, err)
		}
		conn.Close()
	}
}
