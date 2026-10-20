package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/fault"
)

func TestCaptureSeparatesAnonymousResultsAndDoesNotForwardOutput(t *testing.T) {
	h := newHarness(t)
	h.behave(map[string]fakeBehavior{defaultKey: {Stdout: []string{"anonymous finding\n"}, Stderr: []string{"details\n"}, Exit: 42}})
	spec := h.spec(ModeBatch, "{schema}", "{files...}")
	spec.Name, spec.Type, spec.Output = "validator", "custom", "raw"
	opts := h.options(spec)
	opts.Capture = true
	var stdout, stderr bytes.Buffer
	opts.Stdout, opts.Stderr = &stdout, &stderr
	first, second := h.file("first.json", "{}"), h.file("second.json", "{}")
	report, err := h.run([]Input{{Path: first, SchemaID: "a"}, {Path: second, SchemaID: "a"}}, h.schemas("a"), opts)
	if fault.ExitCodeOf(err) != 42 || report.ExitCode != 42 || report.ReportVersion != 2 || report.Mode != ModePerFile {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("raw consumer output escaped: stdout %q stderr %q", stdout.String(), stderr.String())
	}
	if len(report.Tasks) != 2 || len(h.records()) != 2 {
		t.Fatalf("anonymous output must have one task per input: %+v", report.Tasks)
	}
	for i, file := range []string{first, second} {
		task := report.Tasks[i]
		if !reflect.DeepEqual(task.Files, []string{file}) || !reflect.DeepEqual(task.Paths, []string{file}) || task.Cwd != h.workspace {
			t.Fatalf("task attribution = %+v", task)
		}
		if task.Processor == nil || task.Processor.Name != "validator" || task.Processor.Type != "custom" || task.Processor.Output != "raw" || task.Processor.Mode != ModePerFile {
			t.Fatalf("processor = %+v", task.Processor)
		}
		if task.Status != StatusFailed || task.ExitCode != 42 || task.Stdout == nil || task.Stdout.Text == nil || *task.Stdout.Text != "anonymous finding\n" || task.Stderr == nil || task.Stderr.Text == nil || *task.Stderr.Text != "details\n" {
			t.Fatalf("captured outcome = %+v", task)
		}
	}
}

func TestCapturePreservesOriginalAndConvertedInputPaths(t *testing.T) {
	h := newHarness(t)
	original := h.file("source.toml", "x=1")
	converted := h.file("converted.json", `{"x":1}`)
	opts := h.options(h.spec(ModeStdin, "{schema}"))
	opts.Capture = true
	report, err := h.run([]Input{{Path: converted, OriginalPath: original, SchemaID: "a"}}, h.schemas("a"), opts)
	if err != nil {
		t.Fatal(err)
	}
	task := report.Tasks[0]
	if !reflect.DeepEqual(task.Files, []string{original}) || !reflect.DeepEqual(task.Paths, []string{converted}) || string(h.records()[0].Stdin) != `{"x":1}` {
		t.Fatalf("original and consumer paths were conflated: %+v", task)
	}
}

func TestCaptureBoundsRetainedStreamsAndCountsAllBytes(t *testing.T) {
	h := newHarness(t)
	payload := strings.Repeat("x", MaxStreamBytes+73)
	h.behave(map[string]fakeBehavior{defaultKey: {Stdout: []string{payload}, Stderr: []string{payload}, Exit: 3}})
	opts := h.options(h.spec(ModePerFile, "{file}"))
	opts.Capture = true
	report, err := h.run([]Input{{Path: h.file("a.json", "{}"), SchemaID: "a"}}, h.schemas("a"), opts)
	if fault.ExitCodeOf(err) != 3 || report.Tasks[0].Status != StatusFailed {
		t.Fatalf("truncated failure became clean: %+v, %v", report, err)
	}
	for _, stream := range []*StreamReport{report.Tasks[0].Stdout, report.Tasks[0].Stderr} {
		if stream == nil || stream.Bytes != int64(len(payload)) || !stream.Truncated || stream.Text == nil || len(*stream.Text) != MaxStreamBytes {
			t.Fatalf("bounded stream = %+v", stream)
		}
	}
}

func TestCaptureBinaryOutputAndAggregateBudget(t *testing.T) {
	budget := captureBudget{remaining: 7}
	first := captureStream{budget: &budget}
	second := captureStream{budget: &budget}
	if n, err := first.Write([]byte{0xff, 0, 'a', 'b', 'c'}); n != 5 || err != nil {
		t.Fatalf("first write = %d, %v", n, err)
	}
	_, _ = second.Write([]byte("12345"))
	one, two := first.report(), second.report()
	if one.Text != nil || one.Base64 == nil || *one.Base64 != base64.StdEncoding.EncodeToString([]byte{0xff, 0, 'a', 'b', 'c'}) || one.Truncated {
		t.Fatalf("binary stream = %+v", one)
	}
	if two.Bytes != 5 || !two.Truncated || two.Text == nil || *two.Text != "12" {
		t.Fatalf("aggregate limit = %+v", two)
	}
	encoded, err := json.Marshal(one)
	if err != nil || bytes.Contains(encoded, []byte("\\ufffd")) {
		t.Fatalf("binary data was lossily encoded: %s, %v", encoded, err)
	}
}

func TestCaptureInvalidUTF8AfterLimitStillUsesBase64(t *testing.T) {
	budget := captureBudget{remaining: 1}
	stream := captureStream{budget: &budget}
	_, _ = stream.Write([]byte("a"))
	_, _ = stream.Write([]byte{0xff})
	report := stream.report()
	if report.Bytes != 2 || !report.Truncated || report.Text != nil || report.Base64 == nil || *report.Base64 != "YQ==" {
		t.Fatalf("discarded non-UTF8 bytes were reported as text: %+v", report)
	}
}

func TestCaptureCanceledBeforeStartIncludesEmptyStreams(t *testing.T) {
	h := newHarness(t)
	opts := h.options(h.spec(ModeBatch, "{files...}"))
	opts.Capture = true
	plan, err := BuildPlan([]Input{{Path: h.file("a.json", "{}"), SchemaID: "a"}}, h.schemas("a"), opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	report, err := Execute(ctx, plan, opts)
	if fault.KindOf(err) != fault.Canceled || report.ExitCode != 130 || report.Tasks[0].Status != StatusNotStarted || report.Tasks[0].ExitCode != -1 || report.Tasks[0].Stdout == nil || report.Tasks[0].Stderr == nil {
		t.Fatalf("canceled report = %+v, err = %v", report, err)
	}
}

func TestCaptureStartFailureKeepsRealTaskMetadata(t *testing.T) {
	h := newHarness(t)
	opts := h.options(h.spec(ModePerFile, "{file}"))
	opts.Capture = true
	file := h.file("a.json", "{}")
	plan, err := BuildPlan([]Input{{Path: file, SchemaID: "a"}}, h.schemas("a"), opts)
	if err != nil {
		t.Fatal(err)
	}
	plan.Tasks[0].Path = filepath.Join(h.root, "missing-consumer")
	report, err := Execute(t.Context(), plan, opts)
	if fault.KindOf(err) != fault.ConsumerStart || report.Tasks[0].Status != StatusStartError || report.Tasks[0].ExitCode != 7 || report.Tasks[0].Cwd != h.workspace || !reflect.DeepEqual(report.Tasks[0].Paths, []string{file}) {
		t.Fatalf("start failure = %+v, %v", report, err)
	}
}

func TestCaptureDoesNotChangeLegacyReportShape(t *testing.T) {
	report := &Report{ReportVersion: 1, Workspace: "/not-for-v1", Mode: ModePerFile, Tasks: []TaskReport{{SchemaID: "a", Files: []string{}, Paths: []string{"/not-for-v1"}, Cwd: "/not-for-v1", Processor: &Processor{Name: "not-for-v1"}, Stdout: &StreamReport{Bytes: 1}}}}
	var out bytes.Buffer
	if err := WriteJSON(&out, report); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"workspace", "paths", "cwd", "processor", "stdout", "stderr"} {
		if strings.Contains(out.String(), `"`+field+`"`) {
			t.Fatalf("v1 unexpectedly contains %s: %s", field, out.String())
		}
	}
	decoder := json.NewDecoder(&out)
	var decoded map[string]any
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&decoded); !errors.Is(err, io.EOF) {
		t.Fatalf("report emitted multiple documents: %v", err)
	}
}

func TestCaptureUTF8AcrossWritesAndRetentionBoundary(t *testing.T) {
	for _, limit := range []int{2, 4} {
		budget := captureBudget{remaining: limit}
		stream := captureStream{budget: &budget}
		for _, chunk := range [][]byte{{0xf0}, {0x9f, 0x8c}, {0x8d}} {
			_, _ = stream.Write(chunk)
		}
		report := stream.report()
		if report.Bytes != 4 || report.Truncated != (limit == 2) {
			t.Fatalf("UTF8 stream = %+v", report)
		}
		if limit == 4 {
			if report.Text == nil || *report.Text != "\U0001f30d" || report.Base64 != nil {
				t.Fatalf("split valid UTF8 was not preserved: %+v", report)
			}
		} else if report.Base64 == nil || *report.Base64 != "8J8=" || report.Text != nil {
			t.Fatalf("retention split was not lossless: %+v", report)
		}
	}
}

func TestCaptureParallelDoesNotInterleaveTaskStreams(t *testing.T) {
	h := newHarness(t)
	h.behave(map[string]fakeBehavior{
		"a": {Stdout: []string{"first", " task"}, Stderr: []string{"first error"}, PauseMs: 5},
		"b": {Stdout: []string{"second", " task"}, Stderr: []string{"second error"}},
	})
	spec := h.spec(ModePerFile, "{file}")
	spec.Jobs = 2
	opts := h.options(spec)
	opts.Capture = true
	var stdout, stderr bytes.Buffer
	opts.Stdout, opts.Stderr = &stdout, &stderr
	report, err := h.run([]Input{{Path: h.file("a.json", "{}"), SchemaID: "a"}, {Path: h.file("b.json", "{}"), SchemaID: "b"}}, h.schemas("a", "b"), opts)
	if err != nil || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("parallel capture escaped: %v, %q, %q", err, stdout.String(), stderr.String())
	}
	for i, want := range []string{"first task", "second task"} {
		if report.Tasks[i].Stdout.Text == nil || *report.Tasks[i].Stdout.Text != want || report.Tasks[i].Stdout.Truncated {
			t.Fatalf("task %d output = %+v", i, report.Tasks[i].Stdout)
		}
	}
}

func TestCaptureFailFastKeepsNotStartedTasks(t *testing.T) {
	h := newHarness(t)
	h.behave(map[string]fakeBehavior{"a": {Exit: 17}})
	spec := h.spec(ModePerFile, "{file}")
	spec.FailFast = true
	opts := h.options(spec)
	opts.Capture = true
	report, err := h.run([]Input{{Path: h.file("a.json", "{}"), SchemaID: "a"}, {Path: h.file("b.json", "{}"), SchemaID: "b"}}, h.schemas("a", "b"), opts)
	if fault.ExitCodeOf(err) != 17 || len(h.records()) != 1 || report.Tasks[1].Status != StatusNotStarted || report.Tasks[1].ExitCode != -1 || report.Tasks[1].Stdout == nil || report.Tasks[1].Stdout.Bytes != 0 {
		t.Fatalf("fail-fast capture = %+v, %v", report, err)
	}
}

func TestCaptureSuccessfulProcessWithPipeFailureIsNotOK(t *testing.T) {
	h := newHarness(t)
	cmd := exec.CommandContext(t.Context(), h.exe)
	cmd.Dir = h.workspace
	cmd.Env = h.options(h.spec(ModePerFile, "{file}")).Environ
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	x := &executor{capture: true}
	outcome := x.classify(&Task{SchemaID: "a"}, cmd.ProcessState, exec.ErrWaitDelay, stopNone, time.Millisecond)
	if outcome.status != StatusFailed || outcome.exitCode != 1 || fault.KindOf(outcome.err) != fault.Internal {
		t.Fatalf("pipe failure became clean: %+v", outcome)
	}
}

func TestCaptureAggregateRetentionDoesNotDependOnCompletionOrder(t *testing.T) {
	plan := &Plan{Mode: ModePerFile, Tasks: make([]Task, 20)}
	x := newExecutor(plan, Options{Capture: true})
	payload := []byte(strings.Repeat("x", MaxStreamBytes+1))
	retained := 0
	for i := len(x.streams) - 1; i >= 0; i-- {
		for _, stream := range []*captureStream{&x.streams[i].stdout, &x.streams[i].stderr} {
			_, _ = stream.Write(payload)
			report := stream.report()
			if report.Text == nil || len(*report.Text) != MaxCaptureBytes/40 || report.Bytes != int64(len(payload)) || !report.Truncated {
				t.Fatalf("task %d got a completion-order-dependent budget: %+v", i, report)
			}
			retained += len(*report.Text)
		}
	}
	if retained > MaxCaptureBytes {
		t.Fatalf("aggregate retained %d bytes", retained)
	}
}
