package cli

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Vaivaswat2244/job-tracker/internal/dates"
	"github.com/Vaivaswat2244/job-tracker/internal/db"
	"github.com/Vaivaswat2244/job-tracker/internal/export"
	"github.com/Vaivaswat2244/job-tracker/internal/fetch"
	"github.com/Vaivaswat2244/job-tracker/internal/ingest"
)

const untitled = "(untitled — set with `status`/manual edit)"
const dayFormat = "2006-01-02"

// ---------------------------------------------------------------- add-job

func addJob(conn *sql.DB, args []string) int {
	fs := newFlags("add-job")
	company := fs.String("company", "", "company name (default: inferred from the URL)")
	title := fs.String("title", "", "job title (default: from the page)")
	source := fs.String("source", "manual", "where this job came from")
	notes := fs.String("notes", "", "free-text note")
	noFetch := fs.Bool("no-fetch", false, "skip the HTTP fetch entirely")
	pos, ok := parse(fs, args)
	if !ok {
		return 2
	}
	if len(pos) != 1 {
		return fail("usage: tracker add-job <url> [flags]")
	}
	url := strings.TrimSpace(pos[0])

	var (
		existingID    int64
		existingTitle string
	)
	err := conn.QueryRow("SELECT id, title FROM jobs WHERE url = ?", url).
		Scan(&existingID, &existingTitle)
	if err == nil {
		fmt.Printf("already tracked: job %d  %s\n", existingID, existingTitle)
		return 0
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fail("look up job by url: %v", err)
	}

	var pageTitle, jdText, fetchErr string
	if !*noFetch {
		pageTitle, jdText, fetchErr = fetch.JD(url)
	}
	if fetchErr != "" {
		// Losing the row is worse than losing the description.
		fmt.Fprintf(os.Stderr,
			"warn: JD fetch failed (%s); saving row with empty jd_text\n", fetchErr)
	}

	jobTitle := firstNonEmpty(*title, pageTitle, untitled)
	companyName := firstNonEmpty(*company, fetch.CompanyFromURL(url))
	companyID, err := ingest.GetOrCreateCompany(conn, companyName, fetch.DomainFromURL(url))
	if err != nil {
		return fail("%v", err)
	}

	sourceURLs, _ := json.Marshal([]string{url})
	res, err := conn.Exec(
		"INSERT INTO jobs (company_id, title, url, source, seen_at, jd_text, source_urls)"+
			" VALUES (?,?,?,?,?,?,?)",
		companyID, jobTitle, url, nilIfEmpty(*source), db.Now(), jdText, string(sourceURLs))
	if err != nil {
		return fail("insert job: %v", err)
	}
	jobID, err := res.LastInsertId()
	if err != nil {
		return fail("job id: %v", err)
	}
	if _, err := conn.Exec(
		"INSERT INTO applications (job_id, status, notes) VALUES (?, 'found', ?)",
		jobID, nilIfEmpty(*notes)); err != nil {
		return fail("insert application: %v", err)
	}

	fmt.Printf("job %d  %s — %s\n", jobID, companyName, jobTitle)
	if jdText == "" && !*noFetch {
		fmt.Println("      (no jd_text captured)")
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------- apply

func apply(conn *sql.DB, args []string) int {
	fs := newFlags("apply")
	resume := fs.String("resume", "", "resume version used")
	pos, ok := parse(fs, args)
	if !ok {
		return 2
	}
	if len(pos) != 1 {
		return fail("usage: tracker apply <job_id> [--resume v]")
	}
	jobID, err := atoi64(pos[0], "job_id")
	if err != nil {
		return fail("%v", err)
	}

	var jobTitle, companyName string
	err = conn.QueryRow(
		"SELECT j.title, c.name FROM jobs j JOIN companies c ON c.id = j.company_id"+
			" WHERE j.id = ?", jobID).Scan(&jobTitle, &companyName)
	if errors.Is(err, sql.ErrNoRows) {
		return fail("no job with id %d", jobID)
	}
	if err != nil {
		return fail("look up job: %v", err)
	}

	appliedOn := time.Now()
	due := dates.AddBusinessDays(appliedOn, dates.FollowupBusinessDays)
	appliedStr, dueStr := appliedOn.Format(dayFormat), due.Format(dayFormat)

	var appID int64
	err = conn.QueryRow("SELECT id FROM applications WHERE job_id = ?", jobID).Scan(&appID)
	switch {
	case err == nil:
		if _, err := conn.Exec(
			"UPDATE applications SET status='applied', applied_at=?, followup_due=?,"+
				" resume_version=COALESCE(?, resume_version) WHERE id = ?",
			appliedStr, dueStr, nilIfEmpty(*resume), appID); err != nil {
			return fail("update application: %v", err)
		}
	case errors.Is(err, sql.ErrNoRows):
		res, err := conn.Exec(
			"INSERT INTO applications (job_id, applied_at, status, followup_due, resume_version)"+
				" VALUES (?,?, 'applied', ?, ?)",
			jobID, appliedStr, dueStr, nilIfEmpty(*resume))
		if err != nil {
			return fail("insert application: %v", err)
		}
		if appID, err = res.LastInsertId(); err != nil {
			return fail("application id: %v", err)
		}
	default:
		return fail("look up application: %v", err)
	}

	// A fresh application restarts the ladder.
	if _, err := conn.Exec(
		"DELETE FROM followup_notices WHERE application_id = ?", appID); err != nil {
		return fail("clear notices: %v", err)
	}

	fmt.Printf("app %d  applied %s  %s — %s\n", appID, appliedStr, companyName, jobTitle)
	fmt.Printf("          follow-up due %s (+%d business days)\n",
		dueStr, dates.FollowupBusinessDays)
	return 0
}

// ---------------------------------------------------------------- status

func status(conn *sql.DB, args []string) int {
	fs := newFlags("status")
	notes := fs.String("notes", "", "replace the application's notes")
	pos, ok := parse(fs, args)
	if !ok {
		return 2
	}
	if len(pos) != 2 {
		return fail("usage: tracker status <app_id> <%s>", strings.Join(db.Statuses, "|"))
	}
	appID, err := atoi64(pos[0], "app_id")
	if err != nil {
		return fail("%v", err)
	}
	newStatus := pos[1]
	if !oneOf(newStatus, db.Statuses) {
		fmt.Fprintf(os.Stderr, "unknown status %q. one of: %s\n",
			newStatus, strings.Join(db.Statuses, ", "))
		return 2
	}

	var current string
	err = conn.QueryRow("SELECT status FROM applications WHERE id = ?", appID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return fail("no application with id %d", appID)
	}
	if err != nil {
		return fail("look up application: %v", err)
	}

	if _, err := conn.Exec(
		"UPDATE applications SET status = ? WHERE id = ?", newStatus, appID); err != nil {
		return fail("update status: %v", err)
	}
	if newStatus == "followed_up" {
		// A nudge restarts the clock rather than leaving the row permanently overdue.
		next := dates.AddBusinessDays(time.Now(), dates.FollowupBusinessDays)
		if _, err := conn.Exec("UPDATE applications SET followup_due = ? WHERE id = ?",
			next.Format(dayFormat), appID); err != nil {
			return fail("reschedule follow-up: %v", err)
		}
	}
	if oneOf(newStatus, db.ClosedStatuses) || newStatus == "offer" {
		if _, err := conn.Exec(
			"UPDATE applications SET followup_due = NULL WHERE id = ?", appID); err != nil {
			return fail("clear follow-up: %v", err)
		}
	}
	if _, err := conn.Exec(
		"DELETE FROM followup_notices WHERE application_id = ?", appID); err != nil {
		return fail("clear notices: %v", err)
	}
	if *notes != "" {
		if _, err := conn.Exec("UPDATE applications SET notes = ? WHERE id = ?",
			*notes, appID); err != nil {
			return fail("update notes: %v", err)
		}
	}

	fmt.Printf("app %d: %s -> %s\n", appID, current, newStatus)
	return 0
}

// ---------------------------------------------------------------- contact add

// prompt asks interactively, falling back to empty when there is no stdin to
// read (scripted use, systemd) so a non-tty run fails loudly instead of hanging.
type prompter struct{ r *bufio.Reader }

func (p prompter) ask(label string, required bool) (string, error) {
	for {
		fmt.Printf("%s: ", label)
		line, err := p.r.ReadString('\n')
		value := strings.TrimSpace(line)
		if errors.Is(err, io.EOF) && value == "" {
			if required {
				return "", fmt.Errorf("\n%s is required — pass it as a flag",
					strings.TrimSpace(label))
			}
			fmt.Println()
			return "", nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		if value != "" || !required {
			return value, nil
		}
		fmt.Println("  required.")
	}
}

func contact(conn *sql.DB, args []string) int {
	if len(args) == 0 || args[0] != "add" {
		return fail("usage: tracker contact add <company_id> [flags]")
	}
	return contactAdd(conn, args[1:])
}

func contactAdd(conn *sql.DB, args []string) int {
	fs := newFlags("contact add")
	name := fs.String("name", "", "contact name")
	title := fs.String("title", "", "contact title")
	email := fs.String("email", "", "contact email")
	linkedin := fs.String("linkedin", "", "linkedin url")
	source := fs.String("source", "", "where this contact came from")
	confidence := fs.String("confidence", "",
		strings.Join(db.EmailConfidence, "|")+" — inferred is draft-only")
	pos, ok := parse(fs, args)
	if !ok {
		return 2
	}
	if len(pos) != 1 {
		return fail("usage: tracker contact add <company_id> [flags]")
	}
	companyID, err := atoi64(pos[0], "company_id")
	if err != nil {
		return fail("%v", err)
	}

	var (
		companyName string
		companyDom  sql.NullString
	)
	err = conn.QueryRow("SELECT name, domain FROM companies WHERE id = ?", companyID).
		Scan(&companyName, &companyDom)
	if errors.Is(err, sql.ErrNoRows) {
		return fail("no company with id %d", companyID)
	}
	if err != nil {
		return fail("look up company: %v", err)
	}

	fmt.Printf("contact for %s (id %d)\n", companyName, companyID)
	p := prompter{bufio.NewReader(os.Stdin)}
	// askErr carries the first prompt failure; a required field with no stdin
	// behind it must abort rather than silently insert a half-built contact.
	var askErr error
	ask := func(flagValue, label string, required bool) string {
		if flagValue != "" || askErr != nil {
			return flagValue
		}
		v, err := p.ask(label, required)
		if err != nil {
			askErr = err
			return ""
		}
		return v
	}

	// INV-2: name + title + company_id are mandatory before any draft can exist.
	contactName := ask(*name, "  name", true)
	contactTitle := ask(*title, "  title", true)
	addr := strings.ToLower(strings.TrimSpace(ask(*email, "  email (blank ok)", false)))

	conf := *confidence
	if addr != "" && conf == "" {
		conf = ask("", "  confidence [published|verified|inferred]", true)
	}
	if addr != "" && !oneOf(conf, db.EmailConfidence) {
		return fail("confidence must be one of %s", strings.Join(db.EmailConfidence, ", "))
	}
	linkedinURL := ask(*linkedin, "  linkedin url (blank ok)", false)
	src := ask(*source, "  source (careers page / github / manual / referral)", false)
	if askErr != nil {
		return fail("%v", askErr)
	}

	// Name-collision guard: an address on the wrong domain is how you email a stranger.
	if addr != "" && companyDom.Valid && companyDom.String != "" {
		emailDomain := addr[strings.LastIndex(addr, "@")+1:]
		known := strings.TrimPrefix(companyDom.String, "www.")
		if emailDomain != "" && !(emailDomain == known ||
			strings.HasSuffix(emailDomain, "."+known) ||
			strings.HasSuffix(known, "."+emailDomain)) {
			payload, _ := json.Marshal(map[string]any{
				"company_id": companyID, "company_domain": companyDom.String,
				"name": contactName, "title": contactTitle, "email": addr,
				"confidence": conf, "source": src,
			})
			if err := db.LogExclusion(conn, string(payload),
				fmt.Sprintf("email domain '%s' does not match company domain '%s'",
					emailDomain, known),
				"contact.domain_mismatch"); err != nil {
				return fail("%v", err)
			}
			fmt.Fprintf(os.Stderr, "refused: %s is not on %s. Contact saved without the "+
				"address; payload in excluded_log (rule_id=contact.domain_mismatch) "+
				"for manual review.\n", addr, known)
			addr, conf = "", ""
		}
	}

	res, err := conn.Exec(
		"INSERT INTO contacts (company_id, name, title, email, email_confidence,"+
			" linkedin_url, source) VALUES (?,?,?,?,?,?,?)",
		companyID, contactName, contactTitle, nilIfEmpty(addr), nilIfEmpty(conf),
		nilIfEmpty(linkedinURL), nilIfEmpty(src))
	if err != nil {
		return fail("insert contact: %v", err)
	}
	contactID, _ := res.LastInsertId()

	shown := "no email"
	switch {
	case conf == "inferred":
		shown = fmt.Sprintf("[UNVERIFIED: %s]", addr)
	case addr != "":
		shown = addr
	}
	fmt.Printf("contact %d  %s — %s  %s\n", contactID, contactName, contactTitle, shown)
	if conf == "inferred" {
		fmt.Println("      draft-only: never paste this into a To: field unverified.")
	}
	return 0
}

// ---------------------------------------------------------------- list

func list(conn *sql.DB, args []string) int {
	fs := newFlags("list")
	all := fs.Bool("all", false, "include rejected and ghosted")
	if _, ok := parse(fs, args); !ok {
		return 2
	}

	query := "SELECT a.id, a.status, a.followup_due, j.title, c.name" +
		" FROM applications a JOIN jobs j ON j.id = a.job_id" +
		" JOIN companies c ON c.id = j.company_id"
	var params []any
	if !*all {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(db.ClosedStatuses)), ",")
		query += " WHERE a.status NOT IN (" + placeholders + ")"
		for _, s := range db.ClosedStatuses {
			params = append(params, s)
		}
	}
	query += " ORDER BY (a.followup_due IS NULL), a.followup_due, a.id"

	rows, err := conn.Query(query, params...)
	if err != nil {
		return fail("select applications: %v", err)
	}
	defer rows.Close()

	type line struct {
		id                            int64
		status, due, title, companyNm string
	}
	var lines []line
	for rows.Next() {
		var (
			l   line
			due sql.NullString
		)
		if err := rows.Scan(&l.id, &l.status, &due, &l.title, &l.companyNm); err != nil {
			return fail("scan application: %v", err)
		}
		l.due = due.String
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return fail("iterate applications: %v", err)
	}
	if len(lines) == 0 {
		fmt.Println("nothing tracked yet — `add-job <url>` to start.")
		return 0
	}

	today := time.Now()
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	fmt.Printf("%s  %s %s %s %s flag\n", padLeft("id", 4), pad("status", 12),
		pad("company", 20), pad("title", 40), pad("due", 12))
	fmt.Println(dash(100))
	for _, l := range lines {
		flag, shown := "", "-"
		if d := dates.ParseDay(l.due); !d.IsZero() {
			shown = d.Format(dayFormat)
			switch delta := int(d.Sub(today).Hours() / 24); {
			case delta < 0:
				flag = "OVERDUE"
			case delta == 0:
				flag = "DUE TODAY"
			default:
				flag = fmt.Sprintf("in %dd", delta)
			}
		}
		fmt.Printf("%s  %s %s %s %s %s\n", padLeft(fmt.Sprint(l.id), 4), pad(l.status, 12),
			pad(l.companyNm, 20), pad(l.title, 40), pad(shown, 12), flag)
	}
	fmt.Printf("\n%d application(s).\n", len(lines))
	return 0
}

// ---------------------------------------------------------------- export

func exportCmd(conn *sql.DB, args []string) int {
	fs := newFlags("export")
	out := fs.String("o", "tracker.xlsx", "output path")
	output := fs.String("output", "", "output path (long form)")
	if _, ok := parse(fs, args); !ok {
		return 2
	}
	path := *out
	if *output != "" {
		path = *output
	}
	abs, counts, err := export.Export(conn, path)
	if err != nil {
		return fail("%v", err)
	}
	fmt.Printf("wrote %d application(s) + %d pipeline row(s) -> %s\n",
		counts.Applications, counts.Pipeline, abs)
	return 0
}
