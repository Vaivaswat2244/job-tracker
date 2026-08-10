// Package ats holds the provider adapters. Each exposes Fetch(slug) -> FetchResult.
package ats

import "github.com/Vaivaswat2244/job-tracker/internal/textutil"

// Providers is the set with a working adapter. "unknown" is a valid stored
// value but has no adapter.
var Providers = []string{"greenhouse", "lever", "ashby"}

// NormalizedJob is one posting with provider-shaped fields flattened.
// Everything is optional except the three that identify it — adapters must
// never fail on a missing field.
type NormalizedJob struct {
	ExternalID     string
	Title          string
	URL            string
	Source         string  // "greenhouse" | "lever" | "ashby"
	PostedAt       *string // ISO-8601 date/datetime
	Location       *string
	EmploymentType *string
	Remote         *bool
	JDText         string
	PayMin         *float64
	PayMax         *float64
	PayCurrency    *string
	Raw            map[string]any
}

// FetchResult is the adapter outcome plus the transport facts poll health
// needs.
//
// Jobs is nil on failure and an empty non-nil slice on a genuinely empty board.
// That difference is the whole point of feed-death detection, so it must
// survive to the caller — every Parse returns a non-nil slice for this reason.
type FetchResult struct {
	Jobs         []NormalizedJob
	Status       int
	Error        string
	NotModified  bool
	ETag         string
	LastModified string
}

func (r FetchResult) OK() bool { return r.Error == "" && r.Jobs != nil }

// Adapter is the contract every provider satisfies.
type Adapter interface {
	Name() string
	BoardURL(slug string) string
	Fetch(slug string, etag, lastModified string) FetchResult
}

// For returns the adapter for a provider name, or nil when there is none.
func For(name string) Adapter {
	switch name {
	case "greenhouse":
		return Greenhouse{}
	case "lever":
		return Lever{}
	case "ashby":
		return Ashby{}
	default:
		return nil
	}
}

// optString mirrors textutil.AsString but yields a nil pointer where the Python
// build yielded None, so an absent field stores as SQL NULL rather than "".
func optString(value any) *string {
	s, ok := textutil.AsString(value)
	if !ok {
		return nil
	}
	return &s
}

func optFloat(value any) *float64 {
	f, ok := textutil.AsFloat(value)
	if !ok {
		return nil
	}
	return &f
}

// str dereferences an optional string for the callers that treat absent and
// empty alike.
func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
