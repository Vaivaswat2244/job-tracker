package funding

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Extraction pulls funding entities out of a headline.
//
// Rules first, always. The LLM path exists but is off by default and never runs
// unless the rules left something empty: a deterministic miss is auditable and a
// hallucinated company name is not.
//
// Partial extraction is never a reason to discard an item. A row with only a
// company name and a date is still useful; it is stored with
// extraction_confidence="low" and surfaces in the needs-review section.

// StageRule and CurrencyRule mirror the funding_rules.yaml entries.
type StageRule struct {
	Pattern string `yaml:"pattern"`
	Stage   string `yaml:"stage"`
}

type CurrencyRule struct {
	Pattern  string `yaml:"pattern"`
	Currency string `yaml:"currency"`
}

// RuleSet is funding_rules.yaml, compiled once. All patterns are
// case-insensitive, matched against the headline — headlines are the only text
// this pipeline stores, so they are the only text extraction runs on.
type RuleSet struct {
	Triggers     []string       `yaml:"triggers"`
	AntiTriggers []string       `yaml:"anti_triggers"`
	NearMiss     []string       `yaml:"near_miss"`
	Stages       []StageRule    `yaml:"stages"`
	NamePrefixes []string       `yaml:"name_prefixes"`
	Currencies   []CurrencyRule `yaml:"currencies"`

	triggers     []*regexp.Regexp
	antiTriggers []*regexp.Regexp
	nearMiss     []*regexp.Regexp
	stages       []*regexp.Regexp
	namePrefixes []*regexp.Regexp
	currencies   []*regexp.Regexp
}

// compile builds every pattern up front so a malformed rule fails at load time
// rather than silently never matching mid-run.
func (r *RuleSet) compile() error {
	var err error
	build := func(patterns []string) ([]*regexp.Regexp, error) {
		out := make([]*regexp.Regexp, 0, len(patterns))
		for _, p := range patterns {
			re, e := regexp.Compile("(?i)" + p)
			if e != nil {
				return nil, fmt.Errorf("bad pattern %q: %w", p, e)
			}
			out = append(out, re)
		}
		return out, nil
	}

	if r.triggers, err = build(r.Triggers); err != nil {
		return err
	}
	if r.antiTriggers, err = build(r.AntiTriggers); err != nil {
		return err
	}
	if r.nearMiss, err = build(r.NearMiss); err != nil {
		return err
	}
	if r.namePrefixes, err = build(r.NamePrefixes); err != nil {
		return err
	}

	stagePatterns := make([]string, len(r.Stages))
	for i, s := range r.Stages {
		stagePatterns[i] = s.Pattern
	}
	if r.stages, err = build(stagePatterns); err != nil {
		return err
	}

	currencyPatterns := make([]string, len(r.Currencies))
	for i, c := range r.Currencies {
		currencyPatterns[i] = c.Pattern
	}
	if r.currencies, err = build(currencyPatterns); err != nil {
		return err
	}
	return nil
}

func RulesPath() string {
	if p := os.Getenv("TRACKER_FUNDING_RULES"); p != "" {
		return p
	}
	return "funding_rules.yaml"
}

var (
	rulesMu    sync.Mutex
	rulesCache = map[string]*RuleSet{}
)

// Rules loads and caches the rule set for a path.
func Rules(path string) (*RuleSet, error) {
	if path == "" {
		path = RulesPath()
	}
	rulesMu.Lock()
	defer rulesMu.Unlock()
	if rs, ok := rulesCache[path]; ok {
		return rs, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	rs := &RuleSet{}
	if err := yaml.Unmarshal(data, rs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := rs.compile(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	rulesCache[path] = rs
	return rs, nil
}

func anyMatch(patterns []*regexp.Regexp, text string) bool {
	for _, re := range patterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// fundingVerbRe is the verb that separates the company from the rest of the
// headline.
var fundingVerbRe = regexp.MustCompile(`(?i)\b(raise[sd]?|raising|bags?|bagged|secures?|secured|nets?|netted|garners?|` +
	`garnered|closes?|closed|mops?\s+up|picks?\s+up|lands?|landed|scoops?\s+up|gets?|` +
	`receives?|attracts?|kicks?\s+off|wraps?\s+up|opens?)\b`)

// notAnInvestorRe matches placeholders that appear where an investor name would
// be. Storing these as investors would make the field useless for the one thing
// it is for.
var notAnInvestorRe = regexp.MustCompile(`(?i)^(?:others?|among|more|existing\s+(?:investors?|backers?|shareholders?)|` +
	`investors?|backers?|new\s+and\s+existing|its\s+\w+)$`)

var amountRe = regexp.MustCompile(`(?i)` +
	`(?:[$₹]|\bUSD\b|\bINR\b|\bRs\.?\s*)\s*\d[\d,.]*(?:\s*[-\s])?(?:mn|mln|million|bn|billion|cr|crore|lakh|k|m\b)?` +
	`|\d[\d,.]*\s*(?:mn|mln|million|bn|billion|cr|crore|lakh)\b`)

var (
	investorsRe      = regexp.MustCompile(`(?i)\b(?:led\s+by|backed\s+by|from)\s+(.+?)(?:\s+(?:to|for|in|at|as|amid|after)\s|$)`)
	splitInvestorsRe = regexp.MustCompile(`(?i)\s*(?:,|\band\b|&|\+)\s*`)
	auxiliaryTailRe  = regexp.MustCompile(`(?i)\s+(?:is|are|was|were|to|set|about|looking|planning|plans?|preparing|` +
		`poised|likely|in|advanced|talks|close|closing|said|reported|expected|eyes|` +
		`may|will|could|might)\s*$`)
	trailingDescriptorRe = regexp.MustCompile(`(?i),\s*(?:an?|the)\s+[^,]{0,40}$`)
	trailingNounRe       = regexp.MustCompile(`(?i)\s+(?:startup|firm|company|platform|brand|maker|app)$`)
	multiCompanyRe       = regexp.MustCompile(`(?i),| and `)
)

func (r *RuleSet) IsFunding(headline string) bool {
	if anyMatch(r.antiTriggers, headline) {
		return false
	}
	return anyMatch(r.triggers, headline)
}

// IsNearMiss reports a headline that mentions money but tripped no trigger —
// worth logging, not storing.
func (r *RuleSet) IsNearMiss(headline string) bool { return anyMatch(r.nearMiss, headline) }

func (r *RuleSet) RoundStage(headline string) string {
	for i, re := range r.stages {
		if re.MatchString(headline) {
			return r.Stages[i].Stage
		}
	}
	return "unknown"
}

func (r *RuleSet) Currency(text string) string {
	for i, re := range r.currencies {
		if re.MatchString(text) {
			return r.Currencies[i].Currency
		}
	}
	return ""
}

func Amount(headline string) string {
	match := amountRe.FindString(headline)
	if match == "" {
		return ""
	}
	return strings.Join(strings.Fields(match), " ")
}

// CompanyName is everything before the funding verb, minus the descriptive noise.
func (r *RuleSet) CompanyName(headline string) string {
	// Split at the verb FIRST. The descriptor rules are greedy by necessity
	// ("Personal assistance startup Hulp"), and run against a whole headline
	// they happily consume the verb too — "Acme secures venture debt funding"
	// would strip through "venture" and leave nothing attributable.
	verb := fundingVerbRe.FindStringIndex(headline)
	if verb == nil {
		// "Series A funding for Acme" style. Give up rather than guess.
		return ""
	}
	text := headline[:verb[0]]

	for _, re := range r.namePrefixes {
		text = re.ReplaceAllString(text, "")
	}

	// "Simplismart set to raise $9 Mn": the verb split leaves the auxiliary
	// attached to the name. Announced-but-not-closed rounds are still signal,
	// so trim the auxiliary rather than dropping the item. Applied repeatedly:
	// "Beta plans to" needs two passes ("to", then "plans"), and "is in advanced
	// talks to" is five words deep.
	for range 8 {
		trimmed := auxiliaryTailRe.ReplaceAllString(strings.TrimSpace(text), "")
		if trimmed == strings.TrimSpace(text) {
			break
		}
		text = trimmed
	}

	// Strip a trailing descriptor the prefix rules could not reach because it
	// sat after the company name ("Acme, an EV startup, raises ...").
	text = trailingDescriptorRe.ReplaceAllString(strings.TrimRight(strings.TrimSpace(text), ","), "")
	text = trailingNounRe.ReplaceAllString(strings.TrimSpace(text), "")
	return strings.Trim(text, " -–—:|,")
}

func Investors(headline string) []string {
	match := investorsRe.FindStringSubmatch(headline)
	if match == nil {
		return []string{}
	}
	names := []string{}
	for _, part := range splitInvestorsRe.Split(match[1], -1) {
		name := strings.Trim(part, " .,-–—;:")
		if len([]rune(name)) > 1 && !notAnInvestorRe.MatchString(name) {
			names = append(names, name)
		}
	}
	return names
}

// Extract runs the deterministic rules over one headline.
func (r *RuleSet) Extract(headline, url, publishedAt string) Extraction {
	name := r.CompanyName(headline)
	stage := r.RoundStage(headline)
	amount := Amount(headline)

	currencyOver := amount
	if currencyOver == "" {
		currencyOver = headline
	}

	result := Extraction{
		CompanyName: name,
		RoundStage:  stage,
		AmountRaw:   amount,
		Currency:    r.Currency(currencyOver),
		Investors:   Investors(headline),
		AnnouncedAt: publishedAt,
		ArticleURL:  url,
		Method:      "rules",
		RawText:     headline,
	}

	// A headline naming several companies at once ("A, B and C raise seed") is
	// not confidently attributable to one of them; it stays low and gets reviewed.
	multi := name != "" && multiCompanyRe.MatchString(name)
	if name != "" && !multi && stage != "unknown" && amount != "" {
		result.Confidence = "high"
	} else {
		result.Confidence = "low"
	}
	return result
}

// LLMEnabled is off unless explicitly switched on. INV-2 territory: an invented
// company name here eventually becomes outreach to a company that never raised.
func LLMEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TRACKER_FUNDING_LLM"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
