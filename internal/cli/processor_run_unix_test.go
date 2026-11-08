//go:build !windows

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/config"
	"github.com/ovineko/schepherd/internal/digest"
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
	if err != nil || len(plan.Tasks) != 0 || len(skipped) != 1 || skipped[0].Category != runner.SkipUnsupportedProcessor || !strings.Contains(skipped[0].Reason, "toml") {
		t.Fatalf("skip: %+v %v %v", plan, skipped, err)
	}
}

func TestSourcemetaDoesNotPartiallyValidateMultiDocumentYAML(t *testing.T) {
	cfg, opts, inputs, schemas := processorConfig(t, map[string]processor.Definition{
		"native": {Type: "sourcemeta"},
	}, "native")
	cfg.Unsupported = "skip"
	inputs[0].Path = filepath.Join(cfg.Workspace, "stream.yaml")
	if err := os.WriteFile(inputs[0].Path, []byte("---\nvalue: one\n---\nvalue: two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, skipped, cleanup, err := buildProcessorPlan(t.Context(), cfg, inputs, schemas, opts)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil || len(plan.Tasks) != 0 || len(skipped) != 1 || skipped[0].Category != runner.SkipUnsupportedProcessor || !strings.Contains(skipped[0].Reason, "multi-document YAML") {
		t.Fatalf("multi-document YAML was not rejected before validation: plan=%+v skipped=%+v err=%v", plan, skipped, err)
	}
}

func TestProcessorConversionPreservesAttribution(t *testing.T) {
	validator := processorFixture(t, `if [ "$1" = compile ]; then printf '{"supported":true,"template":"module.exports = () => true"}'; exit 0; fi
exit 0
`)
	converter := processorFixture(t, `printf '{"format":"%s"}' "$1"`)
	cfg, opts, inputs, schemas := processorConfig(t, map[string]processor.Definition{"json": {Type: "ajv", Convert: converter, ConvertArgs: []string{"-p={format}", "{file}"}, Formats: []string{"toml"}, Spec: runner.Spec{Command: validator}}}, "json")
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
	if err != nil || string(data) != `{"format":"-p=toml"}` {
		t.Fatalf("converted bytes: %q %v", data, err)
	}
	cleanup()
	if _, err := os.Stat(task.Paths[0]); !os.IsNotExist(err) {
		t.Fatalf("converted file was not removed: %v", err)
	}
}

func TestCheckJSONSchemaConversionUsesOriginalFormatAndConvertedJSON(t *testing.T) {
	validator := processorFixture(t, `case "$1" in
probe) printf '{"supported":true}' ;;
validate)
  test "$4" = --format && test "$5" = json || exit 8
  test "$(cat "$3")" = '{"answer":42}' || exit 9
  printf '{"status":"ok","errors":[],"parse_errors":[]}' ;;
*) exit 10 ;;
esac
`)
	converter := processorFixture(t, `test "$1" = -p=toml && test "$2" = -o=json && test "$3" = -I=0 || exit 8
printf '{"answer":42}'
`)
	cfg, opts, inputs, schemas := processorConfig(t, map[string]processor.Definition{
		"python": {Type: "check-jsonschema", Convert: converter, ConvertArgs: []string{"-p={format}", "-o=json", "-I=0", "{file}"}, Formats: []string{"toml"}, Spec: runner.Spec{Command: validator}},
	}, "python")
	inputs[0].Path = filepath.Join(cfg.Workspace, "policy")
	if err := os.WriteFile(inputs[0].Path, []byte("answer = 42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	schema := schemas["schema"]
	schema.SourceFormats = []runner.DocumentFormat{{Format: "toml", Filenames: []string{"policy"}}}
	schemas["schema"] = schema

	plan, skipped, cleanup, err := buildProcessorPlan(t.Context(), cfg, inputs, schemas, opts)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil || len(skipped) != 0 || len(plan.Tasks) != 1 {
		t.Fatalf("conversion plan: plan=%+v skipped=%v err=%v", plan, skipped, err)
	}
	task := plan.Tasks[0]
	if task.Files[0] != inputs[0].Path || task.Paths[0] == task.Files[0] {
		t.Fatalf("conversion attribution: %+v", task)
	}
	report, err := runner.Execute(t.Context(), plan, opts)
	if err != nil || report.Tasks[0].Status != runner.StatusOK {
		t.Fatalf("converted check-jsonschema run: report=%+v err=%v", report, err)
	}
}

func TestProcessorProfilesRequireMatchingSchemaAndIdentity(t *testing.T) {
	for _, test := range []struct {
		name             string
		result           string
		reason           string
		schemaDigest     string
		settingsMismatch bool
		toolMismatch     bool
		duplicate        bool
		wantProbe        bool
		wantError        bool
	}{
		{name: "matching supported profile", result: "supported"},
		{name: "schema digest mismatch reprobes", result: "supported", schemaDigest: "sha256:" + strings.Repeat("0", 64), wantProbe: true},
		{name: "settings mismatch reprobes", result: "supported", settingsMismatch: true, wantProbe: true},
		{name: "tool identity mismatch reprobes", result: "supported", toolMismatch: true, wantProbe: true},
		{name: "duplicate exact profiles are ambiguous", result: "supported", duplicate: true, wantError: true},
		{name: "matching unsupported profile skips", result: "unsupported", reason: "dialect unsupported", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "probe-ran")
			validator := processorFixture(t, `case "$1" in
identity) printf '{"runtime":"fixture"}' ;;
probe) printf x >> "$PROBE_MARKER"; printf '{"supported":true}' ;;
*) exit 9 ;;
esac
`)
			cfg, opts, inputs, schemas := processorConfig(t, map[string]processor.Definition{
				"validator": {Type: "check-jsonschema", Spec: runner.Spec{Command: validator}},
			}, "validator")
			definition := cfg.Processors["validator"]
			definition.Spec.Env = map[string]string{"PROBE_MARKER": marker}
			cfg.Processors["validator"] = definition
			identityPlan, err := runner.BuildPlan(inputs, schemas, runner.Options{Spec: definition.Spec, Workspace: cfg.Workspace, Capture: true})
			if err != nil {
				t.Fatal(err)
			}
			tool, settings, err := processor.Identity(t.Context(), definition, identityPlan.Tasks[0])
			if err != nil {
				t.Fatal(err)
			}
			schemaBytes, err := os.ReadFile(schemas["schema"].Path)
			if err != nil {
				t.Fatal(err)
			}
			schemaDigest := digest.FromBytes(schemaBytes)
			if test.schemaDigest != "" {
				schemaDigest = test.schemaDigest
			}
			if test.settingsMismatch {
				settings = "sha256:" + strings.Repeat("1", 64)
			}
			if test.toolMismatch {
				tool = "sha256:" + strings.Repeat("2", 64)
			}
			schema := schemas["schema"]
			schema.Origin = originCatalog
			schema.SchemaDigest = schemaDigest
			profile := runner.ProcessorProfile{Processor: "check-jsonschema", Tool: tool, SettingsDigest: settings, Result: test.result, Reason: test.reason}
			schema.ProcessorProfiles = []runner.ProcessorProfile{profile}
			if test.duplicate {
				schema.ProcessorProfiles = append(schema.ProcessorProfiles, profile)
			}
			schemas["schema"] = schema

			plan, skipped, cleanup, err := buildProcessorPlan(t.Context(), cfg, inputs, schemas, opts)
			if cleanup != nil {
				defer cleanup()
			}
			if test.wantError {
				if err == nil || len(skipped) != 0 {
					t.Fatalf("expected recorded unsupported profile: plan=%+v skipped=%v err=%v", plan, skipped, err)
				}
			} else if err != nil || len(plan.Tasks) != 1 || len(skipped) != 0 {
				t.Fatalf("profile selection: plan=%+v skipped=%v err=%v", plan, skipped, err)
			}
			_, statErr := os.Stat(marker)
			if ran := statErr == nil; ran != test.wantProbe {
				t.Fatalf("probe ran = %v, want %v (stat error %v)", ran, test.wantProbe, statErr)
			}
		})
	}
}

func TestInputDependentProfileProbesEachRealInput(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "probes")
	validator := processorFixture(t, `case "$1" in
identity) printf '{"runtime":"fixture"}' ;;
probe)
  printf '%s\n' "$SELECTED" >> "$PROBE_MARKER"
  if [ "$SELECTED" = "$UNSUPPORTED_PATH" ]; then
    printf '{"supported":false,"reason":"input rejected"}'
  else
    printf '{"supported":true}'
  fi ;;
*) exit 9 ;;
esac
`)
	cfg, opts, inputs, schemas := processorConfig(t, map[string]processor.Definition{
		"validator": {Type: "check-jsonschema", Spec: runner.Spec{Command: validator}},
	}, "validator")
	cfg.Unsupported = "skip"
	definition := cfg.Processors["validator"]
	definition.Spec.Env = map[string]string{"PROBE_MARKER": marker, "UNSUPPORTED_PATH": inputs[0].Path, "SELECTED": "{file}"}
	cfg.Processors["validator"] = definition
	second := filepath.Join(cfg.Workspace, "supported.json")
	if err := os.WriteFile(second, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	inputs = append(inputs, runner.Input{Path: second, SchemaID: "schema"})
	identityOptions := opts
	identityOptions.Spec = definition.Spec
	identityPlan, err := runner.BuildPlan(inputs[:1], schemas, identityOptions)
	if err != nil {
		t.Fatal(err)
	}
	tool, settings, err := processor.Identity(t.Context(), definition, identityPlan.Tasks[0])
	if err != nil {
		t.Fatal(err)
	}
	schema := schemas["schema"]
	contents, err := os.ReadFile(schema.Path)
	if err != nil {
		t.Fatal(err)
	}
	schema.Origin = originCatalog
	schema.SchemaDigest = digest.FromBytes(contents)
	schema.ProcessorProfiles = []runner.ProcessorProfile{{Processor: "check-jsonschema", Tool: tool, SettingsDigest: settings, Result: "unsupported", Reason: "input rejected"}}
	schemas["schema"] = schema
	plan, skipped, cleanup, err := buildProcessorPlan(t.Context(), cfg, inputs, schemas, opts)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 1 || len(plan.Tasks[0].Files) != 1 || plan.Tasks[0].Files[0] != second || len(skipped) != 1 || skipped[0].File != inputs[0].Path {
		t.Fatalf("per-input probing: plan=%+v skipped=%+v", plan, skipped)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if want := inputs[0].Path + "\n" + second + "\n"; string(data) != want {
		t.Fatalf("probed inputs = %q, want %q", data, want)
	}
}

func TestOfflineLocalTypedSchemaCompilesWithoutSourceCache(t *testing.T) {
	validator := processorFixture(t, `case "$1" in
compile) printf '{"supported":true,"template":"module.exports = () => true"}' ;;
validate) exit 0 ;;
*) exit 9 ;;
esac
`)
	cfg, opts, inputs, schemas := processorConfig(t, map[string]processor.Definition{
		"native": {Type: "sourcemeta", Spec: runner.Spec{Command: validator}},
	}, "native")
	cachePath := filepath.Join(cfg.Workspace, "absent-cache")
	cfg.CacheDir = cachePath
	cfg.Offline = true
	source := &sources{cfg: cfg}
	compiler, closeCompiler, err := source.compiler(schemas)
	if err != nil {
		t.Fatal(err)
	}
	defer closeCompiler()
	if compiler.Cache != nil || !compiler.VerifyOnly {
		t.Fatalf("local compiler cache state: %+v", compiler)
	}
	plan, skipped, cleanup, err := buildCompiledProcessorPlan(t.Context(), cfg, inputs, schemas, opts, compiler)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil || len(skipped) != 0 || len(plan.Tasks) != 1 || plan.Tasks[0].Paths[0] != inputs[0].Path {
		t.Fatalf("local offline plan: plan=%+v skipped=%v err=%v", plan, skipped, err)
	}
	report, err := runner.Execute(t.Context(), plan, opts)
	if err != nil || report.Tasks[0].Status != runner.StatusOK {
		t.Fatalf("local offline validation: report=%+v err=%v", report, err)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("local offline typed run created source cache: %v", err)
	}
}

func TestSafeCatalogDoesNotSkipInvalidMatchedDocument(t *testing.T) {
	workspace := t.TempDir()
	configPath := filepath.Join(workspace, "schepherd.toml")
	input := filepath.Join(workspace, "broken.json")
	writeTestFile(t, filepath.Join(workspace, "schema.json"), `{"type":"object"}`)
	writeTestFile(t, input, "{")
	validator := processorFixture(t, `case "$1" in
probe) printf '{"supported":true}' ;;
validate) printf '{"status":"fail","parse_errors":[{"message":"invalid JSON","filename":"%s","path":"$"}]}' "$3"; exit 6 ;;
*) exit 9 ;;
esac
`)
	config := "config_version = 1\n\n[processors.python]\ntype = \"check-jsonschema\"\ncommand = " + strconv.Quote(validator) + "\ninherit_env = false\n\n" +
		"[schemas.local]\npath = \"schema.json\"\nfile_match = [\"*.json\"]\nprocessor = \"python\"\n"
	writeTestFile(t, configPath, config)
	stdout, stderr, code := run(t, "--config", configPath, "--workspace", workspace, "--cache-dir", filepath.Join(workspace, "cache"), "run", "--safe-catalog", "--json", "--", input)
	report := decodeRunJSON(t, stdout, stderr, code)
	if code != 6 || len(report.Tasks) != 1 || report.Tasks[0].Status != runner.StatusFailed || len(report.Skipped) != 0 || report.Tasks[0].Files[0] != input {
		t.Fatalf("safe-catalog skipped or lost invalid matching input: code=%d report=%+v", code, report)
	}
	if report.Tasks[0].Stdout == nil || report.Tasks[0].Stdout.Text == nil || !strings.Contains(*report.Tasks[0].Stdout.Text, input) {
		t.Fatalf("document parse finding lost input attribution: %+v", report.Tasks[0].Stdout)
	}
}

func TestMalformedSecondYAMLDocumentUsesFullStreamValidator(t *testing.T) {
	input := filepath.Join(t.TempDir(), "input.yaml")
	if err := os.WriteFile(input, []byte("enabled: true\n---\nenabled: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourcemetaMarker := filepath.Join(t.TempDir(), "sourcemeta-ran")
	sourcemeta := processorFixture(t, `printf x >> "$SOURCEMETA_MARKER"
exit 3
`)
	checkJSONSchema := processorFixture(t, `case "$1" in
probe) printf '{"supported":true}' ;;
validate) printf '{"status":"fail","parse_errors":[{"message":"malformed YAML","filename":"%s","path":"$"}]}' "$3"; exit 6 ;;
*) exit 9 ;;
esac
`)
	cfg, opts, inputs, schemas := processorConfig(t, map[string]processor.Definition{
		"native": {Type: "sourcemeta", Spec: runner.Spec{Command: sourcemeta}},
		"python": {Type: "check-jsonschema", Spec: runner.Spec{Command: checkJSONSchema}},
	}, "native", "python")
	inputs[0].Path = input
	native := cfg.Processors["native"]
	native.Spec.Env = map[string]string{"SOURCEMETA_MARKER": sourcemetaMarker}
	cfg.Processors["native"] = native

	plan, skipped, cleanup, err := buildProcessorPlan(t.Context(), cfg, inputs, schemas, opts)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil || len(skipped) != 0 || len(plan.Tasks) != 1 || plan.Tasks[0].Processor.Type != "check-jsonschema" {
		t.Fatalf("malformed YAML selection: plan=%+v skipped=%v err=%v", plan, skipped, err)
	}
	report, err := runner.Execute(t.Context(), plan, opts)
	if err == nil || report.ExitCode != 6 || report.Tasks[0].Status != runner.StatusFailed {
		t.Fatalf("malformed second document did not fail validation: report=%+v err=%v", report, err)
	}
	if report.Tasks[0].Files[0] != input || report.Tasks[0].Processor.Type != "check-jsonschema" {
		t.Fatalf("parse failure attribution/validator = %+v", report.Tasks[0])
	}
	if report.Tasks[0].Stdout == nil || report.Tasks[0].Stdout.Text == nil || !strings.Contains(*report.Tasks[0].Stdout.Text, input) || !strings.Contains(*report.Tasks[0].Stdout.Text, "malformed YAML") {
		t.Fatalf("parse failure was not attributed to the original input: %+v", report.Tasks[0].Stdout)
	}
	if _, err := os.Stat(sourcemetaMarker); !os.IsNotExist(err) {
		t.Fatalf("Sourcemeta inspected a prefix of the malformed stream: %v", err)
	}
}

func TestProcessorConversionRejectsMultipleDocuments(t *testing.T) {
	validator := processorFixture(t, "printf '{\"supported\":true,\"template\":\"module.exports = () => true\"}'\n")
	converter := processorFixture(t, "printf '{}\\n{}\\n'\n")
	cfg, opts, inputs, schemas := processorConfig(t, map[string]processor.Definition{"json": {Type: "ajv", Convert: converter, ConvertArgs: []string{"{file}"}, Formats: []string{"toml"}, Spec: runner.Spec{Command: validator}}}, "json")
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

func TestCustomBatchConversionUsesPerFileFormatExpansion(t *testing.T) {
	validator := processorFixture(t, "test \"$#\" -eq 3\n")
	converter := processorFixture(t, "test \"$1\" = '-p=toml' || exit 8\nprintf '{}'\n")
	cfg, opts, inputs, schemas := processorConfig(t, map[string]processor.Definition{
		"custom": {Type: "custom", Spec: runner.Spec{Command: validator}},
	}, "custom")
	definition := cfg.Processors["custom"]
	definition.Spec.Mode = runner.ModeBatch
	definition.Spec.Args = []string{"{schema}", "{files...}"}
	definition.Convert = converter
	definition.ConvertArgs = []string{"-p={format}", "{file}"}
	definition.Formats = []string{"toml"}
	cfg.Processors["custom"] = definition
	inputs[0].Path = filepath.Join(cfg.Workspace, "first.toml")
	second := filepath.Join(cfg.Workspace, "second.toml")
	for _, path := range []string{inputs[0].Path, second} {
		if err := os.WriteFile(path, []byte("value=1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inputs = append(inputs, runner.Input{Path: second, SchemaID: "schema"})
	opts.Capture = false
	plan, skipped, cleanup, err := buildProcessorPlan(t.Context(), cfg, inputs, schemas, opts)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil || len(skipped) != 0 || len(plan.Tasks) != 1 || plan.Mode != runner.ModeBatch {
		t.Fatalf("converted batch plan: plan=%+v skipped=%v err=%v", plan, skipped, err)
	}
	if len(plan.Tasks[0].Files) != 2 || len(plan.Tasks[0].Paths) != 2 || plan.Tasks[0].Files[0] != inputs[0].Path || plan.Tasks[0].Paths[0] == plan.Tasks[0].Files[0] {
		t.Fatalf("converted batch lost input attribution: %+v", plan.Tasks[0])
	}
	report, err := runner.Execute(t.Context(), plan, opts)
	if err != nil || report.Tasks[0].Status != runner.StatusOK {
		t.Fatalf("converted batch execution: report=%+v err=%v", report, err)
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
	definition.ConvertArgs = []string{"{file}"}
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
