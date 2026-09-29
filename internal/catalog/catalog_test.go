package catalog_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/calver"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/jsonutil"
	"github.com/ovineko/schepherd/internal/match"
)

func TestDefaultLimits(t *testing.T) {
	t.Parallel()

	got := catalog.DefaultLimits()
	if got.MaxEntries != 20000 || got.MaxManifestBytes != 4<<20 {
		t.Fatalf("DefaultLimits() = %+v, want 20000 entries and 4 MiB manifests (docs/oci-format.md)", got)
	}
}

func TestParseValid(t *testing.T) {
	t.Parallel()

	longID := strings.Repeat("a", 128)

	cases := []struct {
		want   func() *catalog.Catalog
		name   string
		doc    string
		limits catalog.Limits
	}{
		{
			name: "empty catalog",
			doc:  `{"formatVersion":2,"revision":"20260923.1200","schemas":[]}`,
			want: func() *catalog.Catalog {
				return &catalog.Catalog{FormatVersion: 2, Revision: testRevision, Schemas: []catalog.Entry{}}
			},
		},
		{
			name: "every member",
			doc:  string(encode(t, validDoc())),
			want: validCatalog,
		},
		{
			name: "canonical form",
			doc:  validCanonical(),
			want: validCatalog,
		},
		{
			name: "indented with members in document order of choice",
			doc: `
			{
				"schemas": [
					{ "artifact": { "size": 512, "digest": "` + digestA + `", "mediaType": "` + catalog.ManifestMediaType + `" },
					  "name": "Alpha", "id": "alpha" }
				],
				"revision" : "20260923.1200",
				"formatVersion" : 2
			}`,
			want: func() *catalog.Catalog {
				c := validCatalog()
				c.Schemas = c.Schemas[:1]

				return c
			},
		},
		{
			name: "id bounds and byte order",
			doc: string(encode(t, map[string]any{
				"formatVersion": 2,
				"revision":      testRevision,
				"schemas": []any{
					entryWithID("0"), entryWithID("a"), entryWithID("a-b"), entryWithID("a.b"),
					entryWithID("a.b-c_d9"), entryWithID("a_b"), entryWithID(longID),
				},
			})),
			want: func() *catalog.Catalog {
				c := &catalog.Catalog{FormatVersion: 2, Revision: testRevision}
				for _, id := range []string{"0", "a", "a-b", "a.b", "a.b-c_d9", "a_b", longID} {
					c.Schemas = append(c.Schemas, catalog.Entry{
						ID: id, Name: "Entry " + id,
						Artifact: catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: digestA, Size: 512},
					})
				}

				return c
			},
		},
		{
			name: "limits at their bounds",
			doc:  validCanonical(),
			want: validCatalog,
			limits: catalog.Limits{
				MaxEntries:       2,
				MaxManifestBytes: 700,
			},
		},
		{
			name: "unicode text, opaque dialect, upper-case scheme and IPv6 host",
			doc: string(mutated(t, func(d map[string]any) {
				entryOf(d, 0)["name"] = "Sch\u00e9ma \u2713 \u65e5\u672c\u8a9e"
				entryOf(d, 1)["dialect"] = "urn:example:dialect"
				provenanceOf(d)["source"] = "HTTP://[2001:db8::1]:8080/beta.json?rev=1#top"
			})),
			want: func() *catalog.Catalog {
				c := validCatalog()
				c.Schemas[0].Name = "Sch\u00e9ma \u2713 \u65e5\u672c\u8a9e"
				c.Schemas[1].Dialect = "urn:example:dialect"
				c.Schemas[1].Provenance.Source = "HTTP://[2001:db8::1]:8080/beta.json?rev=1#top"

				return c
			},
		},
		{
			name: "line and paragraph separators in the multiline description",
			doc: string(mutated(t, func(d map[string]any) {
				entryOf(d, 1)["description"] = "One\u2028two\u2029three"
			})),
			want: func() *catalog.Catalog {
				c := validCatalog()
				c.Schemas[1].Description = "One\u2028two\u2029three"

				return c
			},
		},
		{
			name: "entries sharing a digest with equal sizes",
			doc: string(mutated(t, func(d map[string]any) {
				artifactIn(d, 1)["digest"] = digestA
				artifactIn(d, 1)["size"] = 512
			})),
			want: func() *catalog.Catalog {
				c := validCatalog()
				c.Schemas[1].Artifact = c.Schemas[0].Artifact

				return c
			},
		},
	}

	schema := compileCatalogSchema(t)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := catalog.Parse([]byte(tc.doc), tc.limits)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}

			if want := tc.want(); !reflect.DeepEqual(got, want) {
				t.Fatalf("Parse() =\n%#v\nwant\n%#v", got, want)
			}

			if err := validateAgainstSchema(schema, []byte(tc.doc)); err != nil {
				t.Fatalf("api/catalog.schema.json rejects a valid catalog: %v", err)
			}
		})
	}
}

func TestParseFormatVersion(t *testing.T) {
	t.Parallel()

	rest := `,"revision":"20260923.1200","schemas":[]`
	nested := func(levels int) string { return strings.Repeat("[", levels) + strings.Repeat("]", levels) }

	cases := []struct {
		name   string
		doc    string
		msg    string
		kind   fault.Kind
		schema bool
	}{
		{name: "next version", doc: `{"formatVersion":3` + rest + `}`, kind: fault.Usage, msg: "unsupported catalog formatVersion 3", schema: true},
		{name: "previous version", doc: `{"formatVersion":1` + rest + `}`, kind: fault.Usage, msg: "unsupported catalog formatVersion 1: this build supports 2", schema: true},
		{name: "future version with unknown shape", doc: `{"schemas":{"x":null},"extra":true,"formatVersion":4}`, kind: fault.Usage, msg: "formatVersion 4", schema: true},
		{name: "future version nested deeper than format 1", doc: `{"formatVersion":7,"payload":` + nested(100) + `}`, kind: fault.Usage, msg: "formatVersion 7", schema: true},
		{name: "future version nested deeper than jsonutil's default depth", doc: `{"formatVersion":7,"payload":` + nested(600) + `}`, kind: fault.Usage, msg: "formatVersion 7", schema: true},
		{name: "future version at the nesting encoding/json reads", doc: `{"formatVersion":7,"payload":` + nested(9999) + `}`, kind: fault.Usage, msg: "formatVersion 7", schema: true},
		{name: "future version nested beyond what encoding/json reads", doc: `{"formatVersion":7,"payload":` + nested(10000) + `}`, kind: fault.Integrity, msg: "malformed JSON"},
		{name: "duplicate member after deep nesting", doc: `{"formatVersion":2,"payload":` + nested(600) + `,"formatVersion":2}`, kind: fault.Integrity, msg: `duplicate object key "formatVersion"`},
		{name: "huge version", doc: `{"formatVersion":100000000000000000000}`, kind: fault.Usage, msg: "100000000000000000000", schema: true},
		{name: "string", doc: `{"formatVersion":"1"` + rest + `}`, kind: fault.Integrity, msg: "must be a positive integer", schema: true},
		{name: "fraction", doc: `{"formatVersion":2.0` + rest + `}`, kind: fault.Integrity, msg: "got 2.0"},
		{name: "exponent", doc: `{"formatVersion":2.0` + rest + `}`, kind: fault.Integrity, msg: "got 2.0"},
		{name: "non-integer", doc: `{"formatVersion":2.5` + rest + `}`, kind: fault.Integrity, msg: "got 2.5", schema: true},
		{name: "zero", doc: `{"formatVersion":0` + rest + `}`, kind: fault.Integrity, msg: "got 0", schema: true},
		{name: "negative", doc: `{"formatVersion":-1` + rest + `}`, kind: fault.Integrity, msg: "got -1", schema: true},
		{name: "null", doc: `{"formatVersion":null` + rest + `}`, kind: fault.Integrity, msg: "got null", schema: true},
		{name: "boolean", doc: `{"formatVersion":true` + rest + `}`, kind: fault.Integrity, msg: "got true", schema: true},
		{name: "object", doc: `{"formatVersion":{}` + rest + `}`, kind: fault.Integrity, msg: "got {}", schema: true},
		{name: "array", doc: `{"formatVersion":[1]` + rest + `}`, kind: fault.Integrity, msg: "got [1]", schema: true},
		{name: "missing", doc: `{"revision":"20260923.1200","schemas":[]}`, kind: fault.Integrity, msg: "formatVersion is missing", schema: true},
		{name: "case variant", doc: `{"FormatVersion":1` + rest + `}`, kind: fault.Integrity, msg: "formatVersion is missing", schema: true},
		{name: "duplicate member", doc: `{"formatVersion":2,"formatVersion":2` + rest + `}`, kind: fault.Integrity, msg: "duplicate object key"},
		{name: "leading zero", doc: `{"formatVersion":01` + rest + `}`, kind: fault.Integrity, msg: "malformed JSON"},
		{name: "raw C1 and bidi controls", doc: "{\"formatVersion\":\"\u009b2J\u202eevil\"" + rest + "}", kind: fault.Integrity, msg: `got "\u009b2J\u202eevil"`, schema: true},
		{name: "raw supplementary character", doc: "{\"formatVersion\":[\"\U0001F600\"]" + rest + "}", kind: fault.Integrity, msg: `got ["\ud83d\ude00"]`, schema: true},
		{name: "whitespace inside the value", doc: "{\"formatVersion\":[\n\t1\r]" + rest + "}", kind: fault.Integrity, msg: `got [\u000a\u00091\u000d]`, schema: true},
	}

	schema := compileCatalogSchema(t)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c, err := catalog.Parse([]byte(tc.doc), catalog.DefaultLimits())
			assertFault(t, c, err, tc.kind, tc.msg)
			assertSafeMessage(t, err)

			if tc.schema {
				if err := validateAgainstSchema(schema, []byte(tc.doc)); err == nil {
					t.Fatal("api/catalog.schema.json accepts the document")
				}
			}
		})
	}

	t.Run("exit codes", func(t *testing.T) {
		t.Parallel()

		_, usage := catalog.Parse([]byte(`{"formatVersion":3}`), catalog.Limits{})
		_, integrity := catalog.Parse([]byte(`{"formatVersion":"3"}`), catalog.Limits{})

		if fault.ExitCodeOf(usage) != 2 || fault.ExitCodeOf(integrity) != 5 {
			t.Fatalf("exit codes = %d, %d; want 2 and 5", fault.ExitCodeOf(usage), fault.ExitCodeOf(integrity))
		}
	})
}

var memberName = regexp.MustCompile(`"[A-Za-z]+":`)

type invalidCase struct {
	name   string
	msg    string
	doc    []byte
	limits catalog.Limits
	schema bool
}

func invalidCases(tb testing.TB) []invalidCase {
	tb.Helper()

	canonical := validCanonical()
	replace := func(old, replacement string) []byte {
		if !strings.Contains(canonical, old) {
			tb.Fatalf("canonical test document lacks %q", old)
		}

		return []byte(strings.Replace(canonical, old, replacement, 1))
	}
	set := func(field func(map[string]any) map[string]any, key string, value any) []byte {
		return mutated(tb, func(d map[string]any) { field(d)[key] = value })
	}
	remove := func(field func(map[string]any) map[string]any, key string) []byte {
		return mutated(tb, func(d map[string]any) { delete(field(d), key) })
	}
	root := func(d map[string]any) map[string]any { return d }
	alpha := func(d map[string]any) map[string]any { return entryOf(d, 0) }
	beta := func(d map[string]any) map[string]any { return entryOf(d, 1) }
	alphaArtifact := func(d map[string]any) map[string]any { return artifactIn(d, 0) }
	prov := provenanceOf
	dep := func(d map[string]any) map[string]any { return dependencyOf(d, 0) }
	hexA := digest.Hex(digestA)

	var cases []invalidCase

	add := func(schema bool, name, msg string, doc []byte) {
		cases = append(cases, invalidCase{name: name, msg: msg, doc: doc, schema: schema})
	}

	add(false, "not JSON", "malformed JSON", []byte(`{`))
	add(false, "trailing data", "malformed JSON", []byte(canonical+` {}`))
	add(false, "invalid UTF-8", "not valid UTF-8", []byte("{\"formatVersion\":1,\"revision\":\"\xff\"}"))
	add(false, "byte order mark", "malformed JSON", []byte("\xef\xbb\xbf"+canonical))
	add(true, "top-level array", "must be a JSON object", []byte(`[]`))
	add(true, "top-level null", "must be a JSON object", []byte(`null`))
	add(true, "top-level string", "must be a JSON object", []byte(`"catalog"`))
	add(true, "deep nesting in an unknown member", `document: unknown member "x"`, []byte(`{"formatVersion":2,"x":`+strings.Repeat("[", 600)+strings.Repeat("]", 600)+`}`))
	add(true, "deep nesting in the schemas list", "schemas[0]: must be an object", []byte(`{"formatVersion":2,"revision":"20260923.1200","schemas":`+strings.Repeat("[", 600)+strings.Repeat("]", 600)+`}`))
	add(false, "nesting beyond what encoding/json reads", "malformed JSON", []byte(`{"formatVersion":2,"x":`+strings.Repeat("[", 10000)+strings.Repeat("]", 10000)+`}`))

	add(false, "duplicate key in entry", "duplicate object key \"id\"", replace(`"id":"alpha"`, `"id":"alpha","id":"alpha"`))
	add(false, "duplicate key in artifact", "duplicate object key \"size\"", replace(`"size":512`, `"size":512,"size":1`))
	add(false, "duplicate key in provenance", "duplicate object key \"license\"", replace(`"license":"MIT"`, `"license":"MIT","license":"MIT"`))
	add(false, "duplicate key in dependency", "duplicate object key \"source\"", replace(`"source":"https://schemas.example.com/a.json"`, `"source":"https://schemas.example.com/a.json","source":"https://schemas.example.com/a.json"`))
	add(false, "duplicate root key", "duplicate object key \"schemas\"", replace(`"revision":"20260923.1200",`, `"revision":"20260923.1200","schemas":[],`))

	add(true, "upper-case member names", "formatVersion is missing", []byte(memberName.ReplaceAllStringFunc(canonical, strings.ToUpper)))
	add(true, "case variant of id", `unknown member "ID"`, replace(`"id":"alpha"`, `"ID":"alpha"`))
	add(true, "case variant next to the exact name", `unknown member "ID"`, replace(`"id":"alpha"`, `"id":"alpha","ID":"zeta"`))
	add(true, "case variant of size", `unknown member "Size"`, replace(`"size":512`, `"Size":512`))
	add(true, "case variant of revision", `unknown member "Revision"`, replace(`"revision":"20260923.1200"`, `"revision":"20260923.1200","Revision":"20260923.1201"`))

	add(true, "unknown root member", `document: unknown member "extra"`, set(root, "extra", true))
	add(true, "unknown entry member", `schemas[0]: unknown member "title"`, set(alpha, "title", "Alpha"))
	add(true, "unknown artifact member", `schemas[0].artifact: unknown member "annotations"`, set(alphaArtifact, "annotations", map[string]any{}))
	add(true, "unknown provenance member", `schemas[1].provenance: unknown member "created"`, set(prov, "created", "2026-09-23T00:00:00Z"))
	add(true, "unknown dependency member", `schemas[1].provenance.dependencies[0]: unknown member "size"`, set(dep, "size", 1))

	add(true, "null description", "schemas[1].description: must not be null", set(beta, "description", nil))
	add(true, "null provenance", "schemas[1].provenance: must not be null", set(beta, "provenance", nil))
	add(true, "null fileMatch", "schemas[1].fileMatch: must not be null", set(beta, "fileMatch", nil))
	add(true, "null fileMatch item", "schemas[1].fileMatch[0]: must not be null", set(beta, "fileMatch", []any{nil}))
	add(true, "null schemas", "schemas: must not be null", set(root, "schemas", nil))
	add(true, "null revision", "revision: must not be null", set(root, "revision", nil))
	add(true, "null artifact", "schemas[0].artifact: must not be null", set(alpha, "artifact", nil))
	add(true, "null entry", "schemas[0]: must not be null", set(root, "schemas", []any{nil}))

	add(true, "numeric name", "schemas[0].name: must be a string", set(alpha, "name", 5))
	add(true, "boolean id", "schemas[0].id: must be a string", set(alpha, "id", true))
	add(true, "schemas object", "schemas: must be an array", set(root, "schemas", map[string]any{}))
	add(true, "entry string", "schemas[0]: must be an object", set(root, "schemas", []any{"alpha"}))
	add(true, "fileMatch string", "schemas[1].fileMatch: must be an array", set(beta, "fileMatch", "beta.json"))
	add(true, "artifact array", "schemas[0].artifact: must be an object", set(alpha, "artifact", []any{}))
	add(true, "size string", "schemas[0].artifact.size: must be an integer", set(alphaArtifact, "size", "512"))
	add(true, "provenance string", "schemas[1].provenance: must be an object", set(beta, "provenance", "https://schemas.example.com/beta.json"))
	add(true, "dependencies object", "provenance.dependencies: must be an array", set(prov, "dependencies", map[string]any{}))
	add(true, "numeric revision", "revision: must be a string", []byte(`{"formatVersion":2,"revision":20260923.1200,"schemas":[]}`))

	for _, missing := range []struct {
		field func(map[string]any) map[string]any
		key   string
		where string
	}{
		{root, "revision", "document"},
		{root, "schemas", "document"},
		{alpha, "id", "schemas[0]"},
		{alpha, "name", "schemas[0]"},
		{alpha, "artifact", "schemas[0]"},
		{alphaArtifact, "mediaType", "schemas[0].artifact"},
		{alphaArtifact, "digest", "schemas[0].artifact"},
		{alphaArtifact, "size", "schemas[0].artifact"},
		{prov, "source", "schemas[1].provenance"},
		{dep, "source", "schemas[1].provenance.dependencies[0]"},
		{dep, "digest", "schemas[1].provenance.dependencies[0]"},
	} {
		add(true, "missing "+missing.where+" "+missing.key, fmt.Sprintf("%s: missing required member %q", missing.where, missing.key), remove(missing.field, missing.key))
	}

	for _, empty := range []struct {
		field func(map[string]any) map[string]any
		value any
		key   string
	}{
		{root, "", "revision"},
		{alpha, "", "id"},
		{alpha, "", "name"},
		{beta, "", "description"},
		{beta, "", "dialect"},
		{beta, []any{}, "fileMatch"},
		{prov, "", "license"},
		{prov, "", "sourceDigest"},
		{prov, []any{}, "dependencies"},
	} {
		add(true, "empty "+empty.key, empty.key+": must not be empty", set(empty.field, empty.key, empty.value))
	}

	for _, revision := range []struct {
		value  string
		schema bool
	}{
		{"2026-09-23.1200", true},
		{"20260923", true},
		{"20260923.1", true},
		{"20260923.120", true},
		{"20260923.12000", true},
		{"20260923.2400", true},
		{"20260923.1260", true},
		{"20261332.1200", true},
		{"19991231.2359", true},
		{"20260230.1200", false},
		{"20250229.1200", false},
		{" 20260923.1200", true},
		{"catalog-20260923.1200", true},
	} {
		add(revision.schema, "revision "+revision.value, "invalid catalog revision", set(root, "revision", revision.value))
	}

	for _, id := range []string{
		"a..b", "..", ".", "Alpha", "alpha-", "-alpha", ".alpha", "alpha.", "_alpha", "a/b", "a b", "\u00e9",
		"a\u202eb", strings.Repeat("a", 129),
	} {
		add(true, "id "+id, "must match", set(alpha, "id", id))
	}

	add(false, "duplicate ids", `duplicate schema id "alpha"`, set(beta, "id", "alpha"))
	add(false, "unsorted ids", `must be sorted by id ("beta" before "alpha")`, mutated(tb, func(d map[string]any) {
		s := schemasOf(d)
		s[0], s[1] = s[1], s[0]
	}))
	add(false, "ids sorted by locale instead of bytes", `("alpha.b" before "alpha-b")`, set(root, "schemas", []any{entryWithID("alpha.b"), entryWithID("alpha-b")}))

	for _, bad := range []struct {
		value, msg string
	}{
		{"sha256:" + strings.ToUpper(hexA), "lowercase hex"},
		{"sha512:" + hexA + hexA, "unsupported digest algorithm"},
		{digestA[:len(digestA)-1], "expected 64 hex digits"},
		{digestA + "0", "expected 64 hex digits"},
		{digestA + "00", "expected sha256:<64 lowercase hex digits>"},
		{hexA, "invalid digest"},
		{"SHA256:" + hexA, "malformed algorithm"},
	} {
		add(true, "artifact digest "+bad.value, bad.msg, set(alphaArtifact, "digest", bad.value))
	}

	add(true, "source digest upper-case", "provenance.sourceDigest", set(prov, "sourceDigest", "sha256:"+strings.ToUpper(hexA)))
	add(true, "dependency digest sha512", "provenance.dependencies.digest", set(dep, "digest", "sha512:"+hexA+hexA))

	add(true, "size zero", "artifact size 0 is outside 1-4194304", set(alphaArtifact, "size", 0))
	add(true, "size negative", "artifact size -1 is outside", set(alphaArtifact, "size", -1))
	add(true, "size negative zero", "artifact size 0 is outside", replace(`"size":512`, `"size":-0`))
	add(false, "size over the default limit", "artifact size 4194305 is outside 1-4194304", set(alphaArtifact, "size", 4194305))
	add(true, "size fraction", "must be an integer, got 1.5", set(alphaArtifact, "size", json.Number("1.5")))
	add(false, "size with zero fraction", "must be an integer, got 512.0", set(alphaArtifact, "size", json.Number("512.0")))
	add(false, "size exponent", "must be an integer, got 1e3", set(alphaArtifact, "size", json.Number("1e3")))
	add(false, "size beyond int64", "integer 99999999999999999999 is out of range", set(alphaArtifact, "size", json.Number("99999999999999999999")))
	add(false, "size with huge exponent", "must be an integer, got 1e400", set(alphaArtifact, "size", json.Number("1e400")))

	add(true, "image index media type", "is not application/vnd.oci.image.manifest.v1+json", set(alphaArtifact, "mediaType", "application/vnd.oci.image.index.v1+json"))
	add(true, "media type with parameters", "is not application/vnd.oci.image.manifest.v1+json", set(alphaArtifact, "mediaType", catalog.ManifestMediaType+"; charset=utf-8"))
	add(false, "same digest with different sizes", "is listed with sizes 512 and 700", set(func(d map[string]any) map[string]any { return artifactIn(d, 1) }, "digest", digestA))

	for _, bad := range []struct {
		value, msg string
		schema     bool
	}{
		{"https://user:secret@schemas.example.com/beta.json", "must not contain credentials", true},
		{"https://token@schemas.example.com/beta.json", "must not contain credentials", true},
		{"file:///etc/schema.json", "must use http or https", true},
		{"file:beta.json", "must use http or https", true},
		{"schemas/beta.json", "is not an absolute URI", true},
		{"//schemas.example.com/beta.json", "is not an absolute URI", true},
		{"ftp://schemas.example.com/beta.json", "must use http or https", true},
		{"https:///beta.json", "must name a host", true},
		{"https:beta.json", "must name a host", true},
		{"https://:443/beta.json", "must name a host", false},
		{"https://schemas.example.com/a b.json", "not allowed in a URI", true},
		{"https://schemas.example.com/a\\b.json", "not allowed in a URI", true},
		{"https://schemas.example.com/\"quoted\"", "not allowed in a URI", true},
		{"https://schemas.example.com/%zz", "is not an absolute URI", false},
		{"https://schemas.example.com/" + strings.Repeat("a", 4096), "URI of 1-4096 bytes", true},
	} {
		add(bad.schema, "provenance source "+bad.value, bad.msg, set(prov, "source", bad.value))
	}

	add(true, "dependency source with credentials", "must not contain credentials", set(dep, "source", "https://u:p@schemas.example.com/a.json"))
	add(true, "dependency source with local path", "must use http or https", set(dep, "source", "file:///home/a.json"))
	add(true, "dialect with credentials", "dialect must not contain credentials", set(beta, "dialect", "https://u:p@json-schema.org/draft/2020-12/schema"))
	add(true, "relative dialect", "is not an absolute URI", set(beta, "dialect", "draft-07"))
	add(true, "dialect with control character", "not allowed in a URI", set(beta, "dialect", "https://json-schema.org/\x00"))

	add(false, "unsorted dependencies", "must be sorted by source", mutated(tb, func(d map[string]any) {
		deps, _ := provenanceOf(d)["dependencies"].([]any)
		deps[0], deps[1] = deps[1], deps[0]
	}))
	add(true, "identical dependencies", "without duplicates", mutated(tb, func(d map[string]any) {
		deps, _ := provenanceOf(d)["dependencies"].([]any)
		deps[1] = deps[0]
	}))
	add(false, "dependencies with the same source", "without duplicates", mutated(tb, func(d map[string]any) {
		dependencyOf(d, 1)["source"] = dependencyOf(d, 0)["source"]
	}))
	add(true, "too many dependencies", "dependencies: has more than 1024 items", mutated(tb, func(d map[string]any) {
		deps := make([]any, 0, 1025)
		for i := range 1025 {
			deps = append(deps, map[string]any{"source": fmt.Sprintf("https://schemas.example.com/%04d.json", i), "digest": digestD})
		}

		provenanceOf(d)["dependencies"] = deps
	}))

	for _, text := range []struct {
		field  func(map[string]any) map[string]any
		key    string
		value  string
		msg    string
		schema bool
	}{
		{alpha, "name", "Al\tpha", "U+0009", true},
		{alpha, "name", "Al\npha", "U+000A", true},
		{alpha, "name", "\x1b[31mAlpha", "U+001B", true},
		{alpha, "name", "Alpha\x7f", "U+007F", true},
		{alpha, "name", "Alpha\u0085", "U+0085", true},
		{alpha, "name", "Alpha\u202egnp.exe", "U+202E", false},
		{alpha, "name", "Alpha\u2066", "U+2066", false},
		{alpha, "name", "Alpha\u2028Beta", "U+2028", true},
		{alpha, "name", "Alpha\u2029Beta", "U+2029", true},
		{alpha, "name", "   ", "no visible characters", true},
		{alpha, "name", "\u00a0\u3000", "no visible characters", false},
		{alpha, "name", "\u200b", "no visible characters", false},
		{alpha, "name", strings.Repeat("n", 513), "longer than 512 bytes", true},
		{alpha, "name", strings.Repeat("\u00e9", 257), "longer than 512 bytes", false},
		{beta, "description", "Line\r\nbreak", "U+000D", true},
		{beta, "description", "nul\x00", "U+0000", true},
		{beta, "description", "bidi\u200f", "U+200F", false},
		{beta, "description", strings.Repeat("d", 8193), "longer than 8192 bytes", true},
		{prov, "license", "MIT\nOR Apache-2.0", "U+000A", true},
		{prov, "license", "MIT\u2028OR Apache-2.0", "U+2028", true},
		{prov, "license", strings.Repeat("l", 257), "longer than 256 bytes", true},
	} {
		add(text.schema, fmt.Sprintf("%s %q", text.key, text.value), text.msg, set(text.field, text.key, text.value))
	}

	for _, pattern := range []struct {
		value, msg string
		schema     bool
	}{
		{"a\nb.json", "U+000A", true},
		{"a\tb.json", "U+0009", true},
		{"a\u2028b.json", "U+2028", true},
		{"a\u2029b.json", "U+2029", true},
		{"[", "syntax error in pattern", false},
		{"{a,b", "syntax error in pattern", false},
		{"a\\", "syntax error in pattern", false},
		{"config/", "directory patterns are not supported", true},
		{"!", "empty", true},
		{"!./", "empty", true},
		{"./", "empty", true},
		{"/", "empty", true},
		{strings.Repeat("p", 1025), "longer than 1024 bytes", true},
		{strings.Repeat("\u00e9", 600), "longer than 1024 bytes", false},
	} {
		add(pattern.schema, fmt.Sprintf("fileMatch %q", pattern.value), pattern.msg, set(beta, "fileMatch", []any{"beta.json", pattern.value}))
	}

	add(true, "duplicate fileMatch pattern", `"beta.json" is listed twice`, set(beta, "fileMatch", []any{"beta.json", "beta.json"}))

	patterns := make([]any, 0, 257)
	for i := range 257 {
		patterns = append(patterns, fmt.Sprintf("p%03d.json", i))
	}

	add(true, "too many fileMatch patterns", "fileMatch: has more than 256 items", set(beta, "fileMatch", patterns))

	cases = append(cases,
		invalidCase{name: "entry count over a lowered limit", msg: "schemas: has more than 1 items", doc: []byte(canonical), limits: catalog.Limits{MaxEntries: 1}},
		invalidCase{name: "size over a lowered limit", msg: "artifact size 700 is outside 1-699", doc: []byte(canonical), limits: catalog.Limits{MaxManifestBytes: 699}},
	)

	return cases
}

func TestParseInvalid(t *testing.T) {
	t.Parallel()

	schema := compileCatalogSchema(t)

	for _, tc := range invalidCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c, err := catalog.Parse(tc.doc, tc.limits)
			assertFault(t, c, err, fault.Integrity, tc.msg)

			if tc.schema {
				if err := validateAgainstSchema(schema, tc.doc); err == nil {
					t.Fatal("api/catalog.schema.json accepts the document")
				}
			}
		})
	}
}

func TestParseEntryLimitDefault(t *testing.T) {
	t.Parallel()

	if _, err := catalog.Parse(manyEntries(20000), catalog.Limits{}); err != nil {
		t.Fatalf("Parse() of 20000 entries with default limits: %v", err)
	}

	c, err := catalog.Parse(manyEntries(20001), catalog.Limits{})
	assertFault(t, c, err, fault.Integrity, "schemas: has more than 20000 items")
}

func TestParseTruncatesHostileText(t *testing.T) {
	t.Parallel()

	for _, filler := range []string{"A", "\u009b", "\u202e", "\""} {
		huge := strings.Repeat(filler, 1<<20)

		cases := []struct {
			sentinel error
			name     string
			doc      []byte
		}{
			{name: "id", doc: mutated(t, func(d map[string]any) { entryOf(d, 0)["id"] = huge })},
			{name: "unknown member", doc: mutated(t, func(d map[string]any) { entryOf(d, 0)[huge] = 1 })},
			{name: "duplicate member", doc: []byte(`{"formatVersion":2,` + string(encode(t, huge)) + `:1,` + string(encode(t, huge)) + `:2}`), sentinel: jsonutil.ErrDuplicateKey},
			{name: "string formatVersion", doc: []byte(`{"formatVersion":` + string(encode(t, huge)) + `}`)},
			{name: "numeric formatVersion", doc: []byte(`{"formatVersion":` + strings.Repeat("9", 1<<20) + `}`)},
			{name: "revision", doc: mutated(t, func(d map[string]any) { d["revision"] = huge }), sentinel: calver.ErrInvalid},
			{name: "mediaType", doc: mutated(t, func(d map[string]any) { artifactIn(d, 0)["mediaType"] = huge })},
			{name: "artifact digest", doc: mutated(t, func(d map[string]any) { artifactIn(d, 0)["digest"] = huge }), sentinel: digest.ErrInvalid},
			{name: "source digest", doc: mutated(t, func(d map[string]any) { provenanceOf(d)["sourceDigest"] = huge }), sentinel: digest.ErrInvalid},
			{name: "dependency digest", doc: mutated(t, func(d map[string]any) { dependencyOf(d, 0)["digest"] = huge }), sentinel: digest.ErrInvalid},
			{name: "long digest of another algorithm", doc: mutated(t, func(d map[string]any) { artifactIn(d, 0)["digest"] = "sha512:" + huge }), sentinel: digest.ErrUnsupportedAlgorithm},
			{name: "source", doc: mutated(t, func(d map[string]any) { provenanceOf(d)["source"] = "https://x/" + huge })},
			{name: "name", doc: mutated(t, func(d map[string]any) { entryOf(d, 0)["name"] = huge })},
			{name: "fileMatch", doc: mutated(t, func(d map[string]any) { entryOf(d, 1)["fileMatch"] = []any{huge} })},
		}

		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s %+q", tc.name, filler), func(t *testing.T) {
				t.Parallel()

				_, err := catalog.Parse(tc.doc, catalog.Limits{})
				if err == nil || len(err.Error()) > 1024 {
					t.Fatalf("error %d bytes long, want a short error", len(fmt.Sprint(err)))
				}

				assertSafeMessage(t, err)

				if tc.sentinel != nil && !errors.Is(err, tc.sentinel) {
					t.Fatalf("error %v does not wrap %v", err, tc.sentinel)
				}
			})
		}
	}
}

// assertSafeMessage fails when an error message could flood or drive a
// terminal: every rune must be printable, so control, bidirectional and
// line-breaking characters from the document only ever appear escaped.
func assertSafeMessage(tb testing.TB, err error) {
	tb.Helper()

	for _, r := range err.Error() {
		if !strconv.IsPrint(r) {
			tb.Fatalf("error message contains the raw character %U: %+q", r, err.Error())
		}
	}
}

func assertFault(tb testing.TB, c *catalog.Catalog, err error, kind fault.Kind, msg string) {
	tb.Helper()

	if err == nil {
		tb.Fatalf("Parse() accepted the document: %+v", c)
	}

	if c != nil {
		tb.Fatalf("Parse() returned a catalog together with error %v", err)
	}

	if got := fault.KindOf(err); got != kind {
		tb.Fatalf("fault kind = %s, want %s (error: %v)", got, kind, err)
	}

	if !strings.Contains(err.Error(), msg) {
		tb.Fatalf("error %q does not mention %q", err, msg)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		edit   func(c *catalog.Catalog)
		name   string
		msg    string
		limits catalog.Limits
		kind   fault.Kind
		ok     bool
	}{
		{name: "zero limits select the defaults", edit: func(*catalog.Catalog) {}, ok: true},
		{name: "nil schemas", edit: func(c *catalog.Catalog) { c.Schemas = nil }, ok: true},
		{name: "empty optional slices", edit: func(c *catalog.Catalog) {
			c.Schemas[0].FileMatch = []string{}
			c.Schemas[1].Provenance.Dependencies = []catalog.Dependency{}
		}, ok: true},
		{name: "newer format", edit: func(c *catalog.Catalog) { c.FormatVersion = 3 }, kind: fault.Usage, msg: "unsupported catalog formatVersion 3"},
		{name: "older format", edit: func(c *catalog.Catalog) { c.FormatVersion = 1 }, kind: fault.Usage, msg: "unsupported catalog formatVersion 1"},
		{name: "unset format", edit: func(c *catalog.Catalog) { c.FormatVersion = 0 }, kind: fault.Integrity, msg: "formatVersion 0"},
		{name: "negative format", edit: func(c *catalog.Catalog) { c.FormatVersion = -1 }, kind: fault.Integrity, msg: "formatVersion -1"},
		{name: "bad revision", edit: func(c *catalog.Catalog) { c.Revision = "v1" }, kind: fault.Integrity, msg: "invalid catalog revision"},
		{name: "invalid UTF-8 name", edit: func(c *catalog.Catalog) { c.Schemas[0].Name = "Al\xffpha" }, kind: fault.Integrity, msg: "name is not valid UTF-8"},
		{name: "invalid UTF-8 description", edit: func(c *catalog.Catalog) { c.Schemas[1].Description = "\xc3" }, kind: fault.Integrity, msg: "description is not valid UTF-8"},
		{name: "empty provenance", edit: func(c *catalog.Catalog) { c.Schemas[0].Provenance = &catalog.Provenance{} }, kind: fault.Integrity, msg: "provenance.source must be a URI"},
		{name: "unsorted entries", edit: func(c *catalog.Catalog) { slices.Reverse(c.Schemas) }, kind: fault.Integrity, msg: "must be sorted by id"},
		{name: "lowered entry limit", edit: func(*catalog.Catalog) {}, limits: catalog.Limits{MaxEntries: 1}, kind: fault.Integrity, msg: "catalog has 2 entries, limit is 1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := validCatalog()
			tc.edit(c)

			err := c.Validate(tc.limits)

			switch {
			case tc.ok && err != nil:
				t.Fatalf("Validate() error = %v", err)
			case tc.ok:
			case err == nil:
				t.Fatal("Validate() accepted the catalog")
			case fault.KindOf(err) != tc.kind || !strings.Contains(err.Error(), tc.msg):
				t.Fatalf("Validate() error = %v (%s), want %s mentioning %q", err, fault.KindOf(err), tc.kind, tc.msg)
			}
		})
	}
}

func TestDescriptorValidate(t *testing.T) {
	t.Parallel()

	valid := catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: digestA, Size: 512}

	cases := []struct {
		name  string
		msg   string
		desc  catalog.Descriptor
		limit int64
	}{
		{name: "valid", desc: valid, limit: 512},
		{name: "zero limit selects the default", desc: catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: digestA, Size: 4 << 20}},
		{name: "over the default", desc: catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: digestA, Size: 4<<20 + 1}, msg: "outside 1-4194304"},
		{name: "over the limit", desc: valid, limit: 511, msg: "outside 1-511"},
		{name: "zero size", desc: catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: digestA}, msg: "size 0"},
		{name: "wrong media type", desc: catalog.Descriptor{MediaType: "application/json", Digest: digestA, Size: 1}, msg: `mediaType "application/json"`},
		{name: "bad digest", desc: catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: "sha256:abc", Size: 1}, msg: "artifact digest"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.desc.Validate(tc.limit)

			switch {
			case tc.msg == "" && err != nil:
				t.Fatalf("Validate() error = %v", err)
			case tc.msg != "" && (err == nil || !strings.Contains(err.Error(), tc.msg)):
				t.Fatalf("Validate() error = %v, want one mentioning %q", err, tc.msg)
			}
		})
	}
}

func TestMarshal(t *testing.T) {
	t.Parallel()

	t.Run("canonical bytes", func(t *testing.T) {
		t.Parallel()

		got := mustMarshal(t, validCatalog())
		if string(got) != validCanonical() {
			t.Fatalf("Marshal() =\n%s\nwant\n%s", got, validCanonical())
		}
	})

	t.Run("entries as they appear in the catalog", func(t *testing.T) {
		t.Parallel()

		c := validCatalog()
		c.Schemas[1].Description = "<b>&amp;</b>"

		parts := make([]string, 0, len(c.Schemas))

		for i := range c.Schemas {
			entry, err := catalog.MarshalEntry(&c.Schemas[i])
			if err != nil {
				t.Fatal(err)
			}

			parts = append(parts, string(entry))
		}

		want := `{"formatVersion":2,"revision":"` + testRevision + `","schemas":[` + strings.Join(parts, ",") + `]}`
		if got := string(mustMarshal(t, c)); got != want {
			t.Fatalf("Marshal() =\n%s\nMarshalEntry pieces =\n%s", got, want)
		}

		if !strings.Contains(want, `"description":"<b>&amp;</b>"`) {
			t.Errorf("MarshalEntry escapes HTML: %s", parts[1])
		}
	})

	t.Run("round trip", func(t *testing.T) {
		t.Parallel()

		want := validCatalog()

		got, err := catalog.Parse(mustMarshal(t, want), catalog.Limits{})
		if err != nil {
			t.Fatalf("Parse(Marshal()) error = %v", err)
		}

		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Parse(Marshal()) =\n%#v\nwant\n%#v", got, want)
		}

		if again := mustMarshal(t, got); string(again) != validCanonical() {
			t.Fatalf("Marshal() is not idempotent:\n%s", again)
		}
	})

	t.Run("sorts entries without mutating the input", func(t *testing.T) {
		t.Parallel()

		c := validCatalog()
		slices.Reverse(c.Schemas)

		if got := mustMarshal(t, c); string(got) != validCanonical() {
			t.Fatalf("Marshal() =\n%s", got)
		}

		if c.Schemas[0].ID != "beta" {
			t.Fatalf("Marshal() reordered the caller's entries: %v", c.IDs())
		}
	})

	t.Run("members sorted at every level", func(t *testing.T) {
		t.Parallel()

		assertSortedMembers(t, mustMarshal(t, validCatalog()))
	})

	t.Run("no insignificant whitespace", func(t *testing.T) {
		t.Parallel()

		got := mustMarshal(t, validCatalog())

		var compact bytes.Buffer
		if err := json.Compact(&compact, got); err != nil {
			t.Fatal(err)
		}

		if !bytes.Equal(compact.Bytes(), got) || bytes.HasSuffix(got, []byte("\n")) {
			t.Fatalf("Marshal() output is not compact: %q", got)
		}
	})

	t.Run("no HTML escaping", func(t *testing.T) {
		t.Parallel()

		c := validCatalog()
		c.Schemas[1].Description = "a < b && c > d"

		got := mustMarshal(t, c)
		if !bytes.Contains(got, []byte(`"description":"a < b && c > d"`)) {
			t.Fatalf("Marshal() escaped HTML characters: %s", got)
		}
	})

	t.Run("nil schemas encode as an empty list", func(t *testing.T) {
		t.Parallel()

		c := &catalog.Catalog{FormatVersion: 2, Revision: testRevision}

		got := mustMarshal(t, c)
		if want := `{"formatVersion":2,"revision":"20260923.1200","schemas":[]}`; string(got) != want {
			t.Fatalf("Marshal() = %s, want %s", got, want)
		}

		if _, err := catalog.Parse(got, catalog.Limits{}); err != nil {
			t.Fatalf("Parse(Marshal()) error = %v", err)
		}
	})

	t.Run("empty optional members are omitted", func(t *testing.T) {
		t.Parallel()

		c := validCatalog()
		c.Schemas[1].FileMatch = []string{}
		c.Schemas[1].Provenance.Dependencies = []catalog.Dependency{}

		got := string(mustMarshal(t, c))
		if strings.Contains(got, "fileMatch") || strings.Contains(got, "dependencies") {
			t.Fatalf("Marshal() kept empty members: %s", got)
		}
	})

	t.Run("large sizes keep their exact digits", func(t *testing.T) {
		t.Parallel()

		c := validCatalog()
		c.Schemas[0].Artifact.Size = 1<<53 + 1

		if got := mustMarshal(t, c); !bytes.Contains(got, []byte(`"size":9007199254740993`)) {
			t.Fatalf("Marshal() changed the size: %s", got)
		}
	})
}

func mustMarshal(tb testing.TB, c *catalog.Catalog) []byte {
	tb.Helper()

	data, err := catalog.Marshal(c)
	if err != nil {
		tb.Fatalf("Marshal() error = %v", err)
	}

	return data
}

// assertSortedMembers fails unless every object in data lists its members in
// strictly ascending byte order.
func assertSortedMembers(tb testing.TB, data []byte) {
	tb.Helper()

	type frame struct {
		last     string
		isObject bool
		wantKey  bool
		hasLast  bool
	}

	dec := json.NewDecoder(bytes.NewReader(data))

	var stack []*frame

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}

		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}

		if key, ok := tok.(string); ok && top != nil && top.isObject && top.wantKey {
			if top.hasLast && top.last >= key {
				tb.Fatalf("member %q follows %q", key, top.last)
			}

			top.last, top.hasLast, top.wantKey = key, true, false

			continue
		}

		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{', '[':
				if top != nil && top.isObject {
					top.wantKey = true
				}

				stack = append(stack, &frame{isObject: delim == '{', wantKey: delim == '{'})
			case '}', ']':
				stack = stack[:len(stack)-1]
			}

			continue
		}

		if top != nil && top.isObject {
			top.wantKey = true
		}
	}
}

func TestLookup(t *testing.T) {
	t.Parallel()

	c := &catalog.Catalog{FormatVersion: 2, Revision: testRevision}
	for _, id := range []string{"a", "b", "c", "d"} {
		c.Schemas = append(c.Schemas, catalog.Entry{ID: id, Name: id})
	}

	for i, id := range []string{"a", "b", "c", "d"} {
		entry, ok := c.Lookup(id)
		if !ok || entry != &c.Schemas[i] {
			t.Fatalf("Lookup(%q) = %p, %v; want the catalog's own entry %p", id, entry, ok, &c.Schemas[i])
		}
	}

	for _, id := range []string{"", "0", "bb", "e", "A"} {
		if entry, ok := c.Lookup(id); ok || entry != nil {
			t.Fatalf("Lookup(%q) = %v, %v; want no entry", id, entry, ok)
		}
	}

	if entry, ok := (&catalog.Catalog{}).Lookup("a"); ok || entry != nil {
		t.Fatalf("Lookup() on an empty catalog = %v, %v", entry, ok)
	}
}

func TestIDs(t *testing.T) {
	t.Parallel()

	if got := validCatalog().IDs(); !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Fatalf("IDs() = %v", got)
	}

	if got := (&catalog.Catalog{}).IDs(); got == nil || len(got) != 0 {
		t.Fatalf("IDs() on an empty catalog = %#v, want an empty non-nil slice", got)
	}
}

func TestRules(t *testing.T) {
	t.Parallel()

	c := validCatalog()
	c.Schemas = append(c.Schemas, catalog.Entry{ID: "gamma", Name: "Gamma", FileMatch: []string{"*.gamma.json"}})

	rules := c.Rules()

	want := []match.Rule{
		{SchemaID: "beta", Patterns: []string{"beta.json", "**/.beta/*.json", "!**/node_modules/**"}},
		{SchemaID: "gamma", Patterns: []string{"*.gamma.json"}},
	}
	if !reflect.DeepEqual(rules, want) {
		t.Fatalf("Rules() = %#v, want %#v", rules, want)
	}

	rules[0].Patterns[0] = "changed"
	if c.Schemas[1].FileMatch[0] != "beta.json" {
		t.Fatal("Rules() shares pattern slices with the catalog")
	}

	set, err := match.NewSet(match.OriginCatalog, c.Rules())
	if err != nil {
		t.Fatalf("NewSet(Rules()) error = %v", err)
	}

	for path, id := range map[string]string{"beta.json": "beta", "x/.beta/y.json": "beta", "a/b.gamma.json": "gamma"} {
		res, err := match.ResolveFirst(path, set)
		if err != nil || res.SchemaID != id || res.Origin != match.OriginCatalog {
			t.Fatalf("Resolve(%q) = %+v, %v; want %s from the catalog", path, res, err, id)
		}
	}

	if _, err := match.ResolveFirst("node_modules/.beta/y.json", set); !errors.Is(err, match.ErrNoMatch) {
		t.Fatalf("negative pattern ignored: %v", err)
	}

	if got := (&catalog.Catalog{}).Rules(); got == nil || len(got) != 0 {
		t.Fatalf("Rules() on an empty catalog = %#v", got)
	}
}

func TestList(t *testing.T) {
	t.Parallel()

	items := validCatalog().List()

	want := []catalog.ListItem{
		{ID: "alpha", Name: "Alpha", Digest: digestA},
		{ID: "beta", Name: "Beta schema", Description: "Line one.\nLine two with\ttab.", Dialect: "https://json-schema.org/draft/2020-12/schema", Digest: digestB},
	}
	if !reflect.DeepEqual(items, want) {
		t.Fatalf("List() = %#v, want %#v", items, want)
	}

	data, err := json.Marshal(items[0])
	if err != nil {
		t.Fatal(err)
	}

	if want := `{"id":"alpha","name":"Alpha","digest":"` + digestA + `"}`; string(data) != want {
		t.Fatalf("list item JSON = %s, want %s", data, want)
	}

	if got := (&catalog.Catalog{}).List(); got == nil || len(got) != 0 {
		t.Fatalf("List() on an empty catalog = %#v", got)
	}
}

func TestPatterns(t *testing.T) {
	t.Parallel()

	c := validCatalog()
	items := c.Patterns()

	want := []catalog.PatternItem{{
		ID:        "beta",
		FileMatch: []string{"beta.json", "**/.beta/*.json", "!**/node_modules/**"},
		Artifact:  catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: digestB, Size: 700},
	}}
	if !reflect.DeepEqual(items, want) {
		t.Fatalf("Patterns() = %#v, want %#v", items, want)
	}

	items[0].FileMatch[0] = "changed"
	if c.Schemas[1].FileMatch[0] != "beta.json" {
		t.Fatal("Patterns() shares pattern slices with the catalog")
	}

	data, err := json.Marshal(c.Patterns())
	if err != nil {
		t.Fatal(err)
	}

	wantJSON := `[{"id":"beta","fileMatch":["beta.json","**/.beta/*.json","!**/node_modules/**"],` +
		`"artifact":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + digestB + `","size":700}}]`
	if string(data) != wantJSON {
		t.Fatalf("patterns JSON = %s, want %s", data, wantJSON)
	}

	if got := (&catalog.Catalog{}).Patterns(); got == nil || len(got) != 0 {
		t.Fatalf("Patterns() on an empty catalog = %#v", got)
	}
}

func TestArtifacts(t *testing.T) {
	entry := func(id, dgst string, size int64) catalog.Entry {
		return catalog.Entry{ID: id, Name: id, Artifact: catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: dgst, Size: size}}
	}

	c := &catalog.Catalog{FormatVersion: catalog.FormatVersion, Revision: testRevision, Schemas: []catalog.Entry{
		entry("a", digestB, 700), entry("b", digestA, 512), entry("c", digestB, 700),
	}}

	got := c.Artifacts()

	first, second := digestA, digestB
	if first > second {
		first, second = second, first
	}

	if len(got) != 2 || got[0].Digest.String() != first || got[1].Digest.String() != second {
		t.Fatalf("Artifacts() = %+v, want the two distinct digests in ascending order", got)
	}

	for _, d := range got {
		if d.MediaType != catalog.ManifestMediaType || (d.Digest.String() == digestA && d.Size != 512) || (d.Digest.String() == digestB && d.Size != 700) {
			t.Errorf("descriptor %+v", d)
		}
	}

	if n := len((&catalog.Catalog{}).Artifacts()); n != 0 {
		t.Errorf("an empty catalog has %d artifacts", n)
	}
}
