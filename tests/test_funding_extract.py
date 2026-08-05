"""Extraction from headlines. Rules only — the LLM path stays off."""
import pytest

from tracker.funding import extract


def test_clean_series_a_extracts_every_field():
    e = extract.extract(
        "InRisk Labs raises $27 Mn in Series A round led by Bessemer, Northpoint Capital",
        url="http://x/1", published_at="2026-08-04T00:00:00+00:00")
    assert e.company_name == "InRisk Labs"
    assert e.round_stage == "A"
    assert e.amount_raw == "$27 Mn"
    assert e.currency == "USD"
    assert e.investors == ["Bessemer", "Northpoint Capital"]
    assert e.announced_at == "2026-08-04T00:00:00+00:00"
    assert e.confidence == "high"
    assert e.method == "rules"


def test_item_with_no_amount_is_kept_at_low_confidence():
    """Never discard because extraction was partial: a company and a date is
    still a lead."""
    e = extract.extract("Acme raises seed round")
    assert e.company_name == "Acme"
    assert e.round_stage == "seed"
    assert e.amount_raw is None
    assert e.confidence == "low"


def test_debt_round_is_recognised():
    e = extract.extract("Acme secures venture debt funding of Rs 20 Cr")
    assert e.round_stage == "debt"
    assert e.currency == "INR"
    assert e.company_name == "Acme"


@pytest.mark.parametrize("headline,stage", [
    ("Acme Labs raises $1 Mn in pre-seed round", "pre-seed"),
    ("Acme Labs raises $2 Mn in seed funding", "seed"),
    ("Acme Labs raises $10 Mn in Series A", "A"),
    ("Acme Labs raises $30 Mn in Series B", "B"),
    ("Acme Labs raises $90 Mn in Series D", "C+"),
    ("Acme Labs secures Rs 63 Cr in Series A1 round", "A"),
    ("Acme Labs raises Rs 36 Cr in pre-Series A round", "seed"),
    ("Acme Labs raises Rs 10 Cr in venture debt", "debt"),
    ("Acme Labs raises Rs 10 Cr led by Someone", "unknown"),
])
def test_round_stages(headline, stage):
    assert extract.extract(headline).round_stage == stage


@pytest.mark.parametrize("headline,expected", [
    ("Personal assistance startup Hulp secures $2.6 Mn seed funding", "Hulp"),
    ("C2C marketplace Vingo raises Rs 10 Cr seed round", "Vingo"),
    ("Bengaluru-based Zoppler raises Rs 6.5 Cr", "Zoppler"),
    ("[Update] Acme Labs raises $5 Mn", "Acme Labs"),
    ("Exclusive: Gen AI startup Simplismart set to raise $9 Mn in Series B", "Simplismart"),
    ("Beta plans to raise seed round", "Beta"),
    ("Acme is in advanced talks to raise $5 Mn", "Acme"),
])
def test_company_name_stripping(headline, expected):
    assert extract.extract(headline).company_name == expected


def test_no_verb_means_no_guessed_company():
    assert extract.extract("Series A funding for a stealth startup").company_name is None


def test_multi_company_headline_stays_low_confidence():
    """'A, B and C raise seed' cannot be attributed to one company, so it goes
    to review instead of being silently assigned to the first name."""
    e = extract.extract("InRisk Labs, Hulp and Vingo raise $5 Mn in seed round")
    assert e.confidence == "low"


def test_currency_detection():
    assert extract.extract("Acme Labs raises Rs 10 Cr").currency == "INR"
    assert extract.extract("Acme Labs raises $10 Mn").currency == "USD"
    assert extract.extract("Acme Labs raises a seed round").currency is None


def test_generic_investor_placeholders_are_not_investors():
    e = extract.extract("Stable Money kicks off Series C with $14.3 Mn from existing investors")
    assert e.investors == []


# ------------------------------------------------------------------- triggers
@pytest.mark.parametrize("headline", [
    "Acme raises $5 Mn",
    "Acme bags Rs 10 Cr in funding",
    "Acme closes its Series B round",
    "Acme mops up $3 Mn in funding",
    "Acme announces Series A",
])
def test_funding_headlines_are_detected(headline):
    assert extract.is_funding(headline) is True


@pytest.mark.parametrize("headline", [
    "PB Fintech Q1 Profit Doubles YoY To Rs 163 Cr, Revenue Jumps 40%",
    "Swiggy outlines exclusive product strategy for Instamart",
    "Acme acquires Beta for $10 Mn",
    "The report raises concerns about valuations",
])
def test_non_funding_headlines_are_rejected(headline):
    assert extract.is_funding(headline) is False


def test_a_vc_closing_its_own_fund_is_not_a_company_raising():
    assert extract.is_funding("Peak XV closes its maiden fund at $200 Mn") is False


def test_money_mentioning_non_rounds_are_flagged_as_near_misses():
    headline = "Tracxn slips into loss as revenue remains flat, down Rs 5 Cr"
    assert extract.is_funding(headline) is False
    assert extract.is_near_miss(headline) is True


# ------------------------------------------------------------------------ llm
def test_llm_is_off_by_default(monkeypatch):
    monkeypatch.delenv("TRACKER_FUNDING_LLM", raising=False)
    assert extract.llm_enabled() is False


def test_llm_requires_an_explicit_opt_in(monkeypatch):
    monkeypatch.setenv("TRACKER_FUNDING_LLM", "1")
    assert extract.llm_enabled() is True
    monkeypatch.setenv("TRACKER_FUNDING_LLM", "0")
    assert extract.llm_enabled() is False


def test_raw_text_is_stored_for_auditing():
    headline = "Acme Labs raises $5 Mn in Series A"
    assert extract.extract(headline).raw_text == headline
