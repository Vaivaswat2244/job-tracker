package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/Vaivaswat2244/job-tracker/internal/gmail"
)

const mailSetupHelp = `Connect Gmail once (read-only):

  1. Reuse the Google Cloud project from the sheet setup, or make a new one.
  2. Enable the Gmail API:
       console.cloud.google.com/apis/library/gmail.googleapis.com
  3. OAuth consent screen -> External -> add EVERY address you want to read as
     a test user, then PUBLISH the app. Left in "Testing" the refresh token
     expires after 7 days and the timer breaks every week.
  4. Credentials -> Create credentials -> OAuth client ID -> Desktop app.
     Download the JSON. One client covers every account.
  5. Save it privately and point .env at it:

       GMAIL_CLIENT_SECRET=/home/you/.config/tracker/gmail-client.json

  6. Authorize each mailbox, picking a different Google account in the browser:

       tracker mail auth --account personal
       tracker mail auth --account college

The scope requested is gmail.readonly. This never sends, labels, archives
or deletes anything.
`

func mailCmd(conn *sql.DB, args []string) int {
	if len(args) == 0 {
		return fail("usage: tracker mail <auth|poll|list|apply|dismiss|accounts|setup>")
	}
	switch args[0] {
	case "auth":
		return mailAuth(args[1:])
	case "poll":
		return mailPoll(conn, args[1:])
	case "list":
		return mailList(conn, args[1:])
	case "apply":
		return mailApply(conn, args[1:], false)
	case "dismiss":
		return mailApply(conn, args[1:], true)
	case "accounts":
		return mailAccounts(args[1:])
	case "setup":
		fmt.Fprint(stdout, mailSetupHelp)
		return 0
	default:
		return fail("unknown mail subcommand %q "+
			"(auth, poll, list, apply, dismiss, accounts, setup)", args[0])
	}
}

func mailAuth(args []string) int {
	fs := newFlags("mail auth")
	account := fs.String("account", gmail.DefaultAccount,
		"label for this mailbox, e.g. personal or college")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for the browser")
	if _, ok := parse(fs, args); !ok {
		return 2
	}
	if err := gmail.ValidateAccount(*account); err != nil {
		return fail("%v", err)
	}

	cfg, err := gmail.LoadConfig()
	if err != nil {
		fmt.Fprintf(stdout, "%v\n\n%s", err, mailSetupHelp)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	err = gmail.Authorize(ctx, cfg, *account, func(url string) {
		fmt.Fprintf(stdout,
			"Authorizing read-only Gmail access for account %q.\n"+
				"Pick the Google account you want THIS label to point at.\n"+
				"If the browser does not open, paste this:\n\n%s\n\n", *account, url)
		// Best effort: a headless box just uses the printed URL.
		_ = exec.Command("xdg-open", url).Start()
	})
	if err != nil {
		return fail("%v", err)
	}

	// Confirm which mailbox the label actually landed on — a label called
	// "personal" pointing at the college address is painful to debug later.
	if svc, err := gmail.Service(ctx, cfg, *account); err == nil {
		if addr := gmail.WhoAmI(ctx, svc); addr != "" {
			fmt.Fprintf(stdout, "authorized %s -> %s\n", *account, addr)
			return 0
		}
	}
	fmt.Fprintf(stdout, "authorized %s; token cached at %s\n", *account, cfg.TokenPath(*account))
	return 0
}

func mailAccounts(args []string) int {
	fs := newFlags("mail accounts")
	if _, ok := parse(fs, args); !ok {
		return 2
	}
	cfg, err := gmail.LoadConfig()
	if err != nil {
		fmt.Fprintf(stdout, "%v\n\n%s", err, mailSetupHelp)
		if errors.Is(err, gmail.ErrNotConfigured) {
			return 0
		}
		return 1
	}
	accounts, err := cfg.Accounts()
	if err != nil {
		return fail("%v", err)
	}
	if len(accounts) == 0 {
		fmt.Fprintln(stdout, "no mailbox connected yet — tracker mail auth --account personal")
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, name := range accounts {
		addr := "(could not reach Gmail)"
		if svc, err := gmail.Service(ctx, cfg, name); err == nil {
			if got := gmail.WhoAmI(ctx, svc); got != "" {
				addr = got
			}
		}
		fmt.Fprintf(stdout, "%-14s %s\n", name, addr)
	}
	return 0
}

func mailPoll(conn *sql.DB, args []string) int {
	fs := newFlags("mail poll")
	since := fs.Duration("since", 7*24*time.Hour, "how far back to look")
	max := fs.Int("max", gmail.DefaultMaxMessages, "cap on messages scanned per account")
	only := fs.String("account", "", "poll just this account (default: all connected)")
	timeout := fs.Duration("timeout", 3*time.Minute, "give up after this long")
	if _, ok := parse(fs, args); !ok {
		return 2
	}

	cfg, err := gmail.LoadConfig()
	if err != nil {
		fmt.Fprintf(stdout, "%v\n\n%s", err, mailSetupHelp)
		// Never configured is a choice; the timer should not turn red over it.
		if errors.Is(err, gmail.ErrNotConfigured) {
			return 0
		}
		return 1
	}

	accounts, err := cfg.Accounts()
	if err != nil {
		return fail("%v", err)
	}
	if *only != "" {
		if err := gmail.ValidateAccount(*only); err != nil {
			return fail("%v", err)
		}
		accounts = []string{*only}
	}
	if len(accounts) == 0 {
		fmt.Fprintln(stdout, "no mailbox connected yet — tracker mail auth --account personal")
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var total gmail.Result
	var failed int
	for _, name := range accounts {
		svc, err := gmail.Service(ctx, cfg, name)
		if err != nil {
			// One broken mailbox must not stop the other from being read.
			fmt.Fprintf(stdout, "%s: %v\n", name, err)
			failed++
			continue
		}
		acct := gmail.Account{Name: name, Email: gmail.WhoAmI(ctx, svc)}

		result, err := gmail.Poll(ctx, conn, svc, cfg, acct, *since, *max)
		if err != nil {
			fmt.Fprintf(stdout, "%s: %v\n", name, err)
			failed++
			continue
		}
		fmt.Fprintf(stdout, "%s (%s): scanned %d, %d already seen — "+
			"applied %d, rejected %d, replied %d, queued %d\n",
			name, orDash(acct.Email), result.Scanned, result.Skipped,
			result.Applied, result.Rejected, result.Replied, result.Queued)

		total.Scanned += result.Scanned
		total.Skipped += result.Skipped
		total.Applied += result.Applied
		total.Rejected += result.Rejected
		total.Replied += result.Replied
		total.Queued += result.Queued
	}

	if len(accounts) > 1 {
		fmt.Fprintf(stdout, "\ntotal: applied %d, rejected %d, replied %d, queued %d\n",
			total.Applied, total.Rejected, total.Replied, total.Queued)
	}
	if total.Queued > 0 {
		fmt.Fprintf(stdout, "\n%d need a decision: tracker mail list\n", total.Queued)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func mailList(conn *sql.DB, args []string) int {
	fs := newFlags("mail list")
	if _, ok := parse(fs, args); !ok {
		return 2
	}

	pending, err := gmail.PendingList(conn)
	if err != nil {
		return fail("%v", err)
	}
	if len(pending) == 0 {
		fmt.Fprintln(stdout, "nothing waiting on a decision.")
		return 0
	}

	for _, p := range pending {
		day := p.ReceivedAt
		if len(day) >= 10 {
			day = day[:10]
		}
		mailbox := p.Account
		if p.AccountEmail.Valid && p.AccountEmail.String != "" {
			mailbox = p.Account + " / " + p.AccountEmail.String
		}
		fmt.Fprintf(stdout, "[%s] %s — %s — %s\n", p.GmailID, day, p.Kind, mailbox)
		fmt.Fprintf(stdout, "      %s\n", trunc(p.Subject, 96))
		fmt.Fprintf(stdout, "      from %s\n", p.From)
		if p.CompanyGuess.Valid {
			fmt.Fprintf(stdout, "      reads as: %s\n", p.CompanyGuess.String)
		}
		if p.Reason.Valid {
			fmt.Fprintf(stdout, "      %s\n", p.Reason.String)
		}
		fmt.Fprintln(stdout)
	}
	fmt.Fprintf(stdout, "%d waiting. `tracker mail apply <gmail_id> <job_id>` or "+
		"`tracker mail dismiss <gmail_id>`.\n", len(pending))
	return 0
}

func mailApply(conn *sql.DB, args []string, dismiss bool) int {
	name := "mail apply"
	if dismiss {
		name = "mail dismiss"
	}
	fs := newFlags(name)
	pos, ok := parse(fs, args)
	if !ok {
		return 2
	}

	want := 2
	if dismiss {
		want = 1
	}
	if len(pos) != want {
		if dismiss {
			return fail("usage: tracker mail dismiss <gmail_id>")
		}
		return fail("usage: tracker mail apply <gmail_id> <job_id>")
	}

	var jobID int64
	if !dismiss {
		var err error
		jobID, err = atoi64(pos[1], "job_id")
		if err != nil {
			return fail("%v", err)
		}
	}

	action, err := gmail.Resolve(conn, pos[0], jobID, time.Now().UTC())
	if err != nil {
		return fail("%v", err)
	}
	if dismiss {
		fmt.Fprintf(stdout, "dismissed %s\n", pos[0])
		return 0
	}
	fmt.Fprintf(stdout, "%s -> job %d: %s\n", pos[0], jobID, action)
	return 0
}
