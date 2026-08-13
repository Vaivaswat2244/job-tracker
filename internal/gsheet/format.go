package gsheet

import (
	"context"
	"fmt"

	"google.golang.org/api/sheets/v4"

	"github.com/Vaivaswat2244/job-tracker/internal/export"
)

// Colours match the xlsx export so the two views of the same data look like
// the same document. Sheets wants 0..1 floats rather than hex.
var (
	indiaGreen = &sheets.Color{Red: 0.776, Green: 0.878, Blue: 0.706} // C6E0B4
	authAmber  = &sheets.Color{Red: 0.957, Green: 0.694, Blue: 0.514} // F4B183
	headerGrey = &sheets.Color{Red: 0.937, Green: 0.937, Blue: 0.937}
)

// pixelWidths mirror the xlsx column widths, in pixels.
var pixelWidths = map[string][]int64{
	"Pipeline":     {60, 150, 330, 190, 90, 90, 120, 75, 75, 70, 55, 100, 150, 380},
	"Applications": {60, 60, 160, 300, 100, 100, 100, 120, 100, 120, 80, 80, 70, 100, 70, 380, 220},
}

// applyFormatting makes a bare spreadsheet readable: frozen bold header, a
// filter, sensible column widths, and the same India/auth colouring the xlsx
// carries.
//
// Widths and the filter are only applied on the first push, when the tabs are
// created. Re-imposing them every run would stamp on any column the user
// widened or any filter they set for themselves, and this sheet is meant to be
// read by people.
func applyFormatting(ctx context.Context, svc *sheets.Service, spreadsheetID string,
	tables []export.Table, existing map[string]*sheets.Sheet, firstPush bool) error {

	var requests []*sheets.Request

	for _, t := range tables {
		sheet := existing[t.Name]
		if sheet == nil || sheet.Properties == nil {
			continue
		}
		id := sheet.Properties.SheetId

		if firstPush {
			requests = append(requests,
				&sheets.Request{
					UpdateSheetProperties: &sheets.UpdateSheetPropertiesRequest{
						Properties: &sheets.SheetProperties{
							SheetId:        id,
							GridProperties: &sheets.GridProperties{FrozenRowCount: 1},
						},
						Fields: "gridProperties.frozenRowCount",
					},
				},
				&sheets.Request{
					RepeatCell: &sheets.RepeatCellRequest{
						Range: &sheets.GridRange{
							SheetId: id, StartRowIndex: 0, EndRowIndex: 1,
						},
						Cell: &sheets.CellData{
							UserEnteredFormat: &sheets.CellFormat{
								TextFormat:      &sheets.TextFormat{Bold: true},
								BackgroundColor: headerGrey,
							},
						},
						Fields: "userEnteredFormat(textFormat,backgroundColor)",
					},
				},
				&sheets.Request{
					SetBasicFilter: &sheets.SetBasicFilterRequest{
						Filter: &sheets.BasicFilter{
							Range: &sheets.GridRange{
								SheetId:          id,
								StartRowIndex:    0,
								StartColumnIndex: 0,
								EndColumnIndex:   int64(len(t.Headers)),
							},
						},
					},
				},
			)
			for i, px := range pixelWidths[t.Name] {
				if i >= len(t.Headers) {
					break
				}
				requests = append(requests, &sheets.Request{
					UpdateDimensionProperties: &sheets.UpdateDimensionPropertiesRequest{
						Range: &sheets.DimensionRange{
							SheetId:    id,
							Dimension:  "COLUMNS",
							StartIndex: int64(i),
							EndIndex:   int64(i + 1),
						},
						Properties: &sheets.DimensionProperties{PixelSize: px},
						Fields:     "pixelSize",
					},
				})
			}
		}

		rules := conditionalRules(t, id)
		if len(rules) == 0 {
			continue
		}
		// Drop the rules from the last push before adding this one's, highest
		// index first so the earlier indices stay valid. Without this every run
		// would stack another identical copy onto the sheet.
		for i := len(sheet.ConditionalFormats) - 1; i >= 0; i-- {
			requests = append(requests, &sheets.Request{
				DeleteConditionalFormatRule: &sheets.DeleteConditionalFormatRuleRequest{
					SheetId: id, Index: int64(i),
				},
			})
		}
		requests = append(requests, rules...)
	}

	if len(requests) == 0 {
		return nil
	}
	if _, err := svc.Spreadsheets.BatchUpdate(spreadsheetID,
		&sheets.BatchUpdateSpreadsheetRequest{Requests: requests}).Context(ctx).Do(); err != nil {
		return fmt.Errorf("format sheet: %w", err)
	}
	return nil
}

// statusColours mirror the xlsx status fills, so the same application looks the
// same in the workbook and in the shared sheet.
var statusColours = []struct {
	status string
	colour *sheets.Color
}{
	{"found", &sheets.Color{Red: 0.851, Green: 0.851, Blue: 0.851}},       // grey
	{"applied", &sheets.Color{Red: 1, Green: 0.902, Blue: 0.600}},         // yellow
	{"followed_up", &sheets.Color{Red: 0.973, Green: 0.796, Blue: 0.678}}, // orange
	{"in_process", &sheets.Color{Red: 0.741, Green: 0.843, Blue: 0.933}},  // blue
	{"offer", indiaGreen},
	{"rejected", authAmber},
	{"ghosted", authAmber},
}

// conditionalRules colours by value rather than writing a style per cell, so
// the colouring survives the next push without being rewritten row by row —
// and, for Applications, is already in place before the first row lands.
func conditionalRules(t export.Table, sheetID int64) []*sheets.Request {
	var out []*sheets.Request

	// Applications is coloured by status; a table with a status column gets the
	// ladder's palette rather than the pipeline's flags.
	if col := t.Col("status"); col > 0 {
		for _, s := range statusColours {
			out = append(out, &sheets.Request{
				AddConditionalFormatRule: &sheets.AddConditionalFormatRuleRequest{
					Index: 0,
					Rule: &sheets.ConditionalFormatRule{
						Ranges: []*sheets.GridRange{{
							SheetId:          sheetID,
							StartRowIndex:    1,
							StartColumnIndex: int64(col - 1),
							EndColumnIndex:   int64(col),
						}},
						BooleanRule: &sheets.BooleanRule{
							Condition: &sheets.BooleanCondition{
								Type:   "TEXT_EQ",
								Values: []*sheets.ConditionValue{{UserEnteredValue: s.status}},
							},
							Format: &sheets.CellFormat{BackgroundColor: s.colour},
						},
					},
				},
			})
		}
	}

	for _, rule := range []struct {
		column string
		colour *sheets.Color
	}{
		{"india", indiaGreen},
		{"auth_required", authAmber},
	} {
		col := t.Col(rule.column)
		if col == 0 {
			continue
		}
		out = append(out, &sheets.Request{
			AddConditionalFormatRule: &sheets.AddConditionalFormatRuleRequest{
				Index: 0,
				Rule: &sheets.ConditionalFormatRule{
					Ranges: []*sheets.GridRange{{
						SheetId:          sheetID,
						StartRowIndex:    1, // never the header
						StartColumnIndex: int64(col - 1),
						EndColumnIndex:   int64(col),
					}},
					BooleanRule: &sheets.BooleanRule{
						Condition: &sheets.BooleanCondition{
							Type:   "NUMBER_EQ",
							Values: []*sheets.ConditionValue{{UserEnteredValue: "1"}},
						},
						Format: &sheets.CellFormat{BackgroundColor: rule.colour},
					},
				},
			},
		})
	}
	return out
}
