package prepare

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

const reuseRevision = "20260921.0300"

// stateFromSet records a prepared set as publish would after publishing it
// as reuseRevision, with a made-up artifact per entry.
func stateFromSet(t *testing.T, set *Set) *state.State {
	t.Helper()

	s := &state.State{
		FormatVersion: state.FormatVersion,
		Source:        state.Source{Kind: state.KindLocal, Name: set.Document.Source.Name},
		Recipe:        Recipe,
		Catalog:       state.Catalog{Revision: reuseRevision, Digest: sha("catalog"), Size: 400},
	}

	if src := set.Document.Source; src.Kind == KindSchemaStore {
		s.Source = state.Source{
			Kind: state.KindSchemaStore, Repository: state.SchemaStoreRepository, Commit: src.Commit, TarballDigest: src.TarballDigest,
		}
	}

	for _, e := range set.Document.Entries {
		prov := e.Provenance
		rec := state.Schema{
			ID: e.ID, ContentDigest: e.ContentDigest, FirstRevision: reuseRevision, LastChangedRevision: reuseRevision, License: e.License,
			Entry: catalog.Entry{
				ID: e.ID, Name: e.Name, Description: e.Description, Dialect: e.Dialect,
				Artifact:   catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: sha("artifact " + e.ID), Size: 500},
				Provenance: &prov,
			},
		}

		if len(e.FileMatch) > 0 {
			rec.Entry.FileMatch = e.FileMatch
		}

		if e.Notice != "" {
			rec.NoticeDigest = digest.FromBytes(set.Notices[e.Notice])
		}

		s.Schemas = append(s.Schemas, rec)
	}

	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}

	return s
}

// reuseEnv is a local source of two published schemas: mit, whose license
// automatic detection finds, and net, fetched from the network with a
// dependency, both of which license rules allow.
type reuseEnv struct {
	*localEnv

	services *licenseServices
	state    *state.State
}

const reuseSource = `kind = "local"
name = "reuse"
policy = "licenses.toml"

[fetch]
allow_http = true
allow_private_hosts = ["{{addr}}"]

[[entries]]
id = "mit"
name = "MIT schema"
url = "` + mitURL + `"
file = "mit.json"

[[entries]]
id = "net"
name = "Network schema"
url = "{{server}}/net.json"
`

const reusePolicy = `[auto]
enabled = true
hosts = ["api.github.com"]

[[rules]]
id = "net"
decision = "allow"
urls = ["{{server}}/net.json"]
license = "Apache-2.0"
reason = "reviewed"

[[rules]]
id = "dep"
decision = "{{dep}}"
urls = ["{{server}}/dep.json"]
license = "Apache-2.0"
reason = "reviewed"
`

func newReuseEnv(t *testing.T) *reuseEnv {
	t.Helper()

	e := &reuseEnv{
		localEnv: newLocalEnv(t, map[string]string{
			"source.toml": reuseSource, "licenses.toml": strings.ReplaceAll(reusePolicy, "{{dep}}", "allow"), "mit.json": `{"type":"object"}`,
		}),
		services: newLicenseServices(t),
	}

	e.deps.set("/net.json", `{"$schema":"`+draft7+`","properties":{"x":{"$ref":"`+e.deps.srv.URL+`/dep.json#/definitions/x"}}}`)
	e.deps.set("/dep.json", `{"$schema":"`+draft7+`","definitions":{"x":{"type":"string"}}}`)

	_, out := e.prepare(t, nil, nil)
	e.state = stateFromSet(t, mustLoad(t, out))

	return e
}

func (e *reuseEnv) policy(t *testing.T, extra string) {
	t.Helper()

	e.writePolicy(t, "allow", extra)
}

func (e *reuseEnv) writePolicy(t *testing.T, dep, extra string) {
	t.Helper()

	content := strings.NewReplacer("{{server}}", e.deps.srv.URL, "{{dep}}", dep).Replace(reusePolicy + extra)
	writeFiles(t, e.dir, map[string]string{"licenses.toml": content})
}

func (e *reuseEnv) prepare(t *testing.T, previous *state.State, change func(o *Options)) (*Result, string) {
	t.Helper()

	out := filepath.Join(t.TempDir(), "out")
	opts := Options{
		Tool: pinnedTool(t), SourceFile: filepath.Join(e.dir, "source.toml"), OutDir: out, State: previous,
		LicenseDetector: e.services.detector(t), Now: func() time.Time { return fixedNow },
	}

	if change != nil {
		change(&opts)
	}

	res, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}

	return res, out
}

// reusedIDs returns the IDs of the reused entries of a prepared set.
func reusedIDs(t *testing.T, out string) []string {
	t.Helper()

	var ids []string

	for _, e := range mustLoad(t, out).Document.Entries {
		if e.Reused != nil {
			ids = append(ids, e.ID)
		}
	}

	return ids
}

func TestUnchangedSchemasReuseTheirArtifact(t *testing.T) {
	e := newReuseEnv(t)
	detections, fetches := e.services.total(), e.deps.hitCount("/dep.json")

	res, out := e.prepare(t, e.state, nil)
	if e.services.total() != detections {
		t.Errorf("unchanged schemas made %d license detection requests", e.services.total()-detections)
	}

	if e.deps.hitCount("/dep.json") != fetches+1 {
		t.Error("the recorded dependency was not fetched to compare its digest")
	}

	if res.Totals.Reused != 2 || res.Totals.Entries != 2 || res.Totals.LicenseDetection.AutoAllowed != 1 || len(res.Held) != 0 {
		t.Fatalf("totals = %+v", res.Totals)
	}

	set := mustLoad(t, out)
	if len(set.Schemas)+len(set.Notices) != 0 {
		t.Errorf("reused entries wrote %d schema and %d notice files", len(set.Schemas), len(set.Notices))
	}

	for _, entry := range set.Document.Entries {
		rec, _ := e.state.Lookup(entry.ID)
		if *entry.Reused != rec.Entry.Artifact || entry.ContentDigest != rec.ContentDigest || entry.NoticeDigest != rec.NoticeDigest ||
			!entry.License.Equal(&rec.License) || !reflect.DeepEqual(entry.Provenance, *rec.Entry.Provenance) {
			t.Errorf("%s = %+v, recorded %+v", entry.ID, entry, rec)
		}
	}

	for _, entry := range set.Document.Entries {
		if entry.Reused.Digest != sha("artifact "+entry.ID) {
			t.Errorf("%s reuses %+v", entry.ID, entry.Reused)
		}
	}

	if mit := recordByURL(t, readReport(t, out), mitURL); !mit.Reused || mit.License != "MIT" || mit.Verification != nil {
		t.Errorf("mit record = %+v", mit)
	}
}

// The weekly update prepares SchemaStore: in a week without upstream
// changes every published entry keeps its artifact, whatever the spelling of
// its upstream URL (canonical, json.schemastore.org alias, raw repository
// URL), for a fragment of a document and for a schema with a network
// dependency, and nothing is bundled.
func TestSchemaStoreUnchangedWeekReusesEveryEntry(t *testing.T) {
	e := newSchemaStoreEnv(t)
	first := filepath.Join(t.TempDir(), "first")

	if _, err := Run(t.Context(), e.options(t, first)); err != nil {
		t.Fatal(err)
	}

	published := mustLoad(t, first)
	recorded := stateFromSet(t, published)

	second := filepath.Join(t.TempDir(), "second")
	opts := e.options(t, second)
	opts.State = recorded

	res, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}

	if res.Totals.Entries != len(published.Document.Entries) || res.Totals.Reused != res.Totals.Entries || res.Totals.Bundled+res.Totals.CompactOnly != 0 ||
		len(res.Held)+len(res.Excluded)+len(res.Regressions) != 0 {
		t.Fatalf("totals %+v, held %v, excluded %v", res.Totals, res.Held, res.Excluded)
	}

	for _, entry := range mustLoad(t, second).Document.Entries {
		was := entryByID(t, published, entry.ID)
		if entry.Name != was.Name || entry.Description != was.Description || !slices.Equal(entry.FileMatch, was.FileMatch) ||
			!reflect.DeepEqual(entry.Provenance, was.Provenance) || !entry.License.Equal(&was.License) {
			t.Errorf("%s = %+v, published %+v", entry.ID, entry, was)
		}
	}
}

// Anything the recorded decision does not cover prepares the schema anew:
// changed content of a dependency, an allow rule where detection decided,
// a record without a license decision, or a refresh.
func TestChangedInputsPrepareAnew(t *testing.T) {
	e := newReuseEnv(t)

	t.Run("a dependency changed", func(t *testing.T) {
		e.deps.set("/dep.json", `{"$schema":"`+draft7+`","definitions":{"x":{"type":"string","minLength":2}}}`)
		defer e.deps.set("/dep.json", `{"$schema":"`+draft7+`","definitions":{"x":{"type":"string"}}}`)

		_, out := e.prepare(t, e.state, nil)
		if got := reusedIDs(t, out); !slices.Equal(got, []string{"mit"}) {
			t.Errorf("reused %v", got)
		}
	})

	t.Run("an allow rule now decides what detection decided", func(t *testing.T) {
		e.policy(t, "\n[[rules]]\nid = \"mit\"\ndecision = \"allow\"\nurls = [\""+mitURL+"\"]\nlicense = \"MIT\"\nreason = \"reviewed\"\n")
		defer e.policy(t, "")

		_, out := e.prepare(t, e.state, nil)
		if got := reusedIDs(t, out); !slices.Equal(got, []string{"net"}) {
			t.Errorf("reused %v", got)
		}

		if mit := entryByID(t, mustLoad(t, out), "mit"); !slices.Equal(mit.License.Rules, []string{"mit"}) || len(mit.License.Detections) != 0 {
			t.Errorf("mit = %+v", mit)
		}
	})

	t.Run("a record without a license decision", func(t *testing.T) {
		legacy := *e.state
		legacy.Schemas = slices.Clone(e.state.Schemas)

		for i := range legacy.Schemas {
			legacy.Schemas[i].License = state.LicenseDecision{}
		}

		_, out := e.prepare(t, &legacy, nil)
		if got := reusedIDs(t, out); len(got) != 0 {
			t.Errorf("reused %v", got)
		}
	})

	t.Run("refresh", func(t *testing.T) {
		before := e.services.total()

		_, out := e.prepare(t, e.state, func(o *Options) { o.Refresh = []string{"mit"} })
		if got := reusedIDs(t, out); !slices.Equal(got, []string{"net"}) || e.services.total() == before {
			t.Errorf("--refresh mit reused %v", got)
		}

		_, out = e.prepare(t, e.state, func(o *Options) { o.RefreshAll = true })
		if got := reusedIDs(t, out); len(got) != 0 {
			t.Errorf("--refresh-all reused %v", got)
		}
	})

	t.Run("refresh of an unknown ID", func(t *testing.T) {
		for _, previous := range []*state.State{e.state, nil} {
			_, err := Run(t.Context(), Options{
				Tool: pinnedTool(t), SourceFile: filepath.Join(e.dir, "source.toml"), OutDir: filepath.Join(t.TempDir(), "out"),
				State: previous, Refresh: []string{"nope"},
			})
			wantKind(t, err, fault.Usage)
			mustContain(t, err.Error(), `cannot refresh "nope"`)
		}
	})
}

// The license policy decides again on the recorded findings before anything
// is fetched: rules that exclude or hold a published schema, and a detected
// license the policy no longer permits, need no detection request.
func TestRecordedDecisionMeetsTheCurrentPolicy(t *testing.T) {
	e := newReuseEnv(t)

	for name, tc := range map[string]struct {
		dep      string
		extra    string
		held     []Hold
		excluded []Exclusion
	}{
		"an exclude rule": {
			dep:      "allow",
			extra:    "\n[[rules]]\nid = \"takedown\"\ndecision = \"exclude\"\nurls = [\"" + mitURL + "\"]\nreason = \"takedown\"\n",
			excluded: []Exclusion{{ID: "mit", Rule: "takedown"}},
		},
		"a review rule on a dependency": {
			dep:  "review",
			held: []Hold{{ID: "net", Reason: state.HeldLicenseReview}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			e.writePolicy(t, tc.dep, tc.extra)
			defer e.policy(t, "")

			before, fetches := e.services.total(), e.deps.hitCount("/dep.json")

			res, _ := e.prepare(t, e.state, nil)
			if e.services.total() != before {
				t.Errorf("%d detection requests", e.services.total()-before)
			}

			if tc.held != nil && e.deps.hitCount("/dep.json") != fetches {
				t.Error("a dependency a review rule holds was fetched")
			}

			if !reflect.DeepEqual(res.Held, append([]Hold{}, tc.held...)) || !reflect.DeepEqual(res.Excluded, append([]Exclusion{}, tc.excluded...)) ||
				len(res.Regressions) != 0 {
				t.Errorf("held %v, excluded %v, regressions %v", res.Held, res.Excluded, res.Regressions)
			}
		})
	}

	t.Run("a license the allow list no longer names", func(t *testing.T) {
		writeFiles(t, e.dir, map[string]string{
			"licenses.toml": strings.NewReplacer("{{server}}", e.deps.srv.URL, "{{dep}}", "allow",
				"enabled = true", "enabled = true\nallow = [\"Apache-2.0\"]").Replace(reusePolicy),
		})
		defer e.policy(t, "")

		before := e.services.total()

		res, _ := e.prepare(t, e.state, nil)
		if e.services.total() != before || !reflect.DeepEqual(res.Held, []Hold{{ID: "mit", Reason: state.HeldLicenseRefused}}) {
			t.Errorf("held %v after %d detection requests", res.Held, e.services.total()-before)
		}
	})

	t.Run("a source that cannot be fetched", func(t *testing.T) {
		e.deps.mu.Lock()
		body := e.deps.docs["/net.json"]
		delete(e.deps.docs, "/net.json")
		e.deps.mu.Unlock()

		defer e.deps.set("/net.json", body)

		res, _ := e.prepare(t, e.state, nil)
		if !reflect.DeepEqual(res.Held, []Hold{{ID: "net", Reason: state.HeldFetchFailed}}) {
			t.Errorf("held %v", res.Held)
		}
	})
}

// An exclude rule removes a published schema whatever else keeps it from
// being refreshed: a source upstream no longer lists (a takedown after the
// removal), or new content that fails before its dependencies are decided.
// The rule is matched against the recorded source and dependencies, which
// are the content the catalog would otherwise keep.
func TestExcludeRuleRemovesSchemasThatWouldBeHeld(t *testing.T) {
	e := newReuseEnv(t)
	sourceFile := filepath.Join(e.dir, "source.toml")

	source, err := os.ReadFile(sourceFile)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("removed upstream", func(t *testing.T) {
		mitEntry := "[[entries]]\nid = \"mit\"\nname = \"MIT schema\"\nurl = \"" + mitURL + "\"\nfile = \"mit.json\"\n\n"
		if !strings.Contains(string(source), mitEntry) {
			t.Fatalf("the source file has no mit entry:\n%s", source)
		}

		writeFiles(t, e.dir, map[string]string{"source.toml": strings.Replace(string(source), mitEntry, "", 1)})
		defer writeFiles(t, e.dir, map[string]string{"source.toml": string(source)})

		res, _ := e.prepare(t, e.state, nil)
		if !reflect.DeepEqual(res.Held, []Hold{{ID: "mit", Reason: state.HeldRemovedUpstream}}) || len(res.Excluded) != 0 {
			t.Fatalf("without a rule: held %v, excluded %v", res.Held, res.Excluded)
		}

		e.policy(t, "\n[[rules]]\nid = \"takedown\"\ndecision = \"exclude\"\nurls = [\""+mitURL+"\"]\nreason = \"takedown\"\n")
		defer e.policy(t, "")

		res, out := e.prepare(t, e.state, nil)
		if len(res.Held) != 0 || !reflect.DeepEqual(res.Excluded, []Exclusion{{ID: "mit", Rule: "takedown"}}) || len(res.Regressions) != 0 {
			t.Errorf("held %v, excluded %v, regressions %v", res.Held, res.Excluded, res.Regressions)
		}

		if doc := mustLoad(t, out).Document; !reflect.DeepEqual(doc.Excluded, []Exclusion{{ID: "mit", Rule: "takedown"}}) || len(doc.Held) != 0 {
			t.Errorf("prepared.json: held %v, excluded %v", doc.Held, doc.Excluded)
		}
	})

	t.Run("new content fails before its dependencies are decided", func(t *testing.T) {
		e.deps.mu.Lock()
		body := e.deps.docs["/net.json"]
		e.deps.mu.Unlock()

		e.deps.set("/net.json", `{"properties":`)
		defer e.deps.set("/net.json", body)

		e.writePolicy(t, "exclude", "")
		defer e.policy(t, "")

		res, out := e.prepare(t, e.state, func(o *Options) { o.Refresh = []string{"net"} })
		if len(res.Held) != 0 || !reflect.DeepEqual(res.Excluded, []Exclusion{{ID: "net", Rule: "dep"}}) {
			t.Errorf("held %v, excluded %v", res.Held, res.Excluded)
		}

		if row := recordByURL(t, readReport(t, out), e.deps.srv.URL+"/net.json"); row.ID != "net" || row.Held != "" || row.Status != StatusFailed {
			t.Errorf("report row = %+v", row)
		}
	})
}

// A schema an ID override renames keeps its old ID in the next catalog,
// held as removed upstream, and no other source may take that ID.
func TestRenamedIDStaysHeldAndReserved(t *testing.T) {
	e := newReuseEnv(t)
	writeFiles(t, e.dir, map[string]string{
		"ids.json": `{"` + mitURL + `": "mit-renamed", "` + e.deps.srv.URL + `/other.json": "other"}`,
		"source.toml": strings.NewReplacer("id = \"mit\"\n", "", "{{server}}", e.deps.srv.URL, "{{addr}}", e.deps.srv.Listener.Addr().String()).
			Replace(reuseSource) +
			"\n[[entries]]\nname = \"Newcomer\"\nurl = \"https://schemas.example/mit.json\"\nfile = \"mit.json\"\nlicense = \"MIT\"\n",
	})

	res, out := e.prepare(t, e.state, func(o *Options) { o.IDsFile = filepath.Join(e.dir, "ids.json") })

	ids := map[string]string{}
	for _, entry := range mustLoad(t, out).Document.Entries {
		ids[entry.Provenance.Source] = entry.ID
	}

	if ids[mitURL] != "mit-renamed" || ids["https://schemas.example/mit.json"] == "mit" ||
		!reflect.DeepEqual(res.Held, []Hold{{ID: "mit", Reason: state.HeldRemovedUpstream}}) {
		t.Errorf("ids %v, held %v", ids, res.Held)
	}
}
