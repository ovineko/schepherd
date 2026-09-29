package publish

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

func TestLatestPromotesTheRecordedCatalog(t *testing.T) {
	repo := newFakeRepo()
	first := publish(t, repo, writeSet(t, baseSchemas()), Options{UpdateLatest: true})

	changed := baseSchemas()
	changed["beta"] = `{"type":"string","maxLength":9}`

	second := publish(t, repo, writeSet(t, changed), Options{State: first.State, Now: week1.Add(time.Hour)})
	if repo.tags[LatestTag] != first.CatalogDigest {
		t.Fatalf("catalog-latest moved without UpdateLatest")
	}

	repo.resetCalls()

	checked, err := Latest(t.Context(), repo, second.State, LatestOptions{Repository: "r", CheckOnly: true})
	if err != nil || checked.Latest != LatestChecked || checked.Revision != second.Revision || checked.CatalogDigest != second.CatalogDigest ||
		checked.CatalogSize != second.CatalogSize || checked.Repository != "r" {
		t.Fatalf("check = %+v, %v", checked, err)
	}

	if repo.writes() != 0 || repo.tags[LatestTag] != first.CatalogDigest {
		t.Errorf("the check wrote: calls %v", repo.calls)
	}

	moved, err := Latest(t.Context(), repo, second.State, LatestOptions{})
	if err != nil || moved.Latest != LatestMoved || repo.tags[LatestTag] != second.CatalogDigest {
		t.Fatalf("move = %+v, %v; catalog-latest %s", moved, err, repo.tags[LatestTag])
	}

	if repo.count("PushBlob", "PushManifest", "EnsureTag") != 0 || repo.count("Tag") != 1 {
		t.Errorf("calls = %v", repo.calls)
	}

	repo.resetCalls()

	again, err := Latest(t.Context(), repo, second.State, LatestOptions{})
	if err != nil || again.Latest != LatestCurrent || repo.writes() != 0 {
		t.Errorf("second move = %+v, %v; calls %v", again, err, repo.calls)
	}
}

func TestLatestRefusesAStateTheRegistryDoesNotHold(t *testing.T) {
	repo := newFakeRepo()
	first := publish(t, repo, writeSet(t, baseSchemas()), Options{UpdateLatest: true})

	clone := func(change func(s *state.State)) *state.State {
		s := *first.State
		s.Schemas = slices.Clone(first.State.Schemas)
		change(&s)

		return &s
	}

	cases := map[string]struct {
		state *state.State
		msg   string
		kind  fault.Kind
	}{
		"no state":        {msg: "no state", kind: fault.Usage},
		"another digest":  {state: clone(func(s *state.State) { s.Catalog.Digest = digest.FromBytes([]byte("x")) }), msg: "points to", kind: fault.Integrity},
		"another size":    {state: clone(func(s *state.State) { s.Catalog.Size++ }), msg: "points to", kind: fault.Integrity},
		"missing tag":     {state: clone(func(s *state.State) { s.Catalog.Revision = "20260923.0301" }), msg: "does not exist", kind: fault.Integrity},
		"invalid state":   {state: clone(func(s *state.State) { s.Recipe = "" }), msg: "recipe", kind: fault.Integrity},
		"other entries":   {state: clone(func(s *state.State) { s.Schemas[1].Entry.Name = "Edited by hand" }), msg: "does not list the entries", kind: fault.Integrity},
		"an entry less":   {state: clone(func(s *state.State) { s.Schemas = s.Schemas[1:] }), msg: "does not list the entries", kind: fault.Integrity},
		"another content": {state: clone(func(s *state.State) { s.Schemas[0].Entry.Artifact = s.Schemas[1].Entry.Artifact }), msg: "does not list", kind: fault.Integrity},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			for _, check := range []bool{true, false} {
				repo.resetCalls()

				_, err := Latest(t.Context(), repo, tc.state, LatestOptions{CheckOnly: check})
				if fault.KindOf(err) != tc.kind || !strings.Contains(err.Error(), tc.msg) {
					t.Errorf("check %v: err = %v, want %s containing %q", check, err, tc.kind, tc.msg)
				}

				if repo.writes() != 0 || repo.tags[LatestTag] != first.CatalogDigest {
					t.Errorf("check %v: a refused promotion wrote: %v", check, repo.calls)
				}
			}
		})
	}
}
