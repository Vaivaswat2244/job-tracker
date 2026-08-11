package gmail

import (
	"os"
	"path/filepath"
	"testing"
)

var college = Account{Name: "college", Email: "me@college.edu"}

// The reason the primary key is (account, gmail_id): Gmail ids are unique
// within a mailbox, not across mailboxes. Keyed on the id alone, the second
// account's message would be swallowed as "already processed" and never acted
// on — a silently missed application.
func TestSameIdInTwoAccountsIsTwoMessages(t *testing.T) {
	conn := seed(t)

	msg := Message{
		ID: "shared-id", From: "no-reply@greenhouse.io",
		Subject: "Thank you for applying to Stripe", Body: "Received.",
	}
	if _, err := Ingest(conn, personal, msg, now); err != nil {
		t.Fatalf("personal ingest: %v", err)
	}
	action, err := Ingest(conn, college, msg, now)
	if err != nil {
		t.Fatalf("college ingest: %v", err)
	}
	if AlreadySeen(err) || action == ActionNone {
		t.Fatal("the second account's message was treated as already processed")
	}

	var n int
	if err := conn.QueryRow(
		"SELECT count(*) FROM mail_messages WHERE gmail_id = 'shared-id'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("%d rows for the shared id, want one per account", n)
	}
}

// Re-polling one account must still skip its own messages.
func TestIdempotencyIsPerAccount(t *testing.T) {
	conn := seed(t)
	msg := Message{
		ID: "m1", From: "no-reply@greenhouse.io",
		Subject: "Thank you for applying to Stripe", Body: "Received.",
	}
	if _, err := Ingest(conn, personal, msg, now); err != nil {
		t.Fatal(err)
	}
	if _, err := Ingest(conn, personal, msg, now); !AlreadySeen(err) {
		t.Errorf("re-ingesting the same account's message returned %v", err)
	}
}

// Thread ids are per-mailbox too: a reply in one account must not claim an
// application that the other account's thread matched.
func TestThreadLookupIsScopedToAccount(t *testing.T) {
	conn := seed(t)
	if _, err := Ingest(conn, personal, Message{
		ID: "m1", ThreadID: "t1", From: "no-reply@greenhouse.io",
		Subject: "Thank you for applying to Stripe", Body: "Received.",
	}, now); err != nil {
		t.Fatal(err)
	}

	if _, ok := ResolveThread(conn, college.Name, "t1"); ok {
		t.Error("a thread from the personal account resolved under the college account")
	}
	if _, ok := ResolveThread(conn, personal.Name, "t1"); !ok {
		t.Error("the thread did not resolve under its own account")
	}
}

// Resolving a queued message must not also resolve a colliding id queued in
// the other mailbox.
func TestResolveTouchesOnlyItsOwnAccount(t *testing.T) {
	conn := seed(t)
	ambiguous := Message{
		ID: "dupe", From: "no-reply@hire.lever.co",
		Subject: "Thank you for applying to Groww", Body: "Received.",
	}
	for _, acct := range []Account{personal, college} {
		if _, err := Ingest(conn, acct, ambiguous, now); err != nil {
			t.Fatalf("%s: %v", acct.Name, err)
		}
	}
	if pending, _ := PendingList(conn); len(pending) != 2 {
		t.Fatalf("%d queued, want one per account", len(pending))
	}

	if _, err := Resolve(conn, "dupe", 21, now); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	pending, _ := PendingList(conn)
	if len(pending) != 1 {
		t.Errorf("%d still queued after resolving one account's copy, want 1", len(pending))
	}
}

func TestValidateAccount(t *testing.T) {
	for _, ok := range []string{"personal", "college", "work-2", "a_b", "x"} {
		if err := ValidateAccount(ok); err != nil {
			t.Errorf("ValidateAccount(%q) = %v, want nil", ok, err)
		}
	}
	// A label becomes a filename, so traversal and separators must be refused.
	for _, bad := range []string{"", "../../etc/passwd", "with space", "UPPER",
		"trailing/", "a/b", "-leading", "..'"} {
		if err := ValidateAccount(bad); err == nil {
			t.Errorf("ValidateAccount(%q) = nil, want an error", bad)
		}
	}
}

func TestAccountsListsAuthorizedMailboxes(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{TokenDir: dir}

	if got, err := cfg.Accounts(); err != nil || len(got) != 0 {
		t.Fatalf("Accounts() on an empty dir = %v, %v", got, err)
	}
	for _, name := range []string{"personal", "college"} {
		if err := os.WriteFile(cfg.TokenPath(name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Unrelated files in the same directory must not read as accounts.
	for _, junk := range []string{"gmail-client.json", "google-sheet.json", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, junk), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := cfg.Accounts()
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(got) != 2 || got[0] != "college" || got[1] != "personal" {
		t.Errorf("Accounts() = %v, want [college personal]", got)
	}
}

// The token is read access to a personal mailbox; it must not be group- or
// world-readable.
func TestTokenIsWrittenPrivately(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{TokenDir: filepath.Join(dir, "nested")}
	if err := saveToken(cfg.TokenPath("personal"), nil); err != nil {
		t.Fatalf("saveToken: %v", err)
	}
	info, err := os.Stat(cfg.TokenPath("personal"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token mode = %o, want 600", perm)
	}
}
