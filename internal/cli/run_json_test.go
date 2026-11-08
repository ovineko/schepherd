package cli

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/config"
	"github.com/ovineko/schepherd/internal/match"
	"github.com/ovineko/schepherd/internal/processor"
	"github.com/ovineko/schepherd/internal/runner"
)

func decodeRunJSON(t *testing.T, stdout, stderr string, code int) runner.Report {
	t.Helper()
	if stderr != "" {
		t.Fatalf("machine run wrote bytes outside its document: %q", stderr)
	}
	decoder := json.NewDecoder(strings.NewReader(stdout))
	var report runner.Report
	if err := decoder.Decode(&report); err != nil {
		t.Fatalf("missing JSON report: %v\n%s", err, stdout)
	}
	if report.ReportVersion != runner.CaptureReportVersion || report.ExitCode != code || report.Tasks == nil || report.Skipped == nil {
		t.Fatalf("invalid machine report: %+v, exit %d", report, code)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("extra output after report: %v", err)
	}
	return report
}

func publishManifestCollisionCatalog(t *testing.T) (*testRegistry, string, string) {
	t.Helper()
	reg := newTestRegistry(t)
	repoPath := "org/schemas"
	repo := reg.repo(repoPath, "", "")
	entries := make([]catalog.Entry, 0, 2)
	for _, item := range []struct{ id, schema string }{
		{"fox-manifest", `{"type":"object","title":"Fox"}`},
		{"webextension", `{"type":"object","title":"WebExtension"}`},
	} {
		packed, err := artifact.PackSchema([]byte(item.schema), nil)
		if err != nil {
			t.Fatal(err)
		}
		reg.add(repoPath, packed)
		entries = append(entries, catalog.Entry{
			ID: item.id, Name: item.id + ".json", FileMatch: []string{"manifest.json"},
			Artifact: catalog.Descriptor{MediaType: packed.Manifest.Descriptor.MediaType, Digest: packed.Manifest.Descriptor.Digest.String(), Size: packed.Manifest.Descriptor.Size},
		})
	}
	raw, err := catalog.Marshal(&catalog.Catalog{FormatVersion: catalog.FormatVersion, Revision: "20260924.1200", Schemas: entries})
	if err != nil {
		t.Fatal(err)
	}
	return reg, repo, reg.publishRawCatalog(t, repoPath, raw)
}

func TestSafeCatalogSkipsAutomaticManifestCollisionsButHonorsMappings(t *testing.T) {
	isolateDockerConfig(t)
	reg, repository, pinned := publishManifestCollisionCatalog(t)
	workspace := t.TempDir()
	input := filepath.Join(workspace, "manifest.json")
	if err := os.WriteFile(input, []byte(`{"manifest_version":"3.0.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(workspace, "consumer.args")
	configPath := filepath.Join(workspace, "schepherd.toml")
	writeConfig := func(mapping string) {
		t.Helper()
		config := "config_version = 1\n\n[registries." + strconv.Quote(reg.host()) + "]\nplain_http = true\n\n" + mapping + consumerRunner(t, marker, 0)
		writeTestFile(t, configPath, config)
	}
	writeConfig("")
	base := []string{"--quiet", "--config", configPath, "--workspace", workspace, "--cache-dir", filepath.Join(workspace, "cache"), "--repository", repository, "--catalog", pinned}
	strictStdout, strictStderr, strictCode := run(t, append(append([]string{}, base...), "run", "--json", "--", input)...)
	strict := decodeRunJSON(t, strictStdout, strictStderr, strictCode)
	if strictCode != 3 || strict.Error == nil || strict.Error.File != input || !strings.Contains(strict.Error.Message, "fox-manifest") || !strings.Contains(strict.Error.Message, "webextension") {
		t.Fatalf("strict ambiguity lost candidates or input: report=%+v", strict)
	}
	safeStdout, safeStderr, safeCode := run(t, append(append([]string{}, base...), "run", "--safe-catalog", "--ignore-unmatched", "--json", "--", input)...)
	safe := decodeRunJSON(t, safeStdout, safeStderr, safeCode)
	if safeCode != 0 || len(safe.Tasks) != 0 || len(safe.Skipped) != 1 || safe.Skipped[0].File != input || safe.Skipped[0].Category != runner.SkipCatalogAmbiguous || !strings.Contains(safe.Skipped[0].Reason, "fox-manifest") || !strings.Contains(safe.Skipped[0].Reason, "webextension") {
		t.Fatalf("safe catalog ambiguity was not explicitly skipped: report=%+v", safe)
	}
	writeConfig("[[mappings]]\nschema = \"fox-manifest\"\nfile_match = [\"manifest.json\"]\n\n")
	mappedStdout, mappedStderr, mappedCode := run(t, append(append([]string{}, base...), "run", "--safe-catalog", "--json", "--", input)...)
	mapped := decodeRunJSON(t, mappedStdout, mappedStderr, mappedCode)
	if mappedCode != 0 || len(mapped.Tasks) != 1 || mapped.Tasks[0].SchemaID != "fox-manifest" {
		t.Fatalf("explicit mapping did not disambiguate catalog collision: report=%+v", mapped)
	}
	args, err := os.ReadFile(marker)
	if err != nil || !strings.Contains(string(args), input) {
		t.Fatalf("mapped manifest did not reach the validator: %q %v", args, err)
	}
	localSchema := filepath.Join(workspace, "local.schema.json")
	writeTestFile(t, localSchema, `{"type":"object"}`)
	writeConfig("[schemas.local]\npath = \"local.schema.json\"\nfile_match = [\"manifest.json\"]\n\n")
	localStdout, localStderr, localCode := run(t, append(append([]string{}, base...), "run", "--safe-catalog", "--json", "--", input)...)
	local := decodeRunJSON(t, localStdout, localStderr, localCode)
	if localCode != 0 || len(local.Tasks) != 1 || local.Tasks[0].SchemaID != "local" || local.Tasks[0].Origin != "local" {
		t.Fatalf("local schema did not retain precedence in safe mode: report=%+v", local)
	}
}

func TestSafeCatalogSkipsSchemasWithNoFormatCapableProcessor(t *testing.T) {
	workspace := t.TempDir()
	set, err := match.NewSet(match.OriginCatalog, []match.Rule{{SchemaID: "yaml-only", Patterns: []string{"*.toml"}}})
	if err != nil {
		t.Fatal(err)
	}
	r := &resolver{
		src: &sources{cfg: &config.Config{
			Processors:     map[string]processor.Definition{"native": {Type: "sourcemeta"}},
			ProcessorOrder: []string{"native"},
		}},
		catalog: set, workspace: workspace, safeCatalog: true,
	}
	_, _, err = r.resolve(t.Context(), filepath.Join(workspace, "settings.toml"))
	unavailable, ok := errors.AsType[*safeCatalogUnavailableError](err)
	if !ok || unavailable.category != runner.SkipCatalogFormatUnavailable || !strings.Contains(unavailable.Error(), "toml") || !strings.Contains(unavailable.Error(), "yaml-only") {
		t.Fatalf("format-incompatible catalog entry was not explicitly unavailable: %v", err)
	}
	strict := *r
	strict.safeCatalog = false
	_, resolution, err := strict.resolve(t.Context(), filepath.Join(workspace, "settings.toml"))
	if err != nil || resolution.SchemaID != "yaml-only" {
		t.Fatalf("strict mode changed catalog association: %+v %v", resolution, err)
	}
}

func TestProcessorSelectionRunJSONAttributesInputFile(t *testing.T) {
	workspace := t.TempDir()
	configPath := filepath.Join(workspace, "schepherd.toml")
	schemaPath := filepath.Join(workspace, "schema.json")
	input := filepath.Join(workspace, "input.toml")
	writeTestFile(t, schemaPath, `{"type":"object"}`)
	writeTestFile(t, input, "value = 1\n")
	config := "config_version = 1\n\n[runner]\nprocessors = [\"native\"]\n\n" +
		"[processors.native]\ntype = \"sourcemeta\"\ncommand = \"must-not-run\"\ninherit_env = false\n\n" +
		"[schemas.example]\npath = \"schema.json\"\nfile_match = [\"*.toml\"]\nprocessor = \"native\"\n"
	writeTestFile(t, configPath, config)
	stdout, stderr, code := run(t, "--config", configPath, "--workspace", workspace, "--cache-dir", filepath.Join(workspace, "cache"), "run", "--json", "--", input)
	report := decodeRunJSON(t, stdout, stderr, code)
	if code != 2 || report.Error == nil || report.Error.File != input || len(report.Error.Files) != 1 || report.Error.Files[0] != input {
		t.Fatalf("selection error lost source file: report=%+v code=%d", report, code)
	}
}

func TestRunJSONCapturesFailureAndPreservesFileReportV1(t *testing.T) {
	p := newLocalProject(t, companySection)
	config := "config_version = 1\n\n" + companySection + "\n" + consumerRunner(t, p.marker, 42)
	writeTestFile(t, p.config, config)
	legacy := p.file("report.json")
	stdout, stderr, code := run(t, p.args("run", "--json", "--report", legacy, "--", p.file("config/team.json"), p.file("config/company.json"))...)
	report := decodeRunJSON(t, stdout, stderr, code)
	if code != 42 || report.Workspace != p.ws || report.Mode != runner.ModePerFile || len(report.Tasks) != 2 {
		t.Fatalf("consumer failure = %+v, exit %d", report, code)
	}
	for i, rel := range []string{"config/team.json", "config/company.json"} {
		task := report.Tasks[i]
		if len(task.Files) != 1 || task.Files[0] != p.file(rel) || len(task.Paths) != 1 || task.Paths[0] != p.file(rel) || task.Cwd != p.ws || task.Status != runner.StatusFailed || task.ExitCode != 42 || task.Stdout == nil || task.Stdout.Text == nil || *task.Stdout.Text != "consumer stdout\n" || task.Stderr == nil || task.Stderr.Text == nil || *task.Stderr.Text != "consumer stderr\n" {
			t.Fatalf("task = %+v", task)
		}
	}
	v1 := readReport(t, legacy)
	if v1.ExitCode != 42 || len(v1.Tasks) != 2 {
		t.Fatalf("legacy report = %+v", v1)
	}
}

func TestRunJSONReportsEveryEarlyFailure(t *testing.T) {
	p := newLocalProject(t, companySection)
	badConfig := p.file("bad.toml")
	writeTestFile(t, badConfig, "config_version = 1\nunknown = true\n")
	cases := []struct {
		name string
		args []string
	}{
		{"missing inputs", p.args("run", "--json")},
		{"invalid flag before json", p.args("run", "--not-a-flag", "--json")},
		{"missing flag value", p.args("run", "--json", "--schema")},
		{"invalid configuration", []string{"--config", badConfig, "run", "--json", "--", p.file("config/team.json")}},
		{"missing input", p.args("run", "--json", "--", p.file("missing.json"))},
		{"unmatched input", p.args("run", "--json", "--", p.file("notes/readme.json"))},
		{"unknown schema", p.args("run", "--json", "--schema", "missing", "--", p.file("config/team.json"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := run(t, tc.args...)
			report := decodeRunJSON(t, stdout, stderr, code)
			if code == 0 || report.Error == nil || report.Error.Kind == "" || report.Error.Message == "" || len(report.Tasks) != 0 {
				t.Fatalf("early failure lacks classification: %+v, exit %d", report, code)
			}
		})
	}
}

func TestRunJSONPlanningFailureDoesNotInventInvocationMetadata(t *testing.T) {
	p := newLocalProject(t, companySection)
	writeTestFile(t, p.config, "config_version = 1\n\n"+companySection+"\n[runner]\ncommand = \"missing-test-consumer\"\nargs = [\"{files...}\"]\n")
	stdout, stderr, code := run(t, p.args("run", "--json", "--", p.file("config/team.json"))...)
	report := decodeRunJSON(t, stdout, stderr, code)
	if code != 7 || report.Error == nil || report.Error.Kind != "consumer-start" || len(report.Tasks) != 0 {
		t.Fatalf("planning failure fabricated an execution: %+v, exit %d", report, code)
	}
}

func TestRunJSONIsNotEnabledByInputOrFlagValue(t *testing.T) {
	p := newLocalProject(t, companySection)
	for _, args := range [][]string{
		p.args("run", "--", "--json"),
		p.args("run", "--schema", "--json", "--", p.file("config/team.json")),
	} {
		stdout, stderr, code := run(t, args...)
		if code == 0 || stdout != "" || !strings.Contains(stderr, "schepherd:") {
			t.Fatalf("input or flag value enabled machine mode: stdout %q stderr %q exit %d", stdout, stderr, code)
		}
	}
}

func TestRunJSONSkipIncludesOriginalAbsoluteInput(t *testing.T) {
	p := newLocalProject(t, companySection)
	stdout, stderr, code := run(t, p.args("run", "--json", "--ignore-unmatched", "--", p.file("notes/readme.json"))...)
	report := decodeRunJSON(t, stdout, stderr, code)
	if code != 0 || len(report.Tasks) != 0 || len(report.Skipped) != 1 || report.Skipped[0].File != p.file("notes/readme.json") || report.Skipped[0].Reason != "unmatched" || report.Skipped[0].Category != runner.SkipUnmatched {
		t.Fatalf("skip report = %+v", report)
	}
}

func TestRunJSONRetainsSkippedInputsWhenMatchingLaterFails(t *testing.T) {
	p := newLocalProject(t, companySection)
	writeTestFile(t, p.config, "config_version = 1\n\n"+companySection+"\n[schemas.other]\npath = \"../schemas/alpha.schema.json\"\nfile_match = [\"config/*.json\"]\n"+localRunner(t, p.marker))
	file := p.file("config/team.json")
	stdout, stderr, code := run(t, p.args("run", "--json", "--ignore-unmatched", "--", p.file("notes/readme.json"), file)...)
	report := decodeRunJSON(t, stdout, stderr, code)
	if code != 3 || report.Error == nil || report.Error.File != file || len(report.Skipped) != 1 || report.Skipped[0].File != p.file("notes/readme.json") {
		t.Fatalf("matching failure lost known metadata: %+v", report)
	}
}

func TestRunJSONMaterializationFailureIncludesAffectedOriginalInputs(t *testing.T) {
	p := newLocalProject(t, companySection)
	writeTestFile(t, p.schema("company.schema.json"), "not JSON")
	first, second := p.file("config/team.json"), p.file("config/company.json")
	stdout, stderr, code := run(t, p.args("run", "--json", "--ignore-unmatched", "--", p.file("notes/readme.json"), first, second)...)
	report := decodeRunJSON(t, stdout, stderr, code)
	if code != 2 || report.Error == nil || len(report.Error.Files) != 2 || report.Error.Files[0] != first || report.Error.Files[1] != second || len(report.Skipped) != 1 || len(report.Tasks) != 0 {
		t.Fatalf("materialization failure lost known metadata: %+v", report)
	}
}
