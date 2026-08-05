"""Adapter normalization, against real captured responses.

The recurring theme: a provider omitting a field must yield None, never an
exception. One malformed posting killing a poll would take a whole company out
of the pipeline silently, which is the INV-1 failure.
"""
from conftest import fixture

from tracker.ats import ashby, greenhouse, lever


# ------------------------------------------------------------------- greenhouse
def test_greenhouse_parses_real_board():
    jobs = greenhouse.parse(fixture("greenhouse_postman.json"))
    assert len(jobs) == 3
    job = jobs[0]
    assert job.source == "greenhouse"
    assert job.external_id and job.title
    assert job.url.startswith("http")
    assert job.posted_at


def test_greenhouse_content_is_html_escaped_and_gets_unescaped():
    """Greenhouse sends '&lt;p&gt;...' — a naive tag strip leaves the markup in."""
    raw = fixture("greenhouse_postman.json")["jobs"][0]["content"]
    assert "&lt;" in raw, "fixture should still be escaped, or the test proves nothing"

    job = greenhouse.parse(fixture("greenhouse_postman.json"))[0]
    assert "&lt;" not in job.jd_text
    assert "<p>" not in job.jd_text and "<div" not in job.jd_text
    assert "&amp;" not in job.jd_text
    assert len(job.jd_text) > 100


def test_greenhouse_missing_optional_fields():
    payload = {"jobs": [{"id": 7, "title": "Engineer"}]}   # no url, location, content
    job = greenhouse.parse(payload)[0]
    assert job.external_id == "7"
    assert job.url == ""
    assert job.location is None
    assert job.jd_text == ""


def test_greenhouse_skips_postings_without_id_or_title():
    payload = {"jobs": [{"title": "no id"}, {"id": 1}, None, "junk", {"id": 2, "title": "ok"}]}
    jobs = greenhouse.parse(payload)
    assert [j.external_id for j in jobs] == ["2"]


def test_greenhouse_empty_board():
    assert greenhouse.parse({"jobs": []}) == []
    assert greenhouse.parse({}) == []
    assert greenhouse.parse(None) == []


# ------------------------------------------------------------------------ lever
def test_lever_parses_real_board():
    jobs = lever.parse(fixture("lever_epifi.json"))
    assert len(jobs) == 3
    assert all(j.source == "lever" for j in jobs)
    assert jobs[0].location and jobs[0].employment_type


def test_lever_created_at_is_epoch_milliseconds():
    """createdAt is ms. Treating it as seconds puts every posting in 1970."""
    raw = fixture("lever_epifi.json")[0]["createdAt"]
    assert raw > 1_000_000_000_000, "fixture should carry a ms-scale epoch"

    job = lever.parse(fixture("lever_epifi.json"))[0]
    year = int(job.posted_at[:4])
    assert 2015 < year < 2100, f"decoded to {job.posted_at}"


def test_lever_bad_epoch_is_none_not_an_exception():
    for value in (None, "", "not-a-number", {}):
        job = lever.parse([{"id": "a", "text": "T", "createdAt": value}])[0]
        assert job.posted_at is None


def test_lever_missing_categories():
    job = lever.parse([{"id": "a", "text": "Engineer"}])[0]
    assert job.location is None and job.employment_type is None
    assert job.url == ""


def test_lever_falls_back_to_html_description():
    job = lever.parse([{"id": "a", "text": "T", "description": "<p>Hello &amp; welcome</p>"}])[0]
    assert job.jd_text == "Hello & welcome"


def test_lever_empty_array():
    assert lever.parse([]) == []
    assert lever.parse(None) == []


# ------------------------------------------------------------------------ ashby
def test_ashby_parses_real_board():
    jobs = ashby.parse(fixture("ashby_neon.json"))
    assert len(jobs) == 3
    assert all(j.source == "ashby" for j in jobs)


def test_ashby_absent_compensation_yields_none():
    job = ashby.parse(fixture("ashby_neon.json"))[0]
    assert job.pay_min is None and job.pay_max is None and job.pay_currency is None


def test_ashby_extracts_real_compensation():
    jobs = ashby.parse(fixture("ashby_with_comp.json"))
    job = jobs[0]
    assert job.pay_min and job.pay_max
    assert job.pay_min < job.pay_max
    assert job.pay_currency == "USD"


def test_ashby_ignores_non_salary_components():
    payload = {"jobs": [{"id": "1", "title": "T", "compensation": {"summaryComponents": [
        {"compensationType": "Equity", "minValue": 1, "maxValue": 2, "currencyCode": "USD"},
    ]}}]}
    job = ashby.parse(payload)[0]
    assert job.pay_min is None and job.pay_max is None


def test_ashby_malformed_compensation_shapes():
    for comp in (None, {}, "nonsense", {"compensationTiers": None}, {"summaryComponents": [None]}):
        job = ashby.parse({"jobs": [{"id": "1", "title": "T", "compensation": comp}]})[0]
        assert job.pay_min is None


def test_ashby_remote_flag_tri_state():
    assert ashby.parse({"jobs": [{"id": "1", "title": "T", "isRemote": True}]})[0].remote is True
    assert ashby.parse({"jobs": [{"id": "1", "title": "T", "isRemote": False}]})[0].remote is False
    assert ashby.parse({"jobs": [{"id": "1", "title": "T"}]})[0].remote is None


def test_ashby_empty_board():
    assert ashby.parse({"jobs": []}) == []
