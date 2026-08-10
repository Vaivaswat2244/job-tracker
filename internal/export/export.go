// Package export does one-way export: SQLite -> tracker.xlsx, and (via gsheet)
// SQLite -> Google Sheets. State is never read back from either.
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

const (
	indiaColour = "C6E0B4" // green
	authColour  = "F4B183" // red
)

var columnWidths = map[string]float64{
	"company": 22, "title": 42, "status": 13, "url": 55, "notes": 30,
}

var pipelineWidths = []float64{8, 20, 46, 26, 12, 12, 16, 10, 10, 9, 7, 14, 20, 55}

var contactWidths = []float64{6, 22, 22, 26, 34, 12, 34, 16}

// Counts is what one export wrote, per sheet. Applications is what the user
// applied to; Pipeline is everything poll ingested, which is the larger number
// and the reason the sheet is worth opening before anything is applied to.
type Counts struct {
	Applications int
	Pipeline     int
}

// Export writes the workbook, returning its absolute path and the row counts.
func Export(conn *sql.DB, path string) (string, Counts, error) {
	var counts Counts
	if path == "" {
		path = "tracker.xlsx"
	}

	applications, err := Applications(conn)
	if err != nil {
		return "", counts, err
	}
	pipeline, err := Pipeline(conn)
	if err != nil {
		return "", counts, err
	}
	contacts, err := Contacts(conn)
	if err != nil {
		return "", counts, err
	}
	counts.Applications = len(applications.Rows)
	counts.Pipeline = len(pipeline.Rows)

	f := excelize.NewFile()
	defer f.Close()

	index, err := f.NewSheet(applications.Name)
	if err != nil {
		return "", counts, fmt.Errorf("create sheet: %w", err)
	}
	f.SetActiveSheet(index)
	// NewFile seeds a "Sheet1"; the export has its own names for every sheet.
	_ = f.DeleteSheet("Sheet1")

	bold, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err != nil {
		return "", counts, fmt.Errorf("create header style: %w", err)
	}

	if err := writeTable(f, applications, bold); err != nil {
		return "", counts, err
	}
	if err := fillByValue(f, applications, "status", func(v any) (string, bool) {
		s, _ := v.(string)
		colour, ok := statusFill[s]
		return colour, ok
	}); err != nil {
		return "", counts, err
	}
	for i, h := range applications.Headers {
		width, ok := columnWidths[h]
		if !ok {
			width = 14
		}
		if err := setWidth(f, applications.Name, i+1, width); err != nil {
			return "", counts, err
		}
	}
	if err := freeze(f, applications.Name); err != nil {
		return "", counts, err
	}
	if err := autoFilter(f, applications); err != nil {
		return "", counts, err
	}

	if _, err := f.NewSheet(pipeline.Name); err != nil {
		return "", counts, fmt.Errorf("create pipeline sheet: %w", err)
	}
	if err := writeTable(f, pipeline, bold); err != nil {
		return "", counts, err
	}
	if err := fillByValue(f, pipeline, "india", flagFill(indiaColour)); err != nil {
		return "", counts, err
	}
	if err := fillByValue(f, pipeline, "auth_required", flagFill(authColour)); err != nil {
		return "", counts, err
	}
	for i, w := range pipelineWidths {
		if err := setWidth(f, pipeline.Name, i+1, w); err != nil {
			return "", counts, err
		}
	}
	if err := freeze(f, pipeline.Name); err != nil {
		return "", counts, err
	}
	if err := autoFilter(f, pipeline); err != nil {
		return "", counts, err
	}

	if _, err := f.NewSheet(contacts.Name); err != nil {
		return "", counts, fmt.Errorf("create contacts sheet: %w", err)
	}
	if err := writeTable(f, contacts, bold); err != nil {
		return "", counts, err
	}
	for i, w := range contactWidths {
		if err := setWidth(f, contacts.Name, i+1, w); err != nil {
			return "", counts, err
		}
	}
	if err := freeze(f, contacts.Name); err != nil {
		return "", counts, err
	}

	if err := f.SaveAs(path); err != nil {
		return "", counts, fmt.Errorf("save %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return abs, counts, nil
}

// writeTable lays down the bold header row and the body, skipping nil cells so
// they stay genuinely empty.
func writeTable(f *excelize.File, t Table, headerStyle int) error {
	for i, h := range t.Headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		if err := f.SetCellValue(t.Name, cell, h); err != nil {
			return fmt.Errorf("write header %q: %w", h, err)
		}
		if err := f.SetCellStyle(t.Name, cell, cell, headerStyle); err != nil {
			return fmt.Errorf("style header %q: %w", h, err)
		}
	}
	for r, row := range t.Rows {
		for c, v := range row {
			if v == nil {
				continue
			}
			cell, _ := excelize.CoordinatesToCellName(c+1, r+2)
			if err := f.SetCellValue(t.Name, cell, v); err != nil {
				return fmt.Errorf("write %s cell: %w", t.Name, err)
			}
		}
	}
	return nil
}

// fillByValue colours one column's cells according to what they contain. The
// style cache matters: excelize interns styles, and creating one per row would
// bloat the workbook.
func fillByValue(f *excelize.File, t Table, column string, colour func(any) (string, bool)) error {
	col := t.Col(column)
	if col == 0 {
		return fmt.Errorf("%s has no %q column", t.Name, column)
	}
	styles := map[string]int{}
	for r, row := range t.Rows {
		if col > len(row) {
			continue
		}
		hex, ok := colour(row[col-1])
		if !ok {
			continue
		}
		style, ok := styles[hex]
		if !ok {
			var err error
			style, err = f.NewStyle(&excelize.Style{
				Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{hex}},
			})
			if err != nil {
				return fmt.Errorf("create %s fill: %w", hex, err)
			}
			styles[hex] = style
		}
		cell, _ := excelize.CoordinatesToCellName(col, r+2)
		if err := f.SetCellStyle(t.Name, cell, cell, style); err != nil {
			return fmt.Errorf("style %s cell: %w", column, err)
		}
	}
	return nil
}

// flagFill colours a 0/1 flag column only when the flag is set.
func flagFill(hex string) func(any) (string, bool) {
	return func(v any) (string, bool) {
		n, ok := v.(int64)
		return hex, ok && n == 1
	}
}

func setWidth(f *excelize.File, sheet string, column int, width float64) error {
	col, _ := excelize.ColumnNumberToName(column)
	if err := f.SetColWidth(sheet, col, col, width); err != nil {
		return fmt.Errorf("set %s column width: %w", sheet, err)
	}
	return nil
}

func freeze(f *excelize.File, sheet string) error {
	if err := f.SetPanes(sheet, &excelize.Panes{
		Freeze: true, Split: false, XSplit: 0, YSplit: 1,
		TopLeftCell: "A2", ActivePane: "bottomLeft",
	}); err != nil {
		return fmt.Errorf("freeze %s panes: %w", sheet, err)
	}
	return nil
}

func autoFilter(f *excelize.File, t Table) error {
	lastCol, _ := excelize.ColumnNumberToName(len(t.Headers))
	rng := fmt.Sprintf("A1:%s%d", lastCol, len(t.Rows)+1)
	if err := f.AutoFilter(t.Name, rng, []excelize.AutoFilterOptions{}); err != nil {
		return fmt.Errorf("set %s autofilter: %w", t.Name, err)
	}
	return nil
}
