package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// unrunnableTool fails every CLI run with an internal fault, so a test that
// gets a Failure back proves the CLI was never started.
func unrunnableTool(t *testing.T) *Tool {
	t.Helper()

	return &Tool{Path: filepath.Join(t.TempDir(), "jsonschema-must-not-run"), Version: PinnedVersion}
}

func TestDefaultLimitsAreTheDocumentedOnes(t *testing.T) {
	want := Limits{MaxDepth: 8, MaxDocuments: 64, MaxDocumentBytes: 16 << 20, MaxTotalBytes: 256 << 20}
	if got := DefaultLimits(); got != want {
		t.Errorf("DefaultLimits() = %+v, want %+v", got, want)
	}
}

func TestLocalMetaschema(t *testing.T) {
	found := map[string]string{
		`{"$schema":"file:///dev/zero"}`:                                                 "file:///dev/zero",
		`{"definitions":{"x":{"$schema":"FILE:/etc/hostname"}}}`:                         "FILE:/etc/hostname",
		`{"a":[{"b":{"$schema":" \tfile://localhost/etc/hostname"}}]}`:                   " \tfile://localhost/etc/hostname",
		`{"$defs":{"x":{"$schema":"file:///dev/zero"}}}`:                                 "file:///dev/zero",
		`{"$schema":"http://json-schema.org/draft-07/schema#","x":{"$schema":"file:x"}}`: "file:x",
	}

	for input, want := range found {
		got, ok, err := localMetaschema([]byte(input))
		if err != nil || !ok || got != want {
			t.Errorf("localMetaschema(%s) = %q, %v, %v; want %q", input, got, ok, err, want)
		}
	}

	for _, input := range []string{
		`{"$schema":"http://json-schema.org/draft-07/schema#"}`, `{"$schema":"urn:file:x"}`, `{"$schema":"profile:x"}`,
		`{"properties":{"$schema":{"type":"string"}}}`, `{"examples":["$schema","file:///x"]}`, `{"$id":"file:///x"}`,
		`{"$schema":"fil:///x"}`, `true`,
	} {
		if got, ok, err := localMetaschema([]byte(input)); err != nil || ok {
			t.Errorf("localMetaschema(%s) = %q, %v, %v", input, got, ok, err)
		}
	}
}

func TestLocalMetaschemaNeverReachesTheCLI(t *testing.T) {
	roots := map[string]string{
		"nested in a known dialect": `{"$schema":"` + draft7 + `","$id":"https://example.com/lm/root.json",
			"definitions":{"x":{"$schema":"file:///dev/zero"}},"properties":{"a":{"$ref":"#/definitions/x"}}}`,
		"top level":         `{"$schema":"file:///dev/zero","type":"string"}`,
		"without a dialect": `{"definitions":{"x":{"$schema":"file:///etc/hostname"}}}`,
	}

	for name, root := range roots {
		t.Run(name, func(t *testing.T) {
			r := (&run{tool: unrunnableTool(t), rootURI: "https://example.com/lm/root.json", root: root}).do(t)
			r.wantReason(t, ReasonUnsupportedDialect)
		})
	}

	t.Run("dependency", func(t *testing.T) {
		tool := pinnedTool(t)
		tool.runTimeout = 20 * time.Second
		// The CLI inspects the root, an ordinary schema, before the dependency
		// is fetched. Below the production floor the kernel sometimes kills
		// that run on CI runners; the dependency itself must never reach the
		// CLI, and if it did, the rejection reason below would change.
		tool.addressSpace = addressSpaceFloor

		web := &fakeWeb{docs: map[string]string{
			"https://example.com/lm/dep.json": `{"$schema":"` + draft7 + `","definitions":{"x":{"$schema":"file:///dev/zero"}}}`,
		}}

		started := time.Now()
		r := (&run{
			tool:    tool,
			web:     web,
			rootURI: "https://example.com/lm/root.json",
			root:    `{"$schema":"` + draft7 + `","$id":"https://example.com/lm/root.json","properties":{"a":{"$ref":"dep.json"}}}`,
		}).do(t)

		if detail := r.wantReason(t, ReasonUnsupportedDialect); !strings.Contains(detail, "https://example.com/lm/dep.json") {
			t.Errorf("detail = %q", detail)
		}

		if elapsed := time.Since(started); elapsed > 10*time.Second {
			t.Errorf("rejection took %s", elapsed)
		}
	})
}

// TestRelativeMetaschemaIsNotReadFromDisk covers the documents the CLI sees
// without an absolute identifier, where a relative $schema could otherwise
// resolve against the workspace file.
func TestRelativeMetaschemaIsNotReadFromDisk(t *testing.T) {
	tool := pinnedTool(t)
	tool.runTimeout = 20 * time.Second
	tool.addressSpace = 2 << 30

	for _, root := range []string{
		`{"$schema":"/dev/zero","type":"string"}`,
		`{"$schema":"../../../../../../../../dev/zero","properties":{"a":{"$ref":"x.json"}}}`,
		`{"$schema":"` + draft7 + `","$id":"https://example.com/rm/root.json","definitions":{"x":{"$id":"file:///dev/","$schema":"zero"}},
			"properties":{"a":{"$ref":"x.json"}}}`,
	} {
		r := (&run{tool: tool, rootURI: "https://example.com/rm/root.json", root: root}).do(t)
		if detail := r.wantReason(t, ReasonUnsupportedDialect); strings.Contains(detail, "did not finish") {
			t.Errorf("detail = %q", detail)
		}
	}
}

// TestMain lets the test binary stand in for a JSON Schema CLI: under a
// rewriting name (see rewritingTool) it forwards to the pinned CLI and
// corrupts the bundle, otherwise it never finishes. Tool passes the
// subcommand as the first argument, which the go test driver never does.
func TestMain(m *testing.M) {
	if keyword, ok := rewritingKeyword(); ok {
		os.Exit(runRewritingCLI(keyword))
	}

	if len(os.Args) > 1 && (os.Args[1] == "inspect" || os.Args[1] == "bundle") {
		time.Sleep(time.Hour)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// hangingTool runs the test binary as a CLI that hangs. Its address space is
// left alone because the race detector reserves far more than any limit.
func hangingTool(t *testing.T, timeout time.Duration) *Tool {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	return &Tool{Path: self, Version: PinnedVersion, runTimeout: timeout, addressSpace: math.MaxUint64}
}

func TestCLIRunsAreBoundedInTime(t *testing.T) {
	const rootURI = "https://example.com/slow/root.json"

	root := `{"$schema":"` + draft7 + `","$id":"` + rootURI + `","properties":{"a":{"$ref":"dep.json"}}}`

	started := time.Now()
	r := (&run{tool: hangingTool(t, 200*time.Millisecond), rootURI: rootURI, root: root}).do(t)

	if detail := r.wantReason(t, ReasonBundlerError); !strings.Contains(detail, "inspect did not finish within 200ms") {
		t.Errorf("detail = %q", detail)
	}

	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("timed-out inspect took %s", elapsed)
	}

	tmp := isolateTemp(t)

	web := &fakeWeb{docs: map[string]string{"https://example.com/slow/dep.json": `{"$schema":"` + draft7 + `","type":"string"}`}}

	closure, err := Resolve(t.Context(), pinnedTool(t), Document{URI: rootURI, Bytes: []byte(root)}, web.fetch, Limits{})
	if err != nil {
		t.Fatalf("Resolve = %v", err)
	}

	started = time.Now()

	_, err = Prepare(t.Context(), hangingTool(t, 200*time.Millisecond), closure, nil)
	if failure, ok := errors.AsType[*Failure](err); !ok || failure.Reason != ReasonBundlerError || !strings.Contains(failure.Detail, "bundle did not finish within 200ms") {
		t.Errorf("Prepare with a hanging bundler = %v", err)
	}

	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("timed-out bundle took %s", elapsed)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	_, err = Resolve(ctx, hangingTool(t, time.Minute), Document{URI: rootURI, Bytes: []byte(root)}, web.fetch, Limits{})
	if _, isFailure := errors.AsType[*Failure](err); isFailure || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("caller deadline = %v, want the deadline as an operational error", err)
	}

	if entries, err := os.ReadDir(tmp); err != nil || len(entries) != 0 {
		t.Errorf("temporary files left behind by interrupted runs: %v, %v", entries, err)
	}
}

func TestCanonicalURI(t *testing.T) {
	cases := map[string]string{
		"https://Example.COM/c/x.json":              "https://example.com/c/x.json",
		"https://example.com:443/c/y.json":          "https://example.com/c/y.json",
		"https://example.com/%7Euser/w.json":        "https://example.com/~user/w.json",
		"HTTPS://example.com/a/./b/../z.json":       "https://example.com/a/z.json",
		"https://example.com":                       "https://example.com",
		"http://example.com:80/p%2Fq.json?x=%7e#/a": "http://example.com/p%2Fq.json?x=~",
		"https://example.com/%e2%82%ac.json":        "https://example.com/%E2%82%AC.json",
		"https://user@Example.com:8443/x.json":      "https://user@example.com:8443/x.json",
		"https://example.com/a%2fb/%41.json":        "https://example.com/a%2Fb/A.json",
		"https://example.com:/x.json":               "https://example.com/x.json",
		"https://EXAMPLE.com./x.json?":              "https://example.com./x.json?",
		"http://example.com:443/x.json":             "http://example.com:443/x.json",
		"https://example.com/x.json#":               "https://example.com/x.json",
		"urn:Example:Foo":                           "urn:Example:Foo",
		"https://ex%41mple.com/x.json":              "https://example.com/x.json",
		"https://example.com/a/b/../../../c.json":   "https://example.com/c.json",
		"https://example.com/a?b=c/../d":            "https://example.com/a?b=c/../d",
		"https://[::1]:443/x.json":                  "https://[::1]/x.json",
		"https://example.com/a/..":                  "https://example.com/",
		"https://example.com/a/.":                   "https://example.com/a/",
		"relative.json":                             "relative.json",
		"%zz":                                       "%zz",
		"a:%%%3370":                                 "a:%%%3370",
		"https://example.com/a%4":                   "https://example.com/a%4",
		"a:////x":                                   "a:/.//x",
	}

	for in, want := range cases {
		if got := canonicalURI(in); got != want {
			t.Errorf("canonicalURI(%s) = %s, want %s", in, got, want)
		}
	}
}

// TestCanonicalURIAgreesWithTheCLI pins canonicalURI to the reference bases
// the pinned CLI reports, the form in which Resolve fetches dependencies.
func TestCanonicalURIAgreesWithTheCLI(t *testing.T) {
	isolateTemp(t)

	refs := []string{
		"https://Example.COM/c/x.json", "https://example.com:443/c/y.json", "https://example.com/%7Euser/w.json",
		"HTTPS://example.com/a/./b/../z.json", "http://example.com:80/p%2Fq.json?x=%7e", "https://example.com/%e2%82%ac.json",
		"https://user@Example.com:8443/x.json", "https://example.com/a%2fb/%41.json", "https://example.com:/x.json",
		"http://example.com:443/x.json", "https://ex%41mple.com/x.json", "https://example.com/a/b/../../../c.json",
	}

	props := map[string]any{}
	for i, ref := range refs {
		props[fmt.Sprintf("p%02d", i)] = map[string]any{"$ref": ref}
	}

	data, err := json.Marshal(map[string]any{"$schema": draft7, "$id": "https://example.com/canon.json", "properties": props})
	if err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(t.TempDir(), "canon.json")
	if err := writeFile(file, string(data)); err != nil {
		t.Fatal(err)
	}

	got, err := inspectFile(t, pinnedTool(t), file)
	if err != nil {
		t.Fatalf("inspect = %v", err)
	}

	if len(got) != len(refs) {
		t.Fatalf("inspect = %+v", got)
	}

	for _, ref := range got {
		var i int
		if _, err := fmt.Sscanf(ref.Origin, "/properties/p%02d/$ref", &i); err != nil {
			t.Fatalf("origin %s: %v", ref.Origin, err)
		}

		if want := canonicalURI(refs[i]); ref.Base != want {
			t.Errorf("%s: CLI base %s, canonicalURI %s", refs[i], ref.Base, want)
		}
	}
}

func TestNonCanonicalSpellings(t *testing.T) {
	t.Run("dependency identifier equal after normalization", func(t *testing.T) {
		web := &fakeWeb{docs: map[string]string{
			"https://example.com/nc/x.json": `{"$schema":"` + draft7 + `","$id":"HTTPS://EXAMPLE.com:443/nc/./x.json","type":"string"}`,
		}}

		r := (&run{
			web:       web,
			rootURI:   "https://example.com/nc/root.json",
			root:      `{"$schema":"` + draft7 + `","$id":"https://example.com/nc/root.json","properties":{"a":{"$ref":"x.json"}}}`,
			instances: instances("ok.json", `{"a":"s"}`, "bad.json", `{"a":1}`),
		}).do(t)

		if p := r.wantBundled(t); p.Checked.Agreed != 2 || len(p.Dependencies) != 1 {
			t.Errorf("prepared = %+v", p)
		}
	})

	cases := map[string]struct{ ref, id, fetched string }{
		"host case":      {ref: "https://Example.COM/nc/x.json", id: `,"$id":"https://Example.COM/nc/x.json"`, fetched: "https://example.com/nc/x.json"},
		"default port":   {ref: "https://example.com:443/nc/y.json", fetched: "https://example.com/nc/y.json"},
		"escaped tilde":  {ref: "https://example.com/%7Euser/w.json", fetched: "https://example.com/~user/w.json"},
		"id with a port": {ref: "https://example.com:443/nc/y.json", id: `,"$id":"https://example.com:443/nc/y.json"`, fetched: "https://example.com/nc/y.json"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			web := &fakeWeb{docs: map[string]string{tc.fetched: `{"$schema":"` + draft7 + `"` + tc.id + `,"type":"string"}`}}

			r := (&run{
				web:       web,
				rootURI:   "https://example.com/nc/root.json",
				root:      `{"$schema":"` + draft7 + `","$id":"https://example.com/nc/root.json","properties":{"a":{"$ref":"` + tc.ref + `"}}}`,
				instances: instances("ok.json", `{"a":"s"}`, "bad.json", `{"a":1}`),
			}).do(t)

			if !reflect.DeepEqual(web.calls, []string{tc.fetched}) {
				t.Errorf("fetch calls = %v", web.calls)
			}

			detail := r.wantReason(t, ReasonNotSelfContained)
			if !strings.Contains(detail, "references "+tc.ref+" but embeds that document as "+tc.fetched) {
				t.Errorf("detail = %q", detail)
			}
		})
	}
}

func TestStripEmbeddedMetaschemas(t *testing.T) {
	const (
		meta7    = `"http://json-schema.org/draft-07/schema":{"$id":"http://json-schema.org/draft-07/schema","type":["object","boolean"]}`
		metaCore = `"https://json-schema.org/draft/2020-12/meta/core":{"$id":"https://json-schema.org/draft/2020-12/meta/core"}`
		dep      = `"https://example.com/dep.json":{"$id":"https://example.com/dep.json","type":"string"}`
	)

	cases := []struct {
		name, bundled, root, want string
	}{
		{
			name:    "metaschema next to a dependency",
			bundled: `{"$id":"https://example.com/r.json","definitions":{` + meta7 + `,` + dep + `}}`,
			root:    `{"$id":"https://example.com/r.json"}`,
			want:    `{"$id":"https://example.com/r.json","definitions":{` + dep + `}}`,
		},
		{
			name:    "container the bundler added",
			bundled: `{"$id":"https://example.com/r.json","definitions":{` + meta7 + `},"type":"object"}`,
			root:    `{"$id":"https://example.com/r.json","type":"object"}`,
			want:    `{"$id":"https://example.com/r.json","type":"object"}`,
		},
		{
			name:    "container the root declared empty",
			bundled: `{"definitions":{` + meta7 + `}}`,
			root:    `{"definitions":{}}`,
			want:    `{"definitions":{}}`,
		},
		{
			name:    "vendored metaschema of the root",
			bundled: `{"definitions":{` + meta7 + `,"own":{}}}`,
			root:    `{"definitions":{` + meta7 + `,"own":{}}}`,
			want:    `{"definitions":{` + meta7 + `,"own":{}}}`,
		},
		{
			name:    "2020-12 vocabulary metaschemas",
			bundled: `{"$defs":{"a":{},` + metaCore + `,` + dep + `},"$id":"x"}`,
			root:    `{"$defs":{"a":{}}}`,
			want:    `{"$defs":{"a":{},` + dep + `},"$id":"x"}`,
		},
		{
			name:    "lookalike identifier",
			bundled: `{"definitions":{"https://example.com/draft-07/schema":{}}}`,
			root:    `{}`,
			want:    `{"definitions":{"https://example.com/draft-07/schema":{}}}`,
		},
		{
			name:    "non-object container",
			bundled: `{"definitions":5}`,
			root:    `{}`,
			want:    `{"definitions":5}`,
		},
		{name: "boolean", bundled: `true`, root: `true`, want: `true`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := stripEmbeddedMetaschemas([]byte(tc.bundled), []byte(tc.root))
			if err != nil {
				t.Fatalf("stripEmbeddedMetaschemas = %v", err)
			}

			if string(got) != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}
