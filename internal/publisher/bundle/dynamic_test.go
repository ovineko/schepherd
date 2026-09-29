package bundle

import (
	"bytes"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const rewritingPrefix = "jsonschema-rewriting-"

// rewritingKeyword reports whether the test binary runs as a rewriting CLI
// stand-in and which keyword it turns into $ref.
func rewritingKeyword() (string, bool) {
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")

	return strings.CutPrefix(name, rewritingPrefix)
}

// runRewritingCLI runs the pinned CLI named in the file "real-cli" next to
// the stand-in and, for bundle, renames every $<keyword> member of its
// output to $ref: a bundler that silently drops dynamic scope.
func runRewritingCLI(keyword string) int {
	pinned, err := os.ReadFile(filepath.Join(filepath.Dir(os.Args[0]), "real-cli"))
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error())

		return 2
	}

	cmd := exec.Command(string(pinned), os.Args[1:]...)
	cmd.Stderr = os.Stderr

	out, err := cmd.Output()
	if len(os.Args) > 1 && os.Args[1] == "bundle" {
		out = bytes.ReplaceAll(out, []byte(`"$`+keyword+`"`), []byte(`"$ref"`))
	}

	_, _ = os.Stdout.Write(out)

	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode()
	}

	if err != nil {
		return 2
	}

	return 0
}

// rewritingTool returns a CLI that bundles with the pinned CLI and then
// renames every $<keyword> of the bundle to $ref.
func rewritingTool(t *testing.T, keyword string) *Tool {
	t.Helper()

	pinned := pinnedTool(t)

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()

	name := rewritingPrefix + keyword
	if runtime.GOOS == "windows" {
		name += ".exe"
	}

	path := filepath.Join(dir, name)
	if err := os.Link(self, path); err != nil {
		copyFile(t, self, path)
	}

	if err := os.WriteFile(filepath.Join(dir, "real-cli"), []byte(pinned.Path), 0o600); err != nil {
		t.Fatal(err)
	}

	return &Tool{Path: path, Version: PinnedVersion, addressSpace: math.MaxUint64}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()

	src, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}

	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
}

// dynamicCase is an extensible schema spread over two resources: the root
// extends a tree whose recursion goes through dynamic scope, so nested nodes
// must obey the root's unevaluatedProperties as well.
type dynamicCase struct {
	web       *fakeWeb
	rootURI   string
	root      string
	base      string
	baseURI   string
	keywords  []string
	instances []Instance
	checked   Checked
}

func dynamicCases() map[string]dynamicCase {
	nested := instances(
		"valid.json", `{"data":1,"children":[{"data":2,"children":[]}]}`,
		"nested-typo.json", `{"children":[{"daat":1}]}`,
		"top-typo.json", `{"daat":1}`,
	)

	tree := `{"$schema":"` + d202012 + `","$id":"https://example.com/dyn/tree.json","$dynamicAnchor":"node","type":"object",
		"properties":{"data":true,"children":{"type":"array","items":{"$dynamicRef":"#node"}}}}`
	rtree := `{"$schema":"` + d201909 + `","$id":"https://example.com/dyn/rtree.json","$recursiveAnchor":true,"type":"object",
		"properties":{"data":true,"children":{"type":"array","items":{"$recursiveRef":"#"}}}}`

	return map[string]dynamicCase{
		"2020-12 $dynamicRef": {
			web:     &fakeWeb{docs: map[string]string{"https://example.com/dyn/tree.json": tree}},
			rootURI: "https://example.com/dyn/strict-tree.json",
			root: `{"$schema":"` + d202012 + `","$id":"https://example.com/dyn/strict-tree.json","$dynamicAnchor":"node",
				"$ref":"tree.json","unevaluatedProperties":false}`,
			base: tree, baseURI: "https://example.com/dyn/tree.json",
			keywords:  []string{`"$dynamicRef":"#node"`, `"$dynamicAnchor":"node"`},
			instances: nested, checked: Checked{Valid: 1, Invalid: 2, Agreed: 3},
		},
		"2019-09 $recursiveRef": {
			web:     &fakeWeb{docs: map[string]string{"https://example.com/dyn/rtree.json": rtree}},
			rootURI: "https://example.com/dyn/strict-rtree.json",
			root: `{"$schema":"` + d201909 + `","$id":"https://example.com/dyn/strict-rtree.json","$recursiveAnchor":true,
				"$ref":"rtree.json","unevaluatedProperties":false}`,
			base: rtree, baseURI: "https://example.com/dyn/rtree.json",
			keywords:  []string{`"$recursiveRef":"#"`, `"$recursiveAnchor":true`},
			instances: nested, checked: Checked{Valid: 1, Invalid: 2, Agreed: 3},
		},
		"2020-12 $dynamicRef to an external anchor": {
			web:     &fakeWeb{docs: map[string]string{"https://example.com/dyn/tree.json": tree}},
			rootURI: "https://example.com/dyn/loose-tree.json",
			root: `{"$schema":"` + d202012 + `","$id":"https://example.com/dyn/loose-tree.json",
				"$dynamicRef":"tree.json#node","unevaluatedProperties":false}`,
			base: tree, baseURI: "https://example.com/dyn/tree.json",
			keywords:  []string{`"$dynamicRef":"tree.json#node"`, `"$dynamicAnchor":"node"`},
			instances: nested, checked: Checked{Valid: 2, Invalid: 1, Agreed: 3},
		},
	}
}

func TestDynamicScopeSurvivesBundling(t *testing.T) {
	for name, tc := range dynamicCases() {
		t.Run(name, func(t *testing.T) {
			base := mustCompileDialect(t, tc.baseURI, tc.base)
			if err := base.Validate(mustInstance(t, `{"children":[{"daat":1}]}`)); err != nil {
				t.Fatalf("the base schema alone rejects the nested typo, so the instances do not exercise dynamic scope: %v", err)
			}

			r := (&run{web: tc.web, rootURI: tc.rootURI, root: tc.root, instances: tc.instances}).do(t)
			p := r.wantBundled(t)

			if p.Checked != tc.checked {
				t.Errorf("checked = %+v, want %+v", p.Checked, tc.checked)
			}

			for _, keyword := range tc.keywords {
				if !bytes.Contains(p.Schema, []byte(keyword)) {
					t.Errorf("bundle lacks %s", keyword)
				}
			}
		})
	}
}

func TestMixedDialectRecursiveDependency(t *testing.T) {
	web := &fakeWeb{docs: map[string]string{
		"https://example.com/mix/rtree.json": `{"$schema":"` + d201909 + `","$recursiveAnchor":true,"type":"object",
			"properties":{"children":{"type":"array","items":{"$recursiveRef":"#"}},"leaf":{"type":"integer"}}}`,
	}}

	r := (&run{
		web:       web,
		rootURI:   "https://example.com/mix/root.json",
		root:      `{"$schema":"` + d202012 + `","properties":{"t":{"$ref":"rtree.json"}}}`,
		instances: instances("ok.json", `{"t":{"children":[{"leaf":1}]}}`, "bad.json", `{"t":{"children":[{"leaf":"x"}]}}`),
	}).do(t)

	p := r.wantBundled(t)

	if want := (Checked{Valid: 1, Invalid: 1, Agreed: 2}); p.Checked != want {
		t.Errorf("checked = %+v, want %+v", p.Checked, want)
	}

	if !bytes.Contains(p.Schema, []byte(`"$schema":"`+d201909+`","$id":"https://example.com/mix/rtree.json"`)) {
		t.Errorf("the 2019-09 dependency lost its dialect: %s", p.Schema)
	}
}

func TestDraft2019ExternalReferences(t *testing.T) {
	web := &fakeWeb{docs: map[string]string{
		"https://example.com/19/defs/common.json": `{"$schema":"` + d201909 + `","$id":"https://example.com/19/defs/common.json",
			"$defs":{"name":{"$anchor":"name","type":"string","minLength":1},"tags":{"type":"array","items":{"$ref":"tag.json"}}}}`,
		"https://example.com/19/defs/tag.json": `{"type":"string","pattern":"^[a-z]+$"}`,
		"https://other.example/19/port.json":   `{"$schema":"` + d201909 + `","type":"integer","minimum":1,"maximum":65535}`,
	}}

	r := (&run{
		web:     web,
		rootURI: "https://example.com/19/root.json",
		root: `{"$schema":"` + d201909 + `","type":"object",
			"properties":{"name":{"$ref":"defs/common.json#name"},"tags":{"$ref":"defs/common.json#/$defs/tags"},
			"port":{"$ref":"https://other.example/19/port.json"}},
			"dependentRequired":{"port":["name"]},"unevaluatedProperties":false}`,
		instances: instances(
			"ok.json", `{"name":"a","tags":["x"],"port":80}`,
			"empty-name.json", `{"name":""}`,
			"bad-tag.json", `{"tags":["X"]}`,
			"port-without-name.json", `{"port":80}`,
			"extra.json", `{"name":"a","other":1}`,
		),
	}).do(t)

	p := r.wantBundled(t)

	if want := (Checked{Valid: 1, Invalid: 4, Agreed: 5}); p.Checked != want {
		t.Errorf("checked = %+v, want %+v", p.Checked, want)
	}

	if len(p.Dependencies) != 3 || p.Dialect != d201909 {
		t.Errorf("dependencies = %+v, dialect %s", p.Dependencies, p.Dialect)
	}

	for _, want := range []string{
		`"$id":"https://example.com/19/root.json"`, `"$id":"https://example.com/19/defs/tag.json"`,
		`"$ref":"defs/common.json#name"`, `"$anchor":"name"`,
	} {
		if !bytes.Contains(p.Schema, []byte(want)) {
			t.Errorf("bundle lacks %s", want)
		}
	}
}

func TestMishandledDynamicScopeIsRejected(t *testing.T) {
	cases := dynamicCases()

	for name, keyword := range map[string]string{
		"2020-12 $dynamicRef":   "dynamicRef",
		"2019-09 $recursiveRef": "recursiveRef",
	} {
		tc := cases[name]

		t.Run(name, func(t *testing.T) {
			tool := rewritingTool(t, keyword)

			for _, withInstances := range []bool{false, true} {
				var inst []Instance
				if withInstances {
					inst = tc.instances
				}

				r := (&run{tool: tool, web: &fakeWeb{docs: tc.web.docs}, rootURI: tc.rootURI, root: tc.root, instances: inst}).do(t)
				if detail := r.wantReason(t, ReasonReferenceMismatch); !strings.Contains(detail, "$"+keyword) {
					t.Errorf("detail = %q", detail)
				}
			}
		})
	}

	t.Run("faithful stand-in", func(t *testing.T) {
		tc := cases["2020-12 $dynamicRef"]

		r := (&run{tool: rewritingTool(t, "unused"), web: tc.web, rootURI: tc.rootURI, root: tc.root, instances: tc.instances}).do(t)
		r.wantBundled(t)
	})
}

func mustCompileDialect(t *testing.T, loc, schema string) *jsonschema.Schema {
	t.Helper()

	tl, err := scanTopLevel([]byte(schema))
	if err != nil {
		t.Fatal(err)
	}

	dialectURI, _, err := tl.stringMember([]byte(schema), "$schema")
	if err != nil {
		t.Fatal(err)
	}

	sch, err := compile(loc, []byte(schema), dialectURI, offlineLoader{})
	if err != nil {
		t.Fatalf("compile %s: %v", loc, err)
	}

	return sch
}

func mustInstance(t *testing.T, data string) any {
	t.Helper()

	v, err := jsonschema.UnmarshalJSON(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	return v
}
