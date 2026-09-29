package bundle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/jsonutil"
	"github.com/ovineko/schepherd/tools/pins"
)

const (
	draft4  = "http://json-schema.org/draft-04/schema#"
	draft7  = "http://json-schema.org/draft-07/schema#"
	d202012 = "https://json-schema.org/draft/2020-12/schema"
)

var errNotFound = errors.New("not found")

var pinnedToolPath = sync.OnceValues(func() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}

	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		return "", errors.New("not inside a Go module")
	}

	name := "jsonschema"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}

	return filepath.Join(filepath.Dir(gomod), ".tools", "bin", name), nil
})

// pinnedTool returns the pinned CLI installed in the module. Its absence is a
// test failure, never a skip: the integration tests are the only proof that
// the bundler behaves as the package assumes.
func pinnedTool(t *testing.T) *Tool {
	t.Helper()

	path, err := pinnedToolPath()
	if err != nil {
		t.Fatalf("locate module root: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the pinned JSON Schema CLI is missing at %s; install it with 'go run ./tools/install-jsonschema' from the module root", path)
	}

	tool, err := FindTool(path, PinnedVersion)
	if err != nil {
		t.Fatalf("FindTool: %v; reinstall it with 'go run ./tools/install-jsonschema -force'", err)
	}

	return tool
}

type fakeWeb struct {
	docs  map[string]string
	calls []string
}

func (w *fakeWeb) fetch(_ context.Context, uri string) (Document, error) {
	w.calls = append(w.calls, uri)

	body, ok := w.docs[uri]
	if !ok {
		return Document{}, fmt.Errorf("%w: %s", errNotFound, uri)
	}

	return Document{URI: uri, Bytes: []byte(body)}, nil
}

type run struct {
	tool      *Tool
	web       *fakeWeb
	closure   *Closure
	prepared  *Prepared
	err       error
	tmp       string
	rootURI   string
	root      string
	limits    Limits
	instances []Instance
}

func (r *run) do(t *testing.T) *run {
	t.Helper()

	r.tmp = isolateTemp(t)

	if r.web == nil {
		r.web = &fakeWeb{}
	}

	if r.tool == nil {
		r.tool = pinnedTool(t)
	}

	r.closure, r.err = Resolve(t.Context(), r.tool, Document{URI: r.rootURI, Bytes: []byte(r.root)}, r.web.fetch, r.limits)
	if r.err == nil {
		r.prepared, r.err = Prepare(t.Context(), r.tool, r.closure, r.instances)
	}

	entries, err := os.ReadDir(r.tmp)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 0 {
		t.Errorf("temporary files left behind: %v", entries)
	}

	return r
}

func (r *run) wantReason(t *testing.T, reason string) string {
	t.Helper()

	failure, ok := errors.AsType[*Failure](r.err)
	if !ok || failure.Reason != reason {
		t.Fatalf("err = %v, want Failure %s", r.err, reason)
	}

	t.Logf("failure: %v", failure)

	if strings.Contains(failure.Error(), r.tmp) {
		t.Errorf("failure leaks the temporary directory: %v", failure)
	}

	return failure.Detail
}

func (r *run) wantBundled(t *testing.T) *Prepared {
	t.Helper()

	if r.err != nil {
		t.Fatalf("err = %v", r.err)
	}

	p := r.prepared
	t.Logf("bundle: %s", p.Schema)

	if !p.Bundled {
		t.Fatalf("Bundled = false")
	}

	compact, err := jsonutil.Compact(p.Schema, 0)
	if err != nil || !bytes.Equal(compact, p.Schema) {
		t.Errorf("schema is not compact strict JSON: %v", err)
	}

	if bytes.Contains(p.Schema, []byte("file:")) || bytes.Contains(p.Schema, []byte(r.tmp)) {
		t.Errorf("schema mentions local files: %s", p.Schema)
	}

	if _, err := compile("https://registry.invalid/copy.json", p.Schema, p.Dialect, offlineLoader{}); err != nil {
		t.Errorf("bundle does not compile offline from another location: %v", err)
	}

	return p
}

func instances(pairs ...string) []Instance {
	var out []Instance
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Instance{Name: pairs[i], Data: []byte(pairs[i+1])})
	}

	return out
}

func TestPinnedVersionMatchesManifest(t *testing.T) {
	tool, err := pins.JSONSchema()
	if err != nil {
		t.Fatal(err)
	}

	if tool.Version != PinnedVersion {
		t.Errorf("tools/pins/jsonschema.json pins %s, the package requires %s", tool.Version, PinnedVersion)
	}
}

func TestFindTool(t *testing.T) {
	tool := pinnedTool(t)

	if tool.Version != PinnedVersion || !filepath.IsAbs(tool.Path) {
		t.Errorf("tool = %+v", tool)
	}

	if _, err := FindTool(tool.Path, "0.0.1"); fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), "install-jsonschema") {
		t.Errorf("wrong version: %v", err)
	}

	if _, err := FindTool(filepath.Join(t.TempDir(), "missing-jsonschema"), ""); fault.KindOf(err) != fault.Usage {
		t.Errorf("missing binary: %v", err)
	}
}

func TestDraft7RelativeAndAbsoluteDependencies(t *testing.T) {
	web := &fakeWeb{docs: map[string]string{
		"https://example.com/schemas/common.json": `{"$schema":"` + draft7 + `","$id":"https://example.com/schemas/common.json#",
			"definitions":{"name":{"type":"string","minLength":1,"pattern":"^(?!admin$)"}}}`,
		"https://example.com/other/age.json": `{"$schema":"` + draft7 + `","type":"integer","minimum":0}`,
	}}

	r := (&run{
		web:     web,
		rootURI: "https://cdn.example.net/root.json",
		root: `{"$schema":"` + draft7 + `","$id":"https://example.com/schemas/root.json","type":"object",
			"properties":{"name":{"$ref":"common.json#/definitions/name"},"age":{"$ref":"https://example.com/other/age.json"},
			"big":{"type":"integer","maximum":12345678901234567890123}},"required":["name"]}`,
		instances: instances(
			"valid.json", `{"name":"x","age":3,"big":1}`,
			"negative-age.json", `{"name":"x","age":-1}`,
			"missing-name.json", `{"age":3}`,
			"config.yaml", "name: x\n",
		),
	}).do(t)

	p := r.wantBundled(t)

	if want := []string{"https://example.com/other/age.json", "https://example.com/schemas/common.json"}; !reflect.DeepEqual(web.calls, want) {
		t.Errorf("fetch calls = %v, want %v", web.calls, want)
	}

	if len(p.Dependencies) != 2 || p.Dependencies[0].URI != "https://example.com/other/age.json" ||
		p.Dependencies[1].URI != "https://example.com/schemas/common.json" || !strings.HasPrefix(p.Dependencies[0].Digest, "sha256:") {
		t.Errorf("dependencies = %+v", p.Dependencies)
	}

	if want := (Checked{Valid: 1, Invalid: 2, Agreed: 3, SkippedNonJSON: 1}); p.Checked != want {
		t.Errorf("checked = %+v, want %+v", p.Checked, want)
	}

	if p.Dialect != draft7 {
		t.Errorf("dialect = %q", p.Dialect)
	}

	for _, want := range []string{`"$id":"https://example.com/schemas/root.json"`, `"$id":"https://example.com/other/age.json"`} {
		if !bytes.Contains(p.Schema, []byte(want)) {
			t.Errorf("bundle lacks %s: %s", want, p.Schema)
		}
	}
}

func TestDraft4WithID(t *testing.T) {
	web := &fakeWeb{docs: map[string]string{
		"https://example.com/d4/lib.json": `{"id":"https://example.com/d4/lib.json","$schema":"` + draft4 + `","definitions":{"p":{"type":"integer"}}}`,
		"https://example.com/d4/nos.json": `{"type":"number","maximum":5,"exclusiveMaximum":true}`,
	}}

	r := (&run{
		web:     web,
		rootURI: "https://example.com/d4/root.json",
		root: `{"id":"https://example.com/d4/root.json","$schema":"` + draft4 + `",
			"properties":{"p":{"$ref":"lib.json#/definitions/p"},"q":{"$ref":"nos.json"}}}`,
		instances: instances("ok.json", `{"p":3,"q":4.99}`, "edge.json", `{"p":3,"q":5}`, "frac.json", `{"p":1.5}`),
	}).do(t)

	p := r.wantBundled(t)

	if want := (Checked{Valid: 1, Invalid: 2, Agreed: 3}); p.Checked != want {
		t.Errorf("checked = %+v, want %+v", p.Checked, want)
	}

	if !bytes.Contains(p.Schema, []byte(`"id":"https://example.com/d4/nos.json"`)) {
		t.Errorf("dependency without id was not embedded under its URI: %s", p.Schema)
	}
}

func TestDraft2020DefsAndAnchors(t *testing.T) {
	web := &fakeWeb{docs: map[string]string{
		"https://example.com/2020/person.json": `{"$schema":"` + d202012 + `","$id":"https://example.com/2020/person.json","type":"object",
			"properties":{"name":{"type":"string"},"tags":{"$ref":"#/$defs/tags"}},
			"$defs":{"nick":{"$anchor":"nick","type":"string","maxLength":8},"tags":{"type":"array","items":{"type":"string"}}}}`,
	}}

	r := (&run{
		web:     web,
		rootURI: "https://example.com/2020/root.json",
		root: `{"$schema":"` + d202012 + `","$id":"https://example.com/2020/root.json","type":"object",
			"properties":{"person":{"$ref":"person.json"},"nick":{"$ref":"person.json#nick"},"flag":{"$ref":"#loc"}},
			"$defs":{"local":{"$anchor":"loc","type":"boolean"}},"unevaluatedProperties":false}`,
		instances: instances(
			"ok.json", `{"person":{"name":"a","tags":["x"]},"nick":"bob","flag":true}`,
			"long-nick.json", `{"nick":"waytoolongnick"}`,
			"extra.json", `{"extra":1}`,
			"bad-tags.json", `{"person":{"tags":[1]}}`,
		),
	}).do(t)

	p := r.wantBundled(t)

	if want := (Checked{Valid: 1, Invalid: 3, Agreed: 4}); p.Checked != want {
		t.Errorf("checked = %+v, want %+v", p.Checked, want)
	}

	if !bytes.Contains(p.Schema, []byte(`"$defs":{"local":`)) || !bytes.Contains(p.Schema, []byte(`"https://example.com/2020/person.json":{`)) {
		t.Errorf("unexpected bundle layout: %s", p.Schema)
	}
}

func TestSchemaWithoutExternalRefsIsKeptByteForByte(t *testing.T) {
	cases := map[string]string{
		"draft-07 with id": `{
  "$schema": "` + draft7 + `",
  "$id": "https://example.com/self.json",
  "title": "café \/ \"quoted\"",
  "properties": {"n": {"maximum": 12345678901234567890123, "minimum": -0, "multipleOf": 1.50}, "s": {"$ref": "#/definitions/s"}},
  "definitions": {"s": {"type": "string"}}
}`,
		"no $schema, fragment refs only": `{"properties":{"a":{"$ref":"#/definitions/a"}},"definitions":{"a":{"type":"integer"}}}`,
		"boolean":                        " true ",
		"draft-07 top-level internal ref": `{"$schema":"` + draft7 + `","$ref":"#/definitions/root",
			"definitions":{"root":{"type":"object","properties":{"x":{"$ref":"#/definitions/x"}}},"x":{"type":"string"}}}`,
		"no id, fragment refs only": `{"$schema":"` + d202012 + `","$defs":{"a":{"type":"string"}},"items":{"$ref":"#/$defs/a"}}`,
		"draft-03 without refs":     `{"$schema":"http://json-schema.org/draft-03/schema#","type":"string"}`,
		"pointer used as anchor": `{"$schema":"` + draft7 + `","$id":"https://example.com/self.json",
			"definitions":{"a":{"$id":"#/definitions/a","type":"string"}},"properties":{"p":{"$ref":"#/definitions/a"}}}`,
		"draft-07 root id is a plain-name anchor": `{"$schema":"` + draft7 + `","$id":"#top","type":"object",
			"properties":{"a":{"$ref":"#/definitions/a"},"b":{"$ref":"#top"}},"definitions":{"a":{"type":"string"}}}`,
		"draft-04 root id is a plain-name anchor": `{"$schema":"` + draft4 + `","id":"#top","properties":{"a":{"$ref":"#/definitions/a"}},
			"definitions":{"a":{"type":"string"}}}`,
		"root id with a pointer fragment": `{"$schema":"` + draft7 + `","$id":"https://example.com/p.json#/x",
			"properties":{"a":{"$ref":"#/definitions/a"}},"definitions":{"a":{"type":"string"}}}`,
		"root id that does not parse": `{"$schema":"` + draft7 + `","$id":"https://example.com/%zz.json","items":{"$ref":"#/definitions/a"},
			"definitions":{"a":{"type":"string"}}}`,
		"references to an official metaschema": `{"$schema":"` + draft7 + `","$id":"https://example.com/self.json",
			"properties":{"s":{"$ref":"` + draft7 + `"},"n":{"$ref":"http://json-schema.org/draft-07/schema#/definitions/nonNegativeInteger"}}}`,
		"metaschema reference without id":          `{"$schema":"` + d202012 + `","properties":{"s":{"$ref":"https://json-schema.org/draft/2020-12/schema"}}}`,
		"metaschema reference without $schema":     `{"properties":{"s":{"$ref":"http://json-schema.org/draft-07/schema#"}}}`,
		"draft-07 top-level ref to the metaschema": `{"$schema":"` + draft7 + `","$ref":"http://json-schema.org/draft-07/schema#"}`,
	}

	for name, root := range cases {
		t.Run(name, func(t *testing.T) {
			r := (&run{rootURI: "https://example.com/self.json", root: root, instances: instances("x.json", `1`)}).do(t)
			if r.err != nil {
				t.Fatalf("err = %v", r.err)
			}

			want, err := jsonutil.Compact([]byte(root), 0)
			if err != nil {
				t.Fatal(err)
			}

			if r.prepared.Bundled || !bytes.Equal(r.prepared.Schema, want) {
				t.Errorf("prepared = %s (bundled %v), want %s", r.prepared.Schema, r.prepared.Bundled, want)
			}

			if len(r.web.calls) != 0 || len(r.prepared.Dependencies) != 0 || r.prepared.Checked != (Checked{}) {
				t.Errorf("unexpected work: calls %v, prepared %+v", r.web.calls, r.prepared)
			}
		})
	}
}

func TestMissingDependency(t *testing.T) {
	r := (&run{
		rootURI: "https://example.com/m/root.json",
		root:    `{"$schema":"` + draft7 + `","$id":"https://example.com/m/root.json","properties":{"a":{"$ref":"gone.json"}}}`,
	}).do(t)

	r.wantReason(t, ReasonUnresolvedRef)

	if !errors.Is(r.err, errNotFound) {
		t.Errorf("the fetch error is not reachable: %v", r.err)
	}
}

func TestMissingFragmentInDependency(t *testing.T) {
	web := &fakeWeb{docs: map[string]string{
		"https://example.com/f/dep.json": `{"$schema":"` + draft7 + `","definitions":{}}`,
	}}

	r := (&run{
		web:     web,
		rootURI: "https://example.com/f/root.json",
		root:    `{"$schema":"` + draft7 + `","$id":"https://example.com/f/root.json","properties":{"a":{"$ref":"dep.json#/definitions/missing"}}}`,
	}).do(t)

	r.wantReason(t, ReasonUnresolvedRef)
}

func TestDependencyWithMismatchedID(t *testing.T) {
	web := &fakeWeb{docs: map[string]string{
		"https://example.com/x/common.json": `{"$schema":"` + draft7 + `","$id":"https://example.com/x/common-v2.json","type":"string"}`,
	}}

	r := (&run{
		web:     web,
		rootURI: "https://example.com/x/root.json",
		root:    `{"$schema":"` + draft7 + `","$id":"https://example.com/x/root.json","properties":{"a":{"$ref":"common.json"}}}`,
	}).do(t)

	if detail := r.wantReason(t, ReasonIDMismatch); !strings.Contains(detail, "common-v2.json") {
		t.Errorf("detail = %q", detail)
	}
}

func TestDependencyWithRelativeIDIsAccepted(t *testing.T) {
	web := &fakeWeb{docs: map[string]string{
		"https://example.com/r/common.json": `{"$schema":"` + draft7 + `","$id":"common.json","type":"string"}`,
	}}

	r := (&run{
		web:       web,
		rootURI:   "https://example.com/r/root.json",
		root:      `{"$schema":"` + draft7 + `","$id":"https://example.com/r/root.json","properties":{"a":{"$ref":"common.json"}}}`,
		instances: instances("ok.json", `{"a":"x"}`, "bad.json", `{"a":1}`),
	}).do(t)

	p := r.wantBundled(t)
	if p.Checked.Agreed != 2 {
		t.Errorf("checked = %+v", p.Checked)
	}
}

func TestDraft7TopLevelRefShim(t *testing.T) {
	web := &fakeWeb{docs: map[string]string{
		"https://upstream.example.org/schema.json": `{"$schema":"` + draft7 + `","type":"object","properties":{"port":{"type":"integer"}},"required":["port"]}`,
	}}

	r := (&run{
		web:     web,
		rootURI: "https://example.com/shim.json",
		root: `{"$schema":"` + draft7 + `","title":"Shim","$comment":"moved upstream","x-editor":{"hint":1},
			"$ref":"https://upstream.example.org/schema.json"}`,
		instances: instances("ok.json", `{"port":1}`, "missing.json", `{}`, "wrong.json", `{"port":"x"}`),
	}).do(t)

	p := r.wantBundled(t)

	if want := (Checked{Valid: 1, Invalid: 2, Agreed: 3}); p.Checked != want {
		t.Errorf("checked = %+v, want %+v", p.Checked, want)
	}

	for _, want := range []string{
		`"$id":"https://example.com/shim.json"`, `"allOf":[{"$ref":"https://upstream.example.org/schema.json"}]`,
		`"title":"Shim"`, `"$comment":"moved upstream"`, `"x-editor":{"hint":1}`,
	} {
		if !bytes.Contains(p.Schema, []byte(want)) {
			t.Errorf("bundle lacks %s", want)
		}
	}
}

func TestDraft7TopLevelRefNextToIgnoredKeywordsIsRejected(t *testing.T) {
	for _, sibling := range []string{`"type":"object"`, `"properties":{"a":{"type":"string"}}`, `"allOf":[{"required":["a"]}]`, `"format":"uri"`} {
		t.Run(sibling, func(t *testing.T) {
			r := (&run{
				tool:    unrunnableTool(t),
				rootURI: "https://example.com/shim.json",
				root:    `{"$schema":"` + draft7 + `","$ref":"https://upstream.example.org/schema.json",` + sibling + `}`,
			}).do(t)

			detail := r.wantReason(t, ReasonTopLevelRefDraft7)
			if key := sibling[1 : strings.Index(sibling[1:], `"`)+1]; !strings.Contains(detail, key) {
				t.Errorf("detail %q does not name %s", detail, key)
			}

			if len(r.web.calls) != 0 {
				t.Errorf("fetched %v for a rejected shim", r.web.calls)
			}
		})
	}
}

func TestDependencyTopLevelRefInDraft7(t *testing.T) {
	web := &fakeWeb{docs: map[string]string{
		"https://example.com/t/dep.json": `{"$schema":"` + draft7 + `","$ref":"#/definitions/x","description":"a dep","definitions":{"x":{"type":"string","maxLength":3}}}`,
		"https://example.com/t/bad.json": `{"$schema":"` + draft7 + `","$ref":"#/definitions/x","type":"integer","definitions":{"x":{"type":"string"}}}`,
	}}

	r := (&run{
		web:       web,
		rootURI:   "https://example.com/t/root.json",
		root:      `{"$schema":"` + draft7 + `","$id":"https://example.com/t/root.json","properties":{"a":{"$ref":"dep.json"},"b":{"$ref":"dep.json#/definitions/x"}}}`,
		instances: instances("ok.json", `{"a":"abc","b":"x"}`, "long.json", `{"a":"abcd"}`, "number.json", `{"b":1}`),
	}).do(t)

	p := r.wantBundled(t)

	if want := (Checked{Valid: 1, Invalid: 2, Agreed: 3}); p.Checked != want {
		t.Errorf("checked = %+v, want %+v", p.Checked, want)
	}

	if !bytes.Contains(p.Schema, []byte(`"$id":"https://example.com/t/dep.json"`)) || !bytes.Contains(p.Schema, []byte(`"allOf":[{"$ref":"#/definitions/x"}]`)) {
		t.Errorf("dependency was not embedded in its rewritten form: %s", p.Schema)
	}

	r = (&run{
		web:     web,
		rootURI: "https://example.com/t/root.json",
		root:    `{"$schema":"` + draft7 + `","$id":"https://example.com/t/root.json","properties":{"a":{"$ref":"bad.json"}}}`,
	}).do(t)

	if detail := r.wantReason(t, ReasonTopLevelRefDraft7); !strings.Contains(detail, "https://example.com/t/bad.json") || !strings.Contains(detail, "type") {
		t.Errorf("detail = %q", detail)
	}
}

func TestRootWithoutSchemaButWithRefs(t *testing.T) {
	r := (&run{
		rootURI: "https://example.com/u/root.json",
		root:    `{"type":"object","properties":{"a":{"$ref":"common.json#/definitions/a"}}}`,
	}).do(t)

	r.wantReason(t, ReasonUndeclaredDialect)
}

func TestDependencyCycle(t *testing.T) {
	web := &fakeWeb{docs: map[string]string{
		"https://example.com/cycle/b.json": `{"$schema":"` + draft7 + `","$id":"https://example.com/cycle/b.json",
			"type":"object","properties":{"back":{"$ref":"a.json"},"leaf":{"$ref":"a.json#/definitions/leaf"}}}`,
	}}

	r := (&run{
		web:     web,
		rootURI: "https://example.com/cycle/a.json",
		root: `{"$schema":"` + draft7 + `","$id":"https://example.com/cycle/a.json","type":"object",
			"properties":{"b":{"$ref":"b.json"}},"definitions":{"leaf":{"type":"integer"}}}`,
		instances: instances("deep.json", `{"b":{"back":{"b":{"leaf":1}}}}`, "bad.json", `{"b":{"back":{"b":{"leaf":"x"}}}}`),
	}).do(t)

	p := r.wantBundled(t)

	if !reflect.DeepEqual(web.calls, []string{"https://example.com/cycle/b.json"}) {
		t.Errorf("fetch calls = %v", web.calls)
	}

	if want := (Checked{Valid: 1, Invalid: 1, Agreed: 2}); p.Checked != want {
		t.Errorf("checked = %+v", p.Checked)
	}
}

func TestLimits(t *testing.T) {
	web := &fakeWeb{docs: map[string]string{
		"https://example.com/l/c1.json":  `{"$schema":"` + draft7 + `","properties":{"n":{"$ref":"c2.json"}}}`,
		"https://example.com/l/c2.json":  `{"$schema":"` + draft7 + `","type":"integer"}`,
		"https://example.com/l/big.json": `{"$schema":"` + draft7 + `","description":"` + strings.Repeat("x", 512) + `"}`,
	}}
	chain := `{"$schema":"` + draft7 + `","$id":"https://example.com/l/root.json","properties":{"a":{"$ref":"c1.json"}}}`
	two := `{"$schema":"` + draft7 + `","$id":"https://example.com/l/root.json","properties":{"a":{"$ref":"c2.json"},"b":{"$ref":"big.json"}}}`

	cases := map[string]struct {
		root   string
		limits Limits
	}{
		"depth":         {root: chain, limits: Limits{MaxDepth: 1}},
		"documents":     {root: two, limits: Limits{MaxDocuments: 1}},
		"document size": {root: two, limits: Limits{MaxDocumentBytes: 256}},
		"total size":    {root: two, limits: Limits{MaxTotalBytes: 600}},
		"root size":     {root: chain, limits: Limits{MaxDocumentBytes: 32}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := (&run{web: &fakeWeb{docs: web.docs}, rootURI: "https://example.com/l/root.json", root: tc.root, limits: tc.limits}).do(t)
			r.wantReason(t, ReasonDependencyLimit)
		})
	}

	for _, root := range []string{chain, two} {
		ok := (&run{web: &fakeWeb{docs: web.docs}, rootURI: "https://example.com/l/root.json", root: root, limits: Limits{MaxDepth: 2, MaxDocuments: 2}}).do(t)
		if p := ok.wantBundled(t); len(p.Dependencies) != 2 {
			t.Errorf("exactly MaxDocuments dependencies: %+v", p.Dependencies)
		}
	}
}

func TestLocalFileReferencesAreRejected(t *testing.T) {
	cases := map[string]string{
		"absolute file ref": `{"$schema":"` + draft7 + `","$id":"https://example.com/lf/root.json","properties":{"a":{"$ref":"file:///etc/passwd"}}}`,
		"relative to a file id": `{"$schema":"` + draft7 + `","$id":"https://example.com/lf/root.json",
			"properties":{"a":{"$ref":"file:///tmp/x/y.json#/definitions/z"}},"definitions":{"d":{"$id":"file:///tmp/","properties":{"b":{"$ref":"passwd"}}}}}`,
	}

	for name, root := range cases {
		t.Run(name, func(t *testing.T) {
			r := (&run{rootURI: "https://example.com/lf/root.json", root: root}).do(t)
			r.wantReason(t, ReasonUnresolvedRef)

			if len(r.web.calls) != 0 {
				t.Errorf("fetched %v", r.web.calls)
			}
		})
	}
}

func TestRootWithoutIDUsesRetrievalURI(t *testing.T) {
	web := &fakeWeb{docs: map[string]string{
		"https://example.com/rel/dep.json": `{"$schema":"` + draft7 + `","type":"string"}`,
	}}

	r := (&run{
		web:       web,
		rootURI:   "https://example.com/rel/root.json",
		root:      `{"$schema":"` + draft7 + `","properties":{"a":{"$ref":"dep.json"}}}`,
		instances: instances("ok.json", `{"a":"s"}`, "bad.json", `{"a":1}`),
	}).do(t)

	p := r.wantBundled(t)

	if !reflect.DeepEqual(web.calls, []string{"https://example.com/rel/dep.json"}) {
		t.Errorf("fetch calls = %v", web.calls)
	}

	if !bytes.Contains(p.Schema, []byte(`"$id":"https://example.com/rel/root.json"`)) {
		t.Errorf("root identifier is not the retrieval URI: %s", p.Schema)
	}
}

func TestRootWithoutIDAndSelfReferenceIsBundled(t *testing.T) {
	r := (&run{
		rootURI:   "https://example.com/self/root.json",
		root:      `{"$schema":"` + draft7 + `","definitions":{"a":{"type":"string"}},"properties":{"x":{"$ref":"root.json#/definitions/a"}}}`,
		instances: instances("ok.json", `{"x":"s"}`, "bad.json", `{"x":1}`),
	}).do(t)

	p := r.wantBundled(t)

	if len(p.Dependencies) != 0 || len(r.web.calls) != 0 || !bytes.Contains(p.Schema, []byte(`"$id":"https://example.com/self/root.json"`)) {
		t.Errorf("prepared = %+v, calls %v", p, r.web.calls)
	}
}

func TestOfficialMetaschemasAreNeitherFetchedNorEmbedded(t *testing.T) {
	web := &fakeWeb{docs: map[string]string{
		"https://example.com/mm/dep.json": `{"$schema":"` + d202012 + `","$id":"https://example.com/mm/dep.json","type":"object",
			"properties":{"s":{"$ref":"https://json-schema.org/draft/2020-12/schema"}}}`,
	}}

	r := (&run{
		web:     web,
		rootURI: "https://example.com/mm/root.json",
		root: `{"$schema":"` + draft7 + `","$id":"https://example.com/mm/root.json",
			"properties":{"n":{"$ref":"http://json-schema.org/draft-07/schema#/definitions/nonNegativeInteger"},"d":{"$ref":"dep.json"}},
			"definitions":{"own":{"type":"null"}}}`,
		instances: instances(
			"ok.json", `{"n":1,"d":{"s":{"type":"string"}}}`,
			"negative.json", `{"n":-1}`,
			"bad-type.json", `{"d":{"s":{"type":12}}}`,
			"bad-min.json", `{"d":{"s":{"minLength":-1}}}`,
		),
	}).do(t)

	p := r.wantBundled(t)

	if !reflect.DeepEqual(web.calls, []string{"https://example.com/mm/dep.json"}) || len(p.Dependencies) != 1 || p.Dependencies[0].URI != "https://example.com/mm/dep.json" {
		t.Errorf("calls %v, dependencies %+v", web.calls, p.Dependencies)
	}

	for _, embedded := range []string{`"http://json-schema.org/draft-07/schema":{`, `"https://json-schema.org/draft/2020-12/schema":{`, `"https://json-schema.org/draft/2020-12/meta/core":{`} {
		if bytes.Contains(p.Schema, []byte(embedded)) {
			t.Errorf("bundle embeds %s", embedded)
		}
	}

	for _, kept := range []string{`"$ref":"http://json-schema.org/draft-07/schema#/definitions/nonNegativeInteger"`, `"own":{"type":"null"}`, `"$id":"https://example.com/mm/dep.json"`} {
		if !bytes.Contains(p.Schema, []byte(kept)) {
			t.Errorf("bundle lacks %s", kept)
		}
	}

	if want := (Checked{Valid: 1, Invalid: 3, Agreed: 4}); p.Checked != want {
		t.Errorf("checked = %+v", p.Checked)
	}
}

func TestRejectedDocuments(t *testing.T) {
	root := func(ref string) string {
		return `{"$schema":"` + draft7 + `","$id":"https://example.com/rj/root.json","properties":{"a":{"$ref":"` + ref + `"}}}`
	}

	cases := map[string]struct {
		docs   map[string]string
		root   string
		reason string
	}{
		"custom metaschema root": {
			root:   `{"$schema":"https://example.com/meta.json","$id":"https://example.com/rj/root.json","type":"string"}`,
			reason: ReasonUnsupportedDialect,
		},
		"draft-03 root with refs": {
			root:   `{"$schema":"http://json-schema.org/draft-03/schema#","properties":{"a":{"$ref":"x.json"}}}`,
			reason: ReasonUnsupportedDialect,
		},
		"dependency with custom metaschema": {
			docs:   map[string]string{"https://example.com/rj/dep.json": `{"$schema":"https://example.com/meta.json","type":"string"}`},
			root:   root("dep.json"),
			reason: ReasonUnsupportedDialect,
		},
		"boolean dependency": {
			docs:   map[string]string{"https://example.com/rj/dep.json": `true`},
			root:   root("dep.json"),
			reason: ReasonUnresolvedRef,
		},
		"dependency with duplicate keys": {
			docs:   map[string]string{"https://example.com/rj/dep.json": `{"type":"string","type":"integer"}`},
			root:   root("dep.json"),
			reason: ReasonInvalidJSON,
		},
		"root is an array":   {root: `[1]`, reason: ReasonInvalidSchema},
		"root is not JSON":   {root: `{"a":`, reason: ReasonInvalidJSON},
		"non-string $schema": {root: `{"$schema":7}`, reason: ReasonInvalidSchema},
		"pointer used as anchor with external ref": {
			root: `{"$schema":"` + draft7 + `","$id":"https://example.com/rj/root.json",
				"definitions":{"a":{"$id":"#/definitions/a","type":"string"}},"properties":{"p":{"$ref":"other.json"}}}`,
			reason: ReasonBundlerError,
		},
		"root id with anchor": {root: `{"$schema":"` + draft7 + `","$id":"#top","properties":{"a":{"$ref":"x.json"}}}`, reason: ReasonIDMismatch},
		"unparsable root id with external ref": {
			root:   `{"$schema":"` + draft7 + `","$id":"https://example.com/%zz.json","properties":{"a":{"$ref":"x.json"}}}`,
			reason: ReasonIDMismatch,
		},
		"schema invalid for verifier": {
			docs:   map[string]string{"https://example.com/rj/dep.json": `{"$schema":"` + draft7 + `","type":"not-a-type"}`},
			root:   root("dep.json"),
			reason: ReasonInvalidSchema,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := (&run{web: &fakeWeb{docs: tc.docs}, rootURI: "https://example.com/rj/root.json", root: tc.root}).do(t)
			r.wantReason(t, tc.reason)
		})
	}
}

func TestFragmentRootURIs(t *testing.T) {
	doc := `{"$schema":"` + draft7 + `","definitions":{"x":{"type":"string"}}}`

	for uri, reason := range map[string]string{
		"https://example.com/a.json#definitions/x":  ReasonFragmentRoot,
		"https://example.com/a.json#/definitions/y": ReasonUnresolvedRef,
	} {
		r := (&run{rootURI: uri, root: doc}).do(t)
		if detail := r.wantReason(t, reason); strings.Contains(detail, "urn:") {
			t.Errorf("%s: detail names the temporary root: %q", uri, detail)
		}
	}

	for root, reason := range map[string]string{
		`true`: ReasonFragmentRoot,
		`{"definitions":{"x":{"type":"string"}}}`:                                      ReasonUndeclaredDialect,
		`{"$schema":"http://json-schema.org/draft-03/schema#","definitions":{"x":{}}}`: ReasonUnsupportedDialect,
	} {
		r := (&run{tool: unrunnableTool(t), rootURI: "https://example.com/a.json#/definitions/x", root: root}).do(t)
		r.wantReason(t, reason)
	}

	r := (&run{rootURI: "https://example.com/a.json#", root: `{"$schema":"` + draft7 + `"}`}).do(t)
	if r.err != nil || r.prepared.Bundled {
		t.Errorf("empty fragment: %v, prepared %+v", r.err, r.prepared)
	}

	for _, uri := range []string{"relative.json", "file:///etc/schema.json", "#/definitions/x"} {
		r := (&run{rootURI: uri, root: `{}`}).do(t)
		if fault.KindOf(r.err) != fault.Usage {
			t.Errorf("root URI %s: %v, want a usage fault", uri, r.err)
		}
	}
}

func TestDigestMismatchAndCancellation(t *testing.T) {
	isolateTemp(t)

	tool := pinnedTool(t)
	root := Document{URI: "https://example.com/d.json", Bytes: []byte(`{}`), Digest: "sha256:" + strings.Repeat("0", 64)}

	_, err := Resolve(t.Context(), tool, root, (&fakeWeb{}).fetch, Limits{})
	if fault.KindOf(err) != fault.Integrity {
		t.Errorf("digest mismatch: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	root = Document{URI: "https://example.com/c.json", Bytes: []byte(`{"$schema":"` + draft7 + `","$id":"https://example.com/c.json","items":{"$ref":"x.json"}}`)}

	_, err = Resolve(ctx, tool, root, (&fakeWeb{}).fetch, Limits{})
	if fault.KindOf(err) != fault.Canceled {
		t.Errorf("canceled context: %v", err)
	}

	if _, err := Prepare(t.Context(), tool, &Closure{}, nil); fault.KindOf(err) != fault.Internal {
		t.Errorf("zero closure: %v", err)
	}

	if _, err := Resolve(t.Context(), nil, root, nil, Limits{}); fault.KindOf(err) != fault.Internal {
		t.Errorf("nil tool: %v", err)
	}
}

func TestInspectExternalRefs(t *testing.T) {
	isolateTemp(t)

	dir := t.TempDir()
	file := filepath.Join(dir, "schema.json")

	schema := `{"$schema":"` + draft7 + `","$id":"https://example.com/e/root.json",
		"properties":{"a":{"$ref":"common.json#/definitions/a"},"b":{"$ref":"#/definitions/b"},"c":{"$ref":"` + draft7 + `"}},
		"definitions":{"b":{"type":"string"}}}`
	if err := writeFile(file, schema); err != nil {
		t.Fatal(err)
	}

	refs, err := inspectFile(t, pinnedTool(t), file)
	if err != nil {
		t.Fatalf("inspect = %v", err)
	}

	want := []Ref{
		{Origin: "/properties/a/$ref", Destination: "https://example.com/e/common.json#/definitions/a", Base: "https://example.com/e/common.json"},
		{Origin: "/properties/c/$ref", Destination: "http://json-schema.org/draft-07/schema", Base: "http://json-schema.org/draft-07/schema"},
	}

	if !reflect.DeepEqual(refs, want) {
		t.Errorf("refs = %+v\nwant %+v", refs, want)
	}

	if err := writeFile(file, `{"type":"string"}`); err != nil {
		t.Fatal(err)
	}

	_, err = inspectFile(t, pinnedTool(t), file)
	if reasonOf(err) != ReasonUndeclaredDialect {
		t.Errorf("schema without dialect: %v", err)
	}
}
