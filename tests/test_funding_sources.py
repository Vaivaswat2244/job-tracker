"""Funding source parsing and per-run parser health.

Site redesigns are the expected failure here, so the tests that matter most are
the ones proving a redesign is loud rather than quiet.
"""
import os

import pytest
from conftest import FIXTURES

from tracker import health
from tracker.funding import sources

CONFIGS = {c["name"]: c for c in sources.load_config()}


def fixture_text(name):
    with open(os.path.join(FIXTURES, name), encoding="utf-8") as fh:
        return fh.read()


# -------------------------------------------------------------------- parsing
def test_entrackr_rss():
    items = sources.parse(fixture_text("entrackr_rss.xml"), CONFIGS["entrackr"])
    assert len(items) == 6
    assert all(i.headline and i.url.startswith("http") for i in items)
    assert items[0].published_at.startswith("2026-")
    assert items[0].source == "entrackr"


def test_inc42_rss():
    items = sources.parse(fixture_text("inc42_feed.xml"), CONFIGS["inc42"])
    assert len(items) == 6
    assert all(i.published_at for i in items)


def test_vccircle_uses_embedded_json_not_css():
    """The page's class names are build-hashed, so the JSON path is the durable
    reading of it."""
    items = sources.parse(fixture_text("vccircle_listing.html"), CONFIGS["vccircle"])
    assert len(items) == 6
    assert items[0].url.startswith("https://www.vccircle.com/")
    assert items[0].published_at


def test_selectors_come_from_config_not_code():
    assert CONFIGS["vccircle"]["json_path"] == "props.pageProps.data.all_articles"
    assert all("selector_version" in c for c in CONFIGS.values())


def test_a_redesigned_page_yields_zero_items_not_an_exception():
    items = sources.parse("<html><body>we redesigned</body></html>", CONFIGS["vccircle"])
    assert items == []


def test_malformed_feed_is_reported_not_raised(monkeypatch):
    monkeypatch.setattr(sources.http, "allowed", lambda url: True)
    monkeypatch.setattr(sources.http, "get", lambda url, **k: sources.http.Fetch(
        url=url, status=200, body="<rss><broken"))
    result = sources.fetch(CONFIGS["entrackr"])
    assert result.parse_ok is False
    assert result.items is None
    assert "parse failed" in result.error


def test_empty_feed_parses_to_zero_items():
    assert sources.parse("<rss><channel></channel></rss>", CONFIGS["entrackr"]) == []


# --------------------------------------------------------------------- health
def test_run_records_parse_ok_items_found_and_selector_version(monkeypatch):
    monkeypatch.setattr(sources.http, "allowed", lambda url: True)
    monkeypatch.setattr(sources.http, "get", lambda url, **k: sources.http.Fetch(
        url=url, status=200, body=fixture_text("entrackr_rss.xml")))
    result = sources.fetch(CONFIGS["entrackr"])
    assert result.parse_ok is True
    assert result.items_found == 6
    assert result.selector_version == CONFIGS["entrackr"]["selector_version"]


def test_robots_disallow_is_refused_not_ignored(monkeypatch):
    monkeypatch.setattr(sources.http, "allowed", lambda url: False)
    result = sources.fetch(CONFIGS["vccircle"])
    assert result.parse_ok is False
    assert "robots" in result.error


def test_conditional_request_304_is_not_a_failure(monkeypatch):
    monkeypatch.setattr(sources.http, "allowed", lambda url: True)
    monkeypatch.setattr(sources.http, "get", lambda url, **k: sources.http.Fetch(
        url=url, status=304))
    result = sources.fetch(CONFIGS["entrackr"], etag='W/"x"')
    assert result.not_modified and result.parse_ok
    assert result.etag == 'W/"x"'


def test_source_going_to_zero_after_producing_items_alerts(conn):
    """A site redesign that silently yields nothing must raise the same
    stale_feed alert the ATS poller uses."""
    for count, ok in [(6, True), (0, True), (0, True)]:
        health.record_poll(conn, "funding_source", "entrackr", http_status=200,
                           item_count=count, ok=ok)
    assert health.check(conn, "funding_source", "entrackr", "entrackr feed") is None

    health.record_poll(conn, "funding_source", "entrackr", http_status=200,
                       item_count=0, ok=True)
    assert health.check(conn, "funding_source", "entrackr", "entrackr feed") == "empty"
    alerts = health.open_alerts(conn)
    assert len(alerts) == 1 and alerts[0]["kind"] == "stale_feed"


def test_source_that_never_produced_items_does_not_alert(conn):
    for _ in range(5):
        health.record_poll(conn, "funding_source", "quiet", http_status=200,
                           item_count=0, ok=True)
    assert health.check(conn, "funding_source", "quiet", "quiet feed") is None
