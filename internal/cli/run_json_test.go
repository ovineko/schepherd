package cli

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

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
	if report.ReportVersion != 2 || report.ExitCode != code || report.Tasks == nil || report.Skipped == nil {
		t.Fatalf("invalid machine report: %+v, exit %d", report, code)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("extra output after report: %v", err)
	}
	return report
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
	if code != 0 || len(report.Tasks) != 0 || len(report.Skipped) != 1 || report.Skipped[0].File != p.file("notes/readme.json") || report.Skipped[0].Reason != "unmatched" {
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
