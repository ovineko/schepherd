package prepare

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/bundle"
	"github.com/ovineko/schepherd/internal/publisher/httpfetch"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

const (
	testCommit  = "0123456789abcdef0123456789abcdef01234567"
	draft7      = "http://json-schema.org/draft-07/schema#"
	upstreamTxt = "JSON Schema Store\nCopyright 2015-Current Mads Kristensen and Contributors"
)

// depServer serves external dependencies over plain HTTP on 127.0.0.1,
// answers redirects for configured paths and counts requests per path.
type depServer struct {
	srv       *httptest.Server
	docs      map[string]string
	redirects map[string]string
	hits      map[string]int
	mu        sync.Mutex
}

func newDepServer(t *testing.T) *depServer {
	t.Helper()

	s := &depServer{docs: map[string]string{}, redirects: map[string]string{}, hits: map[string]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits[r.URL.Path]++
		body, ok := s.docs[r.URL.Path]
		location, redirected := s.redirects[r.URL.Path]
		s.mu.Unlock()

		if redirected {
			http.Redirect(w, r, location, http.StatusFound)

			return
		}

		if !ok {
			http.NotFound(w, r)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.srv.Close)

	return s
}

func (s *depServer) set(path, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.docs[path] = body
}

func (s *depServer) redirect(path, location string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.redirects[path] = location
}

func (s *depServer) hitCount(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.hits[path]
}

func (s *depServer) fetcher() *httpfetch.Fetcher {
	return httpfetch.New(httpfetch.Policy{AllowHTTP: true, AllowPrivateHosts: []string{s.srv.Listener.Addr().String()}, MaxBytes: 1 << 20})
}

// schemaStoreTree is a small SchemaStore repository layout, keyed by path.
func schemaStoreTree(depURL string) map[string]string {
	catalogDoc := map[string]any{
		"$schema": "https://www.schemastore.org/schema-catalog.json",
		"version": 1,
		"schemas": []map[string]any{
			{
				"name": "A config", "description": "A", "url": "https://www.schemastore.org/a.json",
				"fileMatch": []string{"a.json", "**/.a/*.yml", "**/x/!(config).yml"},
				"versions":  map[string]string{"1.0": "https://www.schemastore.org/a-1.0.json", "2.0": "https://www.schemastore.org/a.json"},
			},
			{"name": "A yaml", "description": "A as YAML", "url": "https://json.schemastore.org/a", "fileMatch": []string{"a.yaml", "a.json"}},
			{"name": "B", "description": "B uses A", "url": "https://www.schemastore.org/b.json", "fileMatch": []string{"b.json"}},
			{"name": "External", "description": "E", "url": "https://example.com/ext.json", "fileMatch": []string{"ext.json"}},
			{"name": "Excluded", "description": "X", "url": "https://www.schemastore.org/excluded.json"},
			{"name": "Fragment", "description": "F", "url": "https://www.schemastore.org/a.json#/definitions/name"},
			{"name": "Duplicate keys", "description": "D", "url": "https://www.schemastore.org/dup.json"},
			{"name": "Broken", "description": "Br", "url": "https://www.schemastore.org/broken.json"},
			{"name": "Missing", "description": "M", "url": "https://www.schemastore.org/missing.json"},
			{"name": "With dependency", "description": "W", "url": "https://www.schemastore.org/withdep.json"},
			{"name": "With dependency too", "description": "W2", "url": "https://www.schemastore.org/withdep2.json"},
			{"name": "Uses reviewed", "description": "R", "url": "https://www.schemastore.org/usesreview.json"},
			{"name": "C", "description": "C via raw", "url": "https://raw.githubusercontent.com/SchemaStore/schemastore/master/src/schemas/json/c.json"},
			{"name": "Bad\u0007name", "description": "bell", "url": "https://www.schemastore.org/bell.json"},
		},
	}

	data, err := json.MarshalIndent(catalogDoc, "", "  ")
	if err != nil {
		panic(err)
	}

	return map[string]string{
		"LICENSE":                   "Apache License\n",
		"NOTICE":                    upstreamTxt + "\n",
		"src/api/json/catalog.json": string(data),
		"src/schemas/json/a.json": `{"$schema":"` + draft7 + `","$id":"https://json.schemastore.org/a.json",
			"type":"object","definitions":{"name":{"type":"string","minLength":1}},"properties":{"name":{"$ref":"#/definitions/name"}}}`,
		"src/schemas/json/b.json": `{"$schema":"` + draft7 + `","$id":"https://json.schemastore.org/b.json",
			"type":"object","properties":{"name":{"$ref":"a.json#/definitions/name"}},"required":["name"]}`,
		"src/schemas/json/c.json":              `{ "type" : "string" }`,
		"src/schemas/json/excluded.json":       `{"type":"string"}`,
		"src/schemas/json/dup.json":            `{"type":"object","type":"string"}`,
		"src/schemas/json/broken.json":         `{"type":`,
		"src/schemas/json/bell.json":           `{"type":"string"}`,
		"src/schemas/json/partial-review.json": `{"$schema":"` + draft7 + `","definitions":{"x":{"type":"string"}}}`,
		"src/schemas/json/usesreview.json": `{"$schema":"` + draft7 + `","$id":"https://www.schemastore.org/usesreview.json",
			"properties":{"x":{"$ref":"partial-review.json#/definitions/x"}}}`,
		"src/schemas/json/withdep.json": `{"$schema":"` + draft7 + `","$id":"https://json.schemastore.org/withdep.json",
			"type":"object","properties":{"port":{"$ref":"` + depURL + `#/definitions/port"}}}`,
		"src/schemas/json/withdep2.json": `{"$schema":"` + draft7 + `","$id":"https://json.schemastore.org/withdep2.json",
			"type":"array","items":{"$ref":"` + depURL + `#/definitions/port"}}`,
		"src/test/b/valid.json":            `{"name":"x"}`,
		"src/test/b/valid.yaml":            "name: x\n",
		"src/negative_test/b/invalid.json": `{"name":""}`,
	}
}

func writeSnapshot(t *testing.T, dir string, tree map[string]string) {
	t.Helper()

	writeFiles(t, dir, tree)
	writeFiles(t, dir, map[string]string{
		"snapshot.json": `{"commit":"` + testCommit + `","tarballDigest":"` + sha("tarball") + `","formatVersion":1}`,
	})
}

type schemaStoreEnv struct {
	deps     *depServer
	dir      string
	snapshot string
	source   string
	policy   string
	ids      string
	depURL   string
}

func newSchemaStoreEnv(t *testing.T) *schemaStoreEnv {
	t.Helper()

	e := &schemaStoreEnv{deps: newDepServer(t), dir: t.TempDir()}
	e.depURL = e.deps.srv.URL + "/dep.json"
	e.deps.set("/dep.json", `{"$schema":"`+draft7+`","definitions":{"port":{"type":"integer","minimum":1}}}`)
	e.snapshot = filepath.Join(e.dir, "snapshot")
	writeSnapshot(t, e.snapshot, schemaStoreTree(e.depURL))

	e.source = filepath.Join(e.dir, "schemastore.toml")
	e.policy = filepath.Join(e.dir, "licenses.toml")
	e.ids = filepath.Join(e.dir, "ids.json")

	writeFiles(t, e.dir, map[string]string{
		"schemastore.toml": `kind = "upstream"
commit = "` + testCommit + `"
tarball_base_url = "` + e.deps.srv.URL + `/tar.gz"
max_tarball_bytes = 1048576

[dependencies]
max_depth = 8
max_per_schema = 64
max_document_bytes = 1048576
max_total_bytes = 8388608
`,
		"licenses.toml": `[[rules]]
id = "schemastore"
decision = "allow"
hosts = ["www.schemastore.org", "json.schemastore.org"]
license = "Apache-2.0"
notice = "Includes schemas from the JSON Schema Store repository."
reason = "test"

[[rules]]
id = "excluded"
decision = "exclude"
urls = ["https://www.schemastore.org/excluded.json"]
reason = "excluded in test"

[[rules]]
id = "review"
decision = "review"
urls = ["https://www.schemastore.org/partial-review.json"]
reason = "needs review"

[[rules]]
id = "dep"
decision = "allow"
urls = ["` + e.depURL + `"]
license = "MIT"
notice = "Dependency notice."
reason = "test dependency"
`,
		"ids.json": `{"https://www.schemastore.org/b.json": "bee"}`,
	})

	return e
}

func (e *schemaStoreEnv) options(t *testing.T, out string) Options {
	t.Helper()

	return Options{
		Tool: pinnedTool(t), SourceFile: e.source, PolicyFile: e.policy, IDsFile: e.ids, OutDir: out,
		SnapshotDir: e.snapshot, Fetcher: e.deps.fetcher(), Now: func() time.Time { return fixedNow }, Jobs: 3,
	}
}

func previousState() *state.State {
	return stateOf(
		stateRecord{id: "custom-c", source: "https://www.schemastore.org/c.json"},
		stateRecord{id: "dup", source: "https://www.schemastore.org/dup.json"},
		stateRecord{id: "excluded", source: "https://www.schemastore.org/excluded.json"},
		stateRecord{id: "withdep", source: "https://www.schemastore.org/gone.json"},
	)
}

func TestSchemaStore(t *testing.T) {
	e := newSchemaStoreEnv(t)
	out := filepath.Join(t.TempDir(), "prepared")

	logs := &logRecorder{}
	opts := e.options(t, out)
	opts.State = previousState()
	opts.Log = logs.logf

	res, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}

	logs.find(t, "warning: 2 published schema(s) could not be refreshed; the catalog keeps their last entry and artifact",
		"dup (prepare-failed), withdep (removed-upstream)")
	logs.find(t, "warning: 1 published schema(s) are excluded by an explicit rule and leave the catalog", "excluded (rule excluded)")

	report := readReport(t, out)
	status := map[string]string{}

	wantHeld := []Hold{{ID: "dup", Reason: state.HeldPrepareFailed}, {ID: "withdep", Reason: state.HeldRemovedUpstream}}
	wantExcluded := []Exclusion{{ID: "excluded", Rule: "excluded"}}

	if !reflect.DeepEqual(res.Held, wantHeld) || !reflect.DeepEqual(report.Held, wantHeld) {
		t.Errorf("held: result %v, report %v, want %v", res.Held, report.Held, wantHeld)
	}

	if !reflect.DeepEqual(res.Excluded, wantExcluded) || !reflect.DeepEqual(report.Excluded, wantExcluded) {
		t.Errorf("excluded: result %v, report %v, want %v", res.Excluded, report.Excluded, wantExcluded)
	}

	for _, rec := range report.Records {
		status[rec.Name] = rec.Status + "/" + rec.Reason
	}

	want := map[string]string{
		"A config": "included/pattern-unsupported", "A yaml": "included/", "B": "included/", "External": "pending-review/",
		"Excluded": "excluded/", "Fragment": "included/", "Duplicate keys": "failed/duplicate-keys",
		"Broken": "failed/invalid-json", "Missing": "failed/fetch-failed", "With dependency": "included/",
		"With dependency too": "included/", "Uses reviewed": "pending-review/", "C": "included/",
		"Bad\u0007name": "failed/invalid-metadata",
	}
	if !reflect.DeepEqual(status, want) {
		t.Errorf("statuses:\n got %v\nwant %v", status, want)
	}

	wantTotals := Totals{
		Records: 14, Entries: 6, Held: 2, Dropped: 1, Included: 7, Excluded: 1, PendingReview: 2, Failed: 4,
		PatternsDropped: 1, VersionsNotPublished: 2, Bundled: 4, CompactOnly: 2, BehaviourCompared: 1, StructuralOnly: 3,
	}
	if res.Totals != wantTotals || report.Totals != wantTotals {
		t.Errorf("totals = %+v, want %+v", res.Totals, wantTotals)
	}

	if len(res.Regressions) != 0 {
		t.Errorf("regressions = %+v; a published SchemaStore schema that fails is held, never a regression", res.Regressions)
	}

	if err := res.Check(); err != nil {
		t.Errorf("Check = %v", err)
	}

	if rec := recordByURL(t, report, "https://www.schemastore.org/dup.json"); rec.ID != "dup" || rec.Held != state.HeldPrepareFailed ||
		rec.Regression {
		t.Errorf("held record = %+v", rec)
	}

	a := recordByURL(t, report, "https://www.schemastore.org/a.json")
	if a.ID != "a" || !slices.Equal(a.DroppedPatterns, []string{"**/x/!(config).yml"}) || a.License != "Apache-2.0" || a.Rule != "schemastore" ||
		a.Reason != ReasonPatternUnsupported || !strings.Contains(a.Detail, "1 fileMatch pattern(s) dropped") {
		t.Errorf("A record = %+v", a)
	}

	if want := map[string]string{"1.0": "https://www.schemastore.org/a-1.0.json", "2.0": "https://www.schemastore.org/a.json"}; !reflect.DeepEqual(a.Versions, want) {
		t.Errorf("A versions = %v, want %v", a.Versions, want)
	}

	if a.Verification == nil || *a.Verification != (Verification{Method: VerifiedCompactOnly}) {
		t.Errorf("A verification = %+v", a.Verification)
	}

	if rec := recordByURL(t, report, "https://json.schemastore.org/a"); rec.MergedInto != "A config" || rec.ID != "a" || rec.Versions != nil {
		t.Errorf("merged record = %+v", rec)
	}

	if rec := recordByURL(t, report, "https://www.schemastore.org/b.json"); rec.Verification == nil ||
		*rec.Verification != (Verification{Method: VerifiedBehaviour, Valid: 1, Invalid: 1, SkippedNonJSON: 1}) {
		t.Errorf("B verification = %+v", rec.Verification)
	}

	if rec := recordByURL(t, report, "https://www.schemastore.org/withdep.json"); rec.Verification == nil ||
		*rec.Verification != (Verification{Method: VerifiedStructural}) {
		t.Errorf("withdep verification = %+v", rec.Verification)
	}

	if rec := recordByURL(t, report, "https://www.schemastore.org/missing.json"); rec.Verification != nil {
		t.Errorf("a failed record has a verification: %+v", rec)
	}

	if rec := recordByURL(t, report, "https://www.schemastore.org/usesreview.json"); rec.Rule != "review" ||
		!strings.Contains(rec.Detail, "dependency https://www.schemastore.org/partial-review.json") {
		t.Errorf("reviewed dependency record = %+v", rec)
	}

	if rec := recordByURL(t, report, "https://example.com/ext.json"); rec.Detail != "no license rule matched" || rec.Regression {
		t.Errorf("external record = %+v", rec)
	}

	if rec := recordByURL(t, report, "https://www.schemastore.org/excluded.json"); rec.Regression || rec.Rule != "excluded" ||
		rec.ID != "excluded" || rec.Held != "" {
		t.Errorf("excluded record = %+v", rec)
	}

	doc := mustLoad(t, out).Document
	if !reflect.DeepEqual(doc.Held, wantHeld) || !reflect.DeepEqual(doc.Excluded, wantExcluded) {
		t.Errorf("prepared.json held %v, excluded %v", doc.Held, doc.Excluded)
	}

	set := mustLoad(t, out)

	ids := make([]string, 0, len(set.Document.Entries))
	for _, entry := range set.Document.Entries {
		ids = append(ids, entry.ID)
	}

	if want := []string{"a", "a-name", "bee", "custom-c", "withdep-schemastore-org", "withdep2"}; !slices.Equal(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}

	if len(report.Collisions) != 1 || report.Collisions[0].ID != "withdep" ||
		!slices.Equal(report.Collisions[0].Sources, []string{"https://www.schemastore.org/gone.json", "https://www.schemastore.org/withdep.json"}) {
		t.Errorf("collisions = %+v", report.Collisions)
	}

	checkSchemaStoreEntries(t, e, set)

	if hits := e.deps.hitCount("/dep.json"); hits != 1 {
		t.Errorf("the shared dependency was requested %d times", hits)
	}

	if report.Source != (Source{Kind: KindSchemaStore, Commit: testCommit, TarballDigest: sha("tarball")}) {
		t.Errorf("report source = %+v", report.Source)
	}

	for _, rec := range report.Records {
		if rec.ID == "a-name" && (rec.SnapshotPath != "src/schemas/json/a.json" || rec.Verification.Method != VerifiedStructural) {
			t.Errorf("fragment record = %+v", rec)
		}
	}

	if rec := recordByURL(t, report, "https://raw.githubusercontent.com/SchemaStore/schemastore/master/src/schemas/json/c.json"); rec.SnapshotPath != "src/schemas/json/c.json" {
		t.Errorf("c record = %+v", rec)
	}

	logs.find(t, "warning: 3 of 4 bundled entries had no JSON test instance", "a-name, withdep-schemastore-org, withdep2")
}

func checkSchemaStoreEntries(t *testing.T, e *schemaStoreEnv, set *Set) {
	t.Helper()

	fragment := entryByID(t, set, "a-name")
	if fragment.Provenance.Source != "https://www.schemastore.org/a.json#/definitions/name" || fragment.Name != "Fragment" ||
		fragment.Provenance.SourceDigest != entryByID(t, set, "a").Provenance.SourceDigest || len(fragment.Provenance.Dependencies) != 0 ||
		fragment.Dialect != draft7 || fragment.Notice == "" {
		t.Errorf("fragment entry = %+v", fragment)
	}

	if schema := string(set.Schemas["a-name"]); !strings.HasPrefix(schema,
		`{"$schema":"`+draft7+`","allOf":[{"$ref":"https://json.schemastore.org/a.json#/definitions/name"}],"definitions":{"https://json.schemastore.org/a.json":{`) {
		t.Errorf("fragment schema = %s", schema)
	}

	a := entryByID(t, set, "a")
	if !slices.Equal(a.FileMatch, []string{"a.json", "**/.a/*.yml", "a.yaml"}) || a.Provenance.Source != "https://www.schemastore.org/a.json" ||
		a.Name != "A config" || a.Description != "A" {
		t.Errorf("a = %+v", a)
	}

	wantNotice := "Includes schemas from the JSON Schema Store repository.\n\nApache License\n\n" + upstreamTxt + "\n"
	if got := string(set.Notices[a.Notice]); got != wantNotice {
		t.Errorf("a notice = %q", got)
	}

	bee := entryByID(t, set, "bee")
	if len(bee.Provenance.Dependencies) != 1 || bee.Provenance.Dependencies[0].Source != "https://json.schemastore.org/a.json" {
		t.Errorf("bee dependencies = %+v", bee.Provenance.Dependencies)
	}

	c := entryByID(t, set, "custom-c")
	if string(set.Schemas["custom-c"]) != `{"type":"string"}` || c.Provenance.Source != "https://www.schemastore.org/c.json" ||
		c.Provenance.SourceDigest != sha(`{ "type" : "string" }`) || c.Dialect != "" {
		t.Errorf("c = %+v, schema %s", c, set.Schemas["custom-c"])
	}

	withdep := entryByID(t, set, "withdep-schemastore-org")
	if withdep.Provenance.License != "Apache-2.0 AND MIT" ||
		len(withdep.Provenance.Dependencies) != 1 || withdep.Provenance.Dependencies[0].Source != e.depURL {
		t.Errorf("withdep = %+v", withdep)
	}

	wantDepNotice := "Includes schemas from the JSON Schema Store repository.\n\nDependency notice.\n\nApache License\n\n" + upstreamTxt + "\n"
	if got := string(set.Notices[withdep.Notice]); got != wantDepNotice {
		t.Errorf("withdep notice = %q", got)
	}

	if !bytes.Contains(set.Schemas["withdep-schemastore-org"], []byte(`"$ref":"`+e.depURL+`#/definitions/port"`)) {
		t.Errorf("withdep bundle = %s", set.Schemas["withdep-schemastore-org"])
	}
}

// A malformed entry of the upstream catalog is one failed record, not an
// unusable snapshot: a published schema whose entry turned malformed is
// held, a new malformed entry stays out, one that repeats the URL of a valid
// entry leaves that entry alone, and every other record is prepared. Members
// Schepherd does not read are SchemaStore's to add and change nothing.
func TestSchemaStoreInvalidUpstreamEntries(t *testing.T) {
	e := newSchemaStoreEnv(t)
	first := filepath.Join(t.TempDir(), "first")

	if _, err := Run(t.Context(), e.options(t, first)); err != nil {
		t.Fatal(err)
	}

	recorded := stateFromSet(t, mustLoad(t, first))

	tree := schemaStoreTree(e.depURL)

	var doc map[string]any
	if err := json.Unmarshal([]byte(tree["src/api/json/catalog.json"]), &doc); err != nil {
		t.Fatal(err)
	}

	schemas, _ := doc["schemas"].([]any)
	for _, item := range schemas {
		switch entry, _ := item.(map[string]any); entry["name"] {
		case "With dependency":
			entry["fileMatch"] = "withdep.json"
		case "B":
			entry["tags"] = []string{"new upstream member"}
			entry["deprecated"] = nil
		}
	}

	doc["generated"] = "new upstream member"

	doc["schemas"] = append(schemas,
		map[string]any{"name": "New", "description": "n", "url": "relative.json"},
		map[string]any{"name": "Shadow", "url": "https://www.schemastore.org/b.json"},
	)

	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	tree["src/api/json/catalog.json"] = string(data)
	writeSnapshot(t, e.snapshot, tree)

	out := filepath.Join(t.TempDir(), "second")
	opts := e.options(t, out)
	opts.State = recorded

	res, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatalf("malformed upstream entries stopped the run: %v", err)
	}

	if !reflect.DeepEqual(res.Held, []Hold{{ID: "withdep", Reason: state.HeldPrepareFailed}}) || len(res.Excluded)+len(res.Regressions) != 0 {
		t.Errorf("held %v, excluded %v, regressions %v", res.Held, res.Excluded, res.Regressions)
	}

	report := readReport(t, out)

	withdep := recordByURL(t, report, "https://www.schemastore.org/withdep.json")
	if withdep.Status != StatusFailed || withdep.Reason != ReasonInvalidMetadata || withdep.ID != "withdep" || withdep.Held != state.HeldPrepareFailed ||
		!strings.Contains(withdep.Detail, `"fileMatch" has the wrong type`) {
		t.Errorf("withdep record = %+v", withdep)
	}

	for _, rec := range report.Records {
		if (rec.Name == "New" || rec.Name == "Shadow") && (rec.Status != StatusFailed || rec.Reason != ReasonInvalidMetadata || rec.ID != "") {
			t.Errorf("%s record = %+v", rec.Name, rec)
		}
	}

	if b := entryByID(t, mustLoad(t, out), "bee"); b.Name != "B" || b.Reused == nil {
		t.Errorf("the valid entry of the repeated URL = %+v", b)
	}
}

func TestSchemaStoreIDOverrideAliases(t *testing.T) {
	e := newSchemaStoreEnv(t)
	writeFiles(t, e.dir, map[string]string{"ids.json": `{
		"https://raw.githubusercontent.com/SchemaStore/schemastore/master/src/schemas/json/c.json": "sea",
		"https://json.schemastore.org/b.json": "bee",
		"https://json.schemastore.org/a": "ay",
		"https://www.schemastore.org/nowhere.json": "nowhere"
	}`})

	logs := &logRecorder{}
	out := filepath.Join(t.TempDir(), "prepared")
	opts := e.options(t, out)
	opts.State = previousState()
	opts.Log = logs.logf

	if _, err := Run(t.Context(), opts); err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, entry := range mustLoad(t, out).Document.Entries {
		got[entry.Provenance.Source] = entry.ID
	}

	for source, id := range map[string]string{
		"https://www.schemastore.org/a.json": "ay", "https://www.schemastore.org/b.json": "bee", "https://www.schemastore.org/c.json": "sea",
	} {
		if got[source] != id {
			t.Errorf("%s has ID %q, want %q (all: %v)", source, got[source], id, got)
		}
	}

	raw := "https://raw.githubusercontent.com/SchemaStore/schemastore/master/src/schemas/json/c.json"
	if rec := recordByURL(t, readReport(t, out), raw); rec.ID != "sea" {
		t.Errorf("record = %+v", rec)
	}

	logs.find(t, "warning: the ID override for https://www.schemastore.org/nowhere.json", "no effect")

	writeFiles(t, e.dir, map[string]string{"ids.json": `{
		"https://json.schemastore.org/b.json": "bee",
		"https://www.schemastore.org/b": "bea"
	}`})

	_, err := Run(t.Context(), e.options(t, filepath.Join(t.TempDir(), "conflict")))
	wantKind(t, err, fault.Usage)
	mustContain(t, err.Error(), "name the same source https://www.schemastore.org/b.json")
}

func TestSchemaStoreDeterministic(t *testing.T) {
	e := newSchemaStoreEnv(t)
	first := filepath.Join(t.TempDir(), "one")
	second := filepath.Join(t.TempDir(), "two")

	if _, err := Run(t.Context(), e.options(t, first)); err != nil {
		t.Fatal(err)
	}

	opts := e.options(t, second)
	opts.Jobs = 1
	opts.Now = func() time.Time { return fixedNow.Add(72 * time.Hour) }

	if _, err := Run(t.Context(), opts); err != nil {
		t.Fatal(err)
	}

	a, b := readTree(t, first), readTree(t, second)

	for name := range a {
		if name == ReportFile {
			continue
		}

		if !bytes.Equal(a[name], b[name]) {
			t.Errorf("%s differs between runs", name)
		}
	}

	if len(a) != len(b) {
		t.Errorf("runs wrote %d and %d files", len(a), len(b))
	}

	ra, rb := readReport(t, first), readReport(t, second)
	if ra.GeneratedAt == rb.GeneratedAt {
		t.Error("generatedAt did not change")
	}

	ra.GeneratedAt, rb.GeneratedAt = "", ""
	if !reflect.DeepEqual(ra, rb) {
		t.Error("reports differ beyond generatedAt")
	}
}

func TestSchemaStoreDependencyOnlyChange(t *testing.T) {
	e := newSchemaStoreEnv(t)
	before := filepath.Join(t.TempDir(), "before")
	after := filepath.Join(t.TempDir(), "after")

	if _, err := Run(t.Context(), e.options(t, before)); err != nil {
		t.Fatal(err)
	}

	e.deps.set("/dep.json", `{"$schema":"`+draft7+`","definitions":{"port":{"type":"integer","minimum":1,"maximum":65535}}}`)

	if _, err := Run(t.Context(), e.options(t, after)); err != nil {
		t.Fatal(err)
	}

	old, updated := mustLoad(t, before), mustLoad(t, after)

	for i, entry := range updated.Document.Entries {
		changed := entry.ContentDigest != old.Document.Entries[i].ContentDigest
		if want := entry.ID == "withdep" || entry.ID == "withdep2"; changed != want {
			t.Errorf("%s changed = %v, want %v", entry.ID, changed, want)
		}

		if entry.Provenance.SourceDigest != old.Document.Entries[i].Provenance.SourceDigest {
			t.Errorf("%s source digest changed", entry.ID)
		}
	}
}

func TestSchemaStoreSnapshotDownload(t *testing.T) {
	e := newSchemaStoreEnv(t)
	tarball := buildTarball(t, schemaStoreTree(e.depURL))
	e.deps.set("/tar.gz/"+testCommit, string(tarball))

	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	opts := e.options(t, filepath.Join(t.TempDir(), "temp-snapshot"))
	opts.SnapshotDir = ""

	res, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}

	if res.Source.TarballDigest != sha(string(tarball)) || res.Totals.Entries != 6 {
		t.Errorf("result = %+v", res)
	}

	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("temporary snapshot left behind: %v", entries)
	}

	kept := filepath.Join(t.TempDir(), "kept")

	for _, name := range []string{"first", "second"} {
		opts := e.options(t, filepath.Join(t.TempDir(), name))
		opts.SnapshotDir = kept

		if _, err := Run(t.Context(), opts); err != nil {
			t.Fatal(err)
		}
	}

	if hits := e.deps.hitCount("/tar.gz/" + testCommit); hits != 2 {
		t.Errorf("tarball downloads = %d, want 2 (one temporary, one kept and reused)", hits)
	}

	if _, err := os.Stat(filepath.Join(kept, "src", "api", "json", "catalog.json")); err != nil {
		t.Errorf("kept snapshot: %v", err)
	}
}

func buildTarball(t *testing.T, tree map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	top := "schemastore-" + testCommit + "/"

	if err := tw.WriteHeader(&tar.Header{Name: top, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(tree))
	for name := range tree {
		names = append(names, name)
	}

	slices.Sort(names)

	for _, name := range names {
		body := tree[name]
		if err := tw.WriteHeader(&tar.Header{Name: top + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}

		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func TestSchemaStoreConfigurationErrors(t *testing.T) {
	e := newSchemaStoreEnv(t)

	opts := e.options(t, filepath.Join(t.TempDir(), "out"))
	opts.PolicyFile = ""

	_, err := Run(t.Context(), opts)
	wantKind(t, err, fault.Usage)
	mustContain(t, err.Error(), "license policy")

	other := filepath.Join(t.TempDir(), "other")
	tree := schemaStoreTree(e.depURL)
	writeFiles(t, other, tree)
	writeFiles(t, other, map[string]string{
		"snapshot.json": `{"commit":"` + strings.Repeat("f", 40) + `","tarballDigest":"` + sha("x") + `","formatVersion":1}`,
	})

	opts = e.options(t, filepath.Join(t.TempDir(), "out"))
	opts.SnapshotDir = other

	_, err = Run(t.Context(), opts)
	wantKind(t, err, fault.Usage)
	mustContain(t, err.Error(), "the source pins")

	busy := t.TempDir()
	writeFiles(t, busy, map[string]string{"keep.txt": "x"})

	_, err = Run(t.Context(), e.options(t, busy))
	wantKind(t, err, fault.Usage)

	if _, err := Run(t.Context(), Options{SourceFile: e.source, OutDir: busy}); fault.KindOf(err) != fault.Usage {
		t.Errorf("missing tool: %v", err)
	}
}

func TestSchemaStoreCanceled(t *testing.T) {
	e := newSchemaStoreEnv(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	out := filepath.Join(t.TempDir(), "out")

	_, err := Run(ctx, e.options(t, out))
	wantKind(t, err, fault.Canceled)

	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Errorf("a canceled run wrote %s", out)
	}
}

func TestBundleLimitsFromSource(t *testing.T) {
	src, err := loadSource(filepath.Join(newSchemaStoreEnv(t).dir, "schemastore.toml"))
	if err != nil {
		t.Fatal(err)
	}

	want := bundle.Limits{MaxDepth: 8, MaxDocuments: 64, MaxDocumentBytes: 1 << 20, MaxTotalBytes: 8 << 20}
	if src.limits != want || src.fetch.MaxBytes != 1<<20 || src.fetch.AllowHTTP {
		t.Errorf("limits = %+v, fetch = %+v", src.limits, src.fetch)
	}

	if _, err := loadSource(filepath.Join(t.TempDir(), "missing.toml")); fault.KindOf(err) != fault.Usage {
		t.Errorf("missing source: %v", err)
	}

	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"x.toml": `kind = "git"`})

	_, err = loadSource(filepath.Join(dir, "x.toml"))
	wantKind(t, err, fault.Usage)
	mustContain(t, err.Error(), fmt.Sprintf("%q", "git"))
}

// TestSchemaStoreTopLevelRefRoots covers the SchemaStore layout of the
// records that used to fail as top-level-ref-draft7: the file is retrieved
// from www.schemastore.org, declares its json.schemastore.org alias as $id
// (which draft-07 ignores next to $ref) and refers to a sibling file that is
// a top-level $ref itself.
func TestSchemaStoreTopLevelRefRoots(t *testing.T) {
	e := newSchemaStoreEnv(t)

	catalogDoc := `{"$schema":"https://www.schemastore.org/schema-catalog.json","version":1,"schemas":[
		{"name":"Shim","description":"S","url":"https://www.schemastore.org/shim.json","fileMatch":["shim.json"]},
		{"name":"Typed shim","description":"T","url":"https://www.schemastore.org/typed.json"},
		{"name":"Shimmed part","description":"P","url":"https://json.schemastore.org/shimmed.json#/definitions/part"},
		{"name":"Shimmed root","description":"R","url":"https://www.schemastore.org/shimmed.json#/definitions/root"}]}`

	e.snapshot = filepath.Join(t.TempDir(), "snapshot")
	writeSnapshot(t, e.snapshot, map[string]string{
		"LICENSE":                   "Apache License\n",
		"NOTICE":                    upstreamTxt + "\n",
		"src/api/json/catalog.json": catalogDoc,
		"src/schemas/json/shim.json": `{"$schema":"` + draft7 + `","$id":"https://json.schemastore.org/shim.json",
			"$ref":"shimmed.json","title":"Shim"}`,
		"src/schemas/json/shimmed.json": `{"$schema":"` + draft7 + `","$id":"https://json.schemastore.org/shimmed.json",
			"$ref":"#/definitions/root","definitions":{"root":{"type":"object","required":["a"],"properties":{"p":{"$ref":"#/definitions/part"}}},
			"part":{"type":"string","maxLength":3}}}`,
		"src/schemas/json/typed.json":       `{"$schema":"` + draft7 + `","$ref":"shimmed.json","type":"object"}`,
		"src/test/shim/ok.json":             `{"a":1,"p":"abc"}`,
		"src/negative_test/shim/none.json":  `{}`,
		"src/negative_test/shim/long.json":  `{"a":1,"p":"abcd"}`,
		"src/negative_test/typed/none.json": `{}`,
	})

	// The override names the fragment through the other SchemaStore alias.
	writeFiles(t, e.dir, map[string]string{"ids.json": `{"https://json.schemastore.org/shimmed.json#/definitions/root": "shimmed-root-override"}`})

	out := filepath.Join(t.TempDir(), "prepared")
	if _, err := Run(t.Context(), e.options(t, out)); err != nil {
		t.Fatal(err)
	}

	report := readReport(t, out)

	if root := recordByURL(t, report, "https://www.schemastore.org/shimmed.json#/definitions/root"); root.Status != StatusIncluded ||
		root.ID != "shimmed-root-override" {
		t.Errorf("overridden fragment = %+v", root)
	}

	shim := recordByURL(t, report, "https://www.schemastore.org/shim.json")
	if shim.Status != StatusIncluded || shim.Verification == nil ||
		*shim.Verification != (Verification{Method: VerifiedBehaviour, Valid: 1, Invalid: 2}) {
		t.Errorf("shim = %+v, verification %+v", shim, shim.Verification)
	}

	if typed := recordByURL(t, report, "https://www.schemastore.org/typed.json"); typed.Status != StatusFailed ||
		typed.Reason != bundle.ReasonTopLevelRefDraft7 || !strings.Contains(typed.Detail, "next to type") {
		t.Errorf("typed = %+v", typed)
	}

	part := recordByURL(t, report, "https://json.schemastore.org/shimmed.json#/definitions/part")
	if part.Status != StatusIncluded || part.ID != "shimmed-part" || part.Verification == nil || part.Verification.Method != VerifiedStructural {
		t.Errorf("part = %+v, verification %+v", part, part.Verification)
	}

	set := mustLoad(t, out)

	entry := entryByID(t, set, "shim")
	if len(entry.Provenance.Dependencies) != 1 || entry.Provenance.Dependencies[0].Source != "https://json.schemastore.org/shimmed.json" {
		t.Errorf("shim dependencies = %+v", entry.Provenance.Dependencies)
	}

	schema := string(set.Schemas["shim"])
	for _, want := range []string{
		`"$id":"https://json.schemastore.org/shim.json"`, `"allOf":[{"$ref":"shimmed.json"}]`,
		`"https://json.schemastore.org/shimmed.json":{"$schema":"` + draft7 + `","$id":"https://json.schemastore.org/shimmed.json","allOf":[{"$ref":"#/definitions/root"}]`,
	} {
		if !strings.Contains(schema, want) {
			t.Errorf("shim schema lacks %s: %s", want, schema)
		}
	}

	if got := entryByID(t, set, "shimmed-part").Provenance.Source; got != "https://www.schemastore.org/shimmed.json#/definitions/part" {
		t.Errorf("part source = %s", got)
	}
}

// TestRewrittenRootIdentityNeedsTheLicensePolicy covers a draft-07 root that
// is a top-level $ref and declares an $id on a host the policy does not
// allow: confirming that the identifier serves the same bytes would mean
// requesting it, so the record is held for review and nothing is requested.
func TestRewrittenRootIdentityNeedsTheLicensePolicy(t *testing.T) {
	e := newSchemaStoreEnv(t)
	alias := e.deps.srv.URL + "/alias/shim.json"

	catalogDoc := `{"$schema":"https://www.schemastore.org/schema-catalog.json","version":1,"schemas":[
		{"name":"Shim","description":"S","url":"https://www.schemastore.org/shim.json","fileMatch":["shim.json"]}]}`

	e.snapshot = filepath.Join(t.TempDir(), "snapshot")
	writeSnapshot(t, e.snapshot, map[string]string{
		"LICENSE":                   "Apache License\n",
		"NOTICE":                    upstreamTxt + "\n",
		"src/api/json/catalog.json": catalogDoc,
		"src/schemas/json/shim.json": `{"$schema":"` + draft7 + `","$id":"` + alias + `",
			"$ref":"https://json.schemastore.org/shimmed.json"}`,
		"src/schemas/json/shimmed.json": `{"$schema":"` + draft7 + `","type":"object"}`,
	})

	out := filepath.Join(t.TempDir(), "prepared")
	if _, err := Run(t.Context(), e.options(t, out)); err != nil {
		t.Fatal(err)
	}

	shim := recordByURL(t, readReport(t, out), "https://www.schemastore.org/shim.json")
	if shim.Status != StatusPendingReview || !strings.Contains(shim.Detail, "dependency "+alias) || shim.Rule != "" {
		t.Errorf("shim = %+v", shim)
	}

	e.deps.mu.Lock()
	defer e.deps.mu.Unlock()

	if hits := e.deps.hits["/alias/shim.json"]; hits != 0 {
		t.Errorf("the unreviewed identifier was requested %d time(s)", hits)
	}
}
