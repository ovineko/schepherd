package bundle

import (
	"bytes"
	"encoding/json"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/jsonutil"
)

func mustSet(t *testing.T, input, key, value string) string {
	t.Helper()

	tl, err := scanTopLevel([]byte(input))
	if err != nil {
		t.Fatalf("scanTopLevel(%s) = %v", input, err)
	}

	out, err := setStringMember([]byte(input), tl, key, value)
	if err != nil {
		t.Fatalf("setStringMember(%s) = %v", input, err)
	}

	if err := jsonutil.Check(out, 0); err != nil {
		t.Fatalf("result %s is not strict JSON: %v", out, err)
	}

	return string(out)
}

func TestSetStringMemberInsertsFirstAndPreservesBytes(t *testing.T) {
	cases := []struct {
		name, input, want string
	}{
		{
			name:  "big numbers and escapes",
			input: `{"type":"object","maximum":12345678901234567890123,"minimum":-0,"x":1.50e+00,"title":"café \/ \"q\"","enum":[1E400]}`,
			want:  `{"$id":"https://example.com/s.json","type":"object","maximum":12345678901234567890123,"minimum":-0,"x":1.50e+00,"title":"café \/ \"q\"","enum":[1E400]}`,
		},
		{
			name:  "whitespace kept",
			input: "{\n  \"type\" : \"string\"\n}\n",
			want:  "{\"$id\":\"https://example.com/s.json\",\n  \"type\" : \"string\"\n}\n",
		},
		{
			name:  "leading whitespace before object",
			input: " \t{\"a\":1}",
			want:  " \t{\"$id\":\"https://example.com/s.json\",\"a\":1}",
		},
		{
			name:  "empty object",
			input: "{ }",
			want:  `{"$id":"https://example.com/s.json" }`,
		},
		{
			name:  "nested $id is not top level",
			input: `{"properties":{"$id":{"type":"string"}}}`,
			want:  `{"$id":"https://example.com/s.json","properties":{"$id":{"type":"string"}}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustSet(t, tc.input, "$id", "https://example.com/s.json"); got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestSetStringMemberReplacesOnlyTheValue(t *testing.T) {
	input := `{"a":12345678901234567890123, "$id" :  "rel.json" ,"b":" "}`
	want := `{"a":12345678901234567890123, "$id" :  "https://example.com/rel.json" ,"b":" "}`

	if got := mustSet(t, input, "$id", "https://example.com/rel.json"); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}

	draft4 := `{"id":"x.json","$schema":"http://json-schema.org/draft-04/schema#"}`
	if got := mustSet(t, draft4, "id", "https://e.org/x.json"); got != `{"id":"https://e.org/x.json","$schema":"http://json-schema.org/draft-04/schema#"}` {
		t.Errorf("draft-04 id: %s", got)
	}
}

func TestSetStringMemberEncodesValueWithoutHTMLEscaping(t *testing.T) {
	got := mustSet(t, `{}`, "$id", `https://example.com/a?b=1&c=<d>"`)
	if got != `{"$id":"https://example.com/a?b=1&c=<d>\""}` {
		t.Errorf("got %s", got)
	}
}

func TestSetStringMemberRejectsNonObjects(t *testing.T) {
	for _, input := range []string{`true`, `[1]`, `"s"`, `12`} {
		tl, err := scanTopLevel([]byte(input))
		if err != nil {
			t.Fatalf("scanTopLevel(%s) = %v", input, err)
		}

		if tl.isObject {
			t.Errorf("%s reported as object", input)
		}

		if _, err := setStringMember([]byte(input), tl, "$id", "x"); err == nil {
			t.Errorf("setStringMember(%s) succeeded", input)
		}
	}
}

func TestStringMember(t *testing.T) {
	data := []byte(`{"$schema":"http://json-schema.org/draft-07/schema#","$id":7,"s":"a\"b"}`)

	tl, err := scanTopLevel(data)
	if err != nil {
		t.Fatal(err)
	}

	if v, ok, err := tl.stringMember(data, "$schema"); err != nil || !ok || v != "http://json-schema.org/draft-07/schema#" {
		t.Errorf("$schema = %q %v %v", v, ok, err)
	}

	if v, ok, err := tl.stringMember(data, "s"); err != nil || !ok || v != `a"b` {
		t.Errorf("s = %q %v %v", v, ok, err)
	}

	if _, ok, err := tl.stringMember(data, "$id"); err == nil || !ok {
		t.Errorf("non-string $id: ok=%v err=%v", ok, err)
	}

	if _, ok, err := tl.stringMember(data, "missing"); err != nil || ok {
		t.Errorf("missing: ok=%v err=%v", ok, err)
	}
}

func TestHasNonFragmentRef(t *testing.T) {
	cases := map[string]bool{
		`{}`:                         false,
		`true`:                       false,
		`{"$ref":"#/definitions/a"}`: false,
		`{"$ref":"#"}`:               false,
		`{"a":{"$ref":"#a"},"b":[{"$ref":"#/x"}]}`: false,
		`{"$ref":"other.json"}`:                    true,
		`{"a":[{"b":{"$ref":"https://x/y"}}]}`:     true,
		`{"$dynamicRef":"meta.json#m"}`:            true,
		`{"$recursiveRef":"#"}`:                    false,
		`{"$recursiveRef":"x"}`:                    true,
		`{"x":"$ref","y":"other.json"}`:            false,
		`{"$ref":{"$ref":"#"}}`:                    false,
		`{"$ref":["other.json"]}`:                  false,
		`{"$ref":1,"z":"other.json"}`:              false,
		`["$ref","other.json"]`:                    false,
		`{"a":{},"$ref":"x.json"}`:                 true,
		`{"a":[],"b":"x.json","$ref":"#"}`:         false,
		`{"$ref":"http://json-schema.org/draft-07/schema#/definitions/nonNegativeInteger"}`: false,
		`{"$ref":"https://json-schema.org/draft/2020-12/schema"}`:                           false,
		`{"$ref":"http://json-schema.org/draft-07/schema-extra#"}`:                          true,
		`{"$ref":"https://json-schema.org/draft/2020-12/meta/nope"}`:                        true,
	}

	for input, want := range cases {
		got, err := hasNonFragmentRef([]byte(input))
		if err != nil {
			t.Errorf("hasNonFragmentRef(%s) = %v", input, err)
		}

		if got != want {
			t.Errorf("hasNonFragmentRef(%s) = %v, want %v", input, got, want)
		}

		if ref := referenceNonFragment(t, []byte(input)); ref != want {
			t.Errorf("reference implementation disagrees for %s", input)
		}
	}
}

// referenceNonFragment is a straightforward reimplementation over decoded
// values, used to cross-check the streaming scanner.
func referenceNonFragment(t *testing.T, data []byte) bool {
	t.Helper()

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}

	var walk func(any) bool

	walk = func(v any) bool {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				if s, ok := child.(string); ok {
					if _, isRef := referenceKeywords[k]; isRef && !strings.HasPrefix(s, "#") && !isOfficialMetaschemaURI(s) {
						return true
					}
				}

				if walk(child) {
					return true
				}
			}
		case []any:
			if slices.ContainsFunc(x, walk) {
				return true
			}
		}

		return false
	}

	return walk(v)
}

func isOfficialMetaschemaURI(ref string) bool {
	for _, prefix := range []string{"http://json-schema.org/draft-0", "https://json-schema.org/draft-0", "http://json-schema.org/draft/20", "https://json-schema.org/draft/20"} {
		if strings.HasPrefix(ref, prefix) {
			document, _, _ := strings.Cut(ref, "#")

			return isWellKnownMetaschema(document)
		}
	}

	return false
}

func TestDialects(t *testing.T) {
	for _, uri := range []string{
		"http://json-schema.org/draft-04/schema#", "https://json-schema.org/draft-04/schema",
		"http://json-schema.org/draft-06/schema#", "http://json-schema.org/draft-07/schema#",
		"https://json-schema.org/draft-07/schema", "https://json-schema.org/draft/2019-09/schema",
		"https://json-schema.org/draft/2020-12/schema", "http://json-schema.org/draft/2020-12/schema#",
	} {
		if _, ok := lookupDialect(uri); !ok {
			t.Errorf("lookupDialect(%s) failed", uri)
		}

		if !isWellKnownMetaschema(uri) {
			t.Errorf("isWellKnownMetaschema(%s) = false", uri)
		}
	}

	for _, uri := range []string{
		"http://json-schema.org/draft-03/schema#", "https://example.com/meta.json", "json-schema.org/draft-07/schema",
		"http://json-schema.org/draft-07/hyper-schema#", "ftp://json-schema.org/draft-07/schema", "",
		"https://json-schema.org/draft/2020-12/schema#/x",
	} {
		if _, ok := lookupDialect(uri); ok {
			t.Errorf("lookupDialect(%s) succeeded", uri)
		}
	}

	if !isWellKnownMetaschema("https://json-schema.org/draft/2020-12/meta/applicator") {
		t.Error("2020-12 applicator vocabulary not well known")
	}

	if isWellKnownMetaschema("https://json-schema.org/draft/2020-12/meta/nope") || isWellKnownMetaschema("https://example.com/draft-07/schema") {
		t.Error("unknown metaschema reported as well known")
	}

	if d, _ := lookupDialect("http://json-schema.org/draft-04/schema#"); d.idKey != "id" || !d.legacy {
		t.Errorf("draft-04 = %+v", d)
	}

	if d, _ := lookupDialect("https://json-schema.org/draft/2020-12/schema"); d.idKey != "$id" || d.legacy {
		t.Errorf("2020-12 = %+v", d)
	}
}

func TestRetrievalBase(t *testing.T) {
	good := map[string]string{
		"https://example.com/a.json":  "https://example.com/a.json",
		"https://example.com/a.json#": "https://example.com/a.json",
		"urn:example:schema":          "urn:example:schema",
	}

	for in, want := range good {
		got, _, err := retrievalBase(in)
		if err != nil || got != want {
			t.Errorf("retrievalBase(%s) = %q, %v; want %q", in, got, err, want)
		}
	}

	for _, in := range []string{"a.json", "/abs/a.json", "file:///etc/passwd", "FILE:///x", "https://e.com/a#/x", "%zz", ""} {
		if _, _, err := retrievalBase(in); err == nil {
			t.Errorf("retrievalBase(%s) succeeded", in)
		}
	}
}

func TestResolveID(t *testing.T) {
	base, err := url.Parse("https://example.com/schemas/a.json")
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"https://example.com/schemas/a.json":  "https://example.com/schemas/a.json",
		"https://example.com/schemas/a.json#": "https://example.com/schemas/a.json",
		"a.json":                              "https://example.com/schemas/a.json",
		"../b.json":                           "https://example.com/b.json",
		"#":                                   "https://example.com/schemas/a.json",
	}

	for id, want := range cases {
		if got, err := resolveID(base, id); err != nil || got != want {
			t.Errorf("resolveID(%s) = %q, %v; want %q", id, got, err, want)
		}
	}

	for _, id := range []string{"#anchor", "a.json#/x", "%zz"} {
		if _, err := resolveID(base, id); err == nil {
			t.Errorf("resolveID(%s) succeeded", id)
		}
	}
}

func TestParseInspection(t *testing.T) {
	report := `{
  "mode": "static",
  "locations": {
    "static": {
      "https://example.com/root.json": {"type": "resource", "root": "https://example.com/root.json", "pointer": ""},
      "https://example.com/root.json#/properties/a": {"type": "pointer"},
      "https://example.com/nested.json": {"type": "resource"},
      "https://example.com/root.json#name": {"type": "anchor"}
    },
    "dynamic": {"https://example.com/root.json#meta": {"type": "anchor"}}
  },
  "references": [
    {"type": "static", "origin": "/$schema", "position": [1,2,3,4], "original": "http://json-schema.org/draft-07/schema#", "destination": "http://json-schema.org/draft-07/schema", "base": "http://json-schema.org/draft-07/schema", "fragment": null},
    {"type": "static", "origin": "/properties/a/$ref", "original": "common.json#/definitions/name", "destination": "https://example.com/common.json#/definitions/name", "base": "https://example.com/common.json", "fragment": "/definitions/name"},
    {"type": "static", "origin": "/properties/b/$ref", "original": "#name", "destination": "https://example.com/root.json#name", "base": "https://example.com/root.json", "fragment": "name"},
    {"type": "static", "origin": "/properties/c/$ref", "original": "nested.json", "destination": "https://example.com/nested.json", "base": "https://example.com/nested.json", "fragment": null},
    {"type": "static", "origin": "/definitions/x/$schema", "original": "https://example.com/meta", "destination": "https://example.com/meta", "base": "https://example.com/meta", "fragment": null},
    {"type": "static", "origin": "/properties/m/$ref", "original": "http://json-schema.org/draft-07/schema#", "destination": "http://json-schema.org/draft-07/schema", "base": "http://json-schema.org/draft-07/schema", "fragment": null}
  ]
}`

	in, err := parseInspection(strings.NewReader(report))
	if err != nil {
		t.Fatalf("parseInspection = %v", err)
	}

	wantResources := map[string]struct{}{"https://example.com/root.json": {}, "https://example.com/nested.json": {}}
	if !reflect.DeepEqual(in.resources, wantResources) {
		t.Errorf("resources = %v", in.resources)
	}

	want := []Ref{
		{Origin: "/properties/a/$ref", Destination: "https://example.com/common.json#/definitions/name", Base: "https://example.com/common.json"},
		{Origin: "/definitions/x/$schema", Destination: "https://example.com/meta", Base: "https://example.com/meta"},
		{Origin: "/properties/m/$ref", Destination: "http://json-schema.org/draft-07/schema", Base: "http://json-schema.org/draft-07/schema"},
	}

	if got := in.external(); !reflect.DeepEqual(got, want) {
		t.Errorf("external = %+v\nwant %+v", got, want)
	}

	if !dependsOnBase(in) {
		t.Error("dependsOnBase = false")
	}

	metaOnly := &inspection{refs: []inspectedRef{
		{Origin: "/$schema", Original: "https://example.com/meta", Base: "https://example.com/meta"},
		{Origin: "/properties/m/$ref", Original: "http://json-schema.org/draft-07/schema#/definitions/x", Base: "http://json-schema.org/draft-07/schema"},
		{Origin: "/properties/f/$ref", Original: "#/definitions/f", Base: "https://example.com/root.json"},
	}}
	if dependsOnBase(metaOnly) {
		t.Error("dependsOnBase counts $schema, fragments or official metaschemas")
	}

	for _, bad := range []string{``, `[]`, `{"locations":[]}`, `{"references":{}}`, `{"a":1} {}`, `{"locations":{"static":{"x":1}}}`} {
		if _, err := parseInspection(strings.NewReader(bad)); err == nil {
			t.Errorf("parseInspection(%s) succeeded", bad)
		}
	}
}

func TestCountFileURIs(t *testing.T) {
	cases := map[string]int{
		`{}`: 0,
		`{"description":"use file://x or file:\/\/y","file://k":"file://v file://w"}`: 5,
		`["file:/x","FILE://y"]`: 0,
	}

	for input, want := range cases {
		if got, err := countFileURIs([]byte(input)); err != nil || got != want {
			t.Errorf("countFileURIs(%s) = %d, %v; want %d", input, got, err, want)
		}
	}
}

func TestCompilePatternFallsBackForUnsupportedSyntax(t *testing.T) {
	re, err := compilePattern(`^a+$`)
	if err != nil || !re.MatchString("aa") || re.MatchString("b") {
		t.Errorf("RE2 pattern: %v", err)
	}

	lookahead, err := compilePattern(`^(?!foo)`)
	if err != nil {
		t.Fatalf("lookahead: %v", err)
	}

	if !lookahead.MatchString("foo") || lookahead.String() != `^(?!foo)` {
		t.Errorf("unsupported pattern should match everything and keep its source")
	}
}

func TestLimitsWithDefaults(t *testing.T) {
	got := Limits{MaxDepth: 3, MaxTotalBytes: -1}.withDefaults()
	def := DefaultLimits()

	if got.MaxDepth != 3 || got.MaxDocuments != def.MaxDocuments || got.MaxDocumentBytes != def.MaxDocumentBytes || got.MaxTotalBytes != def.MaxTotalBytes {
		t.Errorf("withDefaults = %+v", got)
	}
}
