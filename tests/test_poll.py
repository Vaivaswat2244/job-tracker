"""Poll orchestration, end to end with stubbed providers.

This is the acceptance path: `poll --force` twice in a row must produce zero
duplicate rows, and a broken slug must alert within three polls.
"""
import pytest

from tracker import ats, poll, watchlist
from tracker.ats import FetchResult, NormalizedJob


@pytest.fixture
def wl(tmp_path, monkeypatch):
    path = tmp_path / "watchlist.yaml"
    monkeypatch.setattr(watchlist, "PATH", str(path))
    watchlist.save([
        {"name": "Alpha", "domain": "alpha.com", "ats": "greenhouse", "slug": "alpha"},
        {"name": "Beta", "domain": "beta.com", "ats": "lever", "slug": "beta"},
    ])
    return str(path)


def board(slug, count=3):
    return [NormalizedJob(external_id=f"{slug}-{i}", title=f"Engineer {i}",
                          url=f"https://{slug}/{i}", source="greenhouse",
                          posted_at="2026-08-03T00:00:00+00:00") for i in range(count)]


@pytest.fixture
def stub(monkeypatch):
    """Replace every provider module with one that returns a scripted result."""
    state = {}

    class FakeModule:
        @staticmethod
        def fetch(slug, **_):
            result = state.get(slug, FetchResult(jobs=board(slug)))
            return result() if callable(result) else result

    monkeypatch.setattr(ats, "adapter", lambda name: FakeModule)
    return state


def test_poll_ingests_from_the_whole_watchlist(conn, wl, stub):
    summary = poll.run(conn, verbose=False)
    assert summary["polled"] == 2
    assert summary["new"] == 6
    assert conn.execute("SELECT count(*) n FROM jobs").fetchone()["n"] == 6


def test_polling_twice_with_force_creates_no_duplicates(conn, wl, stub):
    poll.run(conn, verbose=False)
    before = conn.execute("SELECT count(*) n FROM jobs").fetchone()["n"]

    summary = poll.run(conn, force=True, verbose=False)
    after = conn.execute("SELECT count(*) n FROM jobs").fetchone()["n"]

    assert summary["new"] == 0
    assert summary["updated"] == before
    assert after == before


def test_cadence_skips_a_company_polled_moments_ago(conn, wl, stub):
    poll.run(conn, verbose=False)
    summary = poll.run(conn, verbose=False)
    assert summary["polled"] == 0
    assert summary["skipped_cadence"] == 2


def test_force_overrides_the_cadence(conn, wl, stub):
    poll.run(conn, verbose=False)
    assert poll.run(conn, force=True, verbose=False)["polled"] == 2


def test_only_restricts_to_one_company(conn, wl, stub):
    summary = poll.run(conn, only="alpha", verbose=False)
    assert summary["polled"] == 1
    companies = conn.execute(
        "SELECT DISTINCT c.name FROM jobs j JOIN companies c ON c.id = j.company_id"
    ).fetchall()
    assert [r["name"] for r in companies] == ["Alpha"]


def test_304_touches_no_rows_but_still_counts_as_a_poll(conn, wl, stub):
    poll.run(conn, verbose=False)
    before = conn.execute("SELECT id, seen_at FROM jobs ORDER BY id").fetchall()

    stub["alpha"] = FetchResult(jobs=None, status=304, not_modified=True)
    stub["beta"] = FetchResult(jobs=None, status=304, not_modified=True)
    summary = poll.run(conn, force=True, verbose=False)

    after = conn.execute("SELECT id, seen_at FROM jobs ORDER BY id").fetchall()
    assert summary["not_modified"] == 2 and summary["new"] == 0
    assert [tuple(r) for r in before] == [tuple(r) for r in after]


def test_broken_slug_alerts_within_three_polls(conn, wl, stub):
    stub["alpha"] = FetchResult(jobs=None, status=404, error="HTTP 404")
    for _ in range(2):
        poll.run(conn, force=True, verbose=False)
    assert conn.execute("SELECT count(*) n FROM alerts WHERE resolved_at IS NULL"
                        ).fetchone()["n"] == 0

    summary = poll.run(conn, force=True, verbose=False)
    assert ("Alpha", "failing") in summary["alerts"]
    alert = conn.execute("SELECT * FROM alerts WHERE resolved_at IS NULL").fetchone()
    assert alert["kind"] == "stale_feed"


def test_a_failing_company_does_not_stop_the_others(conn, wl, stub):
    stub["alpha"] = FetchResult(jobs=None, status=500, error="HTTP 500")
    summary = poll.run(conn, force=True, verbose=False)
    assert summary["failed"] == 1
    assert summary["new"] == 3, "Beta must still be ingested"


def test_an_adapter_that_raises_is_recorded_as_a_failed_poll(conn, wl, stub, monkeypatch):
    class Exploding:
        @staticmethod
        def fetch(slug, **_):
            raise RuntimeError("boom")

    monkeypatch.setattr(ats, "adapter", lambda name: Exploding)
    summary = poll.run(conn, force=True, verbose=False)
    assert summary["failed"] == 2
    rows = conn.execute("SELECT * FROM poll_log").fetchall()
    assert len(rows) == 2 and all("boom" in r["error"] for r in rows)


def test_unknown_provider_never_silently_skips(conn, wl, stub, monkeypatch):
    monkeypatch.setattr(ats, "adapter", lambda name: None)
    summary = poll.run(conn, force=True, verbose=False)
    assert summary["failed"] == 2
    assert conn.execute("SELECT count(*) n FROM poll_log").fetchone()["n"] == 2
