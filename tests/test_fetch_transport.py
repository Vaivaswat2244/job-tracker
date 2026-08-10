"""Adapter behaviour at the transport boundary: 404, 304, garbage bodies.

The distinction that matters everywhere below: `jobs is None` means "we learned
nothing", `jobs == []` means "the board is genuinely empty". Collapsing the two
is what makes a dead feed look like a company that simply is not hiring.
"""
import json

import pytest
from conftest import fixture

from tracker import http
from tracker.ats import ashby, greenhouse, lever

ADAPTERS = [greenhouse, lever, ashby]


@pytest.fixture
def stub_http(monkeypatch):
    def install(**kwargs):
        def fake_get(url, **_):
            return http.Fetch(url=url, **kwargs)
        for module in ADAPTERS:
            monkeypatch.setattr(module.http, "get", fake_get)
    return install


@pytest.mark.parametrize("module", ADAPTERS, ids=lambda m: m.SOURCE)
def test_404_is_a_failure_not_an_empty_board(module, stub_http):
    stub_http(status=404, error="HTTP 404")
    result = module.fetch("wrong-slug")
    assert result.jobs is None, "a 404 must not look like an empty board"
    assert result.ok is False
    assert result.status == 404
    assert "404" in result.error


@pytest.mark.parametrize("module", ADAPTERS, ids=lambda m: m.SOURCE)
def test_304_is_not_modified_and_preserves_validators(module, stub_http):
    stub_http(status=304)
    result = module.fetch("slug", etag='W/"abc"', last_modified="Wed, 21 Oct 2026 07:28:00 GMT")
    assert result.not_modified is True
    assert result.jobs is None
    assert result.etag == 'W/"abc"'
    assert result.last_modified == "Wed, 21 Oct 2026 07:28:00 GMT"


@pytest.mark.parametrize("module", ADAPTERS, ids=lambda m: m.SOURCE)
def test_unparseable_body_is_an_error_not_a_crash(module, stub_http):
    stub_http(status=200, body="<html>maintenance</html>")
    result = module.fetch("slug")
    assert result.jobs is None
    assert result.ok is False
    assert result.error


@pytest.mark.parametrize("module", ADAPTERS, ids=lambda m: m.SOURCE)
def test_connection_failure_surfaces_as_error(module, stub_http):
    stub_http(status=None, error="ConnectionError: refused")
    result = module.fetch("slug")
    assert result.jobs is None and result.ok is False


def test_empty_board_is_ok_with_zero_jobs(stub_http):
    stub_http(status=200, body=json.dumps({"jobs": []}))
    result = greenhouse.fetch("slug")
    assert result.ok is True
    assert result.jobs == []


def test_successful_fetch_returns_parsed_jobs(stub_http):
    stub_http(status=200, body=json.dumps(fixture("greenhouse_postman.json")),
              headers={"ETag": 'W/"v1"'})
    result = greenhouse.fetch("postman")
    assert result.ok and len(result.jobs) == 3
    assert result.etag == 'W/"v1"'


def test_lever_object_body_is_rejected(stub_http):
    """Lever returns a bare array; an object means the endpoint changed shape."""
    stub_http(status=200, body=json.dumps({"error": "nope"}))
    result = lever.fetch("slug")
    assert result.jobs is None and "array" in result.error
