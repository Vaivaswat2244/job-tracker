"""Text coercion shared by the adapters. Every helper returns a value for any
input, including None — adapters must not raise on a field a provider omitted."""
import html
import re

_TAG = re.compile(r"<[^>]+>")
_BLOCK_END = re.compile(r"(?i)</(p|div|li|tr|td|th|h[1-6]|section|ul|ol|table)\s*>")
_BR = re.compile(r"(?i)<br\s*/?>")


def strip_html(value) -> str:
    """HTML (possibly entity-escaped, as Greenhouse sends it) -> readable text.

    Unescape first: Greenhouse double-encodes, so the body arrives as
    '&lt;p&gt;…' and a naive tag strip would leave the markup intact.
    """
    if not value:
        return ""
    text = str(value)
    for _ in range(2):
        unescaped = html.unescape(text)
        if unescaped == text:
            break
        text = unescaped
    text = _BR.sub("\n", text)
    text = _BLOCK_END.sub("\n", text)
    # Dropped, not spaced: inline markup lands mid-word ("V<b>isa</b>"), and a
    # space there turns "Visa sponsorship" into "V isa sponsorship", which no
    # amount of pattern-writing downstream can recover.
    text = _TAG.sub("", text)
    text = html.unescape(text)
    return clean(text)


def clean(text: str) -> str:
    text = text.replace("\xa0", " ").replace("\r\n", "\n").replace("\r", "\n")
    text = re.sub(r"[ \t]+", " ", text)
    text = re.sub(r" *\n *", "\n", text)
    return re.sub(r"\n{3,}", "\n\n", text).strip()


def as_str(value) -> str | None:
    if value is None:
        return None
    if isinstance(value, (dict, list)):
        return None
    text = str(value).strip()
    return text or None


def as_float(value) -> float | None:
    try:
        return float(value)
    except (TypeError, ValueError):
        return None


def dig(payload, *keys):
    """payload['a']['b'] without caring whether either level exists."""
    node = payload
    for key in keys:
        if not isinstance(node, dict):
            return None
        node = node.get(key)
    return node
