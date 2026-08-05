"""Best-effort JD fetch. A failure here must never cost us the row."""
import re
from urllib.parse import urlparse

UA = "Mozilla/5.0 (X11; Linux x86_64) job-tracker/1.0"
TIMEOUT = 10

# Host -> company name, for ATS domains where the path carries the real company.
_ATS_PATH_COMPANY = {
    "boards.greenhouse.io": 1,
    "job-boards.greenhouse.io": 1,
    "jobs.lever.co": 1,
    "jobs.ashbyhq.com": 1,
    "apply.workable.com": 1,
    "careers.smartrecruiters.com": 1,
}


def company_from_url(url: str) -> str:
    host = (urlparse(url).hostname or "").lower().removeprefix("www.")
    parts = [p for p in urlparse(url).path.split("/") if p]
    idx = _ATS_PATH_COMPANY.get(host)
    if idx and len(parts) >= idx:
        return parts[idx - 1].replace("-", " ").replace("_", " ").title()
    if not host:
        return "Unknown"
    label = host.split(".")[0]
    if label in ("jobs", "careers", "boards", "apply", "job-boards", "hire"):
        label = host.split(".")[1] if len(host.split(".")) > 1 else label
    return label.replace("-", " ").title()


_ATS_HOSTS = set(_ATS_PATH_COMPANY) | {
    "boards-api.greenhouse.io", "api.lever.co", "api.ashbyhq.com", "jobs.workable.com",
    "www.linkedin.com", "linkedin.com", "wellfound.com", "remoteok.com", "himalayas.app",
    "weworkremotely.com", "jobicy.com", "arbeitnow.com", "remotive.com", "instahyre.com",
}


def domain_from_url(url: str) -> str | None:
    """Company domain, or None when the URL belongs to an ATS or aggregator.

    Guessing 'jobs.lever.co' as the company domain would make the contact
    domain-collision guard reject every genuine address at that company.
    """
    host = (urlparse(url).hostname or "").lower().removeprefix("www.")
    if not host or host in _ATS_HOSTS:
        return None
    return host


def _clean(text: str) -> str:
    return re.sub(r"\n{3,}", "\n\n", re.sub(r"[ \t]+", " ", text)).strip()


def fetch_jd(url: str) -> tuple[str, str, str | None]:
    """Return (title, jd_text, error). Never raises: an empty JD beats a lost row."""
    try:
        import requests
        from bs4 import BeautifulSoup

        resp = requests.get(url, headers={"User-Agent": UA}, timeout=TIMEOUT)
        resp.raise_for_status()
        soup = BeautifulSoup(resp.text, "html.parser")

        title = ""
        og = soup.find("meta", property="og:title")
        if og and og.get("content"):
            title = og["content"].strip()
        elif soup.title and soup.title.string:
            title = soup.title.string.strip()
        elif soup.h1:
            title = soup.h1.get_text(" ", strip=True)

        for tag in soup(["script", "style", "nav", "footer", "header", "noscript"]):
            tag.decompose()
        return title, _clean(soup.get_text("\n")), None
    except Exception as exc:  # network, parse, encoding — all non-fatal
        return "", "", f"{type(exc).__name__}: {exc}"
