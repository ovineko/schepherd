package bundle

import (
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/env"
)

// garbledInspectSource stands in for the JSON Schema CLI: it reports the
// pinned version, and for inspect it exits 0 with a report that is not the
// JSON object the publisher reads.
const garbledInspectSource = `package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("` + PinnedVersion + `")
		return
	}

	fmt.Print("{\"locations\": {\"static\": [1, 2]}}")
}
`

func buildFakeCLI(t *testing.T, source string) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dir, "jsonschema-fake")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	build := exec.Command("go", "build", "-o", bin, "main.go")
	build.Dir = dir
	build.Env = append(env.Environ(), "CGO_ENABLED=0", "GOFLAGS=", "GOWORK=off")

	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the fake CLI: %v\n%s", err, out)
	}

	return bin
}

// TestUnreadableInspectReportRejectsTheDocument pins that an inspect report
// the publisher cannot read rejects that one document, so preparation holds
// or leaves out its record instead of failing the weekly run.
func TestUnreadableInspectReportRejectsTheDocument(t *testing.T) {
	tool, err := FindTool(buildFakeCLI(t, garbledInspectSource), PinnedVersion)
	if err != nil {
		t.Fatal(err)
	}

	tool.addressSpace = math.MaxUint64

	file := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(file, []byte(`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = inspectFile(t, tool, file)

	failure, ok := errors.AsType[*Failure](err)
	if !ok || failure.Reason != ReasonBundlerError || !strings.Contains(failure.Detail, "inspect report for "+file) {
		t.Fatalf("inspect = %v, want a %s rejection naming the document", err, ReasonBundlerError)
	}
}

// inspectFile runs the CLI's inspect on file as Resolve does for every
// document of a closure and returns the references that leave the file.
func inspectFile(t *testing.T, tool *Tool, file string) ([]Ref, error) {
	t.Helper()

	abs, err := filepath.Abs(file)
	if err != nil {
		t.Fatal(err)
	}

	ws, err := newWorkspace()
	if err != nil {
		t.Fatal(err)
	}

	defer ws.close()

	ws.files[abs] = file

	in, err := tool.inspect(t.Context(), ws, abs, "")
	if err != nil {
		return nil, err
	}

	return in.external(), nil
}
