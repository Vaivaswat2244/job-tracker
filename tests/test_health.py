"""Feed-death detection.

The alert must fire on the third consecutive failure — not the second (noise)
and not the tenth (by which point the company has been invisible for days).
"""
from tracker import health
from tracker.ats import FetchResult


def fail(status=404):
    return FetchResult(jobs=None, status=status, error=f"HTTP {status}")


def empty():
    return FetchResult(jobs=[], status=200)


def jobs(n=2):
    return FetchResult(jobs=[object()] * n, status=200)


def not_modified():
    return FetchResult(jobs=None, status=304, not_modified=True)


def company(conn, name="Acme"):
    cur = conn.execute("INSERT INTO companies (name) VALUES (?)", (name,))
    conn.commit()
    return cur.lastrowid


# ------------------------------------------------------------------- failures
def test_alert_fires_on_exactly_the_third_consecutive_failure(conn, no_notifications):
    cid = company(conn)
    assert health.poll_health(conn, cid, fail()) is None
    assert len(health.open_alerts(conn)) == 0

    assert health.poll_health(conn, cid, fail()) is None
    assert len(health.open_alerts(conn)) == 0, "two failures is noise, not a dead feed"

    assert health.poll_health(conn, cid, fail()) == "failing"
    alerts = health.open_alerts(conn)
    assert len(alerts) == 1
    assert alerts[0]["kind"] == "stale_feed"
    assert "3 consecutive failed polls" in alerts[0]["detail"]


def test_alert_notifies_once_not_every_poll(conn, no_notifications):
    cid = company(conn)
    for _ in range(6):
        health.poll_health(conn, cid, fail())
    assert len(health.open_alerts(conn)) == 1
    assert len(no_notifications) == 1, "a month-dead feed must not send a month of pings"


def test_a_success_between_failures_resets_the_streak(conn):
    cid = company(conn)
    health.poll_health(conn, cid, fail())
    health.poll_health(conn, cid, fail())
    health.poll_health(conn, cid, jobs())
    health.poll_health(conn, cid, fail())
    health.poll_health(conn, cid, fail())
    assert health.open_alerts(conn) == []


def test_recovery_resolves_an_open_alert(conn):
    cid = company(conn)
    for _ in range(3):
        health.poll_health(conn, cid, fail())
    assert len(health.open_alerts(conn)) == 1

    health.poll_health(conn, cid, jobs())
    assert health.open_alerts(conn) == []


# ---------------------------------------------------------------- empty boards
def test_going_empty_after_producing_jobs_alerts(conn):
    """The silent failure: a renamed slug that 200s with an empty array forever."""
    cid = company(conn)
    health.poll_health(conn, cid, jobs(5))
    health.poll_health(conn, cid, empty())
    health.poll_health(conn, cid, empty())
    assert health.open_alerts(conn) == []

    assert health.poll_health(conn, cid, empty()) == "empty"
    alerts = health.open_alerts(conn)
    assert len(alerts) == 1
    assert "0 items" in alerts[0]["detail"]


def test_a_board_that_was_always_empty_does_not_alert(conn):
    """A genuinely small company with nothing open is not a broken feed."""
    cid = company(conn)
    for _ in range(5):
        health.poll_health(conn, cid, empty())
    assert health.open_alerts(conn) == []


def test_304_neither_extends_nor_breaks_the_empty_streak(conn):
    cid = company(conn)
    health.poll_health(conn, cid, jobs(3))
    health.poll_health(conn, cid, empty())
    health.poll_health(conn, cid, not_modified())
    health.poll_health(conn, cid, empty())
    assert health.open_alerts(conn) == [], "304s are not zero-item polls"

    health.poll_health(conn, cid, empty())
    assert len(health.open_alerts(conn)) == 1


def test_304_counts_as_alive_for_failure_detection(conn):
    cid = company(conn)
    health.poll_health(conn, cid, fail())
    health.poll_health(conn, cid, fail())
    health.poll_health(conn, cid, not_modified())
    health.poll_health(conn, cid, fail())
    assert health.open_alerts(conn) == []


# ------------------------------------------------------------------- behaviour
def test_alerting_company_is_never_auto_disabled(conn):
    cid = company(conn)
    conn.execute("UPDATE companies SET watchlist_enabled = 1, ats='greenhouse', slug='x'"
                 " WHERE id = ?", (cid,))
    conn.commit()
    for _ in range(5):
        health.poll_health(conn, cid, fail())
    row = conn.execute("SELECT watchlist_enabled FROM companies WHERE id = ?", (cid,)).fetchone()
    assert row["watchlist_enabled"] == 1, "auto-disabling is how a company vanishes quietly"


def test_every_poll_is_recorded(conn):
    cid = company(conn)
    health.poll_health(conn, cid, jobs(4))
    health.poll_health(conn, cid, fail(500))
    rows = conn.execute("SELECT * FROM poll_log ORDER BY id").fetchall()
    assert len(rows) == 2
    assert rows[0]["item_count"] == 4 and rows[0]["ok"] == 1
    assert rows[1]["http_status"] == 500 and rows[1]["ok"] == 0


def test_diagnose_is_pure(conn):
    """Streak arithmetic is testable without a database."""
    rows = [{"ok": 0, "http_status": 404, "error": "HTTP 404", "item_count": None}] * 3
    reason, detail = health.diagnose(rows)
    assert reason == "failing" and "404" in detail
    assert health.diagnose([]) == (None, None)
