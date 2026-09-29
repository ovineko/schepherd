package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/tools/release/internal/e2ereport"
)

// streamE2E runs e2e-stream with events on stdin and returns its exit code
// and output; stderr ends with the error as run would print it.
func streamE2E(t *testing.T, events string, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	root := newRoot(nil)
	root.SetArgs(append([]string{"e2e-stream"}, args...))
	root.SetIn(strings.NewReader(events))
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	err := root.ExecuteContext(t.Context())
	if err != nil {
		stderr.WriteString("release: " + err.Error() + "\n")
	}

	return fault.ExitCodeOf(err), stdout.String(), stderr.String()
}

const failingE2ERun = `{"Action":"run","Package":"p","Test":"TestE01_RoundTrip"}
{"Action":"pass","Package":"p","Test":"TestE01_RoundTrip","Elapsed":0.5}
{"Action":"run","Package":"p","Test":"TestE02_CatalogOnly"}
{"Action":"output","Package":"p","Test":"TestE02_CatalogOnly","Output":"--- FAIL: TestE02_CatalogOnly\n"}
{"Action":"fail","Package":"p","Test":"TestE02_CatalogOnly","Elapsed":0.25}
{"Action":"fail","Package":"p","Elapsed":1}
`

func TestE2EStreamWritesTheReportOfAFailedRun(t *testing.T) {
	dir := t.TempDir()
	save, report := filepath.Join(dir, "e2e.json"), filepath.Join(dir, "e2e-report.md")

	code, stdout, stderr := streamE2E(t, failingE2ERun, "--save", save, "--report", report)
	if code != 1 || !strings.Contains(stderr, "tests failed") {
		t.Fatalf("a failed run must still fail e2e-stream: exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	if !strings.Contains(stdout, "--- FAIL: TestE02_CatalogOnly\n") ||
		!strings.Contains(stdout, "2 tests: 1 passed, 1 failed, 0 not run; 41 required scenarios: 1 passed, 1 failed, 39 not run; report: "+report) {
		t.Errorf("stdout:\n%s", stdout)
	}

	if got, err := os.ReadFile(save); err != nil || string(got) != failingE2ERun {
		t.Errorf("--save holds %q (%v), want the raw stream", got, err)
	}

	md, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("no report after a failed run: %v", err)
	}

	for _, line := range []string{
		"| E01 | Round trip | PASS | 1 |",
		"| E02 | Catalog only | FAIL | 1 |",
		"| E03 | Only the requested schema | NOT RUN | 0 |",
		"| E39 | Dependency-only change | NOT RUN | 0 |",
		"| E40 | Local schemas | NOT RUN | 0 |",
		"| E41 | PyPI and RubyGems wrappers | NOT RUN | 0 |",
		"**Package failures:** p",
	} {
		if !strings.Contains(string(md), line) {
			t.Errorf("report lacks %q:\n%s", line, md)
		}
	}
}

func TestE2EStreamWritesTheReportOfATruncatedRun(t *testing.T) {
	report := filepath.Join(t.TempDir(), "e2e-report.md")

	code, _, _ := streamE2E(t, `{"Action":"run","Package":"p","Test":"TestE01_RoundTrip"}
{"Action":"output","Package":"p","Test":"TestE01_RoundTrip","Outp`, "--report", report)
	if code != 0 {
		t.Errorf("exit %d: an interrupted stream without failures is not a test failure by itself", code)
	}

	if md, err := os.ReadFile(report); err != nil || !strings.Contains(string(md), "| E01 | Round trip | FAIL | 1 |") {
		t.Errorf("report (%v):\n%s", err, md)
	}
}

func TestE2EStrictNeedsEveryRequiredScenario(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "e2e-report.md")
	passing := `{"Action":"pass","Package":"p","Test":"TestE01_RoundTrip","Elapsed":0.5}` + "\n"

	if code, stdout, stderr := streamE2E(t, passing, "--report", report); code != 0 {
		t.Fatalf("without --strict a passing run succeeds: exit %d\n%s\n%s", code, stdout, stderr)
	}

	missing := fmt.Sprintf("%d required scenario(s) did not pass: E02 (NOT RUN), E03 (NOT RUN)", len(e2ereport.RequiredScenarios)-1)

	code, _, stderr := streamE2E(t, passing, "--report", report, "--strict")
	if code != 1 || !strings.Contains(stderr, missing) {
		t.Errorf("--strict with missing scenarios: exit %d, stderr %q", code, stderr)
	}

	if _, err := os.Stat(report); err != nil {
		t.Errorf("--strict must still write the report: %v", err)
	}

	if code, _, stderr := streamE2E(t, passing, "--strict"); code != 2 || !strings.Contains(stderr, "--strict needs --report") {
		t.Errorf("--strict without --report: exit %d, stderr %q", code, stderr)
	}

	in := filepath.Join(dir, "e2e.json")
	if err := os.WriteFile(in, []byte(passing), 0o600); err != nil {
		t.Fatal(err)
	}

	if code, _, _ := execute(t, nil, "e2e-report", "--in", in, "--out", report); code != 0 {
		t.Errorf("e2e-report without --strict: exit %d", code)
	}

	last := e2ereport.RequiredScenarios[len(e2ereport.RequiredScenarios)-1].ID
	if code, _, stderr := execute(t, nil, "e2e-report", "--in", in, "--out", report, "--strict"); code != 1 || !strings.Contains(stderr, last+" (NOT RUN)") {
		t.Errorf("e2e-report --strict: exit %d, stderr %q", code, stderr)
	}
}

// TestE2EStreamSurvivesALineTooLongToParse keeps the report parser, which
// reads the stream while it arrives, from blocking the stream when it stops
// early.
func TestE2EStreamSurvivesALineTooLongToParse(t *testing.T) {
	report := filepath.Join(t.TempDir(), "e2e-report.md")
	events := `{"Action":"pass","Package":"p","Test":"TestE01_RoundTrip","Elapsed":0.5}` + "\n" +
		`{"Output":"` + strings.Repeat("x", 17<<20) + `"}` + "\n"

	if code, _, stderr := streamE2E(t, events, "--report", report); code == 0 || !strings.Contains(stderr, "too long") {
		t.Errorf("exit %d, stderr %.200q", code, stderr)
	}
}
