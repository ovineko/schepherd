package prepare

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/bundle"
	"github.com/ovineko/schepherd/internal/publisher/policy"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

const preparedSchemaURL = "https://raw.githubusercontent.com/ovineko/schepherd/main/api/prepared.schema.json"

var documentsDir = filepath.Join(fixturesDir, "documents")

func compilePreparedSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()

	f, err := os.Open(schemaFile)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = f.Close() }()

	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatal(err)
	}

	c := jsonschema.NewCompiler()
	if err := c.AddResource(preparedSchemaURL, doc); err != nil {
		t.Fatal(err)
	}

	schema, err := c.Compile(preparedSchemaURL)
	if err != nil {
		t.Fatalf("api/prepared.schema.json does not compile: %v", err)
	}

	return schema
}

func validateAgainstSchema(schema *jsonschema.Schema, data []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return err
	}

	return schema.Validate(inst)
}

// parseFixture applies the publisher's rules to a fixture whose member order
// and indentation a repository formatter may have changed: the document is
// decoded strictly, must survive a canonical round trip unchanged in
// meaning, and the canonical bytes must parse.
func parseFixture(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var doc Document
	if err := dec.Decode(&doc); err != nil {
		return err
	}

	canonical, err := Encode(&doc)
	if err != nil {
		return err
	}

	var before, after any
	if err := json.Unmarshal(data, &before); err != nil {
		return err
	}

	if err := json.Unmarshal(canonical, &after); err != nil {
		return err
	}

	if !reflect.DeepEqual(before, after) {
		return fault.New(fault.Integrity, "the document has no canonical form")
	}

	_, err = ParseDocument(canonical)

	return err
}

func TestPreparedSchemaDocument(t *testing.T) {
	data, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		ID     string `json:"$id"`
		Schema string `json:"$schema"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}

	if doc.ID != preparedSchemaURL || doc.Schema != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("$id = %q, $schema = %q", doc.ID, doc.Schema)
	}

	var recipe struct {
		Properties struct {
			Recipe struct {
				Const string `json:"const"`
			} `json:"recipe"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &recipe); err != nil {
		t.Fatal(err)
	}

	if recipe.Properties.Recipe.Const != Recipe {
		t.Errorf("api/prepared.schema.json pins recipe %q, this build writes %q", recipe.Properties.Recipe.Const, Recipe)
	}

	if want := "schepherd-prepare/2+sourcemeta-jsonschema-" + bundle.PinnedVersion; Recipe != want {
		t.Errorf("Recipe = %q, want %q: a bundler upgrade must change the recipe", Recipe, want)
	}

	compilePreparedSchema(t)
}

func TestPreparedSchemaAgreesWithParser(t *testing.T) {
	schema := compilePreparedSchema(t)

	for _, kind := range []string{"valid", "invalid"} {
		files, err := filepath.Glob(filepath.Join(documentsDir, kind, "*.json"))
		if err != nil {
			t.Fatal(err)
		}

		if len(files) == 0 {
			t.Fatalf("no %s fixtures", kind)
		}

		for _, file := range files {
			t.Run(kind+"/"+filepath.Base(file), func(t *testing.T) {
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}

				schemaErr := validateAgainstSchema(schema, data)
				parseErr := parseFixture(data)

				if kind == "valid" && (schemaErr != nil || parseErr != nil) {
					t.Fatalf("valid fixture rejected: schema %v, parser %v", schemaErr, parseErr)
				}

				if kind == "invalid" && (schemaErr == nil || parseErr == nil) {
					t.Fatalf("invalid fixture accepted: schema %v, parser %v", schemaErr, parseErr)
				}
			})
		}
	}
}

// TestLicenseGateAgreesWithSchema checks that the parser and
// api/prepared.schema.json refuse the same non-asserted licenses.
func TestLicenseGateAgreesWithSchema(t *testing.T) {
	schema := compilePreparedSchema(t)

	for license, asserted := range map[string]bool{
		"MIT": true, "Apache-2.0 AND MIT": true, "(MIT OR Apache-2.0) AND BSD-3-Clause": true, "LicenseRef-NOASSERTION": true,
		"Unlicense": true, "NOASSERTION": false, "noassertion": false, "MIT AND NONE": false, "(UNKNOWN)": false,
		"UNLICENSED": false, "Other": false, "MIT OR NOASSERTION": false,
	} {
		set := sampleSet()
		set.Document.Entries[0].Provenance.License = license

		data, err := Encode(&set.Document)
		if err != nil {
			t.Fatal(err)
		}

		_, parseErr := ParseDocument(data)
		schemaErr := validateAgainstSchema(schema, data)

		if asserted && (parseErr != nil || schemaErr != nil) {
			t.Errorf("%q refused: parser %v, schema %v", license, parseErr, schemaErr)
		}

		if !asserted && (fault.KindOf(parseErr) != fault.Integrity || schemaErr == nil) {
			t.Errorf("%q accepted: parser %v, schema %v", license, parseErr, schemaErr)
		}
	}
}

func TestParseDocumentVersionsAreUsageErrors(t *testing.T) {
	for _, doc := range []string{
		`{"formatVersion":2,"recipe":"` + Recipe + `","source":{"kind":"local","name":"x"},"entries":[]}`,
		`{"formatVersion":1,"recipe":"schepherd-prepare/9","source":{"kind":"local","name":"x"},"entries":[]}`,
		`{"formatVersion":1,"recipe":"schepherd-prepare/1","source":{"kind":"local","name":"x"},"entries":[]}`,
		`{"formatVersion":1,"recipe":"schepherd-prepare/2+sourcemeta-jsonschema-0.0.1","source":{"kind":"local","name":"x"},"entries":[]}`,
		`{"recipe":"` + Recipe + `"}`,
	} {
		_, err := ParseDocument([]byte(doc))
		wantKind(t, err, fault.Usage)
	}

	_, err := ParseDocument([]byte(`{"formatVersion":1,"recipe":"` + Recipe + `","source":{"kind":"local","name":"x"},"entries":[]}`))
	wantKind(t, err, fault.Integrity)

	_, err = ParseDocument([]byte(`{"formatVersion":1,"formatVersion":1}`))
	wantKind(t, err, fault.Integrity)
}

const gammaURL = "https://raw.githubusercontent.com/owner/repo/main/gamma.json"

// sampleSet builds a set in memory, without the bundler: two prepared
// entries, one reused entry, a hold and an exclusion.
func sampleSet() *Set {
	notice := []byte("Sample notice.\n")
	alpha := []byte(`{"type":"object"}`)
	beta := []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"string"}`)

	return &Set{
		Document: Document{
			FormatVersion: FormatVersion, Recipe: Recipe, Source: Source{Kind: KindLocal, Name: "sample"},
			Entries: []Entry{
				{
					ID: "alpha", Name: "Alpha", FileMatch: []string{"alpha.json"}, Schema: SchemaPath("alpha"),
					ContentDigest: sha(string(alpha)), Notice: NoticePathFor(notice),
					Provenance: catalog.Provenance{Source: "https://schemas.example/alpha.json", SourceDigest: sha("a"), License: "MIT"},
				},
				{
					ID: "beta", Name: "Beta", Description: "B", Dialect: "https://json-schema.org/draft/2020-12/schema",
					FileMatch: []string{}, Schema: SchemaPath("beta"), ContentDigest: sha(string(beta)),
					Provenance: catalog.Provenance{Source: "https://schemas.example/beta.json", SourceDigest: sha("b"), License: "MIT"},
					License:    state.LicenseDecision{Rules: []string{"schemas"}},
				},
				{
					ID: "gamma", Name: "Gamma", FileMatch: []string{}, ContentDigest: sha("gamma content"), NoticeDigest: sha("gamma notice"),
					Reused:     &catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: sha("gamma manifest"), Size: 400},
					Provenance: catalog.Provenance{Source: gammaURL, SourceDigest: sha("g"), License: "MIT"},
					License: state.LicenseDecision{Detections: []policy.Detection{{
						URL: gammaURL, Source: "github:owner/repo@" + detectedCommit, License: "MIT", LicenseFile: "LICENSE",
						LicenseDigest: sha(mitLicense), Verdict: policy.Allow,
					}}},
				},
			},
			Held:     []Hold{{ID: "delta", Reason: state.HeldFetchFailed}},
			Excluded: []Exclusion{{ID: "epsilon", Rule: "takedown"}},
		},
		Schemas: map[string][]byte{"alpha": alpha, "beta": beta},
		Notices: map[string][]byte{NoticePathFor(notice): notice},
	}
}

func TestWriteSetAndLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	set := sampleSet()

	if err := WriteSet(dir, set, map[string][]byte{"extra/file.txt": []byte("x")}); err != nil {
		t.Fatal(err)
	}

	loaded := mustLoad(t, dir)
	if !reflect.DeepEqual(loaded, set) {
		t.Errorf("loaded set differs:\n%+v\n%+v", loaded, set)
	}

	schema := compilePreparedSchema(t)

	data, err := os.ReadFile(filepath.Join(dir, PreparedFile))
	if err != nil {
		t.Fatal(err)
	}

	if err := validateAgainstSchema(schema, data); err != nil {
		t.Errorf("api/prepared.schema.json rejects written prepared.json: %v", err)
	}

	if err := WriteSet(dir, set, nil); fault.KindOf(err) != fault.Usage {
		t.Errorf("WriteSet into a non-empty directory: %v", err)
	}

	empty := t.TempDir()
	if err := WriteSet(empty, set, nil); err != nil {
		t.Errorf("WriteSet into an empty directory: %v", err)
	}

	parent := t.TempDir()
	if err := WriteSet(filepath.Join(parent, "x"), &Set{Document: set.Document}, nil); fault.KindOf(err) != fault.Internal {
		t.Errorf("WriteSet without schema bytes: %v", err)
	}

	if entries, _ := os.ReadDir(parent); len(entries) != 0 {
		t.Errorf("a failed WriteSet left %v behind", entries)
	}
}

func TestLoadRejectsTamperedSets(t *testing.T) {
	cases := []struct {
		tamper func(t *testing.T, dir string)
		name   string
		want   string
		kind   fault.Kind
	}{
		{
			name: "schema digest",
			tamper: func(t *testing.T, dir string) {
				t.Helper()
				writeFiles(t, dir, map[string]string{"schemas/alpha.json": `{"type":"array"}`})
			},
			kind: fault.Integrity, want: "has digest",
		},
		{
			name: "schema not compact",
			tamper: func(t *testing.T, dir string) {
				t.Helper()

				set := sampleSet()
				set.Schemas["alpha"] = []byte(`{ "type": "object" }`)
				set.Document.Entries[0].ContentDigest = sha(`{ "type": "object" }`)
				rewrite(t, dir, set)
			},
			kind: fault.Integrity, want: "not compact",
		},
		{
			name: "missing schema",
			tamper: func(t *testing.T, dir string) {
				t.Helper()

				if err := os.Remove(filepath.Join(dir, "schemas", "beta.json")); err != nil {
					t.Fatal(err)
				}
			},
			kind: fault.Usage, want: "schemas/beta.json",
		},
		{
			name: "notice digest",
			tamper: func(t *testing.T, dir string) {
				t.Helper()
				writeFiles(t, dir, map[string]string{NoticePathFor([]byte("Sample notice.\n")): "Changed.\n"})
			},
			kind: fault.Integrity, want: "notice",
		},
		{
			name: "prepared.json not canonical",
			tamper: func(t *testing.T, dir string) {
				t.Helper()

				data, err := os.ReadFile(filepath.Join(dir, PreparedFile))
				if err != nil {
					t.Fatal(err)
				}

				writeFiles(t, dir, map[string]string{PreparedFile: strings.ReplaceAll(string(data), "  ", "\t")})
			},
			kind: fault.Integrity, want: "canonical",
		},
		{
			name: "schema is a symlink",
			tamper: func(t *testing.T, dir string) {
				t.Helper()

				target := filepath.Join(dir, "schemas", "beta.json")
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}

				if err := os.Symlink(filepath.Join(dir, "schemas", "alpha.json"), target); err != nil {
					t.Fatal(err)
				}
			},
			kind: fault.Integrity, want: "beta.json",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "set")
			if err := WriteSet(dir, sampleSet(), nil); err != nil {
				t.Fatal(err)
			}

			tc.tamper(t, dir)

			_, err := Load(dir)
			wantKind(t, err, tc.kind)
			mustContain(t, err.Error(), tc.want)
		})
	}

	_, err := Load(filepath.Join(t.TempDir(), "missing"))
	wantKind(t, err, fault.Usage)
}

func rewrite(t *testing.T, dir string, set *Set) {
	t.Helper()

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	if err := WriteSet(dir, set, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCheckMetadata(t *testing.T) {
	valid := &catalog.Provenance{Source: "https://schemas.example/a.json"}

	if err := CheckMetadata("Name", "Line\nwith\ttab", "http://json-schema.org/draft-07/schema#", []string{"a.json"}, valid); err != nil {
		t.Fatal(err)
	}

	for name, err := range map[string]error{
		"empty name":        CheckMetadata(" ", "", "", nil, nil),
		"control in name":   CheckMetadata("a\x01", "", "", nil, nil),
		"bidi in text":      CheckMetadata("a\u202eb", "", "", nil, nil),
		"long description":  CheckMetadata("a", strings.Repeat("x", 8193), "", nil, nil),
		"bad dialect":       CheckMetadata("a", "", "not a uri", nil, nil),
		"extglob":           CheckMetadata("a", "", "", []string{"!(x).yml"}, nil),
		"duplicate pattern": CheckMetadata("a", "", "", []string{"a", "a"}, nil),
		"source scheme":     CheckMetadata("a", "", "", nil, &catalog.Provenance{Source: "ftp://x/y"}),
	} {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func FuzzParseDocument(f *testing.F) {
	set := sampleSet()

	seed, err := Encode(&set.Document)
	if err != nil {
		f.Fatal(err)
	}

	f.Add(seed)
	f.Add([]byte(`{"formatVersion":1,"recipe":"` + Recipe + `","source":{"kind":"local","name":"x"},"entries":[]}` + "\n"))
	f.Add([]byte(`{"formatVersion":2}`))
	f.Add([]byte(`[]`))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 {
			return
		}

		doc, err := ParseDocument(data)
		if err != nil {
			return
		}

		again, err := Encode(doc)
		if err != nil {
			t.Fatal(err)
		}

		if !bytes.Equal(again, data) {
			t.Fatalf("accepted a non-canonical document:\n%s", data)
		}
	})
}

// TestDocumentListRules covers the rules for held and excluded IDs that
// api/prepared.schema.json cannot express.
func TestDocumentListRules(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(d *Document)
		want   string
	}{
		"held and an entry": {func(d *Document) { d.Held = append(d.Held, Hold{ID: "gamma", Reason: state.HeldFetchFailed}) }, "listed in both entries and held"},
		"held and excluded": {func(d *Document) {
			d.Excluded = append(d.Excluded, Exclusion{ID: "zz"})
			d.Held = append(d.Held, Hold{ID: "zz", Reason: state.HeldFetchFailed})
		}, "listed in both held and excluded"},
		"unsorted held": {func(d *Document) {
			d.Held = []Hold{{ID: "z", Reason: state.HeldFetchFailed}, {ID: "y", Reason: state.HeldFetchFailed}}
		}, "held must be sorted"},
		"duplicate exclusion":   {func(d *Document) { d.Excluded = []Exclusion{{ID: "z"}, {ID: "z"}} }, "excluded must be sorted"},
		"reused without digest": {func(d *Document) { d.Entries[2].ContentDigest = "" }, "contentDigest"},
		"unsorted rules":        {func(d *Document) { d.Entries[1].License.Rules = []string{"b", "a"} }, "rules must be sorted"},
	} {
		t.Run(name, func(t *testing.T) {
			set := sampleSet()
			tc.change(&set.Document)

			data, err := Encode(&set.Document)
			if err != nil {
				t.Fatal(err)
			}

			_, err = ParseDocument(data)
			wantKind(t, err, fault.Integrity)
			mustContain(t, err.Error(), tc.want)
		})
	}
}
