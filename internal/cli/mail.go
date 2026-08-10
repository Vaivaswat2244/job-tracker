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
  3. OAuth consent screen -> External -> add yourself as a test user, then
     PUBLISH the app. Left in "Testing" the refresh token expires after 7
     days and the timer breaks every week.
  4. Credentials -> Create credentials -> OAuth client ID -> Desktop app.
     Download the JSON.
  5. Save it privately and point .env at it:

       GMAIL_CLIENT_SECRET=/home/you/.config/tracker/gmail-client.json

  6. tracker mail auth      # opens a browser once, caches a refresh token

The scope requested is gmail.readonly. This never sends, labels, archives
or deletes anything.
`

func mailCmd(conn *sql.DB, args []string) int {
	if len(args) == 0 {
		return fail("usage: tracker mail <auth|poll|list|apply|dismiss|setup>")
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
	case "setup":
		fmt.Fprint(stdout, mailSetupHelp)
		return 0
	default:
		return fail("unknown mail subcommand %q (auth, poll, list, apply, dismiss, setup)", args[0])
	}
}

func mailAuth(args []string) int {
	fs := newFlags("mail auth")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for the browser")
	if _, ok := parse(fs, args); !ok {
		return 2
	}

	cfg, err := gmail.LoadConfig()
	if err != nil {
		fmt.Fprintf(stdout, "%v\n\n%s", err, mailSetupHelp)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	err = gmail.Authorize(ctx, cfg, func(url string) {
		fmt.Fprintf(stdout, "Opening your browser to authorize read-only Gmail access.\n"+
			"If it does not open, paste this:\n\n%s\n\n", url)
		// Best effort: a headless box just uses the printed URL.
		_ = exec.Command("xdg-open", url).Start()
	})
	if err != nil {
		return fail("%v", err)
	}
	fmt.Fprintf(stdout, "authorized; token cached at %s\n", cfg.TokenPath)
	return 0
}

func mailPoll(conn *sql.DB, args []string) int {
	fs := newFlags("mail poll")
	since := fs.Duration("since", 7*24*time.Hour, "how far back to look")
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

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	svc, err := gmail.Service(ctx, cfg)
	if err != nil {
		return fail("%v", err)
	}
	result, err := gmail.Poll(ctx, conn, svc, cfg, *since)
	if err != nil {
		return fail("%v", err)
	}

	fmt.Fprintf(stdout, "scanned %d new message(s) (%d already seen)\n", result.Scanned, result.Skipped)
	fmt.Fprintf(stdout, "  applied  %d\n  rejected %d\n  replied  %d\n  queued   %d\n",
		result.Applied, result.Rejected, result.Replied, result.Queued)
	if result.Queued > 0 {
		fmt.Fprintf(stdout, "\n%d need a decision: tracker mail list\n", result.Queued)
	}
	return 0
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
		fmt.Fprintf(stdout, "[%s] %s — %s\n", p.GmailID, day, p.Kind)
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
