package funding

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Vaivaswat2244/job-tracker/internal/ats"
	"github.com/Vaivaswat2244/job-tracker/internal/db"
	"github.com/Vaivaswat2244/job-tracker/internal/health"
	"github.com/Vaivaswat2244/job-tracker/internal/normalize"
	"github.com/Vaivaswat2244/job-tracker/internal/watchlist"
)

// Run orchestrates one funding-signal run.
//
// Nothing here writes to `companies` except to update a company the user
// already put on the watchlist. New companies stop at `watchlist_candidates`
// with status='needs_review' and wait for an explicit approval.

const (
	FundingWindowDaysRun = 60
	MaxResolvePerRun     = 15 // article fetches; the listing itself is one request
)

// --------------------------------------------------------------- source state

type sourceState struct {
	ETag         sql.NullString
	LastModified sql.NullString
}

func loadState(conn *sql.DB, name string) (sourceState, error) {
	var s sourceState
	err := conn.QueryRow(
		"SELECT etag, last_modified FROM funding_source_state WHERE name = ?", name).
		Scan(&s.ETag, &s.LastModified)
	if errors.Is(err, sql.ErrNoRows) {
		return sourceState{}, nil
	}
	if err != nil {
		return s, fmt.Errorf("read state for source %q: %w", name, err)
	}
	return s, nil
}

func saveState(conn *sql.DB, name, etag, lastModified string) error {
	_, err := conn.Exec(
		"INSERT INTO funding_source_state (name, etag, last_modified, last_run_at)"+
			" VALUES (?,?,?,?) ON CONFLICT(name) DO UPDATE SET"+
			" etag=excluded.etag, last_modified=excluded.last_modified,"+
			" last_run_at=excluded.last_run_at",
		name, nilIfEmpty(etag), nilIfEmpty(lastModified), db.Now(),
	)
	if err != nil {
		return fmt.Errorf("save state for source %q: %w", name, err)
	}
	return nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// -------------------------------------------------------------------- storage

// StoreItem inserts one funding item, reporting whether it was new.
//
// Partial extraction is stored, not dropped: a row with only a company name and
// a date is still a lead.
func StoreItem(conn *sql.DB, item FeedItem, e Extraction) (int64, bool, error) {
	investors, err := json.Marshal(e.Investors)
	if err != nil {
		investors = []byte("[]")
	}

	res, err := conn.Exec(
		"INSERT OR IGNORE INTO funding_items (source, headline, article_url, published_at,"+
			" company_name, round_stage, amount_raw, currency, investors, announced_at,"+
			" extraction_confidence, extraction_method, raw_text, llm_output, created_at)"+
			" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		item.Source, item.Headline, item.URL, nilIfEmpty(item.PublishedAt),
		nilIfEmpty(e.CompanyName), e.RoundStage, nilIfEmpty(e.AmountRaw),
		nilIfEmpty(e.Currency), string(investors), nilIfEmpty(e.AnnouncedAt),
		e.Confidence, e.Method, e.RawText, nilIfEmpty(e.LLMRaw), db.Now(),
	)
	if err != nil {
		return 0, false, fmt.Errorf("store funding item %q: %w", item.URL, err)
	}
	affected, err := res.RowsAffected()
	if err != nil || affected == 0 {
		return 0, false, nil
	}
	id, err := res.LastInsertId()
	return id, true, err
}

func AddCandidate(conn *sql.DB, e Extraction, domain, reason, atsName, slug string) error {
	// `name` is NOT NULL, and OR IGNORE (there for the dedupe index) would
	// swallow the constraint failure and drop the item without a trace. An
	// unparsed headline is exactly the case a human should look at, so it gets a
	// placeholder name and stays visible.
	name := e.CompanyName
	if name == "" {
		raw := e.RawText
		if raw == "" {
			raw = "?"
		}
		name = "(unparsed) " + headRunes(raw, 90)
	}

	_, err := conn.Exec(
		"INSERT OR IGNORE INTO watchlist_candidates (name, domain, round_stage, amount_raw,"+
			" announced_at, article_url, resolved_ats, resolved_slug, status, reason, created_at)"+
			" VALUES (?,?,?,?,?,?,?,?,'needs_review',?,?)",
		name, nilIfEmpty(domain), nilIfEmpty(e.RoundStage), nilIfEmpty(e.AmountRaw),
		nilIfEmpty(e.AnnouncedAt), nilIfEmpty(e.ArticleURL), nilIfEmpty(atsName),
		nilIfEmpty(slug), nilIfEmpty(reason), db.Now(),
	)
	if err != nil {
		return fmt.Errorf("add candidate %q: %w", name, err)
	}
	return nil
}

func headRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ApplyFunding records a confirmed raise and opens the hiring window.
//
// The window is stored as an expiry rather than a flag, so it decays on its own
// when the date passes — there is no cleanup job that can fail to run.
func ApplyFunding(conn *sql.DB, companyID int64, e Extraction) error {
	var until any
	if announced, ok := normalize.ParseDT(e.AnnouncedAt); ok {
		until = announced.AddDate(0, 0, FundingWindowDaysRun).Format(db.ISO8601)
	}

	_, err := conn.Exec(
		"UPDATE companies SET recently_funded_at = ?, funding_stage = ?,"+
			" funding_amount_raw = ?, priority_until = ? WHERE id = ?",
		nilIfEmpty(e.AnnouncedAt), nilIfEmpty(e.RoundStage),
		nilIfEmpty(e.AmountRaw), until, companyID,
	)
	if err != nil {
		return fmt.Errorf("apply funding to company %d: %w", companyID, err)
	}
	return nil
}

// DetectATSFor runs Task A detection against a resolved domain, for the
// approval preview.
func DetectATSFor(domain string) (string, string) {
	for _, u := range []string{
		"https://" + domain + "/careers",
		"https://" + domain + "/jobs",
		"https://" + domain,
	} {
		if result := ats.Detect(u); result.Found() {
			return result.ATS, result.Slug
		}
	}
	return "", ""
}

// ------------------------------------------------------------------ resolution

// ResolveItem attaches an item to a company, or sends it to review, returning
// the outcome.
func ResolveItem(conn *sql.DB, e Extraction, doNetwork bool) (string, error) {
	if e.CompanyName == "" {
		return "needs_review", AddCandidate(conn, e, "",
			"extraction found no company name — headline needs a human", "", "")
	}

	domain, reason := "", "resolution skipped"
	if doNetwork {
		domain, reason = ResolveDomain(e.ArticleURL, e.CompanyName, true)
	}

	collisions, err := NameCollisions(conn, e.CompanyName)
	if err != nil {
		return "", err
	}

	if domain != "" {
		matched, found, err := MatchOnDomain(conn, domain)
		if err != nil {
			return "", err
		}
		if found {
			return "confirmed", ApplyFunding(conn, matched.ID, e)
		}

		// Resolved, but not a company we track. This is the one case where a
		// name collision would have produced a wrong match, so say so plainly.
		note := fmt.Sprintf("resolved to %s; not on the watchlist", domain)
		if len(collisions) > 0 {
			note += fmt.Sprintf(". NAME COLLISION: watchlist already has %s"+
				" — different domain, so NOT matched", describeCollisions(collisions))
		}
		atsName, slug := "", ""
		if doNetwork {
			atsName, slug = DetectATSFor(domain)
		}
		return "needs_review", AddCandidate(conn, e, domain, note, atsName, slug)
	}

	note := fmt.Sprintf("no domain resolved (%s)", reason)
	if len(collisions) > 0 {
		note += fmt.Sprintf(". Name resembles watchlist entry: %s"+
			" — not matched without a domain", describeCollisions(collisions))
	}
	return "needs_review", AddCandidate(conn, e, "", note, "", "")
}

func describeCollisions(collisions []Collision) string {
	parts := make([]string, len(collisions))
	for i, c := range collisions {
		domain := "no domain"
		if c.Domain.Valid && c.Domain.String != "" {
			domain = c.Domain.String
		}
		parts[i] = fmt.Sprintf("%s (%s)", c.Name, domain)
	}
	return strings.Join(parts, ", ")
}

// -------------------------------------------------------------------- the run

// SourceAlert names a source whose feed looks unhealthy.
type SourceAlert struct {
	Name   string
	Reason string
}

type RunSummary struct {
	Sources     int
	Items       int
	Funding     int
	Stored      int
	NearMiss    int
	Confirmed   int
	NeedsReview int
	Failed      int
	Alerts      []SourceAlert
}

type RunOptions struct {
	Only         string
	ResolveLimit int
	DoNetwork    bool
	Verbose      bool
	ConfigPath   string
	RulesPath    string
	Stdout       io.Writer
	Stderr       io.Writer
}

func Run(conn *sql.DB, opts RunOptions) (RunSummary, error) {
	var summary RunSummary
	if opts.ResolveLimit == 0 {
		opts.ResolveLimit = MaxResolvePerRun
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}

	rules, err := Rules(opts.RulesPath)
	if err != nil {
		return summary, err
	}
	configs, err := LoadConfig(opts.ConfigPath)
	if err != nil {
		return summary, err
	}

	resolveLimit := opts.ResolveLimit
	for _, config := range configs {
		if !config.IsEnabled() {
			continue
		}
		if opts.Only != "" && !strings.EqualFold(opts.Only, config.Name) {
			continue
		}
		summary.Sources++

		state, err := loadState(conn, config.Name)
		if err != nil {
			return summary, err
		}
		result := Fetch(config, state.ETag.String, state.LastModified.String)

		itemCount := health.Null
		if !result.NotModified && result.Items != nil {
			itemCount = health.Int(result.ItemsFound)
		}
		status := health.Null
		if result.Status != 0 {
			status = health.Int(result.Status)
		}
		meta, _ := json.Marshal(map[string]any{
			"parse_ok": result.ParseOK, "items_found": result.ItemsFound,
			"selector_version": result.SelectorVersion,
		})
		if err := health.RecordPoll(conn, "funding_source", config.Name, health.Poll{
			HTTPStatus: status, ItemCount: itemCount, OK: result.ParseOK,
			Error: result.Error, Meta: string(meta),
		}); err != nil {
			return summary, err
		}

		reason, err := health.Check(conn, "funding_source", config.Name,
			config.Name+" funding feed")
		if err != nil {
			return summary, err
		}
		if reason != "" {
			summary.Alerts = append(summary.Alerts, SourceAlert{config.Name, reason})
		}

		if result.NotModified {
			if opts.Verbose {
				fmt.Fprintf(opts.Stdout, "  %-12s 304 not modified\n", config.Name)
			}
			if err := saveState(conn, config.Name, result.ETag, result.LastModified); err != nil {
				return summary, err
			}
			continue
		}
		if !result.ParseOK {
			summary.Failed++
			fmt.Fprintf(opts.Stderr, "  %-12s FAILED  %s\n", config.Name, result.Error)
			continue
		}

		summary.Items += result.ItemsFound
		var pending []Extraction
		for _, item := range result.Items {
			if !rules.IsFunding(item.Headline) {
				if rules.IsNearMiss(item.Headline) {
					// Mentions money but matched no trigger. Not stored as a
					// round, but never dropped without a trace either.
					payload, _ := json.Marshal(map[string]string{
						"headline": item.Headline, "url": item.URL,
					})
					if err := db.LogExclusion(conn, string(payload),
						"mentions money but matched no funding trigger",
						"funding.not_a_round"); err != nil {
						return summary, err
					}
					summary.NearMiss++
				}
				continue
			}

			summary.Funding++
			extraction := rules.Extract(item.Headline, item.URL, item.PublishedAt)
			_, stored, err := StoreItem(conn, item, extraction)
			if err != nil {
				return summary, err
			}
			if stored {
				summary.Stored++
				pending = append(pending, extraction)
			}
		}

		if opts.Verbose {
			fmt.Fprintf(opts.Stdout, "  %-12s %3d items, %d new funding row(s)\n",
				config.Name, result.ItemsFound, len(pending))
		}

		for _, extraction := range pending {
			if resolveLimit <= 0 {
				break
			}
			outcome, err := ResolveItem(conn, extraction, opts.DoNetwork)
			if err != nil {
				return summary, err
			}
			switch outcome {
			case "confirmed":
				summary.Confirmed++
			case "needs_review":
				summary.NeedsReview++
			}
			resolveLimit--
		}

		if err := saveState(conn, config.Name, result.ETag, result.LastModified); err != nil {
			return summary, err
		}
	}

	if opts.Verbose {
		fmt.Fprintf(opts.Stdout,
			"\n%d source(s), %d items, %d new funding row(s), %d confirmed,"+
				" %d need review, %d near-misses logged\n",
			summary.Sources, summary.Items, summary.Stored, summary.Confirmed,
			summary.NeedsReview, summary.NearMiss)
		for _, a := range summary.Alerts {
			fmt.Fprintf(opts.Stderr, "  ALERT stale_feed: %s (%s)\n", a.Name, a.Reason)
		}
	}
	return summary, nil
}

// ------------------------------------------------------------------- approval

// Approve is the only path from a candidate into the watchlist, and it is
// explicit.
func Approve(conn *sql.DB, candidateID int64, path string) (bool, string, error) {
	var (
		name                                   string
		domain, stage, amount, announced       sql.NullString
		resolvedATS, resolvedSlug, statusValue sql.NullString
	)
	err := conn.QueryRow(
		"SELECT name, domain, round_stage, amount_raw, announced_at,"+
			" resolved_ats, resolved_slug, status FROM watchlist_candidates WHERE id = ?",
		candidateID,
	).Scan(&name, &domain, &stage, &amount, &announced,
		&resolvedATS, &resolvedSlug, &statusValue)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Sprintf("no candidate with id %d", candidateID), nil
	}
	if err != nil {
		return false, "", fmt.Errorf("read candidate %d: %w", candidateID, err)
	}
	if statusValue.String != "needs_review" {
		return false, fmt.Sprintf("candidate %d is already %s",
			candidateID, statusValue.String), nil
	}

	entries, err := watchlist.Load(path)
	if err != nil {
		return false, "", err
	}
	if _, found := watchlist.Find(entries, name, domain.String); found {
		if _, err := conn.Exec(
			"UPDATE watchlist_candidates SET status='approved' WHERE id = ?",
			candidateID); err != nil {
			return false, "", fmt.Errorf("mark candidate %d approved: %w", candidateID, err)
		}
		return true, name + " was already on the watchlist", nil
	}

	atsName := resolvedATS.String
	if atsName == "" {
		atsName = "unknown"
	}
	careersURL := ""
	if domain.String != "" {
		careersURL = "https://" + domain.String + "/careers"
	}
	stageLabel := stage.String
	if stageLabel == "" {
		stageLabel = "unknown"
	}
	enabled := true

	if err := watchlist.Append(watchlist.Entry{
		Name:       name,
		Domain:     domain.String,
		ATS:        atsName,
		Slug:       resolvedSlug.String,
		CareersURL: careersURL,
		Source:     "funding:" + stageLabel,
		Priority:   "high", // freshly funded: the whole point is to watch closely
		Enabled:    &enabled,
	}, path); err != nil {
		return false, "", err
	}
	if _, _, err := watchlist.Sync(conn, path); err != nil {
		return false, "", err
	}

	if domain.String != "" {
		company, found, err := MatchOnDomain(conn, domain.String)
		if err != nil {
			return false, "", err
		}
		if found {
			var until any
			if parsed, ok := normalize.ParseDT(announced.String); ok {
				until = parsed.AddDate(0, 0, FundingWindowDaysRun).Format(db.ISO8601)
			}
			if _, err := conn.Exec(
				"UPDATE companies SET recently_funded_at = ?, funding_stage = ?,"+
					" funding_amount_raw = ?, priority_until = ? WHERE id = ?",
				nullable(announced), nullable(stage), nullable(amount), until, company.ID,
			); err != nil {
				return false, "", fmt.Errorf("apply funding on approve: %w", err)
			}
		}
	}

	if _, err := conn.Exec(
		"UPDATE watchlist_candidates SET status='approved' WHERE id = ?",
		candidateID); err != nil {
		return false, "", fmt.Errorf("mark candidate %d approved: %w", candidateID, err)
	}

	detail := "ats unknown — set a slug in watchlist.yaml to start polling"
	if resolvedSlug.String != "" {
		detail = resolvedATS.String + "/" + resolvedSlug.String
	}
	return true, fmt.Sprintf("added %s to the watchlist (%s)", name, detail), nil
}

func nullable(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}

func Reject(conn *sql.DB, candidateID int64) (bool, string, error) {
	res, err := conn.Exec(
		"UPDATE watchlist_candidates SET status='rejected'"+
			" WHERE id = ? AND status = 'needs_review'", candidateID)
	if err != nil {
		return false, "", fmt.Errorf("reject candidate %d: %w", candidateID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil || affected == 0 {
		return false, fmt.Sprintf("no reviewable candidate with id %d", candidateID), nil
	}
	return true, fmt.Sprintf("candidate %d dismissed", candidateID), nil
}
