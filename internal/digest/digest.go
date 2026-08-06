// Package digest builds the daily digest: everything the user should look at
// today, in one screen.
//
// Ordering is deliberate. Alerts come first because a dead feed makes every
// section below it a lie by omission. Funding comes next because that window is
// the only place in this pipeline where being early is structurally possible.
package digest

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Vaivaswat2244/job-tracker/internal/db"
	"github.com/Vaivaswat2244/job-tracker/internal/health"
	"github.com/Vaivaswat2244/job-tracker/internal/normalize"
)

const (
	NewJobHours       = 48
	CandidateCap      = 10 // keep needs-review clearable, or it stops being read
	FundingWindowDays = 60
)

func iso(t time.Time) string { return t.Format(db.ISO8601) }

func daysAgo(value string, now time.Time) (int, bool) {
	parsed, ok := normalize.ParseDT(value)
	if !ok {
		return 0, false
	}
	return int(now.Sub(parsed).Hours() / 24), true
}

// ------------------------------------------------------------------- sections

func AlertsSection(conn *sql.DB) ([]string, error) {
	rows, err := health.OpenAlerts(conn)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	lines := []string{fmt.Sprintf("ALERTS — %d feed(s) need attention", len(rows))}
	for _, row := range rows {
		lines = append(lines, "  ! "+row.Message)
		if row.Detail != "" {
			lines = append(lines, "      "+row.Detail)
		}
	}
	return append(lines,
		"  These companies are still being polled. Fix the slug in watchlist.yaml."), nil
}

func boardLink(ats, slug string) string {
	if slug == "" {
		return "[no board known]"
	}
	switch ats {
	case "greenhouse":
		return "https://job-boards.greenhouse.io/" + slug
	case "lever":
		return "https://jobs.lever.co/" + slug
	case "ashby":
		return "https://jobs.ashbyhq.com/" + slug
	default:
		return "[no board known]"
	}
}

func FundedSection(conn *sql.DB, now time.Time) ([]string, error) {
	cutoff := iso(now.Add(-FundingWindowDays * 24 * time.Hour))
	rows, err := conn.Query(
		"SELECT c.id, c.name, c.funding_stage, c.funding_amount_raw, c.recently_funded_at,"+
			" c.ats, c.slug,"+
			" (SELECT count(*) FROM jobs j WHERE j.company_id = c.id AND j.canonical_id IS NULL)"+
			"   AS role_count"+
			" FROM companies c WHERE c.recently_funded_at IS NOT NULL AND c.recently_funded_at >= ?"+
			" ORDER BY c.recently_funded_at DESC",
		cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("read funded companies: %w", err)
	}
	defer rows.Close()

	lines := []string{"RECENTLY FUNDED — hiring window open"}
	found := false
	for rows.Next() {
		var (
			id                                 int64
			name                               string
			stage, amount, fundedAt, ats, slug sql.NullString
			roleCount                          int
		)
		if err := rows.Scan(&id, &name, &stage, &amount, &fundedAt,
			&ats, &slug, &roleCount); err != nil {
			return nil, fmt.Errorf("scan funded company: %w", err)
		}
		found = true

		when := "recently"
		if d, ok := daysAgo(fundedAt.String, now); ok {
			when = fmt.Sprintf("%d days ago", d)
		}
		board := boardLink(ats.String, slug.String)

		tail := fmt.Sprintf("%d open roles — %s", roleCount, board)
		if roleCount == 0 {
			// The valuable line: funded, hiring imminent, nothing posted yet.
			tail = "no roles posted yet — [watching] " + board
		}
		lines = append(lines, fmt.Sprintf("  %s — %s, %s, %s — %s",
			name, orDefault(stage, "unknown round"), orDefault(amount, "undisclosed"), when, tail))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return lines, nil
}

func orDefault(v sql.NullString, fallback string) string {
	if v.Valid && v.String != "" {
		return v.String
	}
	return fallback
}

func CandidatesSection(conn *sql.DB) ([]string, error) {
	rows, err := conn.Query(
		"SELECT id, name, domain, round_stage, amount_raw, resolved_ats, resolved_slug, reason"+
			" FROM watchlist_candidates WHERE status = 'needs_review'"+
			" ORDER BY announced_at DESC, id DESC LIMIT ?",
		CandidateCap,
	)
	if err != nil {
		return nil, fmt.Errorf("read candidates: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		id                                       int64
		name                                     string
		domain, stage, amount, ats, slug, reason sql.NullString
	}
	var found []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.name, &c.domain, &c.stage, &c.amount,
			&c.ats, &c.slug, &c.reason); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		found = append(found, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, nil
	}

	var total int
	if err := conn.QueryRow(
		"SELECT count(*) FROM watchlist_candidates WHERE status = 'needs_review'").
		Scan(&total); err != nil {
		return nil, fmt.Errorf("count candidates: %w", err)
	}

	heading := fmt.Sprintf("NEEDS REVIEW — %d unresolved funding match(es)", total)
	if total > len(found) {
		heading += fmt.Sprintf(", showing %d", len(found))
	}

	lines := []string{heading}
	for _, c := range found {
		detected := "ats unknown"
		if c.slug.Valid && c.slug.String != "" {
			detected = c.ats.String + "/" + c.slug.String
		}
		lines = append(lines, fmt.Sprintf("  [%3d] %s (%s) — %s, %s — %s",
			c.id, c.name, orDefault(c.domain, "no domain"),
			orDefault(c.stage, "?"), orDefault(c.amount, "undisclosed"), detected))
		if c.reason.Valid && c.reason.String != "" {
			lines = append(lines, "        "+c.reason.String)
		}
	}
	return append(lines,
		"  approve: tracker candidate approve <id>   dismiss: tracker candidate reject <id>"), nil
}

func NewRolesSection(conn *sql.DB, hours int, now time.Time) ([]string, error) {
	cutoff := iso(now.Add(-time.Duration(hours) * time.Hour))
	rows, err := conn.Query(
		"SELECT j.id, j.title, j.location, j.url, j.auth_required, j.hires_in_india,"+
			" j.comp_model, c.name AS company"+
			" FROM jobs j JOIN companies c ON c.id = j.company_id"+
			" WHERE j.canonical_id IS NULL AND j.first_seen_at IS NOT NULL AND j.first_seen_at >= ?"+
			// auth_required sorts a role lower; it never removes it (INV-1).
			" ORDER BY j.auth_required ASC, (j.hires_in_india = 1) DESC, j.first_seen_at DESC",
		cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("read new roles: %w", err)
	}
	defer rows.Close()

	var lines []string
	count := 0
	for rows.Next() {
		var (
			id                         int64
			title, company             string
			location, url, compModel   sql.NullString
			authRequired, hiresInIndia sql.NullInt64
		)
		if err := rows.Scan(&id, &title, &location, &url, &authRequired,
			&hiresInIndia, &compModel, &company); err != nil {
			return nil, fmt.Errorf("scan new role: %w", err)
		}
		count++

		var flags []string
		if authRequired.Valid && authRequired.Int64 != 0 {
			flags = append(flags, "needs US/EU auth")
		}
		if hiresInIndia.Valid && hiresInIndia.Int64 == 1 {
			flags = append(flags, "india/global")
		}
		if compModel.Valid && compModel.String != "" && compModel.String != "unknown" {
			flags = append(flags, compModel.String)
		}
		suffix := ""
		if len(flags) > 0 {
			suffix = "  [" + strings.Join(flags, ", ") + "]"
		}
		lines = append(lines, fmt.Sprintf("  [%4d] %s — %s (%s)%s",
			id, company, title, orDefault(location, "location n/a"), suffix))
		lines = append(lines, "         "+url.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if count == 0 {
		return []string{fmt.Sprintf("NEW ROLES — none in the last %dh", hours)}, nil
	}

	heading := fmt.Sprintf("NEW ROLES — %d in the last %dh", count, hours)
	return append(append([]string{heading}, lines...), "  apply: tracker apply <job_id>"), nil
}

// Build assembles the digest. now is injectable so the output is deterministic
// under test.
func Build(conn *sql.DB, hours int, now time.Time) (string, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if hours == 0 {
		hours = NewJobHours
	}

	blocks := [][]string{{fmt.Sprintf("JOB PIPELINE DIGEST — %s", now.Format("2006-01-02"))}}

	alerts, err := AlertsSection(conn)
	if err != nil {
		return "", err
	}
	funded, err := FundedSection(conn, now)
	if err != nil {
		return "", err
	}
	candidates, err := CandidatesSection(conn)
	if err != nil {
		return "", err
	}
	roles, err := NewRolesSection(conn, hours, now)
	if err != nil {
		return "", err
	}

	for _, section := range [][]string{alerts, funded, candidates, roles} {
		if len(section) > 0 {
			blocks = append(blocks, section)
		}
	}

	joined := make([]string, len(blocks))
	for i, block := range blocks {
		joined[i] = strings.Join(block, "\n")
	}
	return strings.Join(joined, "\n\n"), nil
}
