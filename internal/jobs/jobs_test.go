package jobs

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Vaivaswat2244/job-tracker/internal/db"
)

func seed(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Connect(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	for _, c := range []struct {
		id   int
		name string
	}{{1, "Grafana Labs"}, {2, "GoPuff"}} {
		if _, err := conn.Exec(
			"INSERT INTO companies (id, name) VALUES (?,?)", c.id, c.name); err != nil {
			t.Fatalf("seed company: %v", err)
		}
	}

	rows := []struct {
		id                  int
		company             int
		title, loc, seen    string
		india, auth, remote any
		canonical           any
	}{
		{1, 1, "Senior Backend Engineer", "Remote - India", "2026-08-09T10:00:00+00:00", 1, 0, 1, nil},
		{2, 1, "Frontend Engineer", "Berlin", "2026-08-08T10:00:00+00:00", 0, 0, 0, nil},
		{3, 2, "Warehouse Associate", "Philadelphia", "2026-08-07T10:00:00+00:00", 0, 0, 0, nil},
		{4, 2, "Staff Engineer", "New York", "2026-08-09T12:00:00+00:00", 0, 1, 0, nil}, // auth-gated
		{5, 1, "Duplicate Engineer", "Remote", "2026-08-09T13:00:00+00:00", 1, 0, 1, 1}, // linked dupe
	}
	for _, r := range rows {
		if _, err := conn.Exec(
			"INSERT INTO jobs (id, company_id, title, url, seen_at, first_seen_at, location,"+
				" hires_in_india, auth_required, remote, canonical_id)"+
				" VALUES (?,?,?,?,?,?,?,?,?,?,?)",
			r.id, r.company, r.title, "https://x/"+r.title, r.seen, r.seen, r.loc,
			r.india, r.auth, r.remote, r.canonical); err != nil {
			t.Fatalf("seed job %d: %v", r.id, err)
		}
	}
	// A hand-added job has seen_at but never first_seen_at.
	if _, err := conn.Exec(
		"INSERT INTO jobs (id, company_id, title, url, seen_at)" +
			" VALUES (6, 1, 'Referred Role', 'https://x/ref', '2026-08-10T09:00:00+00:00')",
	); err != nil {
		t.Fatalf("seed manual job: %v", err)
	}
	return conn
}

func ids(rows []Row) []int64 {
	out := make([]int64, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

func mustList(t *testing.T, conn *sql.DB, f Filter) []Row {
	t.Helper()
	rows, err := List(conn, f)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return rows
}

// A hand-added job only ever gets seen_at. Keying the view on first_seen_at
// alone would make it invisible in its own pipeline.
func TestManuallyAddedJobIsVisible(t *testing.T) {
	conn := seed(t)
	for _, r := range mustList(t, conn, Filter{}) {
		if r.ID == 6 {
			return
		}
	}
	t.Error("job 6 (seen_at only) missing from the default listing")
}

func TestDuplicatesHiddenUnlessAsked(t *testing.T) {
	conn := seed(t)
	for _, r := range mustList(t, conn, Filter{}) {
		if r.ID == 5 {
			t.Fatal("linked duplicate appeared in the default listing")
		}
	}
	var found bool
	for _, r := range mustList(t, conn, Filter{IncludeDuplicates: true}) {
		if r.ID == 5 {
			found = true
		}
	}
	if !found {
		t.Error("--dupes did not surface the linked duplicate")
	}
}

// INV-1: a flag sorts a role down, it never removes it.
func TestAuthRequiredSortsLastButIsStillListed(t *testing.T) {
	rows := mustList(t, seed(t), Filter{})
	got := ids(rows)
	if len(got) == 0 || got[len(got)-1] != 4 {
		t.Errorf("order = %v, want auth-required job 4 last", got)
	}
	if rows[0].HiresInIndia.Int64 != 1 {
		t.Errorf("first row = %+v, want an India-friendly role", rows[0])
	}
}

func TestFilters(t *testing.T) {
	conn := seed(t)
	for _, tc := range []struct {
		name string
		f    Filter
		want []int64
	}{
		{"company substring, case-insensitive", Filter{Company: "grafana"}, []int64{1, 6, 2}},
		{"title substring", Filter{Title: "engineer"}, []int64{1, 2, 4}},
		{"india only", Filter{IndiaOnly: true}, []int64{1}},
		{"remote only", Filter{RemoteOnly: true}, []int64{1}},
		{"combined", Filter{Company: "gopuff", Title: "engineer"}, []int64{4}},
		{"limit", Filter{Limit: 2}, []int64{1, 6}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ids(mustList(t, conn, tc.f))
			if len(got) != len(tc.want) {
				t.Fatalf("ids = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("ids = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestSinceExcludesOlderRows(t *testing.T) {
	conn := seed(t)
	cutoff, err := time.Parse(time.RFC3339, "2026-08-09T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range mustList(t, conn, Filter{Since: cutoff}) {
		if r.ID == 3 { // 2026-08-07
			t.Error("--since let through a row older than the cutoff")
		}
	}
}

func TestCountIgnoresLimit(t *testing.T) {
	conn := seed(t)
	f := Filter{Limit: 2}
	rows := mustList(t, conn, f)
	n, err := f.Count(conn)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("listed %d rows, want 2", len(rows))
	}
	if n != 5 { // 6 seeded, 1 is a linked duplicate
		t.Errorf("count = %d, want 5", n)
	}
}

func TestFlags(t *testing.T) {
	india := sql.NullInt64{Int64: 1, Valid: true}
	for _, tc := range []struct {
		name string
		row  Row
		want string
	}{
		{"india", Row{HiresInIndia: india}, "IN"},
		{"auth", Row{AuthRequired: india}, "AUTH"},
		{"comp model shown when known", Row{
			CompModel: sql.NullString{String: "location_agnostic", Valid: true}}, "location_agnostic"},
		{"unknown comp model omitted", Row{
			CompModel: sql.NullString{String: "unknown", Valid: true}}, ""},
		{"combined", Row{HiresInIndia: india, AuthRequired: india}, "IN AUTH"},
	} {
		if got := tc.row.Flags(); got != tc.want {
			t.Errorf("%s: Flags() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The count wraps the same SQL the listing uses; if the filter ever stops being
// a single self-contained SELECT this breaks loudly rather than miscounting.
func TestSQLIsWrappable(t *testing.T) {
	query, args := Filter{Company: "x", Limit: 5}.SQL()
	if !strings.Contains(query, "LIMIT 5") {
		t.Errorf("limit missing from %q", query)
	}
	if len(args) != 1 {
		t.Errorf("args = %v, want one LIKE argument", args)
	}
}

// Unknown geography must not sort below known-not-India. `NULL = 1` is NULL in
// SQLite, and NULL sorts last under DESC, so the naive predicate buried every
// hand-added referral and every job the normalizer could not place.
func TestUnknownGeographyOutranksKnownNonIndia(t *testing.T) {
	conn := seed(t)
	var unknownAt, nonIndiaAt = -1, -1
	for i, r := range mustList(t, conn, Filter{Company: "grafana"}) {
		switch r.ID {
		case 6: // hires_in_india IS NULL, newest
			unknownAt = i
		case 2: // hires_in_india = 0, older
			nonIndiaAt = i
		}
	}
	if unknownAt < 0 || nonIndiaAt < 0 {
		t.Fatalf("expected both rows listed (unknown=%d, non-india=%d)", unknownAt, nonIndiaAt)
	}
	if unknownAt > nonIndiaAt {
		t.Errorf("unknown-geography row sorted below a known-not-India row (%d > %d)",
			unknownAt, nonIndiaAt)
	}
}
