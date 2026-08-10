// Package gmail reads the user's mailbox to keep application state current.
//
// Read-only, always: the scope requested is gmail.readonly, nothing here sends,
// labels, archives or deletes. INV-2 is about never sending mail the user did
// not read, and the safest way to honour that is to have no send capability at
// all — the same stance the rest of the tool takes.
//
// The classifier is deliberately conservative. An email that cannot be tied to
// exactly one application is queued for a one-key decision rather than guessed
// at: a wrongly auto-rejected application is worse than an unreviewed one,
// because the user stops trusting the status column and starts checking every
// row by hand, which is the job the tool exists to remove.
package gmail

import (
	"regexp"
	"strings"
)

// Kind is what a message appears to be about.
type Kind string

const (
	Confirmation Kind = "confirmation" // "thanks for applying"
	Rejection    Kind = "rejection"    // "moving forward with other candidates"
	Reply        Kind = "reply"        // a human answering an outreach thread
	Other        Kind = "other"        // everything else, recorded and ignored
)

// Message is the subset of a Gmail message the classifier needs.
type Message struct {
	ID         string
	ThreadID   string
	From       string // "Careers <no-reply@greenhouse.io>"
	Subject    string
	Snippet    string
	Body       string
	ReceivedAt string
}

// rejectionPhrases are the formulaic closings. They are matched against subject
// and body together.
//
// "unfortunately" is deliberately absent: it appears in scheduling mails and in
// rejections alike, and a false rejection silently ends a live application.
// Every phrase here is one that only ever appears in a decline.
var rejectionPhrases = []*regexp.Regexp{
	regexp.MustCompile(`(?i)move forward with other candidate`),
	regexp.MustCompile(`(?i)moving forward with other candidate`),
	regexp.MustCompile(`(?i)decided (?:to|not to) (?:move|proceed|progress)[^.]{0,40}(?:other candidate|with your application)`),
	regexp.MustCompile(`(?i)not (?:be )?(?:moving|progressing|proceeding) (?:forward|ahead)[^.]{0,30}(?:your application|at this time|with your candidacy)`),
	regexp.MustCompile(`(?i)regret to inform`),
	regexp.MustCompile(`(?i)will not be (?:moving forward|progressing)`),
	regexp.MustCompile(`(?i)your application[^.]{0,40}(?:was not successful|has been unsuccessful)`),
	regexp.MustCompile(`(?i)we (?:have )?(?:decided )?(?:to )?pursue other candidate`),
	regexp.MustCompile(`(?i)not (?:been )?selected (?:for|to)`),
}

var confirmationPhrases = []*regexp.Regexp{
	regexp.MustCompile(`(?i)thank(?:s| you) for (?:applying|your application|your interest)`),
	regexp.MustCompile(`(?i)we(?:'ve| have)? received your application`),
	regexp.MustCompile(`(?i)your application (?:has been|was) received`),
	regexp.MustCompile(`(?i)application (?:received|submitted|confirmation)`),
	regexp.MustCompile(`(?i)we(?:'ve| have)? got your application`),
	regexp.MustCompile(`(?i)successfully (?:applied|submitted your application)`),
}

// atsSenders send on behalf of a company, so their domain identifies the ATS,
// never the employer. The company has to come out of the subject line instead.
var atsSenders = []string{
	"greenhouse.io", "greenhouse-mail.io", "us.greenhouse-mail.io",
	"hire.lever.co", "lever.co", "ashbyhq.com", "ashby.hq",
	"myworkday.com", "workday.com", "icims.com", "smartrecruiters.com",
	"successfactors.com", "taleo.net", "jobvite.com", "breezy.hr",
	"recruitee.com", "workable.com", "teamtailor.com", "hire.withgoogle.com",
}

// automatedLocalParts mark a machine sender. A reply from one of these is not a
// human answering, so it must not stop the follow-up ladder.
var automatedLocalParts = []string{
	"no-reply", "noreply", "donotreply", "do-not-reply", "notifications",
	"notification", "mailer", "automated", "auto-reply", "bounce", "postmaster",
}

// subjectCompany pulls the employer out of the phrasings ATS mail actually uses.
// Ordered most specific first; the first match wins.
var subjectCompany = []*regexp.Regexp{
	regexp.MustCompile(`(?i)your application (?:to|at|for) ([A-Z0-9][\w&.' -]{1,40}?)(?:\s*[-–—|,:(]|$)`),
	regexp.MustCompile(`(?i)thank(?:s| you) for (?:applying|your interest) (?:to|at|in) ([A-Z0-9][\w&.' -]{1,40}?)(?:\s*[-–—|,:(]|$)`),
	regexp.MustCompile(`(?i)applying (?:to|at) ([A-Z0-9][\w&.' -]{1,40}?)(?:\s*[-–—|,:(]|$)`),
	regexp.MustCompile(`(?i)application (?:to|at|for) ([A-Z0-9][\w&.' -]{1,40}?)(?:\s*[-–—|,:(]|$)`),
	regexp.MustCompile(`(?i)^([A-Z0-9][\w&.' -]{1,40}?)\s*[-–—|:]\s*(?:application|thank)`),
}

// Classify decides what a message is. It never reads state, so it can be
// tested on its own and reasoned about without a database.
func Classify(m Message) Kind {
	haystack := m.Subject + "\n" + m.Snippet + "\n" + m.Body

	// Rejection is checked first: a decline often quotes the original
	// "thank you for applying" underneath it, and the later state wins.
	for _, re := range rejectionPhrases {
		if re.MatchString(haystack) {
			return Rejection
		}
	}
	for _, re := range confirmationPhrases {
		if re.MatchString(haystack) {
			return Confirmation
		}
	}
	if !IsAutomated(m.From) && strings.HasPrefix(strings.ToLower(strings.TrimSpace(m.Subject)), "re:") {
		return Reply
	}
	return Other
}

// IsAutomated reports whether the address is a machine sender.
func IsAutomated(from string) bool {
	addr := strings.ToLower(Address(from))
	local, _, ok := strings.Cut(addr, "@")
	if !ok {
		return true // unparseable: treat as automated, never as a human reply
	}
	for _, marker := range automatedLocalParts {
		if strings.Contains(local, marker) {
			return true
		}
	}
	return false
}

// Address extracts the bare address from a From header.
func Address(from string) string {
	from = strings.TrimSpace(from)
	if i := strings.LastIndex(from, "<"); i >= 0 {
		if j := strings.Index(from[i:], ">"); j > 0 {
			return strings.TrimSpace(from[i+1 : i+j])
		}
	}
	return from
}

// Domain returns the sender's domain, lowercased.
func Domain(from string) string {
	_, domain, ok := strings.Cut(Address(from), "@")
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(domain))
}

// IsATS reports whether the sender is a recruiting platform rather than the
// employer, in which case the domain says nothing about which company wrote.
func IsATS(from string) bool {
	domain := Domain(from)
	for _, s := range atsSenders {
		if domain == s || strings.HasSuffix(domain, "."+s) {
			return true
		}
	}
	return false
}

// CompanyFromSubject extracts the employer named in a subject line, or "".
func CompanyFromSubject(subject string) string {
	subject = strings.TrimSpace(subject)
	// Strip a leading Re:/Fwd: so the anchored patterns still match.
	for {
		lower := strings.ToLower(subject)
		if strings.HasPrefix(lower, "re:") || strings.HasPrefix(lower, "fw:") {
			subject = strings.TrimSpace(subject[3:])
			continue
		}
		if strings.HasPrefix(lower, "fwd:") {
			subject = strings.TrimSpace(subject[4:])
			continue
		}
		break
	}
	for _, re := range subjectCompany {
		if m := re.FindStringSubmatch(subject); m != nil {
			if name := cleanCompany(m[1]); name != "" {
				return name
			}
		}
	}
	return ""
}

// stopWords are words that follow "application to" without naming a company.
var stopWords = map[string]bool{
	"the": true, "our": true, "us": true, "your": true, "this": true,
	"a": true, "an": true, "join": true, "work": true, "be": true,
}

func cleanCompany(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "-–—|,:;.!\"' ")

	// Drop a trailing legal suffix so "Stripe, Inc" matches "Stripe". Only
	// genuine legal forms: "Labs" and "Technologies" are part of the real name
	// for Grafana Labs and Cockroach Labs, and trimming them would stop those
	// matching the company on file.
	for _, suffix := range []string{" inc", " inc.", " llc", " ltd", " limited",
		" corp", " corporation", " pvt", " pvt.", " private limited"} {
		if strings.HasSuffix(strings.ToLower(s), suffix) {
			s = strings.TrimSpace(s[:len(s)-len(suffix)])
		}
	}
	if len(s) < 2 {
		return ""
	}
	// "applying to the team" and friends: the pattern matched, but what it
	// caught is a sentence, not an employer.
	first, _, _ := strings.Cut(strings.ToLower(s), " ")
	if stopWords[first] {
		return ""
	}
	return s
}
