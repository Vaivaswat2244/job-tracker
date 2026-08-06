// Package export does one-way export: SQLite -> tracker.xlsx. State is never
// read back from the sheet.
package export

import (
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/xuri/excelize/v2"
)

var statusFill = map[string]string{
	"found":       "D9D9D9", // grey
	"applied":     "FFE699", // yellow
	"followed_up": "F8CBAD", // orange
	"in_process":  "BDD7EE", // blue
	"offer":       "C6E0B4", // green
	"rejected":    "F4B183", // red
	"ghosted":     "F4B183", // red
}

var headers = []string{
	"app_id", "job_id", "company", "title", "status", "applied_at",
	"followup_due", "days_since_applied", "source", "comp_model",
	"pay_min", "pay_max", "currency", "auth_required", "contacts", "url", "notes",
}

// query orders NULL follow-up dates last, then by due date — the soonest
// follow-up is what the user opens the sheet to see.
const query = `
SELECT a.id AS app_id, j.id AS job_id, c.name AS company, j.title, a.status,
       a.applied_at, a.followup_due, j.source, j.comp_model, j.pay_min, j.pay_max,
       j.pay_currency, j.auth_required, j.url, a.notes,
       (SELECT count(*) FROM contacts ct WHERE ct.company_id = c.id) AS contacts,
       CAST(julianday('now') - julianday(a.applied_at) AS INTEGER) AS days_since_applied
FROM applications a
JOIN jobs j ON j.id = a.job_id
JOIN companies c ON c.id = j.company_id
ORDER BY (a.followup_due IS NULL), a.followup_due, a.id
`

var columnWidths = map[string]float64{
	"company": 22, "title": 42, "status": 13, "url": 55, "notes": 30,
}

// Export writes the workbook, returning its absolute path and the row count.
func Export(conn *sql.DB, path string) (string, int, error) {
	if path == "" {
		path = "tracker.xlsx"
	}

	f := excelize.NewFile()
	defer f.Close()

	const sheet = "Applications"
	index, err := f.NewSheet(sheet)
	if err != nil {
		return "", 0, fmt.Errorf("create sheet: %w", err)
	}
	f.SetActiveSheet(index)
	// NewFile seeds a "Sheet1"; the export has its own names for both sheets.
	_ = f.DeleteSheet("Sheet1")

	bold, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err != nil {
		return "", 0, fmt.Errorf("create header style: %w", err)
	}
	if err := writeHeaders(f, sheet, headers, bold); err != nil {
		return "", 0, err
	}

	count, err := writeApplications(f, sheet, conn)
	if err != nil {
		return "", 0, err
	}

	for i, h := range headers {
		width, ok := columnWidths[h]
		if !ok {
			width = 14
		}
		col, _ := excelize.ColumnNumberToName(i + 1)
		if err := f.SetColWidth(sheet, col, col, width); err != nil {
			return "", 0, fmt.Errorf("set column width: %w", err)
		}
	}
	if err := f.SetPanes(sheet, &excelize.Panes{
		Freeze: true, Split: false, XSplit: 0, YSplit: 1,
		TopLeftCell: "A2", ActivePane: "bottomLeft",
	}); err != nil {
		return "", 0, fmt.Errorf("freeze panes: %w", err)
	}
	lastCol, _ := excelize.ColumnNumberToName(len(headers))
	if err := f.AutoFilter(sheet, fmt.Sprintf("A1:%s%d", lastCol, count+1),
		[]excelize.AutoFilterOptions{}); err != nil {
		return "", 0, fmt.Errorf("set autofilter: %w", err)
	}

	if err := contactsSheet(f, conn, bold); err != nil {
		return "", 0, err
	}
	if err := f.SaveAs(path); err != nil {
		return "", 0, fmt.Errorf("save %s: %w", path, err)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return abs, count, nil
}

func writeHeaders(f *excelize.File, sheet string, names []string, style int) error {
	for i, h := range names {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		if err := f.SetCellValue(sheet, cell, h); err != nil {
			return fmt.Errorf("write header %q: %w", h, err)
		}
		if err := f.SetCellStyle(sheet, cell, cell, style); err != nil {
			return fmt.Errorf("style header %q: %w", h, err)
		}
	}
	return nil
}

func writeApplications(f *excelize.File, sheet string, conn *sql.DB) (int, error) {
	rows, err := conn.Query(query)
	if err != nil {
		return 0, fmt.Errorf("read applications: %w", err)
	}
	defer rows.Close()

	statusCol := indexOf(headers, "status") + 1
	fills := map[string]int{}
	row := 1

	for rows.Next() {
		var (
			appID, jobID                int64
			company, title, status      string
			appliedAt, followupDue      sql.NullString
			source, compModel, currency sql.NullString
			payMin, payMax              sql.NullFloat64
			authRequired                sql.NullInt64
			url, notes                  sql.NullString
			contacts                    int
			daysSinceApplied            sql.NullInt64
		)
		if err := rows.Scan(&appID, &jobID, &company, &title, &status,
			&appliedAt, &followupDue, &source, &compModel, &payMin, &payMax,
			&currency, &authRequired, &url, &notes, &contacts, &daysSinceApplied); err != nil {
			return 0, fmt.Errorf("scan application: %w", err)
		}
		row++

		values := []any{
			appID, jobID, company, title, status, nullAny(appliedAt), nullAny(followupDue),
			nullIntAny(daysSinceApplied), nullAny(source), nullAny(compModel),
			nullFloatAny(payMin), nullFloatAny(payMax), nullAny(currency),
			nullIntAny(authRequired), contacts, nullAny(url), nullAny(notes),
		}
		for i, v := range values {
			if v == nil {
				continue
			}
			cell, _ := excelize.CoordinatesToCellName(i+1, row)
			if err := f.SetCellValue(sheet, cell, v); err != nil {
				return 0, fmt.Errorf("write cell: %w", err)
			}
		}

		colour, ok := statusFill[status]
		if !ok {
			continue
		}
		style, ok := fills[colour]
		if !ok {
			style, err = f.NewStyle(&excelize.Style{
				Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{colour}},
			})
			if err != nil {
				return 0, fmt.Errorf("create status fill: %w", err)
			}
			fills[colour] = style
		}
		cell, _ := excelize.CoordinatesToCellName(statusCol, row)
		if err := f.SetCellStyle(sheet, cell, cell, style); err != nil {
			return 0, fmt.Errorf("style status cell: %w", err)
		}
	}
	return row - 1, rows.Err()
}

func contactsSheet(f *excelize.File, conn *sql.DB, bold int) error {
	const sheet = "Contacts"
	if _, err := f.NewSheet(sheet); err != nil {
		return fmt.Errorf("create contacts sheet: %w", err)
	}

	names := []string{"id", "company", "name", "title", "email", "confidence", "linkedin", "source"}
	if err := writeHeaders(f, sheet, names, bold); err != nil {
		return err
	}

	rows, err := conn.Query(
		"SELECT ct.id, c.name AS company, ct.name, ct.title, ct.email, ct.email_confidence," +
			" ct.linkedin_url, ct.source FROM contacts ct" +
			" JOIN companies c ON c.id = ct.company_id ORDER BY c.name, ct.name")
	if err != nil {
		return fmt.Errorf("read contacts: %w", err)
	}
	defer rows.Close()

	row := 1
	for rows.Next() {
		var (
			id                   int64
			company, name, title string
			email, confidence    sql.NullString
			linkedin, source     sql.NullString
		)
		if err := rows.Scan(&id, &company, &name, &title, &email,
			&confidence, &linkedin, &source); err != nil {
			return fmt.Errorf("scan contact: %w", err)
		}
		row++

		// INV-2: an inferred address must never be pasteable as-is.
		shown := email.String
		if email.Valid && email.String != "" && confidence.String == "inferred" {
			shown = "[UNVERIFIED: " + email.String + "]"
		}

		values := []any{id, company, name, title, nullOr(email, shown),
			nullAny(confidence), nullAny(linkedin), nullAny(source)}
		for i, v := range values {
			if v == nil {
				continue
			}
			cell, _ := excelize.CoordinatesToCellName(i+1, row)
			if err := f.SetCellValue(sheet, cell, v); err != nil {
				return fmt.Errorf("write contact cell: %w", err)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for i, w := range []float64{6, 22, 22, 26, 34, 12, 34, 16} {
		col, _ := excelize.ColumnNumberToName(i + 1)
		if err := f.SetColWidth(sheet, col, col, w); err != nil {
			return fmt.Errorf("set contacts column width: %w", err)
		}
	}
	return f.SetPanes(sheet, &excelize.Panes{
		Freeze: true, Split: false, XSplit: 0, YSplit: 1,
		TopLeftCell: "A2", ActivePane: "bottomLeft",
	})
}

func indexOf(list []string, want string) int {
	for i, v := range list {
		if v == want {
			return i
		}
	}
	return -1
}

// nullAny and friends leave a SQL NULL as an empty cell rather than writing a
// zero value, matching what openpyxl did with None.
func nullAny(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}

func nullOr(v sql.NullString, replacement string) any {
	if !v.Valid {
		return nil
	}
	return replacement
}

func nullIntAny(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

func nullFloatAny(v sql.NullFloat64) any {
	if !v.Valid {
		return nil
	}
	return v.Float64
}
