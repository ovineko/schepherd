package notes

import (
	"cmp"
	"slices"

	"github.com/ovineko/schepherd/internal/publisher/state"
)

// Held is a published schema that a run could not refresh: the catalog keeps
// its last published entry and artifact. Reason is one of state.HeldReasons;
// an unknown reason is shown as it is.
type Held struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

var reasonTexts = map[string]string{
	state.HeldRemovedUpstream:        "upstream removed it",
	state.HeldFetchFailed:            "its source could not be fetched",
	state.HeldLicenseDetectionFailed: "its license could not be detected",
	state.HeldLicenseRefused:         "its license is no longer on the permissive allowlist",
	state.HeldLicenseReview:          "its license is held for review",
	state.HeldPrepareFailed:          "its new upstream version could not be prepared",
}

// ReasonText explains a hold reason in words. An unknown reason, for
// example one a newer publisher introduced, is quoted literally instead of
// failing the notes or the summary.
func ReasonText(reason string) string {
	if text, ok := reasonTexts[reason]; ok {
		return text
	}

	return "held (" + codeSpan(reason) + ")"
}

func sortHeld(held []Held) []Held {
	out := slices.Clone(held)
	slices.SortFunc(out, func(a, b Held) int { return cmp.Or(cmp.Compare(a.ID, b.ID), cmp.Compare(a.Reason, b.Reason)) })

	return out
}
