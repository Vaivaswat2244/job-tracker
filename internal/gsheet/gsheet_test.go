package gsheet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"

	"github.com/Vaivaswat2244/job-tracker/internal/db"
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
		{2, "=HYPERLINK(\"http://evil\",\"click\")", "2026-08-08T10:00:00+00:00", 0, 1},
	} {
		if _, err := conn.Exec(
			"INSERT INTO jobs (id, company_id, title, url, seen_at, first_seen_at,"+
				" hires_in_india, auth_required) VALUES (?,1,?,?,?,?,?,?)",
			j.id, j.title, "https://x/"+j.title, j.seen, j.seen, j.india, j.auth); err != nil {
			t.Fatalf("seed job: %v", err)
		}
	}
	// A contact exists so "Contacts is never pushed" is a real assertion rather
	// than a vacuous one against an empty table.
	if _, err := conn.Exec(
		"INSERT INTO contacts (id, company_id, name, title, email, email_confidence)" +
			" VALUES (1, 1, 'A Person', 'Eng Manager', 'a.person@grafana.com', 'inferred')",
	); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	return conn
}

func TestParseSpreadsheetID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"1AbC-dEf_23", "1AbC-dEf_23"},
		{"https://docs.google.com/spreadsheets/d/1AbC-dEf_23/edit#gid=0", "1AbC-dEf_23"},
		{"https://docs.google.com/spreadsheets/d/1AbC-dEf_23", "1AbC-dEf_23"},
		{"  1AbC-dEf_23  ", "1AbC-dEf_23"},
	} {
		if got := ParseSpreadsheetID(tc.in); got != tc.want {
			t.Errorf("ParseSpreadsheetID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The allow-list is a privacy decision, not an oversight: Contacts holds other
// people's details, including guessed addresses. If someone adds it to
// pushOrder this test is what should stop them.
func TestContactsAreNeverPushed(t *testing.T) {
	for _, name := range pushOrder {
		if name == "Contacts" {
			t.Fatal("Contacts is in pushOrder — it must not leave this machine")
		}
	}
	if _, ok := tableFor["Contacts"]; ok {
		t.Error("Contacts has a push builder")
	}
}

func TestCounts(t *testing.T) {
	got, err := Counts(seed(t))
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if got["Pipeline"] != 2 {
		t.Errorf("pipeline = %d, want 2", got["Pipeline"])
	}
	if got["Applications"] != 0 {
		t.Errorf("applications = %d, want 0", got["Applications"])
	}
	if _, ok := got["Contacts"]; ok {
		t.Error("counts reported Contacts")
	}
}

// Nothing configured is "feature off" — the daily timer exits 0 on it. Half
// configured is a typo, and must be a real error.
func TestLoadConfigDistinguishesUnsetFromMisconfigured(t *testing.T) {
	t.Setenv(SheetEnv, "")
	t.Setenv(CredentialsEnv, "")
	if _, err := LoadConfig(); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}

	t.Setenv(SheetEnv, "abc123")
	_, err := LoadConfig()
	if errors.Is(err, ErrNotConfigured) {
		t.Fatal("half-configured reported as not configured")
	}
	if err == nil || !strings.Contains(err.Error(), CredentialsEnv) {
		t.Errorf("error %v does not name the missing %s", err, CredentialsEnv)
	}
}

func TestLoadConfigRejectsAMissingKeyFile(t *testing.T) {
	t.Setenv(SheetEnv, "abc123")
	t.Setenv(CredentialsEnv, filepath.Join(t.TempDir(), "nope.json"))
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected an error for a key file that does not exist")
	}
}

// A nil cell must reach the API as an empty string. Encoding it as JSON null
// makes the request invalid, so an absent pay figure would fail the whole push.
func TestValuesRendersNilAsEmptyString(t *testing.T) {
	conn := seed(t)
	table, err := tableFor["Pipeline"](conn)
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	rows := values(table)
	if len(rows) != 3 { // header + 2
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	for i, cell := range rows[1] {
		if cell == nil {
			t.Fatalf("cell %d is nil; want an empty string", i)
		}
	}
	if rows[0][0] != "job_id" {
		t.Errorf("header = %v", rows[0])
	}
}

// stub records every request the push makes and answers with the minimum the
// client needs to carry on.
//
// Bodies are kept as a list, not keyed by path: a push sends several
// batchUpdate calls to the same URL and a map would keep only the last.
type stub struct {
	calls  []string
	bodies []string
}

func newStub(existingTabs []string) (*httptest.Server, *stub) {
	s := &stub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.calls = append(s.calls, r.Method+" "+r.URL.Path)
		s.bodies = append(s.bodies, string(body))
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet:
			resp := sheets.Spreadsheet{}
			for i, title := range existingTabs {
				resp.Sheets = append(resp.Sheets, &sheets.Sheet{
					Properties: &sheets.SheetProperties{
						SheetId: int64(i + 100), Title: title,
						GridProperties: &sheets.GridProperties{RowCount: 1000, ColumnCount: 26},
					},
				})
			}
			json.NewEncoder(w).Encode(resp)
		case strings.HasSuffix(r.URL.Path, ":batchUpdate") && strings.Contains(r.URL.Path, "/values/"):
			w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, ":batchUpdate"):
			// Reply to every AddSheet so the caller learns the new sheet ids.
			var req sheets.BatchUpdateSpreadsheetRequest
			json.Unmarshal(body, &req)
			resp := sheets.BatchUpdateSpreadsheetResponse{}
			for i, rq := range req.Requests {
				reply := &sheets.Response{}
				if rq.AddSheet != nil {
					reply.AddSheet = &sheets.AddSheetResponse{
						Properties: &sheets.SheetProperties{
							SheetId: int64(i + 200), Title: rq.AddSheet.Properties.Title,
							GridProperties: &sheets.GridProperties{RowCount: 1000, ColumnCount: 26},
						},
					}
				}
				resp.Replies = append(resp.Replies, reply)
			}
			json.NewEncoder(w).Encode(resp)
		default:
			w.Write([]byte(`{}`))
		}
	})
	return httptest.NewServer(mux), s
}

func (s *stub) sent(substr string) bool {
	for _, body := range s.bodies {
		if strings.Contains(body, substr) {
			return true
		}
	}
	return false
}

func pushAgainst(t *testing.T, srv *httptest.Server, conn *sql.DB) Result {
	t.Helper()
	svc, err := sheets.NewService(context.Background(),
		option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	res, err := PushTo(context.Background(), svc, conn, Config{SpreadsheetID: "sid"})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	return res
}

// The full sequence against a blank spreadsheet: create the tabs, make room in
// the grid, clear, then write.
func TestPushCreatesTabsAndWritesRaw(t *testing.T) {
	srv, s := newStub([]string{"Sheet1"})
	defer srv.Close()

	res := pushAgainst(t, srv, seed(t))
	if res.Rows["Pipeline"] != 2 {
		t.Errorf("pipeline rows = %d, want 2", res.Rows["Pipeline"])
	}
	if !strings.Contains(res.URL, "sid") {
		t.Errorf("url = %q", res.URL)
	}

	if !s.sent(`"addSheet"`) {
		t.Error("no tab was created on a spreadsheet that had none")
	}
	if !s.sent(`"RAW"`) {
		t.Error("values were not written with ValueInputOption RAW")
	}
	// USER_ENTERED would let a job title beginning with = run as a formula in a
	// document other people open.
	if s.sent("USER_ENTERED") {
		t.Error("values were written with USER_ENTERED")
	}
	if s.sent(`a.person@grafana.com`) {
		t.Error("a contact's email address was sent to the spreadsheet")
	}
	var cleared bool
	for _, key := range s.calls {
		if strings.Contains(key, "batchClear") {
			cleared = true
		}
	}
	if !cleared {
		t.Error("tabs were not cleared before writing; deleted roles would linger")
	}
}

// The formula-injection guard is about the payload as much as the option: the
// title must travel as its literal text.
func TestPushSendsSuspiciousTitleAsLiteralText(t *testing.T) {
	srv, s := newStub([]string{"Pipeline", "Applications"})
	defer srv.Close()

	pushAgainst(t, srv, seed(t))
	if !s.sent(`=HYPERLINK`) {
		t.Error("the title never reached the write payload")
	}
}

// A second push must not restack the conditional-format rules it added on the
// first, or the sheet accumulates a duplicate rule per day.
func TestSecondPushReplacesConditionalFormats(t *testing.T) {
	srv, s := newStub([]string{"Pipeline", "Applications"})
	defer srv.Close()

	pushAgainst(t, srv, seed(t))
	if !s.sent(`"addConditionalFormatRule"`) {
		t.Error("no conditional formatting was applied")
	}
	// Tabs already existed, so the one-time layout must not be reapplied over
	// column widths the reader may have adjusted.
	if s.sent(`"setBasicFilter"`) {
		t.Error("basic filter was reapplied on a push that created no tabs")
	}
	if s.sent(`"pixelSize"`) {
		t.Error("column widths were reapplied on a push that created no tabs")
	}
}

// The grid on a new tab is 1000 rows; a pipeline larger than that must ask for
// room before writing, or the write fails.
func TestPushGrowsTheGridForALargePipeline(t *testing.T) {
	conn := seed(t)
	for i := 3; i < 1200; i++ {
		if _, err := conn.Exec(
			"INSERT INTO jobs (id, company_id, title, url, seen_at, first_seen_at)"+
				" VALUES (?,1,'Filler',?,?,?)",
			i, fmt.Sprintf("https://x/filler/%d", i),
			"2026-08-01T10:00:00+00:00", "2026-08-01T10:00:00+00:00"); err != nil {
			t.Fatalf("seed filler %d: %v", i, err)
		}
	}
	srv, s := newStub([]string{"Pipeline", "Applications"})
	defer srv.Close()

	pushAgainst(t, srv, conn)
	if !s.sent(`"rowCount"`) {
		t.Error("the grid was never resized for a pipeline larger than the default 1000 rows")
	}
}

func TestDescribeAPIErrorExplainsSharing(t *testing.T) {
	cfg := Config{SpreadsheetID: "sid", CredentialsPath: "/key.json"}
	err := describeAPIError(&stubErr{"googleapi: Error 403: caller does not have permission"}, cfg)
	if !strings.Contains(err.Error(), "Editor") {
		t.Errorf("403 error does not explain sharing: %v", err)
	}
	err = describeAPIError(&stubErr{"googleapi: Error 404: not found"}, cfg)
	if !strings.Contains(err.Error(), SheetEnv) {
		t.Errorf("404 error does not point at the id: %v", err)
	}
}

type stubErr struct{ msg string }

func (e *stubErr) Error() string { return e.msg }
