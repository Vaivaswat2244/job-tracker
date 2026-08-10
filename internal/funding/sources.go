package funding

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
	"gopkg.in/yaml.v3"

	"github.com/Vaivaswat2244/job-tracker/internal/db"
	"github.com/Vaivaswat2244/job-tracker/internal/httpx"
	"github.com/Vaivaswat2244/job-tracker/internal/normalize"
)

// Site redesigns are the expected failure mode, so every run records ParseOK,
// ItemsFound and SelectorVersion, and a source that previously produced items
// and now produces none raises the same stale_feed alert the ATS poller uses.

// SourceConfig is one entry of funding_sources.yaml.
type SourceConfig struct {
	Name            string            `yaml:"name"`
	Enabled         *bool             `yaml:"enabled"`
	Mode            string            `yaml:"mode"`
	URL             string            `yaml:"url"`
	SelectorVersion int               `yaml:"selector_version"`
	JSONScriptID    string            `yaml:"json_script_id"`
	JSONPath        string            `yaml:"json_path"`
	Fields          map[string]string `yaml:"fields"`
	LinkPrefix      string            `yaml:"link_prefix"`
	Selectors       map[string]string `yaml:"selectors"`
	CSSFallback     map[string]string `yaml:"css_fallback"`
}

// IsEnabled mirrors `config.get("enabled") is False`: absent means enabled.
func (c SourceConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

func (c SourceConfig) version() int {
	if c.SelectorVersion == 0 {
		return 1
	}
	return c.SelectorVersion
}

func (c SourceConfig) field(key, fallback string) string {
	if v, ok := c.Fields[key]; ok && v != "" {
		return v
	}
	return fallback
}

// ConfigPath resolves funding_sources.yaml the way the Python build did.
func ConfigPath() string {
	if p := os.Getenv("TRACKER_FUNDING_SOURCES"); p != "" {
		return p
	}
	return "funding_sources.yaml"
}

// LoadConfig reads the source list, skipping entries with no name. A missing
// file is not an error: it means no sources are configured.
func LoadConfig(path string) ([]SourceConfig, error) {
	if path == "" {
		path = ConfigPath()
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var doc struct {
		Sources []SourceConfig `yaml:"sources"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	out := make([]SourceConfig, 0, len(doc.Sources))
	for _, s := range doc.Sources {
		if s.Name != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// ----------------------------------------------------------------------- dates

// toISO accepts either an RFC 2822 date (what RSS pubDate carries) or an ISO
// timestamp, and returns the ISO form the DB stores.
func toISO(value string) string {
	text := strings.TrimSpace(value)
	if text == "" {
		return ""
	}
	// RFC 1123/2822, with and without a named zone, as feeds vary.
	for _, layout := range []string{
		http.TimeFormat, // "Mon, 02 Jan 2006 15:04:05 GMT"
		"Mon, 02 Jan 2006 15:04:05 -0700",
		"Mon, 2 Jan 2006 15:04:05 -0700",
		"Mon, 02 Jan 2006 15:04:05 MST",
		"Mon, 2 Jan 2006 15:04:05 MST",
		"02 Jan 2006 15:04:05 -0700",
	} {
		// The feed's own offset is preserved rather than normalised to UTC: a
		// post at 00:30 +05:30 falls on the previous day in UTC, which would
		// move its ISO week and shift the funding window by a day.
		if t, err := time.Parse(layout, text); err == nil {
			return t.Format(db.ISO8601)
		}
	}

	// Fall back to the ISO parser, giving "2026-08-06 09:00:00" the T it needs.
	candidate := text
	if i := strings.Index(candidate, " "); i >= 0 {
		candidate = candidate[:i] + "T" + candidate[i+1:]
	}
	if parsed, ok := normalize.ParseDT(candidate); ok {
		return parsed.Format(db.ISO8601)
	}
	return ""
}

// ------------------------------------------------------------------- rss/atom

// xmlNode is a namespace-agnostic parse tree. Feeds mix RSS and Atom, and
// several wrap elements in namespaces that differ per publisher, so matching is
// done on the local name only.
type xmlNode struct {
	Name     string
	Attrs    map[string]string
	Text     string
	Children []*xmlNode
}

func parseXML(body string) (*xmlNode, error) {
	dec := xml.NewDecoder(strings.NewReader(body))
	// Strict=false tolerates the undeclared HTML entities publishers embed in
	// titles. AutoClose is deliberately NOT set to xml.HTMLAutoClose: that
	// treats <link> as a void element per HTML rules, and RSS closes it
	// normally, so every feed would die on its first </link>.
	dec.Strict = false
	// Feeds routinely declare charsets the stdlib does not know; the bytes are
	// already decoded, so pass them through rather than failing the whole feed.
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }

	var root, current *xmlNode
	var stack []*xmlNode

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			node := &xmlNode{Name: t.Name.Local, Attrs: map[string]string{}}
			for _, a := range t.Attr {
				node.Attrs[a.Name.Local] = a.Value
			}
			if current != nil {
				current.Children = append(current.Children, node)
			} else if root == nil {
				root = node
			}
			stack = append(stack, node)
			current = node
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			if len(stack) > 0 {
				current = stack[len(stack)-1]
			} else {
				current = nil
			}
		case xml.CharData:
			if current != nil {
				current.Text += string(t)
			}
		}
	}
	if root == nil {
		return nil, fmt.Errorf("no XML root element")
	}
	return root, nil
}

// findAll walks the whole tree collecting nodes with a matching local name,
// which is what ElementTree's ".//name" did.
func (n *xmlNode) findAll(name string) []*xmlNode {
	var out []*xmlNode
	var walk func(*xmlNode)
	walk = func(node *xmlNode) {
		for _, c := range node.Children {
			if c.Name == name {
				out = append(out, c)
			}
			walk(c)
		}
	}
	walk(n)
	return out
}

// childText returns the first non-empty direct child text matching any of the
// names, in the order given.
func (n *xmlNode) childText(names ...string) string {
	for _, name := range names {
		for _, c := range n.Children {
			if c.Name == name {
				if t := strings.TrimSpace(c.Text); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

func ParseRSS(body, source string) ([]FeedItem, error) {
	root, err := parseXML(body)
	if err != nil {
		return nil, err
	}

	nodes := root.findAll("item")
	if len(nodes) == 0 {
		nodes = root.findAll("entry")
	}

	items := []FeedItem{}
	for _, node := range nodes {
		headline := node.childText("title")
		link := node.childText("link")
		if link == "" {
			// Atom puts the URL in an href attribute rather than the body.
			for _, c := range node.Children {
				if c.Name == "link" && c.Attrs["href"] != "" {
					link = c.Attrs["href"]
					break
				}
			}
		}
		if headline == "" || link == "" {
			continue
		}
		items = append(items, FeedItem{
			Headline:    headline,
			URL:         strings.TrimSpace(link),
			PublishedAt: toISO(node.childText("pubDate", "published", "updated")),
			Source:      source,
		})
	}
	return items, nil
}

// ------------------------------------------------------------------ json_path

func digPath(payload any, path string) any {
	node := payload
	for _, part := range strings.Split(path, ".") {
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = m[part]
	}
	return node
}

func ParseJSONPath(body string, config SourceConfig, source string) ([]FeedItem, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return nil, err
	}

	scriptID := config.JSONScriptID
	if scriptID == "" {
		scriptID = "__NEXT_DATA__"
	}
	blob := strings.TrimSpace(doc.Find("script#" + scriptID).First().Text())
	if blob == "" {
		return []FeedItem{}, nil
	}

	var payload any
	if err := json.Unmarshal([]byte(blob), &payload); err != nil {
		return nil, fmt.Errorf("embedded JSON: %w", err)
	}

	rows, _ := digPath(payload, config.JSONPath).([]any)
	items := []FeedItem{}
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		headline := stringField(row[config.field("title", "title")])
		slug := stringField(row[config.field("slug", "slug")])
		if slug == "" {
			slug = stringField(row["url"])
		}
		if headline == "" || slug == "" {
			continue
		}
		items = append(items, FeedItem{
			Headline:    strings.TrimSpace(headline),
			URL:         absolute(config.LinkPrefix, slug),
			PublishedAt: toISO(stringField(row[config.field("published", "publish")])),
			Source:      source,
		})
	}
	return items, nil
}

func stringField(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%v", t)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

func absolute(prefix, href string) string {
	if strings.HasPrefix(href, "http") {
		return href
	}
	base, err := url.Parse(prefix)
	if err != nil {
		return href
	}
	ref, err := url.Parse(href)
	if err != nil {
		return href
	}
	return base.ResolveReference(ref).String()
}

// ------------------------------------------------------------------------ css

func selectorSet(config SourceConfig) map[string]string {
	if len(config.CSSFallback) > 0 {
		return config.CSSFallback
	}
	return config.Selectors
}

func selectorOr(set map[string]string, key, fallback string) string {
	if v, ok := set[key]; ok && v != "" {
		return v
	}
	return fallback
}

func ParseCSS(body string, config SourceConfig, source string) ([]FeedItem, error) {
	selectors := selectorSet(config)
	if selectors["item"] == "" {
		return []FeedItem{}, nil
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return nil, err
	}

	items := []FeedItem{}
	doc.Find(selectors["item"]).Each(func(_ int, node *goquery.Selection) {
		linkNode := node.Find(selectorOr(selectors, "link", "a")).First()
		href, hasHref := linkNode.Attr("href")
		if linkNode.Length() == 0 || !hasHref || href == "" {
			return
		}

		headline := ""
		if titleNode := node.Find(selectorOr(selectors, "title", "a")).First(); titleNode.Length() > 0 {
			headline = spacedText(titleNode)
		}
		if headline == "" {
			headline, _ = linkNode.Attr("title")
		}
		if headline == "" {
			headline, _ = node.Find("img").First().Attr("alt")
		}
		if headline == "" {
			return
		}
		items = append(items, FeedItem{
			Headline: strings.TrimSpace(headline),
			URL:      absolute(config.LinkPrefix, href),
			Source:   source,
		})
	})
	return items, nil
}

// spacedText is BeautifulSoup's get_text(" ", strip=True): each text node
// stripped, then joined with single spaces. goquery's Text concatenates raw, so
// "<span>Acme</span><span>raises</span>" would otherwise become "Acmeraises".
func spacedText(sel *goquery.Selection) string {
	var parts []string
	for _, node := range sel.Nodes {
		collectText(node, &parts)
	}
	return strings.Join(parts, " ")
}

func collectText(n *html.Node, parts *[]string) {
	if n.Type == html.TextNode {
		if t := strings.TrimSpace(n.Data); t != "" {
			*parts = append(*parts, t)
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		collectText(c, parts)
	}
}

// ----------------------------------------------------------------- dispatch

// Parse dispatches on the configured mode, falling back to CSS when the primary
// mode yields nothing but a fallback is configured.
func Parse(body string, config SourceConfig) ([]FeedItem, error) {
	mode := config.Mode
	if mode == "" {
		mode = "rss"
	}

	var (
		items []FeedItem
		err   error
	)
	switch mode {
	case "rss":
		items, err = ParseRSS(body, config.Name)
	case "json_path":
		items, err = ParseJSONPath(body, config, config.Name)
	default:
		items, err = ParseCSS(body, config, config.Name)
	}
	if err != nil {
		return nil, err
	}

	if len(items) == 0 && mode != "css" && len(config.CSSFallback) > 0 {
		return ParseCSS(body, config, config.Name)
	}
	return items, nil
}

// ---------------------------------------------------------------------- fetch

// Fetch performs one request per source per run: conditional, robots-respecting.
func Fetch(config SourceConfig, etag, lastModified string) SourceResult {
	version := config.version()
	if config.URL == "" {
		return SourceResult{Error: "no url configured", SelectorVersion: version}
	}
	if !httpx.Allowed(config.URL) {
		return SourceResult{Error: "disallowed by robots.txt", SelectorVersion: version}
	}

	resp := httpx.Get(config.URL, httpx.Options{ETag: etag, LastModified: lastModified})
	if resp.NotModified() {
		return SourceResult{
			NotModified: true, ParseOK: true, Status: 304, SelectorVersion: version,
			ETag: etag, LastModified: lastModified,
		}
	}
	if !resp.OK() {
		msg := resp.Err
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.Status)
		}
		return SourceResult{Status: resp.Status, Error: msg, SelectorVersion: version}
	}

	items, err := Parse(resp.Body, config)
	if err != nil {
		// A parse failure is a site redesign until proven otherwise, and must be
		// loud rather than silently yielding zero items.
		return SourceResult{
			Status: resp.Status, SelectorVersion: version,
			Error: fmt.Sprintf("parse failed: %v", err),
		}
	}

	return SourceResult{
		Items: items, ParseOK: true, ItemsFound: len(items), SelectorVersion: version,
		Status: resp.Status, ETag: resp.ETag(), LastModified: resp.LastModified(),
	}
}
