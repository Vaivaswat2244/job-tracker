package export

import (
	"fmt"
	"testing"
	"time"
)

// The daily read: roles that appeared in the last 24 hours, and nothing older.
func TestNewRolesKeepsOnlyTheLastDay(t *testing.T) {
	conn := seed(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

	// seed() lays down 2026-08-09, -08 and -07. Only the first two are inside a
	// rolling 24 hours from midday on the 9th.
	table, err := NewRoles(conn, now)
	if err != nil {
		t.Fatalf("NewRoles: %v", err)
	}
	if table.Name != "New (24h)" {
		t.Errorf("name = %q", table.Name)
	}
	if len(table.Rows) != 2 {
		t.Fatalf("%d rows, want 2 (the 9th and the 8th)", len(table.Rows))
	}
	seen := table.Col("seen") - 1
	for _, row := range table.Rows {
		if day := row[seen].(string); day < "2026-08-08" {
			t.Errorf("row from %s leaked past the cutoff", day)
		}
	}
}

// Same columns as Pipeline, so a reader moving between the two tabs is not
// reading two different layouts.
func TestNewRolesMatchesPipelineColumns(t *testing.T) {
	conn := seed(t)
	pipeline, err := Pipeline(conn)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := NewRoles(conn, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.Headers) != len(pipeline.Headers) {
		t.Fatalf("headers = %v, want the pipeline's %v", fresh.Headers, pipeline.Headers)
	}
	for i := range pipeline.Headers {
		if fresh.Headers[i] != pipeline.Headers[i] {
			t.Errorf("header %d = %q, want %q", i, fresh.Headers[i], pipeline.Headers[i])
		}
	}
}

// A quiet day is an empty tab, not an error and not yesterday's rows left in
// place — a stale "new today" is worse than an empty one.
func TestNewRolesIsEmptyWhenNothingIsNew(t *testing.T) {
	conn := seed(t)
	future := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	table, err := NewRoles(conn, future)
	if err != nil {
		t.Fatalf("NewRoles: %v", err)
	}
	if len(table.Rows) != 0 {
		t.Errorf("%d rows on a day with nothing new, want 0", len(table.Rows))
	}
}

// The ranking has to survive the filter: an auth-gated role still sorts last
// within the day's roles (INV-1 — a flag sorts, it never removes).
func TestNewRolesKeepsPipelineOrdering(t *testing.T) {
	conn := seed(t)
	if _, err := conn.Exec(
		"INSERT INTO jobs (id, company_id, title, url, seen_at, first_seen_at,"+
			" hires_in_india, auth_required) VALUES (?,1,?,?,?,?,?,?)",
		99, "Auth Gated Role", fmt.Sprintf("https://x/%d", 99),
		"2026-08-09T11:00:00+00:00", "2026-08-09T11:00:00+00:00", 0, 1); err != nil {
		t.Fatal(err)
	}
	table, err := NewRoles(conn, time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	// Both seeded auth-gated roles sink below the unrestricted one; among
	// themselves the newer sorts first, so the tail is auth-gated throughout.
	auth := table.Col("auth_required") - 1
	var seenAuth bool
	for i, row := range table.Rows {
		gated, _ := row[auth].(int64)
		if gated == 1 {
			seenAuth = true
			continue
		}
		if seenAuth {
			t.Fatalf("row %d is unrestricted but sorts below an auth-gated role", i)
		}
	}
	if !seenAuth {
		t.Fatal("the auth-gated role is missing entirely — a flag must sort, not remove")
	}
}
