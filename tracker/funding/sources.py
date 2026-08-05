"""Fetch and parse each funding source per its config entry.

Site redesigns are the expected failure mode, so every run records parse_ok,
items_found and selector_version, and a source that previously produced items
and now produces none raises the same stale_feed alert the ATS poller uses.
"""
import json
import os
import xml.etree.ElementTree as ET
from email.utils import parsedate_to_datetime
from urllib.parse import urljoin

import yaml

from .. import http
from . import FeedItem, SourceResult

CONFIG_PATH = os.environ.get(
    "TRACKER_FUNDING_SOURCES",
    os.path.join(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))),
                 "funding_sources.yaml"),
)


def load_config(path: str | None = None) -> list[dict]:
    path = path or CONFIG_PATH
    if not os.path.exists(path):
        return []
    with open(path, encoding="utf-8") as fh:
        data = yaml.safe_load(fh) or {}
    return [s for s in (data.get("sources") or []) if isinstance(s, dict) and s.get("name")]


# ----------------------------------------------------------------------- dates
def _to_iso(value) -> str | None:
    if not value:
        return None
    text = str(value).strip()
    try:
        return parsedate_to_datetime(text).isoformat(timespec="seconds")
    except (TypeError, ValueError):
        pass
    from ..normalize import parse_dt

    parsed = parse_dt(text.replace(" ", "T", 1) if " " in text else text)
    return parsed.isoformat(timespec="seconds") if parsed else None


# ------------------------------------------------------------------- rss/atom
def parse_rss(body: str, source: str) -> list[FeedItem]:
    root = ET.fromstring(body.encode("utf-8") if isinstance(body, str) else body)

    def text(node, *names):
        for name in names:
            for child in node:
                if child.tag.split("}")[-1] == name and (child.text or "").strip():
                    return child.text.strip()
        return None

    items = []
    nodes = root.findall(".//item") or root.findall(".//{http://www.w3.org/2005/Atom}entry")
    for node in nodes:
        headline = text(node, "title")
        link = text(node, "link")
        if not link:
            for child in node:
                if child.tag.split("}")[-1] == "link" and child.get("href"):
                    link = child.get("href")
                    break
        if not headline or not link:
            continue
        items.append(FeedItem(
            headline=headline,
            url=link.strip(),
            published_at=_to_iso(text(node, "pubDate", "published", "updated")),
            source=source,
        ))
    return items


# ------------------------------------------------------------------ json_path
def _dig_path(payload, path: str):
    node = payload
    for part in path.split("."):
        if isinstance(node, dict):
            node = node.get(part)
        else:
            return None
    return node


def parse_json_path(body: str, config: dict, source: str) -> list[FeedItem]:
    from bs4 import BeautifulSoup

    soup = BeautifulSoup(body, "html.parser")
    tag = soup.find("script", id=config.get("json_script_id", "__NEXT_DATA__"))
    if not tag or not tag.string:
        return []
    payload = json.loads(tag.string)
    rows = _dig_path(payload, config.get("json_path", "")) or []
    fields = config.get("fields") or {}
    prefix = config.get("link_prefix", "")

    items = []
    for row in rows:
        if not isinstance(row, dict):
            continue
        headline = row.get(fields.get("title", "title"))
        slug = row.get(fields.get("slug", "slug")) or row.get("url")
        if not headline or not slug:
            continue
        items.append(FeedItem(
            headline=str(headline).strip(),
            url=slug if str(slug).startswith("http") else urljoin(prefix, str(slug)),
            published_at=_to_iso(row.get(fields.get("published", "publish"))),
            source=source,
        ))
    return items


# ------------------------------------------------------------------------ css
def parse_css(body: str, config: dict, source: str) -> list[FeedItem]:
    from bs4 import BeautifulSoup

    selectors = config.get("css_fallback") or config.get("selectors") or {}
    if not selectors.get("item"):
        return []
    soup = BeautifulSoup(body, "html.parser")
    prefix = config.get("link_prefix", "")

    items = []
    for node in soup.select(selectors["item"]):
        link_node = node.select_one(selectors.get("link", "a"))
        title_node = node.select_one(selectors.get("title", "a"))
        if not link_node or not link_node.get("href"):
            continue
        headline = (title_node.get_text(" ", strip=True) if title_node else "") or \
                   (link_node.get("title") or "")
        if not headline:
            img = node.find("img")
            headline = (img.get("alt") or "") if img else ""
        if not headline:
            continue
        href = link_node["href"]
        items.append(FeedItem(
            headline=headline.strip(),
            url=href if href.startswith("http") else urljoin(prefix, href),
            source=source,
        ))
    return items


PARSERS = {"rss": None, "json_path": None, "css": None}   # documented modes


def parse(body: str, config: dict) -> list[FeedItem]:
    """Dispatch on the configured mode, falling back to CSS when the primary
    mode yields nothing but a fallback is configured."""
    name = config.get("name", "")
    mode = config.get("mode", "rss")
    if mode == "rss":
        items = parse_rss(body, name)
    elif mode == "json_path":
        items = parse_json_path(body, config, name)
    else:
        items = parse_css(body, config, name)

    if not items and mode != "css" and config.get("css_fallback"):
        items = parse_css(body, config, name)
    return items


# ---------------------------------------------------------------------- fetch
def fetch(config: dict, etag=None, last_modified=None) -> SourceResult:
    """One request per source per run, conditional, robots-respecting."""
    version = int(config.get("selector_version", 1))
    url = config.get("url") or ""
    if not url:
        return SourceResult(items=None, error="no url configured", selector_version=version)

    if not http.allowed(url):
        return SourceResult(items=None, error="disallowed by robots.txt",
                            selector_version=version)

    resp = http.get(url, etag=etag, last_modified=last_modified)
    if resp.not_modified:
        return SourceResult(items=None, not_modified=True, parse_ok=True, status=304,
                            selector_version=version, etag=etag, last_modified=last_modified)
    if not resp.ok:
        return SourceResult(items=None, status=resp.status,
                            error=resp.error or f"HTTP {resp.status}",
                            selector_version=version)
    try:
        items = parse(resp.body, config)
    except Exception as exc:
        # A parse failure is a site redesign until proven otherwise, and must be
        # loud rather than silently yielding zero items.
        return SourceResult(items=None, status=resp.status, selector_version=version,
                            error=f"parse failed: {type(exc).__name__}: {exc}")

    return SourceResult(items=items, parse_ok=True, items_found=len(items),
                        selector_version=version, status=resp.status,
                        etag=resp.etag, last_modified=resp.last_modified)
