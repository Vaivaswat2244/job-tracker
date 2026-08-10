"""Company resolution, the collision guard, and the approval gate.

The failure this file exists to prevent: matching "Atlas" the Bangalore fintech
to "Atlas" the US logistics company, corrupting the watchlist, and eventually
producing outreach to the wrong company.
"""
import pytest

from tracker import watchlist
from tracker.funding import Extraction, resolve
from tracker.funding import run as fr


@pytest.fixture
def wl(tmp_path, monkeypatch):
    path = tmp_path / "watchlist.yaml"
    monkeypatch.setattr(watchlist, "PATH", str(path))
    return str(path)


@pytest.fixture
def seeded(conn, wl):
    watchlist.save([
        {"name": "Atlas", "domain": "atlas-logistics.com", "ats": "lever", "slug": "atlas"},
        {"name": "InRisk Labs", "domain": "inrisklabs.com", "ats": "unknown"},
    ])
    watchlist.sync(conn)
    return conn


def extraction(**kw):
    base = dict(company_name="InRisk Labs", round_stage="A", amount_raw="$27 Mn",
                announced_at="2026-08-04T00:00:00+00:00", article_url="http://news/1")
    base.update(kw)
    return Extraction(**base)


@pytest.fixture
def offline(monkeypatch):
    """Control what resolve_domain returns without touching the network."""
    def install(domain, reason="stubbed"):
        monkeypatch.setattr(resolve, "resolve_domain", lambda *a, **k: (domain, reason))
        monkeypatch.setattr(fr, "detect_ats_for", lambda d: ("greenhouse", "acme"))
    return install


# --------------------------------------------------------------- domain logic
def test_single_word_names_are_never_guessed():
    """'Atlas' is exactly the collision case; guessing atlas.com would confirm
    the wrong company against content that legitimately says 'atlas'."""
    assert resolve.guess_domains("Atlas") == []
    assert resolve.guess_domains("Vaaree") == []


def test_multi_word_names_produce_candidates_to_verify():
    guesses = resolve.guess_domains("InRisk Labs")
    assert "inrisklabs.com" in guesses
    assert len(guesses) <= resolve.MAX_GUESSES


def test_domain_generation_keeps_descriptive_words():
    """'Labs' is dropped when comparing names but must be kept when building a
    domain: inrisklabs.com, not inrisk.com."""
    assert "inrisklabs.com" in resolve.guess_domains("InRisk Labs")
    assert "adiabatictechnologies.com" in resolve.guess_domains("Adiabatic Technologies")


def test_publisher_and_social_links_are_never_company_domains():
    html = ('<a href="https://entrackr.com/x">src</a>'
            '<a href="https://twitter.com/acme">tw</a>'
            '<a href="https://acmelabs.com">site</a>')
    assert resolve.candidate_domains(html) == ["acmelabs.com"]


def test_domain_must_plausibly_match_the_name():
    assert resolve.domain_matches_name("rivermobility.com", "River Mobility") is True
    assert resolve.domain_matches_name("sequoiacap.com", "River Mobility") is False


# ------------------------------------------------------------ matching policy
def test_match_is_on_domain_only(seeded):
    assert resolve.match_on_domain(seeded, "inrisklabs.com")["name"] == "InRisk Labs"
    assert resolve.match_on_domain(seeded, None) is None
    assert resolve.match_on_domain(seeded, "unknown-domain.com") is None


def test_confirmed_match_sets_funding_fields_and_the_window(seeded, offline):
    offline("inrisklabs.com")
    assert fr.resolve_item(seeded, extraction()) == "confirmed"

    row = seeded.execute("SELECT * FROM companies WHERE lower(domain)='inrisklabs.com'").fetchone()
    assert row["recently_funded_at"] == "2026-08-04T00:00:00+00:00"
    assert row["funding_stage"] == "A"
    assert row["funding_amount_raw"] == "$27 Mn"
    assert row["priority_until"].startswith("2026-10-03")   # +60 days


def test_confirmed_match_raises_effective_priority_to_high(seeded, offline):
    offline("inrisklabs.com")
    fr.resolve_item(seeded, extraction())
    row = seeded.execute("SELECT * FROM companies WHERE lower(domain)='inrisklabs.com'").fetchone()
    assert row["priority"] == "normal", "the YAML baseline is untouched"
    assert watchlist.effective_priority(row) == "high"


def test_window_decays_after_sixty_days(seeded, offline):
    from datetime import datetime, timedelta, timezone

    old = (datetime.now(timezone.utc) - timedelta(days=90)).isoformat()
    offline("inrisklabs.com")
    fr.resolve_item(seeded, extraction(announced_at=old))
    row = seeded.execute("SELECT * FROM companies WHERE lower(domain)='inrisklabs.com'").fetchone()
    assert watchlist.effective_priority(row) == "normal"


# ------------------------------------------------------------ the name twin
def test_same_name_different_domain_does_not_match(seeded, offline):
    """The headline acceptance case: a funded 'Atlas' whose domain is not the
    watchlist Atlas must not touch that company."""
    offline("atlas-fintech.in")
    outcome = fr.resolve_item(seeded, extraction(company_name="Atlas Fintech"))

    assert outcome == "needs_review"
    twin = seeded.execute("SELECT * FROM companies WHERE domain='atlas-logistics.com'").fetchone()
    assert twin["recently_funded_at"] is None, "the unrelated Atlas must be untouched"
    assert twin["priority_until"] is None


def test_name_collision_is_explained_on_the_candidate(seeded, offline):
    offline("atlas-fintech.in")
    fr.resolve_item(seeded, extraction(company_name="Atlas Fintech"))
    row = seeded.execute("SELECT * FROM watchlist_candidates").fetchone()
    assert row["status"] == "needs_review"
    assert "COLLISION" in row["reason"].upper()
    assert "atlas-logistics.com" in row["reason"]


def test_unresolvable_company_goes_to_review_not_companies(seeded, offline):
    offline(None, "no outbound link matched")
    before = seeded.execute("SELECT count(*) n FROM companies").fetchone()["n"]

    assert fr.resolve_item(seeded, extraction(company_name="Mystery Corp")) == "needs_review"
    after = seeded.execute("SELECT count(*) n FROM companies").fetchone()["n"]
    assert after == before, "no path here may insert into companies"


def test_resolvable_new_company_lands_in_candidates_with_a_detected_board(seeded, offline):
    offline("brandnewco.com")
    assert fr.resolve_item(seeded, extraction(company_name="Brand Newco")) == "needs_review"

    row = seeded.execute("SELECT * FROM watchlist_candidates WHERE name='Brand Newco'").fetchone()
    assert row["domain"] == "brandnewco.com"
    assert (row["resolved_ats"], row["resolved_slug"]) == ("greenhouse", "acme")
    assert row["status"] == "needs_review"
    assert seeded.execute(
        "SELECT count(*) n FROM companies WHERE lower(domain)='brandnewco.com'"
    ).fetchone()["n"] == 0


def test_missing_company_name_still_produces_a_review_row(seeded, offline):
    offline(None)
    assert fr.resolve_item(seeded, extraction(company_name=None)) == "needs_review"
    assert seeded.execute("SELECT count(*) n FROM watchlist_candidates").fetchone()["n"] == 1


# ------------------------------------------------------------------- approval
def test_approval_is_the_only_route_into_the_watchlist(seeded, offline, wl):
    offline("brandnewco.com")
    fr.resolve_item(seeded, extraction(company_name="Brand Newco"))
    candidate = seeded.execute(
        "SELECT id FROM watchlist_candidates WHERE name='Brand Newco'").fetchone()

    ok, message = fr.approve(seeded, candidate["id"], wl)
    assert ok
    assert "Brand Newco" in message

    entry = watchlist.find(watchlist.load(wl), name="Brand Newco")
    assert entry["ats"] == "greenhouse" and entry["slug"] == "acme"
    assert entry["priority"] == "high", "a fresh raise is the reason to watch closely"
    assert seeded.execute(
        "SELECT count(*) n FROM companies WHERE name='Brand Newco'").fetchone()["n"] == 1


def test_approving_twice_is_harmless(seeded, offline, wl):
    offline("brandnewco.com")
    fr.resolve_item(seeded, extraction(company_name="Brand Newco"))
    cid = seeded.execute("SELECT id FROM watchlist_candidates").fetchone()["id"]
    assert fr.approve(seeded, cid, wl)[0] is True
    assert fr.approve(seeded, cid, wl)[0] is False


def test_rejecting_a_candidate_leaves_companies_alone(seeded, offline, wl):
    offline("brandnewco.com")
    fr.resolve_item(seeded, extraction(company_name="Brand Newco"))
    cid = seeded.execute("SELECT id FROM watchlist_candidates").fetchone()["id"]

    ok, _ = fr.reject(seeded, cid)
    assert ok
    row = seeded.execute("SELECT status FROM watchlist_candidates WHERE id=?", (cid,)).fetchone()
    assert row["status"] == "rejected"
    assert seeded.execute(
        "SELECT count(*) n FROM companies WHERE name='Brand Newco'").fetchone()["n"] == 0
