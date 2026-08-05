"""One full funding run, offline, plus digest integration."""
import os

import pytest
from conftest import FIXTURES

from tracker import digest, http, watchlist
from tracker.funding import resolve, sources
from tracker.funding import run as fr


def fixture_text(name):
    with open(os.path.join(FIXTURES, name), encoding="utf-8") as fh:
        return fh.read()


@pytest.fixture
def config(tmp_path):
    """A single-source config pointed at a saved snapshot."""
    path = tmp_path / "sources.yaml"
    path.write_text(
        "sources:\n"
        "  - name: entrackr\n"
        "    enabled: true\n"
        "    mode: rss\n"
        "    url: https://entrackr.com/rss\n"
        "    selector_version: 1\n", encoding="utf-8")
    return str(path)


@pytest.fixture
def offline_feed(monkeypatch):
    monkeypatch.setattr(http, "allowed", lambda url: True)
    monkeypatch.setattr(sources.http, "allowed", lambda url: True)
    monkeypatch.setattr(sources.http, "get", lambda url, **k: http.Fetch(
        url=url, status=200, body=fixture_text("entrackr_rss.xml")))
    monkeypatch.setattr(resolve, "resolve_domain", lambda *a, **k: (None, "offline"))


@pytest.fixture
def wl(tmp_path, monkeypatch):
    path = tmp_path / "watchlist.yaml"
    monkeypatch.setattr(watchlist, "PATH", str(path))
    return str(path)


def test_full_run_stores_items_and_queues_review(conn, config, offline_feed, wl):
    summary = fr.run(conn, verbose=False, config_path=config)

    assert summary["sources"] == 1
    assert summary["items"] == 6
    assert summary["stored"] >= 1
    assert summary["confirmed"] == 0
    assert summary["needs_review"] == summary["stored"]
    assert conn.execute("SELECT count(*) n FROM companies").fetchone()["n"] == 0


def test_rerunning_stores_nothing_new(conn, config, offline_feed, wl):
    first = fr.run(conn, verbose=False, config_path=config)
    second = fr.run(conn, verbose=False, config_path=config)
    assert second["stored"] == 0
    assert conn.execute("SELECT count(*) n FROM funding_items").fetchone()["n"] == first["stored"]


def test_article_bodies_are_never_stored(conn, config, offline_feed, wl):
    fr.run(conn, verbose=False, config_path=config)
    for row in conn.execute("SELECT * FROM funding_items"):
        # raw_text is the headline extraction ran on, nothing more.
        assert row["raw_text"] == row["headline"]
        assert row["llm_output"] is None


def test_non_funding_items_are_logged_not_silently_dropped(conn, config, offline_feed, wl):
    summary = fr.run(conn, verbose=False, config_path=config)
    if summary["near_miss"]:
        rows = conn.execute(
            "SELECT * FROM excluded_log WHERE rule_id = 'funding.not_a_round'").fetchall()
        assert len(rows) == summary["near_miss"]
        assert all(r["reason"] for r in rows)


def test_run_records_health_for_the_source(conn, config, offline_feed, wl):
    fr.run(conn, verbose=False, config_path=config)
    row = conn.execute(
        "SELECT * FROM poll_log WHERE target_type='funding_source'").fetchone()
    assert row["target_id"] == "entrackr"
    assert row["ok"] == 1 and row["item_count"] == 6
    assert "selector_version" in row["meta"]


def test_second_run_sends_conditional_headers(conn, config, monkeypatch, wl, offline_feed):
    fr.run(conn, verbose=False, config_path=config)
    conn.execute("UPDATE funding_source_state SET etag='W/\"abc\"' WHERE name='entrackr'")
    conn.commit()

    seen = {}

    def capture(url, **kw):
        seen.update(kw)
        return http.Fetch(url=url, status=304)

    monkeypatch.setattr(sources.http, "get", capture)
    fr.run(conn, verbose=False, config_path=config)
    assert seen.get("etag") == 'W/"abc"'


# ------------------------------------------------------------------- digest
def test_digest_puts_alerts_above_everything(conn):
    from tracker import health

    health.raise_alert(conn, "company", 1, "Acme", "Acme: board is returning errors",
                       "3 consecutive failed polls")
    text = digest.build(conn)
    assert text.index("ALERTS") < text.index("NEW ROLES")
    assert "still being polled" in text


def test_digest_shows_funded_companies_with_no_roles_yet(conn):
    """The valuable line: funded, hiring imminent, nothing posted."""
    from tracker import db

    conn.execute(
        "INSERT INTO companies (name, domain, ats, slug, recently_funded_at, funding_stage,"
        " funding_amount_raw) VALUES ('Newco','newco.com','lever','newco',?,'A','$12 Mn')",
        (db.now(),))
    conn.commit()

    text = digest.build(conn)
    assert "RECENTLY FUNDED" in text
    assert "no roles posted yet" in text
    assert "jobs.lever.co/newco" in text
    assert text.index("RECENTLY FUNDED") < text.index("NEW ROLES")


def test_digest_needs_review_is_capped(conn):
    from tracker import db

    for i in range(15):
        conn.execute(
            "INSERT INTO watchlist_candidates (name, status, created_at) VALUES (?,?,?)",
            (f"Company {i}", "needs_review", db.now()))
    conn.commit()

    text = digest.build(conn)
    assert "NEEDS REVIEW" in text
    assert "15 unresolved" in text
    assert text.count("[") >= digest.CANDIDATE_CAP
    listed = [line for line in text.splitlines() if line.strip().startswith("[")]
    assert len(listed) == digest.CANDIDATE_CAP, "an uncappable list stops being read"
