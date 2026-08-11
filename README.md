# Job Application Pipeline — Phases 1-3

Tracker, follow-up, ATS watchlist polling, and funding-signal ingest. Local-first, single
user, zero recurring cost. SQLite is the source of truth; the spreadsheet is a generated
view and is never read back.

Written in Go: one static binary, no venv, no interpreter on the timer path.

| phase | what it does | status |
|---|---|---|
| 1 | tracker, follow-up ladder, xlsx export | built |
| 2 | ATS watchlist polling (Tier 2 ingest) | built |
| 3 | funding-signal ingest (Tier 3) | built |
| 4 | semantic scoring, contact discovery | not built |
| — | Gmail ingest of application state | built |

## Setup

```bash
make build                    # -> bin/tracker
cp .env.example .env          # optional, for phone push
make install-timers           # builds, then installs all six timers
make test                     # go test ./...
```

Six user timers are installed: hourly `poll`, twice-daily `funding poll`, the 09:00
`digest`, the 09:30 follow-up check, the 09:05 Google Sheet push, and the two-hourly
Gmail read. Each fires under `Persistent=true`, so a run missed while the laptop was asleep happens on wake rather than
being skipped.

The hourly poll timer is a *check*, not a poll: a company is only fetched once its cadence
has elapsed — 3h at `high` priority, 6h at `normal` — so a `normal` company is polled about
four times a day. `tracker-sheet` and `tracker-mail` print setup instructions and exit 0
until they are configured: never set up is a choice, and a daily timer should not turn red
over it. Half set up, or set up wrongly, exits 1.

Optional phone push via ntfy. Public topics are readable by anyone who knows the name,
so generate an unguessable one and keep it out of git (`.env` is already ignored):

```bash
echo "NTFY_TOPIC=$(tr -dc 'A-Za-z0-9_-' </dev/urandom | head -c 24)" >> .env
```

## Daily use

`./tracker.sh` builds on first use and execs `bin/tracker`, so it works from any
directory; `bin/tracker` directly is the same thing once built.

```bash
./tracker.sh add-job <url>              # -> job id, in about five seconds
./tracker.sh apply <job_id>             # applied, follow-up in +5 business days
./tracker.sh status <app_id> <status>   # found|applied|followed_up|in_process|offer|rejected|ghosted
./tracker.sh contact add <company_id>   # interactive; flags also accepted
./tracker.sh list                       # active applications, soonest follow-up first
./tracker.sh jobs                       # everything poll ingested (see below)
./tracker.sh export                     # tracker.xlsx
./tracker.sh sheet push                 # -> the shared Google Sheet
./tracker.sh mail poll                  # read Gmail for confirmations/rejections
./tracker.sh mail list                  # mail the ingest could not place
./tracker.sh mail accounts               # which mailboxes are connected
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
./tracker.sh followups                     # the ladder; what the 09:30 timer runs
```

A JD fetch failure is never fatal — the row is created with an empty `jd_text` and a
warning on stderr. Losing the row is worse than losing the description. `add-job` on a
URL already tracked prints the existing job id rather than creating a duplicate.

## Seeing what was ingested

`list` shows **applications** — things you applied to or added by hand. `jobs` shows the
**pipeline**: every role `poll` collected, which is the far larger number and is invisible
to `list` by design.

```bash
./tracker.sh jobs --india --title engineer     # India-friendly engineering roles
./tracker.sh jobs --company grafana --since 7d # one company, last week
./tracker.sh jobs --remote --limit 0 --urls    # everything remote, with apply links
```

Filters: `--company`, `--title` (substrings, case-insensitive), `--india`, `--remote`,
`--since` (`7d`, `48h`, `2w`, or `2026-08-01`), `--limit` (default 50, `0` for all),
`--dupes`, `--urls`.

Ordering is the same everywhere: auth-gated roles sort **last**, India-friendly roles
first, then newest. A flag never removes a row (INV-1). Rows linked to a canonical
duplicate are hidden unless `--dupes`.

Both flags are compared through `COALESCE`, because in SQLite `NULL = 1` is NULL, not
false, and NULL sorts last under DESC. Without it every job whose geography could not be
determined — 257 on current data, and every hand-added referral, which never gets the
column set — would sort below known-not-India roles. Unknown is not the same as no.

## Follow-ups

`tracker followups` runs daily at 09:30 via a systemd **user** timer and fires
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
./tracker.sh followups --dry-run   # notifies, records nothing
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

## The spreadsheet

`export` writes three sheets, one-way, and never reads state back:

| sheet | rows | what it is |
|---|---|---|
| Applications | one per application | what you applied to, status-coloured |
| Pipeline | one per ingested job | everything `poll` collected |
| Contacts | one per contact | `inferred` addresses render `[UNVERIFIED: …]` |

Applications starts `FROM applications`, so it is **empty until you apply to something** —
that is correct, not a bug, but it meant the workbook said nothing about the 2000+ roles
already collected. Pipeline is that view: same filter and ordering as `jobs`, shared code,
so the sheet and the terminal cannot disagree. India-friendly cells are filled green,
auth-required amber, and both sheets carry a freeze pane and an autofilter.

## The shared Google Sheet

`sheet push` sends **Pipeline and Applications** to a Google Sheet you own, so the pipeline
can be read on a phone or shared with friends. Same direction as the xlsx: one-way, never
read back. Both destinations build their rows from `internal/export`, so the sheet, the
workbook and `jobs` cannot disagree.

**Contacts is deliberately not pushed.** It holds other people's names, titles and email
addresses, including `inferred` ones that exist only as guesses. A document shared with
other people is the wrong home for third parties' contact details, and an unverified guess
read by someone else is the harm INV-2 exists to prevent. Contacts stays in the local xlsx.
`TestContactsAreNeverPushed` is what stops someone quietly adding it.

Setup, once (`tracker sheet setup` prints this):

1. Create an empty spreadsheet in your own Drive — **you** stay the owner, so sharing and
   revoking work the ordinary way.
2. In console.cloud.google.com: new project → enable the Google Sheets API → IAM & Admin →
   Service Accounts → Create → Keys → Add key (JSON).
3. Save the JSON outside the repo, e.g. `~/.config/tracker/google-sheet.json`, `chmod 600`.
4. Share the spreadsheet with the key's `client_email` as an **Editor**.
5. Put `GOOGLE_SHEET_ID` and `GOOGLE_SHEET_CREDENTIALS` in `.env` (gitignored).

```bash
tracker sheet push --dry-run   # what would be sent, no network, no credentials needed
tracker sheet push
```

A service account rather than a user OAuth flow because the 09:05 timer has no browser to
complete a consent screen. Values are written with `ValueInputOption: RAW`, never
`USER_ENTERED`: job titles come from third-party ATS feeds, and a title beginning with `=`
would otherwise evaluate as a formula in a document other people open.

The push clears each tab before writing, so a role that leaves the pipeline does not linger
as a stale row. Column widths and the filter are applied only when a tab is first created —
re-imposing them daily would stamp on anything a reader adjusted.

## Gmail ingest

`mail poll` reads recent mail and keeps application state current without you
typing it. It creates an application when the confirmation arrives, closes one when
the rejection does, and — the part nothing else in the codebase did — writes
`outreach.replied_at` so a human reply stops the follow-up ladder.

**Read-only.** The scope requested is `gmail.readonly`. Nothing here sends, labels,
archives or deletes, and the token itself cannot: the guarantee is enforced at
Google's end, not just by this code. This is the same stance as INV-2 — the safest
way never to send mail the user did not read is to have no send capability.

**It queues rather than guesses.** A message that cannot be tied to exactly one
application is recorded with a reason and waits for you:

```bash
tracker mail list                        # what could not be placed, and why
tracker mail apply <gmail_id> <job_id>   # "this one" -> does what the ingest would have
tracker mail dismiss <gmail_id>          # not about an application
```

A wrongly auto-rejected application is worse than an unreviewed one: you stop
trusting the status column and start checking every row by hand, which is the job
the tool exists to remove. So `unfortunately` is deliberately *not* a rejection
phrase — it appears in scheduling mail as often as in declines.

Most application mail comes from `greenhouse.io` or `lever.co` on behalf of the
employer, so the sender domain identifies the ATS and says nothing about who is
hiring. For those the company is read out of the subject line, and an exact name
match beats a prefix one so "Stripe" does not collide with "Stripe Capital".

### Several accounts

One OAuth client covers every mailbox. Authorize each with its own label, picking
a different Google account in the browser each time:

```bash
tracker mail auth --account personal   # prints which address it landed on
tracker mail auth --account college
tracker mail accounts                  # label -> address, for when you forget
tracker mail poll                      # polls every connected account
tracker mail poll --account college    # or just one
```

Tokens are per account at `~/.config/tracker/gmail-token-<label>.json`, mode 0600.
What is authorized *is* the account list — there is no separate config to drift
from it.

The `mail_messages` key is `(account, gmail_id)`, not `gmail_id`. Gmail ids are
unique within a mailbox, **not across mailboxes**: keyed on the id alone, a message
from the second account could collide with one from the first and be silently
skipped as already processed. Databases from the single-account version are
rebuilt in place on the next run, with existing rows landing under `default`.

### Filling the sheet from history

The routine poll looks back 7 days and caps at 200 messages per account. For the
first run you want the whole history instead:

```bash
tracker mail poll --since 8760h --max 3000 --timeout 30m
tracker mail list          # whatever it could not place
tracker sheet push         # Applications tab, now populated
```

### Setup

1. Reuse the Cloud project from the sheet setup; enable the **Gmail API**.
2. OAuth consent screen → External → add **every** address you want to read as a
   test user → **Publish**. Left in "Testing", refresh tokens expire after 7 days
   and the timer breaks every week.
3. Credentials → OAuth client ID → **Desktop app** → download the JSON.
4. `GMAIL_CLIENT_SECRET=/path/to/gmail-client.json` in `.env`.

The `tracker-mail` timer runs every two hours at :15; the 08:15 run lands before
the 09:30 follow-up check so overnight replies stop the ladder in time.

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

cmd/tracker/             main; dispatches to internal/cli
internal/cli/            every command, flag parsing, table output
internal/db/             schema, migrations, excluded_log
internal/jobs/           the ingested-pipeline read model, shared by `jobs` and export
internal/followup/       the escalation ladder, run by the 09:30 timer
internal/fetch/          tolerant JD fetch, company/domain inference
internal/export/         row builders + excelize -> tracker.xlsx, status colours
internal/gsheet/         Pipeline + Applications -> a shared Google Sheet
internal/gmail/          read-only mail ingest: classify, match, review queue
internal/notify/         notify-send + optional ntfy
internal/dates/          business-day arithmetic
internal/httpx/          retries, backoff, conditional GET, per-host politeness, robots
internal/textutil/       HTML/entity coercion shared by adapters
internal/normalize/      comp_model, auth_required, hires_in_india, dedupe keys
internal/watchlist/      YAML load/save/sync, poll cadence, priority decay
internal/ingest/         idempotent upsert, cross-source dedupe, renormalize
internal/health/         poll_log, stale_feed detection, alerts
internal/poll/           concurrent poll orchestration
internal/digest/         alerts -> funded -> needs-review -> new roles
internal/ats/            detect + greenhouse/lever/ashby adapters
internal/funding/        sources, extract, resolve, run
cmd/differ/              Go-vs-Python funding extraction differ (parity harness)

tracker/, tests/,        the previous Python implementation and its 209 tests.
check_followups.py       Retained as the parity oracle; nothing live runs it.
```

### The Python tree

`tracker/`, `check_followups.py` and `tests/` are the implementation this Go build
replaced. Nothing on the live path touches them any more — all four timers and
`tracker.sh` run `bin/tracker`. They are kept deliberately: the 209 pytest cases and
`cmd/differ` are what the port was verified against, and they are the only executable
record of the intended behaviour. Deleting them is safe once you no longer want that
check; it costs nothing to keep and does not run.

## Not built yet, on purpose

Aggregator ingest, Phase 4 semantic scoring, and contact discovery. No HTML scraping of
careers pages for job content: if a company's ATS is not one of the three supported
providers it stays `unknown` and jobs get added manually, rather than growing a bespoke
scraper per company.

Out of scope entirely: auto-send, LinkedIn, a web UI, multi-user, paid data sources
(Tracxn, Crunchbase Pro), sentiment analysis, "hotness" scoring, dashboards.
