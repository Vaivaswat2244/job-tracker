"""Greenhouse job board API.

GET https://boards-api.greenhouse.io/v1/boards/{slug}/jobs?content=true
`content` arrives HTML-escaped, so it is unescaped before tags are stripped.
"""
from .. import http
from ..textutil import as_str, dig, strip_html
from . import FetchResult, NormalizedJob

API = "https://boards-api.greenhouse.io/v1/boards/{slug}/jobs?content=true"
SOURCE = "greenhouse"


def board_url(slug: str) -> str:
    return f"https://job-boards.greenhouse.io/{slug}"


def parse(payload) -> list[NormalizedJob]:
    jobs = []
    for item in (payload or {}).get("jobs") or []:
        if not isinstance(item, dict):
            continue
        external_id = as_str(item.get("id"))
        title = as_str(item.get("title"))
        url = as_str(item.get("absolute_url"))
        if not external_id or not title:
            continue
        jobs.append(
            NormalizedJob(
                external_id=external_id,
                title=title,
                url=url or "",
                source=SOURCE,
                posted_at=as_str(item.get("updated_at")),
                location=as_str(dig(item, "location", "name")),
                jd_text=strip_html(item.get("content")),
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
