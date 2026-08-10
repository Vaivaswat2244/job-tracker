package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/Vaivaswat2244/job-tracker/internal/funding"
)

type row struct {
	CompanyName any      `json:"company_name"`
	RoundStage  string   `json:"round_stage"`
	AmountRaw   any      `json:"amount_raw"`
	Currency    any      `json:"currency"`
	Investors   []string `json:"investors"`
	AnnouncedAt any      `json:"announced_at"`
	ArticleURL  string   `json:"article_url"`
	Confidence  string   `json:"confidence"`
	Method      string   `json:"method"`
	RawText     string   `json:"raw_text"`
	IsFunding   bool     `json:"is_funding"`
	IsNearMiss  bool     `json:"is_near_miss"`
}

// nilIfEmpty mirrors Python's None for an absent value.
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func main() {
	var heads []string
	data, _ := os.ReadFile(os.Args[1])
	json.Unmarshal(data, &heads)

	rules, err := funding.Rules("funding_rules.yaml")
	if err != nil {
		panic(err)
	}

	out := []row{}
	for _, h := range heads {
		e := rules.Extract(h, "http://x/1", "2026-08-05T10:00:00+00:00")
		out = append(out, row{
			CompanyName: nilIfEmpty(e.CompanyName), RoundStage: e.RoundStage,
			AmountRaw: nilIfEmpty(e.AmountRaw), Currency: nilIfEmpty(e.Currency),
			Investors: e.Investors, AnnouncedAt: nilIfEmpty(e.AnnouncedAt),
			ArticleURL: e.ArticleURL, Confidence: e.Confidence, Method: e.Method,
			RawText: e.RawText, IsFunding: rules.IsFunding(h), IsNearMiss: rules.IsNearMiss(h),
		})
	}
	enc, _ := json.Marshal(out)
	os.WriteFile(os.Args[2], enc, 0o644)
	fmt.Printf("%d headlines\n", len(out))
}
