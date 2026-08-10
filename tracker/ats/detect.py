"""Detect which ATS a careers page is backed by, and the board slug.

Detection is best-effort by design. `watchlist add` must never refuse to add a
company because detection failed (INV-1): an entry sitting at ats=unknown is
visible and fixable, a company the user believed they added is not.
"""
import re
from dataclasses import dataclass
from urllib.parse import urljoin

from .. import http

# Slugs that are actually route segments, not boards.
_RESERVED = {"embed", "job_board", "api", "v0", "v1", "jobs", "job", "boards", "static"}

# Ordered: the Greenhouse embed form must win over the bare path form, since
# 'boards.greenhouse.io/embed/job_board?for=acme' matches both and only the
# first one yields the real slug.
_PATTERNS = [
    ("greenhouse", re.compile(
        r"(?:boards|job-boards)\.greenhouse\.io/embed/job_board[^\"'\s<>]*?[?&]for=([A-Za-z0-9_.-]+)")),
    ("greenhouse", re.compile(
        r"(?:boards|job-boards)\.greenhouse\.io/([A-Za-z0-9_.-]+)")),
    ("lever", re.compile(r"jobs\.lever\.co/([A-Za-z0-9_.-]+)")),
    ("ashby", re.compile(r"jobs\.ashbyhq\.com/([A-Za-z0-9_.-]+)")),
]

_IFRAME = re.compile(r"(?i)<iframe[^>]+src=[\"']([^\"']+)[\"']")


@dataclass
class Detection:
    ats: str = "unknown"
    slug: str | None = None
    evidence: str | None = None      # where the match was found
    error: str | None = None

    @property
    def found(self) -> bool:
        return self.ats != "unknown" and bool(self.slug)


def match_text(text: str) -> tuple[str, str] | None:
    """First (provider, slug) found in any URL-shaped string."""
    if not text:
        return None
    for provider, pattern in _PATTERNS:
        for m in pattern.finditer(text):
            slug = m.group(1).strip("/.")
            if slug and slug.lower() not in _RESERVED:
                return provider, slug
    return None


def detect(url: str, *, _depth: int = 0) -> Detection:
    """Check the URL itself, then the page it redirects to, then its body, then
    one level of iframe. Anything deeper is a scraper, which is a non-goal."""
    direct = match_text(url)
    if direct:
        return Detection(ats=direct[0], slug=direct[1], evidence="url")

    resp = http.get(url)
    if not resp.ok:
        return Detection(error=resp.error or f"HTTP {resp.status}")

    if resp.final_url and resp.final_url != url:
        redirected = match_text(resp.final_url)
        if redirected:
            return Detection(ats=redirected[0], slug=redirected[1], evidence="redirect")

    in_body = match_text(resp.body)
    if in_body:
        return Detection(ats=in_body[0], slug=in_body[1], evidence="page body")

    if _depth == 0:
        for src in _IFRAME.findall(resp.body or ""):
            nested = detect(urljoin(resp.final_url or url, src), _depth=1)
            if nested.found:
                nested.evidence = f"iframe -> {nested.evidence}"
                return nested

    return Detection(error="no greenhouse/lever/ashby board found on the page")
