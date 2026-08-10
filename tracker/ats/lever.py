"""Lever postings API.

GET https://api.lever.co/v0/postings/{slug}?mode=json
Returns a bare array. `createdAt` is epoch MILLISECONDS, not seconds — divide
before converting or every posting lands in 1970.
"""
from datetime import datetime, timezone

from .. import http
from ..textutil import as_str, clean, dig, strip_html
from . import FetchResult, NormalizedJob

API = "https://api.lever.co/v0/postings/{slug}?mode=json"
SOURCE = "lever"


def board_url(slug: str) -> str:
    return f"https://jobs.lever.co/{slug}"


def _epoch_ms(value) -> str | None:
    try:
        ms = float(value)
    except (TypeError, ValueError):
        return None
    return datetime.fromtimestamp(ms / 1000.0, tz=timezone.utc).isoformat(timespec="seconds")


def parse(payload) -> list[NormalizedJob]:
    jobs = []
    for item in payload or []:
        if not isinstance(item, dict):
            continue
        external_id = as_str(item.get("id"))
        title = as_str(item.get("text"))
        if not external_id or not title:
            continue
        body = as_str(item.get("descriptionPlain"))
        jobs.append(
            NormalizedJob(
                external_id=external_id,
                title=title,
                url=as_str(item.get("hostedUrl")) or "",
                source=SOURCE,
                posted_at=_epoch_ms(item.get("createdAt")),
                location=as_str(dig(item, "categories", "location")),
                employment_type=as_str(dig(item, "categories", "commitment")),
                jd_text=clean(body) if body else strip_html(item.get("description")),
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
    if not isinstance(payload, list):
        return FetchResult(jobs=None, status=resp.status,
                           error="expected a JSON array of postings")
    return FetchResult(jobs=parse(payload), status=resp.status,
                       etag=resp.etag, last_modified=resp.last_modified)
