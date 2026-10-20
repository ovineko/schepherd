//go:build !windows

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/config"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/processor"
	"github.com/ovineko/schepherd/internal/runner"
)

func buildProcessorPlan(ctx context.Context, cfg *config.Config, inputs []runner.Input, schemas map[string]runner.SchemaInfo, opts runner.Options) (*runner.Plan, []runner.Skipped, func(), error) {
	return buildCompiledProcessorPlan(ctx, cfg, inputs, schemas, opts, &processor.Compiler{})
}

func processorFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "validator")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func processorConfig(t *testing.T, definitions map[string]processor.Definition, order ...string) (*config.Config, runner.Options, []runner.Input, map[string]runner.SchemaInfo) {
	t.Helper()
	dir := t.TempDir()
	input := filepath.Join(dir, "input.json")
	schema := filepath.Join(dir, "schema.json")
	for _, file := range []string{input, schema} {
		if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base := runner.Spec{Mode: runner.ModePerFile, Cwd: runner.DefaultCwd, CwdBase: dir, Jobs: 1, Timeout: runner.DefaultTimeout, MaxArgsBytes: runner.DefaultMaxArgsBytes(), Env: map[string]string{}, InheritEnv: false}
	for name, definition := range definitions {
		command := definition.Spec.Command
		definition.Spec = base
		definition.Spec.Command = command
		definition.Spec.Name = name
		definition.Spec.Type = definition.Type
		definition.Spec.Output = definition.Type + "-json"
		definition.Spec.Args = processor.DefaultArgs(definition.Type)
		definitions[name] = definition
	}
	cfg := &config.Config{Workspace: dir, Runner: base, Processors: definitions, ProcessorOrder: order, Unsupported: "error"}
	opts := runner.Options{Spec: base, Workspace: dir, Capture: true}
	inputs := []runner.Input{{Path: input, SchemaID: "schema"}}
	schemas := map[string]runner.SchemaInfo{"schema": {ID: "schema", Path: schema, Ref: "local:" + schema, Origin: "local"}}
	return cfg, opts, inputs, schemas
}

func TestProcessorFallsBackOnlyBeforeDocumentValidation(t *testing.T) {
	unsupported := processorFixture(t, `if [ "$1" = compile ]; then exit 3; fi
exit 99
`)
	marker := filepath.Join(t.TempDir(), "validated")
	invalid := processorFixture(t, `if [ "$1" = compile ]; then printf '{"supported":true,"template":"module.exports = () => false"}'; exit 0; fi
printf invalid > '`+marker+`'
printf '{"valid":false,"errors":[]}'
exit 1
`)
	later := processorFixture(t, "exit 99\n")
	cfg, opts, inputs, schemas := processorConfig(t, map[string]processor.Definition{
		"native":   {Type: "sourcemeta", Spec: runner.Spec{Command: unsupported}},
		"selected": {Type: "ajv", Spec: runner.Spec{Command: invalid}},
		"later":    {Type: "ajv", Spec: runner.Spec{Command: later}},
	}, "native", "selected", "later")
	plan, skipped, cleanup, err := buildProcessorPlan(context.Background(), cfg, inputs, schemas, opts)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil || len(skipped) != 0 {
		t.Fatalf("plan: %v %v", skipped, err)
	}
	if len(plan.Tasks) != 1 || plan.Tasks[0].Processor.Name != "selected" {
		t.Fatalf("selection: %+v", plan)
	}
	report, err := runner.Execute(context.Background(), plan, opts)
	if err == nil || report.ExitCode != 1 {
		t.Fatalf("invalid result: %+v %v", report, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("selected consumer not executed: %v", err)
	}
}

func TestProcessorInfrastructureNeverSkipsOrFallsBack(t *testing.T) {
	broken := processorFixture(t, "exit 1\n")
	cfg, opts, inputs, schemas := processorConfig(t, map[string]processor.Definition{"broken": {Type: "ajv", Spec: runner.Spec{Command: broken}}}, "broken")
	cfg.Unsupported = "skip"
	_, skipped, cleanup, err := buildProcessorPlan(context.Background(), cfg, inputs, schemas, opts)
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil || fault.KindOf(err) != fault.ConsumerStart || len(skipped) != 0 {
		t.Fatalf("infrastructure: skipped=%v err=%v", skipped, err)
	}
}

func TestProcessorUnsupportedFormatSkipIsVisible(t *testing.T) {
	cfg, opts, inputs, schemas := processorConfig(t, map[string]processor.Definition{"native": {Type: "sourcemeta", Spec: runner.Spec{Command: "not-started"}}}, "native")
	inputs[0].Path = filepath.Join(cfg.Workspace, "input.toml")
	_, _, _, err := buildProcessorPlan(context.Background(), cfg, inputs, schemas, opts)
	if err == nil || fault.KindOf(err) != fault.Usage {
		t.Fatalf("default unsupported: %v", err)
	}
	cfg.Unsupported = "skip"
	plan, skipped, cleanup, err := buildProcessorPlan(context.Background(), cfg, inputs, schemas, opts)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil || len(plan.Tasks) != 0 || len(skipped) != 1 || !strings.Contains(skipped[0].Reason, "toml") {
		t.Fatalf("skip: %+v %v %v", plan, skipped, err)
	}
}

func TestProcessorConversionPreservesAttribution(t *testing.T) {
	validator := processorFixture(t, `if [ "$1" = compile ]; then printf '{"supported":true,"template":"module.exports = () => true"}'; exit 0; fi
exit 0
`)
	converter := processorFixture(t, "printf '{\"answer\":42}'\n")
	cfg, opts, inputs, schemas := processorConfig(t, map[string]processor.Definition{"json": {Type: "ajv", Convert: converter, Formats: []string{"toml"}, Spec: runner.Spec{Command: validator}}}, "json")
	inputs[0].Path = filepath.Join(cfg.Workspace, "input.toml")
	if err := os.WriteFile(inputs[0].Path, []byte("answer=42"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, _, cleanup, err := buildProcessorPlan(context.Background(), cfg, inputs, schemas, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	task := plan.Tasks[0]
	if task.Files[0] != inputs[0].Path || task.Paths[0] == task.Files[0] {
		t.Fatalf("attribution: %+v", task)
	}
	data, err := os.ReadFile(task.Paths[0])
	if err != nil || string(data) != `{"answer":42}` {
		t.Fatalf("converted bytes: %q %v", data, err)
	}
	cleanup()
	if _, err := os.Stat(task.Paths[0]); !os.IsNotExist(err) {
		t.Fatalf("converted file was not removed: %v", err)
	}
}

func TestProcessorConversionRejectsMultipleDocuments(t *testing.T) {
	validator := processorFixture(t, "printf '{\"supported\":true,\"template\":\"module.exports = () => true\"}'\n")
	converter := processorFixture(t, "printf '{}\\n{}\\n'\n")
	cfg, opts, inputs, schemas := processorConfig(t, map[string]processor.Definition{"json": {Type: "ajv", Convert: converter, Formats: []string{"toml"}, Spec: runner.Spec{Command: validator}}}, "json")
	inputs[0].Path = filepath.Join(cfg.Workspace, "input.toml")
	if err := os.WriteFile(inputs[0].Path, []byte("answer=42"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, skipped, cleanup, err := buildProcessorPlan(context.Background(), cfg, inputs, schemas, opts)
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil || fault.KindOf(err) != fault.Usage || len(skipped) != 0 {
		t.Fatalf("multiple converted documents: skipped=%v err=%v", skipped, err)
	}
}

func TestProcessorExplicitSelectionDoesNotTryOtherDeclaredProcessors(t *testing.T) {
	unsupported := processorFixture(t, "exit 3\n")
	cfg, opts, inputs, schemas := processorConfig(t, map[string]processor.Definition{
		"automatic": {Type: "ajv", Spec: runner.Spec{Command: "must-not-start"}},
		"chosen":    {Type: "sourcemeta", Spec: runner.Spec{Command: unsupported}},
	}, "automatic", "chosen")
	cfg.LocalSchemas = []config.LocalSchema{{ID: "schema", Path: schemas["schema"].Path, Processor: "chosen"}}
	_, _, cleanup, err := buildProcessorPlan(context.Background(), cfg, inputs, schemas, opts)
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil || fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), "chosen") {
		t.Fatalf("explicit selection: %v", err)
	}
}

func TestCustomProcessorRetainsBatchGrouping(t *testing.T) {
	cfg, opts, inputs, schemas := processorConfig(t, map[string]processor.Definition{"custom": {Type: "custom", Spec: runner.Spec{Command: processorFixture(t, "exit 0\n")}}}, "custom")
	definition := cfg.Processors["custom"]
	definition.Spec.Mode = runner.ModeBatch
	definition.Spec.Args = []string{"{schema}", "{files...}"}
	cfg.Processors["custom"] = definition
	second := filepath.Join(cfg.Workspace, "second.json")
	if err := os.WriteFile(second, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	inputs = append(inputs, runner.Input{Path: second, SchemaID: "schema"})
	opts.Capture = false
	plan, _, cleanup, err := buildProcessorPlan(context.Background(), cfg, inputs, schemas, opts)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 1 || len(plan.Tasks[0].Files) != 2 || plan.Mode != runner.ModeBatch {
		t.Fatalf("batch plan: %+v", plan)
	}
}

func TestProcessorSchemaCompileRejectionFallsBackBeforeValidation(t *testing.T) {
	rejected := processorFixture(t, "printf 'unsupported schema construct' >&2\nexit 4\n")
	accepted := processorFixture(t, "printf '{\"supported\":true}'\n")
	cfg, opts, inputs, schemas := processorConfig(t, map[string]processor.Definition{
		"native": {Type: "sourcemeta", Spec: runner.Spec{Command: rejected}},
		"python": {Type: "check-jsonschema", Spec: runner.Spec{Command: accepted}},
	}, "native", "python")
	plan, _, cleanup, err := buildProcessorPlan(context.Background(), cfg, inputs, schemas, opts)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil || len(plan.Tasks) != 1 || plan.Tasks[0].Processor.Name != "python" {
		t.Fatalf("compile rejection fallback: %+v %v", plan, err)
	}
	cfg.Processors["python"] = cfg.Processors["native"]
	_, _, cleanup, err = buildProcessorPlan(context.Background(), cfg, inputs, schemas, opts)
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil || fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), "unsupported schema construct") {
		t.Fatalf("all processors rejected schema: %v", err)
	}
}

func TestMalformedSchemaNeverBecomesUnsupportedSkip(t *testing.T) {
	cfg, opts, inputs, schemas := processorConfig(t, map[string]processor.Definition{"native": {Type: "sourcemeta", Spec: runner.Spec{Command: "must-not-start"}}}, "native")
	cfg.Unsupported = "skip"
	if err := os.WriteFile(schemas["schema"].Path, []byte(`{"broken":`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, skipped, cleanup, err := buildProcessorPlan(context.Background(), cfg, inputs, schemas, opts)
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil || fault.KindOf(err) != fault.Usage || len(skipped) != 0 {
		t.Fatalf("malformed schema: %v %v", skipped, err)
	}
}

func TestProcessorCompilationRespectsInputDependentSettings(t *testing.T) {
	validator := processorFixture(t, `case "$1" in
compile) printf '{"selected":"%s"}' "$SELECTED" ;;
validate)
  shift 3
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --template) template=$2; shift 2 ;;
      *) shift ;;
    esac
  done
  test "$(cat "$template")" = "{\"selected\":\"$SELECTED\"}" ;;
*) exit 9 ;;
esac
`)
	cfg, opts, inputs, schemas := processorConfig(t, map[string]processor.Definition{
		"native": {Type: "sourcemeta", Spec: runner.Spec{Command: validator}},
	}, "native")
	definition := cfg.Processors["native"]
	definition.Spec.Env = map[string]string{"SELECTED": "{file}"}
	definition.Convert = processorFixture(t, "printf '{}'\n")
	definition.Formats = []string{"toml"}
	cfg.Processors["native"] = definition
	inputs[0].Path = filepath.Join(cfg.Workspace, "input.toml")
	second := filepath.Join(cfg.Workspace, "second.toml")
	for _, path := range []string{inputs[0].Path, second} {
		if err := os.WriteFile(path, []byte("value=1"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inputs = append(inputs, runner.Input{Path: second, SchemaID: "schema"})
	plan, skipped, cleanup, err := buildProcessorPlan(context.Background(), cfg, inputs, schemas, opts)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil || len(skipped) != 0 || len(plan.Tasks) != 2 {
		t.Fatalf("input-dependent plan: %+v %v %v", plan, skipped, err)
	}
	for index, task := range plan.Tasks {
		if task.Files[0] != inputs[index].Path || task.Paths[0] == inputs[index].Path {
			t.Fatalf("conversion lost original/actual input identity: %+v", task)
		}
	}
	report, err := runner.Execute(context.Background(), plan, opts)
	if err != nil || report.Tasks[0].Status != runner.StatusOK || report.Tasks[1].Status != runner.StatusOK {
		t.Fatalf("a document used another document's compiler settings: %+v %v", report, err)
	}
}
