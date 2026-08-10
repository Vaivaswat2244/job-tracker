"""SQLite is the source of truth. Everything else is a generated view."""
import os
import sqlite3
from datetime import datetime, timezone

DB_PATH = os.environ.get(
    "TRACKER_DB",
    os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "tracker.db"),
)

STATUSES = (
    "found",
    "applied",
    "followed_up",
    "in_process",
    "offer",
    "rejected",
    "ghosted",
)

# Terminal states: no follow-up chasing, hidden from the default `list`.
CLOSED_STATUSES = ("rejected", "ghosted")

EMAIL_CONFIDENCE = ("published", "verified", "inferred")

# Watchlist / ingest vocabulary (Phase 2-3).
ATS_PROVIDERS = ("greenhouse", "lever", "ashby", "unknown")
PRIORITIES = ("normal", "high")
COMP_MODELS = ("location_agnostic", "geo_adjusted", "local_market", "unknown")
ROUND_STAGES = ("pre-seed", "seed", "A", "B", "C+", "debt", "unknown")
CANDIDATE_STATUSES = ("needs_review", "approved", "rejected")

SCHEMA = """
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
"""

# Columns added to Phase 1 tables after the fact. Applied idempotently on every
# connect() so an existing tracker.db upgrades in place without a migration step.
ADDED_COLUMNS = {
    "companies": [
        ("ats", "TEXT"),                    # greenhouse | lever | ashby | unknown
        ("slug", "TEXT"),
        ("priority", "TEXT DEFAULT 'normal'"),
        ("watchlist_enabled", "INTEGER DEFAULT 1"),
        ("oss_repo", "TEXT"),
        ("discovery_source", "TEXT"),       # watchlist.yaml `source:`
        ("last_polled_at", "TEXT"),
        ("poll_etag", "TEXT"),
        ("poll_last_modified", "TEXT"),
        ("recently_funded_at", "TEXT"),
        ("funding_stage", "TEXT"),
        ("funding_amount_raw", "TEXT"),
        ("priority_until", "TEXT"),         # high-priority window expiry; decays to normal
    ],
    "jobs": [
        ("external_id", "TEXT"),
        ("first_seen_at", "TEXT"),          # seen_at is last-seen; this is first-seen
        ("location", "TEXT"),
        ("employment_type", "TEXT"),
        ("remote", "INTEGER"),
        ("raw_json", "TEXT"),
        ("dedupe_key", "TEXT"),         # normalized company|title|posted_week
    ],
}

# Idempotent upsert key for ingested jobs. Partial so manually added rows, which
# have no external_id, are never forced to collide with each other.
POST_INDEXES = [
    "CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_source_external"
    " ON jobs(source, external_id) WHERE external_id IS NOT NULL",
    "CREATE INDEX IF NOT EXISTS idx_jobs_canonical ON jobs(canonical_id)",
    "CREATE INDEX IF NOT EXISTS idx_jobs_dedupe ON jobs(dedupe_key)",
    "CREATE INDEX IF NOT EXISTS idx_companies_domain ON companies(lower(domain))",
]


def migrate(conn: sqlite3.Connection) -> None:
    """Add any column this version expects but the file on disk lacks."""
    for table, columns in ADDED_COLUMNS.items():
        have = {r["name"] for r in conn.execute(f"PRAGMA table_info({table})")}
        for name, decl in columns:
            if name not in have:
                conn.execute(f"ALTER TABLE {table} ADD COLUMN {name} {decl}")
    for stmt in POST_INDEXES:
        conn.execute(stmt)
    conn.commit()


def now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def connect(path: str | None = None) -> sqlite3.Connection:
    conn = sqlite3.connect(path or DB_PATH)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA foreign_keys = ON")
    conn.executescript(SCHEMA)
    migrate(conn)
    return conn


def log_exclusion(conn, raw_payload: str, reason: str, rule_id: str) -> None:
    """Every exclusion leaves a greppable row. Nothing is dropped silently."""
    conn.execute(
        "INSERT INTO excluded_log (raw_payload, reason, rule_id, logged_at) VALUES (?,?,?,?)",
        (raw_payload, reason, rule_id, now()),
    )
    conn.commit()
