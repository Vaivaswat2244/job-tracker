"""The daily digest: everything the user should look at today, in one screen.

Ordering is deliberate. Alerts come first because a dead feed makes every
section below it a lie by omission. Funding comes next because that window is
the only place in this pipeline where being early is structurally possible.
"""
from datetime import datetime, timedelta, timezone

from . import health

NEW_JOB_HOURS = 48
CANDIDATE_CAP = 10          # keep needs-review clearable, or it stops being read
FUNDING_WINDOW_DAYS = 60


def _iso(dt: datetime) -> str:
    return dt.isoformat(timespec="seconds")


def _days_ago(value) -> int | None:
    from .normalize import parse_dt

    parsed = parse_dt(value)
    if not parsed:
        return None
    return (datetime.now(timezone.utc) - parsed).days


# ------------------------------------------------------------------- sections
def alerts_section(conn) -> list[str]:
    rows = health.open_alerts(conn)
    if not rows:
        return []
    lines = [f"ALERTS — {len(rows)} feed(s) need attention"]
    for row in rows:
        lines.append(f"  ! {row['message']}")
        if row["detail"]:
            lines.append(f"      {row['detail']}")
    lines.append("  These companies are still being polled. Fix the slug in watchlist.yaml.")
    return lines


def funded_section(conn) -> list[str]:
    cutoff = _iso(datetime.now(timezone.utc) - timedelta(days=FUNDING_WINDOW_DAYS))
    rows = conn.execute(
        "SELECT c.id, c.name, c.funding_stage, c.funding_amount_raw, c.recently_funded_at,"
        " c.ats, c.slug,"
        " (SELECT count(*) FROM jobs j WHERE j.company_id = c.id AND j.canonical_id IS NULL)"
        "   AS role_count"
        " FROM companies c WHERE c.recently_funded_at IS NOT NULL AND c.recently_funded_at >= ?"
        " ORDER BY c.recently_funded_at DESC",
        (cutoff,),
    ).fetchall()
    if not rows:
        return []
    lines = ["RECENTLY FUNDED — hiring window open"]
    for row in rows:
        days = _days_ago(row["recently_funded_at"])
        when = f"{days} days ago" if days is not None else "recently"
        stage = row["funding_stage"] or "unknown round"
        amount = row["funding_amount_raw"] or "undisclosed"
        board = _board_link(row["ats"], row["slug"])
        if row["role_count"]:
            tail = f"{row['role_count']} open roles — {board}"
        else:
            # The valuable line: funded, hiring imminent, nothing posted yet.
            tail = f"no roles posted yet — [watching] {board}"
        lines.append(f"  {row['name']} — {stage}, {amount}, {when} — {tail}")
    return lines


def _board_link(ats: str | None, slug: str | None) -> str:
    if not slug:
        return "[no board known]"
    return {
        "greenhouse": f"https://job-boards.greenhouse.io/{slug}",
        "lever": f"https://jobs.lever.co/{slug}",
        "ashby": f"https://jobs.ashbyhq.com/{slug}",
    }.get(ats or "", "[no board known]")


def candidates_section(conn) -> list[str]:
    rows = conn.execute(
        "SELECT * FROM watchlist_candidates WHERE status = 'needs_review'"
        " ORDER BY announced_at DESC, id DESC LIMIT ?",
        (CANDIDATE_CAP,),
    ).fetchall()
    if not rows:
        return []
    total = conn.execute(
        "SELECT count(*) AS n FROM watchlist_candidates WHERE status = 'needs_review'"
    ).fetchone()["n"]
    lines = [f"NEEDS REVIEW — {total} unresolved funding match(es)"
             + (f", showing {len(rows)}" if total > len(rows) else "")]
    for row in rows:
        detected = (f"{row['resolved_ats']}/{row['resolved_slug']}"
                    if row["resolved_slug"] else "ats unknown")
        lines.append(
            f"  [{row['id']:>3}] {row['name']} ({row['domain'] or 'no domain'}) — "
            f"{row['round_stage'] or '?'}, {row['amount_raw'] or 'undisclosed'} — {detected}"
        )
        if row["reason"]:
            lines.append(f"        {row['reason']}")
    lines.append("  approve: tracker candidate approve <id>   dismiss: tracker candidate reject <id>")
    return lines


def new_roles_section(conn, hours: int = NEW_JOB_HOURS) -> list[str]:
    cutoff = _iso(datetime.now(timezone.utc) - timedelta(hours=hours))
    rows = conn.execute(
        "SELECT j.id, j.title, j.location, j.url, j.auth_required, j.hires_in_india,"
        " j.comp_model, c.name AS company"
        " FROM jobs j JOIN companies c ON c.id = j.company_id"
        " WHERE j.canonical_id IS NULL AND j.first_seen_at IS NOT NULL AND j.first_seen_at >= ?"
        # auth_required sorts a role lower; it never removes it (INV-1).
        " ORDER BY j.auth_required ASC, (j.hires_in_india = 1) DESC, j.first_seen_at DESC",
        (cutoff,),
    ).fetchall()
    if not rows:
        return [f"NEW ROLES — none in the last {hours}h"]
    lines = [f"NEW ROLES — {len(rows)} in the last {hours}h"]
    for row in rows:
        flags = []
        if row["auth_required"]:
            flags.append("needs US/EU auth")
        if row["hires_in_india"] == 1:
            flags.append("india/global")
        if row["comp_model"] and row["comp_model"] != "unknown":
            flags.append(row["comp_model"])
        suffix = f"  [{', '.join(flags)}]" if flags else ""
        lines.append(f"  [{row['id']:>4}] {row['company']} — {row['title']}"
                     f" ({row['location'] or 'location n/a'}){suffix}")
        lines.append(f"         {row['url']}")
    lines.append("  apply: tracker apply <job_id>")
    return lines


def build(conn, hours: int = NEW_JOB_HOURS) -> str:
    today = datetime.now(timezone.utc).date()
    blocks = [[f"JOB PIPELINE DIGEST — {today}"]]
    for section in (alerts_section(conn), funded_section(conn),
                    candidates_section(conn), new_roles_section(conn, hours)):
        if section:
            blocks.append(section)
    return "\n\n".join("\n".join(block) for block in blocks)
