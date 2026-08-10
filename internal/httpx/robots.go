package httpx

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/temoto/robotstxt"
)

var (
	robotsMu    sync.Mutex
	robotsCache = map[string]*robotstxt.Group{}
)

// Allowed performs the robots.txt check. It fails open: an unreachable
// robots.txt is not consent withheld, and blocking the whole run on it would be
// its own kind of silent loss.
func Allowed(rawURL string) bool {
	parts, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	root := fmt.Sprintf("%s://%s", parts.Scheme, parts.Host)

	robotsMu.Lock()
	group, cached := robotsCache[root]
	if !cached {
		group = fetchRobots(root)
		robotsCache[root] = group
	}
	robotsMu.Unlock()

	if group == nil {
		return true
	}
	path := parts.Path
	if path == "" {
		path = "/"
	}
	return group.Test(path)
}

// fetchRobots returns nil for every failure mode — unreachable host, 4xx, or an
// unparseable body — which Allowed reads as "no restrictions".
func fetchRobots(root string) *robotstxt.Group {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, root+"/robots.txt", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", UserAgent())

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil
	}

	data, err := robotstxt.FromResponse(resp)
	if err != nil {
		return nil
	}
	return data.FindGroup(UserAgent())
}
