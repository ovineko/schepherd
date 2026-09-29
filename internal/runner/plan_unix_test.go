//go:build unix

package runner

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
)

func (h *harness) link(path string) string {
	h.t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}

	if err := os.Symlink(h.exe, path); err != nil {
		h.t.Fatal(err)
	}

	return path
}

func TestResolveBareNameInConsumerPath(t *testing.T) {
	h := newHarness(t)
	bin := filepath.Join(h.root, "bin")
	want := h.link(filepath.Join(bin, "fake-consumer"))
	h.link(filepath.Join(h.workspace, "fake-consumer"))
	h.link(filepath.Join(h.workspace, "rel", "fake-consumer"))

	if err := os.MkdirAll(filepath.Join(h.root, "nox"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(h.root, "nox", "fake-consumer"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := h.file("in.json", "{}")

	spec := h.spec(ModeBatch, "{files...}")
	spec.Command = "fake-consumer"
	spec.InheritEnv = false
	spec.Env[fakeVar] = h.config
	spec.Env["GORACE"] = strings.TrimPrefix(raceNoExitDelay, "GORACE=")
	spec.Env["PATH"] = strings.Join([]string{"", ".", "rel", filepath.Join(h.root, "nox"), bin}, string(os.PathListSeparator))
	opts := h.options(spec)
	opts.Environ = []string{"PATH=" + filepath.Join(h.workspace, "rel")}

	report, err := h.run([]Input{{Path: f, SchemaID: "a"}}, h.schemas("a"), opts)
	if err != nil {
		t.Fatal(err)
	}

	if report.ExitCode != 0 {
		t.Fatalf("exit code %d", report.ExitCode)
	}

	recs := h.records()
	if len(recs) != 1 || !slices.Equal(recs[0].Args, []string{"fake-consumer", f}) {
		t.Fatalf("records = %+v", recs)
	}

	plan, err := BuildPlan([]Input{{Path: f, SchemaID: "a"}}, h.schemas("a"), opts)
	if err != nil {
		t.Fatal(err)
	}

	if plan.Tasks[0].Path != want {
		t.Errorf("Path = %q, want %q (empty, relative and non-executable PATH entries must be skipped)", plan.Tasks[0].Path, want)
	}

	opts.Spec.Env["PATH"] = strings.Join([]string{"", ".", "rel"}, string(os.PathListSeparator))
	_, err = BuildPlan([]Input{{Path: f, SchemaID: "a"}}, h.schemas("a"), opts)
	wantKind(t, err, fault.ConsumerStart)

	opts.Spec.InheritEnv = true
	opts.Environ = []string{"PATH=" + bin}
	delete(opts.Spec.Env, "PATH")

	plan, err = BuildPlan([]Input{{Path: f, SchemaID: "a"}}, h.schemas("a"), opts)
	if err != nil {
		t.Fatal(err)
	}

	if plan.Tasks[0].Path != want {
		t.Errorf("inherited PATH: Path = %q, want %q", plan.Tasks[0].Path, want)
	}
}

func TestResolveRelativeExecutableAgainstCwd(t *testing.T) {
	h := newHarness(t)
	work := filepath.Join(h.root, "work")
	want := h.link(filepath.Join(work, "tools", "validate"))
	f := h.file("in.json", "{}")

	spec := h.spec(ModePerFile, "{file}")
	spec.Command = "./tools/validate"
	spec.Cwd = work

	opts := h.options(spec)

	plan, err := BuildPlan([]Input{{Path: f, SchemaID: "a"}}, h.schemas("a"), opts)
	if err != nil {
		t.Fatal(err)
	}

	if plan.Tasks[0].Path != want || plan.Tasks[0].Args[0] != "./tools/validate" {
		t.Fatalf("Path = %q Args = %q, want %q", plan.Tasks[0].Path, plan.Tasks[0].Args, want)
	}

	if _, err := Execute(t.Context(), plan, opts); err != nil {
		t.Fatal(err)
	}

	recs := h.records()
	if len(recs) != 1 || !samePath(t, recs[0].Cwd, work) {
		t.Fatalf("records = %+v, want one run in %s", recs, work)
	}
}

func TestResolveRejectsNonExecutable(t *testing.T) {
	h := newHarness(t)
	script := filepath.Join(h.root, "not-executable")

	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := h.spec(ModeBatch, "{files...}")
	spec.Command = script

	_, err := BuildPlan([]Input{{Path: h.file("in.json", ""), SchemaID: "a"}}, h.schemas("a"), h.options(spec))
	wantKind(t, err, fault.ConsumerStart)
}

func TestResolveUsesThisUsersExecutePermission(t *testing.T) {
	h := newHarness(t)
	shadow := filepath.Join(h.root, "shadow", "fake-consumer")
	target := h.link(filepath.Join(h.root, "bin", "fake-consumer"))

	if err := os.MkdirAll(filepath.Dir(shadow), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(shadow, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Only the group may execute it, and the owner class decides for the
	// owner. Root may still run it, so exec itself is the oracle.
	if err := os.Chmod(shadow, 0o610); err != nil {
		t.Fatal(err)
	}

	runnable := !errors.Is(exec.Command(shadow).Run(), fs.ErrPermission)
	f := h.file("in.json", "{}")

	spec := h.spec(ModeBatch, "{files...}")
	spec.Command = "fake-consumer"
	spec.Env["PATH"] = filepath.Dir(shadow) + string(os.PathListSeparator) + filepath.Dir(target)

	plan, err := BuildPlan([]Input{{Path: f, SchemaID: "a"}}, h.schemas("a"), h.options(spec))
	if err != nil {
		t.Fatal(err)
	}

	want := target
	if runnable {
		want = shadow
	}

	if plan.Tasks[0].Path != want {
		t.Errorf("Path = %q, want %q (exec permitted: %v)", plan.Tasks[0].Path, want, runnable)
	}

	spec.Command = shadow

	_, err = BuildPlan([]Input{{Path: f, SchemaID: "a"}}, h.schemas("a"), h.options(spec))

	switch {
	case runnable && err != nil:
		t.Errorf("BuildPlan rejected a command exec accepts: %v", err)
	case !runnable:
		wantKind(t, err, fault.ConsumerStart)
	}
}

func TestArgvCostCountsBytesAndEnvironment(t *testing.T) {
	if got := argBytes("ü b"); got != len("ü b")+1 {
		t.Errorf("argBytes = %d", got)
	}

	if got := envBytes([]string{"A=1", "BB=22"}); got != 4+6 {
		t.Errorf("envBytes = %d", got)
	}
}
