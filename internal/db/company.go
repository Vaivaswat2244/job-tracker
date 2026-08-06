package db

import "database/sql"

// Company is a row of `companies`. The nullable columns are modelled as
// sql.Null* rather than bare strings because several of them carry meaning when
// absent: a NULL slug means "not pollable yet", not "empty slug", and a NULL
// priority_until means no funding window was ever opened.
type Company struct {
	ID               int64
	Name             string
	Domain           sql.NullString
	CareersURL       sql.NullString
	RemotePolicy     sql.NullString
	HiringCountries  sql.NullString
	EORProvider      sql.NullString
	Notes            sql.NullString
	ATS              sql.NullString
	Slug             sql.NullString
	Priority         sql.NullString
	WatchlistEnabled sql.NullInt64
	OSSRepo          sql.NullString
	DiscoverySource  sql.NullString
	LastPolledAt     sql.NullString
	PollETag         sql.NullString
	PollLastModified sql.NullString
	RecentlyFundedAt sql.NullString
	FundingStage     sql.NullString
	FundingAmountRaw sql.NullString
	PriorityUntil    sql.NullString
}

// CompanyColumns is the select list every company query shares, in the order
// ScanCompany expects.
const CompanyColumns = `id, name, domain, careers_url, remote_policy, hiring_countries,
	eor_provider, notes, ats, slug, priority, watchlist_enabled, oss_repo,
	discovery_source, last_polled_at, poll_etag, poll_last_modified,
	recently_funded_at, funding_stage, funding_amount_raw, priority_until`

// Scanner is satisfied by both *sql.Row and *sql.Rows.
type Scanner interface{ Scan(dest ...any) error }

func ScanCompany(s Scanner) (Company, error) {
	var c Company
	err := s.Scan(
		&c.ID, &c.Name, &c.Domain, &c.CareersURL, &c.RemotePolicy, &c.HiringCountries,
		&c.EORProvider, &c.Notes, &c.ATS, &c.Slug, &c.Priority, &c.WatchlistEnabled,
		&c.OSSRepo, &c.DiscoverySource, &c.LastPolledAt, &c.PollETag, &c.PollLastModified,
		&c.RecentlyFundedAt, &c.FundingStage, &c.FundingAmountRaw, &c.PriorityUntil,
	)
	return c, err
}

// PriorityOr returns the stored priority, defaulting to "normal".
func (c Company) PriorityOr() string {
	if c.Priority.Valid && c.Priority.String != "" {
		return c.Priority.String
	}
	return "normal"
}

// ATSOr returns the stored provider, defaulting to "unknown".
func (c Company) ATSOr() string {
	if c.ATS.Valid && c.ATS.String != "" {
		return c.ATS.String
	}
	return "unknown"
}
