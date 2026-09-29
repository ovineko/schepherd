package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func execute(t *testing.T, environ []string, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	code := run(t.Context(), args, &stdout, &stderr, environ)

	return code, stdout.String(), stderr.String()
}

func TestUsageErrors(t *testing.T) {
	cases := [][]string{
		{"version", "parse", "--nope", "0.1.0"},
		{"version", "snapshot", "extra"},
		{"packages", "build"},
		{"packages", "build", "--dist", "d", "--version", "0.1.0", "--out", "o", "--ruby", "podman"},
		{"packages", "build", "--dist", "d", "--version", "0.1.0", "--out", "o", "--kinds", "deb"},
		{"packages", "build", "--dist", "d", "--version", "0.1.0", "--out", "o", "extra"},
		{"packages", "publish-plan", "--dir", "d", "--checksums", "c", "--version", "0.1.0"},
		{"packages", "publish-plan", "--kind", "deb", "--dir", "d", "--checksums", "c", "--version", "0.1.0"},
		{"e2e-report", "--in", "x"},
		{},
		{"nope"},
		{"version"},
		{"version", "pares", "v0.1.0"},
		{"packages"},
		{"packages", "nope"},
		{"packages", "ruby-image", "extra"},
	}

	for _, args := range cases {
		if code, stdout, stderr := execute(t, nil, args...); code != 2 || stdout != "" {
			t.Errorf("%v: exit %d, want 2 (stdout %q, stderr %q)", args, code, stdout, stderr)
		}
	}

	if _, _, stderr := execute(t, nil, "version", "pares", "x"); !strings.Contains(stderr, `did you mean parse?`) {
		t.Errorf("no suggestion for a mistyped subcommand: %s", stderr)
	}

	if code, stdout, _ := execute(t, nil, "packages", "ruby-image"); code != 0 || !strings.HasPrefix(stdout, "ruby:") || !strings.Contains(stdout, "@sha256:") {
		t.Errorf("packages ruby-image: exit %d, stdout %q", code, stdout)
	}

	for _, args := range [][]string{{"--help"}, {"version", "--help"}, {"packages", "--help"}} {
		if code, stdout, _ := execute(t, nil, args...); code != 0 || !strings.Contains(stdout, "Available Commands") {
			t.Errorf("%v: exit %d, stdout %q", args, code, stdout)
		}
	}
}

func TestE2EStreamCommand(t *testing.T) {
	events := `{"Action":"run","Package":"p","Test":"TestE01_Path"}
{"Action":"output","Package":"p","Test":"TestE01_Path","Output":"=== RUN   TestE01_Path\n"}
{"Action":"pass","Package":"p","Test":"TestE01_Path","Elapsed":0.5}
plain line
`

	var stdout, stderr bytes.Buffer

	root := newRoot(nil)
	root.SetArgs([]string{"e2e-stream"})
	root.SetIn(strings.NewReader(events))
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}

	if want := "=== RUN   TestE01_Path\nplain line\ne2e-stream: 1 passed, 0 failed (0 package failures), 0 skipped\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}

	stdout.Reset()

	root = newRoot(nil)
	root.SetArgs([]string{"e2e-stream"})
	root.SetIn(strings.NewReader(`{"Action":"fail","Package":"p","Test":"TestE02_X"}` + "\n" + `{"Action":"fail","Package":"p"}` + "\n"))
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	if err := root.ExecuteContext(t.Context()); err == nil {
		t.Error("a failed test must fail e2e-stream")
	}

	if want := "e2e-stream: 0 passed, 1 failed (1 package failures), 0 skipped\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestE2EReportCommand(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "e2e.json")
	out := filepath.Join(dir, "e2e.md")

	events := `{"Action":"run","Package":"p","Test":"TestE01_Path"}
{"Action":"pass","Package":"p","Test":"TestE01_Path","Elapsed":0.5}
{"Action":"skip","Package":"p","Test":"TestE02_Offline","Elapsed":0}
`
	if err := os.WriteFile(in, []byte(events), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := execute(t, nil, "e2e-report", "--in", in, "--out", out)
	if code != 0 || !strings.Contains(stdout, "2 tests: 1 passed, 0 failed, 1 not run") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(data), "| E02 | TestE02_Offline | NOT RUN | 0.00s |") {
		t.Errorf("report:\n%s", data)
	}
}

func TestE2EReportCommandSurvivesTruncatedInput(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "e2e.json")
	out := filepath.Join(dir, "e2e.md")

	events := `{"Action":"run","Package":"p","Test":"TestE01_Path"}
{"Action":"pass","Package":"p","Test":"TestE01_Path","Elapsed":0.5}
{"Action":"output","Package":"p","Test":"TestE02_X","Outp`
	if err := os.WriteFile(in, []byte(events), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := execute(t, nil, "e2e-report", "--in", in, "--out", out)
	if code != 0 || !strings.Contains(stdout, "1 tests: 1 passed") || !strings.Contains(stderr, "skipped 1 undecodable line") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(data), "| E01 | TestE01_Path | PASS | 0.50s |") || !strings.Contains(string(data), "**Warning:**") {
		t.Errorf("report:\n%s", data)
	}
}
