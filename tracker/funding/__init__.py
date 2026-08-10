"""Tier 3: funding-signal ingest.

Detects funding announcements and uses them to drive watchlist priority. It
applies to nothing and contacts nobody — per INV-2 there is no send path here,
and per deliverable 4 no company enters the watchlist without user approval.
"""
from dataclasses import dataclass, field


@dataclass
class FeedItem:
    """One article as listed. Bodies are never stored: headline, link and date
    are all this pipeline is entitled to keep."""
    headline: str
    url: str
    published_at: str | None = None
    source: str = ""


@dataclass
class SourceResult:
    """Per-run parser health, recorded for every source on every run."""
    items: list[FeedItem] | None
    parse_ok: bool = False
    items_found: int = 0
    selector_version: int = 0
    status: int | None = None
    error: str | None = None
    not_modified: bool = False
    etag: str | None = None
    last_modified: str | None = None


@dataclass
class Extraction:
    company_name: str | None = None
    round_stage: str = "unknown"
    amount_raw: str | None = None
    currency: str | None = None
    investors: list[str] = field(default_factory=list)
    announced_at: str | None = None
    article_url: str = ""
    confidence: str = "low"
    method: str = "rules"
    raw_text: str = ""          # the text extraction actually ran on
    llm_raw: str | None = None  # unparsed model response, kept for auditing
