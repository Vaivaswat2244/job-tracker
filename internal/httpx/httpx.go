// Package httpx is the one HTTP path for every poller: retries, conditional
// GETs, per-host politeness.
//
// Nothing here returns a transport error to the caller. A poll that fails must
// return a Fetch describing the failure so poll health can count it — an error
// return would lose the signal that a feed died.
package httpx

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	Timeout     = 15 * time.Second
	Attempts    = 3
	BackoffBase = time.Second // doubled per attempt, plus jitter
	HostDelay   = time.Second // minimum gap between requests to the same host

	Version        = "1.0"
	contactEnv     = "TRACKER_CONTACT_EMAIL"
	defaultContact = "vaivaswat2244@gmail.com"
)

// retryStatus is 429 plus everything >= 500.
func retryStatus(code int) bool { return code == http.StatusTooManyRequests || code >= 500 }

// UserAgent identifies the tool and a human to complain to. Anonymous polling
// is rude and gets the whole ATS host blocked for everybody.
func UserAgent() string {
	contact := strings.TrimSpace(os.Getenv(contactEnv))
	if contact == "" {
		contact = defaultContact
	}
	return fmt.Sprintf("job-tracker/%s (+personal job search; %s)", Version, contact)
}

// Fetch is the outcome of one attempt sequence. Never an error return.
type Fetch struct {
	URL      string
	FinalURL string // after redirects; ATS detection reads this
	Status   int    // 0 when the request never completed
	Body     string
	Headers  http.Header
	Err      string
	Attempts int
}

func (f Fetch) OK() bool {
	return f.Status != 0 && ((f.Status >= 200 && f.Status < 300) || f.Status == 304)
}

func (f Fetch) NotModified() bool { return f.Status == 304 }

func (f Fetch) ETag() string { return f.Headers.Get("ETag") }

func (f Fetch) LastModified() string { return f.Headers.Get("Last-Modified") }

// JSON parses the body into v. A 200 with a broken body is a failure, not a
// crash — callers treat the error as "no items this round".
func (f Fetch) JSON(v any) error {
	return json.Unmarshal([]byte(f.Body), v)
}

// JSONAny is the untyped form, for adapters that walk a dynamic payload.
func (f Fetch) JSONAny() any {
	var v any
	if err := json.Unmarshal([]byte(f.Body), &v); err != nil {
		return nil
	}
	return v
}

// --------------------------------------------------------------- host politeness

// hostGate allows one in-flight request per host, with a floor on the gap
// between them. Five workers all hammering boards-api.greenhouse.io is how you
// get a 429.
type hostGate struct {
	delay time.Duration
	mu    sync.Mutex
	locks map[string]*sync.Mutex
	last  map[string]time.Time
}

func newHostGate(delay time.Duration) *hostGate {
	return &hostGate{
		delay: delay,
		locks: make(map[string]*sync.Mutex),
		last:  make(map[string]time.Time),
	}
}

func (g *hostGate) forHost(host string) *sync.Mutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	if l, ok := g.locks[host]; ok {
		return l
	}
	l := &sync.Mutex{}
	g.locks[host] = l
	return l
}

func (g *hostGate) acquire(host string) *sync.Mutex {
	lock := g.forHost(host)
	lock.Lock()

	g.mu.Lock()
	last, seen := g.last[host]
	g.mu.Unlock()

	if seen {
		if gap := g.delay - time.Since(last); gap > 0 {
			time.Sleep(gap)
		}
	}
	return lock
}

func (g *hostGate) release(host string, lock *sync.Mutex) {
	g.mu.Lock()
	g.last[host] = time.Now()
	g.mu.Unlock()
	lock.Unlock()
}

var gate = newHostGate(HostDelay)

func sleepBackoff(attempt int, retryAfter string) {
	if retryAfter != "" {
		if secs, err := strconv.ParseFloat(retryAfter, 64); err == nil {
			d := time.Duration(secs * float64(time.Second))
			time.Sleep(min(d, 30*time.Second))
			return
		}
	}
	backoff := BackoffBase * (1 << attempt)
	jitter := time.Duration(rand.Float64() * 0.3 * float64(time.Second))
	time.Sleep(backoff + jitter)
}

// Options carry the conditional-request validators and any extra headers.
type Options struct {
	ETag         string
	LastModified string
	Headers      map[string]string
	Timeout      time.Duration
	Attempts     int
}

// Get performs a conditional GET with bounded retries. It returns a Fetch,
// always.
func Get(rawURL string, opts Options) Fetch {
	if opts.Timeout == 0 {
		opts.Timeout = Timeout
	}
	if opts.Attempts == 0 {
		opts.Attempts = Attempts
	}

	// Accept-Encoding is deliberately not set here. net/http adds it and
	// transparently decompresses the response only when it owns the header;
	// setting it by hand opts out of that and hands back raw gzip, which every
	// RSS parse then rejects as "illegal character code U+001F".
	hdrs := map[string]string{
		"User-Agent": UserAgent(),
	}
	if opts.ETag != "" {
		hdrs["If-None-Match"] = opts.ETag
	}
	if opts.LastModified != "" {
		hdrs["If-Modified-Since"] = opts.LastModified
	}
	for k, v := range opts.Headers {
		hdrs[k] = v
	}

	var host string
	if u, err := url.Parse(rawURL); err == nil {
		host = strings.ToLower(u.Hostname())
	}

	out := Fetch{URL: rawURL}
	client := &http.Client{Timeout: opts.Timeout}

	for attempt := range opts.Attempts {
		out.Attempts = attempt + 1

		resp, err := func() (*http.Response, error) {
			lock := gate.acquire(host)
			// Released exactly once per acquire, before any sleep — holding the
			// host lock through a backoff would serialise every other worker on it.
			defer gate.release(host, lock)

			req, err := http.NewRequest(http.MethodGet, rawURL, nil)
			if err != nil {
				return nil, err
			}
			for k, v := range hdrs {
				req.Header.Set(k, v)
			}
			return client.Do(req)
		}()

		if err != nil {
			out.Status = 0
			out.Err = err.Error()
			if attempt+1 < opts.Attempts {
				sleepBackoff(attempt, "")
				continue
			}
			return out
		}

		out.Status = resp.StatusCode
		out.Headers = resp.Header.Clone()
		out.FinalURL = rawURL
		if resp.Request != nil && resp.Request.URL != nil {
			out.FinalURL = resp.Request.URL.String()
		}
		out.Err = ""

		if resp.StatusCode == http.StatusNotModified {
			resp.Body.Close()
			out.Body = ""
			return out
		}
		if retryStatus(resp.StatusCode) {
			retryAfter := resp.Header.Get("Retry-After")
			resp.Body.Close()
			out.Err = fmt.Sprintf("HTTP %d", resp.StatusCode)
			if attempt+1 < opts.Attempts {
				sleepBackoff(attempt, retryAfter)
				continue
			}
			return out
		}
		if resp.StatusCode >= 400 {
			// Other 4xx are deterministic. Retrying a 404 just wastes the window.
			resp.Body.Close()
			out.Err = fmt.Sprintf("HTTP %d", resp.StatusCode)
			return out
		}

		// A server may compress even when nothing asked it to, and Go only
		// auto-decompresses what it negotiated itself.
		reader := io.Reader(resp.Body)
		if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
			zr, zerr := gzip.NewReader(resp.Body)
			if zerr != nil {
				resp.Body.Close()
				out.Err = fmt.Sprintf("gzip: %v", zerr)
				return out
			}
			defer zr.Close()
			reader = zr
		}

		body, err := io.ReadAll(reader)
		resp.Body.Close()
		if err != nil {
			out.Err = err.Error()
			return out
		}
		out.Body = string(body)
		return out
	}
	return out
}
