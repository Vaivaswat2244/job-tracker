"""Append-only ingest: upsert by (source, external_id), link duplicates, never delete.

INV-1 lives here. The two rules that matter:
  * re-polling the same board must not create a second row, and
  * discovering the same role on a second board must not remove the first.
"""
import json

from . import db, normalize
from .ats import NormalizedJob


def get_or_create_company(conn, name: str, domain: str | None = None) -> int:
    if domain:
        row = conn.execute(
            "SELECT id FROM companies WHERE lower(domain) = lower(?)", (domain,)
        ).fetchone()
        if row:
            return row["id"]
    row = conn.execute(
        "SELECT id, domain FROM companies WHERE lower(name) = lower(?)", (name,)
    ).fetchone()
    if row:
        if domain and not row["domain"]:
            conn.execute("UPDATE companies SET domain = ? WHERE id = ?", (domain, row["id"]))
        return row["id"]
    cur = conn.execute("INSERT INTO companies (name, domain) VALUES (?,?)", (name, domain))
    return cur.lastrowid


def dedupe_key(company_name: str, title: str, posted_at: str | None) -> str:
    return "|".join((
        normalize.norm_company(company_name),
        normalize.norm_title(title),
        normalize.posted_week(posted_at),
    ))


def _append_source_url(conn, job_id: int, url: str) -> None:
    """Every URL a role was seen at, on the canonical row. Never replaces."""
    row = conn.execute("SELECT source_urls FROM jobs WHERE id = ?", (job_id,)).fetchone()
    try:
        urls = json.loads(row["source_urls"]) if row and row["source_urls"] else []
        if not isinstance(urls, list):
            urls = []
    except (ValueError, TypeError):
        urls = []
    if url and url not in urls:
        urls.append(url)
        conn.execute("UPDATE jobs SET source_urls = ? WHERE id = ?",
                     (json.dumps(urls), job_id))


def link_duplicate(conn, job_id: int, key: str, url: str, source: str | None = None) -> int | None:
    """Point this row at the oldest row from a *different* source sharing its
    dedupe key. Returns that row's id, or None when this is the first sighting.

    Cross-source only. Within one board, two postings with distinct external_ids
    are distinct postings — the ATS says so. Collapsing them would hide the role
    a company opened in six cities behind a single row and lose five real
    application URLs, which is the loss INV-1 forbids.

    Deleting the newer row would lose a URL too, so the row stays and only gains
    a pointer.
    """
    if not key or key.count("|") != 2 or key.strip("|") == "":
        return None
    canonical = conn.execute(
        "SELECT id FROM jobs WHERE dedupe_key = ? AND id != ? AND canonical_id IS NULL"
        " AND ifnull(source,'') != ifnull(?,'') ORDER BY id LIMIT 1",
        (key, job_id, source),
    ).fetchone()
    if not canonical:
        return None
    conn.execute("UPDATE jobs SET canonical_id = ? WHERE id = ?", (canonical["id"], job_id))
    _append_source_url(conn, canonical["id"], url)
    return canonical["id"]


UPDATE_SQL = """
UPDATE jobs SET
    company_id = ?, title = ?, url = ?, posted_at = ?, seen_at = ?,
    jd_text = CASE WHEN ? != '' THEN ? ELSE jd_text END,
    location = ?, employment_type = ?, remote = ?,
    pay_min = ?, pay_max = ?, pay_currency = ?,
    comp_model = ?, hires_in_india = ?, auth_required = ?,
    dedupe_key = ?, raw_json = ?
WHERE id = ?
"""

INSERT_SQL = """
INSERT INTO jobs (company_id, title, url, source, external_id, posted_at, seen_at,
    first_seen_at, jd_text, location, employment_type, remote, pay_min, pay_max,
    pay_currency, comp_model, hires_in_india, auth_required, dedupe_key, raw_json,
    source_urls)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
"""


def upsert(conn, company_id: int, company_name: str, job: NormalizedJob) -> tuple[int, str]:
    """Insert or refresh one posting. Returns (job_id, 'new'|'updated')."""
    now = db.now()
    key = dedupe_key(company_name, job.title, job.posted_at)
    comp_model = normalize.comp_model(job.jd_text, job.pay_currency)
    auth = normalize.auth_required(job.jd_text)
    india = normalize.hires_in_india(job.jd_text, job.location)
    remote = None if job.remote is None else int(job.remote)
    raw = json.dumps(job.raw, default=str)[:200_000] if job.raw else None

    existing = conn.execute(
        "SELECT id FROM jobs WHERE source = ? AND external_id = ?",
        (job.source, job.external_id),
    ).fetchone()

    if existing:
        conn.execute(UPDATE_SQL, (
            company_id, job.title, job.url, job.posted_at, now,
            job.jd_text or "", job.jd_text or "",
            job.location, job.employment_type, remote,
            job.pay_min, job.pay_max, job.pay_currency,
            comp_model, india, auth, key, raw, existing["id"],
        ))
        _append_source_url(conn, existing["id"], job.url)
        return existing["id"], "updated"

    cur = conn.execute(INSERT_SQL, (
        company_id, job.title, job.url, job.source, job.external_id, job.posted_at,
        now, now, job.jd_text or "", job.location, job.employment_type, remote,
        job.pay_min, job.pay_max, job.pay_currency, comp_model, india, auth, key, raw,
        json.dumps([job.url] if job.url else []),
    ))
    job_id = cur.lastrowid
    link_duplicate(conn, job_id, key, job.url, job.source)
    return job_id, "new"


def renormalize(conn) -> int:
    """Re-run the heuristics over stored rows.

    Needed whenever comp_model/auth_required/hires_in_india change: a poll only
    refreshes a row the board still lists, so without this an improved rule
    would never reach the archive. Touches derived columns only.
    """
    rows = conn.execute(
        "SELECT id, jd_text, location, pay_currency FROM jobs"
    ).fetchall()
    for row in rows:
        conn.execute(
            "UPDATE jobs SET comp_model = ?, auth_required = ?, hires_in_india = ? WHERE id = ?",
            (
                normalize.comp_model(row["jd_text"] or "", row["pay_currency"]),
                normalize.auth_required(row["jd_text"] or ""),
                normalize.hires_in_india(row["jd_text"] or "", row["location"]),
                row["id"],
            ),
        )
    conn.commit()
    return len(rows)


def ingest(conn, company_id: int, company_name: str, jobs: list[NormalizedJob]) -> dict:
    stats = {"new": 0, "updated": 0, "skipped": 0}
    for job in jobs:
        if not job.external_id or not job.title:
            db.log_exclusion(
                conn, json.dumps(job.raw, default=str)[:5000],
                reason="posting had no usable id or title",
                rule_id="ingest.incomplete_posting",
            )
            stats["skipped"] += 1
            continue
        try:
            _, outcome = upsert(conn, company_id, company_name, job)
            stats[outcome] += 1
        except Exception as exc:
            # One malformed posting must not abort the other 40 on the board.
            db.log_exclusion(
                conn, json.dumps(job.raw, default=str)[:5000],
                reason=f"upsert failed: {type(exc).__name__}: {exc}",
                rule_id="ingest.upsert_error",
            )
            stats["skipped"] += 1
    conn.commit()
    return stats
