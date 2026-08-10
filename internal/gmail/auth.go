package gmail

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

const (
	// ClientEnv points at the OAuth client JSON downloaded from Google Cloud.
	ClientEnv = "GMAIL_CLIENT_SECRET"
	// TokenEnv overrides where the refresh token is cached.
	TokenEnv = "GMAIL_TOKEN"
	// QueryEnv overrides the Gmail search the poll runs.
	QueryEnv = "GMAIL_QUERY"
)

// DefaultQuery is what the poll searches for. Narrow on purpose: this reads a
// personal mailbox, and there is no reason to pull messages that cannot
// possibly be about an application.
const DefaultQuery = `(subject:(application OR applying OR candidacy OR interview) ` +
	`OR from:(greenhouse.io OR lever.co OR ashbyhq.com OR myworkday.com OR smartrecruiters.com)) ` +
	`-in:spam -in:trash`

// ErrNotConfigured means Gmail ingest was never set up, as distinct from being
// set up wrongly — the timer exits quietly on the former, loudly on the latter.
var ErrNotConfigured = fmt.Errorf("gmail ingest not configured (%s unset)", ClientEnv)

// Config locates the OAuth client and the cached token.
type Config struct {
	ClientPath string
	TokenPath  string
	Query      string
}

// LoadConfig reads .env then the environment.
func LoadConfig() (Config, error) {
	_ = godotenv.Load(".env")

	cfg := Config{
		ClientPath: strings.TrimSpace(os.Getenv(ClientEnv)),
		TokenPath:  strings.TrimSpace(os.Getenv(TokenEnv)),
		Query:      strings.TrimSpace(os.Getenv(QueryEnv)),
	}
	if cfg.Query == "" {
		cfg.Query = DefaultQuery
	}
	if cfg.TokenPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return cfg, fmt.Errorf("locate home directory: %w", err)
		}
		cfg.TokenPath = filepath.Join(home, ".config", "tracker", "gmail-token.json")
	}
	if cfg.ClientPath == "" {
		return cfg, ErrNotConfigured
	}
	if _, err := os.Stat(cfg.ClientPath); err != nil {
		return cfg, fmt.Errorf("oauth client %s: %w", cfg.ClientPath, err)
	}
	return cfg, nil
}

// oauthConfig builds the OAuth config with the read-only scope.
//
// gmail.GmailReadonlyScope and nothing else. A narrower token cannot send,
// label, archive or delete however wrong the rest of this package might be, so
// the guarantee holds at Google's end rather than only in our code.
func oauthConfig(cfg Config, redirect string) (*oauth2.Config, error) {
	raw, err := os.ReadFile(cfg.ClientPath)
	if err != nil {
		return nil, fmt.Errorf("read oauth client: %w", err)
	}
	oc, err := google.ConfigFromJSON(raw, gmail.GmailReadonlyScope)
	if err != nil {
		return nil, fmt.Errorf("parse oauth client (is it a Desktop app client?): %w", err)
	}
	if redirect != "" {
		oc.RedirectURL = redirect
	}
	return oc, nil
}

// Authorize runs the one-time consent flow and caches the refresh token.
//
// It listens on loopback rather than pasting a code: Google retired the
// out-of-band copy-paste flow, and a desktop client is expected to receive the
// redirect on 127.0.0.1.
func Authorize(ctx context.Context, cfg Config, open func(string)) error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("open loopback listener: %w", err)
	}
	defer listener.Close()

	redirect := fmt.Sprintf("http://127.0.0.1:%d", listener.Addr().(*net.TCPAddr).Port)
	oc, err := oauthConfig(cfg, redirect)
	if err != nil {
		return err
	}

	state := fmt.Sprintf("tracker-%d", time.Now().UnixNano())
	codes := make(chan string, 1)
	errs := make(chan error, 1)

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errs <- fmt.Errorf("oauth state mismatch — the callback did not come from this run")
			return
		}
		if e := r.URL.Query().Get("error"); e != "" {
			http.Error(w, "authorization denied", http.StatusBadRequest)
			errs <- fmt.Errorf("authorization denied: %s", e)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "no code", http.StatusBadRequest)
			errs <- fmt.Errorf("callback carried no authorization code")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><meta charset="utf-8">`+
			`<body style="font-family:system-ui;padding:3rem">`+
			`<h2>Connected.</h2><p>You can close this tab and go back to the terminal.</p>`)
		codes <- code
	})}
	go srv.Serve(listener)
	defer srv.Close()

	// AccessTypeOffline plus prompt=consent is what makes Google hand back a
	// refresh token; without it the timer would need a browser every hour.
	url := oc.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"))
	open(url)

	var code string
	select {
	case code = <-codes:
	case err := <-errs:
		return err
	case <-ctx.Done():
		return fmt.Errorf("timed out waiting for the browser callback")
	}

	token, err := oc.Exchange(ctx, code)
	if err != nil {
		return fmt.Errorf("exchange code: %w", err)
	}
	if token.RefreshToken == "" {
		return fmt.Errorf("google returned no refresh token; revoke the app at " +
			"myaccount.google.com/permissions and run auth again")
	}
	return saveToken(cfg.TokenPath, token)
}

func saveToken(path string, token *oauth2.Token) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create token directory: %w", err)
	}
	raw, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return fmt.Errorf("encode token: %w", err)
	}
	// 0600: this token reads the user's mail. It is as sensitive as a password.
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("write token: %w", err)
	}
	return nil
}

func loadToken(path string) (*oauth2.Token, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var token oauth2.Token
	if err := json.Unmarshal(raw, &token); err != nil {
		return nil, fmt.Errorf("parse token %s: %w", path, err)
	}
	return &token, nil
}

// Service builds an authenticated Gmail client from the cached token,
// refreshing it and writing the refreshed copy back.
func Service(ctx context.Context, cfg Config) (*gmail.Service, error) {
	oc, err := oauthConfig(cfg, "")
	if err != nil {
		return nil, err
	}
	token, err := loadToken(cfg.TokenPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("not authorized yet — run `tracker mail auth`")
		}
		return nil, err
	}

	source := oc.TokenSource(ctx, token)
	refreshed, err := source.Token()
	if err != nil {
		return nil, fmt.Errorf("refresh token (re-run `tracker mail auth` if this persists): %w", err)
	}
	if refreshed.AccessToken != token.AccessToken {
		if refreshed.RefreshToken == "" {
			refreshed.RefreshToken = token.RefreshToken
		}
		if err := saveToken(cfg.TokenPath, refreshed); err != nil {
			return nil, err
		}
	}

	svc, err := gmail.NewService(ctx, option.WithTokenSource(oauth2.ReuseTokenSource(refreshed, source)))
	if err != nil {
		return nil, fmt.Errorf("gmail client: %w", err)
	}
	return svc, nil
}
