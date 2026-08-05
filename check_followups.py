#!/usr/bin/env python3
"""Follow-up scheduler. Run daily by a systemd user timer at 09:30.

Escalation ladder, one notification per stage:
  due (0-2 days over)   -> notify
  nudge (3-6 days over) -> renotify once
  stale (7+ days over)  -> notify and suggest `ghosted`
Auto-transition happens for exactly one case: ghosted at 30 days with no reply.
"""
import sys
from datetime import date

from tracker import db, notify
from tracker.dates import parse_day

GHOST_AFTER_DAYS = 30
CHASE_STATUSES = ("applied", "followed_up")

OPEN_APPS = """
SELECT a.id, a.status, a.applied_at, a.followup_due, j.title, c.name AS company,
       (SELECT ct.name || ' (' || ct.title || ')' FROM contacts ct
         WHERE ct.company_id = c.id ORDER BY ct.id LIMIT 1) AS contact,
       (SELECT max(o.replied_at) FROM outreach o WHERE o.application_id = a.id) AS replied_at
FROM applications a
JOIN jobs j ON j.id = a.job_id
JOIN companies c ON c.id = j.company_id
WHERE a.status IN (?, ?)
ORDER BY a.followup_due
"""


def stage_for(days_over: int) -> str | None:
    if days_over >= 7:
        return "stale"
    if days_over >= 3:
        return "nudge"
    if days_over >= 0:
        return "due"
    return None


def already_notified(conn, app_id: int, stage: str) -> bool:
    return conn.execute(
        "SELECT 1 FROM followup_notices WHERE application_id = ? AND stage = ?",
        (app_id, stage),
    ).fetchone() is not None


def mark_notified(conn, app_id: int, stage: str) -> None:
    conn.execute(
        "INSERT OR IGNORE INTO followup_notices (application_id, stage, notified_at)"
        " VALUES (?,?,?)",
        (app_id, stage, db.now()),
    )
    conn.commit()


def run(conn, today: date | None = None, dry_run: bool = False) -> int:
    today = today or date.today()
    sent = 0

    for r in conn.execute(OPEN_APPS, CHASE_STATUSES).fetchall():
        applied = parse_day(r["applied_at"])
        elapsed = (today - applied).days if applied else 0

        # The one permitted auto-transition.
        if elapsed >= GHOST_AFTER_DAYS and not r["replied_at"]:
            if not dry_run:
                conn.execute("UPDATE applications SET status='ghosted', followup_due=NULL"
                             " WHERE id = ?", (r["id"],))
                conn.commit()
            notify.send(
                f"Ghosted: {r['company']}",
                f"{r['title']} — {elapsed} days since applying, no reply. "
                f"Marked ghosted (app {r['id']}).",
            )
            sent += 1
            continue

        due = parse_day(r["followup_due"])
        if not due:
            continue
        stage = stage_for((today - due).days)
        if not stage or already_notified(conn, r["id"], stage):
            continue

        contact = r["contact"] or "no contact yet — `contact add <company_id>`"
        suffix = {
            "due": "Follow-up due today.",
            "nudge": "Still no movement 3 days after due.",
            "stale": f"7+ days past due — consider `status {r['id']} ghosted`.",
        }[stage]
        notify.send(
            f"Follow up: {r['company']}",
            f"{r['title']}\n{contact}\n{elapsed} days since applying (app {r['id']}). {suffix}",
            urgency="critical" if stage == "stale" else "normal",
        )
        if not dry_run:
            mark_notified(conn, r["id"], stage)
        sent += 1

    print(f"{today}: {sent} notification(s)")
    return sent


if __name__ == "__main__":
    conn = db.connect()
    try:
        run(conn, dry_run="--dry-run" in sys.argv)
    finally:
        conn.close()
