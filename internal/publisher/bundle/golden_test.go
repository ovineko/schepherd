package bundle

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/publisher/bundle/*.golden with the pinned bundler's output")

const regenerateGolden = "go test ./internal/publisher/bundle -run TestPinnedBundlerOutput -update"

var goldenDir = filepath.Join("..", "..", "..", "testdata", "publisher", "bundle")

// annotated is a root and its dependencies full of members a validator does
// not evaluate: annotations, $comment and unknown keywords, some nested in
// subschemas and carrying numbers, escapes and nulls.
type annotated struct {
	docs      map[string]string
	rootURI   string
	root      string
	container string
}

func annotatedCases() map[string]annotated {
	return map[string]annotated{
		"draft-07": {
			rootURI: "https://example.com/golden/7/root.json",
			root: `{"$schema":"` + draft7 + `","$id":"https://example.com/golden/7/root.json","title":"Root é \/ \"q\"",
				"description":"line\nbreak","$comment":"kept","x-unknown":{"nested":[1,2.50,-0,1e2,null,true]},"markdownDescription":"**b**",
				"default":{"name":"n"},"examples":[{"name":"e"}],"readOnly":false,"writeOnly":false,"deprecated":true,
				"type":"object","properties":{"name":{"$ref":"common.json#/definitions/name","description":"sibling ignored by draft-07"},
				"size":{"$ref":"https://other.example/golden/size.json"}},"definitions":{"own":{"$comment":"own def","default":12345678901234567890123}}}`,
			docs: map[string]string{
				"https://example.com/golden/7/common.json": `{"$schema":"` + draft7 + `","$id":"https://example.com/golden/7/common.json",
					"$comment":"dep comment","x-vendor":"v","definitions":{"name":{"type":"string","description":"a name","examples":["a","b"],
					"default":"a","format":"hostname","x-order":3}}}`,
				"https://other.example/golden/size.json": `{"$schema":"` + draft7 + `","$id":"https://other.example/golden/size.json",
					"type":"integer","minimum":0,"multipleOf":0.5,"title":"Size","x-unit":"bytes"}`,
			},
			container: "definitions",
		},
		"2020-12": {
			rootURI: "https://example.com/golden/2020/root.json",
			root: `{"$schema":"` + d202012 + `","$id":"https://example.com/golden/2020/root.json","$comment":"kept",
				"title":"Root","description":"d","default":{},"examples":[{}],"deprecated":false,"x-unknown":{"a":[{"b":null}]},
				"$anchor":"top","$dynamicAnchor":"meta","type":"object",
				"properties":{"tags":{"$ref":"lib/tags.json","description":"sibling kept in 2020-12","default":[]},
				"id":{"$ref":"https://other.example/golden/2020/id.json#uid"}},"unevaluatedProperties":false,
				"$defs":{"own":{"$comment":"own","const":1.0}}}`,
			docs: map[string]string{
				"https://example.com/golden/2020/lib/tags.json": `{"$schema":"` + d202012 + `","$id":"https://example.com/golden/2020/lib/tags.json",
					"type":"array","items":{"type":"string","examples":["x"]},"x-ui":{"widget":"chips"},"$comment":"tags"}`,
				"https://other.example/golden/2020/id.json": `{"$schema":"` + d202012 + `","$id":"https://other.example/golden/2020/id.json",
					"$defs":{"uid":{"$anchor":"uid","type":"string","format":"uuid","contentMediaType":"text/plain","readOnly":true,"writeOnly":false}}}`,
			},
			container: "$defs",
		},
	}
}

func decodeValue(t *testing.T, data []byte) any {
	t.Helper()

	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}

	return v
}

// TestBundlingKeepsAnnotationsAndUnknownKeywords checks the embedding as
// values: every source document appears in the bundle with every member,
// and the root is the source root plus the embedded documents.
func TestBundlingKeepsAnnotationsAndUnknownKeywords(t *testing.T) {
	for name, tc := range annotatedCases() {
		t.Run(name, func(t *testing.T) {
			r := (&run{web: &fakeWeb{docs: tc.docs}, rootURI: tc.rootURI, root: tc.root}).do(t)
			p := r.wantBundled(t)

			bundle, ok := decodeValue(t, p.Schema).(map[string]any)
			if !ok {
				t.Fatalf("bundle is not an object: %s", p.Schema)
			}

			container, ok := bundle[tc.container].(map[string]any)
			if !ok {
				t.Fatalf("bundle has no %s: %s", tc.container, p.Schema)
			}

			for uri, doc := range tc.docs {
				if got, want := container[uri], decodeValue(t, []byte(doc)); !reflect.DeepEqual(got, want) {
					t.Errorf("embedded %s differs from its source:\n got %v\nwant %v", uri, got, want)
				}

				delete(container, uri)
			}

			if want := decodeValue(t, []byte(tc.root)); !reflect.DeepEqual(any(bundle), want) {
				t.Errorf("bundle root differs from the source root:\n got %v\nwant %v", bundle, want)
			}
		})
	}
}

// goldenCases adds the documents Schepherd rewrites before bundling to the
// annotated cases.
func goldenCases() map[string]annotated {
	cases := annotatedCases()

	cases["draft-07-top-level-ref"] = annotated{
		rootURI: "https://example.com/golden/shim.json",
		root: `{"$schema":"` + draft7 + `","$id":"https://example.com/golden/shim.json","title":"Shim",
			"$ref":"https://other.example/golden/shimmed.json","definitions":{"kept":{"type":"null"}}}`,
		docs: map[string]string{
			"https://other.example/golden/shimmed.json": `{"$schema":"` + draft7 + `","$ref":"#/definitions/obj",
				"definitions":{"obj":{"type":"object","additionalProperties":{"$ref":"#/definitions/leaf"}},"leaf":{"type":"number"}}}`,
		},
	}
	cases["fragment-root"] = annotated{
		rootURI: "https://example.com/golden/doc.json#/$defs/item",
		root: `{"$schema":"` + d202012 + `","$id":"https://example.com/golden/doc.json","type":"array",
			"$defs":{"item":{"type":"object","properties":{"size":{"$ref":"https://other.example/golden/2020/size.json"}}}}}`,
		docs: map[string]string{
			"https://other.example/golden/2020/size.json": `{"$schema":"` + d202012 + `","type":"integer","minimum":0}`,
		},
	}

	return cases
}

// TestPinnedBundlerOutput pins the exact bytes the pinned bundler produces,
// so a bundler upgrade that changes prepared schemas fails here and its
// effect is reviewed in the diff of the golden files. The files are not
// named .json so that formatters leave member order and literals alone.
func TestPinnedBundlerOutput(t *testing.T) {
	for name, tc := range goldenCases() {
		t.Run(name, func(t *testing.T) {
			r := (&run{web: &fakeWeb{docs: tc.docs}, rootURI: tc.rootURI, root: tc.root}).do(t)
			got := r.wantBundled(t).Schema
			file := filepath.Join(goldenDir, name+".golden")

			if *updateGolden {
				if err := os.MkdirAll(goldenDir, 0o755); err != nil {
					t.Fatal(err)
				}

				if err := os.WriteFile(file, slices.Concat(got, []byte("\n")), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			want, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("%v; run %s", err, regenerateGolden)
			}

			// A checkout may end the line with CRLF; compact JSON has no raw CR.
			if want = bytes.TrimRight(want, "\r\n"); !bytes.Equal(got, want) {
				t.Errorf("the bundler output changed; review it and run %s\n got %s\nwant %s", regenerateGolden, got, want)
			}
		})
	}
}
