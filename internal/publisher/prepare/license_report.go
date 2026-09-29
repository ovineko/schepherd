package prepare

import (
	"slices"
	"strings"

	"github.com/ovineko/schepherd/internal/publisher/policy"
)

// LicenseTotals counts upstream records decided by automatic license
// detection: AutoAllowed were included on at least one detected license,
// AutoRefused were held for review because detection refused the first URL
// that held them, by reason.
type LicenseTotals struct {
	AutoAllowed int          `json:"autoAllowed"`
	AutoRefused AutoRefusals `json:"autoRefused"`
}

// AutoRefusals counts records held for review by automatic license
// detection per policy.Refused* reason.
type AutoRefusals struct {
	NoLicense       int `json:"noLicense"`
	NotPermissive   int `json:"notPermissive"`
	NotAsserted     int `json:"notAsserted"`
	UnsupportedHost int `json:"unsupportedHost"`
	FetchFailed     int `json:"fetchFailed"`
}

// Refused returns the number of records detection held for review.
func (t *LicenseTotals) Refused() int {
	r := t.AutoRefused

	return r.NoLicense + r.NotPermissive + r.NotAsserted + r.UnsupportedHost + r.FetchFailed
}

// count adds one upstream record with the given status and decision.
func (t *LicenseTotals) count(status string, d *policy.Decision) {
	switch {
	case status == StatusIncluded && len(d.Detections) > 0:
		t.AutoAllowed++
	case status != StatusPendingReview:
	case d.AutoReason == policy.RefusedNoLicense:
		t.AutoRefused.NoLicense++
	case d.AutoReason == policy.RefusedNotPermissive:
		t.AutoRefused.NotPermissive++
	case d.AutoReason == policy.RefusedNotAsserted:
		t.AutoRefused.NotAsserted++
	case d.AutoReason == policy.RefusedUnsupportedHost:
		t.AutoRefused.UnsupportedHost++
	case d.AutoReason == policy.RefusedFetchFailed:
		t.AutoRefused.FetchFailed++
	}
}

// licenseSources returns the detections that allowed an included schema, the
// form its license decision in prepared.json pins them in: sorted by URL, one
// per URL.
func licenseSources(d *policy.Decision) []policy.Detection {
	var out []policy.Detection

	for _, detection := range d.Detections {
		if detection.Verdict == policy.Allow && !slices.ContainsFunc(out, func(o policy.Detection) bool { return o.URL == detection.URL }) {
			out = append(out, detection)
		}
	}

	slices.SortFunc(out, func(a, b policy.Detection) int { return strings.Compare(a.URL, b.URL) })

	return out
}
