package ats

import (
	"fmt"
	"time"

	"github.com/Vaivaswat2244/job-tracker/internal/db"
	"github.com/Vaivaswat2244/job-tracker/internal/httpx"
	"github.com/Vaivaswat2244/job-tracker/internal/textutil"
)

// Lever reads the postings API:
//
//	GET https://api.lever.co/v0/postings/{slug}?mode=json
//
// It returns a bare array. `createdAt` is epoch MILLISECONDS, not seconds —
// divide before converting or every posting lands in 1970.
type Lever struct{}

const leverAPI = "https://api.lever.co/v0/postings/%s?mode=json"

func (Lever) Name() string { return "lever" }

func (Lever) BoardURL(slug string) string { return "https://jobs.lever.co/" + slug }

func epochMS(value any) *string {
	ms, ok := textutil.AsFloat(value)
	if !ok {
		return nil
	}
	sec, frac := int64(ms/1000), ms-float64(int64(ms/1000))*1000
	t := time.Unix(sec, int64(frac)*int64(time.Millisecond)).UTC()
	s := t.Format(db.ISO8601)
	return &s
}

func (l Lever) Parse(payload any) []NormalizedJob {
	jobs := []NormalizedJob{}
	items, _ := payload.([]any)
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		externalID, hasID := textutil.AsString(item["id"])
		title, hasTitle := textutil.AsString(item["text"])
		if !hasID || !hasTitle {
			continue
		}
		url, _ := textutil.AsString(item["hostedUrl"])

		// descriptionPlain is authoritative when present; the HTML field is the
		// fallback and needs tags stripped.
		jd := ""
		if body, ok := textutil.AsString(item["descriptionPlain"]); ok {
			jd = textutil.Clean(body)
		} else {
			html, _ := textutil.AsString(item["description"])
			jd = textutil.StripHTML(html)
		}

		jobs = append(jobs, NormalizedJob{
			ExternalID:     externalID,
			Title:          title,
			URL:            url,
			Source:         l.Name(),
			PostedAt:       epochMS(item["createdAt"]),
			Location:       optString(textutil.Dig(item, "categories", "location")),
			EmploymentType: optString(textutil.Dig(item, "categories", "commitment")),
			JDText:         jd,
			Raw:            item,
		})
	}
	return jobs
}

func (l Lever) Fetch(slug, etag, lastModified string) FetchResult {
	resp := httpx.Get(fmt.Sprintf(leverAPI, slug), httpx.Options{
		ETag: etag, LastModified: lastModified,
	})
	return finish(l, resp, etag, lastModified, func(payload any) (any, string) {
		if _, ok := payload.([]any); !ok {
			return nil, "expected a JSON array of postings"
		}
		return payload, ""
	})
}
