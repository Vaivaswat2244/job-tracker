"""Extract funding entities from a headline.

Rules first, always. The LLM path exists but is off by default and never runs
unless the rules left something empty: a deterministic miss is auditable and a
hallucinated company name is not.

Partial extraction is never a reason to discard an item. A row with only a
company name and a date is still useful; it is stored with
extraction_confidence='low' and surfaces in the needs-review section.
"""
import os
import re

import yaml

from . import Extraction

RULES_PATH = os.environ.get(
    "TRACKER_FUNDING_RULES",
    os.path.join(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))),
                 "funding_rules.yaml"),
)

_cache: dict = {}


def rules(path: str | None = None) -> dict:
    path = path or RULES_PATH
    if path not in _cache:
        with open(path, encoding="utf-8") as fh:
            _cache[path] = yaml.safe_load(fh) or {}
    return _cache[path]


def _any(patterns, text) -> bool:
    return any(re.search(p, text, re.I) for p in patterns or [])


# The verb that separates the company from the rest of the headline.
_FUNDING_VERB = re.compile(
    r"(?i)\b(raise[sd]?|raising|bags?|bagged|secures?|secured|nets?|netted|garners?|"
    r"garnered|closes?|closed|mops?\s+up|picks?\s+up|lands?|landed|scoops?\s+up|gets?|"
    r"receives?|attracts?|kicks?\s+off|wraps?\s+up|opens?)\b"
)

# Placeholders that appear where an investor name would be. Storing these as
# investors would make the field useless for the one thing it is for.
_NOT_AN_INVESTOR = re.compile(
    r"(?i)^(?:others?|among|more|existing\s+(?:investors?|backers?|shareholders?)|"
    r"investors?|backers?|new\s+and\s+existing|its\s+\w+)$"
)

_AMOUNT = re.compile(r"""(?ix)
    (?:[$₹]|\bUSD\b|\bINR\b|\bRs\.?\s*)\s*
    \d[\d,.]*
    (?:\s*[-\s])?
    (?:mn|mln|million|bn|billion|cr|crore|lakh|k|m\b)?
  | \d[\d,.]*\s*(?:mn|mln|million|bn|billion|cr|crore|lakh)\b
""")

_INVESTORS = re.compile(
    r"(?i)\b(?:led\s+by|backed\s+by|from)\s+(.+?)(?:\s+(?:to|for|in|at|as|amid|after)\s|$)"
)
_SPLIT_INVESTORS = re.compile(r"(?i)\s*(?:,|\band\b|&|\+)\s*")


def is_funding(headline: str, rule_set: dict | None = None) -> bool:
    r = rule_set or rules()
    if _any(r.get("anti_triggers"), headline):
        return False
    return _any(r.get("triggers"), headline)


def is_near_miss(headline: str, rule_set: dict | None = None) -> bool:
    """Mentions money but tripped no trigger — worth logging, not storing."""
    r = rule_set or rules()
    return _any(r.get("near_miss"), headline)


def round_stage(headline: str, rule_set: dict | None = None) -> str:
    for entry in (rule_set or rules()).get("stages") or []:
        if re.search(entry["pattern"], headline, re.I):
            return entry["stage"]
    return "unknown"


def currency(text: str, rule_set: dict | None = None) -> str | None:
    for entry in (rule_set or rules()).get("currencies") or []:
        if re.search(entry["pattern"], text or "", re.I):
            return entry["currency"]
    return None


def amount(headline: str) -> str | None:
    match = _AMOUNT.search(headline or "")
    return " ".join(match.group(0).split()) if match else None


def company_name(headline: str, rule_set: dict | None = None) -> str | None:
    """Everything before the funding verb, minus the descriptive noise."""
    r = rule_set or rules()
    text = headline or ""

    # Split at the verb FIRST. The descriptor rules are greedy by necessity
    # ("Personal assistance startup Hulp"), and run against a whole headline
    # they happily consume the verb too — "Acme secures venture debt funding"
    # would strip through "venture" and leave nothing attributable.
    verb = _FUNDING_VERB.search(text)
    if not verb:
        # "Series A funding for Acme" style. Give up rather than guess.
        return None
    text = text[:verb.start()]

    for pattern in r.get("name_prefixes") or []:
        text = re.sub(pattern, "", text, flags=re.I)

    # "Simplismart set to raise $9 Mn": the verb split leaves the auxiliary
    # attached to the name. Announced-but-not-closed rounds are still signal,
    # so trim the auxiliary rather than dropping the item.
    # Applied repeatedly: "Beta plans to" needs two passes ("to", then "plans").
    for _ in range(8):   # "is in advanced talks to" is five words deep
        trimmed = re.sub(
            r"(?i)\s+(?:is|are|was|were|to|set|about|looking|planning|plans?|preparing|"
            r"poised|likely|in|advanced|talks|close|closing|said|reported|expected|eyes|"
            r"may|will|could|might)\s*$",
            "", text.strip())
        if trimmed == text.strip():
            break
        text = trimmed

    # Strip a trailing descriptor the prefix rules could not reach because it
    # sat after the company name ("Acme, an EV startup, raises ...").
    text = re.sub(r"(?i),\s*(?:an?|the)\s+[^,]{0,40}$", "", text.strip().rstrip(","))
    text = re.sub(r"(?i)\s+(?:startup|firm|company|platform|brand|maker|app)$", "", text.strip())
    text = text.strip(" -–—:|,")
    return text or None


def investors(headline: str) -> list[str]:
    match = _INVESTORS.search(headline or "")
    if not match:
        return []
    names = []
    for part in _SPLIT_INVESTORS.split(match.group(1)):
        name = part.strip(" .,-–—;:")
        if name and len(name) > 1 and not _NOT_AN_INVESTOR.match(name):
            names.append(name)
    return names


def extract(headline: str, url: str = "", published_at: str | None = None,
            rule_set: dict | None = None) -> Extraction:
    r = rule_set or rules()
    name = company_name(headline, r)
    stage = round_stage(headline, r)
    amt = amount(headline)

    result = Extraction(
        company_name=name,
        round_stage=stage,
        amount_raw=amt,
        currency=currency(amt or headline, r),
        investors=investors(headline),
        announced_at=published_at,
        article_url=url,
        method="rules",
        raw_text=headline or "",
    )
    # A headline naming several companies at once ("A, B and C raise seed") is
    # not confidently attributable to one of them; it stays low and gets reviewed.
    multi = bool(name and re.search(r",| and ", name, re.I))
    result.confidence = "high" if (name and not multi and stage != "unknown" and amt) else "low"
    return result


# ------------------------------------------------------------------------- llm
def llm_enabled() -> bool:
    """Off unless explicitly switched on. INV-2 territory: an invented company
    name here eventually becomes outreach to a company that never raised."""
    return os.environ.get("TRACKER_FUNDING_LLM", "").strip().lower() in ("1", "true", "yes")


def extract_with_llm(headline: str, base: Extraction) -> Extraction:
    """Fill only the fields the rules left empty. Raw text and raw response are
    both stored on the row so a bad extraction can be audited after the fact.

    Never called unless llm_enabled() and the rules came back incomplete.
    """
    import json

    try:
        import anthropic
    except ImportError:
        return base

    prompt = (
        "Extract funding details from this headline. Reply with JSON only, using keys "
        "company_name, round_stage (one of pre-seed, seed, A, B, C+, debt, unknown), "
        "amount_raw, currency, investors (array). Use null for anything not stated. "
        "Do not infer or guess.\n\nHeadline: " + (headline or "")
    )
    try:
        client = anthropic.Anthropic()
        response = client.messages.create(
            model="claude-sonnet-5",
            max_tokens=400,
            messages=[{"role": "user", "content": prompt}],
        )
        raw = response.content[0].text
        parsed = json.loads(re.search(r"\{.*\}", raw, re.S).group(0))
    except Exception:
        return base

    filled = Extraction(**{**base.__dict__})
    filled.llm_raw = raw
    for field in ("company_name", "amount_raw", "currency"):
        if not getattr(filled, field) and parsed.get(field):
            setattr(filled, field, str(parsed[field]))
    if filled.round_stage == "unknown" and parsed.get("round_stage"):
        filled.round_stage = str(parsed["round_stage"])
    if not filled.investors and isinstance(parsed.get("investors"), list):
        filled.investors = [str(i) for i in parsed["investors"] if i]
    filled.method = "llm"
    return filled
