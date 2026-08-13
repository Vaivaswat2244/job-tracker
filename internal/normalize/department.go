package normalize

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Department pulls the team an opening belongs to out of the raw ATS payload.
//
// This was being stored and never read. All three providers report it, on 99%
// of postings, and it is far better evidence of what a role actually is than
// the title: "Solution Engineering" and "Sales Engineering" both contain
// "engineering" and are pre-sales jobs, while "MFC Hourly" and "Key Holders"
// are retail. No amount of title keyword matching recovers that.
func Department(source, rawJSON string) string {
	if rawJSON == "" {
		return ""
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &doc); err != nil {
		return ""
	}

	switch strings.ToLower(source) {
	case "greenhouse":
		// departments is a list; the first is the owning team, the rest are
		// parents in the org tree.
		if list, ok := doc["departments"].([]any); ok && len(list) > 0 {
			if first, ok := list[0].(map[string]any); ok {
				return clean(str(first["name"]))
			}
		}
	case "lever":
		if cat, ok := doc["categories"].(map[string]any); ok {
			// team is the narrower of the two and the more informative.
			if v := clean(str(cat["team"])); v != "" {
				return v
			}
			return clean(str(cat["department"]))
		}
	case "ashby":
		if v := clean(str(doc["department"])); v != "" {
			return v
		}
		return clean(str(doc["team"]))
	}
	return ""
}

// EmploymentType reads the ATS's own contract type where it offers one. Exact
// when present, absent on three quarters of postings — useful as a confirming
// signal, never as the only one.
func EmploymentType(source, rawJSON string) string {
	if rawJSON == "" {
		return ""
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &doc); err != nil {
		return ""
	}
	switch strings.ToLower(source) {
	case "lever":
		if cat, ok := doc["categories"].(map[string]any); ok {
			return clean(str(cat["commitment"]))
		}
	case "ashby":
		return clean(str(doc["employmentType"]))
	case "greenhouse":
		// Greenhouse puts it in free-form metadata, if at all.
		if list, ok := doc["metadata"].([]any); ok {
			for _, item := range list {
				m, ok := item.(map[string]any)
				if !ok {
					continue
				}
				name := strings.ToLower(str(m["name"]))
				if strings.Contains(name, "employment") || name == "type" {
					return clean(str(m["value"]))
				}
			}
		}
	}
	return ""
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func clean(s string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "-–—|,:;"))
}

// engineeringRe and notEngineeringRe are the deterministic first pass over
// department names.
//
// notEngineering is checked first and deliberately includes terms that contain
// "engineering": Solution Engineering, Sales Engineering, Field Engineering and
// Customer Engineering are all pre-sales or support functions. Getting these
// backwards is the single most common way a keyword filter produces junk.
var notEngineeringRe = regexp.MustCompile(`(?i)\b(sales|marketing|legal|compliance|finance|accounting|account\s+management|people|human\s+resources|hr|recruit|talent|support|customer\s+success|success|barista|hourly|key[\s-]?holders?|keyholders?|retail|store|warehouse|driver|courier|facilities|procurement|tax|payroll|communications|brand|content|design\s+ops|community|partnerships|business\s+development|revenue|gtm|go[\s-]to[\s-]market)\b|\b(solution|sales|field|customer|pre[\s-]?sales)\s+engineer`)

var engineeringRe = regexp.MustCompile(`(?i)\b(engineer|engineering|software|developer|development|platform|infrastructure|infra|sre|devops|reliability|backend|frontend|full[\s-]?stack|mobile|android|ios|web|data|machine\s+learning|ml|ai|security|cloud|systems|technology|technical|r&d|research)\b`)

// Function classes a department name. Unknown is a real answer: 885 distinct
// names appear across the boards polled, and inventing a class for the ones
// that read like "All Cost Centers" would be worse than admitting ignorance.
const (
	FunctionEngineering = "engineering"
	FunctionOther       = "other"
	FunctionUnknown     = "unknown"
)

// preSalesTitleRe catches the roles that sit inside an engineering department
// but are not engineering jobs. "Product Solution Engineer" under "Engineering
// Development" is a real example from the live data: the department is honest,
// the role is pre-sales. The title is the better evidence in this one direction.
var preSalesTitleRe = regexp.MustCompile(`(?i)\b(solutions?|sales|field|customer|partner|pre[\s-]?sales|forward[\s-]?deployed|implementation|onboarding|support|success)\s+(?:solution\s+)?engineer|\bengineer\s*[-,]\s*(?:sales|presales|solutions)`)

// Function classifies a department name into the coarse buckets the views
// filter on. Title is passed too so a pre-sales role inside an engineering
// department is not counted as engineering.
func Function(department, title string) string {
	if preSalesTitleRe.MatchString(title) {
		return FunctionOther
	}
	d := strings.TrimSpace(department)
	if d == "" {
		return FunctionUnknown
	}
	if notEngineeringRe.MatchString(d) {
		return FunctionOther
	}
	if engineeringRe.MatchString(d) {
		return FunctionEngineering
	}
	return FunctionUnknown
}
