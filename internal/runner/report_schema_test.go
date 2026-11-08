package runner

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type reportSchemaLoader struct{}

func (reportSchemaLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("report schema must not load external resource %s", url)
}

func TestCapturedSuccessFailureAndEarlyErrorConformToReportSchema(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "api", "report.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	const schemaURL = "https://schepherd.invalid/api/report.schema.json"
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(reportSchemaLoader{})
	if err := compiler.AddResource(schemaURL, document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(schemaURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []int{0, 23} {
		h := newHarness(t)
		h.behave(map[string]fakeBehavior{defaultKey: {Stdout: []string{"diagnostics"}, Exit: code}})
		opts := h.options(h.spec(ModeBatch, "{files...}"))
		opts.Capture = true
		report, runErr := h.run([]Input{{Path: h.file("a.json", "{}"), SchemaID: "a"}}, h.schemas("a"), opts)
		if code == 0 && runErr != nil {
			t.Fatal(runErr)
		}
		assertReportSchema(t, schema, report)
	}
	assertReportSchema(t, schema, &Report{ReportVersion: 2, Mode: ModePerFile, ExitCode: 2, Error: &ReportError{Kind: "usage", Message: "invalid configuration"}})
	assertReportSchema(t, schema, &Report{ReportVersion: 3, Mode: ModePerFile, ExitCode: 0, Skipped: []Skipped{{File: "/work/a.toml", Reason: "no capable processor", Category: SkipCatalogFormatUnavailable}}})
	v2WithInternalCategory, err := marshalReport(&Report{ReportVersion: 2, Mode: ModePerFile, ExitCode: 0, Skipped: []Skipped{{File: "/work/a.toml", Reason: "no capable processor", Category: SkipCatalogFormatUnavailable}}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(v2WithInternalCategory, []byte(`"category"`)) {
		t.Fatalf("version-two serializer exposed a category: %s", v2WithInternalCategory)
	}
	legacy, err := jsonschema.UnmarshalJSON(bytes.NewReader(v2WithInternalCategory))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(legacy); err != nil {
		t.Fatalf("version-two serialization changed: %v", err)
	}
	invalidV2 := []byte(`{"reportVersion":2,"workspace":"","mode":"per-file","exitCode":0,"tasks":[],"skipped":[{"file":"/work/a.toml","reason":"no capable processor","category":"catalog-format-unavailable"}]}`)
	document, err = jsonschema.UnmarshalJSON(bytes.NewReader(invalidV2))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(document); err == nil {
		t.Fatal("version-two schema accepted a version-three skip category")
	}
}

func assertReportSchema(t *testing.T, schema *jsonschema.Schema, report *Report) {
	t.Helper()
	data, err := marshalReport(report)
	if err != nil {
		t.Fatal(err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(document); err != nil {
		t.Fatalf("report violates schema: %v\n%s", err, data)
	}
}
