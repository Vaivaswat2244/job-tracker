"""watchlist.yaml: the hand-curated list of companies whose boards we poll.

The YAML file is the source of truth for *membership*; the DB holds the
operational state (last_polled_at, etag, funding window). `sync` pushes the
former into the latter and never the other way round, so the user can always
read and edit the list in git.
"""
import os
from datetime import datetime, timedelta, timezone

import yaml

from . import db

PATH = os.environ.get(
    "TRACKER_WATCHLIST",
    os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "watchlist.yaml"),
)

# Poll cadence by effective priority.
CADENCE_HOURS = {"high": 3, "normal": 6}
FUNDING_WINDOW_DAYS = 60

FIELDS = ("name", "domain", "ats", "slug", "careers_url", "source", "oss_repo",
          "priority", "enabled")


def load(path: str | None = None) -> list[dict]:
    path = path or PATH
    if not os.path.exists(path):
        return []
    with open(path, encoding="utf-8") as fh:
        data = yaml.safe_load(fh) or {}
    companies = data.get("companies") or []
    return [c for c in companies if isinstance(c, dict) and c.get("name")]


def save(companies: list[dict], path: str | None = None) -> None:
    path = path or PATH
    ordered = [{k: c[k] for k in FIELDS if k in c and c[k] is not None} for c in companies]
    with open(path, "w", encoding="utf-8") as fh:
        fh.write("# Hand-curated watchlist. `tracker watchlist add <careers_url>` appends here.\n")
        fh.write("# ats: greenhouse | lever | ashby | unknown   priority: normal | high\n")
        yaml.safe_dump({"companies": ordered}, fh, sort_keys=False, allow_unicode=True,
                       default_flow_style=False, width=100)


def append(entry: dict, path: str | None = None) -> None:
    companies = load(path)
    companies.append(entry)
    save(companies, path)


def find(companies: list[dict], name: str = "", domain: str = "") -> dict | None:
    for c in companies:
        if domain and (c.get("domain") or "").lower() == domain.lower():
            return c
        if name and (c.get("name") or "").lower() == name.lower():
            return c
    return None


# ------------------------------------------------------------------------- sync
def sync(conn, path: str | None = None) -> tuple[int, int]:
    """Upsert watchlist entries into `companies`. Returns (added, updated).

    Membership only — operational columns (last_polled_at, etag, funding state)
    are never touched here.
    """
    added = updated = 0
    for entry in load(path):
        name = str(entry["name"]).strip()
        domain = (entry.get("domain") or "").strip().lower() or None
        row = None
        if domain:
            row = conn.execute(
                "SELECT id FROM companies WHERE lower(domain) = ?", (domain,)
            ).fetchone()
        if not row:
            row = conn.execute(
                "SELECT id FROM companies WHERE lower(name) = lower(?)", (name,)
            ).fetchone()

        values = (
            name,
            domain,
            entry.get("careers_url"),
            (entry.get("ats") or "unknown"),
            entry.get("slug"),
            (entry.get("priority") or "normal"),
            0 if entry.get("enabled") is False else 1,
            entry.get("oss_repo"),
            entry.get("source"),
        )
        if row:
            conn.execute(
                "UPDATE companies SET name=?, domain=COALESCE(?, domain), careers_url=COALESCE(?, careers_url),"
                " ats=?, slug=?, priority=?, watchlist_enabled=?, oss_repo=COALESCE(?, oss_repo),"
                " discovery_source=COALESCE(?, discovery_source) WHERE id=?",
                values + (row["id"],),
            )
            updated += 1
        else:
            conn.execute(
                "INSERT INTO companies (name, domain, careers_url, ats, slug, priority,"
                " watchlist_enabled, oss_repo, discovery_source) VALUES (?,?,?,?,?,?,?,?,?)",
                values,
            )
            added += 1
    conn.commit()
    return added, updated


# --------------------------------------------------------------------- cadence
def effective_priority(row, now: datetime | None = None) -> str:
    """Baseline priority from the YAML, raised to 'high' while a funding window
    is open. Computed rather than stored, so the window decays on its own the
    moment it expires — there is no cron step that can fail to run."""
    now = now or datetime.now(timezone.utc)
    if (row["priority"] or "normal") == "high":
        return "high"
    until = _parse(row["priority_until"] if "priority_until" in row.keys() else None)
    if until and now < until:
        return "high"
    return "normal"


def _parse(value) -> datetime | None:
    if not value:
        return None
    try:
        parsed = datetime.fromisoformat(str(value))
    except ValueError:
        return None
    return parsed if parsed.tzinfo else parsed.replace(tzinfo=timezone.utc)


def due_for_poll(row, now: datetime | None = None) -> bool:
    now = now or datetime.now(timezone.utc)
    last = _parse(row["last_polled_at"])
    if last is None:
        return True
    hours = CADENCE_HOURS[effective_priority(row, now)]
    return now - last >= timedelta(hours=hours)


def pollable(conn) -> list:
    """Watchlist companies with a usable board. `unknown` providers stay in the
    list and stay visible; they are simply not pollable until a slug is known."""
    return conn.execute(
        "SELECT * FROM companies WHERE watchlist_enabled = 1"
        " AND ats IN ('greenhouse','lever','ashby') AND slug IS NOT NULL AND slug != ''"
        " ORDER BY name"
    ).fetchall()


def mark_polled(conn, company_id: int, etag=None, last_modified=None) -> None:
    conn.execute(
        "UPDATE companies SET last_polled_at=?, poll_etag=?, poll_last_modified=? WHERE id=?",
        (db.now(), etag, last_modified, company_id),
    )
    conn.commit()
