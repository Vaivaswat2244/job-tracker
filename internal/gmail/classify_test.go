package gmail

import "testing"

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  Message
		want Kind
	}{
		{"greenhouse confirmation", Message{
			From:    "Stripe <no-reply@greenhouse.io>",
			Subject: "Thank you for applying to Stripe",
			Body:    "We have received your application and will be in touch.",
		}, Confirmation},
		{"lever confirmation", Message{
			From:    "Meesho <no-reply@hire.lever.co>",
			Subject: "Your application to Meesho",
			Body:    "Thanks for your interest in Meesho.",
		}, Confirmation},
		{"plain rejection", Message{
			From:    "recruiting@example.com",
			Subject: "Update on your application",
			Body:    "We have decided to move forward with other candidates.",
		}, Rejection},
		{"regret rejection", Message{
			From:    "careers@example.com",
			Subject: "Your application",
			Body:    "We regret to inform you that we will not be progressing.",
		}, Rejection},
		{"not selected", Message{
			From:    "careers@example.com",
			Subject: "Application update",
			Body:    "You have not been selected for the next round.",
		}, Rejection},
		{"human reply", Message{
			From:    "priya@stripe.com",
			Subject: "Re: Backend Engineer role",
			Body:    "Happy to chat next week.",
		}, Reply},
		{"newsletter", Message{
			From:    "news@techcrunch.com",
			Subject: "Your daily briefing",
			Body:    "Top stories today.",
		}, Other},
	} {
		if got := Classify(tc.msg); got != tc.want {
			t.Errorf("%s: Classify = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A decline usually quotes the original confirmation underneath it. Reading the
// quoted text first would leave a dead application marked live.
func TestRejectionBeatsQuotedConfirmation(t *testing.T) {
	msg := Message{
		From:    "no-reply@greenhouse.io",
		Subject: "Update on your application to Stripe",
		Body: "We have decided to move forward with other candidates.\n\n" +
			"> Thank you for applying to Stripe\n> We have received your application.",
	}
	if got := Classify(msg); got != Rejection {
		t.Errorf("Classify = %q, want rejection despite the quoted confirmation", got)
	}
}

// "Unfortunately" appears in scheduling mail as often as in declines. Treating
// it as a rejection would silently close a live application.
func TestUnfortunateSchedulingIsNotARejection(t *testing.T) {
	msg := Message{
		From:    "priya@stripe.com",
		Subject: "Interview scheduling",
		Body:    "Unfortunately I am travelling Thursday — could we do Friday instead?",
	}
	if got := Classify(msg); got == Rejection {
		t.Error("a scheduling email was classified as a rejection")
	}
}

// An automated "Re:" is not a human answering, so it must not stop the ladder.
func TestAutomatedReplyIsNotAReply(t *testing.T) {
	msg := Message{
		From:    "no-reply@greenhouse.io",
		Subject: "Re: your application",
		Body:    "This mailbox is not monitored.",
	}
	if got := Classify(msg); got == Reply {
		t.Error("an automated message was classified as a human reply")
	}
}

func TestIsAutomated(t *testing.T) {
	for _, tc := range []struct {
		from string
		want bool
	}{
		{"no-reply@greenhouse.io", true},
		{"noreply@lever.co", true},
		{"donotreply@example.com", true},
		{"notifications@ashbyhq.com", true},
		{"Priya <priya@stripe.com>", false},
		{"recruiting@example.com", false},
		{"garbage-without-an-at-sign", true},
	} {
		if got := IsAutomated(tc.from); got != tc.want {
			t.Errorf("IsAutomated(%q) = %v, want %v", tc.from, got, tc.want)
		}
	}
}

func TestAddressAndDomain(t *testing.T) {
	for _, tc := range []struct{ from, addr, domain string }{
		{"Stripe Careers <no-reply@greenhouse.io>", "no-reply@greenhouse.io", "greenhouse.io"},
		{"plain@example.com", "plain@example.com", "example.com"},
		{"Name <UPPER@Example.COM>", "UPPER@Example.COM", "example.com"},
	} {
		if got := Address(tc.from); got != tc.addr {
			t.Errorf("Address(%q) = %q, want %q", tc.from, got, tc.addr)
		}
		if got := Domain(tc.from); got != tc.domain {
			t.Errorf("Domain(%q) = %q, want %q", tc.from, got, tc.domain)
		}
	}
}

// The common case: mail arrives from the ATS, not the employer, so the sender
// domain identifies Greenhouse and says nothing about who is hiring.
func TestIsATS(t *testing.T) {
	for _, tc := range []struct {
		from string
		want bool
	}{
		{"no-reply@greenhouse.io", true},
		{"x@us.greenhouse-mail.io", true},
		{"x@hire.lever.co", true},
		{"x@ashbyhq.com", true},
		{"x@myworkday.com", true},
		{"priya@stripe.com", false},
		{"careers@razorpay.com", false},
	} {
		if got := IsATS(tc.from); got != tc.want {
			t.Errorf("IsATS(%q) = %v, want %v", tc.from, got, tc.want)
		}
	}
}

func TestCompanyFromSubject(t *testing.T) {
	for _, tc := range []struct{ subject, want string }{
		{"Thank you for applying to Stripe", "Stripe"},
		{"Your application to Razorpay", "Razorpay"},
		// "Labs" is part of the name, not a legal suffix to trim.
		{"Thanks for your interest in Cockroach Labs", "Cockroach Labs"},
		{"Thank you for applying to Grafana Labs", "Grafana Labs"},
		{"Your application at Meesho - Backend Engineer", "Meesho"},
		{"Re: Your application to Stripe", "Stripe"},
		{"Application to Stripe, Inc", "Stripe"},
		{"Groww | Application received", "Groww"},
		{"Your daily briefing", ""},
		{"Thank you for applying to the team", ""},
	} {
		if got := CompanyFromSubject(tc.subject); got != tc.want {
			t.Errorf("CompanyFromSubject(%q) = %q, want %q", tc.subject, got, tc.want)
		}
	}
}
