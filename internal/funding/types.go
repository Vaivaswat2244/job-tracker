// Package funding is Tier 3: funding-signal ingest.
//
// It detects funding announcements and uses them to drive watchlist priority.
// It applies to nothing and contacts nobody — per INV-2 there is no send path
// here, and per deliverable 4 no company enters the watchlist without user
// approval.
package funding

// FeedItem is one article as listed. Bodies are never stored: headline, link
// and date are all this pipeline is entitled to keep.
type FeedItem struct {
	Headline    string
	URL         string
	PublishedAt string
	Source      string
}

// SourceResult carries per-run parser health, recorded for every source on
// every run.
//
// Items is nil on failure and an empty non-nil slice when a source parsed
// cleanly but listed nothing — the same distinction the ATS adapters preserve,
// and for the same reason: only one of those is a dead feed.
type SourceResult struct {
	Items           []FeedItem
	ParseOK         bool
	ItemsFound      int
	SelectorVersion int
	Status          int
	Error           string
	NotModified     bool
	ETag            string
	LastModified    string
}

// Extraction is what the rules pulled out of a headline.
type Extraction struct {
	CompanyName string
	RoundStage  string
	AmountRaw   string
	Currency    string
	Investors   []string
	AnnouncedAt string
	ArticleURL  string
	Confidence  string // "high" | "low"
	Method      string // "rules" | "llm"
	RawText     string // the text extraction actually ran on
	LLMRaw      string // unparsed model response, kept for auditing
}
