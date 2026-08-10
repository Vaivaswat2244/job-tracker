"""One HTTP path for every poller: retries, conditional GETs, per-host politeness.

Nothing here raises. A poll that fails must return a Fetch describing the failure
so poll_health can count it — an exception would lose the signal that a feed died.
"""
import json as _json
import os
import random
import threading
import time
import urllib.robotparser
from dataclasses import dataclass, field
from urllib.parse import urlparse

TIMEOUT = 15
ATTEMPTS = 3
BACKOFF_BASE = 1.0          # seconds; doubled per attempt, plus jitter
HOST_DELAY = 1.0            # minimum gap between requests to the same host
RETRY_STATUS = {429}        # plus everything >= 500

VERSION = "1.0"
_CONTACT_ENV = "TRACKER_CONTACT_EMAIL"
_DEFAULT_CONTACT = "vaivaswat2244@gmail.com"


def user_agent() -> str:
    """Identify the tool and a human to complain to. Anonymous polling is rude
    and gets the whole ATS host blocked for everybody."""
    contact = os.environ.get(_CONTACT_ENV, "").strip() or _DEFAULT_CONTACT
    return f"job-tracker/{VERSION} (+personal job search; {contact})"


@dataclass
class Fetch:
    """Outcome of one HTTP attempt sequence. Never an exception."""
    url: str
    final_url: str = ""        # after redirects; ATS detection reads this
    status: int | None = None
    body: str = ""
    headers: dict = field(default_factory=dict)
    error: str | None = None
    attempts: int = 0

    @property
    def ok(self) -> bool:
        return self.status is not None and (200 <= self.status < 300 or self.status == 304)

    @property
    def not_modified(self) -> bool:
        return self.status == 304

    @property
    def etag(self) -> str | None:
        return self.headers.get("ETag")

    @property
    def last_modified(self) -> str | None:
        return self.headers.get("Last-Modified")

    def json(self):
        """Parsed body, or None. A 200 with a broken body is a failure, not a crash."""
        try:
            return _json.loads(self.body)
        except Exception:
            return None


# --------------------------------------------------------------- host politeness
class _HostGate:
    """One in-flight request per host, with a floor on the gap between them.
    Five workers all hammering boards-api.greenhouse.io is how you get a 429."""

    def __init__(self, delay: float = HOST_DELAY):
        self.delay = delay
        self._lock = threading.Lock()
        self._locks: dict[str, threading.Lock] = {}
        self._last: dict[str, float] = {}

    def _for(self, host: str) -> threading.Lock:
        with self._lock:
            return self._locks.setdefault(host, threading.Lock())

    def acquire(self, host: str) -> threading.Lock:
        lock = self._for(host)
        lock.acquire()
        gap = self.delay - (time.monotonic() - self._last.get(host, 0.0))
        if gap > 0:
            time.sleep(gap)
        return lock

    def release(self, host: str, lock: threading.Lock) -> None:
        self._last[host] = time.monotonic()
        lock.release()


_gate = _HostGate()


def _sleep_backoff(attempt: int, retry_after: str | None = None) -> None:
    if retry_after:
        try:
            time.sleep(min(float(retry_after), 30.0))
            return
        except ValueError:
            pass
    time.sleep(BACKOFF_BASE * (2 ** attempt) + random.uniform(0, 0.3))


def get(
    url: str,
    *,
    etag: str | None = None,
    last_modified: str | None = None,
    headers: dict | None = None,
    timeout: int = TIMEOUT,
    attempts: int = ATTEMPTS,
) -> Fetch:
    """Conditional GET with bounded retries. Returns a Fetch, always."""
    import requests

    hdrs = {"User-Agent": user_agent(), "Accept-Encoding": "gzip"}
    if etag:
        hdrs["If-None-Match"] = etag
    if last_modified:
        hdrs["If-Modified-Since"] = last_modified
    hdrs.update(headers or {})

    host = (urlparse(url).hostname or "").lower()
    out = Fetch(url=url)

    for attempt in range(attempts):
        out.attempts = attempt + 1
        lock = _gate.acquire(host)
        try:
            resp = requests.get(url, headers=hdrs, timeout=timeout, allow_redirects=True)
        except Exception as exc:
            resp, out.status, out.error = None, None, f"{type(exc).__name__}: {exc}"
        finally:
            # Released exactly once per acquire, before any sleep — holding the
            # host lock through a backoff would serialise every other worker on it.
            _gate.release(host, lock)

        if resp is None:
            if attempt + 1 < attempts:
                _sleep_backoff(attempt)
                continue
            return out

        out.status = resp.status_code
        out.headers = dict(resp.headers)
        out.final_url = str(getattr(resp, "url", "") or url)
        out.error = None
        if resp.status_code == 304:
            out.body = ""
            return out
        if resp.status_code in RETRY_STATUS or resp.status_code >= 500:
            out.error = f"HTTP {resp.status_code}"
            if attempt + 1 < attempts:
                _sleep_backoff(attempt, resp.headers.get("Retry-After"))
                continue
            return out
        if resp.status_code >= 400:
            # Other 4xx are deterministic. Retrying a 404 just wastes the window.
            out.error = f"HTTP {resp.status_code}"
            return out
        out.body = resp.text
        return out
    return out


# ------------------------------------------------------------------- robots.txt
_robots_cache: dict[str, urllib.robotparser.RobotFileParser | None] = {}
_robots_lock = threading.Lock()


def allowed(url: str) -> bool:
    """robots.txt check. Fails open: an unreachable robots.txt is not consent
    withheld, and blocking the whole run on it would be its own kind of silent loss."""
    parts = urlparse(url)
    root = f"{parts.scheme}://{parts.netloc}"
    with _robots_lock:
        if root not in _robots_cache:
            rp = urllib.robotparser.RobotFileParser()
            rp.set_url(f"{root}/robots.txt")
            try:
                import requests

                resp = requests.get(
                    f"{root}/robots.txt", headers={"User-Agent": user_agent()}, timeout=10
                )
                if resp.status_code >= 400:
                    _robots_cache[root] = None
                else:
                    rp.parse(resp.text.splitlines())
                    _robots_cache[root] = rp
            except Exception:
                _robots_cache[root] = None
        rp = _robots_cache[root]
    return True if rp is None else rp.can_fetch(user_agent(), url)
