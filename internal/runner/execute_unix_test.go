//go:build unix

package runner

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ovineko/schepherd/internal/fault"
)

func TestExecuteReportsSignalAs128PlusSignal(t *testing.T) {
	h := newHarness(t)
	h.behave(map[string]fakeBehavior{defaultKey: {SelfKill: true}})

	report, err := h.run([]Input{{Path: h.file("f.json", ""), SchemaID: "a"}}, h.schemas("a"), h.options(h.spec(ModeBatch, "{files...}")))

	var exitErr *fault.ConsumerExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 128+9 {
		t.Fatalf("err = %v, want ConsumerExitError 137", err)
	}

	if report.ExitCode != 137 || report.Tasks[0].Status != StatusFailed || report.Tasks[0].ExitCode != 137 {
		t.Errorf("report = %+v", report)
	}
}

func TestExecuteParallelClosesSpoolsOfFinishedTasks(t *testing.T) {
	h := newHarness(t)
	release := filepath.Join(h.root, "release")
	h.behave(map[string]fakeBehavior{
		"slow": {WaitFile: release, Stdout: []string{"S\n"}},
		"fast": {Stdout: []string{"F\n"}},
	})

	const fast = 48

	inputs := make([]Input, 0, 1+fast)
	inputs = append(inputs, Input{Path: h.file("slow.json", ""), SchemaID: "slow"})

	for i := range fast {
		inputs = append(inputs, Input{Path: h.file(fmt.Sprintf("fast-%02d.json", i), ""), SchemaID: "fast"})
	}

	spec := h.spec(ModePerFile, "{file}")
	spec.Jobs = 4
	opts := h.options(spec)

	var stdout bytes.Buffer

	opts.Stdout = &stdout

	plan, err := BuildPlan(inputs, h.schemas("slow", "fast"), opts)
	if err != nil {
		t.Fatal(err)
	}

	spoolRoot := t.TempDir()
	t.Setenv("TMPDIR", spoolRoot)

	baseline := openFDs()
	done := make(chan error, 1)

	go func() {
		_, err := Execute(t.Context(), plan, opts)
		done <- err
	}()

	deadline := time.Now().Add(time.Minute)
	for !fastConsumersExited(h, fast) {
		if time.Now().After(deadline) {
			t.Fatal("the fast consumers did not finish")
		}

		time.Sleep(20 * time.Millisecond)
	}

	limit := baseline + 4*spec.Jobs
	open := openFDs()

	for deadline := time.Now().Add(5 * time.Second); open > limit && time.Now().Before(deadline); open = openFDs() {
		time.Sleep(20 * time.Millisecond)
	}

	if open > limit {
		t.Errorf("%d descriptors open while the first task still runs (%d before the run): finished tasks hold their spool files", open, baseline)
	}

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Minute):
		t.Fatal("Execute did not return")
	}

	if want := "S\n" + strings.Repeat("F\n", fast); stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}

	if entries, err := os.ReadDir(spoolRoot); err != nil || len(entries) != 0 {
		t.Errorf("temporary directory after the run: %v (%v)", entries, err)
	}
}

func fastConsumersExited(h *harness, fast int) bool {
	recs := h.records()
	if len(recs) != fast+1 {
		return false
	}

	for _, rec := range recs {
		if rec.Key == "fast" && processAlive(rec.Pid) {
			return false
		}
	}

	return true
}

// openFDs probes descriptors instead of listing /proc or /dev/fd, which not
// every Unix provides. POSIX hands out the lowest free number, so a test
// process never holds one beyond the probed range.
func openFDs() int {
	n := 0

	for fd := range 1 << 12 {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
			n++
		}
	}

	return n
}

func TestExecuteParallelWithoutSpoolDirectory(t *testing.T) {
	h := newHarness(t)
	spec := h.spec(ModeBatch, "{files...}")
	spec.Jobs = 2
	opts := h.options(spec)

	var stdout bytes.Buffer

	opts.Stdout = &stdout

	plan, err := BuildPlan([]Input{
		{Path: h.file("a.json", ""), SchemaID: "a"},
		{Path: h.file("b.json", ""), SchemaID: "b"},
	}, h.schemas("a", "b"), opts)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("TMPDIR", filepath.Join(h.root, "missing"))

	report, err := Execute(t.Context(), plan, opts)
	wantKind(t, err, fault.Internal)

	if report.ExitCode != 1 || report.Tasks[0].Status != StatusStartError || report.Tasks[1].Status != StatusStartError {
		t.Errorf("report = %+v", report)
	}

	if n := len(h.records()); n != 0 {
		t.Errorf("%d consumers ran without a place for their output", n)
	}
}
