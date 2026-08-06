// Package poll polls the watchlist.
//
// Network work is fanned out across a small worker pool; every database write
// happens back on the calling goroutine. Serialising the writes costs nothing
// next to the HTTP time, and it keeps the single SQLite connection uncontended.
package poll

import (
	"database/sql"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/Vaivaswat2244/job-tracker/internal/ats"
	"github.com/Vaivaswat2244/job-tracker/internal/db"
	"github.com/Vaivaswat2244/job-tracker/internal/health"
	"github.com/Vaivaswat2244/job-tracker/internal/ingest"
	"github.com/Vaivaswat2244/job-tracker/internal/watchlist"
)

const Workers = 5

// Alert pairs a company label with why its feed is unhealthy.
type Alert struct {
	Label  string
	Reason string
}

// Summary is the tally one run reports.
type Summary struct {
	Polled         int
	SkippedCadence int
	New            int
	Updated        int
	NotModified    int
	Failed         int
	Alerts         []Alert
}

// Options controls one run. Stdout and Stderr are injected so the CLI can wire
// real streams and tests can capture them.
type Options struct {
	Only    string
	Force   bool
	Workers int
	Verbose bool
	Stdout  io.Writer
	Stderr  io.Writer
	Now     time.Time
}

func selectCompanies(conn *sql.DB, opts Options) ([]db.Company, int, error) {
	rows, err := watchlist.Pollable(conn)
	if err != nil {
		return nil, 0, err
	}

	if opts.Only != "" {
		needle := strings.ToLower(strings.TrimSpace(opts.Only))
		var filtered []db.Company
		for _, r := range rows {
			if strings.Contains(strings.ToLower(r.Name), needle) ||
				needle == strings.ToLower(r.Slug.String) {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}

	if opts.Force {
		return rows, 0, nil
	}
	var due []db.Company
	for _, r := range rows {
		if watchlist.DueForPoll(r, opts.Now) {
			due = append(due, r)
		}
	}
	return due, len(rows) - len(due), nil
}

// fetchOne runs one adapter. A failure becomes a FetchResult rather than an
// error: a company that breaks on every parse must still register as a failed
// poll, or it would never trip feed-death detection.
func fetchOne(c db.Company) ats.FetchResult {
	adapter := ats.For(c.ATSOr())
	if adapter == nil {
		return ats.FetchResult{Error: fmt.Sprintf("no adapter for ats=%s", c.ATSOr())}
	}
	return adapter.Fetch(c.Slug.String, c.PollETag.String, c.PollLastModified.String)
}

func Run(conn *sql.DB, opts Options) (Summary, error) {
	if opts.Workers <= 0 {
		opts.Workers = Workers
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}

	var summary Summary
	if _, _, err := watchlist.Sync(conn, ""); err != nil {
		return summary, err
	}

	rows, skipped, err := selectCompanies(conn, opts)
	if err != nil {
		return summary, err
	}
	summary.SkippedCadence = skipped

	if len(rows) == 0 {
		if opts.Verbose {
			fmt.Fprintf(opts.Stdout,
				"nothing due (%d within cadence). --force to poll anyway.\n", skipped)
		}
		return summary, nil
	}

	results := fanOut(rows, opts.Workers)

	for i, c := range rows {
		result := results[i]
		label := c.Name
		summary.Polled++

		reason, err := health.PollHealth(conn, c.ID, health.PollOutcome{
			JobsPresent: result.Jobs != nil,
			JobCount:    len(result.Jobs),
			NotModified: result.NotModified,
			Status:      result.Status,
			OK:          result.OK(),
			Error:       result.Error,
		}, label)
		if err != nil {
			return summary, err
		}
		if reason != "" {
			summary.Alerts = append(summary.Alerts, Alert{label, reason})
		}

		switch {
		case result.NotModified:
			summary.NotModified++
			if err := watchlist.MarkPolled(conn, c.ID,
				c.PollETag.String, c.PollLastModified.String); err != nil {
				return summary, err
			}
			if opts.Verbose {
				fmt.Fprintf(opts.Stdout, "  %-24s 304 not modified\n", label)
			}

		case !result.OK():
			summary.Failed++
			if err := watchlist.MarkPolled(conn, c.ID,
				c.PollETag.String, c.PollLastModified.String); err != nil {
				return summary, err
			}
			fmt.Fprintf(opts.Stderr, "  %-24s FAILED  %s\n", label, result.Error)

		default:
			stats, err := ingest.Ingest(conn, c.ID, label, result.Jobs)
			if err != nil {
				return summary, err
			}
			summary.New += stats.New
			summary.Updated += stats.Updated
			if err := watchlist.MarkPolled(conn, c.ID,
				result.ETag, result.LastModified); err != nil {
				return summary, err
			}
			if opts.Verbose {
				fmt.Fprintf(opts.Stdout, "  %-24s %3d jobs  (+%d new, %d updated)\n",
					label, len(result.Jobs), stats.New, stats.Updated)
			}
		}
	}

	if opts.Verbose {
		fmt.Fprintf(opts.Stdout,
			"\npolled %d, %d new, %d updated, %d unchanged, %d failed, %d within cadence\n",
			summary.Polled, summary.New, summary.Updated, summary.NotModified,
			summary.Failed, summary.SkippedCadence)
		for _, a := range summary.Alerts {
			fmt.Fprintf(opts.Stderr, "  ALERT stale_feed: %s (%s)\n", a.Label, a.Reason)
		}
	}
	return summary, nil
}

// fanOut fetches every board concurrently, returning results positionally so
// the caller processes them in watchlist order regardless of completion order.
func fanOut(rows []db.Company, workers int) []ats.FetchResult {
	results := make([]ats.FetchResult, len(rows))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for i, c := range rows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = fetchOne(c)
		}()
	}
	wg.Wait()
	return results
}
