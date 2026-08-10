package db

// Statuses is the application lifecycle vocabulary.
var Statuses = []string{
	"found",
	"applied",
	"followed_up",
	"in_process",
	"offer",
	"rejected",
	"ghosted",
}

// ClosedStatuses are terminal: no follow-up chasing, hidden from the default `list`.
var ClosedStatuses = []string{"rejected", "ghosted"}

var EmailConfidence = []string{"published", "verified", "inferred"}

// Watchlist / ingest vocabulary (Phase 2-3).
var (
	ATSProviders      = []string{"greenhouse", "lever", "ashby", "unknown"}
	Priorities        = []string{"normal", "high"}
	CompModels        = []string{"location_agnostic", "geo_adjusted", "local_market", "unknown"}
	RoundStages       = []string{"pre-seed", "seed", "A", "B", "C+", "debt", "unknown"}
	CandidateStatuses = []string{"needs_review", "approved", "rejected"}
)

const Schema = `
CREATE TABLE IF NOT EXISTS companies (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    domain TEXT,
    careers_url TEXT,
    remote_policy TEXT,
    hiring_countries TEXT,
    eor_provider TEXT,
    notes TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_companies_name ON companies(lower(name));

CREATE TABLE IF NOT EXISTS jobs (
    id INTEGER PRIMARY KEY,
    company_id INTEGER NOT NULL REFERENCES companies(id),
    title TEXT NOT NULL,
    url TEXT NOT NULL,
    source TEXT,
    posted_at TEXT,
    seen_at TEXT NOT NULL,
    score REAL,
    jd_text TEXT,
    canonical_id INTEGER REFERENCES jobs(id),
    pay_min REAL,
    pay_max REAL,
    pay_currency TEXT,
    comp_model TEXT,
    hires_in_india INTEGER,
    auth_required INTEGER,
    source_urls TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_url ON jobs(url);

CREATE TABLE IF NOT EXISTS applications (
    id INTEGER PRIMARY KEY,
    job_id INTEGER NOT NULL REFERENCES jobs(id),
    applied_at TEXT,
    status TEXT NOT NULL CHECK (status IN
        ('found','applied','followed_up','in_process','offer','rejected','ghosted')),
    followup_due TEXT,
    resume_version TEXT,
    notes TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_applications_job ON applications(job_id);

CREATE TABLE IF NOT EXISTS contacts (
    id INTEGER PRIMARY KEY,
    company_id INTEGER NOT NULL REFERENCES companies(id),
    name TEXT NOT NULL,
    title TEXT NOT NULL,
    email TEXT,
    email_confidence TEXT CHECK (email_confidence IN ('published','verified','inferred')),
    linkedin_url TEXT,
    source TEXT
);

CREATE TABLE IF NOT EXISTS outreach (
    id INTEGER PRIMARY KEY,
    contact_id INTEGER NOT NULL REFERENCES contacts(id),
    application_id INTEGER REFERENCES applications(id),
    channel TEXT,
    status TEXT NOT NULL DEFAULT 'draft',
    drafted_at TEXT,
    sent_at TEXT,
    replied_at TEXT,
    body TEXT
);

-- One row per Gmail message the ingest has looked at, keyed on Gmail's own id
-- so a re-poll is idempotent and a message is never acted on twice.
--
-- needs_review is the whole point of the table: when the message cannot be tied
-- to exactly one application with confidence, it is recorded and queued rather
-- than guessed at. A wrong auto-transition is worse than a queue, because the
-- user stops trusting the status column.
CREATE TABLE IF NOT EXISTS mail_messages (
    gmail_id TEXT PRIMARY KEY,
    thread_id TEXT,
    from_addr TEXT,
    from_domain TEXT,
    subject TEXT,
    received_at TEXT,
    kind TEXT NOT NULL,              -- confirmation | rejection | reply | other
    company_id INTEGER REFERENCES companies(id),
    job_id INTEGER REFERENCES jobs(id),
    application_id INTEGER REFERENCES applications(id),
    action TEXT NOT NULL,            -- applied | rejected | replied | queued | none
    needs_review INTEGER NOT NULL DEFAULT 0,
    reason TEXT,
    company_guess TEXT,              -- what the subject line claimed, pre-match
    decided_at TEXT,
    seen_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS excluded_log (
    id INTEGER PRIMARY KEY,
    raw_payload TEXT,
    reason TEXT NOT NULL,
    rule_id TEXT NOT NULL,
    logged_at TEXT NOT NULL
);

-- Internal state for the escalation ladder. Not domain data: one row per
-- (application, stage) so each escalation step notifies exactly once.
CREATE TABLE IF NOT EXISTS followup_notices (
    application_id INTEGER NOT NULL REFERENCES applications(id),
    stage TEXT NOT NULL,
    notified_at TEXT NOT NULL,
    PRIMARY KEY (application_id, stage)
);

-- ------------------------------------------------------------------ Phase 2-3

-- One row per poll of anything pollable. Shared by the ATS watchlist (Task A)
-- and the funding sources (Task B) so feed-death detection has one implementation.
CREATE TABLE IF NOT EXISTS poll_log (
    id INTEGER PRIMARY KEY,
    target_type TEXT NOT NULL,          -- 'company' | 'funding_source'
    target_id TEXT NOT NULL,            -- company id, or source name
    polled_at TEXT NOT NULL,
    http_status INTEGER,                -- NULL when the request never completed
    item_count INTEGER,                 -- NULL on failure; 0 is meaningfully different
    ok INTEGER NOT NULL,                -- 2xx or 304
    error TEXT,
    meta TEXT
);
CREATE INDEX IF NOT EXISTS idx_poll_log_target
    ON poll_log(target_type, target_id, polled_at DESC);

-- Operational alerts. An open alert is (kind, target) unique so a feed that has
-- been dead for a month does not produce thirty rows.
CREATE TABLE IF NOT EXISTS alerts (
    id INTEGER PRIMARY KEY,
    kind TEXT NOT NULL,                 -- 'stale_feed'
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    message TEXT NOT NULL,
    detail TEXT,
    raised_at TEXT NOT NULL,
    notified_at TEXT,
    resolved_at TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_alerts_open
    ON alerts(kind, target_type, target_id) WHERE resolved_at IS NULL;

-- Conditional-request validators per funding source, so each run costs one
-- request and usually returns 304.
CREATE TABLE IF NOT EXISTS funding_source_state (
    name TEXT PRIMARY KEY,
    etag TEXT,
    last_modified TEXT,
    last_run_at TEXT
);

-- Funding items as scraped. Article bodies are deliberately not stored.
CREATE TABLE IF NOT EXISTS funding_items (
    id INTEGER PRIMARY KEY,
    source TEXT NOT NULL,
    headline TEXT NOT NULL,
    article_url TEXT NOT NULL,
    published_at TEXT,
    company_name TEXT,
    round_stage TEXT,
    amount_raw TEXT,
    currency TEXT,
    investors TEXT,                     -- JSON array
    announced_at TEXT,
    extraction_confidence TEXT,         -- 'high' | 'low'
    extraction_method TEXT,             -- 'rules' | 'llm'
    raw_text TEXT,                      -- headline-derived snippet used for extraction
    llm_output TEXT,                    -- raw LLM response when the flag was on
    resolved_domain TEXT,
    resolved_company_id INTEGER REFERENCES companies(id),
    created_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_funding_items_url ON funding_items(article_url);

-- Fuzzy or unresolved funding matches. Never promoted into companies without approval.
CREATE TABLE IF NOT EXISTS watchlist_candidates (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    domain TEXT,
    round_stage TEXT,
    amount_raw TEXT,
    announced_at TEXT,
    article_url TEXT,
    resolved_ats TEXT,
    resolved_slug TEXT,
    status TEXT NOT NULL DEFAULT 'needs_review'
        CHECK (status IN ('needs_review','approved','rejected')),
    reason TEXT,
    created_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_candidates_ident
    ON watchlist_candidates(lower(name), ifnull(article_url,''));
`

// addedColumn is a column added to a Phase 1 table after the fact. They are
// applied idempotently on every Connect so an existing tracker.db upgrades in
// place without a migration step.
type addedColumn struct{ Name, Decl string }

var addedColumns = []struct {
	Table   string
	Columns []addedColumn
}{
	{"companies", []addedColumn{
		{"ats", "TEXT"}, // greenhouse | lever | ashby | unknown
		{"slug", "TEXT"},
		{"priority", "TEXT DEFAULT 'normal'"},
		{"watchlist_enabled", "INTEGER DEFAULT 1"},
		{"oss_repo", "TEXT"},
		{"discovery_source", "TEXT"}, // watchlist.yaml `source:`
		{"last_polled_at", "TEXT"},
		{"poll_etag", "TEXT"},
		{"poll_last_modified", "TEXT"},
		{"recently_funded_at", "TEXT"},
		{"funding_stage", "TEXT"},
		{"funding_amount_raw", "TEXT"},
		{"priority_until", "TEXT"}, // high-priority window expiry; decays to normal
	}},
	{"jobs", []addedColumn{
		{"external_id", "TEXT"},
		{"first_seen_at", "TEXT"}, // seen_at is last-seen; this is first-seen
		{"location", "TEXT"},
		{"employment_type", "TEXT"},
		{"remote", "INTEGER"},
		{"raw_json", "TEXT"},
		{"dedupe_key", "TEXT"}, // normalized company|title|posted_week
	}},
}

// postIndexes includes the idempotent upsert key for ingested jobs. It is
// partial so manually added rows, which have no external_id, are never forced
// to collide with each other.
var postIndexes = []string{
	"CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_source_external" +
		" ON jobs(source, external_id) WHERE external_id IS NOT NULL",
	"CREATE INDEX IF NOT EXISTS idx_jobs_canonical ON jobs(canonical_id)",
	"CREATE INDEX IF NOT EXISTS idx_jobs_dedupe ON jobs(dedupe_key)",
	"CREATE INDEX IF NOT EXISTS idx_companies_domain ON companies(lower(domain))",
	"CREATE INDEX IF NOT EXISTS idx_mail_review ON mail_messages(needs_review, received_at)",
	"CREATE INDEX IF NOT EXISTS idx_mail_thread ON mail_messages(thread_id)",
}
