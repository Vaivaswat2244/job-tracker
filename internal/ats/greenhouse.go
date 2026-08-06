package ats

import (
	"fmt"

	"github.com/Vaivaswat2244/job-tracker/internal/httpx"
	"github.com/Vaivaswat2244/job-tracker/internal/textutil"
)

// Greenhouse reads the job board API:
//
//	GET https://boards-api.greenhouse.io/v1/boards/{slug}/jobs?content=true
//
// `content` arrives HTML-escaped, so it is unescaped before tags are stripped.
type Greenhouse struct{}

const greenhouseAPI = "https://boards-api.greenhouse.io/v1/boards/%s/jobs?content=true"

func (Greenhouse) Name() string { return "greenhouse" }

func (Greenhouse) BoardURL(slug string) string {
	return "https://job-boards.greenhouse.io/" + slug
}

func (g Greenhouse) Parse(payload any) []NormalizedJob {
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
		url, _ := textutil.AsString(item["absolute_url"])

		content, _ := textutil.AsString(item["content"])
		jobs = append(jobs, NormalizedJob{
			ExternalID: externalID,
			Title:      title,
			URL:        url,
			Source:     g.Name(),
			PostedAt:   optString(item["updated_at"]),
			Location:   optString(textutil.Dig(item, "location", "name")),
			JDText:     textutil.StripHTML(content),
			Raw:        item,
		})
	}
	return jobs
}

func (g Greenhouse) Fetch(slug, etag, lastModified string) FetchResult {
	resp := httpx.Get(fmt.Sprintf(greenhouseAPI, slug), httpx.Options{
		ETag: etag, LastModified: lastModified,
	})
	return finish(g, resp, etag, lastModified, func(payload any) (any, string) {
		if payload == nil {
			return nil, "unparseable JSON body"
		}
		return payload, ""
	})
}

// finish collapses the transport handling every adapter repeats: 304 short
// circuit, transport failure, then a provider-specific payload check.
func finish(
	a interface {
		Adapter
		Parse(any) []NormalizedJob
	},
	resp httpx.Fetch,
	etag, lastModified string,
	check func(any) (any, string),
) FetchResult {
	if resp.NotModified() {
		return FetchResult{
			Status: 304, NotModified: true, ETag: etag, LastModified: lastModified,
		}
	}
	if !resp.OK() {
		msg := resp.Err
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.Status)
		}
		return FetchResult{Status: resp.Status, Error: msg}
	}

	payload, problem := check(resp.JSONAny())
	if problem != "" {
		return FetchResult{Status: resp.Status, Error: problem}
	}
	return FetchResult{
		Jobs:         a.Parse(payload),
		Status:       resp.Status,
		ETag:         resp.ETag(),
		LastModified: resp.LastModified(),
	}
}
