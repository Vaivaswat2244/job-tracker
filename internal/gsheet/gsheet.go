// Package gsheet pushes the export tables into a Google Sheet the user owns.
//
// The direction is one-way, like the xlsx export: SQLite is the source of
// truth and nothing is ever read back. A shared sheet is a view for other
// people, not a second database.
//
// Auth is a service account rather than a user OAuth flow because the push
// runs from a systemd timer, which has no browser to complete a consent
// screen and no one present to click through one. The user creates the sheet
// in their own Drive and shares it with the service account, so they stay the
// owner: sharing with friends is ordinary Drive sharing and revoking access is
// one click.
package gsheet

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/joho/godotenv"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"

	"github.com/Vaivaswat2244/job-tracker/internal/export"
)

const (
	// SheetEnv accepts a bare spreadsheet id or a pasted browser URL.
	SheetEnv = "GOOGLE_SHEET_ID"
	// CredentialsEnv points at the service account JSON key. It is a path,
	// never the key itself, so the secret stays out of the environment and
	// out of `ps`.
	CredentialsEnv = "GOOGLE_SHEET_CREDENTIALS"
)

// Config is what the push needs beyond the database.
type Config struct {
	SpreadsheetID   string
	CredentialsPath string
}

// tableFor is the allow-list of what may leave this machine.
//
// Contacts is deliberately absent. It holds other people's names, titles and
// email addresses, including `inferred` ones that exist only as guesses and
// are rendered [UNVERIFIED: …] precisely so they are never mistaken for real.
// A sheet shared with friends is the wrong home for third parties' contact
// details, and an unverified guess read by someone else is exactly the harm
// INV-2 exists to prevent. It stays in the local xlsx only.
var tableFor = map[string]func(*sql.DB) (export.Table, error){
	"Applications": export.Applications,
	"Pipeline":     export.Pipeline,
}

// pushOrder fixes the tab order; map iteration would shuffle it per run.
var pushOrder = []string{"Pipeline", "Applications"}

// sheetIDPattern pulls the id out of a pasted URL like
// https://docs.google.com/spreadsheets/d/<id>/edit#gid=0
var sheetIDPattern = regexp.MustCompile(`/spreadsheets/d/([a-zA-Z0-9-_]+)`)

// ParseSpreadsheetID accepts either a bare id or the URL from the browser bar,
// because the id is what the API wants but the URL is what a person has.
func ParseSpreadsheetID(value string) string {
	value = strings.TrimSpace(value)
	if m := sheetIDPattern.FindStringSubmatch(value); m != nil {
		return m[1]
	}
	return value
}

// ErrNotConfigured means the Google Sheet was never set up, as distinct from
// being set up wrongly. The daily timer treats it as "feature off" and exits
// quietly; anything else is a real failure worth a red unit in systemctl.
var ErrNotConfigured = fmt.Errorf("google sheet not configured (%s and %s unset)",
	SheetEnv, CredentialsEnv)

// LoadConfig reads .env then the environment, so a value exported in the shell
// wins over the file.
func LoadConfig() (Config, error) {
	_ = godotenv.Load(".env")

	cfg := Config{
		SpreadsheetID:   ParseSpreadsheetID(os.Getenv(SheetEnv)),
		CredentialsPath: strings.TrimSpace(os.Getenv(CredentialsEnv)),
	}
	if cfg.SpreadsheetID == "" && cfg.CredentialsPath == "" {
		return cfg, ErrNotConfigured
	}
	// Half-configured is a mistake, not a choice: say which half.
	var missing []string
	if cfg.SpreadsheetID == "" {
		missing = append(missing, SheetEnv)
	}
	if cfg.CredentialsPath == "" {
		missing = append(missing, CredentialsEnv)
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("%s not set — see the Google Sheet section of the README",
			strings.Join(missing, " and "))
	}
	if _, err := os.Stat(cfg.CredentialsPath); err != nil {
		return cfg, fmt.Errorf("service account key %s: %w", cfg.CredentialsPath, err)
	}
	return cfg, nil
}

// Result reports what one push wrote.
type Result struct {
	Rows map[string]int
	URL  string
}

// Push authenticates with the service account and sends the tables.
func Push(ctx context.Context, conn *sql.DB, cfg Config) (Result, error) {
	svc, err := sheets.NewService(ctx,
		option.WithCredentialsFile(cfg.CredentialsPath),
		option.WithScopes(sheets.SpreadsheetsScope))
	if err != nil {
		return Result{}, fmt.Errorf("google sheets client: %w", err)
	}
	return PushTo(ctx, svc, conn, cfg)
}

// PushTo writes every allow-listed table into the spreadsheet, creating tabs
// that do not exist yet and formatting them so a fresh sheet needs no manual
// setup. It takes the service rather than building one so the whole request
// sequence can be tested against a stub endpoint.
func PushTo(ctx context.Context, svc *sheets.Service, conn *sql.DB, cfg Config) (Result, error) {
	result := Result{
		Rows: map[string]int{},
		URL:  "https://docs.google.com/spreadsheets/d/" + cfg.SpreadsheetID,
	}

	tables := make([]export.Table, 0, len(pushOrder))
	for _, name := range pushOrder {
		build, ok := tableFor[name]
		if !ok {
			return result, fmt.Errorf("no builder for table %q", name)
		}
		t, err := build(conn)
		if err != nil {
			return result, err
		}
		tables = append(tables, t)
		result.Rows[t.Name] = len(t.Rows)
	}

	meta, err := svc.Spreadsheets.Get(cfg.SpreadsheetID).Context(ctx).
		Fields("sheets.properties,sheets.conditionalFormats").Do()
	if err != nil {
		return result, describeAPIError(err, cfg)
	}

	existing := map[string]*sheets.Sheet{}
	for _, s := range meta.Sheets {
		if s.Properties != nil {
			existing[s.Properties.Title] = s
		}
	}

	// Create missing tabs first: everything after this needs their sheet ids.
	var create []*sheets.Request
	for _, t := range tables {
		if _, ok := existing[t.Name]; ok {
			continue
		}
		create = append(create, &sheets.Request{
			AddSheet: &sheets.AddSheetRequest{
				Properties: &sheets.SheetProperties{Title: t.Name},
			},
		})
	}
	firstPush := len(create) > 0
	if firstPush {
		resp, err := svc.Spreadsheets.BatchUpdate(cfg.SpreadsheetID,
			&sheets.BatchUpdateSpreadsheetRequest{Requests: create}).Context(ctx).Do()
		if err != nil {
			return result, fmt.Errorf("create tabs: %w", err)
		}
		for _, r := range resp.Replies {
			if r.AddSheet != nil && r.AddSheet.Properties != nil {
				existing[r.AddSheet.Properties.Title] = &sheets.Sheet{Properties: r.AddSheet.Properties}
			}
		}
	}

	// Grow the grid before writing. A new tab is 1000x26 and values.update
	// will not silently extend it, so a 2000-row pipeline needs the room made
	// first — otherwise the write fails once the pipeline outgrows the default.
	var resize []*sheets.Request
	for _, t := range tables {
		sheet := existing[t.Name]
		if sheet == nil || sheet.Properties == nil {
			return result, fmt.Errorf("tab %q missing after create", t.Name)
		}
		wantRows := int64(len(t.Rows) + 1)
		wantCols := int64(len(t.Headers))
		grid := sheet.Properties.GridProperties
		if grid != nil && grid.RowCount >= wantRows && grid.ColumnCount >= wantCols {
			continue
		}
		rows, cols := wantRows, wantCols
		if grid != nil {
			rows = max64(rows, grid.RowCount)
			cols = max64(cols, grid.ColumnCount)
		}
		resize = append(resize, &sheets.Request{
			UpdateSheetProperties: &sheets.UpdateSheetPropertiesRequest{
				Properties: &sheets.SheetProperties{
					SheetId:        sheet.Properties.SheetId,
					GridProperties: &sheets.GridProperties{RowCount: rows, ColumnCount: cols},
				},
				Fields: "gridProperties.rowCount,gridProperties.columnCount",
			},
		})
	}
	if len(resize) > 0 {
		if _, err := svc.Spreadsheets.BatchUpdate(cfg.SpreadsheetID,
			&sheets.BatchUpdateSpreadsheetRequest{Requests: resize}).Context(ctx).Do(); err != nil {
			return result, fmt.Errorf("resize grid: %w", err)
		}
	}

	// Clear before writing: a role that disappears from the pipeline must not
	// linger as a stale row below the new data.
	var ranges []string
	for _, t := range tables {
		ranges = append(ranges, t.Name)
	}
	if _, err := svc.Spreadsheets.Values.BatchClear(cfg.SpreadsheetID,
		&sheets.BatchClearValuesRequest{Ranges: ranges}).Context(ctx).Do(); err != nil {
		return result, fmt.Errorf("clear tabs: %w", err)
	}

	data := make([]*sheets.ValueRange, 0, len(tables))
	for _, t := range tables {
		data = append(data, &sheets.ValueRange{
			Range:  t.Name + "!A1",
			Values: values(t),
		})
	}
	if _, err := svc.Spreadsheets.Values.BatchUpdate(cfg.SpreadsheetID,
		&sheets.BatchUpdateValuesRequest{
			// RAW, never USER_ENTERED: job titles and locations come from
			// third-party ATS feeds, and USER_ENTERED would let a title
			// beginning with = or + evaluate as a formula in a document other
			// people open. RAW stores every cell as the literal text it is.
			ValueInputOption: "RAW",
			Data:             data,
		}).Context(ctx).Do(); err != nil {
		return result, fmt.Errorf("write values: %w", err)
	}

	if err := applyFormatting(ctx, svc, cfg.SpreadsheetID, tables, existing, firstPush); err != nil {
		return result, err
	}
	return result, nil
}

// Counts reports how many rows each pushed table currently holds, without
// touching the network, so `--dry-run` works before credentials exist.
func Counts(conn *sql.DB) (map[string]int, error) {
	out := map[string]int{}
	for _, name := range pushOrder {
		t, err := tableFor[name](conn)
		if err != nil {
			return nil, err
		}
		out[t.Name] = len(t.Rows)
	}
	return out, nil
}

// values renders a table as the API's row-major payload. A nil cell becomes an
// empty string: the JSON encoding has no notion of "leave this alone", and an
// empty string in a cleared sheet is an empty cell.
func values(t export.Table) [][]any {
	out := make([][]any, 0, len(t.Rows)+1)
	header := make([]any, len(t.Headers))
	for i, h := range t.Headers {
		header[i] = h
	}
	out = append(out, header)
	for _, row := range t.Rows {
		cells := make([]any, len(row))
		for i, v := range row {
			if v == nil {
				cells[i] = ""
				continue
			}
			cells[i] = v
		}
		out = append(out, cells)
	}
	return out
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// describeAPIError turns the two failures a first-time setup actually hits into
// instructions rather than a bare 403/404.
func describeAPIError(err error, cfg Config) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "404"):
		return fmt.Errorf("spreadsheet %s not found — check %s is the id from the sheet URL: %w",
			cfg.SpreadsheetID, SheetEnv, err)
	case strings.Contains(msg, "403"):
		return fmt.Errorf(
			"access denied — share the sheet with the service account's client_email "+
				"(in %s) as an Editor: %w", cfg.CredentialsPath, err)
	default:
		return fmt.Errorf("read spreadsheet: %w", err)
	}
}
