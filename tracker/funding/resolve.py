"""Resolve a funded company to a domain, and match it on that domain alone.

Company names collide constantly. Matching "Atlas" the Bangalore fintech to
"Atlas" the US logistics company corrupts the watchlist, and every downstream
consequence — priority, ATS slug, eventually a draft addressed to a stranger —
inherits the error. So:

  1. resolve to a domain, or do not match at all;
  2. match on domain only, never on name;
  3. a name that looks right without domain confirmation is a candidate for
     human review, never an automatic write to `companies`.
"""
import re
from urllib.parse import urlparse

from .. import http

# Publishers, socials, CDNs and shorteners. A link to any of these tells us
# nothing about who raised the money.
_NEVER_A_COMPANY = {
    "entrackr.com", "inc42.com", "vccircle.com", "yourstory.com", "moneycontrol.com",
    "economictimes.indiatimes.com", "indiatimes.com", "livemint.com", "business-standard.com",
    "techcrunch.com", "reuters.com", "bloomberg.com", "forbes.com", "medium.com",
    "twitter.com", "x.com", "facebook.com", "linkedin.com", "instagram.com", "youtube.com",
    "whatsapp.com", "t.me", "telegram.me", "threads.net", "reddit.com", "pinterest.com",
    "google.com", "gstatic.com", "googleapis.com", "doubleclick.net", "gravatar.com",
    "wordpress.com", "wp.com", "cloudflare.com", "amazonaws.com", "publive.online",
    "bit.ly", "tinyurl.com", "crunchbase.com", "tracxn.com", "wikipedia.org",
    "apple.com", "play.google.com", "sharechat.com", "koo.app",
}

_STOPWORDS = {"the", "and", "labs", "technologies", "technology", "systems", "solutions",
              "ventures", "capital", "group", "india", "global", "inc", "ltd", "pvt",
              "limited", "private", "company", "digital", "online", "app", "ai"}

_LINK = re.compile(r"(?i)<a[^>]+href=[\"']([^\"']+)[\"']")


def registrable(host: str) -> str:
    """Close-enough registrable domain. Good enough to compare two hostnames."""
    host = (host or "").lower().strip().removeprefix("www.")
    return host


def _is_publisher(host: str) -> bool:
    return any(host == bad or host.endswith("." + bad) for bad in _NEVER_A_COMPANY)


def name_tokens(name: str) -> list[str]:
    words = re.split(r"[^a-z0-9]+", (name or "").lower())
    return [w for w in words if len(w) >= 3 and w not in _STOPWORDS]


def domain_matches_name(domain: str, name: str) -> bool:
    """Does this hostname plausibly belong to this company?

    Deliberately strict. An outbound link on a funding article is as likely to
    be an investor's site as the company's, and a wrong domain here is worse
    than no domain: no domain sends the item to review, a wrong one confirms a
    match against the wrong company.
    """
    label = registrable(domain).split(".")[0].replace("-", "")
    tokens = name_tokens(name)
    if not label or not tokens:
        return False
    joined = "".join(tokens)
    if label == joined or label == tokens[0]:
        return True
    # "River Mobility" -> rivermobility.com, rivermobility.in
    if joined.startswith(label) and len(label) >= 5:
        return True
    return label.startswith(joined) and len(joined) >= 5


def candidate_domains(html: str) -> list[str]:
    seen = []
    for href in _LINK.findall(html or ""):
        host = registrable(urlparse(href).hostname or "")
        if not host or "." not in host or _is_publisher(host):
            continue
        if host not in seen:
            seen.append(host)
    return seen


TLDS = ("com", "in", "co", "io", "ai")
PREFIXES = ("", "get", "try")
MAX_GUESSES = 7

_PARKED = re.compile(
    r"(?i)(?:domain\s+(?:is\s+)?for\s+sale|buy\s+this\s+domain|parked\s+(?:free\s+)?(?:at|by|domain)"
    r"|this\s+domain\s+(?:may\s+be|is)\s+for\s+sale|godaddy\s+domain|under\s+construction"
    r"|coming\s+soon)"
)


# Only legal-form suffixes. Unlike name_tokens (used for *comparing* names),
# domain generation must keep words like "labs" and "technologies": the domain
# for "InRisk Labs" is far more likely to be inrisklabs.com than inrisk.com.
_LEGAL_ONLY = {"inc", "ltd", "pvt", "limited", "private", "llp", "corp", "co"}


def domain_tokens(name: str) -> list[str]:
    words = re.split(r"[^a-z0-9]+", (name or "").lower())
    return [w for w in words if w and w not in _LEGAL_ONLY]


def guess_domains(name: str) -> list[str]:
    """Plausible domains for a company name — candidates to verify, not answers.

    Only for names with two or more meaningful tokens. A single-token name is
    precisely the collision the guard exists for: 'Atlas' would resolve to
    atlas.com, whose content mentions 'Atlas', and the verification below would
    happily confirm the wrong company. Those go to human review instead.
    """
    tokens = domain_tokens(name)
    if len(tokens) < 2:
        return []
    joined = "".join(tokens)
    if not joined.isalnum() or len(joined) < 4:
        return []

    out = []
    for prefix in PREFIXES:
        for tld in TLDS:
            candidate = f"{prefix}{joined}.{tld}"
            if candidate not in out:
                out.append(candidate)
    return out[:MAX_GUESSES]


def verify_domain(domain: str, name: str) -> bool:
    """Does a real site live here, and does it say it is this company?"""
    resp = http.get(f"https://{domain}")
    if not resp.ok or not resp.body:
        return False
    body = resp.body[:20000].lower()
    if _PARKED.search(body):
        return False
    # The site must name the company, not merely resolve.
    return all(token in body for token in name_tokens(name))


def resolve_domain(article_url: str, company_name: str,
                   allow_guessing: bool = True) -> tuple[str | None, str]:
    """(domain, reason). The article is fetched only to read its outbound links;
    the body is never stored, per the no-article-text rule.

    Two paths, in order of trustworthiness: a link in the article, then a
    name-derived domain that is fetched and confirmed to describe the company.
    Failing both, the answer is None — which sends the item to review rather
    than attaching it to a company it might not be.
    """
    if not article_url or not company_name:
        return None, "no article url or company name"

    if http.allowed(article_url):
        resp = http.get(article_url)
        if resp.ok:
            for host in candidate_domains(resp.body):
                if domain_matches_name(host, company_name):
                    return host, f"confirmed by outbound link in the article ({host})"
        else:
            return None, f"could not fetch article ({resp.error or resp.status})"
    else:
        return None, "article disallowed by robots.txt"

    if not allow_guessing:
        return None, "no outbound link matched the company name"

    guesses = guess_domains(company_name)
    if not guesses:
        return None, ("no outbound link matched, and the name is a single word — "
                      "too collision-prone to resolve by guessing")
    for domain in guesses:
        if verify_domain(domain, company_name):
            return domain, f"no article link; verified {domain} names the company"
    return None, f"no outbound link matched and none of {len(guesses)} candidate domains verified"


# ------------------------------------------------------------------- matching
def match_on_domain(conn, domain: str | None):
    """The only way a funding item is allowed to attach to a company."""
    if not domain:
        return None
    return conn.execute(
        "SELECT * FROM companies WHERE lower(domain) = lower(?)", (registrable(domain),)
    ).fetchone()


def name_collisions(conn, name: str) -> list:
    """Companies whose name looks like this one. Reported, never matched — this
    function exists to explain a near-miss to the user, not to resolve it."""
    tokens = name_tokens(name)
    if not tokens:
        return []
    rows = conn.execute("SELECT id, name, domain FROM companies").fetchall()
    hits = []
    for row in rows:
        other = name_tokens(row["name"])
        if not other:
            continue
        if other == tokens or (tokens[0] == other[0] and len(tokens[0]) >= 4):
            hits.append(row)
    return hits
