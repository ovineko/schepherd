package bundle

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func isolateTemp(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)

	return dir
}

func writeFile(name, content string) error {
	return os.WriteFile(name, []byte(content), 0o600)
}

func mustCompile(t *testing.T, loc, schema string, loader jsonschema.URLLoader) *jsonschema.Schema {
	t.Helper()

	sch, err := compile(loc, []byte(schema), "http://json-schema.org/draft-07/schema#", loader)
	if err != nil {
		t.Fatalf("compile %s: %v", loc, err)
	}

	return sch
}

func TestCompareBehaviourAgrees(t *testing.T) {
	original := mustCompile(t, "https://example.com/root.json",
		`{"$schema":"http://json-schema.org/draft-07/schema#","properties":{"n":{"$ref":"dep.json"}}}`,
		closureLoader{canonicalURI("https://example.com/dep.json"): map[string]any{"type": "integer"}})
	bundled := mustCompile(t, "https://example.com/root.json",
		`{"$schema":"http://json-schema.org/draft-07/schema#","properties":{"n":{"$ref":"dep.json"}},"definitions":{"d":{"$id":"https://example.com/dep.json","type":"integer"}}}`,
		offlineLoader{})

	instances := []Instance{
		{Name: "ok.json", Data: []byte(`{"n":12345678901234567890123}`)},
		{Name: "bad.JSON", Data: []byte(`{"n":1.5}`)},
		{Name: "also-ok.json", Data: []byte(`{}`)},
		{Name: "config.yaml", Data: []byte("n: 1\n")},
		{Name: "config.toml", Data: []byte("n = 1\n")},
		{Name: "noext", Data: []byte(`{}`)},
	}

	checked, err := compareBehaviour(original, bundled, instances)
	if err != nil {
		t.Fatalf("compareBehaviour = %v", err)
	}

	want := Checked{Valid: 2, Invalid: 1, Agreed: 3, SkippedNonJSON: 3}
	if checked != want {
		t.Errorf("checked = %+v, want %+v", checked, want)
	}
}

func TestCompareBehaviourCatchesBrokenBundle(t *testing.T) {
	original := mustCompile(t, "https://example.com/root.json",
		`{"$schema":"http://json-schema.org/draft-07/schema#","properties":{"n":{"type":"integer","maximum":10}}}`, offlineLoader{})
	broken := mustCompile(t, "https://example.com/root.json",
		`{"$schema":"http://json-schema.org/draft-07/schema#","properties":{"n":{"type":"integer"}}}`, offlineLoader{})

	instances := []Instance{
		{Name: "small.json", Data: []byte(`{"n":3}`)},
		{Name: "large.json", Data: []byte(`{"n":11}`)},
	}

	_, err := compareBehaviour(original, broken, instances)

	failure, ok := errors.AsType[*Failure](err)
	if !ok || failure.Reason != ReasonBehaviourMismatch {
		t.Fatalf("compareBehaviour = %v, want behaviour-mismatch", err)
	}

	if !strings.Contains(failure.Detail, "large.json is invalid for the original schema but valid for the bundle") {
		t.Errorf("detail = %q", failure.Detail)
	}
}

func TestCompareBehaviourRejectsInvalidInstances(t *testing.T) {
	sch := mustCompile(t, "https://example.com/root.json", `{"$schema":"http://json-schema.org/draft-07/schema#"}`, offlineLoader{})

	for _, data := range []string{`{"a":1,"a":2}`, `{`, "\xff"} {
		_, err := compareBehaviour(sch, sch, []Instance{{Name: "x.json", Data: []byte(data)}})
		if reasonOf(err) != ReasonInvalidInstance {
			t.Errorf("instance %q: %v, want invalid-instance", data, err)
		}
	}
}

func TestLoaders(t *testing.T) {
	loader := closureLoader{canonicalURI("https://Example.COM/a.json"): true}

	if doc, err := loader.Load("https://example.com/a.json"); err != nil || doc != true {
		t.Errorf("closureLoader normalized lookup = %v, %v", doc, err)
	}

	if _, err := loader.Load("https://example.com/b.json"); err == nil {
		t.Error("closureLoader served a document outside the closure")
	}

	if _, err := (offlineLoader{}).Load("https://example.com/a.json"); err == nil {
		t.Error("offlineLoader loaded a URL")
	}
}

func TestCompileDefaultsToRootDialect(t *testing.T) {
	loader := closureLoader{canonicalURI("https://example.com/dep.json"): map[string]any{
		"type": "number", "maximum": jsonNumber(t, "5"), "exclusiveMaximum": true,
	}}

	_, err := compile("https://example.com/root.json",
		[]byte(`{"$schema":"http://json-schema.org/draft-04/schema#","properties":{"a":{"$ref":"dep.json"}}}`),
		"http://json-schema.org/draft-04/schema#", loader)
	if err != nil {
		t.Errorf("draft-04 dependency without $schema: %v", err)
	}
}

func jsonNumber(t *testing.T, literal string) any {
	t.Helper()

	v, err := jsonschema.UnmarshalJSON(strings.NewReader(literal))
	if err != nil {
		t.Fatal(err)
	}

	return v
}
