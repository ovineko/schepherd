package bundle

import (
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/env"
)

const envMarker = "SCHEPHERD_BUNDLE_TEST_MARKER"

// envProbeSource is a stand-in for the JSON Schema CLI that reports what it
// sees of its environment: for --version the pinned version only when
// GITHUB_TOKEN is absent and the rest of the environment arrived, and for
// any other command a line naming both.
const envProbeSource = `package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	token := false
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(name, "GITHUB_TOKEN") {
			token = true
		}
	}

	_, marker := os.LookupEnv("` + envMarker + `")

	if len(os.Args) > 1 && os.Args[1] == "--version" {
		if token || !marker {
			fmt.Printf("token=%t marker=%t\n", token, marker)
			return
		}

		fmt.Println("` + PinnedVersion + `")
		return
	}

	fmt.Printf("token=%t marker=%t\n", token, marker)
}
`

// envProbeTool builds envProbeSource once per test.
func envProbeTool(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(envProbeSource), 0o600); err != nil {
		t.Fatal(err)
	}

	name := "jsonschema-envprobe"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}

	bin := filepath.Join(dir, name)

	build := exec.Command("go", "build", "-o", bin, "main.go")
	build.Dir = dir
	build.Env = append(env.Environ(), "CGO_ENABLED=0", "GOFLAGS=", "GOWORK=off")

	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the environment probe: %v\n%s", err, out)
	}

	return bin
}

// TestCLIDoesNotSeeTheGitHubToken pins that the token meant for license
// detection never reaches the CLI, which reads untrusted schemas, while the
// rest of the environment does.
func TestCLIDoesNotSeeTheGitHubToken(t *testing.T) {
	bin := envProbeTool(t)

	t.Setenv(env.KeyGitHubToken, "ghs_secret-token-value")
	t.Setenv(envMarker, "1")

	tool, err := FindTool(bin, PinnedVersion)
	if err != nil {
		t.Fatalf("the version probe saw GITHUB_TOKEN or lost the environment: %v", err)
	}

	tool.addressSpace = math.MaxUint64

	ws, err := newWorkspace()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(ws.close)

	res, err := tool.run(t.Context(), ws, "bundle", "--json")
	if err != nil {
		t.Fatal(err)
	}

	out, err := os.ReadFile(res.stdout)
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.TrimSpace(string(out)); res.exit != 0 || got != "token=false marker=true" {
		t.Errorf("the CLI run reported %q (exit %d), want token=false marker=true", got, res.exit)
	}
}
