package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/fault"
)

func TestExecuteBatchRunsOneProcessPerSchema(t *testing.T) {
	h := newHarness(t)
	h.behave(map[string]fakeBehavior{defaultKey: {Stdout: []string{"out\n"}, Stderr: []string{"err\n"}}}, keyVar, "EXTRA")
	schemas := h.schemas("a", "b")
	a1, b1, a2 := h.file("a1.json", ""), h.file("b1.json", ""), h.file("a2.json", "")

	spec := h.spec(ModeBatch, "validate", "{schema}", "{files...}")
	spec.Env["EXTRA"] = "{schema-ref}"
	opts := h.options(spec)

	var stdout, stderr bytes.Buffer

	opts.Stdout, opts.Stderr = &stdout, &stderr

	report, err := h.run([]Input{{Path: a1, SchemaID: "a"}, {Path: b1, SchemaID: "b"}, {Path: a2, SchemaID: "a"}}, schemas, opts)
	if err != nil {
		t.Fatal(err)
	}

	recs := h.records()
	if len(recs) != 2 {
		t.Fatalf("%d processes, want 2", len(recs))
	}

	for i, w := range []struct {
		id    string
		files []string
	}{{"a", []string{a1, a2}}, {"b", []string{b1}}} {
		want := slices.Concat([]string{h.exe, "validate", schemas[w.id].Path}, w.files)
		if !slices.Equal(recs[i].Args, want) {
			t.Errorf("process %d argv = %q, want %q", i, recs[i].Args, want)
		}

		if !samePath(t, recs[i].Cwd, h.workspace) || recs[i].Key != w.id || len(recs[i].Stdin) != 0 {
			t.Errorf("process %d cwd=%q key=%q stdin=%q", i, recs[i].Cwd, recs[i].Key, recs[i].Stdin)
		}

		if got := recs[i].Env["EXTRA"]; got == nil || *got != schemas[w.id].Ref {
			t.Errorf("process %d EXTRA = %v", i, got)
		}
	}

	if stdout.String() != strings.Repeat("out\n", 2) || stderr.String() != strings.Repeat("err\n", 2) {
		t.Errorf("stdout %q stderr %q", stdout.String(), stderr.String())
	}

	if report.ExitCode != 0 || len(report.Tasks) != 2 || report.Tasks[0].Status != StatusOK || report.Tasks[1].Status != StatusOK {
		t.Errorf("report = %+v", report)
	}
}

func TestExecuteStdinStreamsEachFileExactly(t *testing.T) {
	h := newHarness(t)
	contents := []string{
		"{\r\n  // comment\r\n  \"a\": 1,\r\n}\r\n",
		"{\"ü\": \"日本語 😀\"}",
		"\x00\xff\xfe raw bytes\n\n",
		"",
		"# not json at all",
	}

	inputs := make([]Input, 0, len(contents))

	for i, c := range contents {
		inputs = append(inputs, Input{Path: h.file(filepath.Join("in", string(rune('a'+i))+".json"), c), SchemaID: "a"})
	}

	report, err := h.run(inputs, h.schemas("a"), h.options(h.spec(ModeStdin, "--input", "{file}", "--schema", "{schema}")))
	if err != nil {
		t.Fatal(err)
	}

	recs := h.records()
	if len(recs) != len(contents) || len(report.Tasks) != len(contents) {
		t.Fatalf("%d processes and %d tasks, want %d", len(recs), len(report.Tasks), len(contents))
	}

	for i, rec := range recs {
		if rec.Args[2] != inputs[i].Path {
			t.Errorf("process %d got file %q, want %q", i, rec.Args[2], inputs[i].Path)
		}

		if string(rec.Stdin) != contents[i] {
			t.Errorf("process %d stdin = %q, want %q", i, rec.Stdin, contents[i])
		}
	}
}

func TestExecutePassesArgumentsLiterally(t *testing.T) {
	h := newHarness(t)
	marker := filepath.Join(h.root, "marker")
	f := h.file("-n $(touch marker2) ;x `id`.json", "")
	args := []string{
		"with space",
		`"double" 'single' \ backslash`,
		"$(touch " + marker + ")",
		"`touch " + marker + "`",
		"a;b && c | d > " + marker,
		"-leading",
		"--",
		"ünïcødé ✓ 日本",
		"*",
		"~",
		"$HOME",
		"",
		"{file}",
	}

	report, err := h.run([]Input{{Path: f, SchemaID: "a"}}, h.schemas("a"), h.options(h.spec(ModePerFile, args...)))
	if err != nil || report.ExitCode != 0 {
		t.Fatalf("err = %v report = %+v", err, report)
	}

	want := slices.Concat([]string{h.exe}, args[:len(args)-1], []string{f})

	recs := h.records()
	if len(recs) != 1 || !slices.Equal(recs[0].Args, want) {
		t.Fatalf("records = %+v, want argv %q", recs, want)
	}

	for _, p := range []string{marker, filepath.Join(h.workspace, "marker2"), filepath.Join(h.workspace, "marker")} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists: arguments reached a shell", p)
		}
	}
}

func TestExecutePassesNonzeroExitThrough(t *testing.T) {
	h := newHarness(t)
	h.behave(map[string]fakeBehavior{defaultKey: {Exit: 42}})

	report, err := h.run([]Input{{Path: h.file("f.json", ""), SchemaID: "a"}}, h.schemas("a"), h.options(h.spec(ModeBatch, "{files...}")))

	var exitErr *fault.ConsumerExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 42 {
		t.Fatalf("err = %v, want ConsumerExitError 42", err)
	}

	if fault.ExitCodeOf(err) != 42 || report.ExitCode != 42 {
		t.Errorf("exit codes %d / %d", fault.ExitCodeOf(err), report.ExitCode)
	}

	if task := report.Tasks[0]; task.Status != StatusFailed || task.ExitCode != 42 {
		t.Errorf("task = %+v", task)
	}
}

func TestExecuteFirstFailureInTaskOrderWins(t *testing.T) {
	h := newHarness(t)
	h.behave(map[string]fakeBehavior{
		"a": {SleepMs: 700, Exit: 3},
		"b": {Exit: 5},
		"c": {},
	})

	spec := h.spec(ModeBatch, "{files...}")
	spec.Jobs = 3

	report, err := h.run([]Input{
		{Path: h.file("a.json", ""), SchemaID: "a"},
		{Path: h.file("b.json", ""), SchemaID: "b"},
		{Path: h.file("c.json", ""), SchemaID: "c"},
	}, h.schemas("a", "b", "c"), h.options(spec))

	if code := fault.ExitCodeOf(err); code != 3 || report.ExitCode != 3 {
		t.Fatalf("exit code %d (report %d), want 3 from the first task", code, report.ExitCode)
	}

	got := []string{report.Tasks[0].Status, report.Tasks[1].Status, report.Tasks[2].Status}
	if !slices.Equal(got, []string{StatusFailed, StatusFailed, StatusOK}) || report.Tasks[1].ExitCode != 5 {
		t.Errorf("tasks = %+v", report.Tasks)
	}
}

func TestExecuteFailFast(t *testing.T) {
	cases := []struct {
		behaviors map[string]fakeBehavior
		name      string
		want      []string
		jobs      int
		exit      int
	}{
		{
			name:      "sequential",
			jobs:      1,
			behaviors: map[string]fakeBehavior{"a": {Exit: 2}, defaultKey: {}},
			want:      []string{StatusFailed, StatusNotStarted, StatusNotStarted},
			exit:      2,
		},
		{
			name:      "parallel lets running tasks finish",
			jobs:      2,
			behaviors: map[string]fakeBehavior{"a": {SleepMs: 600}, "b": {Exit: 4}, defaultKey: {}},
			want:      []string{StatusOK, StatusFailed, StatusNotStarted},
			exit:      4,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.behave(c.behaviors)

			spec := h.spec(ModeBatch, "{files...}")
			spec.Jobs = c.jobs
			spec.FailFast = true

			report, err := h.run([]Input{
				{Path: h.file("a.json", ""), SchemaID: "a"},
				{Path: h.file("b.json", ""), SchemaID: "b"},
				{Path: h.file("c.json", ""), SchemaID: "c"},
			}, h.schemas("a", "b", "c"), h.options(spec))

			if fault.ExitCodeOf(err) != c.exit || report.ExitCode != c.exit {
				t.Fatalf("err = %v report exit %d, want %d", err, report.ExitCode, c.exit)
			}

			got := make([]string, 0, len(report.Tasks))
			for _, task := range report.Tasks {
				got = append(got, task.Status)
			}

			if !slices.Equal(got, c.want) {
				t.Errorf("statuses %q, want %q", got, c.want)
			}

			if last := report.Tasks[2]; last.ExitCode != -1 || last.DurationMs != 0 {
				t.Errorf("not-started task = %+v", last)
			}

			if n := len(h.records()); n != slices.Index(c.want, StatusNotStarted) {
				t.Errorf("%d processes ran", n)
			}
		})
	}
}

func TestExecuteParallelOutputIsNotInterleaved(t *testing.T) {
	h := newHarness(t)
	lines := func(tag string) []string {
		out := make([]string, 0, 5)
		for i := range 5 {
			out = append(out, tag+string(rune('0'+i))+"\n")
		}

		return out
	}

	h.behave(map[string]fakeBehavior{
		"a": {Stdout: lines("A"), Stderr: lines("a"), PauseMs: 60, SleepMs: 200},
		"b": {Stdout: lines("B"), Stderr: lines("b"), PauseMs: 40},
		"c": {Stdout: lines("C"), Stderr: lines("c"), PauseMs: 30},
	})

	spec := h.spec(ModeBatch, "{files...}")
	spec.Jobs = 3
	opts := h.options(spec)

	var stdout, stderr bytes.Buffer

	opts.Stdout, opts.Stderr = &stdout, &stderr

	if _, err := h.run([]Input{
		{Path: h.file("a.json", ""), SchemaID: "a"},
		{Path: h.file("b.json", ""), SchemaID: "b"},
		{Path: h.file("c.json", ""), SchemaID: "c"},
	}, h.schemas("a", "b", "c"), opts); err != nil {
		t.Fatal(err)
	}

	if want := strings.Join(slices.Concat(lines("A"), lines("B"), lines("C")), ""); stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}

	if want := strings.Join(slices.Concat(lines("a"), lines("b"), lines("c")), ""); stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}

	recs := h.records()
	if len(recs) != 3 {
		t.Fatalf("%d processes", len(recs))
	}

	if time.Duration(recs[2].Start-recs[0].Start) > 500*time.Millisecond {
		t.Errorf("processes did not run concurrently: starts %d..%d", recs[0].Start, recs[2].Start)
	}
}

func TestExecuteWritesStraightToFiles(t *testing.T) {
	h := newHarness(t)
	h.behave(map[string]fakeBehavior{defaultKey: {Stdout: []string{"to-file\n"}}})

	out, err := os.Create(filepath.Join(h.root, "stdout.txt"))
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = out.Close() }()

	opts := h.options(h.spec(ModeBatch, "{files...}"))
	opts.Stdout = out

	if _, err := h.run([]Input{{Path: h.file("f.json", ""), SchemaID: "a"}}, h.schemas("a"), opts); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}

	if string(data) != "to-file\n" {
		t.Errorf("file content %q", data)
	}
}

func TestExecuteTimeoutKillsTheWholeGroup(t *testing.T) {
	t.Parallel()

	for _, ignoreTerm := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminate", true: "escalate to kill"}[ignoreTerm], func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			pidFile := filepath.Join(h.root, "grandchild.pid")
			h.behave(map[string]fakeBehavior{defaultKey: {Hang: true, Grandchild: pidFile, IgnoreTerm: ignoreTerm}})

			spec := h.spec(ModePerFile, "{file}")
			spec.Timeout = 2 * time.Second
			opts := h.options(spec)
			began := time.Now()

			report, err := h.run([]Input{{Path: h.file("f.json", ""), SchemaID: "a"}}, h.schemas("a"), opts)
			elapsed := time.Since(began)

			wantKind(t, err, fault.ConsumerTimeout)

			if report.ExitCode != 124 || report.Tasks[0].Status != StatusTimeout || report.Tasks[0].ExitCode != 124 {
				t.Errorf("report = %+v", report)
			}

			if ignoreTerm && runtime.GOOS != "windows" && elapsed < spec.Timeout+opts.KillGrace {
				t.Errorf("a consumer ignoring SIGTERM stopped after %s, before the grace period ended", elapsed)
			}

			if elapsed > spec.Timeout+10*time.Second {
				t.Errorf("took %s", elapsed)
			}

			waitDead(t, readPid(t, pidFile, time.Second), 5*time.Second)
		})
	}
}

func TestExecuteCancelKillsTheWholeGroup(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	pidFile := filepath.Join(h.root, "grandchild.pid")
	h.behave(map[string]fakeBehavior{"a": {Hang: true, Grandchild: pidFile}, defaultKey: {}})

	spec := h.spec(ModeBatch, "{files...}")
	opts := h.options(spec)

	plan, err := BuildPlan([]Input{
		{Path: h.file("a.json", ""), SchemaID: "a"},
		{Path: h.file("b.json", ""), SchemaID: "b"},
	}, h.schemas("a", "b"), opts)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	type result struct {
		err    error
		report *Report
	}

	done := make(chan result, 1)

	go func() {
		report, err := Execute(ctx, plan, opts)
		done <- result{err: err, report: report}
	}()

	pid := readPid(t, pidFile, 20*time.Second)

	cancel()

	var res result
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Execute did not return after cancellation")
	}

	wantKind(t, res.err, fault.Canceled)

	if res.report.ExitCode != 130 || res.report.Tasks[0].Status != StatusCanceled || res.report.Tasks[1].Status != StatusNotStarted {
		t.Errorf("report = %+v", res.report)
	}

	waitDead(t, pid, 5*time.Second)
}

func TestExecuteKillsGrandchildrenAfterNormalExit(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	pidFile := filepath.Join(h.root, "grandchild.pid")
	h.behave(map[string]fakeBehavior{defaultKey: {Grandchild: pidFile, GrandchildHoldsOutput: true, Stdout: []string{"done\n"}}})

	opts := h.options(h.spec(ModeBatch, "{files...}"))

	var stdout bytes.Buffer

	opts.Stdout = &stdout
	began := time.Now()

	report, err := h.run([]Input{{Path: h.file("f.json", ""), SchemaID: "a"}}, h.schemas("a"), opts)
	if err != nil || report.ExitCode != 0 {
		t.Fatalf("err = %v report = %+v", err, report)
	}

	if elapsed := time.Since(began); elapsed > 15*time.Second {
		t.Errorf("a grandchild holding stdout delayed the run by %s", elapsed)
	}

	if stdout.String() != "done\n" {
		t.Errorf("stdout = %q", stdout.String())
	}

	waitDead(t, readPid(t, pidFile, time.Second), 5*time.Second)
}

func TestExecuteCanceledBeforeStart(t *testing.T) {
	h := newHarness(t)
	opts := h.options(h.spec(ModeBatch, "{files...}"))

	plan, err := BuildPlan([]Input{{Path: h.file("f.json", ""), SchemaID: "a"}}, h.schemas("a"), opts)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	report, err := Execute(ctx, plan, opts)
	wantKind(t, err, fault.Canceled)

	if report.ExitCode != 130 || report.Tasks[0].Status != StatusNotStarted || len(h.records()) != 0 {
		t.Errorf("report = %+v", report)
	}
}

func TestExecuteStartErrorAtRunTime(t *testing.T) {
	h := newHarness(t)
	gone := h.file("gone.json", "{}")
	kept := h.file("kept.json", "{}")
	opts := h.options(h.spec(ModeStdin))
	opts.Spec.Jobs = 2

	plan, err := BuildPlan([]Input{{Path: kept, SchemaID: "a"}, {Path: gone, SchemaID: "a"}}, h.schemas("a"), opts)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	report, err := Execute(t.Context(), plan, opts)
	wantKind(t, err, fault.ConsumerStart)

	if report.ExitCode != 7 || report.Tasks[0].Status != StatusOK || report.Tasks[1].Status != StatusStartError || report.Tasks[1].ExitCode != 7 {
		t.Errorf("report = %+v", report)
	}
}

func TestExecuteNilPlan(t *testing.T) {
	_, err := Execute(t.Context(), nil, Options{})
	wantKind(t, err, fault.Internal)
}
