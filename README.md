# Job Application Pipeline — Phases 1-3

Tracker, follow-up, ATS watchlist polling, and funding-signal ingest. Local-first, single
user, zero recurring cost. SQLite is the source of truth; the spreadsheet is a generated
view and is never read back.

| phase | what it does | status |
|---|---|---|
| 1 | tracker, follow-up ladder, xlsx export | built |
| 2 | ATS watchlist polling (Tier 2 ingest) | built |
| 3 | funding-signal ingest (Tier 3) | built |
| 4 | semantic scoring, contact discovery | not built |

## Setup

```bash
python3 -m venv .venv && .venv/bin/pip install -r requirements.txt
cp .env.example .env          # optional, for phone push
./systemd/install.sh          # installs all four timers
.venv/bin/python -m pytest -q # 209 tests, no network
```

Four user timers are installed: hourly `poll`, twice-daily `funding poll`, the 09:00
`digest`, and the 09:30 follow-up check.

Optional phone push via ntfy. Public topics are readable by anyone who knows the name,
so generate an unguessable one and keep it out of git (`.env` is already ignored):

```bash
python3 -c "import secrets; print('NTFY_TOPIC=' + secrets.token_urlsafe(18))" >> .env
```

## Daily use

```bash
./tracker.sh add-job <url>              # -> job id, in about five seconds
./tracker.sh apply <job_id>             # applied, follow-up in +5 business days
./tracker.sh status <app_id> <status>   # found|applied|followed_up|in_process|offer|rejected|ghosted
./tracker.sh contact add <company_id>   # interactive; flags also accepted
./tracker.sh list                       # active applications, soonest follow-up first
./tracker.sh export                     # tracker.xlsx
```

`add-job` flags: `--company`, `--title`, `--source`, `--notes`, `--no-fetch`.
Symlink it onto your PATH (`ln -s "$PWD/tracker.sh" ~/.local/bin/tracker`) if you want
`tracker add-job <url>` from anywhere.

Ingest and review:

```bash
./tracker.sh watchlist add <careers_url>   # detect the ATS, append to watchlist.yaml
./tracker.sh watchlist list                # poll state, stale flags, effective priority
./tracker.sh poll [--only X] [--force]     # poll ATS boards
./tracker.sh funding poll                  # check funding sources
./tracker.sh candidate list                # companies awaiting your approval
./tracker.sh candidate approve <id>        # the only route into the watchlist
./tracker.sh digest                        # alerts, funded window, new roles
./tracker.sh renormalize                   # re-derive heuristics over stored jobs
```

A JD fetch failure is never fatal — the row is created with an empty `jd_text` and a
warning on stderr. Losing the row is worse than losing the description. `add-job` on a
URL already tracked prints the existing job id rather than creating a duplicate.

## Follow-ups

`check_followups.py` runs daily at 09:30 via a systemd **user** timer and fires
`notify-send` (plus ntfy if configured). The body carries role, company, first contact,
and days elapsed — enough to act without opening anything.

| when | action |
|---|---|
| due | notify |
| +3 days, no status change | renotify once |
| +7 days | notify, urgency critical, suggest `ghosted` |
| 30 days since applying, no reply | auto-set `ghosted` |

`ghosted` at 30 days is the **only** automatic transition. Each stage notifies exactly
once, tracked in `followup_notices`; `apply` and any `status` change clear that history so
the ladder restarts. Marking `followed_up` also pushes `followup_due` out another 5
business days.

Check it without waiting for the timer:

```bash
.venv/bin/python check_followups.py --dry-run   # notifies, records nothing
systemctl --user list-timers tracker-followups.timer
journalctl --user -u tracker-followups.service -n 20
```

## Phase 2 — ATS watchlist polling

`watchlist.yaml` is hand-curated and version-controlled; it decides *membership*. The DB
holds operational state (`last_polled_at`, etag, funding window) and is never written back
to the YAML. `watchlist add <careers_url>` detects Greenhouse, Lever or Ashby by following
one level of redirect and iframe, including the Greenhouse `embed/job_board?for=` form.
**Detection failure never blocks the add** — the entry lands with `ats: unknown` and a
warning. An unpolled company in the list is recoverable; one the user believed they added
is not.

Cadence is per company: `priority: high` every 3h, `normal` every 6h, tracked in
`last_polled_at` and overridable with `--force`. Requests are capped at 5 concurrent with
one in-flight request per host, send `If-Modified-Since`/`If-None-Match`, honour 304
without touching the DB, retry 5xx/429 three times with exponential backoff, and never
retry other 4xx.

**Feed-death detection is the important part.** A wrong slug 404s forever; a renamed board
returns `[]` forever. Both look exactly like "not hiring". Every poll is recorded in
`poll_log`, and a `stale_feed` alert fires when either 3 consecutive polls fail or a board
that previously returned jobs returns 0 three times running. Alerts sit at the top of the
digest, fire a `notify-send` **once** rather than daily, and never auto-disable the
company — it keeps its entry and keeps being polled.

```bash
./tracker.sh poll --force            # ignore cadence
./tracker.sh watchlist list          # STALE / needs-slug flags
sqlite3 tracker.db "SELECT * FROM alerts WHERE resolved_at IS NULL"
```

Two normalization rules are worth knowing. `auth_required` is a **flag, not a filter**:
flagged roles still appear, sorted lower. And it deliberately ignores US export-control
boilerplate ("sponsorship for an export license"), which otherwise flags a third of every
large US board including genuine India roles. `hires_in_india` lets the posting's own
location field win over body boilerplate — a company with a Bengaluru office mentions
India in every JD footer, which would otherwise mark a Foster City role India-friendly.

Cross-source dedupe links (`canonical_id`) and never deletes. It is **cross-source only**:
within one board, two postings with distinct external IDs are distinct postings, and
collapsing "one role opened in six cities" would lose five real application URLs.

## Phase 3 — funding-signal ingest

A startup that closes a Series A or B starts hiring engineers two to eight weeks later,
often before anything is posted. `funding poll` reads Entrackr, Inc42 and VCCircle,
extracts the round, and uses it to drive watchlist priority. It applies to nothing and
contacts nobody.

Sources are configured in `funding_sources.yaml`, never hardcoded. RSS is preferred
(Entrackr `/rss`, Inc42 `/feed`); VCCircle has no working feed, so its listing page is read
via the embedded `__NEXT_DATA__` JSON — its CSS class names are build-hashed
(`newsCard_article-wrapper__E1o4O`) and change on every deploy. Each run records
`parse_ok`, `items_found` and `selector_version`, and a source that previously returned
items and now returns none raises the **same `stale_feed` alert** as Phase 2. Only
headline, link, date and extracted entities are stored — never article bodies.

Extraction is deterministic regex plus `funding_rules.yaml`. The LLM path is behind
`TRACKER_FUNDING_LLM`, off by default, fills only fields the rules left empty, and stores
both the raw text and the raw response. Partial extraction is never a reason to discard:
a row with a company and a date is stored at `extraction_confidence: low`.

**The collision guard.** Company names collide constantly, and matching "Atlas" the
Bangalore fintech to "Atlas" the US logistics company corrupts the watchlist and
eventually produces outreach to the wrong company. So:

1. resolve to a **domain** — from a link in the article, or a name-derived domain that is
   fetched and confirmed to name the company;
2. match `watchlist.yaml` **on domain only**, never on name;
3. anything else becomes a `watchlist_candidates` row with `status: needs_review`.

Single-word names (`Atlas`, `Vaaree`) are deliberately **never** guessed — that is the
exact collision, and a guessed domain would verify against content legitimately containing
the word. Those go to review.

On a confirmed match: `recently_funded_at`, `funding_stage` and `funding_amount_raw` are
set and priority is raised to `high` for 60 days. The window is stored as an expiry and
evaluated on read, so it decays on its own — no cleanup job can fail to run. For a company
not yet on the watchlist, Phase 2's ATS detection runs against the resolved domain and the
result is stored on the candidate row for one-key approval. **No path in Phase 3 inserts
into `companies` without `candidate approve`.**

## Invariants this code enforces

**INV-1 — never silently lose an opportunity.** Ingest is append-only. `add-job` always
writes a row. Dedupe links (`canonical_id`, `source_urls`) and never deletes. Every
exclusion writes to `excluded_log` with `reason` and `rule_id`:

```bash
sqlite3 tracker.db "SELECT rule_id, reason, raw_payload FROM excluded_log"
```

**INV-2 — never send an email the user did not read.** There is no send capability, and
none should be added until draft quality is proven over ~20 real sends. `outreach` rows
exist for Phase 3 and default to `status='draft'`. A contact needs name + title +
company_id. `inferred` addresses render as `[UNVERIFIED: …]` everywhere they surface, so
they cannot be pasted by accident. If a contact's email domain does not match the
company's known domain, the address is refused, the payload goes to `excluded_log`
(`rule_id=contact.domain_mismatch`), and the contact is kept without it.

Company domains are deliberately **not** inferred from ATS or aggregator URLs — treating
`jobs.lever.co` as the company domain would make the collision guard reject every genuine
address. Set the real domain when you know it:

```bash
sqlite3 tracker.db "UPDATE companies SET domain='example.com' WHERE id=1"
```

## Layout

```
watchlist.yaml           hand-curated companies to poll
funding_sources.yaml     per-source feed/selector config + selector_version
funding_rules.yaml       extraction regexes (triggers, stages, currencies)

tracker/db.py            schema, migrations, excluded_log
tracker/cli.py           every command
tracker/fetch.py         tolerant JD fetch, company/domain inference
tracker/export.py        openpyxl -> tracker.xlsx, status colours
tracker/notify.py        notify-send + optional ntfy
tracker/dates.py         business-day arithmetic
tracker/http.py          retries, backoff, conditional GET, per-host politeness, robots
tracker/textutil.py      HTML/entity coercion shared by adapters
tracker/normalize.py     comp_model, auth_required, hires_in_india, dedupe keys
tracker/watchlist.py     YAML load/save/sync, poll cadence, priority decay
tracker/ingest.py        idempotent upsert, cross-source dedupe, renormalize
tracker/health.py        poll_log, stale_feed detection, alerts
tracker/poll.py          concurrent poll orchestration
tracker/digest.py        alerts -> funded -> needs-review -> new roles
tracker/ats/             detect.py + greenhouse/lever/ashby adapters
tracker/funding/         sources.py, extract.py, resolve.py, run.py
check_followups.py       escalation ladder, run by the timer
tests/                   209 offline tests + captured provider fixtures
```

## Not built yet, on purpose

Aggregator ingest, Phase 4 semantic scoring, and contact discovery. No HTML scraping of
careers pages for job content: if a company's ATS is not one of the three supported
providers it stays `unknown` and jobs get added manually, rather than growing a bespoke
scraper per company.

Out of scope entirely: auto-send, LinkedIn, a web UI, multi-user, paid data sources
(Tracxn, Crunchbase Pro), sentiment analysis, "hotness" scoring, dashboards.
