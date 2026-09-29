package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
)

func TestReportJSON(t *testing.T) {
	h := newHarness(t)
	h.behave(map[string]fakeBehavior{"a": {Exit: 1}, defaultKey: {}})

	spec := h.spec(ModeBatch, "{files...}")
	spec.Env["SECRET"] = "s3cr3t-from-config"
	opts := h.options(spec)
	opts.Environ = append(opts.Environ, "TOKEN=t0ken-from-environment")
	a, b := h.file("a.json", ""), h.file("b.json", "")

	report, err := h.run([]Input{{Path: a, SchemaID: "a"}, {Path: b, SchemaID: "b"}}, h.schemas("a", "b"), opts)
	if fault.ExitCodeOf(err) != 1 {
		t.Fatalf("err = %v", err)
	}

	report.Skipped = append(report.Skipped, Skipped{File: filepath.Join(h.workspace, "README.md"), Reason: "unmatched"})
	path := filepath.Join(h.root, "report.json")

	if err := WriteReport(path, report); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, secret := range []string{"s3cr3t-from-config", "t0ken-from-environment", h.config} {
		if strings.Contains(string(data), secret) {
			t.Errorf("report contains environment value %q", secret)
		}
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}

	tasks, _ := got["tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("tasks = %v", got["tasks"])
	}

	for _, task := range tasks {
		task.(map[string]any)["durationMs"] = 0.0
	}

	want := map[string]any{
		"reportVersion": 1.0,
		"mode":          "batch",
		"exitCode":      1.0,
		"tasks": []any{
			map[string]any{"schemaId": "a", "schemaRef": h.schemas("a")["a"].Ref, "origin": "catalog", "files": []any{a}, "status": "failed", "exitCode": 1.0, "durationMs": 0.0},
			map[string]any{"schemaId": "b", "schemaRef": h.schemas("b")["b"].Ref, "origin": "catalog", "files": []any{b}, "status": "ok", "exitCode": 0.0, "durationMs": 0.0},
		},
		"skipped": []any{map[string]any{"file": filepath.Join(h.workspace, "README.md"), "reason": "unmatched"}},
	}

	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)

	if string(wantJSON) != string(gotJSON) {
		t.Errorf("report\n got %s\nwant %s", gotJSON, wantJSON)
	}

	if !strings.HasPrefix(string(data), "{\n  \"reportVersion\": 1,\n  \"mode\": \"batch\",\n  \"exitCode\": 1,\n") || !strings.HasSuffix(string(data), "}\n") {
		t.Errorf("report layout:\n%s", data)
	}
}

func TestWriteReportIsAtomicAndReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")

	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteReport(path, &Report{ReportVersion: ReportVersion, Mode: ModeStdin}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	want := "{\n  \"reportVersion\": 1,\n  \"mode\": \"stdin\",\n  \"exitCode\": 0,\n  \"tasks\": [],\n  \"skipped\": []\n}\n"
	if string(data) != want {
		t.Errorf("report = %q, want %q", data, want)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the report", len(entries))
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}

		if info.Mode().Perm() != 0o644 {
			t.Errorf("mode = %v, want 0644", info.Mode().Perm())
		}
	}
}

func TestWriteReportErrors(t *testing.T) {
	dir := t.TempDir()

	wantKind(t, WriteReport(filepath.Join(dir, "missing", "report.json"), &Report{}), fault.Internal)
	wantKind(t, WriteReport(filepath.Join(dir, "r.json"), nil), fault.Internal)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 0 {
		t.Errorf("failed writes left %d entries behind", len(entries))
	}
}

func TestPlanFailureReport(t *testing.T) {
	schemas := map[string]SchemaInfo{
		"a": {ID: "a", Path: "/c/a.json", Ref: "r@a", Origin: "catalog"},
		"b": {ID: "b", Path: "/w/schemas/b.json", Ref: "local:schemas/b.json", Origin: "local"},
	}
	inputs := []Input{{Path: "/w/1.json", SchemaID: "a"}, {Path: "/w/2.json", SchemaID: "b"}, {Path: "/w/3.json", SchemaID: "a"}}

	start := PlanFailureReport(inputs, schemas, ModeBatch, fault.New(fault.ConsumerStart, "no such command"))
	if start == nil || start.ExitCode != 7 || len(start.Tasks) != 2 {
		t.Fatalf("start failure report = %+v", start)
	}

	for _, task := range start.Tasks {
		if task.Status != StatusStartError || task.ExitCode != 7 {
			t.Errorf("task = %+v, want start-error with exit code 7", task)
		}
	}

	if got := start.Tasks[0].Files; len(got) != 2 || got[0] != "/w/1.json" || got[1] != "/w/3.json" {
		t.Errorf("batch grouping = %v", got)
	}

	if a, b := start.Tasks[0], start.Tasks[1]; a.Origin != "catalog" || b.Origin != "local" || b.SchemaRef != "local:schemas/b.json" {
		t.Errorf("tasks do not keep the origin and ref of their schema: %+v", start.Tasks)
	}

	usage := PlanFailureReport(inputs, schemas, ModePerFile, fault.New(fault.Usage, "bad template"))
	if usage == nil || usage.ExitCode != 2 || len(usage.Tasks) != 3 || usage.Tasks[0].Status != StatusNotStarted || usage.Tasks[0].ExitCode != 0 {
		t.Fatalf("usage failure report = %+v", usage)
	}

	if PlanFailureReport([]Input{{Path: "relative.json", SchemaID: "a"}}, schemas, ModeBatch, fault.New(fault.Usage, "x")) != nil {
		t.Error("ungroupable inputs must not produce a report")
	}
}
