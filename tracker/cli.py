"""Phase 1 CLI: add-job, apply, status, contact add, list, export."""
import argparse
import json
import sys
from datetime import date

from . import db, fetch
from .dates import FOLLOWUP_BUSINESS_DAYS, add_business_days, parse_day


# --------------------------------------------------------------------------- helpers
def get_or_create_company(conn, name: str, domain: str | None = None) -> int:
    row = conn.execute(
        "SELECT id, domain FROM companies WHERE lower(name) = lower(?)", (name,)
    ).fetchone()
    if row:
        if domain and not row["domain"]:
            conn.execute("UPDATE companies SET domain = ? WHERE id = ?", (domain, row["id"]))
            conn.commit()
        return row["id"]
    cur = conn.execute("INSERT INTO companies (name, domain) VALUES (?,?)", (name, domain))
    conn.commit()
    return cur.lastrowid


def _fmt_date(value: str | None) -> str:
    d = parse_day(value)
    return d.isoformat() if d else "-"


# --------------------------------------------------------------------------- add-job
def cmd_add_job(conn, args) -> int:
    url = args.url.strip()
    existing = conn.execute("SELECT id, title FROM jobs WHERE url = ?", (url,)).fetchone()
    if existing:
        print(f"already tracked: job {existing['id']}  {existing['title']}")
        return 0

    title, jd_text, err = ("", "", None) if args.no_fetch else fetch.fetch_jd(url)
    if err:
        # Losing the row is worse than losing the description.
        print(f"warn: JD fetch failed ({err}); saving row with empty jd_text", file=sys.stderr)

    title = args.title or title or "(untitled — set with `status`/manual edit)"
    company_name = args.company or fetch.company_from_url(url)
    company_id = get_or_create_company(conn, company_name, fetch.domain_from_url(url))

    cur = conn.execute(
        "INSERT INTO jobs (company_id, title, url, source, seen_at, jd_text, source_urls)"
        " VALUES (?,?,?,?,?,?,?)",
        (company_id, title, url, args.source, db.now(), jd_text, json.dumps([url])),
    )
    job_id = cur.lastrowid
    conn.execute(
        "INSERT INTO applications (job_id, status, notes) VALUES (?, 'found', ?)",
        (job_id, args.notes),
    )
    conn.commit()
    print(f"job {job_id}  {company_name} — {title}")
    if not jd_text and not args.no_fetch:
        print("      (no jd_text captured)")
    return 0


# --------------------------------------------------------------------------- apply
def cmd_apply(conn, args) -> int:
    job = conn.execute(
        "SELECT j.id, j.title, c.name AS company FROM jobs j"
        " JOIN companies c ON c.id = j.company_id WHERE j.id = ?",
        (args.job_id,),
    ).fetchone()
    if not job:
        print(f"no job with id {args.job_id}", file=sys.stderr)
        return 1

    applied_on = date.today()
    due = add_business_days(applied_on, FOLLOWUP_BUSINESS_DAYS)
    app = conn.execute("SELECT id FROM applications WHERE job_id = ?", (args.job_id,)).fetchone()
    if app:
        conn.execute(
            "UPDATE applications SET status='applied', applied_at=?, followup_due=?,"
            " resume_version=COALESCE(?, resume_version) WHERE id = ?",
            (applied_on.isoformat(), due.isoformat(), args.resume, app["id"]),
        )
        app_id = app["id"]
    else:
        cur = conn.execute(
            "INSERT INTO applications (job_id, applied_at, status, followup_due, resume_version)"
            " VALUES (?,?, 'applied', ?, ?)",
            (args.job_id, applied_on.isoformat(), due.isoformat(), args.resume),
        )
        app_id = cur.lastrowid
    conn.execute("DELETE FROM followup_notices WHERE application_id = ?", (app_id,))
    conn.commit()
    print(f"app {app_id}  applied {applied_on}  {job['company']} — {job['title']}")
    print(f"          follow-up due {due} (+{FOLLOWUP_BUSINESS_DAYS} business days)")
    return 0


# --------------------------------------------------------------------------- status
def cmd_status(conn, args) -> int:
    if args.new_status not in db.STATUSES:
        print(f"unknown status '{args.new_status}'. one of: {', '.join(db.STATUSES)}",
              file=sys.stderr)
        return 1
    app = conn.execute("SELECT * FROM applications WHERE id = ?", (args.app_id,)).fetchone()
    if not app:
        print(f"no application with id {args.app_id}", file=sys.stderr)
        return 1

    conn.execute("UPDATE applications SET status = ? WHERE id = ?",
                 (args.new_status, args.app_id))
    if args.new_status == "followed_up":
        # A nudge restarts the clock rather than leaving the row permanently overdue.
        conn.execute(
            "UPDATE applications SET followup_due = ? WHERE id = ?",
            (add_business_days(date.today(), FOLLOWUP_BUSINESS_DAYS).isoformat(), args.app_id),
        )
    if args.new_status in db.CLOSED_STATUSES + ("offer",):
        conn.execute("UPDATE applications SET followup_due = NULL WHERE id = ?", (args.app_id,))
    conn.execute("DELETE FROM followup_notices WHERE application_id = ?", (args.app_id,))
    if args.notes:
        conn.execute("UPDATE applications SET notes = ? WHERE id = ?", (args.notes, args.app_id))
    conn.commit()
    print(f"app {args.app_id}: {app['status']} -> {args.new_status}")
    return 0


# --------------------------------------------------------------------------- contact add
def _prompt(label: str, required: bool = False) -> str:
    """Prompt interactively; fall back to empty when there is no stdin to read
    (scripted use, systemd), so a non-tty run fails loudly instead of hanging."""
    while True:
        try:
            value = input(f"{label}: ").strip()
        except EOFError:
            if required:
                raise SystemExit(f"\n{label.strip()} is required — pass it as a flag.")
            print()
            return ""
        if value or not required:
            return value
        print("  required.")


def cmd_contact_add(conn, args) -> int:
    company = conn.execute(
        "SELECT id, name, domain FROM companies WHERE id = ?", (args.company_id,)
    ).fetchone()
    if not company:
        print(f"no company with id {args.company_id}", file=sys.stderr)
        return 1

    print(f"contact for {company['name']} (id {company['id']})")
    # INV-2: name + title + company_id are mandatory before any draft can exist.
    name = args.name or _prompt("  name", required=True)
    title = args.title or _prompt("  title", required=True)
    email = (args.email or _prompt("  email (blank ok)")).strip().lower()
    confidence = args.confidence or ""
    if email and not confidence:
        confidence = _prompt("  confidence [published|verified|inferred]", required=True)
    if email and confidence not in db.EMAIL_CONFIDENCE:
        print(f"confidence must be one of {', '.join(db.EMAIL_CONFIDENCE)}", file=sys.stderr)
        return 1
    linkedin = args.linkedin or _prompt("  linkedin url (blank ok)")
    source = args.source or _prompt("  source (careers page / github / manual / referral)")

    # Name-collision guard: an address on the wrong domain is how you email a stranger.
    if email and company["domain"]:
        email_domain = email.rpartition("@")[2]
        known = company["domain"].removeprefix("www.")
        if email_domain and not (
            email_domain == known
            or email_domain.endswith("." + known)
            or known.endswith("." + email_domain)
        ):
            db.log_exclusion(
                conn,
                json.dumps({"company_id": company["id"], "company_domain": company["domain"],
                            "name": name, "title": title, "email": email,
                            "confidence": confidence, "source": source}),
                reason=f"email domain '{email_domain}' does not match company domain '{known}'",
                rule_id="contact.domain_mismatch",
            )
            print(f"refused: {email} is not on {known}. "
                  "Contact saved without the address; payload in excluded_log "
                  "(rule_id=contact.domain_mismatch) for manual review.", file=sys.stderr)
            email, confidence = "", ""

    cur = conn.execute(
        "INSERT INTO contacts (company_id, name, title, email, email_confidence, linkedin_url,"
        " source) VALUES (?,?,?,?,?,?,?)",
        (company["id"], name, title, email or None, confidence or None, linkedin or None,
         source or None),
    )
    conn.commit()
    shown = f"[UNVERIFIED: {email}]" if confidence == "inferred" else (email or "no email")
    print(f"contact {cur.lastrowid}  {name} — {title}  {shown}")
    if confidence == "inferred":
        print("      draft-only: never paste this into a To: field unverified.")
    return 0


# --------------------------------------------------------------------------- list
def cmd_list(conn, args) -> int:
    where = "" if args.all else f"WHERE a.status NOT IN ({','.join('?' * len(db.CLOSED_STATUSES))})"
    params = () if args.all else db.CLOSED_STATUSES
    rows = conn.execute(
        "SELECT a.id, a.status, a.applied_at, a.followup_due, j.title, c.name AS company"
        " FROM applications a JOIN jobs j ON j.id = a.job_id"
        f" JOIN companies c ON c.id = j.company_id {where}"
        " ORDER BY (a.followup_due IS NULL), a.followup_due, a.id",
        params,
    ).fetchall()
    if not rows:
        print("nothing tracked yet — `add-job <url>` to start.")
        return 0

    today = date.today()
    print(f"{'id':>4}  {'status':<12} {'company':<20} {'title':<40} {'due':<12} flag")
    print("-" * 100)
    for r in rows:
        due = parse_day(r["followup_due"])
        flag = ""
        if due:
            delta = (due - today).days
            flag = "OVERDUE" if delta < 0 else ("DUE TODAY" if delta == 0 else f"in {delta}d")
        print(f"{r['id']:>4}  {r['status']:<12} {r['company'][:20]:<20} "
              f"{r['title'][:40]:<40} {_fmt_date(r['followup_due']):<12} {flag}")
    print(f"\n{len(rows)} application(s).")
    return 0


# --------------------------------------------------------------------------- export
def cmd_export(conn, args) -> int:
    from .export import export

    path, count = export(conn, args.output)
    print(f"wrote {count} row(s) -> {path}")
    return 0


# --------------------------------------------------------------------- watchlist
def cmd_watchlist_add(conn, args) -> int:
    from . import watchlist
    from .ats import detect

    url = args.careers_url.strip()
    entries = watchlist.load()

    result = detect.Detection(ats="unknown") if args.no_detect else detect.detect(url)
    if args.ats:
        result = detect.Detection(ats=args.ats, slug=args.slug, evidence="manual")

    name = args.name or fetch.company_from_url(url)
    domain = args.domain or fetch.domain_from_url(url)

    if watchlist.find(entries, name=name, domain=domain or ""):
        print(f"already on the watchlist: {name}")
        return 0

    entry = {
        "name": name,
        "domain": domain,
        "ats": result.ats,
        "slug": result.slug,
        "careers_url": url,
        "source": args.source,
        "oss_repo": args.oss_repo,
        "priority": args.priority,
        "enabled": True,
    }
    # INV-1: detection failure never blocks the add. An entry sitting at
    # ats=unknown is visible and fixable; a company the user believed they
    # added and didn't is not recoverable.
    watchlist.append(entry)
    watchlist.sync(conn)

    if result.found:
        print(f"added {name}: {result.ats}/{result.slug} (detected via {result.evidence})")
    else:
        print(f"added {name} with ats: unknown", file=sys.stderr)
        print(f"warn: could not detect an ATS board — {result.error or 'no match'}.\n"
              f"      It will not be polled until you set `ats` and `slug` in "
              f"{watchlist.PATH}.", file=sys.stderr)
    return 0


def cmd_watchlist_list(conn, args) -> int:
    from . import watchlist

    watchlist.sync(conn)
    rows = conn.execute(
        "SELECT * FROM companies WHERE watchlist_enabled = 1 ORDER BY name"
    ).fetchall()
    if not rows:
        print(f"watchlist is empty — add companies to {watchlist.PATH}")
        return 0
    open_alerts = {
        a["target_id"] for a in conn.execute(
            "SELECT target_id FROM alerts WHERE resolved_at IS NULL AND target_type = 'company'"
        )
    }
    print(f"{'id':>4}  {'company':<24} {'ats':<11} {'slug':<24} {'prio':<6} {'last polled':<21} flag")
    print("-" * 104)
    for r in rows:
        flag = "STALE" if str(r["id"]) in open_alerts else ""
        if (r["ats"] or "unknown") == "unknown":
            flag = (flag + " needs-slug").strip()
        print(f"{r['id']:>4}  {r['name'][:24]:<24} {(r['ats'] or '-'):<11} "
              f"{(r['slug'] or '-')[:24]:<24} {watchlist.effective_priority(r):<6} "
              f"{(r['last_polled_at'] or 'never')[:21]:<21} {flag}")
    print(f"\n{len(rows)} company(ies).")
    return 0


def cmd_poll(conn, args) -> int:
    from . import poll

    poll.run(conn, only=args.only, force=args.force, workers=args.workers)
    return 0


def cmd_funding_poll(conn, args) -> int:
    from .funding import run as funding_run

    funding_run.run(conn, only=args.only, do_network=not args.no_resolve,
                    resolve_limit=args.resolve_limit)
    return 0


def cmd_candidate_list(conn, args) -> int:
    rows = conn.execute(
        "SELECT * FROM watchlist_candidates WHERE status = ? ORDER BY announced_at DESC, id DESC",
        (args.status,),
    ).fetchall()
    if not rows:
        print(f"no candidates with status '{args.status}'.")
        return 0
    for r in rows:
        detected = f"{r['resolved_ats']}/{r['resolved_slug']}" if r["resolved_slug"] else "ats unknown"
        print(f"[{r['id']:>3}] {r['name']} ({r['domain'] or 'no domain'}) — "
              f"{r['round_stage'] or '?'}, {r['amount_raw'] or 'undisclosed'} — {detected}")
        if r["reason"]:
            print(f"      {r['reason']}")
        if r["article_url"]:
            print(f"      {r['article_url']}")
    print(f"\n{len(rows)} candidate(s).")
    return 0


def cmd_candidate_approve(conn, args) -> int:
    from .funding import run as funding_run

    ok, message = funding_run.approve(conn, args.candidate_id)
    print(message, file=sys.stdout if ok else sys.stderr)
    return 0 if ok else 1


def cmd_candidate_reject(conn, args) -> int:
    from .funding import run as funding_run

    ok, message = funding_run.reject(conn, args.candidate_id)
    print(message, file=sys.stdout if ok else sys.stderr)
    return 0 if ok else 1


def cmd_renormalize(conn, args) -> int:
    from . import ingest

    print(f"re-derived comp_model/auth_required/hires_in_india for "
          f"{ingest.renormalize(conn)} job(s)")
    return 0


def cmd_digest(conn, args) -> int:
    from . import digest, notify

    text = digest.build(conn, hours=args.hours)
    print(text)
    if args.notify:
        first = text.split("\n\n")[1] if "\n\n" in text else text
        notify.send("Job pipeline digest", first[:400])
    return 0


# --------------------------------------------------------------------------- parser
def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="tracker", description="Job application pipeline (Phase 1)")
    sub = p.add_subparsers(dest="cmd", required=True)

    a = sub.add_parser("add-job", help="fetch JD, create company+job rows, print job id")
    a.add_argument("url")
    a.add_argument("--company")
    a.add_argument("--title")
    a.add_argument("--source", default="manual")
    a.add_argument("--notes")
    a.add_argument("--no-fetch", action="store_true", help="skip the HTTP fetch entirely")
    a.set_defaults(func=cmd_add_job)

    ap = sub.add_parser("apply", help="mark applied, schedule follow-up")
    ap.add_argument("job_id", type=int)
    ap.add_argument("--resume", help="resume version used")
    ap.set_defaults(func=cmd_apply)

    st = sub.add_parser("status", help="manual status transition")
    st.add_argument("app_id", type=int)
    st.add_argument("new_status", choices=db.STATUSES, metavar="status")
    st.add_argument("--notes")
    st.set_defaults(func=cmd_status)

    ct = sub.add_parser("contact", help="contact management")
    ctsub = ct.add_subparsers(dest="contact_cmd", required=True)
    ca = ctsub.add_parser("add", help="interactive contact entry")
    ca.add_argument("company_id", type=int)
    for flag in ("name", "title", "email", "linkedin", "source"):
        ca.add_argument(f"--{flag}")
    ca.add_argument("--confidence", choices=db.EMAIL_CONFIDENCE)
    ca.set_defaults(func=cmd_contact_add)

    ls = sub.add_parser("list", help="active applications, sorted by followup_due")
    ls.add_argument("--all", action="store_true", help="include rejected and ghosted")
    ls.set_defaults(func=cmd_list)

    ex = sub.add_parser("export", help="write tracker.xlsx")
    ex.add_argument("-o", "--output", default="tracker.xlsx")
    ex.set_defaults(func=cmd_export)

    wl = sub.add_parser("watchlist", help="the polled company list")
    wlsub = wl.add_subparsers(dest="watchlist_cmd", required=True)
    wa = wlsub.add_parser("add", help="detect the ATS behind a careers page and add it")
    wa.add_argument("careers_url")
    wa.add_argument("--name")
    wa.add_argument("--domain")
    wa.add_argument("--ats", choices=db.ATS_PROVIDERS, help="skip detection, set it yourself")
    wa.add_argument("--slug")
    wa.add_argument("--source", default="manual", help="where this company came from")
    wa.add_argument("--oss-repo", dest="oss_repo")
    wa.add_argument("--priority", choices=db.PRIORITIES, default="normal")
    wa.add_argument("--no-detect", action="store_true", help="do not fetch the page")
    wa.set_defaults(func=cmd_watchlist_add)
    wls = wlsub.add_parser("list", help="watchlist with poll state")
    wls.set_defaults(func=cmd_watchlist_list)

    pl = sub.add_parser("poll", help="poll watchlist ATS boards for new roles")
    pl.add_argument("--only", help="restrict to one company (name or slug)")
    pl.add_argument("--force", action="store_true", help="ignore the poll cadence")
    pl.add_argument("--workers", type=int, default=5)
    pl.set_defaults(func=cmd_poll)

    fn = sub.add_parser("funding", help="funding-signal ingest")
    fnsub = fn.add_subparsers(dest="funding_cmd", required=True)
    fp = fnsub.add_parser("poll", help="check funding sources for new rounds")
    fp.add_argument("--only", help="restrict to one source")
    fp.add_argument("--no-resolve", action="store_true",
                    help="skip article fetches used for domain resolution")
    fp.add_argument("--resolve-limit", type=int, default=15,
                    help="max article fetches this run")
    fp.set_defaults(func=cmd_funding_poll)

    cd = sub.add_parser("candidate", help="review companies suggested by funding signals")
    cdsub = cd.add_subparsers(dest="candidate_cmd", required=True)
    cl = cdsub.add_parser("list", help="candidates awaiting review")
    cl.add_argument("--status", choices=db.CANDIDATE_STATUSES, default="needs_review")
    cl.set_defaults(func=cmd_candidate_list)
    capp = cdsub.add_parser("approve", help="add a candidate to watchlist.yaml")
    capp.add_argument("candidate_id", type=int)
    capp.set_defaults(func=cmd_candidate_approve)
    crej = cdsub.add_parser("reject", help="dismiss a candidate")
    crej.add_argument("candidate_id", type=int)
    crej.set_defaults(func=cmd_candidate_reject)

    rn = sub.add_parser("renormalize", help="re-run scoring heuristics over stored jobs")
    rn.set_defaults(func=cmd_renormalize)

    dg = sub.add_parser("digest", help="alerts, funding window, new roles")
    dg.add_argument("--hours", type=int, default=48, help="new-role lookback window")
    dg.add_argument("--notify", action="store_true", help="also fire a desktop notification")
    dg.set_defaults(func=cmd_digest)
    return p


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    conn = db.connect()
    try:
        return args.func(conn, args)
    except KeyboardInterrupt:
        print("\naborted.", file=sys.stderr)
        return 130
    finally:
        conn.close()


if __name__ == "__main__":
    raise SystemExit(main())
