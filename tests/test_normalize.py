"""Heuristics. Each defaults to unknown; a wrong answer is worse than none."""
import pytest

from tracker.normalize import (auth_required, comp_model, hires_in_india,
                               norm_company, norm_title, posted_week)


# ----------------------------------------------------------------- comp_model
@pytest.mark.parametrize("text", [
    "Our compensation is location agnostic.",
    "We pay the same regardless of where you live.",
    "This is a location-independent salary band.",
    "We do not adjust salary for location.",
])
def test_location_agnostic(text):
    assert comp_model(text) == "location_agnostic"


@pytest.mark.parametrize("text", [
    "Salary is location-adjusted.",
    "We use geo-tiered compensation bands.",
    "Your offer is adjusted for your location.",
    "Pay depends on your geographic tier.",
])
def test_geo_adjusted(text):
    assert comp_model(text) == "geo_adjusted"


@pytest.mark.parametrize("text", ["Compensation: ₹40,00,000", "Salary 25 LPA", "INR 1800000"])
def test_local_market_from_inr(text):
    assert comp_model(text) == "local_market"


def test_local_market_from_currency_field():
    assert comp_model("", pay_currency="INR") == "local_market"


@pytest.mark.parametrize("text", ["", "We offer competitive compensation.", "Great benefits."])
def test_comp_model_defaults_to_unknown(text):
    assert comp_model(text) == "unknown"


def test_explicit_statement_beats_an_inr_figure():
    assert comp_model("Location agnostic pay. Bengaluru band: ₹40L") == "location_agnostic"


# --------------------------------------------------------------- auth_required
@pytest.mark.parametrize("text", [
    "You must be legally authorized to work in the United States.",
    "US work authorization is required.",
    "We do not sponsor visas.",
    "No visa sponsorship for this role.",
    "Visa sponsorship is NOT provided for this position.",
    "Candidates must be US citizens or permanent residents.",
])
def test_flags_real_authorization_requirements(text):
    assert auth_required(text) == 1


@pytest.mark.parametrize("text", [
    "",
    "We hire globally.",
    "Visa sponsorship is available for this role.",
    "We are happy to sponsor the right candidate.",
])
def test_does_not_flag_when_sponsorship_is_offered_or_unmentioned(text):
    assert auth_required(text) == 0


def test_export_control_boilerplate_is_not_an_immigration_statement():
    """The single biggest false positive in real Greenhouse JDs: US export-law
    paragraphs mention 'sponsorship for an export license', which has nothing to
    do with whether the company will hire someone who needs a visa."""
    text = ("Employee must be able to receive software or technology controlled under "
            "U.S. export laws without sponsorship for an export license.")
    assert auth_required(text) == 0


def test_offered_sponsorship_beats_a_generic_authorization_line():
    text = ("You must be authorized to work in the US. Visa sponsorship is available.")
    assert auth_required(text) == 0


# -------------------------------------------------------------- hires_in_india
@pytest.mark.parametrize("location", ["Bengaluru, India", "Remote India", "Mumbai", "Pune"])
def test_india_locations(location):
    assert hires_in_india("", location) == 1


@pytest.mark.parametrize("location", ["Foster City, CA", "United States", "Berlin, Germany"])
def test_named_non_india_location_wins_over_body_boilerplate(location):
    """A company with an India office mentions India in every JD footer. The
    posting's own location field is the authority."""
    assert hires_in_india("We are fully distributed with an office in Bangalore.", location) == 0


@pytest.mark.parametrize("location", ["Remote", "", None, "Global", "In-Office"])
def test_vague_location_defers_to_the_description(location):
    assert hires_in_india("This role is open to candidates in India.", location) == 1
    assert hires_in_india("Hire from anywhere in the world.", location) == 1


def test_unknown_when_nothing_indicates_geography():
    assert hires_in_india("Great team, great mission.", "Remote") is None
    assert hires_in_india("", None) is None


# ---------------------------------------------------------------- dedupe keys
def test_norm_company_strips_legal_suffixes():
    assert norm_company("Acme Technologies Pvt. Ltd.") == norm_company("ACME")


def test_norm_title_strips_location_and_mode_noise():
    assert norm_title("Backend Engineer (Remote)") == norm_title("Backend Engineer")
    assert norm_title("Backend Engineer - India") == norm_title("Backend Engineer")


def test_posted_week_groups_nearby_days():
    assert posted_week("2026-08-03T00:00:00Z") == posted_week("2026-08-06T23:00:00Z")
    assert posted_week("2026-08-03") != posted_week("2026-09-03")
    assert posted_week(None) == ""
    assert posted_week("garbage") == ""
