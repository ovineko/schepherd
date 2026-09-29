package notes

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/policy"
	"github.com/ovineko/schepherd/internal/publisher/prepare"
)

const maxReportBytes = 64 << 20

// Pending-review reasons that are not a code of automatic license
// detection (policy.Refused*).
const (
	pendingByRule = "review rule"
	pendingNoRule = "no license rule matched"
)

var detectionTexts = map[string]string{
	policy.RefusedNoLicense:       "no license file that could travel with the schema",
	policy.RefusedNotPermissive:   "the detected license is not on the permissive allowlist",
	policy.RefusedNotAsserted:     "the detected value names no license",
	policy.RefusedUnsupportedHost: "not hosted where license detection can pin it",
	policy.RefusedFetchFailed:     "a license detection request failed",
}

// Coverage is what a run made of every upstream record, from the report.json
// of `schepherd-publisher prepare`: the totals, the records held for review
// counted by reason, and the records that failed.
type Coverage struct {
	Pending map[string]int
	Source  prepare.Source
	Failed  []prepare.RecordReport
	Totals  prepare.Totals
}

// LoadCoverage reads a prepare report. The report must be in the format of
// this build and its records must add up to its totals, so a changed or
// inconsistent report fails the run instead of misreporting what upstream
// records were left out.
func LoadCoverage(path string) (*Coverage, error) {
	data, err := readLimited(path, maxReportBytes, "prepare report")
	if err != nil {
		return nil, err
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var report prepare.Report
	if err := dec.Decode(&report); err != nil {
		return nil, fault.Wrap(fault.Usage, err, "prepare report %s", path)
	}

	if report.FormatVersion != prepare.FormatVersion {
		return nil, fault.New(fault.Usage, "the prepare report %s has formatVersion %d; this build reads %d", path, report.FormatVersion, prepare.FormatVersion)
	}

	cov := &Coverage{Totals: report.Totals, Source: report.Source, Pending: map[string]int{}, Failed: []prepare.RecordReport{}}
	counted := prepare.Totals{Records: len(report.Records)}

	for i := range report.Records {
		row := &report.Records[i]

		switch row.Status {
		case prepare.StatusIncluded:
			counted.Included++
		case prepare.StatusExcluded:
			counted.Excluded++
		case prepare.StatusPendingReview:
			counted.PendingReview++
			cov.Pending[pendingReason(row)]++
		case prepare.StatusFailed:
			counted.Failed++
			cov.Failed = append(cov.Failed, *row)
		default:
			return nil, fault.New(fault.Usage, "the prepare report %s has a record with the unknown status %q", path, row.Status)
		}
	}

	t := report.Totals
	if counted.Records != t.Records || counted.Included != t.Included || counted.Excluded != t.Excluded ||
		counted.PendingReview != t.PendingReview || counted.Failed != t.Failed {
		return nil, fault.New(fault.Usage, "the records of the prepare report %s do not add up to its totals", path)
	}

	return cov, nil
}

// pendingReason is what holds a record for review: the rule that decided,
// else the code of the license detection that refused it, else that no rule
// matched while automatic detection did not run.
func pendingReason(row *prepare.RecordReport) string {
	if row.Rule != "" {
		return pendingByRule
	}

	for _, d := range row.LicenseDetections {
		if d.Verdict == policy.Review && d.Reason != "" {
			return d.Reason
		}
	}

	return pendingNoRule
}

// counts sums up the upstream records in words.
func (c *Coverage) counts() string {
	t := c.Totals

	return fmt.Sprintf("%d records, %d included (catalog entries: %d, reused: %d), %d excluded by a rule, %d pending review, %d failed",
		t.Records, t.Included, t.Entries, t.Reused, t.Excluded, t.PendingReview, t.Failed)
}

// sections lists the pending-review records by reason and every failed
// record with its reason.
func (c *Coverage) sections() []section {
	reasons := slices.SortedFunc(maps.Keys(c.Pending), func(a, b string) int {
		return cmp.Or(cmp.Compare(c.Pending[b], c.Pending[a]), cmp.Compare(a, b))
	})

	pending := make([]string, 0, len(reasons))

	for _, reason := range reasons {
		line := fmt.Sprintf("- %s: %d", codeSpan(reason), c.Pending[reason])

		switch text, ok := detectionTexts[reason]; {
		case ok:
			line += " (" + text + ")"
		case reason == pendingByRule:
			line += " (a rule in `sources/licenses.toml` holds them)"
		}

		pending = append(pending, line)
	}

	failedBy := map[string]int{}
	failed := make([]string, 0, len(c.Failed))

	for i := range c.Failed {
		row := &c.Failed[i]
		failedBy[row.Reason]++

		line := "- " + codeSpan(row.Name) + " " + codeSpan(row.URL) + ": " + codeSpan(row.Reason)
		if row.ID != "" {
			line += " (published as " + codeSpan(row.ID) + ", held)"
		}

		failed = append(failed, line)
	}

	byReason := make([]string, 0, len(failedBy))
	for _, reason := range slices.Sorted(maps.Keys(failedBy)) {
		byReason = append(byReason, fmt.Sprintf("%s %d", codeSpan(reason), failedBy[reason]))
	}

	return []section{
		{
			title: "Pending review", lines: pending, count: c.Totals.PendingReview,
			intro: "No license rule or detection allows these upstream records; they stay out of the catalog until a maintainer " +
				"decides in `sources/licenses.toml`. By reason:",
		},
		{
			title: "Failed", lines: failed,
			intro: "These upstream records could not be prepared and stay out of the catalog; a published one is held instead. " +
				"By reason: " + strings.Join(byReason, ", ") + ".",
		},
	}
}
