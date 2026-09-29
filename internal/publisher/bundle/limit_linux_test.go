//go:build linux

package bundle

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func processLimits(t *testing.T, pid int) map[string]string {
	t.Helper()

	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/limits")
	if err != nil {
		t.Fatalf("read limits: %v", err)
	}

	limits := map[string]string{}

	for line := range strings.Lines(string(data)) {
		for _, name := range []string{"Max address space", "Max core file size"} {
			if rest, ok := strings.CutPrefix(line, name); ok {
				limits[name] = strings.Join(strings.Fields(rest)[:2], " ")
			}
		}
	}

	return limits
}

// TestCLIRunsAreBoundedInMemory first proves that startLimited caps a child
// process, and only then lets the CLI loose on file:///dev/zero, which it
// would otherwise read into memory without bound.
func TestCLIRunsAreBoundedInMemory(t *testing.T) {
	sleeper := exec.Command("sleep", "30")
	if err := startLimited(sleeper, 256<<20); err != nil {
		t.Fatalf("startLimited = %v", err)
	}

	limits := processLimits(t, sleeper.Process.Pid)

	_ = sleeper.Process.Kill()
	_ = sleeper.Wait()

	if limits["Max address space"] != "268435456 268435456" || limits["Max core file size"] != "0 0" {
		t.Fatalf("limits of the started process = %v", limits)
	}

	isolateTemp(t)

	tool := pinnedTool(t)
	tool.addressSpace = 512 << 20
	// Generous, so a loaded runner still reaches the memory limit first;
	// without the limit the run would end at this timeout and fail below.
	tool.runTimeout = time.Minute

	ws, err := newWorkspace()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(ws.close)

	file, err := ws.write("https://example.com/zero.json", []byte(`{"$schema":"`+draft7+`","$id":"https://example.com/zero.json",
		"definitions":{"x":{"$schema":"file:///dev/zero"}}}`))
	if err != nil {
		t.Fatal(err)
	}

	_, err = tool.inspect(t.Context(), ws, file, "")

	failure, ok := errors.AsType[*Failure](err)
	if !ok || failure.Reason != ReasonBundlerError || strings.Contains(failure.Detail, "did not finish") {
		t.Errorf("inspect of a runaway document = %v, want the CLI to die at the memory limit", err)
	}
}
