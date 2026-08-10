"""Poll the watchlist.

Network work is fanned out across a small thread pool; every database write
happens back on the calling thread. SQLite connections are not thread-safe, and
serialising the writes costs nothing next to the HTTP time.
"""
import sys
from concurrent.futures import ThreadPoolExecutor

from . import ats, health, ingest, watchlist

WORKERS = 5


def _select(conn, only: str | None, force: bool) -> tuple[list, int]:
    rows = watchlist.pollable(conn)
    if only:
        needle = only.strip().lower()
        rows = [r for r in rows
                if needle in (r["name"] or "").lower() or needle == (r["slug"] or "").lower()]
    if force:
        return rows, 0
    due = [r for r in rows if watchlist.due_for_poll(r)]
    return due, len(rows) - len(due)


def _fetch_one(row) -> tuple[int, object]:
    module = ats.adapter(row["ats"])
    if module is None:
        return row["id"], ats.FetchResult(jobs=None, error=f"no adapter for ats={row['ats']}")
    try:
        return row["id"], module.fetch(
            row["slug"], etag=row["poll_etag"], last_modified=row["poll_last_modified"]
        )
    except Exception as exc:
        # A crashing adapter must still register as a failed poll, or a company
        # that breaks on every parse would never trip feed-death detection.
        return row["id"], ats.FetchResult(jobs=None, error=f"{type(exc).__name__}: {exc}")


def run(conn, only: str | None = None, force: bool = False, workers: int = WORKERS,
        verbose: bool = True) -> dict:
    watchlist.sync(conn)
    rows, skipped = _select(conn, only, force)
    summary = {"polled": 0, "skipped_cadence": skipped, "new": 0, "updated": 0,
               "not_modified": 0, "failed": 0, "alerts": []}
    if not rows:
        if verbose:
            print(f"nothing due ({skipped} within cadence). --force to poll anyway.")
        return summary

    by_id = {r["id"]: r for r in rows}
    with ThreadPoolExecutor(max_workers=max(1, workers)) as pool:
        results = list(pool.map(_fetch_one, rows))

    for company_id, result in results:
        row = by_id[company_id]
        label = row["name"]
        summary["polled"] += 1

        reason = health.poll_health(conn, company_id, result, label=label)
        if reason:
            summary["alerts"].append((label, reason))

        if result.not_modified:
            summary["not_modified"] += 1
            watchlist.mark_polled(conn, company_id, row["poll_etag"], row["poll_last_modified"])
            if verbose:
                print(f"  {label:<24} 304 not modified")
            continue

        if not result.ok:
            summary["failed"] += 1
            watchlist.mark_polled(conn, company_id, row["poll_etag"], row["poll_last_modified"])
            print(f"  {label:<24} FAILED  {result.error}", file=sys.stderr)
            continue

        stats = ingest.ingest(conn, company_id, label, result.jobs)
        summary["new"] += stats["new"]
        summary["updated"] += stats["updated"]
        watchlist.mark_polled(conn, company_id, result.etag, result.last_modified)
        if verbose:
            print(f"  {label:<24} {len(result.jobs):>3} jobs  "
                  f"(+{stats['new']} new, {stats['updated']} updated)")

    if verbose:
        print(f"\npolled {summary['polled']}, {summary['new']} new, "
              f"{summary['updated']} updated, {summary['not_modified']} unchanged, "
              f"{summary['failed']} failed, {summary['skipped_cadence']} within cadence")
        for label, reason in summary["alerts"]:
            print(f"  ALERT stale_feed: {label} ({reason})", file=sys.stderr)
    return summary
