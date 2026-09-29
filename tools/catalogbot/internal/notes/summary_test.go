package notes_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/prepare"
	"github.com/ovineko/schepherd/internal/publisher/state"
	"github.com/ovineko/schepherd/tools/catalogbot/internal/notes"
)

var checkedAt = time.Date(2026, 9, 28, 3, 0, 12, 0, time.UTC)

func TestReasonText(t *testing.T) {
	seen := map[string]string{}

	for _, reason := range state.HeldReasons {
		text := notes.ReasonText(reason)
		if text == "" || strings.Contains(text, "`") || strings.HasPrefix(text, "held (") {
			t.Errorf("%s has no text of its own: %q", reason, text)
		}

		if other, dup := seen[text]; dup {
			t.Errorf("%s and %s read the same: %q", reason, other, text)
		}

		seen[text] = reason
	}

	// Only a removal upstream is ever called one.
	for text, reason := range seen {
		if reason != state.HeldRemovedUpstream && strings.Contains(text, "removed") {
			t.Errorf("%s reads %q", reason, text)
		}
	}

	if got := notes.ReasonText("new-reason `x`"); got != "held (`` new-reason `x` ``)" {
		t.Errorf("an unknown reason reads %q", got)
	}
}

func writeDiff(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "diff.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

const fullDiff = `{"hasChanges":true,"added":["new"],"changed":["beta","alpha"],"metadataChanged":["meta"],` +
	`"held":[{"id":"zeta","reason":"removed-upstream"},{"id":"eta","reason":"license-detection-failed"}],` +
	`"excluded":["taken-down"],"unchanged":700}`

func TestLoadDiff(t *testing.T) {
	d, err := notes.LoadDiff(writeDiff(t, fullDiff))
	if err != nil {
		t.Fatal(err)
	}

	if !d.HasChanges || len(d.Changed) != 2 || d.MetadataChanged[0] != "meta" || d.Held[1] != (notes.Held{ID: "eta", Reason: state.HeldLicenseDetectionFailed}) ||
		d.Excluded[0] != "taken-down" || d.Unchanged != 700 {
		t.Errorf("diff = %+v", d)
	}

	for name, body := range map[string]string{
		"no hasChanges":   `{"added":[],"changed":[],"metadataChanged":[],"held":[],"excluded":[],"unchanged":0}`,
		"no held":         `{"hasChanges":false,"added":[],"changed":[],"metadataChanged":[],"excluded":[],"unchanged":0}`,
		"null held":       `{"hasChanges":false,"added":[],"changed":[],"metadataChanged":[],"held":null,"excluded":[],"unchanged":0}`,
		"old format":      `{"revisionHint":null,"added":[],"changed_ids":[],"removedUpstream":[],"unchanged":0,"changed":false}`,
		"extra member":    `{"hasChanges":false,"added":[],"changed":[],"metadataChanged":[],"held":[],"excluded":[],"unchanged":0,"revisionHint":null}`,
		"held as strings": `{"hasChanges":false,"added":[],"changed":[],"metadataChanged":[],"held":["x"],"excluded":[],"unchanged":0}`,
		"negative":        `{"hasChanges":false,"added":[],"changed":[],"metadataChanged":[],"held":[],"excluded":[],"unchanged":-1}`,
		"malformed":       `{`,
	} {
		if _, err := notes.LoadDiff(writeDiff(t, body)); fault.KindOf(err) != fault.Usage {
			t.Errorf("%s: %v", name, err)
		}
	}

	if _, err := notes.LoadDiff(filepath.Join(t.TempDir(), "missing.json")); fault.KindOf(err) != fault.Usage {
		t.Errorf("a missing file: %v", err)
	}
}

// allIncluded is the coverage of a preparation that included its three
// upstream records.
func allIncluded(t *testing.T) *notes.Coverage {
	t.Helper()

	r := sampleReport()
	r.Records = r.Records[:2]
	r.Records = append(r.Records, prepare.RecordReport{Name: "Delta", URL: "https://schemas.example/delta.json", Status: prepare.StatusIncluded, ID: "delta"})
	r.Totals = prepare.Totals{Records: 3, Entries: 3, Included: 3}

	cov, err := notes.LoadCoverage(writeReport(t, r))
	if err != nil {
		t.Fatal(err)
	}

	return cov
}

func TestRunSummary(t *testing.T) {
	d, err := notes.LoadDiff(writeDiff(t, fullDiff))
	if err != nil {
		t.Fatal(err)
	}

	got, err := notes.RunSummary(d, allIncluded(t), checkedAt, commit, true)
	if err != nil {
		t.Fatal(err)
	}

	want := "## Schema catalog update\n" +
		"\n" +
		"Checked at 2026-09-28T03:00:12Z against upstream commit `" + commit + "`: " +
		"1 added, 2 changed, 1 metadata updated, 1 excluded, 700 unchanged (2 of them held).\n" +
		"\n" +
		"The catalog changed, so this run publishes a new revision.\n" +
		"\n" +
		"Upstream coverage: 3 records, 3 included (catalog entries: 3, reused: 0), 0 excluded by a rule, " +
		"0 pending review, 0 failed.\n" +
		"\n" +
		"### Held (2)\n" +
		"\n" +
		"Kept at their last published version:\n" +
		"\n" +
		"- `eta`: its license could not be detected\n" +
		"- `zeta`: upstream removed it\n" +
		"\n" +
		"### Excluded (1)\n" +
		"\n" +
		"Removed from the catalog by an explicit exclude rule in `sources/licenses.toml`:\n" +
		"\n" +
		"- `taken-down`\n"

	if string(got) != want {
		t.Errorf("summary:\n%s\nwant:\n%s", got, want)
	}
}

// A run that does not publish, one that is not on main, only reports what
// would change.
func TestRunSummaryWithoutPublication(t *testing.T) {
	d, err := notes.LoadDiff(writeDiff(t, fullDiff))
	if err != nil {
		t.Fatal(err)
	}

	got, err := notes.RunSummary(d, allIncluded(t), checkedAt, commit, false)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(got), "700 unchanged (2 of them held).\n\nThe catalog changed, but publication is not enabled for this run: "+
		"nothing is published, committed or tagged.\n\nUpstream coverage: ") || strings.Contains(string(got), "publishes a new revision") {
		t.Errorf("summary:\n%s", got)
	}
}

// A run without changes still reports when it checked, what, and which
// schemas it holds.
func TestRunSummaryWithoutChanges(t *testing.T) {
	held, err := notes.LoadDiff(writeDiff(t,
		`{"hasChanges":false,"added":[],"changed":[],"metadataChanged":[],"held":[{"id":"eta","reason":"fetch-failed"}],"excluded":[],"unchanged":9}`))
	if err != nil {
		t.Fatal(err)
	}

	got, err := notes.RunSummary(held, allIncluded(t), checkedAt, commit, true)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"Checked at 2026-09-28T03:00:12Z against upstream commit `" + commit + "`: 0 added, 0 changed, 0 metadata updated, 0 excluded, 9 unchanged (1 of them held).",
		"nothing is published, committed or tagged. Held schemas alone do not make a new revision.",
		"### Held (1)", "- `eta`: its source could not be fetched\n",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}

	quiet, err := notes.LoadDiff(writeDiff(t, `{"hasChanges":false,"added":[],"changed":[],"metadataChanged":[],"held":[],"excluded":[],"unchanged":9}`))
	if err != nil {
		t.Fatal(err)
	}

	got, err = notes.RunSummary(quiet, allIncluded(t), checkedAt, commit, true)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasSuffix(string(got), "9 unchanged (0 of them held).\n\nThe catalog did not change: nothing is published, committed or tagged.\n\n"+
		"Upstream coverage: 3 records, 3 included (catalog entries: 3, reused: 0), 0 excluded by a rule, "+
		"0 pending review, 0 failed.\n") {
		t.Errorf("summary:\n%s", got)
	}

	if _, err := notes.RunSummary(quiet, allIncluded(t), checkedAt, "HEAD", true); fault.KindOf(err) != fault.Usage {
		t.Errorf("an invalid upstream commit: %v", err)
	}
}

func TestRunSummaryKeepsWithinItsBudget(t *testing.T) {
	d := &notes.Diff{HasChanges: false, Added: []string{}, Changed: []string{}, MetadataChanged: []string{}, Excluded: []string{}}
	for i := range 5000 {
		d.Held = append(d.Held, notes.Held{ID: fmt.Sprintf("schema-%04d-%s", i, strings.Repeat("x", 40)), Reason: state.HeldLicenseDetectionFailed})
	}

	got, err := notes.RunSummary(d, allIncluded(t), checkedAt, commit, true)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) > notes.MaxBytes || !strings.Contains(string(got), "### Held (5000)") || !strings.Contains(string(got), " more not listed here\n") {
		t.Errorf("summary of %d bytes ends with %q", len(got), got[max(0, len(got)-200):])
	}
}

func TestFinishSummary(t *testing.T) {
	got, err := notes.FinishSummary(sampleState(t), []string{"tag catalog-" + rev, "release catalog-" + rev}, checkedAt)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"Checked at 2026-09-28T03:00:12Z: revision `20260924.0905` is recorded but not finished " +
			"(missing: `tag catalog-20260924.0905`, `release catalog-20260924.0905`). This run finishes it from the recorded state",
		"Revision `20260924.0905` was prepared from upstream commit `" + commit + "`: " +
			"1 added, 1 changed, 1 metadata updated, 1 excluded, 3 unchanged (2 of them held).\n",
		"### Held (2)", "- `eta`: its license could not be detected\n- `zeta`: upstream removed it\n",
		"### Excluded (1)", "- `iota`\n",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}

	if _, err := notes.FinishSummary(nil, nil, checkedAt); fault.KindOf(err) != fault.Usage {
		t.Errorf("no state: %v", err)
	}
}
