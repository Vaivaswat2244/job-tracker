"""Orchestrate one funding-signal run.

Nothing here writes to `companies` except to update a company the user already
put on the watchlist. New companies stop at `watchlist_candidates` with
status='needs_review' and wait for an explicit approval.
"""
import json
import sys
from datetime import timedelta

from .. import db, health
from ..normalize import parse_dt
from . import extract, resolve, sources

FUNDING_WINDOW_DAYS = 60
MAX_RESOLVE_PER_RUN = 15      # article fetches; the listing itself is one request


# --------------------------------------------------------------- source state
def _state(conn, name: str):
    return conn.execute(
        "SELECT * FROM funding_source_state WHERE name = ?", (name,)
    ).fetchone()


def _save_state(conn, name: str, etag=None, last_modified=None) -> None:
    conn.execute(
        "INSERT INTO funding_source_state (name, etag, last_modified, last_run_at)"
        " VALUES (?,?,?,?) ON CONFLICT(name) DO UPDATE SET"
        " etag=excluded.etag, last_modified=excluded.last_modified,"
        " last_run_at=excluded.last_run_at",
        (name, etag, last_modified, db.now()),
    )
    conn.commit()


# -------------------------------------------------------------------- storage
def store_item(conn, item, extraction) -> int | None:
    """Insert one funding item. Returns its id, or None if already stored.

    Partial extraction is stored, not dropped: a row with only a company name
    and a date is still a lead.
    """
    cur = conn.execute(
        "INSERT OR IGNORE INTO funding_items (source, headline, article_url, published_at,"
        " company_name, round_stage, amount_raw, currency, investors, announced_at,"
        " extraction_confidence, extraction_method, raw_text, llm_output, created_at)"
        " VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
        (item.source, item.headline, item.url, item.published_at,
         extraction.company_name, extraction.round_stage, extraction.amount_raw,
         extraction.currency, json.dumps(extraction.investors), extraction.announced_at,
         extraction.confidence, extraction.method, extraction.raw_text,
         extraction.llm_raw, db.now()),
    )
    conn.commit()
    if not cur.rowcount:
        return None
    return cur.lastrowid


def add_candidate(conn, extraction, domain, reason, ats=None, slug=None) -> None:
    # `name` is NOT NULL, and OR IGNORE (there for the dedupe index) would
    # swallow the constraint failure and drop the item without a trace. An
    # unparsed headline is exactly the case a human should look at, so it gets
    # a placeholder name and stays visible.
    name = extraction.company_name or f"(unparsed) {(extraction.raw_text or '?')[:90]}"
    conn.execute(
        "INSERT OR IGNORE INTO watchlist_candidates (name, domain, round_stage, amount_raw,"
        " announced_at, article_url, resolved_ats, resolved_slug, status, reason, created_at)"
        " VALUES (?,?,?,?,?,?,?,?,'needs_review',?,?)",
        (name, domain, extraction.round_stage, extraction.amount_raw,
         extraction.announced_at, extraction.article_url, ats, slug, reason, db.now()),
    )
    conn.commit()


def apply_funding(conn, company_id: int, extraction) -> None:
    """Confirmed match: record the raise and open the hiring window.

    The window is stored as an expiry rather than a flag, so it decays on its
    own when the date passes — there is no cleanup job that can fail to run.
    """
    announced = parse_dt(extraction.announced_at)
    until = (announced + timedelta(days=FUNDING_WINDOW_DAYS)).isoformat(timespec="seconds") \
        if announced else None
    conn.execute(
        "UPDATE companies SET recently_funded_at = ?, funding_stage = ?,"
        " funding_amount_raw = ?, priority_until = ? WHERE id = ?",
        (extraction.announced_at, extraction.round_stage, extraction.amount_raw,
         until, company_id),
    )
    conn.commit()


def detect_ats_for(domain: str):
    """Task A detection against a resolved domain, for the approval preview."""
    from ..ats import detect

    for url in (f"https://{domain}/careers", f"https://{domain}/jobs", f"https://{domain}"):
        result = detect.detect(url)
        if result.found:
            return result.ats, result.slug
    return None, None


# ------------------------------------------------------------------ resolution
def resolve_item(conn, extraction, do_network: bool = True) -> str:
    """Attach an item to a company, or send it to review. Returns the outcome."""
    if not extraction.company_name:
        add_candidate(conn, extraction, None,
                      "extraction found no company name — headline needs a human")
        return "needs_review"

    domain, reason = resolve.resolve_domain(extraction.article_url, extraction.company_name) \
        if do_network else (None, "resolution skipped")

    collisions = resolve.name_collisions(conn, extraction.company_name)

    if domain:
        matched = resolve.match_on_domain(conn, domain)
        if matched:
            apply_funding(conn, matched["id"], extraction)
            return "confirmed"

        # Resolved, but not a company we track. This is the one case where a
        # name collision would have produced a wrong match, so say so plainly.
        note = f"resolved to {domain}; not on the watchlist"
        if collisions:
            names = ", ".join(f"{c['name']} ({c['domain'] or 'no domain'})" for c in collisions)
            note += (f". NAME COLLISION: watchlist already has {names} — different domain, "
                     "so NOT matched")
        ats, slug = detect_ats_for(domain) if do_network else (None, None)
        add_candidate(conn, extraction, domain, note, ats, slug)
        return "needs_review"

    note = f"no domain resolved ({reason})"
    if collisions:
        names = ", ".join(f"{c['name']} ({c['domain'] or 'no domain'})" for c in collisions)
        note += f". Name resembles watchlist entry: {names} — not matched without a domain"
    add_candidate(conn, extraction, None, note)
    return "needs_review"


# -------------------------------------------------------------------- the run
def run(conn, only: str | None = None, resolve_limit: int = MAX_RESOLVE_PER_RUN,
        do_network: bool = True, verbose: bool = True, config_path: str | None = None) -> dict:
    summary = {"sources": 0, "items": 0, "funding": 0, "stored": 0, "near_miss": 0,
               "confirmed": 0, "needs_review": 0, "alerts": [], "failed": 0}
    rule_set = extract.rules()

    for config in sources.load_config(config_path):
        if config.get("enabled") is False:
            continue
        name = config["name"]
        if only and only.lower() != name.lower():
            continue
        summary["sources"] += 1

        state = _state(conn, name)
        result = sources.fetch(config,
                               etag=state["etag"] if state else None,
                               last_modified=state["last_modified"] if state else None)

        health.record_poll(
            conn, "funding_source", name,
            http_status=result.status,
            item_count=None if result.not_modified or result.items is None else result.items_found,
            ok=result.parse_ok,
            error=result.error,
            meta=json.dumps({"parse_ok": result.parse_ok, "items_found": result.items_found,
                             "selector_version": result.selector_version}),
        )
        reason = health.check(conn, "funding_source", name, f"{name} funding feed")
        if reason:
            summary["alerts"].append((name, reason))

        if result.not_modified:
            if verbose:
                print(f"  {name:<12} 304 not modified")
            _save_state(conn, name, result.etag, result.last_modified)
            continue
        if not result.parse_ok:
            summary["failed"] += 1
            print(f"  {name:<12} FAILED  {result.error}", file=sys.stderr)
            continue

        summary["items"] += result.items_found
        pending = []
        for item in result.items:
            if not extract.is_funding(item.headline, rule_set):
                if extract.is_near_miss(item.headline, rule_set):
                    # Mentions money but matched no trigger. Not stored as a
                    # round, but never dropped without a trace either.
                    db.log_exclusion(
                        conn, json.dumps({"headline": item.headline, "url": item.url}),
                        reason="mentions money but matched no funding trigger",
                        rule_id="funding.not_a_round",
                    )
                    summary["near_miss"] += 1
                continue

            summary["funding"] += 1
            extraction = extract.extract(item.headline, item.url, item.published_at, rule_set)
            if extraction.confidence == "low" and extract.llm_enabled():
                extraction = extract.extract_with_llm(item.headline, extraction)
            if store_item(conn, item, extraction) is not None:
                summary["stored"] += 1
                pending.append(extraction)

        if verbose:
            print(f"  {name:<12} {result.items_found:>3} items, "
                  f"{len(pending)} new funding row(s)")

        for extraction in pending:
            if resolve_limit <= 0:
                break
            outcome = resolve_item(conn, extraction, do_network=do_network)
            summary[outcome] = summary.get(outcome, 0) + 1
            resolve_limit -= 1

        _save_state(conn, name, result.etag, result.last_modified)

    if verbose:
        print(f"\n{summary['sources']} source(s), {summary['items']} items, "
              f"{summary['stored']} new funding row(s), {summary['confirmed']} confirmed, "
              f"{summary['needs_review']} need review, {summary['near_miss']} near-misses logged")
        for name, reason in summary["alerts"]:
            print(f"  ALERT stale_feed: {name} ({reason})", file=sys.stderr)
    return summary


# ------------------------------------------------------------------- approval
def approve(conn, candidate_id: int, path: str | None = None) -> tuple[bool, str]:
    """The only path from a candidate into the watchlist, and it is explicit."""
    from .. import watchlist

    row = conn.execute("SELECT * FROM watchlist_candidates WHERE id = ?",
                       (candidate_id,)).fetchone()
    if not row:
        return False, f"no candidate with id {candidate_id}"
    if row["status"] != "needs_review":
        return False, f"candidate {candidate_id} is already {row['status']}"

    entries = watchlist.load(path)
    if watchlist.find(entries, name=row["name"] or "", domain=row["domain"] or ""):
        conn.execute("UPDATE watchlist_candidates SET status='approved' WHERE id = ?",
                     (candidate_id,))
        conn.commit()
        return True, f"{row['name']} was already on the watchlist"

    watchlist.append({
        "name": row["name"],
        "domain": row["domain"],
        "ats": row["resolved_ats"] or "unknown",
        "slug": row["resolved_slug"],
        "careers_url": f"https://{row['domain']}/careers" if row["domain"] else None,
        "source": f"funding:{row['round_stage'] or 'unknown'}",
        "priority": "high",       # freshly funded: the whole point is to watch closely
        "enabled": True,
    }, path)
    watchlist.sync(conn, path)

    if row["domain"]:
        company = resolve.match_on_domain(conn, row["domain"])
        if company:
            conn.execute(
                "UPDATE companies SET recently_funded_at = ?, funding_stage = ?,"
                " funding_amount_raw = ?, priority_until = ? WHERE id = ?",
                (row["announced_at"], row["round_stage"], row["amount_raw"],
                 (parse_dt(row["announced_at"]) + timedelta(days=FUNDING_WINDOW_DAYS)
                  ).isoformat(timespec="seconds") if parse_dt(row["announced_at"]) else None,
                 company["id"]),
            )
    conn.execute("UPDATE watchlist_candidates SET status='approved' WHERE id = ?",
                 (candidate_id,))
    conn.commit()
    detail = (f"{row['resolved_ats']}/{row['resolved_slug']}" if row["resolved_slug"]
              else "ats unknown — set a slug in watchlist.yaml to start polling")
    return True, f"added {row['name']} to the watchlist ({detail})"


def reject(conn, candidate_id: int) -> tuple[bool, str]:
    cur = conn.execute(
        "UPDATE watchlist_candidates SET status='rejected'"
        " WHERE id = ? AND status = 'needs_review'", (candidate_id,))
    conn.commit()
    if not cur.rowcount:
        return False, f"no reviewable candidate with id {candidate_id}"
    return True, f"candidate {candidate_id} dismissed"
