// Package cli is the command surface. Each command opens the database, does one
// thing, and returns a process exit code; nothing here holds state between calls.
package cli

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Vaivaswat2244/job-tracker/internal/db"
)

const usageText = `tracker — job application pipeline

  add-job <url>              fetch JD, create company+job rows, print job id
  apply <job_id>             status=applied, follow-up in +5 business days
  status <app_id> <status>   manual transition
  contact add <company_id>   interactive contact entry
  list                       active applications, sorted by followup_due
  jobs                       browse every role poll has ingested
  export                     write tracker.xlsx
  sheet push                 push Pipeline + Applications to the Google Sheet
  sheet setup                how to connect a Google Sheet
  mail poll                  read Gmail for application confirmations/rejections
  mail list                  messages the ingest could not place, awaiting a call
  mail auth|setup            connect Gmail, read-only

  watchlist add <careers_url>  detect the ATS behind a careers page and add it
  watchlist list               watchlist with poll state
  poll                         poll watchlist ATS boards for new roles
  funding poll                 check funding sources for new rounds
  candidate list|approve|reject  review companies suggested by funding signals
  renormalize                  re-run scoring heuristics over stored jobs
  digest                       alerts, funding window, new roles
  followups                    run the follow-up ladder (the 09:30 timer)

Run any command with -h for its flags.
`

// Main dispatches argv and returns the process exit code.
func Main(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, usageText)
		return 2
	}

	cmd, rest := argv[0], argv[1:]
	switch cmd {
	case "add-job":
		return withDB(rest, addJob)
	case "apply":
		return withDB(rest, apply)
	case "status":
		return withDB(rest, status)
	case "contact":
		return withDB(rest, contact)
	case "list":
		return withDB(rest, list)
	case "jobs":
		return withDB(rest, jobsCmd)
	case "export":
		return withDB(rest, exportCmd)
	case "sheet":
		return withDB(rest, sheetCmd)
	case "mail":
		return withDB(rest, mailCmd)
	case "watchlist":
		return withDB(rest, watchlistCmd)
	case "poll":
		return withDB(rest, pollCmd)
	case "funding":
		return withDB(rest, fundingCmd)
	case "candidate":
		return withDB(rest, candidateCmd)
	case "renormalize":
		return withDB(rest, renormalize)
	case "digest":
		return withDB(rest, digestCmd)
	case "followups":
		return withDB(rest, followupsCmd)
	case "-h", "--help", "help":
		fmt.Print(usageText)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usageText)
		return 2
	}
}

// withDB opens the database for one command. Commands take their own args so
// each can define its own flags.
func withDB(args []string, fn func(*sql.DB, []string) int) int {
	conn, err := db.Connect("")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer conn.Close()
	return fn(conn, args)
}

// fail prints to stderr and returns 1, so call sites read `return fail(...)`.
func fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	return 1
}

// newFlags builds a FlagSet that prints to stderr and does not os.Exit.
func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// parse binds flags and returns the positional arguments.
//
// The stdlib flag package stops parsing at the first non-flag argument, so
// `add-job <url> --no-fetch` would silently ignore --no-fetch. argparse permutes
// instead, and every documented invocation puts the positional first, so parse
// loops: take a positional, keep parsing what follows. ok is false when the
// flags were bad or -h was asked for.
func parse(fs *flag.FlagSet, args []string) (positional []string, ok bool) {
	for {
		if err := fs.Parse(args); err != nil {
			return nil, false
		}
		if fs.NArg() == 0 {
			return positional, true
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func atoi64(value, label string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number, got %q", label, value)
	}
	return n, nil
}

func oneOf(value string, allowed []string) bool {
	for _, a := range allowed {
		if a == value {
			return true
		}
	}
	return false
}

// trunc and pad count runes, not bytes, so an em dash in a job title does not
// shift a column the way %-*s would.
func trunc(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func pad(s string, n int) string {
	s = trunc(s, n)
	if gap := n - utf8.RuneCountInString(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

func padLeft(s string, n int) string {
	if gap := n - utf8.RuneCountInString(s); gap > 0 {
		return strings.Repeat(" ", gap) + s
	}
	return s
}

// dash draws a table rule.
func dash(n int) string { return strings.Repeat("-", n) }

func nullOr(v sql.NullString, fallback string) string {
	if v.Valid && v.String != "" {
		return v.String
	}
	return fallback
}

// nilIfEmpty writes SQL NULL for an absent value rather than an empty string,
// so `IS NULL` checks keep working.
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

var stdout io.Writer = os.Stdout
