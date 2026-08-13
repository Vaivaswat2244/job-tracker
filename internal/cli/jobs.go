package cli

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/Vaivaswat2244/job-tracker/internal/dates"
	"github.com/Vaivaswat2244/job-tracker/internal/jobs"
)

// jobsCmd browses what `poll` ingested. `list` deliberately shows applications
// only; without this the 2000+ collected roles are reachable only by SQL.
func jobsCmd(conn *sql.DB, args []string) int {
	fs := newFlags("jobs")
	company := fs.String("company", "", "filter by company substring")
	title := fs.String("title", "", "filter by title substring")
	india := fs.Bool("india", false, "only roles marked hires_in_india")
	remote := fs.Bool("remote", false, "only roles the board marked remote")
	since := fs.String("since", "", "lookback: 7d, 48h, 2w, or 2026-08-01")
	limit := fs.Int("limit", 50, "max rows (0 for all)")
	dupes := fs.Bool("dupes", false, "include rows linked to a canonical duplicate")
	early := fs.Bool("early", false,
		"engineering (or unclassified) work at intern/junior level — the target filter")
	function := fs.String("function", "", "engineering | other | unknown")
	level := fs.String("level", "", "intern | junior | mid | senior | unknown")
	urls := fs.Bool("urls", false, "print the apply URL under each row")
	if _, ok := parse(fs, args); !ok {
		return 2
	}

	cutoff, err := dates.ParseSince(*since, time.Now().UTC())
	if err != nil {
		return fail("%v", err)
	}

	filter := jobs.Filter{
		Company: *company, Title: *title, IndiaOnly: *india, RemoteOnly: *remote,
		Since: cutoff, IncludeDuplicates: *dupes, Limit: *limit,
		EarlyCareer: *early, Function: *function, Level: *level,
	}
	rows, err := jobs.List(conn, filter)
	if err != nil {
		return fail("%v", err)
	}
	if len(rows) == 0 {
		fmt.Println("no jobs match — `poll` to ingest, or widen the filter.")
		return 0
	}
	total, err := filter.Count(conn)
	if err != nil {
		return fail("%v", err)
	}

	fmt.Printf("%s  %s %s %s %s %s\n", padLeft("id", 5), pad("company", 16),
		pad("title", 44), pad("location", 24), pad("seen", 10), "flags")
	fmt.Println(dash(112))
	for _, r := range rows {
		seen := r.Seen
		if d := dates.ParseDay(seen); !d.IsZero() {
			seen = d.Format(dayFormat)
		}
		fmt.Printf("%s  %s %s %s %s %s\n",
			padLeft(fmt.Sprint(r.ID), 5), pad(r.Company, 16), pad(r.Title, 44),
			pad(nullOr(r.Location, "-"), 24), pad(seen, 10), r.Flags())
		if *urls {
			fmt.Printf("       %s\n", r.URL)
		}
	}

	if len(rows) < total {
		fmt.Printf("\n%d of %d job(s). --limit 0 for all.\n", len(rows), total)
	} else {
		fmt.Printf("\n%d job(s).\n", len(rows))
	}
	return 0
}
