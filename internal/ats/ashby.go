package ats

import (
	"fmt"
	"strings"

	"github.com/Vaivaswat2244/job-tracker/internal/httpx"
	"github.com/Vaivaswat2244/job-tracker/internal/textutil"
)

// Ashby reads the job board posting API:
//
//	GET https://api.ashbyhq.com/posting-api/job-board/{slug}?includeCompensation=true
//
// Compensation is nested and optional at every level: a board may send no
// `compensation` key, a summary string with no numbers, or tiers whose
// components are equity rather than salary. Everything below tolerates all three.
type Ashby struct{}

const ashbyAPI = "https://api.ashbyhq.com/posting-api/job-board/%s?includeCompensation=true"

func (Ashby) Name() string { return "ashby" }

func (Ashby) BoardURL(slug string) string { return "https://jobs.ashbyhq.com/" + slug }

// salaryComponents collects every salary component anywhere in the
// compensation blob, discarding equity and other non-salary types.
func salaryComponents(comp any) []map[string]any {
	blob, ok := comp.(map[string]any)
	if !ok {
		return nil
	}

	var found []any
	if summary, ok := blob["summaryComponents"].([]any); ok {
		found = append(found, summary...)
	}
	if tiers, ok := blob["compensationTiers"].([]any); ok {
		for _, raw := range tiers {
			if tier, ok := raw.(map[string]any); ok {
				if components, ok := tier["components"].([]any); ok {
					found = append(found, components...)
				}
			}
		}
	}

	var salary []map[string]any
	for _, raw := range found {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := textutil.AsString(c["compensationType"])
		if strings.ToLower(kind) == "salary" {
			salary = append(salary, c)
		}
	}
	return salary
}

func pay(comp any) (*float64, *float64, *string) {
	var (
		low, high *float64
		currency  *string
	)
	for _, c := range salaryComponents(comp) {
		if v, ok := textutil.AsFloat(c["minValue"]); ok {
			if low == nil || v < *low {
				low = &v
			}
		}
		if v, ok := textutil.AsFloat(c["maxValue"]); ok {
			if high == nil || v > *high {
				high = &v
			}
		}
		if currency == nil {
			currency = optString(c["currencyCode"])
		}
	}
	return low, high, currency
}

func (a Ashby) Parse(payload any) []NormalizedJob {
	jobs := []NormalizedJob{}
	items, _ := textutil.Dig(payload, "jobs").([]any)
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		externalID, hasID := textutil.AsString(item["id"])
		title, hasTitle := textutil.AsString(item["title"])
		if !hasID || !hasTitle {
			continue
		}
		url, _ := textutil.AsString(item["jobUrl"])
		payMin, payMax, currency := pay(item["compensation"])

		location := optString(item["location"])
		if location == nil {
			location = optString(textutil.Dig(item, "address", "postalAddress", "addressLocality"))
		}

		// Only a real JSON boolean counts; an absent or non-bool isRemote stays
		// unknown rather than collapsing to false.
		var remote *bool
		if b, ok := item["isRemote"].(bool); ok {
			remote = &b
		}

		jd := ""
		if body, ok := textutil.AsString(item["descriptionPlain"]); ok {
			jd = textutil.Clean(body)
		} else {
			html, _ := textutil.AsString(item["descriptionHtml"])
			jd = textutil.StripHTML(html)
		}

		jobs = append(jobs, NormalizedJob{
			ExternalID:     externalID,
			Title:          title,
			URL:            url,
			Source:         a.Name(),
			PostedAt:       optString(item["publishedAt"]),
			Location:       location,
			EmploymentType: optString(item["employmentType"]),
			Remote:         remote,
			JDText:         jd,
			PayMin:         payMin,
			PayMax:         payMax,
			PayCurrency:    currency,
			Raw:            item,
		})
	}
	return jobs
}

func (a Ashby) Fetch(slug, etag, lastModified string) FetchResult {
	resp := httpx.Get(fmt.Sprintf(ashbyAPI, slug), httpx.Options{
		ETag: etag, LastModified: lastModified,
	})
	return finish(a, resp, etag, lastModified, func(payload any) (any, string) {
		if payload == nil {
			return nil, "unparseable JSON body"
		}
		return payload, ""
	})
}
