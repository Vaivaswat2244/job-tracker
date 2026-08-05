"""Append-only ingest: idempotency, dedupe-by-linking, and never losing a row."""
import json

from tracker import ingest
from tracker.ats import NormalizedJob


def job(**kw):
    base = dict(external_id="1", title="Backend Engineer", url="https://x/1",
                source="greenhouse", posted_at="2026-08-03T10:00:00+00:00",
                location="Bengaluru, India", jd_text="Build things.")
    base.update(kw)
    return NormalizedJob(**base)


def company(conn, name="Acme", domain=None):
    return ingest.get_or_create_company(conn, name, domain)


# ---------------------------------------------------------------- idempotency
def test_reingesting_the_same_posting_updates_rather_than_duplicates(conn):
    cid = company(conn)
    ingest.ingest(conn, cid, "Acme", [job()])
    stats = ingest.ingest(conn, cid, "Acme", [job()])

    assert stats == {"new": 0, "updated": 1, "skipped": 0}
    assert conn.execute("SELECT count(*) n FROM jobs").fetchone()["n"] == 1


def test_repeated_ingest_never_grows_the_table(conn):
    cid = company(conn)
    batch = [job(external_id=str(i), url=f"https://x/{i}") for i in range(20)]
    for _ in range(4):
        ingest.ingest(conn, cid, "Acme", batch)
    assert conn.execute("SELECT count(*) n FROM jobs").fetchone()["n"] == 20


def test_seen_at_refreshes_but_first_seen_at_is_preserved(conn):
    """'Posted in the last 48 hours' is only answerable if first-seen survives."""
    cid = company(conn)
    ingest.ingest(conn, cid, "Acme", [job()])
    first = conn.execute("SELECT first_seen_at, seen_at FROM jobs").fetchone()

    ingest.ingest(conn, cid, "Acme", [job(title="Backend Engineer II")])
    later = conn.execute("SELECT first_seen_at, seen_at, title FROM jobs").fetchone()

    assert later["first_seen_at"] == first["first_seen_at"]
    assert later["seen_at"] >= first["seen_at"]
    assert later["title"] == "Backend Engineer II"


def test_same_external_id_on_a_different_provider_is_a_different_job(conn):
    cid = company(conn)
    ingest.ingest(conn, cid, "Acme", [job(external_id="42", source="greenhouse")])
    ingest.ingest(conn, cid, "Acme", [job(external_id="42", source="lever",
                                          url="https://lever/42")])
    assert conn.execute("SELECT count(*) n FROM jobs").fetchone()["n"] == 2


def test_update_does_not_blank_an_existing_description(conn):
    cid = company(conn)
    ingest.ingest(conn, cid, "Acme", [job(jd_text="The full description.")])
    ingest.ingest(conn, cid, "Acme", [job(jd_text="")])
    assert conn.execute("SELECT jd_text FROM jobs").fetchone()["jd_text"] == "The full description."


# --------------------------------------------------------------------- dedupe
def test_cross_source_duplicate_is_linked_not_deleted(conn):
    cid = company(conn)
    ingest.ingest(conn, cid, "Acme", [job(source="greenhouse", url="https://gh/1")])
    ingest.ingest(conn, cid, "Acme", [job(external_id="9", source="lever",
                                          url="https://lever/9")])

    rows = conn.execute("SELECT id, source, canonical_id FROM jobs ORDER BY id").fetchall()
    assert len(rows) == 2, "dedupe links, it never deletes"
    assert rows[0]["canonical_id"] is None
    assert rows[1]["canonical_id"] == rows[0]["id"]


def test_duplicate_url_is_appended_to_the_canonical_source_list(conn):
    cid = company(conn)
    ingest.ingest(conn, cid, "Acme", [job(source="greenhouse", url="https://gh/1")])
    ingest.ingest(conn, cid, "Acme", [job(external_id="9", source="lever",
                                          url="https://lever/9")])

    canonical = conn.execute("SELECT source_urls FROM jobs WHERE canonical_id IS NULL").fetchone()
    urls = json.loads(canonical["source_urls"])
    assert "https://gh/1" in urls and "https://lever/9" in urls


def test_same_source_postings_are_never_collapsed(conn):
    """One role opened in six cities is six postings with six apply URLs. The
    board gave them distinct ids; collapsing them would lose five of those."""
    cid = company(conn)
    ingest.ingest(conn, cid, "Acme", [
        job(external_id="1", url="https://gh/1", location="Austin"),
        job(external_id="2", url="https://gh/2", location="Denver"),
        job(external_id="3", url="https://gh/3", location="Boston"),
    ])
    linked = conn.execute("SELECT count(*) n FROM jobs WHERE canonical_id IS NOT NULL").fetchone()
    assert linked["n"] == 0


def test_different_week_is_not_a_duplicate(conn):
    cid = company(conn)
    ingest.ingest(conn, cid, "Acme", [job(posted_at="2026-08-03T10:00:00+00:00")])
    ingest.ingest(conn, cid, "Acme", [job(external_id="9", source="lever",
                                          posted_at="2026-06-03T10:00:00+00:00")])
    linked = conn.execute("SELECT count(*) n FROM jobs WHERE canonical_id IS NOT NULL").fetchone()
    assert linked["n"] == 0


def test_dedupe_key_ignores_cosmetic_title_differences(conn):
    assert (ingest.dedupe_key("Acme Technologies Pvt Ltd", "Backend Engineer (Remote)", None)
            == ingest.dedupe_key("Acme", "Backend Engineer", None))


# ------------------------------------------------------------- nothing is lost
def test_unusable_posting_is_logged_never_dropped_silently(conn):
    cid = company(conn)
    stats = ingest.ingest(conn, cid, "Acme", [NormalizedJob(external_id="", title="",
                                                            url="", source="greenhouse",
                                                            raw={"why": "no id"})])
    assert stats["skipped"] == 1
    row = conn.execute("SELECT * FROM excluded_log").fetchone()
    assert row["rule_id"] == "ingest.incomplete_posting"
    assert "no id" in row["raw_payload"]


def test_one_bad_posting_does_not_abort_the_batch(conn):
    cid = company(conn)
    batch = [job(external_id="1"), NormalizedJob(external_id="", title="", url="",
                                                 source="greenhouse"),
             job(external_id="3", url="https://x/3")]
    stats = ingest.ingest(conn, cid, "Acme", batch)
    assert stats["new"] == 2 and stats["skipped"] == 1


# -------------------------------------------------------------- renormalizing
def test_renormalize_rederives_columns_from_stored_text(conn):
    cid = company(conn)
    ingest.ingest(conn, cid, "Acme", [job(jd_text="Salary is location agnostic.")])
    conn.execute("UPDATE jobs SET comp_model = 'wrong'")
    conn.commit()

    assert ingest.renormalize(conn) == 1
    assert conn.execute("SELECT comp_model FROM jobs").fetchone()["comp_model"] == "location_agnostic"


def test_company_matched_on_domain_before_name(conn):
    first = ingest.get_or_create_company(conn, "Acme Inc", "acme.com")
    same = ingest.get_or_create_company(conn, "Acme Corporation", "acme.com")
    assert first == same
