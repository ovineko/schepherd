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
