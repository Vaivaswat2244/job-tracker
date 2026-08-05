"""Ashby job board posting API.

GET https://api.ashbyhq.com/posting-api/job-board/{slug}?includeCompensation=true

Compensation is nested and optional at every level: a board may send no
`compensation` key, a summary string with no numbers, or tiers whose components
are equity rather than salary. Everything below tolerates all three.
"""
from .. import http
from ..textutil import as_float, as_str, clean, dig, strip_html
from . import FetchResult, NormalizedJob

API = "https://api.ashbyhq.com/posting-api/job-board/{slug}?includeCompensation=true"
SOURCE = "ashby"


def board_url(slug: str) -> str:
    return f"https://jobs.ashbyhq.com/{slug}"


def _components(comp) -> list[dict]:
    """Every salary component anywhere in the compensation blob."""
    if not isinstance(comp, dict):
        return []
    found = list(comp.get("summaryComponents") or [])
    for tier in comp.get("compensationTiers") or []:
        if isinstance(tier, dict):
            found.extend(tier.get("components") or [])
    return [
        c for c in found
        if isinstance(c, dict) and str(c.get("compensationType", "")).lower() == "salary"
    ]


def _pay(comp) -> tuple[float | None, float | None, str | None]:
    lows, highs, currency = [], [], None
    for c in _components(comp):
        low, high = as_float(c.get("minValue")), as_float(c.get("maxValue"))
        if low is not None:
            lows.append(low)
        if high is not None:
            highs.append(high)
        currency = currency or as_str(c.get("currencyCode"))
    return (min(lows) if lows else None, max(highs) if highs else None, currency)


def parse(payload) -> list[NormalizedJob]:
    jobs = []
    for item in (payload or {}).get("jobs") or []:
        if not isinstance(item, dict):
            continue
        external_id = as_str(item.get("id"))
        title = as_str(item.get("title"))
        if not external_id or not title:
            continue
        pay_min, pay_max, currency = _pay(item.get("compensation"))
        body = as_str(item.get("descriptionPlain"))
        remote = item.get("isRemote")
        jobs.append(
            NormalizedJob(
                external_id=external_id,
                title=title,
                url=as_str(item.get("jobUrl")) or "",
                source=SOURCE,
                posted_at=as_str(item.get("publishedAt")),
                location=as_str(item.get("location")) or as_str(dig(item, "address", "postalAddress", "addressLocality")),
                employment_type=as_str(item.get("employmentType")),
                remote=bool(remote) if isinstance(remote, bool) else None,
                jd_text=clean(body) if body else strip_html(item.get("descriptionHtml")),
                pay_min=pay_min,
                pay_max=pay_max,
                pay_currency=currency,
                raw=item,
            )
        )
    return jobs


def fetch(slug: str, *, etag=None, last_modified=None) -> FetchResult:
    resp = http.get(API.format(slug=slug), etag=etag, last_modified=last_modified)
    if resp.not_modified:
        return FetchResult(jobs=None, status=304, not_modified=True,
                           etag=etag, last_modified=last_modified)
    if not resp.ok:
        return FetchResult(jobs=None, status=resp.status,
                           error=resp.error or f"HTTP {resp.status}")
    payload = resp.json()
    if payload is None:
        return FetchResult(jobs=None, status=resp.status, error="unparseable JSON body")
    return FetchResult(jobs=parse(payload), status=resp.status,
                       etag=resp.etag, last_modified=resp.last_modified)
