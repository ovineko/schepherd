package prepare

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

func runLocalBasic(t *testing.T, out string) *Result {
	t.Helper()

	res, err := Run(t.Context(), Options{
		Tool:       pinnedTool(t),
		SourceFile: filepath.Join(localBasic, "source.toml"),
		OutDir:     out,
		Now:        func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if err := res.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}

	return res
}

func TestLocalBasic(t *testing.T) {
	out := filepath.Join(t.TempDir(), "prepared")
	res := runLocalBasic(t, out)

	want := Totals{Records: 4, Entries: 4, Included: 4, Bundled: 1, CompactOnly: 3, BehaviourCompared: 1}
	if res.Totals != want {
		t.Errorf("totals = %+v, want %+v", res.Totals, want)
	}

	if res.Source != (Source{Kind: KindLocal, Name: "basic"}) {
		t.Errorf("source = %+v", res.Source)
	}

	set := mustLoad(t, out)

	if got := set.Document.Entries; len(got) != 4 || got[0].ID != "alpha" || got[1].ID != "beta" || got[2].ID != "delta-schema" || got[3].ID != "gamma" {
		t.Fatalf("entries = %+v", got)
	}

	alpha := entryByID(t, set, "alpha")
	if alpha.Dialect != "http://json-schema.org/draft-07/schema#" || alpha.Provenance.License != "MIT" || alpha.Notice != "" ||
		!slices.Equal(alpha.FileMatch, []string{"alpha.json", "**/.github/workflows/*.yml"}) {
		t.Errorf("alpha = %+v", alpha)
	}

	raw, err := os.ReadFile(filepath.Join(localBasic, "schemas", "alpha.json"))
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(set.Schemas["alpha"], compactJSON(t, string(raw))) {
		t.Errorf("alpha was not only compacted: %s", set.Schemas["alpha"])
	}

	if alpha.Provenance.SourceDigest != sha(string(raw)) {
		t.Errorf("alpha source digest = %s", alpha.Provenance.SourceDigest)
	}

	beta := entryByID(t, set, "beta")
	if len(beta.Provenance.Dependencies) != 1 || beta.Provenance.Dependencies[0].Source != "https://schemas.example/common.json" {
		t.Errorf("beta dependencies = %+v", beta.Provenance.Dependencies)
	}

	if !bytes.Contains(set.Schemas["beta"], []byte(`"$id":"https://schemas.example/common.json"`)) ||
		!bytes.Contains(set.Schemas["beta"], []byte(`"$ref":"common.json#/$defs/name"`)) {
		t.Errorf("beta bundle = %s", set.Schemas["beta"])
	}

	delta := entryByID(t, set, "delta-schema")
	if delta.Provenance.License != "MIT" || delta.Notice == "" {
		t.Fatalf("delta = %+v", delta)
	}

	if got := string(set.Notices[delta.Notice]); got != "Schepherd test fixtures (MIT).\n" {
		t.Errorf("delta notice = %q", got)
	}

	if len(delta.FileMatch) != 0 || delta.FileMatch == nil {
		t.Errorf("delta fileMatch = %#v", delta.FileMatch)
	}

	gamma := entryByID(t, set, "gamma")
	if gamma.Provenance.License != "Apache-2.0" || len(set.Schemas["gamma"]) < 4096 {
		t.Errorf("gamma = %+v (%d bytes)", gamma, len(set.Schemas["gamma"]))
	}

	report := readReport(t, out)
	if report.GeneratedAt != "2026-09-23T12:00:00Z" || len(report.Records) != 4 || len(report.Collisions) != 0 {
		t.Errorf("report = %+v", report)
	}

	if rec := recordByURL(t, report, "https://schemas.example/delta.json"); rec.Status != StatusIncluded || rec.ID != "delta-schema" || rec.Rule != "fixtures" {
		t.Errorf("delta record = %+v", rec)
	}

	if rec := recordByURL(t, report, "https://schemas.example/beta.json"); rec.Verification == nil ||
		*rec.Verification != (Verification{Method: VerifiedBehaviour, Valid: 1, Invalid: 1}) {
		t.Errorf("beta verification = %+v", rec.Verification)
	}

	if rec := recordByURL(t, report, "https://schemas.example/alpha.json"); rec.Verification == nil ||
		*rec.Verification != (Verification{Method: VerifiedCompactOnly}) {
		t.Errorf("alpha verification = %+v", rec.Verification)
	}

	if rec := recordByURL(t, report, "https://schemas.example/beta.json"); rec.SnapshotPath != "" || rec.Reused {
		t.Errorf("beta record = %+v", rec)
	}

	for name := range readTree(t, out) {
		if strings.HasPrefix(name, ".") || (!strings.HasPrefix(name, SchemasDir+"/") && !strings.HasPrefix(name, NoticesDir+"/") && name != PreparedFile && name != ReportFile) {
			t.Errorf("unexpected file %s", name)
		}
	}
}

func TestLocalBasicMatchesSchemaAndIsDeterministic(t *testing.T) {
	first := filepath.Join(t.TempDir(), "one")
	second := filepath.Join(t.TempDir(), "two")

	runLocalBasic(t, first)
	runLocalBasic(t, second)

	a, b := readTree(t, first), readTree(t, second)
	if !reflect.DeepEqual(a, b) {
		t.Error("two runs with the same clock produced different files")
	}

	if err := validateAgainstSchema(compilePreparedSchema(t), a[PreparedFile]); err != nil {
		t.Errorf("api/prepared.schema.json rejects prepare output: %v", err)
	}
}

// localEnv is a local source in a temporary directory with a dependency
// server that the source may reach over plain HTTP.
type localEnv struct {
	deps *depServer
	dir  string
}

func newLocalEnv(t *testing.T, files map[string]string) *localEnv {
	t.Helper()

	e := &localEnv{deps: newDepServer(t), dir: t.TempDir()}

	for name, content := range files {
		files[name] = strings.ReplaceAll(content, "{{server}}", e.deps.srv.URL)
		files[name] = strings.ReplaceAll(files[name], "{{addr}}", e.deps.srv.Listener.Addr().String())
	}

	writeFiles(t, e.dir, files)

	return e
}

func (e *localEnv) run(t *testing.T, previous *state.State) (*Result, string, error) {
	t.Helper()

	out := filepath.Join(t.TempDir(), "out")

	res, err := Run(t.Context(), Options{
		Tool: pinnedTool(t), SourceFile: filepath.Join(e.dir, "source.toml"), OutDir: out, State: previous,
		Now: func() time.Time { return fixedNow },
	})

	return res, out, err
}

func TestLocalNetworkDependencies(t *testing.T) {
	e := newLocalEnv(t, map[string]string{
		"source.toml": `kind = "local"
name = "deps"
policy = "licenses.toml"

[fetch]
allow_http = true
allow_private_hosts = ["{{addr}}"]

[[entries]]
name = "Root"
url = "https://schemas.example/root.json"
file = "root.json"
license = "MIT"
instances = ["valid.json", "invalid.json", "note.yaml"]

[[entries]]
name = "Remote"
url = "{{server}}/remote.json"

[[entries]]
name = "Missing remote"
url = "{{server}}/missing.json"
license = "MIT"
`,
		"licenses.toml": `[[rules]]
id = "server"
decision = "allow"
urls = ["{{server}}/dep.json", "{{server}}/remote.json"]
license = "BSD-3-Clause"
reason = "test server"
`,
		"root.json":    `{"$schema":"` + draft7 + `","type":"object","properties":{"port":{"$ref":"{{server}}/dep.json#/definitions/port"}}}`,
		"valid.json":   `{"port":80}`,
		"invalid.json": `{"port":0}`,
		"note.yaml":    "port: 80\n",
	})
	e.deps.set("/dep.json", `{"$schema":"`+draft7+`","definitions":{"port":{"type":"integer","minimum":1}}}`)
	e.deps.set("/remote.json", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"boolean"}`)

	res, out, err := e.run(t, nil)
	if err != nil {
		t.Fatal(err)
	}

	report := readReport(t, out)

	if rec := recordByURL(t, report, e.deps.srv.URL+"/missing.json"); rec.Status != StatusFailed || rec.Reason != ReasonFetchFailed ||
		!rec.Regression || !strings.Contains(rec.Detail, "404") {
		t.Errorf("missing record = %+v", rec)
	}

	wantKind(t, res.Check(), fault.Integrity)

	set := mustLoad(t, out)

	root := entryByID(t, set, "root")
	if root.Provenance.License != "BSD-3-Clause AND MIT" || len(root.Provenance.Dependencies) != 1 ||
		root.Provenance.Dependencies[0].Digest != sha(`{"$schema":"`+draft7+`","definitions":{"port":{"type":"integer","minimum":1}}}`) {
		t.Errorf("root = %+v", root)
	}

	remote := entryByID(t, set, "remote")
	if remote.Provenance.License != "BSD-3-Clause" || string(set.Schemas["remote"]) != `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"boolean"}` {
		t.Errorf("remote = %+v", remote)
	}

	if !strings.Contains(string(set.Schemas["root"]), `"$id":"`+e.deps.srv.URL+`/dep.json"`) {
		t.Errorf("root bundle = %s", set.Schemas["root"])
	}
}

func TestLocalHeldAndFailedEntriesAreRegressions(t *testing.T) {
	e := newLocalEnv(t, map[string]string{
		"source.toml": `kind = "local"
name = "held"

[[entries]]
name = "No license"
url = "https://schemas.example/nolicense.json"
file = "a.json"

[[entries]]
name = "Unreviewed dependency"
url = "https://schemas.example/dep-user.json"
file = "b.json"
license = "MIT"

[[entries]]
name = "Depends on an unlicensed entry"
url = "https://schemas.example/http-user.json"
file = "c.json"
license = "MIT"

[[documents]]
uri = "{{server}}/allowed.json"
file = "allowed.json"
license = "MIT"
`,
		"a.json":       `{"type":"string"}`,
		"b.json":       `{"$schema":"` + draft7 + `","properties":{"x":{"$ref":"https://elsewhere.example/x.json"}}}`,
		"c.json":       `{"$schema":"` + draft7 + `","properties":{"x":{"$ref":"{{server}}/allowed.json"},"y":{"$ref":"https://schemas.example/nolicense.json"}}}`,
		"allowed.json": `{"type":"integer"}`,
	})

	res, out, err := e.run(t, nil)
	if err != nil {
		t.Fatal(err)
	}

	report := readReport(t, out)

	if rec := recordByURL(t, report, "https://schemas.example/nolicense.json"); rec.Status != StatusPendingReview || !rec.Regression {
		t.Errorf("no license = %+v", rec)
	}

	if rec := recordByURL(t, report, "https://schemas.example/dep-user.json"); rec.Status != StatusPendingReview ||
		!strings.Contains(rec.Detail, "dependency https://elsewhere.example/x.json") {
		t.Errorf("unreviewed dependency = %+v", rec)
	}

	if rec := recordByURL(t, report, "https://schemas.example/http-user.json"); rec.Status != StatusPendingReview ||
		!strings.Contains(rec.Detail, "https://schemas.example/nolicense.json") {
		t.Errorf("entry using an unlicensed entry = %+v", rec)
	}

	if e.deps.hitCount("/allowed.json") != 0 {
		t.Error("a document served from a local file was fetched")
	}

	if res.Totals.Regressions != 3 || res.Totals.Entries != 0 {
		t.Errorf("totals = %+v", res.Totals)
	}
}

func TestLocalIDsAndCollisions(t *testing.T) {
	e := newLocalEnv(t, map[string]string{
		"source.toml": `kind = "local"
name = "ids"
ids = "ids.json"

[[entries]]
name = "One"
url = "https://one.example/x.json"
file = "x.json"
license = "MIT"

[[entries]]
name = "Two"
url = "https://two.example/x.json"
file = "x.json"
license = "MIT"

[[entries]]
name = "Kept"
url = "https://three.example/renamed.json"
file = "x.json"
license = "MIT"

[[entries]]
name = "Overridden"
url = "https://four.example/y.json"
file = "x.json"
license = "MIT"
`,
		"x.json":   `{"type":"object"}`,
		"ids.json": `{"https://four.example/y.json": "four"}`,
	})

	previous := stateOf(stateRecord{id: "old-name", source: "https://three.example/renamed.json"})

	res, out, err := e.run(t, previous)
	if err != nil {
		t.Fatal(err)
	}

	if err := res.Check(); err != nil {
		t.Fatal(err)
	}

	set := mustLoad(t, out)

	got := map[string]string{}
	for _, entry := range set.Document.Entries {
		got[entry.Provenance.Source] = entry.ID
	}

	want := map[string]string{
		"https://one.example/x.json": "x", "https://two.example/x.json": "x-two-example",
		"https://three.example/renamed.json": "old-name", "https://four.example/y.json": "four",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ids = %v, want %v", got, want)
	}

	if len(res.Collisions) != 1 || res.Collisions[0].ID != "x" {
		t.Errorf("collisions = %+v", res.Collisions)
	}

	if set.Document.Entries[0].Notice != "" {
		t.Errorf("an entry without policy notices got one: %+v", set.Document.Entries[0])
	}
}

// A published schema that an override renamed leaves its old ID in the
// state, where publish keeps it; no other source may derive it.
func TestStateKeepsRenamedIDsReserved(t *testing.T) {
	e := newLocalEnv(t, map[string]string{
		"source.toml": `kind = "local"
name = "ids"

[[entries]]
name = "One"
url = "https://one.example/x.json"
file = "x.json"
license = "MIT"

[[entries]]
name = "Renamed"
url = "https://three.example/renamed.json"
file = "x.json"
license = "MIT"
`,
		"x.json": `{"type":"object"}`,
	})

	previous := stateOf(
		stateRecord{id: "old-name", source: "https://three.example/renamed.json"},
		stateRecord{id: "x", source: "https://three.example/renamed.json", removed: true},
	)

	res, out, err := e.run(t, previous)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, entry := range mustLoad(t, out).Document.Entries {
		got[entry.Provenance.Source] = entry.ID
	}

	want := map[string]string{"https://one.example/x.json": "x-one-example", "https://three.example/renamed.json": "old-name"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ids = %v, want %v", got, want)
	}

	if want := []Hold{{ID: "x", Reason: state.HeldRemovedUpstream}}; !reflect.DeepEqual(res.Held, want) {
		t.Errorf("held = %v, want %v: old-name keeps the listed source, x lost it", res.Held, want)
	}
}

func TestPreviousIDsPreferTheListedRecord(t *testing.T) {
	const source = "https://schemas.example/s.json"

	got := previousIDs(stateOf(
		stateRecord{id: "a-old", source: source, removed: true},
		stateRecord{id: "b-new", source: source},
		stateRecord{id: "c", source: "https://schemas.example/c.json"},
	))

	want := map[string]string{
		source: "b-new", reservedSource + "a-old": "a-old", "https://schemas.example/c.json": "c",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("previousIDs = %v, want %v", got, want)
	}

	if got := previousIDs(nil); len(got) != 0 {
		t.Errorf("previousIDs(nil) = %v", got)
	}
}

func TestLocalSourceErrors(t *testing.T) {
	base := `kind = "local"
name = "bad"
`
	entry := `
[[entries]]
name = "A"
url = "https://schemas.example/a.json"
file = "a.json"
`

	cases := map[string]string{
		"no name":            "kind = \"local\"\n" + entry,
		"no entries":         base,
		"unknown key":        base + entry + "colour = \"red\"\n",
		"wrong case":         base + entry + "File_Match = []\n",
		"missing file":       base + strings.Replace(entry, `file = "a.json"`, `file = "missing.json"`, 1),
		"escaping file":      base + strings.Replace(entry, `file = "a.json"`, `file = "../a.json"`, 1),
		"fragment url":       base + strings.Replace(entry, "a.json\"\nfile", "a.json#/x\"\nfile", 1),
		"credentials":        base + strings.Replace(entry, "https://", "https://user:pw@", 1),
		"bad license":        base + entry + "license = \"MIT; GPL\"\n",
		"bad id":             base + entry + "id = \"Upper\"\n",
		"extglob":            base + entry + "file_match = [\"!(x).json\"]\n",
		"duplicate url":      base + entry + entry,
		"duplicate id":       base + entry + "id = \"a\"\n" + strings.Replace(entry, "a.json\"\nfile", "b.json\"\nfile", 1) + "id = \"a\"\n",
		"document conflicts": base + entry + "\n[[documents]]\nuri = \"https://schemas.example/a.json\"\nfile = \"a.json\"\n",
		"negative limit":     base + "\n[dependencies]\nmax_depth = -1\n" + entry,
		"missing policy":     base + "policy = \"nope.toml\"\n" + entry,
		"missing instance":   base + entry + "instances = [\"nope.json\"]\n",
		"control in name":    base + strings.Replace(entry, `name = "A"`, `name = "A\u0007"`, 1),
	}

	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"source.toml": source, "a.json": `{}`})

			_, err := Run(t.Context(), Options{Tool: pinnedTool(t), SourceFile: filepath.Join(dir, "source.toml"), OutDir: filepath.Join(dir, "out")})
			wantKind(t, err, fault.Usage)
		})
	}

	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"source.toml": base + entry + "id = \"a\"\n", "a.json": `{}`})

	for _, overrides := range []string{
		`{"https://schemas.example/a.json": "other"}`,
		`{"https://SCHEMAS.example/a.json": "other"}`,
	} {
		writeFiles(t, dir, map[string]string{"ids.json": overrides})

		_, err := Run(t.Context(), Options{
			Tool: pinnedTool(t), SourceFile: filepath.Join(dir, "source.toml"), IDsFile: filepath.Join(dir, "ids.json"),
			OutDir: filepath.Join(dir, "out"),
		})
		wantKind(t, err, fault.Usage)
		mustContain(t, err.Error(), "ID overrides")
	}
}

func TestLocalPlainHTTPNeedsOptIn(t *testing.T) {
	e := newLocalEnv(t, map[string]string{
		"source.toml": `kind = "local"
name = "http"
policy = "licenses.toml"

[[entries]]
name = "Plain HTTP dependency"
url = "https://schemas.example/root.json"
file = "root.json"
license = "MIT"
`,
		"licenses.toml": `[[rules]]
id = "server"
decision = "allow"
urls = ["{{server}}/plain.json"]
license = "MIT"
reason = "test server"
`,
		"root.json": `{"$schema":"` + draft7 + `","properties":{"x":{"$ref":"{{server}}/plain.json"}}}`,
	})
	e.deps.set("/plain.json", `{"type":"string"}`)

	res, out, err := e.run(t, nil)
	if err != nil {
		t.Fatal(err)
	}

	rec := recordByURL(t, readReport(t, out), "https://schemas.example/root.json")
	if rec.Status != StatusFailed || rec.Reason != ReasonFetchFailed || !strings.Contains(rec.Detail, "scheme") || !rec.Regression {
		t.Errorf("record = %+v", rec)
	}

	if e.deps.hitCount("/plain.json") != 0 {
		t.Error("plain HTTP was used without allow_http")
	}

	wantKind(t, res.Check(), fault.Integrity)
}

func TestLocalRedirectTargetsNeedTheirOwnRule(t *testing.T) {
	e := newLocalEnv(t, map[string]string{
		"source.toml": `kind = "local"
name = "redirects"
policy = "licenses.toml"

[fetch]
allow_http = true
allow_private_hosts = ["{{addr}}"]

[[entries]]
name = "Unreviewed target"
url = "https://schemas.example/unreviewed-user.json"
file = "unreviewed-user.json"
license = "MIT"

[[entries]]
name = "Excluded target"
url = "https://schemas.example/excluded-user.json"
file = "excluded-user.json"
license = "MIT"

[[entries]]
name = "Allowed target"
url = "https://schemas.example/allowed-user.json"
file = "allowed-user.json"
license = "MIT"

[[entries]]
name = "Redirected root"
url = "{{server}}/root.json"
`,
		"licenses.toml": `[[rules]]
id = "requested"
decision = "allow"
urls = ["{{server}}/dep.json", "{{server}}/dep-excluded.json", "{{server}}/dep-moved.json", "{{server}}/root.json"]
license = "BSD-3-Clause"
reason = "requested URLs"

[[rules]]
id = "moved"
decision = "allow"
urls = ["{{server}}/moved.json"]
license = "Apache-2.0"
notice = "Moved notice."
reason = "redirect target"

[[rules]]
id = "banned"
decision = "exclude"
urls = ["{{server}}/banned.json"]
reason = "banned target"
`,
		"unreviewed-user.json": `{"$schema":"` + draft7 + `","properties":{"x":{"$ref":"{{server}}/dep.json"}}}`,
		"excluded-user.json":   `{"$schema":"` + draft7 + `","properties":{"x":{"$ref":"{{server}}/dep-excluded.json"}}}`,
		"allowed-user.json":    `{"$schema":"` + draft7 + `","properties":{"x":{"$ref":"{{server}}/dep-moved.json"}}}`,
	})

	for from, to := range map[string]string{
		"/dep.json": "/unreviewed.json", "/dep-excluded.json": "/banned.json", "/dep-moved.json": "/moved.json",
		"/root.json": "/root-unreviewed.json",
	} {
		e.deps.redirect(from, to)
		e.deps.set(to, `{"$schema":"`+draft7+`","type":"integer","minimum":7}`)
	}

	server := e.deps.srv.URL

	res, out, err := e.run(t, nil)
	if err != nil {
		t.Fatal(err)
	}

	report := readReport(t, out)

	if rec := recordByURL(t, report, "https://schemas.example/unreviewed-user.json"); rec.Status != StatusPendingReview || !rec.Regression ||
		!strings.Contains(rec.Detail, "dependency "+server+"/dep.json: redirected to "+server+"/unreviewed.json: "+"no license rule matched") {
		t.Errorf("unreviewed redirect target = %+v", rec)
	}

	if rec := recordByURL(t, report, "https://schemas.example/excluded-user.json"); rec.Status != StatusExcluded || rec.Rule != "banned" ||
		!strings.Contains(rec.Detail, "redirected to "+server+"/banned.json") {
		t.Errorf("excluded redirect target = %+v", rec)
	}

	if rec := recordByURL(t, report, server+"/root.json"); rec.Status != StatusPendingReview ||
		!strings.Contains(rec.Detail, "redirected to "+server+"/root-unreviewed.json") {
		t.Errorf("redirected root = %+v", rec)
	}

	for _, target := range []string{"/unreviewed.json", "/banned.json", "/root-unreviewed.json"} {
		if hits := e.deps.hitCount(target); hits != 0 {
			t.Errorf("redirect target %s the policy does not allow was contacted %d times", target, hits)
		}
	}

	if hits := e.deps.hitCount("/moved.json"); hits != 1 {
		t.Errorf("allowed redirect target was requested %d times, want 1", hits)
	}

	set := mustLoad(t, out)

	if len(set.Document.Entries) != 1 || res.Totals.Regressions != 3 {
		t.Fatalf("entries = %+v, totals = %+v", set.Document.Entries, res.Totals)
	}

	allowed := entryByID(t, set, "allowed-user")
	if allowed.Provenance.License != "Apache-2.0 AND BSD-3-Clause AND MIT" || string(set.Notices[allowed.Notice]) != "Moved notice.\n" {
		t.Errorf("allowed redirect target = %+v, notice %q", allowed, set.Notices[allowed.Notice])
	}

	redirect := state.Redirect{URL: server + "/dep-moved.json", Target: server + "/moved.json", Digest: sha(`{"$schema":"` + draft7 + `","type":"integer","minimum":7}`)}
	if !slices.Equal(allowed.License.Rules, []string{"moved", "requested"}) || !slices.Equal(allowed.License.Redirects, []state.Redirect{redirect}) {
		t.Errorf("allowed redirect target decision = %+v", allowed.License)
	}
}

func TestLocalDeclaredLicenseDoesNotOverrideRules(t *testing.T) {
	e := newLocalEnv(t, map[string]string{
		"source.toml": `kind = "local"
name = "declared"
policy = "licenses.toml"

[[entries]]
name = "Excluded root"
url = "https://schemas.example/root.json"
file = "plain.json"
license = "MIT"

[[entries]]
name = "Excluded dependency"
url = "https://schemas.example/user.json"
file = "user.json"
license = "MIT"

[[entries]]
name = "Held root"
url = "https://review.example/held.json"
file = "plain.json"
license = "MIT"

[[entries]]
name = "Declared over an allow rule"
url = "https://other.example/ok.json"
file = "plain.json"
license = "MIT"

[[documents]]
uri = "https://banned.example/dep.json"
file = "dep.json"
license = "MIT"
`,
		"licenses.toml": `[[rules]]
id = "banned-host"
decision = "exclude"
hosts = ["banned.example"]
reason = "banned host"

[[rules]]
id = "banned-root"
decision = "exclude"
urls = ["https://schemas.example/root.json"]
reason = "banned root"

[[rules]]
id = "hold"
decision = "review"
hosts = ["review.example"]
reason = "license unclear"

[[rules]]
id = "other"
decision = "allow"
hosts = ["other.example"]
license = "Apache-2.0"
reason = "host allowed"
`,
		"plain.json": `{"type":"string"}`,
		"user.json":  `{"$schema":"` + draft7 + `","properties":{"x":{"$ref":"https://banned.example/dep.json"}}}`,
		"dep.json":   `{"$schema":"` + draft7 + `","type":"integer"}`,
	})

	res, out, err := e.run(t, nil)
	if err != nil {
		t.Fatal(err)
	}

	report := readReport(t, out)

	want := map[string][2]string{
		"https://schemas.example/root.json": {StatusExcluded, "banned-root"},
		"https://schemas.example/user.json": {StatusExcluded, "banned-host"},
		"https://review.example/held.json":  {StatusPendingReview, "hold"},
		"https://other.example/ok.json":     {StatusIncluded, ""},
	}

	for url, w := range want {
		if rec := recordByURL(t, report, url); rec.Status != w[0] || rec.Rule != w[1] {
			t.Errorf("%s = %+v, want status %s by rule %q", url, rec, w[0], w[1])
		}
	}

	if rec := recordByURL(t, report, "https://schemas.example/user.json"); !strings.Contains(rec.Detail, "dependency https://banned.example/dep.json") {
		t.Errorf("excluded dependency = %+v", rec)
	}

	if ok := entryByID(t, mustLoad(t, out), "ok"); ok.Provenance.License != "MIT" {
		t.Errorf("declared license over an allow rule = %+v", ok)
	}

	if res.Totals.Regressions != 3 {
		t.Errorf("totals = %+v", res.Totals)
	}
}

func TestLocalNonAssertedLicensesAreHeldForReview(t *testing.T) {
	e := newLocalEnv(t, map[string]string{
		"source.toml": `kind = "local"
name = "noassertion"
policy = "licenses.toml"

[[entries]]
name = "Declares NOASSERTION"
url = "https://schemas.example/noassertion.json"
file = "plain.json"
license = "NOASSERTION"

[[entries]]
name = "Declares NONE over an allow rule"
url = "https://allowed.example/none.json"
file = "plain.json"
license = "none"

[[entries]]
name = "Declares a file pointer"
url = "https://schemas.example/pointer.json"
file = "plain.json"
license = "SEE LICENSE IN LICENSE.md"

[[entries]]
name = "Dependency declares UNLICENSED"
url = "https://schemas.example/user.json"
file = "user.json"
license = "MIT"

[[entries]]
name = "Asserted"
url = "https://schemas.example/ok.json"
file = "plain.json"
license = "MIT"

[[documents]]
uri = "https://schemas.example/dep.json"
file = "dep.json"
license = "UNLICENSED"
`,
		"licenses.toml": `[[rules]]
id = "allowed-host"
decision = "allow"
hosts = ["allowed.example"]
license = "Apache-2.0"
reason = "host allowed"
`,
		"plain.json": `{"type":"string"}`,
		"user.json":  `{"$schema":"` + draft7 + `","properties":{"x":{"$ref":"https://schemas.example/dep.json"}}}`,
		"dep.json":   `{"$schema":"` + draft7 + `","type":"integer"}`,
	})

	res, out, err := e.run(t, nil)
	if err != nil {
		t.Fatal(err)
	}

	report := readReport(t, out)

	for url, want := range map[string]string{
		"https://schemas.example/noassertion.json": "NOASSERTION grants no permission to redistribute",
		"https://allowed.example/none.json":        "none grants no permission to redistribute",
		"https://schemas.example/pointer.json":     "not an SPDX license expression",
		"https://schemas.example/user.json":        "dependency https://schemas.example/dep.json",
	} {
		rec := recordByURL(t, report, url)
		if rec.Status != StatusPendingReview || !rec.Regression || !strings.Contains(rec.Detail, want) || rec.License != "" {
			t.Errorf("%s = %+v, want pending-review mentioning %q", url, rec, want)
		}
	}

	set := mustLoad(t, out)
	if len(set.Document.Entries) != 1 || set.Document.Entries[0].Provenance.License != "MIT" {
		t.Errorf("entries = %+v", set.Document.Entries)
	}

	if res.Totals.PendingReview != 4 || res.Totals.Regressions != 4 {
		t.Errorf("totals = %+v", res.Totals)
	}
}

func TestLocalAllowRuleWithoutAssertedLicenseIsAConfigurationError(t *testing.T) {
	for _, license := range []string{"NOASSERTION", "NONE", "SEE LICENSE IN LICENSE"} {
		e := newLocalEnv(t, map[string]string{
			"source.toml": `kind = "local"
name = "rule"
policy = "licenses.toml"

[[entries]]
name = "A"
url = "https://schemas.example/a.json"
file = "a.json"
`,
			"licenses.toml": `[[rules]]
id = "unasserted"
decision = "allow"
hosts = ["schemas.example"]
license = "` + license + `"
reason = "upstream metadata"
`,
			"a.json": `{"type":"string"}`,
		})

		_, out, err := e.run(t, nil)
		wantKind(t, err, fault.Usage)
		mustContain(t, err.Error(), "unasserted")

		if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
			t.Errorf("%s: a refused policy still wrote %s", license, out)
		}
	}
}
