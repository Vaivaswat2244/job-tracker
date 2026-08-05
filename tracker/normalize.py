"""Turn a NormalizedJob into the column values `jobs` expects.

Every heuristic here defaults to unknown rather than guessing. A wrong
`comp_model` is worse than a blank one: blank prompts the user to look, wrong
does not. `auth_required` is a flag that sorts a job lower, never a filter that
removes it (INV-1).
"""
import re
from datetime import datetime, timezone

# --------------------------------------------------------------------- comp_model
_AGNOSTIC = re.compile(r"""(?ix)
    location[\s-]*agnostic
  | location[\s-]*independent
  | same\s+(?:pay|salary|compensation)\s+(?:regardless|anywhere|no\s+matter)
  | (?:pay|salary|compensation)\s+(?:is\s+)?(?:the\s+)?same\s+(?:regardless|anywhere|everywhere)
  | (?:we\s+)?(?:do\s+not|don't)\s+adjust\s+(?:pay|salary|compensation)\s+(?:for|by)\s+location
  | (?:not|non)[\s-]*adjusted\s+(?:for|by)\s+(?:location|geography)
  | global\s+(?:salary|pay|compensation)\s+band
""")

_GEO = re.compile(r"""(?ix)
    location[\s-]*adjusted
  | geo[\s-]*(?:tiered|tier|adjusted|based)
  | (?:pay|salary|compensation)\s+(?:tier|zone|band)s?\s+(?:by|based\s+on|depend)
  | adjusted\s+(?:for|to)\s+(?:your\s+)?(?:location|geography|market|cost\s+of\s+living)
  | (?:based|depends?|varies)\s+on\s+(?:your\s+)?(?:location|geography|geographic\s+tier)
  | cost[\s-]*of[\s-]*living\s+adjust
""")

_INR = re.compile(r"(?i)(?:₹|\bINR\b|\bRs\.?\s*\d|\d+\s*(?:LPA|lakhs?|lacs?)\b|\bcrores?\b)")


def comp_model(jd_text: str = "", pay_currency: str | None = None) -> str:
    text = jd_text or ""
    if _AGNOSTIC.search(text):
        return "location_agnostic"
    if _GEO.search(text):
        return "geo_adjusted"
    if (pay_currency or "").upper() == "INR" or _INR.search(text):
        return "local_market"
    return "unknown"


# ------------------------------------------------------------------ auth_required
# Sponsorship explicitly refused. Checked first: "we do not sponsor visas" also
# matches the offering patterns below, and the refusal is the stronger signal.
_NO_SPONSOR = re.compile(r"""(?ix)
    (?:do\s+not|don't|cannot|can't|unable\s+to|not\s+able\s+to|will\s+not|won't)
        \s+(?:currently\s+|presently\s+)?(?:offer|provide|sponsor)\w*
        (?:\s+\w+){0,3}?\s*(?:visa|sponsorship|immigration)
  | no\s+(?:visa\s+|work\s+|employment\s+|immigration\s+)?sponsorship
  | (?:visa\s+|work\s+|employment\s+)?sponsorship\s+is\s+
        (?:not\s+(?:provided|offered|available|possible)|unavailable)
    # "without sponsorship" must be qualified: bare use is nearly always the
    # US export-license boilerplate, not an immigration statement.
  | without\s+(?:the\s+need\s+for\s+)?(?:visa|work|employment|immigration)\s+sponsorship
""")

# US export-control paragraphs talk about "sponsorship for an export license"
# and "authorized to receive technology". Both trip the patterns above and have
# nothing to do with whether the company will hire someone who needs a visa.
_EXPORT_BOILERPLATE = re.compile(r"(?i)export\s+(?:license|control|law|administration|regulation)")


def _immigration_match(pattern: re.Pattern, text: str):
    """First match that is not sitting inside an export-control paragraph."""
    for m in pattern.finditer(text):
        window = text[max(0, m.start() - 200):m.end() + 200]
        if _EXPORT_BOILERPLATE.search(window):
            continue
        return m
    return None

_SPONSOR_OK = re.compile(r"""(?ix)
    (?:visa\s+)?sponsorship\s+(?:is\s+)?(?:available|offered|provided|possible)
  | (?:we|company)\s+(?:can|will|do|are\s+happy\s+to|are\s+able\s+to)\s+sponsor
  | (?:offer|provide)s?\s+(?:visa\s+)?sponsorship
  | relocation\s+and\s+visa\s+support
""")

_AUTH_REQUIRED = re.compile(r"""(?ix)
    (?:must\s+be|are|is|be)\s+(?:legally\s+)?(?:authori[sz]ed|eligible|entitled)\s+to\s+work
        \s+in\s+(?:the\s+)?(?:US|U\.S\.|USA|United\s+States|Canada|EU|European\s+Union|UK|United\s+Kingdom)
  | (?:US|U\.S\.|USA|United\s+States|Canada|Canadian|EU|European|UK)\s+work\s+authori[sz]ation
        \s*(?:is\s+)?(?:required|necessary|a\s+must)
  | (?:requires?|require)\s+(?:US|U\.S\.|USA|United\s+States|Canada|EU|UK)\s+work\s+authori[sz]ation
  | (?:US|U\.S\.|United\s+States|Canadian|EU|UK)\s+citizens?\s+or\s+permanent\s+residents?
  | authori[sz]ed\s+to\s+work\s+in\s+(?:the\s+)?(?:US|U\.S\.|USA|United\s+States|Canada|EU|UK)
""")


def auth_required(jd_text: str = "") -> int:
    """1 when the posting demands US/CA/EU authorization the user does not have.

    A flag, not a filter — the caller sorts these lower and still shows them.
    """
    text = jd_text or ""
    if _immigration_match(_NO_SPONSOR, text):
        return 1
    if _SPONSOR_OK.search(text):
        return 0
    return 1 if _immigration_match(_AUTH_REQUIRED, text) else 0


# ----------------------------------------------------------------- hires_in_india
_INDIA = re.compile(r"""(?ix)
    \bindia\b | \bindian\b
  | \bbangalore\b | \bbengaluru\b | \bmumbai\b | \bdelhi\b | \bncr\b | \bgurgaon\b
  | \bgurugram\b | \bhyderabad\b | \bpune\b | \bchennai\b | \bnoida\b | \bkolkata\b
  | \bahmedabad\b | \bjaipur\b | \bkochi\b | \bcoimbatore\b | \bindore\b
""")

_GLOBAL_REMOTE = re.compile(r"""(?ix)
    remote\s*[-–—,(]?\s*(?:global|worldwide|anywhere|international)
  | (?:work\s+from|hire)\s+anywhere
  | anywhere\s+in\s+the\s+world
  | globally\s+remote | fully\s+distributed
""")


# Location strings that name no actual place, so the JD text gets to decide.
_VAGUE_LOCATION = re.compile(r"""(?ix)^\s*(?:
    remote | hybrid | in[\s-]*office | on[\s-]*site | flexible | global | worldwide
  | anywhere | various | multiple(?:\s+locations)? | tbd | n/?a | - | \.
)\s*$""")


def hires_in_india(jd_text: str = "", location: str | None = None) -> int | None:
    """1 India or worldwide-remote, 0 a named non-India location, None if unclear.

    The posting's own location field wins outright when it names a real place.
    Boilerplate in the body ("we are fully distributed", an India office in the
    footer) otherwise flags a Foster City role as India-friendly, and a field
    that says 1 for everything is worth less than one that says nothing.

    None is a real answer here. Recording 0 for a JD that never mentioned
    geography at all would quietly bury roles that do in fact hire in India.
    """
    loc = (location or "").strip()
    if loc and not _VAGUE_LOCATION.match(loc):
        if _INDIA.search(loc):
            return 1
        return 1 if _GLOBAL_REMOTE.search(loc) else 0

    body = jd_text or ""
    if _INDIA.search(body) or _GLOBAL_REMOTE.search(body):
        return 1
    return None


# ------------------------------------------------------------------ dedupe keys
_PUNCT = re.compile(r"[^a-z0-9 ]+")
_SUFFIX = re.compile(
    r"(?i)\b(inc|llc|ltd|limited|pvt|private|corp|corporation|co|gmbh|plc|technologies|"
    r"technology|labs|software|systems|solutions|india)\b"
)
_SENIORITY_NOISE = re.compile(
    r"(?i)\((?:remote|hybrid|onsite|on-site|contract|full[\s-]?time|part[\s-]?time)[^)]*\)"
)


def norm_company(name: str | None) -> str:
    text = _SUFFIX.sub(" ", (name or "").lower())
    return " ".join(_PUNCT.sub(" ", text).split())


def norm_title(title: str | None) -> str:
    text = _SENIORITY_NOISE.sub(" ", (title or "").lower())
    text = re.sub(r"(?i)\s*[-–—|,]\s*(remote|hybrid|onsite|on-site|india|bangalore|bengaluru|"
                  r"mumbai|us|usa|emea|global)\b.*$", " ", text)
    return " ".join(_PUNCT.sub(" ", text).split())


def posted_week(posted_at: str | None) -> str:
    """ISO year-week. Two boards listing the same role rarely agree on the exact
    timestamp but almost always land in the same week."""
    parsed = parse_dt(posted_at)
    if not parsed:
        return ""
    year, week, _ = parsed.isocalendar()
    return f"{year}-W{week:02d}"


def parse_dt(value) -> datetime | None:
    if not value:
        return None
    text = str(value).strip().replace("Z", "+00:00")
    for candidate in (text, text[:19], text[:10]):
        try:
            parsed = datetime.fromisoformat(candidate)
        except ValueError:
            continue
        return parsed if parsed.tzinfo else parsed.replace(tzinfo=timezone.utc)
    return None
