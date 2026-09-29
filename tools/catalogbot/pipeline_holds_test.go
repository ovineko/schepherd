package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/mirror"
	"github.com/ovineko/schepherd/internal/publisher/policy"
	"github.com/ovineko/schepherd/internal/publisher/prepare"
	"github.com/ovineko/schepherd/internal/publisher/publish"
	"github.com/ovineko/schepherd/internal/publisher/state"
	"github.com/ovineko/schepherd/internal/registry"
	"github.com/ovineko/schepherd/internal/testutil/ociregistry"
)

const secondCommit = "2222222222222222222222222222222222222222"

// publishFirstWeek records, tags and releases alpha, beta and gamma and
// returns the path of the recorded state.
func (p *pipeline) publishFirstWeek(t *testing.T, at time.Time) string {
	t.Helper()

	head := p.gh.Head("main")
	commit, _ := p.publishJob(t, jobRun{
		prepared: p.preparedSet(t, week{commit: firstUpstream, schemas: weekSchemas(1)}), base: p.stateAt(t, head),
		upstream: firstUpstream, expected: head,
	}, at)

	return p.stateAt(t, commit)
}

// labelledWeek renames alpha without touching its content, holds beta
// because its license could not be detected and excludes gamma by rule.
func labelledWeek(base *state.State) week {
	schemas := weekSchemas(1)
	delete(schemas, "beta")
	delete(schemas, "gamma")

	return week{
		base: base, commit: secondCommit, schemas: schemas, names: map[string]string{"alpha": "Alpha renamed"},
		held: map[string]string{"beta": state.HeldLicenseDetectionFailed}, excluded: []string{"gamma"},
	}
}

// TestPublishJobLabelsMetadataHoldsAndExclusions follows a week whose
// changes are a metadata-only update, a hold and an exclusion: the notes
// file each under its own section, the hold with its reason, and never
// call the exclusion a removal upstream.
func TestPublishJobLabelsMetadataHoldsAndExclusions(t *testing.T) {
	p := newPipeline(t)
	at := time.Date(2026, 9, 28, 3, 0, 0, 0, time.UTC)
	base := p.publishFirstWeek(t, at)

	head := p.gh.Head("main")
	commit, rev := p.publishJob(t, jobRun{
		prepared: p.preparedSet(t, labelledWeek(p.loadState(t, base))), base: base,
		upstream: secondCommit, expected: head,
	}, at.Add(7*24*time.Hour))

	st := p.loadState(t, p.stateAt(t, commit))

	alpha, _ := st.Lookup("alpha")
	beta, _ := st.Lookup("beta")
	gamma, _ := st.Lookup("gamma")

	if alpha.LastChangedRevision != rev || alpha.ArtifactRevision == "" || beta.HeldSinceRevision != rev ||
		beta.HeldReason != state.HeldLicenseDetectionFailed || gamma.ExcludedRevision != rev {
		t.Fatalf("recorded state: alpha %+v, beta %+v, gamma %+v", alpha, beta, gamma)
	}

	rel, _ := p.gh.ReleaseOf("catalog-" + rev)
	for _, want := range []string{
		"0 added, 0 changed, 1 metadata updated, 1 excluded, 1 unchanged (1 of them held).",
		"## Metadata updated (1)", "- `alpha` `Alpha renamed`\n",
		"## Held (1)", "- `beta` `Schema beta`: its license could not be detected (since `" + rev + "`)\n",
		"## Excluded (1)", "An explicit exclude rule in `sources/licenses.toml`", "- `gamma` `Schema gamma`\n",
	} {
		if !strings.Contains(rel.Body, want) {
			t.Errorf("the release lacks %q:\n%s", want, rel.Body)
		}
	}

	if strings.Contains(rel.Body, "upstream removed") || strings.Contains(strings.ToLower(rel.Body), "removed upstream") || strings.Contains(rel.Body, "## Changed") {
		t.Errorf("the release mislabels a change:\n%s", rel.Body)
	}
}

// reportOf writes the report.json of the preparation of set: every entry
// comes from an included record, and one more record waits for review
// because its host is not supported and one failed.
func reportOf(t *testing.T, set *prepare.Set) string {
	t.Helper()

	report := prepare.Report{
		FormatVersion: prepare.FormatVersion, GeneratedAt: "2026-10-05T03:00:00Z", Source: set.Document.Source,
		Collisions: []prepare.Collision{}, Held: set.Document.Held, Excluded: set.Document.Excluded,
	}

	reused := 0

	for _, e := range set.Document.Entries {
		report.Records = append(report.Records, prepare.RecordReport{
			Name: e.Name, URL: e.Provenance.Source, Status: prepare.StatusIncluded, ID: e.ID, Reused: e.Reused != nil,
		})

		if e.Reused != nil {
			reused++
		}
	}

	report.Records = append(report.Records,
		prepare.RecordReport{
			Name: "Elsewhere", URL: "https://elsewhere.example/a.json", Status: prepare.StatusPendingReview,
			LicenseDetections: []policy.Detection{{URL: "https://elsewhere.example/a.json", Verdict: policy.Review, Reason: policy.RefusedUnsupportedHost}},
		},
		prepare.RecordReport{Name: "Broken", URL: "https://json.schemastore.org/broken.json", Status: prepare.StatusFailed, Reason: "invalid-json"},
	)

	n := len(set.Document.Entries)
	report.Totals = prepare.Totals{Records: n + 2, Entries: n, Reused: reused, Included: n, PendingReview: 1, Failed: 1, Held: len(set.Document.Held)}

	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}

	return writeFile(t, t.TempDir(), "report.json", string(data))
}

// summaryOf renders the job summary of a run from the diff the publisher
// computes for the prepared set against the recorded state, exactly as
// `schepherd-publisher diff --json` writes it, and the report of the
// preparation.
func (p *pipeline) summaryOf(t *testing.T, prepared, recorded string) (publish.Diff, string) {
	t.Helper()

	set, err := prepare.Load(prepared)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := publish.NewPlan(set, p.loadState(t, recorded))
	if err != nil {
		t.Fatal(err)
	}

	d := plan.Diff()

	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	diffFile := writeFile(t, t.TempDir(), "diff.json", string(data)+"\n")

	code, stdout, stderr := invoke(t, p.lookup, "summary", "--diff", diffFile, "--report", reportOf(t, set), "--upstream", secondCommit,
		"--checked-at", "2026-10-05T03:00:09Z")
	if code != 0 {
		t.Fatalf("catalogbot summary exited %d: %s", code, stderr)
	}

	return d, stdout
}

// TestRunSummaryReadsTheDiff keeps the summary in step with the diff the
// publisher writes: every change is counted, and held schemas are listed
// with their reason also when they alone do not make a revision.
func TestRunSummaryReadsTheDiff(t *testing.T) {
	p := newPipeline(t)
	base := p.publishFirstWeek(t, time.Date(2026, 9, 28, 3, 0, 0, 0, time.UTC))

	d, summary := p.summaryOf(t, p.preparedSet(t, labelledWeek(p.loadState(t, base))), base)
	if !d.HasChanges {
		t.Fatalf("diff %+v", d)
	}

	for _, want := range []string{
		"Checked at 2026-10-05T03:00:09Z against upstream commit `" + secondCommit + "`: " +
			"0 added, 0 changed, 1 metadata updated, 1 excluded, 1 unchanged (1 of them held).",
		"this run publishes a new revision", "- `beta`: its license could not be detected\n", "### Excluded (1)", "- `gamma`\n",
		"Upstream coverage: 3 records, 1 included (catalog entries: 1, reused: 1), 0 excluded by a rule, " +
			"1 pending review, 1 failed.",
		"### Pending review (1)\n", "- `Broken` `https://json.schemastore.org/broken.json`: `invalid-json`\n",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, summary)
		}
	}

	// Only holds: upstream no longer lists gamma and beta's source failed.
	onlyHolds := week{
		base: p.loadState(t, base), commit: secondCommit, schemas: map[string]string{"alpha": weekSchemas(1)["alpha"]},
		held: map[string]string{"beta": state.HeldFetchFailed, "gamma": state.HeldRemovedUpstream},
	}

	d, summary = p.summaryOf(t, p.preparedSet(t, onlyHolds), base)
	if d.HasChanges {
		t.Fatalf("holds alone made a revision: %+v", d)
	}

	for _, want := range []string{
		"0 added, 0 changed, 0 metadata updated, 0 excluded, 3 unchanged (2 of them held).",
		"Held schemas alone do not make a new revision.",
		"- `beta`: its source could not be fetched\n- `gamma`: upstream removed it\n",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, summary)
		}
	}
}

func openRepo(t *testing.T, reg *ociregistry.Registry) *registry.Repo {
	t.Helper()

	name, err := registry.ParseRepository(reg.Host() + "/test/schemas")
	if err != nil {
		t.Fatal(err)
	}

	repo, err := registry.NewClient(registry.Options{Hosts: map[string]registry.HostConfig{reg.Host(): {PlainHTTP: true}}}).Open(name)
	if err != nil {
		t.Fatal(err)
	}

	return repo
}

// TestTestJobPublishesIntoAMirrorOfTheRecordedCatalog follows the test job
// of update-schemas.yml: reused and held entries keep artifacts that only
// the real registry holds, so the prepared set is published against the
// recorded state into a mirror of the recorded catalog. Neither an empty
// registry nor a publication without the state would do.
func TestTestJobPublishesIntoAMirrorOfTheRecordedCatalog(t *testing.T) {
	p := newPipeline(t)
	at := time.Date(2026, 9, 28, 3, 0, 0, 0, time.UTC)
	recorded := p.loadState(t, p.publishFirstWeek(t, at))
	prepared := p.preparedSet(t, secondWeek(recorded, secondCommit))
	later := at.Add(7 * 24 * time.Hour)

	empty := openRepo(t, ociregistry.New(t))

	_, err := publish.Run(t.Context(), empty, publish.Options{Now: later, PreparedDir: prepared, Repository: empty.Name().String()})
	if fault.KindOf(err) != fault.Usage {
		t.Errorf("publishing reused and held entries without the state: %v", err)
	}

	_, err = publish.Run(t.Context(), empty, publish.Options{Now: later, State: recorded, PreparedDir: prepared, Repository: empty.Name().String()})
	if fault.KindOf(err) != fault.Integrity || !strings.Contains(fmt.Sprint(err), "is missing from") {
		t.Errorf("publishing against the state into an empty registry: %v", err)
	}

	local := openRepo(t, ociregistry.New(t))
	if _, err := mirror.Run(t.Context(), p.repo, local, recorded.Catalog.Digest, mirror.Options{
		ArtifactLimits: artifact.DefaultLimits(), CatalogLimits: catalog.DefaultLimits(),
	}); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "state-1.json")

	res, err := publish.Run(t.Context(), local, publish.Options{
		Now: later, State: recorded, StateOut: out, PreparedDir: prepared, Repository: local.Name().String(), UpdateLatest: true,
	})
	if err != nil || res.Status != publish.StatusPublished || res.ReusedSchemas != 2 || res.UploadedSchemas != 2 {
		t.Fatalf("publishing into the mirror: %+v, %v", res, err)
	}

	again, err := publish.Run(t.Context(), local, publish.Options{
		Now: later.Add(time.Minute), State: p.loadState(t, out), PreparedDir: prepared, Repository: local.Name().String(),
	})
	if err != nil || again.Status != publish.StatusNoop {
		t.Errorf("republishing: %+v, %v", again, err)
	}
}

// TestTestJobRepublishesAnExclusionWeekAsANoop follows the test job of
// update-schemas.yml for a week with an exclusion, a hold and a
// metadata-only change: after the first publication into the mirror,
// publishing the same prepared set against the state it wrote must be a
// noop that writes the same state, diff against that state must find no
// change, and publishing against the recorded state again must resume the
// revision. A takedown must never fail the weekly job.
func TestTestJobRepublishesAnExclusionWeekAsANoop(t *testing.T) {
	p := newPipeline(t)
	at := time.Date(2026, 9, 28, 3, 0, 0, 0, time.UTC)
	recorded := p.loadState(t, p.publishFirstWeek(t, at))
	prepared := p.preparedSet(t, labelledWeek(recorded))
	later := at.Add(7 * 24 * time.Hour)

	local := openRepo(t, ociregistry.New(t))
	if _, err := mirror.Run(t.Context(), p.repo, local, recorded.Catalog.Digest, mirror.Options{
		ArtifactLimits: artifact.DefaultLimits(), CatalogLimits: catalog.DefaultLimits(),
	}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	first, second, third := filepath.Join(dir, "state-1.json"), filepath.Join(dir, "state-2.json"), filepath.Join(dir, "state-3.json")

	res, err := publish.Run(t.Context(), local, publish.Options{
		Now: later, State: recorded, StateOut: first, PreparedDir: prepared, Repository: local.Name().String(), UpdateLatest: true,
	})
	if err != nil || res.Status != publish.StatusPublished || len(res.Excluded) != 1 {
		t.Fatalf("first publication: %+v, %v", res, err)
	}

	again, err := publish.Run(t.Context(), local, publish.Options{
		Now: later.Add(time.Minute), State: p.loadState(t, first), StateOut: second, PreparedDir: prepared, Repository: local.Name().String(),
	})
	if err != nil || again.Status != publish.StatusNoop || readText(t, first) != readText(t, second) {
		t.Fatalf("republishing against the written state: %+v, %v", again, err)
	}

	set, err := prepare.Load(prepared)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := publish.NewPlan(set, p.loadState(t, first))
	if err != nil || plan.HasChanges() {
		t.Fatalf("diff against the written state: %+v, %v", plan, err)
	}

	resumed, err := publish.Run(t.Context(), local, publish.Options{
		Now: later.Add(2 * time.Minute), State: recorded, StateOut: third, PreparedDir: prepared, Repository: local.Name().String(), UpdateLatest: true,
	})
	if err != nil || resumed.Status != publish.StatusResumed || resumed.Revision != res.Revision || readText(t, first) != readText(t, third) {
		t.Fatalf("resuming against the recorded state: %+v, %v", resumed, err)
	}
}
