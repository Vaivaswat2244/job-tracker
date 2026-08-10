package export

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/Vaivaswat2244/job-tracker/internal/db"
	"github.com/xuri/excelize/v2"
)

func seed(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Connect(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	if _, err := conn.Exec(
		"INSERT INTO companies (id, name, domain) VALUES (1, 'Grafana Labs', 'grafana.com')",
	); err != nil {
		t.Fatalf("seed company: %v", err)
	}
	for _, j := range []struct {
		id          int
		title, seen string
		india, auth int
	}{
		{1, "Senior Backend Engineer", "2026-08-09T10:00:00+00:00", 1, 0},
		{2, "US-only Engineer", "2026-08-08T10:00:00+00:00", 0, 1},
		{3, "Support Engineer", "2026-08-07T10:00:00+00:00", 0, 0},
	} {
		if _, err := conn.Exec(
			"INSERT INTO jobs (id, company_id, title, url, seen_at, first_seen_at,"+
				" hires_in_india, auth_required) VALUES (?,1,?,?,?,?,?,?)",
			j.id, j.title, fmt.Sprintf("https://x/%d", j.id), j.seen, j.seen, j.india, j.auth); err != nil {
			t.Fatalf("seed job: %v", err)
		}
	}
	return conn
}

// The workbook must be worth opening before anything has been applied to —
// which was the whole defect: Applications starts FROM applications.
func TestPipelineSheetCarriesIngestedJobsWithNoApplications(t *testing.T) {
	conn := seed(t)
	path := filepath.Join(t.TempDir(), "out.xlsx")

	_, counts, err := Export(conn, path)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if counts.Applications != 0 {
		t.Errorf("applications = %d, want 0", counts.Applications)
	}
	if counts.Pipeline != 3 {
		t.Fatalf("pipeline = %d, want 3", counts.Pipeline)
	}

	f, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatalf("open workbook: %v", err)
	}
	defer f.Close()

	names := f.GetSheetList()
	want := map[string]bool{"Applications": false, "Pipeline": false, "Contacts": false}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("sheet %q missing from %v", name, names)
		}
	}

	rows, err := f.GetRows("Pipeline")
	if err != nil {
		t.Fatalf("read pipeline: %v", err)
	}
	if len(rows) != 4 { // header + 3
		t.Fatalf("pipeline rows = %d, want 4", len(rows))
	}
	if rows[0][0] != "job_id" || rows[0][1] != "company" {
		t.Errorf("headers = %v", rows[0])
	}
	// India-friendly first, auth-gated last (INV-1: sorted, never dropped).
	if rows[1][2] != "Senior Backend Engineer" {
		t.Errorf("first row = %q, want the India-friendly role", rows[1][2])
	}
	if rows[3][2] != "US-only Engineer" {
		t.Errorf("last row = %q, want the auth-required role", rows[3][2])
	}
	// Dates are rendered day-only for a human reader.
	if got := rows[1][4]; got != "2026-08-09" {
		t.Errorf("seen = %q, want 2026-08-09", got)
	}
}
