package e2ereport

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const stream = `{"Action":"start","Package":"example.test/e2e"}
{"Action":"run","Package":"example.test/e2e","Test":"TestE02_Offline"}
{"Action":"run","Package":"example.test/e2e","Test":"TestE02_Offline/warm_cache"}
{"Action":"output","Package":"example.test/e2e","Test":"TestE02_Offline/warm_cache","Output":"ok\n"}
{"Action":"pass","Package":"example.test/e2e","Test":"TestE02_Offline/warm_cache","Elapsed":0.25}
{"Action":"fail","Package":"example.test/e2e","Test":"TestE02_Offline/cold|cache","Elapsed":0.5}
{"Action":"fail","Package":"example.test/e2e","Test":"TestE02_Offline","Elapsed":0.75}
{"Action":"run","Package":"example.test/e2e","Test":"TestE10_Mirror"}
{"Action":"skip","Package":"example.test/e2e","Test":"TestE10_Mirror","Elapsed":0}
{"Action":"run","Package":"example.test/e2e","Test":"TestE01_Path"}
{"Action":"pass","Package":"example.test/e2e","Test":"TestE01_Path","Elapsed":1.5}
{"Action":"run","Package":"example.test/e2e","Test":"TestHelperNotAScenario"}
{"Action":"pass","Package":"example.test/e2e","Test":"TestHelperNotAScenario","Elapsed":0.1}
{"Action":"run","Package":"example.test/e2e","Test":"TestE03_Crash"}
not json: stray tool output
{"Action":"fail","Package":"example.test/e2e","Elapsed":3.2}
{"ImportPath":"example.test/broken","Action":"build-fail"}
`

func TestParseAndRender(t *testing.T) {
	report, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}

	tests := make([]string, 0, len(report.Rows))
	for _, row := range report.Rows {
		tests = append(tests, row.ID+" "+row.Test+" "+row.Result)
	}

	want := []string{
		"E01 TestE01_Path PASS",
		"E02 TestE02_Offline FAIL",
		"E02 TestE02_Offline/cold|cache FAIL",
		"E02 TestE02_Offline/warm_cache PASS",
		"E03 TestE03_Crash FAIL",
		"E10 TestE10_Mirror NOT RUN",
	}

	if strings.Join(tests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rows:\n%s\nwant:\n%s", strings.Join(tests, "\n"), strings.Join(want, "\n"))
	}

	pass, fail, notRun := report.Counts()
	if pass != 2 || fail != 3 || notRun != 1 {
		t.Errorf("counts = %d/%d/%d", pass, fail, notRun)
	}

	md := report.Markdown()
	for _, line := range []string{
		"| ID | Test | Result | Duration |",
		"| E01 | TestE01_Path | PASS | 1.50s |",
		`| E02 | TestE02_Offline/cold\|cache | FAIL | 0.50s |`,
		"| E03 | TestE03_Crash | FAIL (did not finish) | - |",
		"| E10 | TestE10_Mirror | NOT RUN | 0.00s |",
		"**Totals:** 6 tests: 2 passed, 3 failed, 1 not run.",
		"**Package failures:** example.test/e2e, example.test/broken (build failed)",
	} {
		if !strings.Contains(md, line) {
			t.Errorf("markdown lacks %q:\n%s", line, md)
		}
	}

	if strings.Contains(md, "TestHelperNotAScenario") {
		t.Error("non-scenario test listed")
	}
}

func TestEmptyAndInvalidInput(t *testing.T) {
	report, err := Parse(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(report.Markdown(), "No scenario tests") {
		t.Error("empty input must say that no tests were found")
	}

	truncated := `{"Action":"run","Package":"p","Test":"TestE01_Path"}
{"Action":"pass","Package":"p","Test":"TestE01_Path","Elapsed":0.5}
{"Action":"output","Package":"p","Test":"TestE02_Cancelled","Outp`

	report, err = Parse(strings.NewReader(truncated))
	if err != nil {
		t.Fatalf("a truncated last line must not lose the report: %v", err)
	}

	if len(report.Rows) != 1 || report.Rows[0].Result != ResultPass || report.SkippedLines != 1 {
		t.Errorf("rows = %+v, skipped = %d", report.Rows, report.SkippedLines)
	}

	if md := report.Markdown(); !strings.Contains(md, "| E01 | TestE01_Path | PASS |") || !strings.Contains(md, "**Warning:** 1 line(s)") {
		t.Errorf("markdown:\n%s", md)
	}
}

func FuzzParse(f *testing.F) {
	f.Add(stream)
	f.Add(`{"Action":"pass","Test":"TestE1_x"}`)

	f.Fuzz(func(t *testing.T, input string) {
		report, err := Parse(strings.NewReader(input))
		if err != nil {
			return
		}

		pass, fail, notRun := report.Counts()
		if pass+fail+notRun != len(report.Rows) {
			t.Fatalf("counts do not add up")
		}

		_ = report.Markdown()
	})
}

func TestStream(t *testing.T) {
	var out strings.Builder

	summary, err := Stream(strings.NewReader(stream), &out)
	if err != nil {
		t.Fatal(err)
	}

	if summary.Passed != 3 || summary.Failed != 2 || summary.Skipped != 1 || summary.Packages != 2 || summary.OK() {
		t.Errorf("summary = %+v", summary)
	}

	if !strings.Contains(out.String(), "ok\n") || !strings.Contains(out.String(), "not json: stray tool output\n") {
		t.Errorf("output = %q", out.String())
	}
}

func TestStreamCountsPackageFailuresSeparately(t *testing.T) {
	cases := map[string]struct {
		stream           string
		failed, packages int
	}{
		"failed test": {failed: 1, packages: 1, stream: `{"Action":"fail","Package":"p","Test":"TestE01_Path"}
{"Action":"fail","Package":"p"}
`},
		"panic outside a test": {failed: 0, packages: 1, stream: `{"Action":"output","Package":"p","Output":"panic: boom\n"}
{"Action":"fail","Package":"p"}
`},
		"build failure": {failed: 0, packages: 1, stream: `{"ImportPath":"p [p.test]","Action":"build-fail"}
{"Action":"fail","Package":"p","FailedBuild":"p [p.test]"}
`},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			summary, err := Stream(strings.NewReader(tc.stream), io.Discard)
			if err != nil {
				t.Fatal(err)
			}

			if summary.Failed != tc.failed || summary.Packages != tc.packages || summary.OK() {
				t.Errorf("summary = %+v, want %d failed tests and %d failed packages", summary, tc.failed, tc.packages)
			}
		})
	}
}

func TestRequiredScenariosAreAlwaysReported(t *testing.T) {
	report, err := Parse(strings.NewReader(stream + `{"Action":"run","Package":"example.test/e2e","Test":"TestE99_Extra"}
{"Action":"pass","Package":"example.test/e2e","Test":"TestE99_Extra","Elapsed":0.1}
`))
	if err != nil {
		t.Fatal(err)
	}

	scenarios := report.Scenarios()
	if len(scenarios) != len(RequiredScenarios)+1 {
		t.Fatalf("%d scenarios, want the %d required ones and E99", len(scenarios), len(RequiredScenarios))
	}

	results := map[string]string{}
	for _, s := range scenarios {
		results[s.ID] = s.Result
	}

	want := map[string]string{
		"E01": ResultPass, "E02": ResultFail, "E03": ResultFail, "E10": ResultNotRun,
		"E14": ResultNotRun, "E39": ResultNotRun, "E40": ResultNotRun, "E41": ResultNotRun, "E42": ResultNotRun, "E99": ResultPass,
	}
	for id, result := range want {
		if results[id] != result {
			t.Errorf("%s = %q, want %q", id, results[id], result)
		}
	}

	if last := scenarios[len(scenarios)-1]; last.ID != "E99" || last.Required {
		t.Errorf("the extra scenario is %+v, want E99 listed last as not required", last)
	}

	if pass, fail, notRun := report.ScenarioCounts(); pass != 1 || fail != 2 || notRun != len(RequiredScenarios)-3 {
		t.Errorf("scenario counts = %d/%d/%d", pass, fail, notRun)
	}

	unpassed := report.Unpassed()
	if len(unpassed) != len(RequiredScenarios)-1 || unpassed[0] != "E02 (FAIL)" || unpassed[1] != "E03 (FAIL)" || unpassed[2] != "E04 (NOT RUN)" {
		t.Errorf("unpassed = %v", unpassed)
	}

	md := report.Markdown()
	for _, line := range []string{
		"| ID | Scenario | Result | Tests |",
		"| E01 | Round trip | PASS | 1 |",
		"| E02 | Catalog only | FAIL | 3 |",
		"| E10 | Time-dependent metadata | NOT RUN | 1 |",
		"| E14 | Partial mirror failure | NOT RUN | 0 |",
		"| E39 | Dependency-only change | NOT RUN | 0 |",
		"| E40 | Local schemas | NOT RUN | 0 |",
		"| E41 | PyPI and RubyGems wrappers | NOT RUN | 0 |",
		"| E42 | Generic OCI copy | NOT RUN | 0 |",
		"| E99 | (not a required scenario) | PASS | 1 |",
		"**Scenarios:** 41 required: 1 passed, 2 failed, 38 not run.",
	} {
		if !strings.Contains(md, line) {
			t.Errorf("markdown lacks %q:\n%s", line, md)
		}
	}
}

func TestEmptyInputReportsEveryScenarioAsNotRun(t *testing.T) {
	report, err := Parse(strings.NewReader(`{"ImportPath":"p [p.test]","Action":"build-fail"}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}

	if pass, fail, notRun := report.ScenarioCounts(); pass != 0 || fail != 0 || notRun != len(RequiredScenarios) {
		t.Errorf("scenario counts = %d/%d/%d", pass, fail, notRun)
	}

	if md := report.Markdown(); !strings.Contains(md, "| E01 | Round trip | NOT RUN | 0 |") || !strings.Contains(md, "(build failed)") {
		t.Errorf("markdown:\n%s", md)
	}
}

func TestScenarioWithAnUnpassedTopLevelTestIsNotPassed(t *testing.T) {
	report, err := Parse(strings.NewReader(`{"Action":"pass","Package":"p","Test":"TestE31_RealValidator","Elapsed":1}
{"Action":"skip","Package":"p","Test":"TestE31_ShippedExamples","Elapsed":0}
{"Action":"run","Package":"p","Test":"TestE32_BackendSwap"}
{"Action":"pass","Package":"p","Test":"TestE33_Matching/sub","Elapsed":1}
{"Action":"pass","Package":"p","Test":"TestE33_Matching","Elapsed":1}
`))
	if err != nil {
		t.Fatal(err)
	}

	results := map[string]string{}
	for _, s := range report.Scenarios() {
		results[s.ID] = s.Result
	}

	if results["E31"] != ResultNotRun || results["E32"] != ResultFail || results["E33"] != ResultPass {
		t.Errorf("E31/E32/E33 = %s/%s/%s, want NOT RUN (one test skipped), FAIL (did not finish), PASS",
			results["E31"], results["E32"], results["E33"])
	}
}

// TestRequiredScenariosMatchTheSuite keeps the scenario matrix and the
// scenario tests of tests/e2e in step: every required ID has a test and no
// test uses an ID outside the matrix.
func TestRequiredScenariosMatchTheSuite(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "..", "tests", "e2e")

	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no test files in %s: %v", dir, err)
	}

	decl := regexp.MustCompile(`(?m)^func Test(E\d{2,})_[A-Za-z0-9_]+\(t \*testing\.T\)`)
	found := map[string]bool{}

	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}

		for _, m := range decl.FindAllSubmatch(data, -1) {
			found[string(m[1])] = true
		}
	}

	required := map[string]bool{}
	for _, s := range RequiredScenarios {
		required[s.ID] = true

		if !found[s.ID] {
			t.Errorf("required scenario %s (%s) has no Test%s_<Name> in %s", s.ID, s.Title, s.ID, dir)
		}
	}

	for id := range found {
		if !required[id] {
			t.Errorf("%s has scenario tests but is not in RequiredScenarios", id)
		}
	}
}
