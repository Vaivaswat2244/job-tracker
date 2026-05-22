[x] 1.1

Create Google Cloud project

Enable Gmail API and Google Gemini API. Both are free. Get your credentials JSON file.

[x] 1.2

Set up OAuth 2.0

Configure consent screen, add Gmail readonly scope. This lets your app read the user's inbox without storing their password.

oauth2 · gmail.readonly scope

[x] 1.3

Bootstrap project structure

Python or Node.js backend. Create folders: /agent, /db, /api, /frontend. Install google-api-python-client and google-generativeai packages.

Python recommended


[x] 2.1

Gmail polling service

Use Gmail API's messages.list with a lastChecked timestamp stored locally. Poll every 15–30 min via a cron job or simple scheduler.

schedule · cron

[x] 2.2

Email parser

Extract plain text from email body (strip HTML). Handle multipart MIME messages. Store raw email text + subject + sender + date.

email.parser · beautifulsoup

2.3

Thread grouping

Use Gmail's threadId to link follow-up emails to the same application. One job = one thread group, not separate emails.

threadId · deduplication

Write the Gmail polling code ↗

3.1

Classification prompt

Send email subject + first 300 chars to Gemini Flash. Ask: "Is this job-related? Reply YES or NO." Filter out newsletters, personal mail, etc.

gemini-2.0-flash · free tier

3.2

Extraction prompt

For job-related emails, extract: company name, role title, application date, status, next steps, interview date if any. Ask Gemini to return structured JSON.

structured JSON output

3.3

Status classifier

Map extracted signals to pipeline stages: Applied / Screening / Interview Scheduled / Offer / Rejected / Ghosted. Gemini handles ambiguous language well here.

5-state pipeline

3.4

Prompt testing in AI Studio

Before wiring up, test your prompts on 10–15 real job emails in Google AI Studio. Refine until extraction accuracy is ~90%+.

Write the extraction prompt ↗

4.1

SQLite schema

Three tables: jobs (id, company, role, status, applied_date, last_updated), emails (id, job_id, gmail_thread_id, subject, received_at), and events (id, job_id, type, date, notes).

SQLite · no server needed

4.2

Upsert logic

When a new email links to an existing thread, update the job status rather than creating a duplicate. Match on threadId first, then fallback to company+role name fuzzy match.

upsert · fuzzy match

4.3

Follow-up scheduler

For jobs in "Applied" or "Interviewed" status with no update in 7 days, flag them as needing a follow-up. Store next_followup_date in the jobs table.

nudge logic

Write the DB schema ↗

5.1

Simple backend API

FastAPI or Flask with 3 endpoints: GET /jobs (list all), GET /jobs/:id (detail + email thread), PATCH /jobs/:id (manual status override).

FastAPI · 3 routes

5.2

Kanban or table view

Simple HTML/JS frontend. Columns: Applied → Screening → Interview → Offer → Rejected. Cards show company, role, days since last update, and a follow-up badge if overdue.

vanilla JS or React

5.3

Manual add + edit

Not all applications are via email (some are verbal). Add a simple form to manually add a job and override AI-extracted fields when wrong.

manual override

Build the dashboard UI ↗

6.1

ATS email pattern handling

Greenhouse, Lever, Workday all format emails differently. Build specific parsers or prompts for common ATS systems you encounter.

Greenhouse · Lever · Workday

6.2

Weekly digest

Every Monday, have Gemini summarise your job search: X applications active, Y interviews scheduled, Z rejections this week. Send to yourself via Gmail API.

weekly summary email

6.3

Error handling & logging

Gmail API has rate limits. Add exponential backoff, log failed extractions, and let users flag emails that were miscategorised so you can improve prompts.

rate limits · logging
