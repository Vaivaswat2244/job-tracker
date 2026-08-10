package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Vaivaswat2244/job-tracker/internal/gsheet"
)

const sheetSetupHelp = `Set up the Google Sheet once:

  1. Create an empty spreadsheet in your own Google Drive. You stay the owner,
     so you share it with people the normal way.
  2. In console.cloud.google.com: new project -> enable the Google Sheets API
     -> IAM & Admin -> Service Accounts -> Create -> Keys -> Add key (JSON).
  3. Save the JSON somewhere private, e.g. ~/.config/tracker/google-sheet.json,
     and chmod 600 it. It is a credential; do not put it in the repo.
  4. Open the JSON, copy "client_email", and share the spreadsheet with that
     address as an Editor.
  5. Put both values in .env (already gitignored):

       GOOGLE_SHEET_ID=<the id from the sheet URL, or paste the whole URL>
       GOOGLE_SHEET_CREDENTIALS=/home/you/.config/tracker/google-sheet.json

Then: tracker sheet push
`

func sheetCmd(conn *sql.DB, args []string) int {
	if len(args) == 0 {
		return fail("usage: tracker sheet <push|setup>")
	}
	switch args[0] {
	case "push":
		return sheetPush(conn, args[1:])
	case "setup":
		fmt.Fprint(stdout, sheetSetupHelp)
		return 0
	default:
		return fail("unknown sheet subcommand %q (push, setup)", args[0])
	}
}

func sheetPush(conn *sql.DB, args []string) int {
	fs := newFlags("sheet push")
	timeout := fs.Duration("timeout", 2*time.Minute, "give up after this long")
	dryRun := fs.Bool("dry-run", false, "report what would be sent without sending it")
	pos, ok := parse(fs, args)
	if !ok {
		return 2
	}
	if len(pos) != 0 {
		return fail("usage: tracker sheet push [--dry-run] [--timeout 2m]")
	}

	cfg, err := gsheet.LoadConfig()
	if err != nil {
		if *dryRun {
			// A dry run is what someone types before they have credentials, so
			// it must still be able to report what would be sent.
			return sheetDryRun(conn, err.Error())
		}
		fmt.Fprintf(stdout, "%v\n\n%s", err, sheetSetupHelp)
		// Never set up is a choice; the daily timer should not turn red over
		// it. Set up wrongly is a fault and must be noisy.
		if errors.Is(err, gsheet.ErrNotConfigured) {
			return 0
		}
		return 1
	}
	if *dryRun {
		return sheetDryRun(conn, "")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	result, err := gsheet.Push(ctx, conn, cfg)
	if err != nil {
		return fail("%v", err)
	}
	for _, name := range sortedNames(result.Rows) {
		fmt.Fprintf(stdout, "%-14s %d row(s)\n", name, result.Rows[name])
	}
	fmt.Fprintf(stdout, "\n%s\n", result.URL)
	return 0
}

// sheetDryRun answers "what would my friends see" without a network call.
func sheetDryRun(conn *sql.DB, configErr string) int {
	counts, err := gsheet.Counts(conn)
	if err != nil {
		return fail("%v", err)
	}
	fmt.Fprintln(stdout, "would push:")
	for _, name := range sortedNames(counts) {
		fmt.Fprintf(stdout, "  %-14s %d row(s)\n", name, counts[name])
	}
	fmt.Fprintln(stdout, "\nnot pushed: Contacts (other people's details stay on this machine)")
	if configErr != "" {
		fmt.Fprintf(stdout, "\nnot configured yet: %s\n", configErr)
	}
	return 0
}

func sortedNames(m map[string]int) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
