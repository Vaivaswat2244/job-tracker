package cli

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Vaivaswat2244/job-tracker/internal/ats"
	"github.com/Vaivaswat2244/job-tracker/internal/db"
	"github.com/Vaivaswat2244/job-tracker/internal/digest"
	"github.com/Vaivaswat2244/job-tracker/internal/fetch"
	"github.com/Vaivaswat2244/job-tracker/internal/followup"
	"github.com/Vaivaswat2244/job-tracker/internal/funding"
	"github.com/Vaivaswat2244/job-tracker/internal/ingest"
	"github.com/Vaivaswat2244/job-tracker/internal/notify"
	"github.com/Vaivaswat2244/job-tracker/internal/poll"
	"github.com/Vaivaswat2244/job-tracker/internal/watchlist"
)

// ---------------------------------------------------------------- watchlist

func watchlistCmd(conn *sql.DB, args []string) int {
	if len(args) == 0 {
		return fail("usage: tracker watchlist add <careers_url> | tracker watchlist list")
	}
	switch args[0] {
	case "add":
		return watchlistAdd(conn, args[1:])
	case "list":
		return watchlistList(conn, args[1:])
	default:
		return fail("unknown watchlist command %q (add, list)", args[0])
	}
}

func watchlistAdd(conn *sql.DB, args []string) int {
	fs := newFlags("watchlist add")
	name := fs.String("name", "", "company name (default: inferred from the URL)")
	domain := fs.String("domain", "", "company domain")
	atsName := fs.String("ats", "", "skip detection: "+strings.Join(db.ATSProviders, "|"))
	slug := fs.String("slug", "", "board slug, with --ats")
	source := fs.String("source", "manual", "where this company came from")
	ossRepo := fs.String("oss-repo", "", "public repo for the contributor path")
	priority := fs.String("priority", "normal", strings.Join(db.Priorities, "|"))
	noDetect := fs.Bool("no-detect", false, "do not fetch the page")
	pos, ok := parse(fs, args)
	if !ok {
		return 2
	}
	if len(pos) != 1 {
		return fail("usage: tracker watchlist add <careers_url> [flags]")
	}
	url := strings.TrimSpace(pos[0])

	if *atsName != "" && !oneOf(*atsName, db.ATSProviders) {
		return fail("--ats must be one of %s", strings.Join(db.ATSProviders, ", "))
	}
	if !oneOf(*priority, db.Priorities) {
		return fail("--priority must be one of %s", strings.Join(db.Priorities, ", "))
	}

	entries, err := watchlist.Load("")
	if err != nil {
		return fail("%v", err)
	}

	result := ats.Detection{ATS: "unknown"}
	if !*noDetect && *atsName == "" {
		result = ats.Detect(url)
	}
	if *atsName != "" {
		result = ats.Detection{ATS: *atsName, Slug: *slug, Evidence: "manual"}
	}

	companyName := firstNonEmpty(*name, fetch.CompanyFromURL(url))
	companyDomain := firstNonEmpty(*domain, fetch.DomainFromURL(url))

	if _, found := watchlist.Find(entries, companyName, companyDomain); found {
		fmt.Printf("already on the watchlist: %s\n", companyName)
		return 0
	}

	enabled := true
	entry := watchlist.Entry{
		Name: companyName, Domain: companyDomain, ATS: result.ATS, Slug: result.Slug,
		CareersURL: url, Source: *source, OSSRepo: *ossRepo, Priority: *priority,
		Enabled: &enabled,
	}
	// INV-1: detection failure never blocks the add. An entry sitting at
	// ats=unknown is visible and fixable; a company the user believed they
	// added and didn't is not recoverable.
	if err := watchlist.Append(entry, ""); err != nil {
		return fail("%v", err)
	}
	if _, _, err := watchlist.Sync(conn, ""); err != nil {
		return fail("%v", err)
	}

	if result.Found() {
		fmt.Printf("added %s: %s/%s (detected via %s)\n",
			companyName, result.ATS, result.Slug, result.Evidence)
		return 0
	}
	reason := result.Error
	if reason == "" {
		reason = "no match"
	}
	fmt.Fprintf(os.Stderr, "added %s with ats: unknown\n", companyName)
	fmt.Fprintf(os.Stderr, "warn: could not detect an ATS board — %s.\n"+
		"      It will not be polled until you set `ats` and `slug` in %s.\n",
		reason, watchlist.Path())
	return 0
}

func watchlistList(conn *sql.DB, args []string) int {
	fs := newFlags("watchlist list")
	if _, ok := parse(fs, args); !ok {
		return 2
	}
	if _, _, err := watchlist.Sync(conn, ""); err != nil {
		return fail("%v", err)
	}

	rows, err := conn.Query("SELECT " + db.CompanyColumns +
		" FROM companies WHERE watchlist_enabled = 1 ORDER BY name")
	if err != nil {
		return fail("select watchlist: %v", err)
	}
	defer rows.Close()

	var companies []db.Company
	for rows.Next() {
		c, err := db.ScanCompany(rows)
		if err != nil {
			return fail("scan company: %v", err)
		}
		companies = append(companies, c)
	}
	if err := rows.Err(); err != nil {
		return fail("iterate watchlist: %v", err)
	}
	if len(companies) == 0 {
		fmt.Printf("watchlist is empty — add companies to %s\n", watchlist.Path())
		return 0
	}

	openAlerts := map[string]bool{}
	alertRows, err := conn.Query(
		"SELECT target_id FROM alerts WHERE resolved_at IS NULL AND target_type = 'company'")
	if err != nil {
		return fail("select alerts: %v", err)
	}
	for alertRows.Next() {
		var id string
		if err := alertRows.Scan(&id); err != nil {
			alertRows.Close()
			return fail("scan alert: %v", err)
		}
		openAlerts[id] = true
	}
	alertRows.Close()

	now := time.Now().UTC()
	fmt.Printf("%s  %s %s %s %s %s flag\n", padLeft("id", 4), pad("company", 24),
		pad("ats", 11), pad("slug", 24), pad("prio", 6), pad("last polled", 21))
	fmt.Println(dash(104))
	for _, c := range companies {
		flag := ""
		if openAlerts[fmt.Sprint(c.ID)] {
			flag = "STALE"
		}
		if c.ATSOr() == "unknown" {
			flag = strings.TrimSpace(flag + " needs-slug")
		}
		fmt.Printf("%s  %s %s %s %s %s %s\n",
			padLeft(fmt.Sprint(c.ID), 4), pad(c.Name, 24), pad(nullOr(c.ATS, "-"), 11),
			pad(nullOr(c.Slug, "-"), 24), pad(watchlist.EffectivePriority(c, now), 6),
			pad(nullOr(c.LastPolledAt, "never"), 21), flag)
	}
	fmt.Printf("\n%d company(ies).\n", len(companies))
	return 0
}

// ---------------------------------------------------------------- poll

func pollCmd(conn *sql.DB, args []string) int {
	fs := newFlags("poll")
	only := fs.String("only", "", "restrict to one company (name or slug)")
	force := fs.Bool("force", false, "ignore the poll cadence")
	workers := fs.Int("workers", poll.Workers, "concurrent fetches")
	if _, ok := parse(fs, args); !ok {
		return 2
	}
	if _, err := poll.Run(conn, poll.Options{
		Only: *only, Force: *force, Workers: *workers, Verbose: true,
		Stdout: os.Stdout, Stderr: os.Stderr, Now: time.Now().UTC(),
	}); err != nil {
		return fail("%v", err)
	}
	return 0
}

// ---------------------------------------------------------------- funding

func fundingCmd(conn *sql.DB, args []string) int {
	if len(args) == 0 || args[0] != "poll" {
		return fail("usage: tracker funding poll [flags]")
	}
	fs := newFlags("funding poll")
	only := fs.String("only", "", "restrict to one source")
	noResolve := fs.Bool("no-resolve", false,
		"skip article fetches used for domain resolution")
	resolveLimit := fs.Int("resolve-limit", 15, "max article fetches this run")
	if _, ok := parse(fs, args[1:]); !ok {
		return 2
	}
	if _, err := funding.Run(conn, funding.RunOptions{
		Only: *only, ResolveLimit: *resolveLimit, DoNetwork: !*noResolve,
		Verbose: true, Stdout: os.Stdout, Stderr: os.Stderr,
	}); err != nil {
		return fail("%v", err)
	}
	return 0
}

// ---------------------------------------------------------------- candidate

func candidateCmd(conn *sql.DB, args []string) int {
	if len(args) == 0 {
		return fail("usage: tracker candidate list|approve <id>|reject <id>")
	}
	switch args[0] {
	case "list":
		return candidateList(conn, args[1:])
	case "approve", "reject":
		return candidateDecide(conn, args[0], args[1:])
	default:
		return fail("unknown candidate command %q (list, approve, reject)", args[0])
	}
}

func candidateList(conn *sql.DB, args []string) int {
	fs := newFlags("candidate list")
	statusFlag := fs.String("status", "needs_review", strings.Join(db.CandidateStatuses, "|"))
	if _, ok := parse(fs, args); !ok {
		return 2
	}
	if !oneOf(*statusFlag, db.CandidateStatuses) {
		return fail("--status must be one of %s", strings.Join(db.CandidateStatuses, ", "))
	}

	rows, err := conn.Query(
		"SELECT id, name, domain, round_stage, amount_raw, resolved_ats, resolved_slug,"+
			" reason, article_url FROM watchlist_candidates WHERE status = ?"+
			" ORDER BY announced_at DESC, id DESC", *statusFlag)
	if err != nil {
		return fail("select candidates: %v", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var (
			id                                   int64
			name                                 string
			domain, stage, amount, atsName, slug sql.NullString
			reason, articleURL                   sql.NullString
		)
		if err := rows.Scan(&id, &name, &domain, &stage, &amount,
			&atsName, &slug, &reason, &articleURL); err != nil {
			return fail("scan candidate: %v", err)
		}
		detected := "ats unknown"
		if slug.Valid && slug.String != "" {
			detected = fmt.Sprintf("%s/%s", atsName.String, slug.String)
		}
		fmt.Printf("[%s] %s (%s) — %s, %s — %s\n", padLeft(fmt.Sprint(id), 3), name,
			nullOr(domain, "no domain"), nullOr(stage, "?"),
			nullOr(amount, "undisclosed"), detected)
		if reason.Valid && reason.String != "" {
			fmt.Printf("      %s\n", reason.String)
		}
		if articleURL.Valid && articleURL.String != "" {
			fmt.Printf("      %s\n", articleURL.String)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return fail("iterate candidates: %v", err)
	}
	if count == 0 {
		fmt.Printf("no candidates with status '%s'.\n", *statusFlag)
		return 0
	}
	fmt.Printf("\n%d candidate(s).\n", count)
	return 0
}

func candidateDecide(conn *sql.DB, action string, args []string) int {
	fs := newFlags("candidate " + action)
	pos, ok := parse(fs, args)
	if !ok {
		return 2
	}
	if len(pos) != 1 {
		return fail("usage: tracker candidate %s <candidate_id>", action)
	}
	id, err := atoi64(pos[0], "candidate_id")
	if err != nil {
		return fail("%v", err)
	}

	var (
		applied bool
		message string
	)
	if action == "approve" {
		applied, message, err = funding.Approve(conn, id, "")
	} else {
		applied, message, err = funding.Reject(conn, id)
	}
	if err != nil {
		return fail("%v", err)
	}
	if !applied {
		return fail("%s", message)
	}
	fmt.Println(message)
	return 0
}

// ---------------------------------------------------------------- renormalize

func renormalize(conn *sql.DB, args []string) int {
	fs := newFlags("renormalize")
	if _, ok := parse(fs, args); !ok {
		return 2
	}
	n, err := ingest.Renormalize(conn)
	if err != nil {
		return fail("%v", err)
	}
	fmt.Printf("re-derived comp_model/auth_required/hires_in_india for %d job(s)\n", n)
	return 0
}

// ---------------------------------------------------------------- digest

func digestCmd(conn *sql.DB, args []string) int {
	fs := newFlags("digest")
	hours := fs.Int("hours", 48, "new-role lookback window")
	wantNotify := fs.Bool("notify", false, "also fire a desktop notification")
	if _, ok := parse(fs, args); !ok {
		return 2
	}
	text, err := digest.Build(conn, *hours, time.Now().UTC())
	if err != nil {
		return fail("%v", err)
	}
	fmt.Println(text)
	if *wantNotify {
		first := text
		if parts := strings.SplitN(text, "\n\n", 3); len(parts) > 1 {
			first = parts[1]
		}
		notify.Send("Job pipeline digest", trunc(first, 400), "normal")
	}
	return 0
}

// ---------------------------------------------------------------- followups

func followupsCmd(conn *sql.DB, args []string) int {
	fs := newFlags("followups")
	dryRun := fs.Bool("dry-run", false, "notify but record nothing")
	if _, ok := parse(fs, args); !ok {
		return 2
	}
	if _, err := followup.Run(conn, followup.Options{
		Now: time.Now(), DryRun: *dryRun, Stdout: os.Stdout,
	}); err != nil {
		return fail("%v", err)
	}
	return 0
}
