package notes_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/policy"
	"github.com/ovineko/schepherd/internal/publisher/prepare"
	"github.com/ovineko/schepherd/tools/catalogbot/internal/notes"
)

func detection(url string, verdict policy.Verdict, reason string) policy.Detection {
	return policy.Detection{URL: url, Verdict: verdict, Reason: reason}
}

// sampleReport has two included records, one of them reused, one record
// an exclude rule removes, five pending review (two for an unsupported
// host, one whose dependency's license is not permissive, one held by a
// review rule, one no rule matched without detection) and two failed, one
// of them a published schema that is held.
func sampleReport() *prepare.Report {
	const base = "https://schemas.example/"

	return &prepare.Report{
		FormatVersion: prepare.FormatVersion, GeneratedAt: "2026-09-28T03:00:00Z",
		Source: prepare.Source{Kind: prepare.KindSchemaStore, Commit: commit, TarballDigest: "sha256:" + strings.Repeat("d", 64)},
		Totals: prepare.Totals{Records: 10, Entries: 2, Reused: 1, Held: 1, Included: 2, Excluded: 1, PendingReview: 5, Failed: 2},
		Records: []prepare.RecordReport{
			{Name: "Alpha", URL: base + "alpha.json", Status: prepare.StatusIncluded, ID: "alpha", Reused: true},
			{Name: "Gamma", URL: base + "gamma.json", Status: prepare.StatusIncluded, ID: "gamma"},
			{Name: "Taken down", URL: base + "down.json", Status: prepare.StatusExcluded, Rule: "takedown"},
			{
				Name: "Elsewhere", URL: "https://elsewhere.example/a.json", Status: prepare.StatusPendingReview,
				LicenseDetections: []policy.Detection{detection("https://elsewhere.example/a.json", policy.Review, policy.RefusedUnsupportedHost)},
			},
			{
				Name: "Elsewhere too", URL: "https://elsewhere.example/b.json", Status: prepare.StatusPendingReview,
				LicenseDetections: []policy.Detection{detection("https://elsewhere.example/b.json", policy.Review, policy.RefusedUnsupportedHost)},
			},
			{
				Name: "Copyleft dependency", URL: base + "gpl.json", Status: prepare.StatusPendingReview,
				LicenseDetections: []policy.Detection{
					detection(base+"gpl.json", policy.Allow, ""), detection(base+"dep.json", policy.Review, policy.RefusedNotPermissive),
				},
			},
			{Name: "Reviewed by hand", URL: base + "hand.json", Status: prepare.StatusPendingReview, Rule: "ask-legal"},
			{Name: "Nobody knows", URL: base + "unknown.json", Status: prepare.StatusPendingReview},
			{Name: "Gone `@owner` #1", URL: base + "gone.json", Status: prepare.StatusFailed, Reason: prepare.ReasonFetchFailed},
			{Name: "Beta", URL: base + "beta.json", Status: prepare.StatusFailed, Reason: "unsupported-dialect", ID: "beta", Held: "prepare-failed"},
		},
		Collisions: []prepare.Collision{}, Held: []prepare.Hold{{ID: "beta", Reason: "prepare-failed"}}, Excluded: []prepare.Exclusion{},
	}
}

func writeReport(t *testing.T, report any) string {
	t.Helper()

	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}

	return writeDiff(t, string(data))
}

func loadSample(t *testing.T) *notes.Coverage {
	t.Helper()

	cov, err := notes.LoadCoverage(writeReport(t, sampleReport()))
	if err != nil {
		t.Fatal(err)
	}

	return cov
}

func TestLoadCoverage(t *testing.T) {
	cov := loadSample(t)

	want := map[string]int{policy.RefusedUnsupportedHost: 2, policy.RefusedNotPermissive: 1, "review rule": 1, "no license rule matched": 1}
	if !maps.Equal(cov.Pending, want) || len(cov.Failed) != 2 || cov.Failed[1].ID != "beta" || cov.Totals.Records != 10 || cov.Source.Commit != commit {
		t.Errorf("coverage = %+v", cov)
	}

	for name, change := range map[string]func(r *prepare.Report){
		"format version":  func(r *prepare.Report) { r.FormatVersion++ },
		"records total":   func(r *prepare.Report) { r.Totals.Records++ },
		"pending total":   func(r *prepare.Report) { r.Totals.PendingReview-- },
		"failed total":    func(r *prepare.Report) { r.Totals.Failed = 0 },
		"included total":  func(r *prepare.Report) { r.Totals.Included = 3 },
		"excluded total":  func(r *prepare.Report) { r.Totals.Excluded = 0 },
		"unknown status":  func(r *prepare.Report) { r.Records[0].Status = "skipped" },
		"record left out": func(r *prepare.Report) { r.Records = r.Records[:9] },
	} {
		r := sampleReport()
		change(r)

		if _, err := notes.LoadCoverage(writeReport(t, r)); fault.KindOf(err) != fault.Usage {
			t.Errorf("%s: %v", name, err)
		}
	}

	var loose map[string]any
	if err := json.Unmarshal([]byte(readFile(t, writeReport(t, sampleReport()))), &loose); err != nil {
		t.Fatal(err)
	}

	loose["pendingIDs"] = []string{}

	for name, path := range map[string]string{
		"unknown member": writeReport(t, loose),
		"malformed":      writeDiff(t, "{"),
		"missing":        filepath.Join(t.TempDir(), "missing.json"),
	} {
		if _, err := notes.LoadCoverage(path); fault.KindOf(err) != fault.Usage {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

// Every run shows what the preparation made of every upstream record, so a
// record that newly fails or waits for review is never skipped unnoticed.
func TestRunSummaryShowsTheCoverageOfThePreparation(t *testing.T) {
	d, err := notes.LoadDiff(writeDiff(t, `{"hasChanges":false,"added":[],"changed":[],"metadataChanged":[],`+
		`"held":[{"id":"beta","reason":"prepare-failed"}],"excluded":[],"unchanged":2}`))
	if err != nil {
		t.Fatal(err)
	}

	got, err := notes.RunSummary(d, loadSample(t), checkedAt, commit, true)
	if err != nil {
		t.Fatal(err)
	}

	want := "The catalog did not change: nothing is published, committed or tagged. Held schemas alone do not make a new revision.\n" +
		"\n" +
		"Upstream coverage: 10 records, 2 included (catalog entries: 2, reused: 1), 1 excluded by a rule, " +
		"5 pending review, 2 failed.\n" +
		"\n" +
		"### Held (1)\n" +
		"\n" +
		"Kept at their last published version:\n" +
		"\n" +
		"- `beta`: its new upstream version could not be prepared\n" +
		"\n" +
		"### Pending review (5)\n" +
		"\n" +
		"No license rule or detection allows these upstream records; they stay out of the catalog until a maintainer decides in " +
		"`sources/licenses.toml`. By reason:\n" +
		"\n" +
		"- `unsupported-host`: 2 (not hosted where license detection can pin it)\n" +
		"- `no license rule matched`: 1\n" +
		"- `not-permissive`: 1 (the detected license is not on the permissive allowlist)\n" +
		"- `review rule`: 1 (a rule in `sources/licenses.toml` holds them)\n" +
		"\n" +
		"### Failed (2)\n" +
		"\n" +
		"These upstream records could not be prepared and stay out of the catalog; a published one is held instead. " +
		"By reason: `fetch-failed` 1, `unsupported-dialect` 1.\n" +
		"\n" +
		"- ``Gone `@owner` #1`` `https://schemas.example/gone.json`: `fetch-failed`\n" +
		"- `Beta` `https://schemas.example/beta.json`: `unsupported-dialect` (published as `beta`, held)\n"

	if !strings.HasSuffix(string(got), want) {
		t.Errorf("summary:\n%s\nwant it to end with:\n%s", got, want)
	}

	if _, err := notes.RunSummary(d, nil, checkedAt, commit, true); fault.KindOf(err) != fault.Usage {
		t.Errorf("without a report: %v", err)
	}

	other := sampleReport()
	other.Source.Commit = strings.Repeat("f", 40)

	cov, err := notes.LoadCoverage(writeReport(t, other))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := notes.RunSummary(d, cov, checkedAt, commit, true); fault.KindOf(err) != fault.Usage {
		t.Errorf("a report of another upstream commit: %v", err)
	}
}

// Held schemas and failed records both overflow; the summary still stays
// within its budget and shows every section with its count.
func TestRunSummaryKeepsSeveralLongSectionsWithinItsBudget(t *testing.T) {
	d := &notes.Diff{HasChanges: false, Added: []string{}, Changed: []string{}, MetadataChanged: []string{}, Excluded: []string{}}
	report := sampleReport()

	for i := range 3000 {
		id := fmt.Sprintf("schema-%04d-%s", i, strings.Repeat("x", 40))
		d.Held = append(d.Held, notes.Held{ID: id, Reason: "fetch-failed"})
		d.Excluded = append(d.Excluded, id+"-excluded")
		report.Records = append(report.Records, prepare.RecordReport{
			Name: id, URL: "https://schemas.example/" + id + ".json", Status: prepare.StatusFailed, Reason: "invalid-json",
		})
	}

	report.Totals.Records += 3000
	report.Totals.Failed += 3000

	cov, err := notes.LoadCoverage(writeReport(t, report))
	if err != nil {
		t.Fatal(err)
	}

	got, err := notes.RunSummary(d, cov, checkedAt, commit, true)
	if err != nil {
		t.Fatal(err)
	}

	text := string(got)
	if len(got) > notes.MaxBytes || strings.Count(text, " more not listed here\n") != 3 {
		t.Errorf("summary of %d bytes:\n%s", len(got), text[max(0, len(text)-400):])
	}

	for _, heading := range []string{"### Held (3000)", "### Excluded (3000)", "### Pending review (5)", "### Failed (3002)", "- `review rule`: 1"} {
		if !strings.Contains(text, heading) {
			t.Errorf("summary lacks %q", heading)
		}
	}
}
