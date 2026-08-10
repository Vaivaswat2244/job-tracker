package db_test

import (
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/Vaivaswat2244/job-tracker/internal/db"
)

func TestConnectAppliesSchemaAndMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	conn, err := db.Connect(path)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	// Re-running must be a no-op, proving migrate is idempotent.
	if err := db.Migrate(conn); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	var n int
	if err := conn.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&n); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if n < 12 {
		t.Errorf("expected >=12 tables, got %d", n)
	}
	// A migrated column must be usable.
	if _, err := conn.Exec(
		"INSERT INTO companies (name, ats, slug) VALUES (?,?,?)", "Acme", "lever", "acme"); err != nil {
		t.Fatalf("insert with migrated columns: %v", err)
	}
	if err := db.LogExclusion(conn, "{}", "test", "R1"); err != nil {
		t.Fatalf("log exclusion: %v", err)
	}
}

// Now must render UTC as "+00:00", the way Python's isoformat does — not as a
// bare "Z". Every follow-up query compares these timestamps as strings, so rows
// written in the two spellings would stop sorting against each other.
func TestNowMatchesPythonIsoformat(t *testing.T) {
	got := db.Now()

	want := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\+00:00$`)
	if !want.MatchString(got) {
		t.Fatalf("Now() = %q, want ISO-8601 with a +00:00 offset", got)
	}
	if _, err := time.Parse(db.ISO8601, got); err != nil {
		t.Errorf("Now() output does not parse with ISO8601: %v", err)
	}
}

// Timestamps must sort lexicographically in the order they occur, which is what
// the followup_due and polled_at comparisons depend on.
func TestTimestampsSortLexicographically(t *testing.T) {
	base := time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC)
	prev := ""
	for _, offset := range []time.Duration{0, time.Second, time.Hour, 48 * time.Hour} {
		got := base.Add(offset).Format(db.ISO8601)
		if prev != "" && !(prev < got) {
			t.Errorf("%q should sort before %q", prev, got)
		}
		prev = got
	}
}
