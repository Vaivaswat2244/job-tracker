"""Watchlist YAML, ATS detection, and poll cadence."""
from datetime import datetime, timedelta, timezone

import pytest

from tracker import watchlist
from tracker.ats import detect


@pytest.fixture
def wl(tmp_path, monkeypatch):
    path = tmp_path / "watchlist.yaml"
    monkeypatch.setattr(watchlist, "PATH", str(path))
    return str(path)


# ------------------------------------------------------------------ detection
@pytest.mark.parametrize("url,ats,slug", [
    ("https://boards.greenhouse.io/acme", "greenhouse", "acme"),
    ("https://job-boards.greenhouse.io/acme/jobs/123", "greenhouse", "acme"),
    ("https://jobs.lever.co/acme", "lever", "acme"),
    ("https://jobs.lever.co/acme/abc-def", "lever", "acme"),
    ("https://jobs.ashbyhq.com/acme", "ashby", "acme"),
])
def test_detects_provider_and_slug_from_a_board_url(url, ats, slug):
    result = detect.detect(url)
    assert (result.ats, result.slug) == (ats, slug)
    assert result.evidence == "url"


def test_detects_greenhouse_embed_form():
    """The embed URL matches the bare path pattern too; the slug lives in the
    query string, so ordering decides whether the answer is 'acme' or 'embed'."""
    result = detect.detect("https://boards.greenhouse.io/embed/job_board?for=acme")
    assert (result.ats, result.slug) == ("greenhouse", "acme")


def test_reserved_path_segments_are_not_slugs():
    assert detect.match_text("https://boards.greenhouse.io/embed/") is None


def test_finds_a_board_linked_from_page_markup():
    html = '<html><a href="https://jobs.ashbyhq.com/acme">See openings</a></html>'
    assert detect.match_text(html) == ("ashby", "acme")


def test_no_match_returns_unknown_not_an_exception():
    assert detect.match_text("<html>no board here</html>") is None
    assert detect.Detection().found is False


# -------------------------------------------------------------------- yaml io
def test_roundtrip_preserves_fields(wl):
    entry = {"name": "Acme", "domain": "acme.com", "ats": "lever", "slug": "acme",
             "careers_url": "https://jobs.lever.co/acme", "source": "peakxv",
             "oss_repo": "acme/core", "priority": "high", "enabled": True}
    watchlist.save([entry])
    loaded = watchlist.load()
    assert len(loaded) == 1
    assert {k: loaded[0][k] for k in entry} == entry


def test_missing_file_is_an_empty_list_not_a_crash(wl):
    assert watchlist.load() == []


def test_append_keeps_existing_entries(wl):
    watchlist.save([{"name": "A", "ats": "lever", "slug": "a"}])
    watchlist.append({"name": "B", "ats": "ashby", "slug": "b"})
    assert [e["name"] for e in watchlist.load()] == ["A", "B"]


def test_find_matches_on_name_or_domain(wl):
    entries = [{"name": "Acme", "domain": "acme.com"}]
    assert watchlist.find(entries, name="ACME")
    assert watchlist.find(entries, domain="ACME.COM")
    assert watchlist.find(entries, name="Other") is None


# ----------------------------------------------------------------------- sync
def test_sync_inserts_and_is_idempotent(conn, wl):
    watchlist.save([{"name": "Acme", "domain": "acme.com", "ats": "lever", "slug": "acme"}])
    assert watchlist.sync(conn) == (1, 0)
    assert watchlist.sync(conn) == (0, 1)
    assert conn.execute("SELECT count(*) n FROM companies").fetchone()["n"] == 1


def test_sync_does_not_clobber_poll_state(conn, wl):
    watchlist.save([{"name": "Acme", "domain": "acme.com", "ats": "lever", "slug": "acme"}])
    watchlist.sync(conn)
    cid = conn.execute("SELECT id FROM companies").fetchone()["id"]
    watchlist.mark_polled(conn, cid, etag='W/"1"')

    watchlist.sync(conn)
    row = conn.execute("SELECT * FROM companies").fetchone()
    assert row["last_polled_at"] and row["poll_etag"] == 'W/"1"'


def test_unknown_ats_entries_are_kept_but_not_pollable(conn, wl):
    watchlist.save([
        {"name": "Known", "ats": "lever", "slug": "known"},
        {"name": "Unknown", "ats": "unknown"},
    ])
    watchlist.sync(conn)
    assert conn.execute("SELECT count(*) n FROM companies").fetchone()["n"] == 2
    assert [r["name"] for r in watchlist.pollable(conn)] == ["Known"]


def test_disabled_entries_are_not_polled(conn, wl):
    watchlist.save([{"name": "Off", "ats": "lever", "slug": "off", "enabled": False}])
    watchlist.sync(conn)
    assert watchlist.pollable(conn) == []


# -------------------------------------------------------------------- cadence
def make(priority="normal", last=None, until=None):
    """Stand-in for a sqlite3.Row: dict already offers the .keys() lookup used
    by effective_priority."""
    return dict(priority=priority, last_polled_at=last, priority_until=until)


def test_never_polled_is_always_due():
    assert watchlist.due_for_poll(make()) is True


def test_high_priority_polls_every_three_hours():
    now = datetime.now(timezone.utc)
    two_hours = (now - timedelta(hours=2)).isoformat()
    four_hours = (now - timedelta(hours=4)).isoformat()
    assert watchlist.due_for_poll(make("high", two_hours)) is False
    assert watchlist.due_for_poll(make("high", four_hours)) is True


def test_normal_priority_polls_every_six_hours():
    now = datetime.now(timezone.utc)
    four_hours = (now - timedelta(hours=4)).isoformat()
    seven_hours = (now - timedelta(hours=7)).isoformat()
    assert watchlist.due_for_poll(make("normal", four_hours)) is False
    assert watchlist.due_for_poll(make("normal", seven_hours)) is True


def test_funding_window_raises_effective_priority():
    future = (datetime.now(timezone.utc) + timedelta(days=30)).isoformat()
    assert watchlist.effective_priority(make("normal", until=future)) == "high"


def test_expired_funding_window_decays_without_a_cleanup_job():
    """The decay is computed, so it cannot be missed by a cron that failed."""
    past = (datetime.now(timezone.utc) - timedelta(days=1)).isoformat()
    assert watchlist.effective_priority(make("normal", until=past)) == "normal"
