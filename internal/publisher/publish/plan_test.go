package publish

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/prepare"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

func loadSet(t *testing.T, dir string) *prepare.Set {
	t.Helper()

	set, err := prepare.Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	return set
}

func diffOf(t *testing.T, dir string, previous *state.State) Diff {
	t.Helper()

	plan, err := NewPlan(loadSet(t, dir), previous)
	if err != nil {
		t.Fatal(err)
	}

	return plan.Diff()
}

// without returns the base schemas minus the given IDs.
func without(ids ...string) map[string]string {
	schemas := baseSchemas()
	for _, id := range ids {
		delete(schemas, id)
	}

	return schemas
}

// withRecord returns a copy of st in which change edited the record of id.
func withRecord(st *state.State, id string, change func(rec *state.Schema)) *state.State {
	out := *st
	out.Schemas = append([]state.Schema{}, st.Schemas...)

	for i := range out.Schemas {
		if out.Schemas[i].ID == id {
			change(&out.Schemas[i])
		}
	}

	return &out
}

func emptyDiff() Diff {
	return Diff{Added: []string{}, Changed: []string{}, MetadataChanged: []string{}, Held: []Hold{}, Excluded: []string{}}
}

func TestPlanDiff(t *testing.T) {
	first := publish(t, newFakeRepo(), writeSet(t, baseSchemas()), Options{})
	previous := first.State

	earlier := func(rec *state.Schema) { rec.FirstRevision, rec.LastChangedRevision = "20260901.0000", "20260901.0000" }
	heldGamma := withRecord(previous, "gamma", func(rec *state.Schema) {
		earlier(rec)
		rec.HeldSinceRevision, rec.HeldReason = previous.Catalog.Revision, state.HeldRemovedUpstream
	})
	excludedGamma := withRecord(previous, "gamma", func(rec *state.Schema) {
		earlier(rec)
		rec.ExcludedRevision = previous.Catalog.Revision
	})

	changed := without("delta")
	changed["beta"] = `{"type":"string","minLength":2}`
	changed["epsilon"] = `{"type":"null"}`

	diff := func(change func(d *Diff)) Diff {
		d := emptyDiff()
		change(&d)

		return d
	}

	cases := map[string]struct {
		dir      string
		previous *state.State
		want     Diff
	}{
		"first run": {
			dir:  writeSet(t, baseSchemas()),
			want: diff(func(d *Diff) { d.HasChanges, d.Added = true, []string{"alpha", "beta", "delta", "gamma"} }),
		},
		"unchanged": {
			dir: writeSet(t, baseSchemas()), previous: previous,
			want: diff(func(d *Diff) { d.Unchanged = 4 }),
		},
		"added, changed and held": {
			dir: writeSetWith(t, changed, setOptions{held: map[string]string{"delta": state.HeldRemovedUpstream}}), previous: previous,
			want: diff(func(d *Diff) {
				d.HasChanges, d.Added, d.Changed, d.Unchanged = true, []string{"epsilon"}, []string{"beta"}, 3
				d.Held = []Hold{{ID: "delta", Reason: state.HeldRemovedUpstream}}
			}),
		},
		"a hold alone changes nothing": {
			dir: writeSetWith(t, without("delta"), setOptions{held: map[string]string{"delta": state.HeldLicenseDetectionFailed}}), previous: previous,
			want: diff(func(d *Diff) {
				d.Unchanged, d.Held = 4, []Hold{{ID: "delta", Reason: state.HeldLicenseDetectionFailed}}
			}),
		},
		"metadata only": {
			dir: writeSetWith(t, baseSchemas(), setOptions{names: map[string]string{"alpha": "Alpha config"}}), previous: previous,
			want: diff(func(d *Diff) { d.HasChanges, d.MetadataChanged, d.Unchanged = true, []string{"alpha"}, 3 }),
		},
		"reused with new metadata": {
			dir: writeSetWith(t, baseSchemas(), setOptions{
				names: map[string]string{"alpha": "Alpha config"}, state: previous, reuse: []string{"alpha", "gamma"},
			}),
			previous: previous,
			want:     diff(func(d *Diff) { d.HasChanges, d.MetadataChanged, d.Unchanged = true, []string{"alpha"}, 3 }),
		},
		"notice only": {
			dir: writeSetWith(t, baseSchemas(), setOptions{notice: "Another notice.\n"}), previous: previous,
			want: diff(func(d *Diff) { d.HasChanges, d.Changed, d.Unchanged = true, []string{"alpha"}, 3 }),
		},
		"excluded": {
			dir: writeSetWith(t, without("gamma"), setOptions{excluded: map[string]string{"gamma": "takedown"}}), previous: previous,
			want: diff(func(d *Diff) { d.HasChanges, d.Excluded, d.Unchanged = true, []string{"gamma"}, 3 }),
		},
		"still excluded": {
			dir: writeSet(t, without("gamma")), previous: excludedGamma,
			want: diff(func(d *Diff) { d.Unchanged = 3 }),
		},
		"back after an exclusion": {
			dir: writeSet(t, baseSchemas()), previous: excludedGamma,
			want: diff(func(d *Diff) { d.HasChanges, d.Added, d.Unchanged = true, []string{"gamma"}, 3 }),
		},
		"a hold ends without a change": {
			dir: writeSet(t, baseSchemas()), previous: heldGamma,
			want: diff(func(d *Diff) { d.Unchanged = 4 }),
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := diffOf(t, tc.dir, tc.previous); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Diff() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestPlanRefusesAnotherState checks that a prepared set is only compared
// with the state it was prepared with.
func TestPlanRefusesAnotherState(t *testing.T) {
	first := publish(t, newFakeRepo(), writeSet(t, baseSchemas()), Options{})
	previous := first.State
	otherArtifact := withRecord(previous, "alpha", func(rec *state.Schema) { rec.Entry.Artifact.Size++ })

	for name, tc := range map[string]struct {
		dir      string
		previous *state.State
		want     string
	}{
		"a published schema is missing": {writeSet(t, without("gamma")), previous, "gamma of the recorded catalog is neither"},
		"a held schema is not published": {
			writeSetWith(t, baseSchemas(), setOptions{held: map[string]string{"zeta": state.HeldFetchFailed}}), previous, "zeta is held, but",
		},
		"an excluded schema the state does not know": {
			writeSetWith(t, baseSchemas(), setOptions{excluded: map[string]string{"zeta": "takedown"}}), previous, "zeta is excluded, but the state has no record",
		},
		"a hold without a state": {
			writeSetWith(t, without("gamma"), setOptions{held: map[string]string{"gamma": state.HeldFetchFailed}}), nil, "gamma is held, but",
		},
		"an exclusion without a state": {
			writeSetWith(t, without("gamma"), setOptions{excluded: map[string]string{"gamma": "takedown"}}), nil, "gamma is excluded, but",
		},
		"a reused artifact the state does not record": {
			writeSetWith(t, baseSchemas(), setOptions{state: otherArtifact, reuse: []string{"alpha"}}), previous, "alpha reuses an artifact",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewPlan(loadSet(t, tc.dir), tc.previous)
			if fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a usage error containing %q", err, tc.want)
			}
		})
	}
}

func TestDiffJSON(t *testing.T) {
	data, err := json.Marshal(diffOf(t, writeSet(t, map[string]string{"alpha": `{}`}), nil))
	if err != nil {
		t.Fatal(err)
	}

	want := `{"hasChanges":true,"added":["alpha"],"changed":[],"metadataChanged":[],"held":[],"excluded":[],"unchanged":0}`
	if string(data) != want {
		t.Errorf("Diff JSON = %s, want %s", data, want)
	}
}

func TestNextStateRecordsRevisions(t *testing.T) {
	first := publish(t, newFakeRepo(), writeSet(t, baseSchemas()), Options{})
	previous := withRecord(first.State, "delta", func(rec *state.Schema) {
		rec.FirstRevision, rec.LastChangedRevision = "20260901.0000", "20260901.0000"
		rec.HeldSinceRevision, rec.HeldReason = "20260910.0000", state.HeldFetchFailed
	})

	changed := without("delta", "gamma")
	changed["beta"] = `{"type":"string","minLength":2}`
	changed["epsilon"] = `{"type":"null"}`

	dir := writeSetWith(t, changed, setOptions{
		names: map[string]string{"alpha": "Alpha config"}, state: previous, reuse: []string{"alpha"},
		held: map[string]string{"delta": state.HeldLicenseRefused}, excluded: map[string]string{"gamma": "takedown"},
	})

	plan, err := NewPlan(loadSet(t, dir), previous)
	if err != nil {
		t.Fatal(err)
	}

	for i := range plan.items {
		if plan.items[i].entry.Artifact.Digest == "" {
			plan.items[i].entry.Artifact = first.State.Schemas[0].Entry.Artifact
		}
	}

	const rev = "20260924.1432"

	next := plan.nextState(rev, digest.FromBytes([]byte("catalog")), 321)
	if err := next.Validate(); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"alpha":   "20260923.0300 20260923.0300 " + rev + " / ",
		"beta":    "20260923.0300  " + rev + " / ",
		"delta":   "20260901.0000  20260901.0000 20260910.0000/license-refused ",
		"epsilon": rev + "  " + rev + " / ",
		"gamma":   "20260923.0300  20260923.0300 / " + rev,
	}

	for _, rec := range next.Schemas {
		got := strings.Join([]string{
			rec.FirstRevision, rec.ArtifactRevision, rec.LastChangedRevision, rec.HeldSinceRevision + "/" + rec.HeldReason, rec.ExcludedRevision,
		}, " ")
		if got != want[rec.ID] {
			t.Errorf("%s revisions = %q, want %q", rec.ID, got, want[rec.ID])
		}
	}

	if ids := next.Entries(); len(ids) != 4 {
		t.Errorf("the new catalog lists %d entries; the excluded gamma must not be one", len(ids))
	}

	if next.Catalog != (state.Catalog{Revision: rev, Digest: digest.FromBytes([]byte("catalog")), Size: 321}) {
		t.Errorf("catalog = %+v", next.Catalog)
	}

	if _, err := state.Encode(next); err != nil {
		t.Error(err)
	}
}
