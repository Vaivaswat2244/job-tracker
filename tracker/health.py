"""Feed-death detection.

A wrong slug returns 404 forever. A renamed board returns `[]` forever. Both look
exactly like "this company isn't hiring", and both silently remove a company from
the pipeline — the precise failure INV-1 exists to prevent. So every poll is
recorded, and a feed that stops producing shouts.

Shared by the ATS watchlist (Task A) and the funding sources (Task B): the
failure mode is identical, so the detector is too.
"""
from . import db, notify

FAIL_STREAK = 3        # consecutive non-2xx/non-304 polls
EMPTY_STREAK = 3       # consecutive 0-item polls after the feed had produced items
STALE_FEED = "stale_feed"


def record_poll(conn, target_type: str, target_id, *, http_status=None, item_count=None,
                ok: bool = False, error: str | None = None, meta: str | None = None) -> None:
    conn.execute(
        "INSERT INTO poll_log (target_type, target_id, polled_at, http_status, item_count,"
        " ok, error, meta) VALUES (?,?,?,?,?,?,?,?)",
        (target_type, str(target_id), db.now(), http_status, item_count,
         1 if ok else 0, error, meta),
    )
    conn.commit()


def _recent(conn, target_type: str, target_id, limit: int = 30) -> list:
    return conn.execute(
        "SELECT * FROM poll_log WHERE target_type = ? AND target_id = ?"
        " ORDER BY id DESC LIMIT ?",
        (target_type, str(target_id), limit),
    ).fetchall()


def diagnose(rows: list) -> tuple[str | None, str | None]:
    """(reason, detail) for the most recent polls, newest first. Pure function so
    the streak arithmetic is testable without a database."""
    fails = 0
    for row in rows:
        if row["ok"]:
            break
        fails += 1
    if fails >= FAIL_STREAK:
        last = rows[0]
        status = last["http_status"] or "no response"
        return ("failing", f"{fails} consecutive failed polls (latest: {status}"
                           f"{'; ' + last['error'] if last['error'] else ''})")

    # 304 means "unchanged", which is neither a zero nor a non-zero result: it
    # neither extends nor breaks the empty streak.
    empties = 0
    produced_before = False
    for row in rows:
        if not row["ok"]:
            break
        if row["http_status"] == 304:
            continue
        if row["item_count"] == 0:
            empties += 1
            continue
        produced_before = True
        break
    if empties >= EMPTY_STREAK and produced_before:
        return ("empty", f"{empties} consecutive polls returned 0 items after previously "
                         "returning results — slug or board URL has probably changed")
    return (None, None)


def raise_alert(conn, target_type: str, target_id, label: str, message: str,
                detail: str | None = None, kind: str = STALE_FEED) -> bool:
    """Open an alert if one is not already open. Returns True when newly raised."""
    cur = conn.execute(
        "INSERT OR IGNORE INTO alerts (kind, target_type, target_id, message, detail, raised_at)"
        " VALUES (?,?,?,?,?,?)",
        (kind, target_type, str(target_id), message, detail, db.now()),
    )
    conn.commit()
    if not cur.rowcount:
        return False
    notify.send(f"Feed stale: {label}", f"{message}\n{detail or ''}".strip(),
                urgency="critical")
    conn.execute(
        "UPDATE alerts SET notified_at = ? WHERE kind = ? AND target_type = ?"
        " AND target_id = ? AND resolved_at IS NULL",
        (db.now(), kind, target_type, str(target_id)),
    )
    conn.commit()
    return True


def resolve_alert(conn, target_type: str, target_id, kind: str = STALE_FEED) -> None:
    conn.execute(
        "UPDATE alerts SET resolved_at = ? WHERE kind = ? AND target_type = ?"
        " AND target_id = ? AND resolved_at IS NULL",
        (db.now(), kind, target_type, str(target_id)),
    )
    conn.commit()


def check(conn, target_type: str, target_id, label: str) -> str | None:
    """Evaluate the recorded history and raise or clear the stale_feed alert.

    The target is never disabled. An alerting company keeps its watchlist entry
    and keeps being polled — auto-disabling is how a company disappears quietly.
    """
    rows = _recent(conn, target_type, target_id)
    reason, detail = diagnose(rows)
    if reason is None:
        resolve_alert(conn, target_type, target_id)
        return None
    message = (f"{label}: board is returning errors" if reason == "failing"
               else f"{label}: board went empty")
    raise_alert(conn, target_type, target_id, label, message, detail)
    return reason


def poll_health(conn, company_id: int, poll_result, label: str | None = None) -> str | None:
    """Record one company poll and evaluate feed health. `poll_result` is an
    ats.FetchResult."""
    item_count = None if poll_result.jobs is None else len(poll_result.jobs)
    if poll_result.not_modified:
        item_count = None
    record_poll(
        conn, "company", company_id,
        http_status=poll_result.status,
        item_count=item_count,
        ok=poll_result.ok or poll_result.not_modified,
        error=poll_result.error,
    )
    if label is None:
        row = conn.execute("SELECT name FROM companies WHERE id = ?", (company_id,)).fetchone()
        label = row["name"] if row else f"company {company_id}"
    return check(conn, "company", company_id, label)


def open_alerts(conn) -> list:
    return conn.execute(
        "SELECT * FROM alerts WHERE resolved_at IS NULL ORDER BY raised_at"
    ).fetchall()
