package bundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/digest"
)

const d201909 = "https://json-schema.org/draft/2019-09/schema"

// topLevelMembers decodes the members of a JSON object.
func topLevelMembers(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()

	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}

	return members
}

func TestFragmentRootIsTheNamedSubschema(t *testing.T) {
	common := map[string]string{"https://example.com/f/common.json": `{"$schema":"` + draft7 + `","type":"integer","minimum":0}`}

	cases := map[string]struct {
		docs      map[string]string
		doc       string
		fragment  string
		identity  string
		container string
		instances []Instance
		checked   Checked
	}{
		"draft-07 pointer with an external reference": {
			docs: common,
			doc: `{"$schema":"` + draft7 + `","$id":"https://example.com/f/doc.json","type":"object","required":["whole"],
				"definitions":{"name":{"type":"string","minLength":2},
				"item":{"properties":{"n":{"$ref":"#/definitions/name"},"c":{"$ref":"common.json"}},"required":["n"]}}}`,
			fragment: "/definitions/item", identity: "https://example.com/f/doc.json", container: "definitions",
			instances: instances("item.json", `{"n":"ab","c":1}`, "short.json", `{"n":"a"}`, "negative.json", `{"n":"ab","c":-1}`),
			checked:   Checked{Valid: 1, Invalid: 2, Agreed: 3},
		},
		"2020-12 anchor without an identifier": {
			doc: `{"$schema":"` + d202012 + `","type":"array",
				"$defs":{"n":{"$anchor":"nm","type":"string","maxLength":2},"pair":{"$ref":"#nm","minLength":1}}}`,
			fragment: "nm", identity: "https://example.com/f/doc.json", container: "$defs",
			instances: instances("ok.json", `"ab"`, "long.json", `"abc"`, "array.json", `[]`),
			checked:   Checked{Valid: 1, Invalid: 2, Agreed: 3},
		},
		"2019-09 pointer with a relative identifier": {
			docs: map[string]string{"https://example.com/f/common.json": `{"$schema":"` + d201909 + `","type":"integer"}`},
			doc: `{"$schema":"` + d201909 + `","$id":"doc.json","$defs":{"x":{"type":"object","properties":{"c":{"$ref":"common.json"}},
				"unevaluatedProperties":false}}}`,
			fragment: "/$defs/x", identity: "https://example.com/f/doc.json", container: "$defs",
			instances: instances("ok.json", `{"c":1}`, "extra.json", `{"c":1,"d":2}`, "string.json", `{"c":"1"}`),
			checked:   Checked{Valid: 1, Invalid: 2, Agreed: 3},
		},
		"draft-04 pointer": {
			doc:      `{"$schema":"` + draft4 + `","id":"https://example.com/f/doc.json","definitions":{"n":{"type":"number","maximum":5,"exclusiveMaximum":true}}}`,
			fragment: "/definitions/n", identity: "https://example.com/f/doc.json", container: "definitions",
			instances: instances("ok.json", `4.9`, "edge.json", `5`),
			checked:   Checked{Valid: 1, Invalid: 1, Agreed: 2},
		},
		"draft-04 identifier with an empty fragment": {
			doc:      `{"$schema":"` + draft4 + `","id":"https://example.com/f/doc.json#","definitions":{"n":{"type":"integer","minimum":3}}}`,
			fragment: "/definitions/n", identity: "https://example.com/f/doc.json", container: "definitions",
			instances: instances("ok.json", `3`, "small.json", `2`),
			checked:   Checked{Valid: 1, Invalid: 1, Agreed: 2},
		},
		"draft-07 top-level $ref with an identifier with an empty fragment": {
			doc: `{"$schema":"` + draft7 + `","$id":"https://example.com/f/doc.json#","$ref":"#/definitions/n",
				"definitions":{"n":{"type":"string","pattern":"^a"}}}`,
			fragment: "/definitions/n", identity: "https://example.com/f/doc.json", container: "definitions",
			instances: instances("ok.json", `"ab"`, "other.json", `"b"`),
			checked:   Checked{Valid: 1, Invalid: 1, Agreed: 2},
		},
		"draft-07 plain-name identifier": {
			doc:      `{"$schema":"` + draft7 + `","$id":"https://example.com/f/doc.json","definitions":{"n":{"$id":"#item","enum":["a","b"]}}}`,
			fragment: "item", identity: "https://example.com/f/doc.json", container: "definitions",
			instances: instances("ok.json", `"a"`, "other.json", `"c"`),
			checked:   Checked{Valid: 1, Invalid: 1, Agreed: 2},
		},
		"draft-07 document that is a top-level $ref": {
			doc: `{"$schema":"` + draft7 + `","$ref":"#/definitions/root","title":"T",
				"definitions":{"root":{"type":"object"},"leaf":{"type":"boolean"}}}`,
			fragment: "/definitions/leaf", identity: "https://example.com/f/doc.json", container: "definitions",
			instances: instances("ok.json", `true`, "object.json", `{}`),
			checked:   Checked{Valid: 1, Invalid: 1, Agreed: 2},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rootURI := "https://example.com/f/doc.json#" + tc.fragment
			r := (&run{web: &fakeWeb{docs: tc.docs}, rootURI: rootURI, root: tc.doc, instances: tc.instances}).do(t)
			p := r.wantBundled(t)

			if p.Checked != tc.checked {
				t.Errorf("checked = %+v, want %+v", p.Checked, tc.checked)
			}

			members := topLevelMembers(t, p.Schema)
			if _, ok := members["$id"]; ok {
				t.Errorf("the fragment root has an identifier: %s", p.Schema)
			}

			if _, ok := members["id"]; ok {
				t.Errorf("the fragment root has an identifier: %s", p.Schema)
			}

			wantAllOf := `[{"$ref":"` + tc.identity + `#` + tc.fragment + `"}]`
			if got := string(members["allOf"]); got != wantAllOf {
				t.Errorf("allOf = %s, want %s", got, wantAllOf)
			}

			embedded := topLevelMembers(t, members[tc.container])
			if _, ok := embedded[tc.identity]; !ok {
				t.Errorf("%s does not embed the document as %s: %s", tc.container, tc.identity, p.Schema)
			}

			if bytes.Contains(p.Schema, []byte("urn:")) {
				t.Errorf("the temporary root identifier leaked: %s", p.Schema)
			}

			if r.closure.Root.URI != rootURI || r.closure.Root.Digest != digest.FromBytes([]byte(tc.doc)) {
				t.Errorf("closure root = %s %s", r.closure.Root.URI, r.closure.Root.Digest)
			}

			var deps []string
			for _, dep := range p.Dependencies {
				deps = append(deps, dep.URI)
			}

			var want []string
			for uri := range tc.docs {
				want = append(want, uri)
			}

			slices.Sort(want)

			if !slices.Equal(deps, want) {
				t.Errorf("dependencies = %v, want %v (the document itself is the source)", deps, want)
			}
		})
	}
}

func TestFragmentRootWithoutInstancesHasNoBehaviourCounts(t *testing.T) {
	r := (&run{
		rootURI: "https://example.com/f/doc.json#/definitions/n",
		root:    `{"$schema":"` + draft7 + `","definitions":{"n":{"type":"string"}}}`,
	}).do(t)

	if p := r.wantBundled(t); p.Checked != (Checked{}) {
		t.Errorf("checked = %+v", p.Checked)
	}
}

func TestRewrittenRootAdoptsItsDeclaredIdentifier(t *testing.T) {
	const (
		retrieved = "https://www.example.com/r/root.json"
		declared  = "https://mirror.example.com/r/root.json"
	)

	root := `{"$schema":"` + draft7 + `","$id":"` + declared + `","$ref":"dep.json","title":"Root"}`
	dep := `{"$schema":"` + draft7 + `","$id":"https://mirror.example.com/r/dep.json","$ref":"#/definitions/port",
		"definitions":{"port":{"type":"integer","minimum":1}}}`

	web := &fakeWeb{docs: map[string]string{declared: root, "https://mirror.example.com/r/dep.json": dep}}
	r := (&run{
		web: web, rootURI: retrieved, root: root,
		instances: instances("ok.json", `80`, "zero.json", `0`, "text.json", `"80"`),
	}).do(t)

	p := r.wantBundled(t)

	if want := (Checked{Valid: 1, Invalid: 2, Agreed: 3}); p.Checked != want {
		t.Errorf("checked = %+v, want %+v", p.Checked, want)
	}

	if want := []string{declared, "https://mirror.example.com/r/dep.json"}; !reflect.DeepEqual(web.calls, want) {
		t.Errorf("fetch calls = %v, want %v", web.calls, want)
	}

	if len(p.Dependencies) != 1 || p.Dependencies[0].URI != "https://mirror.example.com/r/dep.json" {
		t.Errorf("dependencies = %+v", p.Dependencies)
	}

	if !bytes.Contains(p.Schema, []byte(`"$id":"`+declared+`"`)) {
		t.Errorf("the declared identifier was not kept: %s", p.Schema)
	}

	for name, docs := range map[string]map[string]string{
		"different document": {declared: strings.Replace(root, "Root", "Other", 1)},
		"missing":            {},
	} {
		t.Run(name, func(t *testing.T) {
			r := (&run{web: &fakeWeb{docs: docs}, rootURI: retrieved, root: root}).do(t)
			if detail := r.wantReason(t, ReasonIDMismatch); !strings.Contains(detail, declared) {
				t.Errorf("detail = %q", detail)
			}

			if name == "missing" && !errors.Is(r.err, errNotFound) {
				t.Errorf("the fetch error is not reachable: %v", r.err)
			}
		})
	}

	web = &fakeWeb{docs: map[string]string{"https://www.example.com/r/dep.json": `{"$schema":"` + draft7 + `","type":"string"}`}}
	r = (&run{
		web: web, rootURI: retrieved,
		root:      `{"$schema":"` + draft7 + `","$id":"` + retrieved + `","$ref":"dep.json"}`,
		instances: instances("ok.json", `"x"`, "bad.json", `1`),
	}).do(t)
	r.wantBundled(t)

	if want := []string{"https://www.example.com/r/dep.json"}; !reflect.DeepEqual(web.calls, want) {
		t.Errorf("an identifier equal to the retrieval URI was fetched: %v", web.calls)
	}
}

func TestRewrittenRootWithOnlyInternalReferencesIsBundled(t *testing.T) {
	const uri = "https://example.com/self/root.json"

	root := `{"$schema":"` + draft7 + `","$id":"` + uri + `","$ref":"` + uri + `#/definitions/a","definitions":{"a":{"type":"string"}}}`

	r := (&run{rootURI: uri, root: root, instances: instances("ok.json", `"a"`, "bad.json", `1`)}).do(t)
	p := r.wantBundled(t)

	if len(r.web.calls) != 0 || len(p.Dependencies) != 0 || !bytes.Contains(p.Schema, []byte(`"allOf":[{"$ref":"`+uri+`#/definitions/a"}]`)) {
		t.Errorf("prepared = %s, calls %v", p.Schema, r.web.calls)
	}
}

func TestWrapLegacyRef(t *testing.T) {
	input := `{ "$schema" : "x", "$ref":"#/definitions/a" , "definitions":{"a":{"maximum":1.50}} }`

	tl, err := scanTopLevel([]byte(input))
	if err != nil {
		t.Fatal(err)
	}

	got, err := wrapLegacyRef([]byte(input), tl, "https://example.com/x.json", draft7)
	if err != nil {
		t.Fatal(err)
	}

	if want := `{ "$schema" : "x", "allOf":[{"$ref":"#/definitions/a"}] , "definitions":{"a":{"maximum":1.50}} }`; string(got) != want {
		t.Errorf("wrapLegacyRef = %s, want %s", got, want)
	}

	for _, input := range []string{`{"$ref":"#","not":{}}`, `{"required":[],"$ref":"#"}`} {
		tl, err := scanTopLevel([]byte(input))
		if err != nil {
			t.Fatal(err)
		}

		if _, err := wrapLegacyRef([]byte(input), tl, "https://example.com/x.json", draft7); reasonOf(err) != ReasonTopLevelRefDraft7 {
			t.Errorf("wrapLegacyRef(%s) = %v", input, err)
		}
	}
}

func TestValidFragment(t *testing.T) {
	for fragment, want := range map[string]bool{
		"/definitions/a": true, "/": true, "/$defs/tasks": true, "name": true, "_x-1.2": true,
		"1abc": false, "definitions/a": false, "a b": false, "%2Fa": false,
	} {
		if got := validFragment(fragment); got != want {
			t.Errorf("validFragment(%q) = %v, want %v", fragment, got, want)
		}
	}
}
