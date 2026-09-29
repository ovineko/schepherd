package notes_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/state"
	"github.com/ovineko/schepherd/tools/catalogbot/internal/notes"
)

const (
	repository = "ghcr.io/ovineko/schepherd-schemas"
	rev        = "20260924.0905"
	earlier    = "20260917.0300"
	oldest     = "20260910.0300"
)

var (
	catalogDigest = "sha256:" + strings.Repeat("e", 64)
	commit        = "05b037b7a2b68ead6893c857db2594bcad9951d4"
)

// rec describes one schema record with the revisions of its state record:
// artifact is set when the last change touched metadata only, heldSince
// and heldReason for a schema kept at its last published version, excluded
// for one an exclude rule removed.
type rec struct {
	id, name, first, artifact, changed, heldSince, heldReason, excluded string
}

func buildState(t *testing.T, records ...rec) *state.State {
	t.Helper()

	st := &state.State{
		FormatVersion: state.FormatVersion,
		Source: state.Source{
			Kind: state.KindSchemaStore, Repository: state.SchemaStoreRepository, Commit: commit,
			TarballDigest: "sha256:" + strings.Repeat("d", 64),
		},
		Recipe:  "test-recipe",
		Catalog: state.Catalog{Revision: rev, Digest: catalogDigest, Size: 1234},
	}

	for i, r := range records {
		st.Schemas = append(st.Schemas, state.Schema{
			ID: r.id, ContentDigest: fmt.Sprintf("sha256:%064x", i+1), FirstRevision: r.first, ArtifactRevision: r.artifact,
			LastChangedRevision: r.changed, HeldSinceRevision: r.heldSince, HeldReason: r.heldReason, ExcludedRevision: r.excluded,
			Entry: catalog.Entry{
				ID: r.id, Name: r.name,
				Artifact:   catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: fmt.Sprintf("sha256:%064x", 1000+i), Size: 500},
				Provenance: &catalog.Provenance{Source: "https://json.schemastore.org/" + r.id + ".json", License: "Apache-2.0"},
			},
		})
	}

	if err := st.Validate(); err != nil {
		t.Fatal(err)
	}

	return st
}

func sampleState(t *testing.T) *state.State {
	t.Helper()

	return buildState(t,
		rec{id: "alpha", name: "Alpha config", first: rev, changed: rev},
		rec{id: "beta", name: "Beta `tick` @someone #12 <b>x</b> [link](https://evil.example)", first: earlier, changed: rev},
		rec{id: "delta", name: "Delta", first: oldest, changed: oldest, excluded: earlier},
		rec{id: "eta", name: "Eta", first: oldest, changed: oldest, heldSince: earlier, heldReason: state.HeldLicenseDetectionFailed},
		rec{id: "gamma", name: "Gamma", first: earlier, changed: earlier},
		rec{id: "iota", name: "Iota", first: oldest, changed: earlier, excluded: rev},
		rec{id: "kappa", name: "Kappa renamed", first: oldest, artifact: earlier, changed: rev},
		rec{id: "zeta", name: "Zeta", first: earlier, changed: earlier, heldSince: rev, heldReason: state.HeldRemovedUpstream},
	)
}

func sampleResult() *notes.Result {
	return &notes.Result{
		Status: notes.StatusPublished, Revision: rev, CatalogDigest: catalogDigest, CatalogSize: 1234,
		Added: []string{"alpha"}, Changed: []string{"beta"}, MetadataChanged: []string{"kappa"},
		Held:     []notes.Held{{ID: "eta", Reason: state.HeldLicenseDetectionFailed}, {ID: "zeta", Reason: state.HeldRemovedUpstream}},
		Excluded: []string{"iota"}, Unchanged: 3,
		UploadedSchemas: 2, ReusedSchemas: 2,
	}
}

func TestRender(t *testing.T) {
	got, err := notes.Render(sampleState(t), repository)
	if err != nil {
		t.Fatal(err)
	}

	want := "Catalog revision `20260924.0905` of `ghcr.io/ovineko/schepherd-schemas`: 1 added, 1 changed, 1 metadata updated, 1 excluded, 3 unchanged (2 of them held).\n" +
		"\n" +
		"- Catalog digest: `" + catalogDigest + "`\n" +
		"- Tag: `ghcr.io/ovineko/schepherd-schemas:catalog-20260924.0905`\n" +
		"- Upstream: [`SchemaStore/schemastore@05b037b7a2b6`](https://github.com/SchemaStore/schemastore/commit/" + commit + ")\n" +
		"\n" +
		"## How to pin\n" +
		"\n" +
		"Resolve the revision tag once and paste the printed `[catalog]` section into `schepherd.toml`:\n" +
		"\n" +
		"```sh\n" +
		"schepherd pin ghcr.io/ovineko/schepherd-schemas:catalog-20260924.0905\n" +
		"```\n" +
		"\n" +
		"or pin the digest directly:\n" +
		"\n" +
		"```toml\n" +
		"[catalog]\n" +
		"repository = \"ghcr.io/ovineko/schepherd-schemas\"\n" +
		"digest = \"" + catalogDigest + "\"\n" +
		"```\n" +
		"\n" +
		"## Added (1)\n" +
		"\n" +
		"- `alpha` `Alpha config`\n" +
		"\n" +
		"## Changed (1)\n" +
		"\n" +
		"- `beta` ``Beta `tick` @someone #12 <b>x</b> [link](https://evil.example)``\n" +
		"\n" +
		"## Metadata updated (1)\n" +
		"\n" +
		"Only the name, description, file patterns or dialect changed; the schema artifact stays the same.\n" +
		"\n" +
		"- `kappa` `Kappa renamed`\n" +
		"\n" +
		"## Held (2)\n" +
		"\n" +
		"These schemas stay in the catalog at their last published version until a later run can refresh them.\n" +
		"\n" +
		"- `eta` `Eta`: its license could not be detected (since `20260917.0300`)\n" +
		"- `zeta` `Zeta`: upstream removed it (since `20260924.0905`)\n" +
		"\n" +
		"## Excluded (1)\n" +
		"\n" +
		"An explicit exclude rule in `sources/licenses.toml` removed these schemas from the catalog; their IDs stay reserved.\n" +
		"\n" +
		"- `iota` `Iota`\n"

	if string(got) != want {
		t.Errorf("notes:\n%s\nwant:\n%s", got, want)
	}

	again, err := notes.Render(sampleState(t), repository)
	if err != nil || !bytes.Equal(got, again) {
		t.Error("the notes are not deterministic")
	}
}

func TestChangesOf(t *testing.T) {
	got := notes.ChangesOf(sampleState(t))
	want := notes.Changes{
		Added: []string{"alpha"}, Changed: []string{"beta"}, MetadataChanged: []string{"kappa"},
		Held:     []notes.Held{{ID: "eta", Reason: state.HeldLicenseDetectionFailed}, {ID: "zeta", Reason: state.HeldRemovedUpstream}},
		Excluded: []string{"iota"}, Unchanged: 3,
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("ChangesOf = %+v, want %+v", got, want)
	}

	// A schema held since an earlier revision is still held in this one.
	back := buildState(t,
		rec{id: "gone", name: "Gone", first: "20260910.0300", changed: "20260910.0300", heldSince: earlier, heldReason: state.HeldRemovedUpstream},
		rec{id: "returned", name: "Returned", first: earlier, changed: rev},
	)

	got = notes.ChangesOf(back)
	want = notes.Changes{
		Added: []string{}, Changed: []string{"returned"}, MetadataChanged: []string{},
		Held: []notes.Held{{ID: "gone", Reason: state.HeldRemovedUpstream}}, Excluded: []string{}, Unchanged: 1,
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("ChangesOf with an earlier hold = %+v, want %+v", got, want)
	}

	rendered, err := notes.Render(back, repository)
	if err != nil || !strings.Contains(string(rendered), "- `gone` `Gone`: upstream removed it (since `20260917.0300`)\n") {
		t.Errorf("notes of an earlier hold (%v):\n%s", err, rendered)
	}
}

func TestRenderOmitsEmptySections(t *testing.T) {
	st := buildState(t,
		rec{id: "beta", name: "Beta", first: earlier, changed: rev},
		rec{id: "gamma", name: "Gamma", first: earlier, changed: earlier},
	)

	got, err := notes.Render(st, repository)
	if err != nil {
		t.Fatal(err)
	}

	for _, absent := range []string{"## Added", "## Metadata updated", "## Held", "## Excluded"} {
		if strings.Contains(string(got), absent) {
			t.Errorf("notes contain the empty section %q", absent)
		}
	}

	if !strings.Contains(string(got), "## Changed (1)") || !strings.Contains(string(got), "0 added, 1 changed, 0 metadata updated, 0 excluded, 1 unchanged (0 of them held).") {
		t.Errorf("notes:\n%s", got)
	}
}

func TestRenderKeepsWithinTheReleaseBodyLimit(t *testing.T) {
	records := make([]rec, 0, 2000)

	for i := range 2000 {
		records = append(records, rec{id: fmt.Sprintf("schema-%04d", i), name: strings.Repeat("n", 200), first: rev, changed: rev})
	}

	got, err := notes.Render(buildState(t, records...), repository)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) > notes.MaxBytes || !strings.Contains(string(got), "## Added (2000)") || !strings.Contains(string(got), " more not listed here\n") {
		t.Errorf("notes of %d bytes end with %q", len(got), got[max(0, len(got)-200):])
	}
}

// longState records n schemas of each kind of change with 200-byte names,
// so that every section overflows the release body limit by itself.
func longState(t *testing.T, n, small int) *state.State {
	t.Helper()

	name := strings.Repeat("n", 200)
	records := make([]rec, 0, n+4*small)

	for i := range n {
		records = append(records, rec{id: fmt.Sprintf("added-%04d", i), name: name, first: rev, changed: rev})
	}

	for i := range small {
		records = append(records,
			rec{id: fmt.Sprintf("changed-%04d", i), name: name, first: earlier, changed: rev},
			rec{id: fmt.Sprintf("meta-%04d", i), name: name, first: oldest, artifact: earlier, changed: rev},
			rec{id: fmt.Sprintf("held-%04d", i), name: name, first: oldest, changed: oldest, heldSince: earlier, heldReason: state.HeldFetchFailed},
			rec{id: fmt.Sprintf("out-%04d", i), name: name, first: oldest, changed: earlier, excluded: rev},
		)
	}

	slices.SortFunc(records, func(a, b rec) int { return strings.Compare(a.id, b.id) })

	return buildState(t, records...)
}

// Once one section used up the budget, later sections used to add their
// headings and counts on top, and `catalogbot release` refused the notes
// after the state commit, run after run.
func TestRenderKeepsSeveralOverflowingSectionsWithinTheLimit(t *testing.T) {
	got, err := notes.Render(longState(t, 2000, 2000), repository)
	if err != nil {
		t.Fatal(err)
	}

	text := string(got)
	if len(got) > notes.MaxBytes || strings.Count(text, " more not listed here\n") != 5 {
		t.Errorf("notes of %d bytes end with %q", len(got), text[max(0, len(text)-300):])
	}

	for _, heading := range []string{"## Added (2000)", "## Changed (2000)", "## Metadata updated (2000)", "## Held (2000)", "## Excluded (2000)"} {
		if !strings.Contains(text, heading) {
			t.Errorf("notes lack %q", heading)
		}
	}
}

// A long section leaves room for the short ones after it, which are listed
// in full.
func TestRenderListsShortSectionsAfterALongOneInFull(t *testing.T) {
	got, err := notes.Render(longState(t, 2000, 5), repository)
	if err != nil {
		t.Fatal(err)
	}

	text := string(got)
	if len(got) > notes.MaxBytes || strings.Count(text, " more not listed here\n") != 1 {
		t.Errorf("notes of %d bytes end with %q", len(got), text[max(0, len(text)-300):])
	}

	for _, want := range []string{"## Added (2000)", "- `changed-0004` `", "- `meta-0004` `", "- `held-0004` `", "- `out-0004` `"} {
		if !strings.Contains(text, want) {
			t.Errorf("notes lack %q", want)
		}
	}
}

func TestRenderRefusesNotesOverTheLimit(t *testing.T) {
	if _, err := notes.Render(sampleState(t), "ghcr.io/"+strings.Repeat("a", notes.MaxBytes)); fault.KindOf(err) != fault.Usage {
		t.Errorf("a repository name longer than the limit: %v", err)
	}
}

func TestRenderRefusesInvalidInput(t *testing.T) {
	for name, tc := range map[string]struct {
		st   *state.State
		repo string
	}{
		"no state":   {repo: repository},
		"repository": {st: sampleState(t), repo: "ghcr.io/Owner/Schemas`"},
	} {
		if _, err := notes.Render(tc.st, tc.repo); fault.KindOf(err) != fault.Usage {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCheck(t *testing.T) {
	st := sampleState(t)

	if err := notes.Check(sampleResult(), st); err != nil {
		t.Fatalf("a matching result: %v", err)
	}

	unsorted := sampleResult()
	unsorted.Added = []string{"alpha"}

	if err := notes.Check(unsorted, st); err != nil {
		t.Errorf("a matching result: %v", err)
	}

	cases := map[string]func(*notes.Result) *state.State{
		"noop":           func(r *notes.Result) *state.State { r.Status = notes.StatusNoop; return st },
		"other revision": func(r *notes.Result) *state.State { r.Revision = "20260924.0906"; return st },
		"other digest": func(r *notes.Result) *state.State {
			r.CatalogDigest = "sha256:" + strings.Repeat("0", 64)

			return st
		},
		"unknown id":        func(r *notes.Result) *state.State { r.Changed = []string{"nope"}; return st },
		"missing hold":      func(r *notes.Result) *state.State { r.Held = nil; return st },
		"other hold reason": func(r *notes.Result) *state.State { r.Held[0].Reason = state.HeldFetchFailed; return st },
		"held as excluded": func(r *notes.Result) *state.State {
			r.Held, r.Excluded = r.Held[:1], append(r.Excluded, "zeta")

			return st
		},
		"old exclusion":     func(r *notes.Result) *state.State { r.Excluded = append(r.Excluded, "delta"); return st },
		"missing exclusion": func(r *notes.Result) *state.State { r.Excluded = nil; return st },
		"metadata as change": func(r *notes.Result) *state.State {
			r.Changed, r.MetadataChanged = []string{"beta", "kappa"}, nil
			return st
		},
		"other unchanged":    func(r *notes.Result) *state.State { r.Unchanged = 2; return st },
		"added as changed":   func(r *notes.Result) *state.State { r.Added, r.Changed = nil, []string{"alpha", "beta"}; return st },
		"no state":           func(*notes.Result) *state.State { return nil },
		"old revision":       func(r *notes.Result) *state.State { r.Revision = "20260924.1"; return st },
		"bad catalog digest": func(r *notes.Result) *state.State { r.CatalogDigest = "sha256:x"; return st },
	}

	for name, mutate := range cases {
		res := sampleResult()

		if err := notes.Check(res, mutate(res)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if k := fault.KindOf(err); k != fault.Usage && k != fault.Integrity {
			t.Errorf("%s: %v is %s", name, err, k)
		}
	}
}

func TestLoadResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "publish.json")

	data := `{"status":"resumed","revision":"20260924.0905","catalogDigest":"` + catalogDigest + `","catalogSize":1234,` +
		`"added":["a"],"changed":[],"metadataChanged":["m"],"held":[{"id":"z","reason":"license-review"}],"excluded":["x"],` +
		`"unchanged":3,"uploadedSchemas":1,"reusedSchemas":3,"repository":"x"}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := notes.LoadResult(path)
	if err != nil {
		t.Fatal(err)
	}

	if res.Status != notes.StatusResumed || res.Added[0] != "a" || res.MetadataChanged[0] != "m" || res.Held[0] != (notes.Held{ID: "z", Reason: state.HeldLicenseReview}) ||
		res.Excluded[0] != "x" || res.Unchanged != 3 || res.ReusedSchemas != 3 {
		t.Errorf("result = %+v", res)
	}

	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := notes.LoadResult(path); fault.KindOf(err) != fault.Usage {
		t.Errorf("malformed result: %v", err)
	}
}
