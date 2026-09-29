package gomodule

import (
	"bytes"
	"debug/buildinfo"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/buildinfo/semver"
	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/tools/release/internal/gittest"
	"github.com/ovineko/schepherd/tools/release/internal/gitx"
)

const (
	modulePath = "github.com/ovineko/schepherd"
	// publicURL is the repository the go command derives from modulePath; git
	// rewrites it to the throwaway repository, so nothing leaves the machine.
	publicURL = "https://" + modulePath
)

// TestModuleTagInstalls publishes a copy of the module in a local repository
// with a release tag and installs the client from it, as a user of the public
// module would; the installed binary reports the version of its tag.
func TestModuleTagInstalls(t *testing.T) {
	const tag = "v0.2.0-rc.1"

	repo := gittest.Init(t)
	copyModule(t, repoRoot(t), repo.Dir)
	repo.CommitAll("release")
	repo.Run("tag", tag)

	gobin := filepath.Join(t.TempDir(), "bin")
	runGo(t, t.TempDir(), moduleEnv(t, repo, gobin), "install", modulePath+"/cmd/schepherd@"+tag)
	checkInstalled(t, gobin, tag)
}

// checkInstalled checks what the go command recorded in the binary it
// installed from tag and what the binary reports.
func checkInstalled(t *testing.T, gobin, tag string) {
	t.Helper()

	binary := filepath.Join(gobin, "schepherd")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}

	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}

	v, err := semver.ParseReleaseTag(info.Main.Version)
	if err != nil || info.Main.Path != modulePath || v.Tag() != tag {
		t.Fatalf("go install recorded %s %s (%v), want %s", info.Main.Path, info.Main.Version, err, tag)
	}

	out, err := exec.CommandContext(t.Context(), binary, "version", "--json").Output()
	if err != nil {
		t.Fatalf("installed binary: %v", err)
	}

	var reported map[string]any
	if err := json.Unmarshal(out, &reported); err != nil {
		t.Fatalf("version --json of the binary installed from %s: %v\n%s", tag, err, out)
	}

	if reported["version"] != v.String() || reported["release"] != true || reported["goVersion"] == "" {
		t.Errorf("version --json of the binary installed from %s = %s, want release %s", tag, out, v)
	}

	if _, ok := reported["distributionVersion"]; ok {
		t.Errorf("version --json of the binary installed from %s has a distributionVersion: %s", tag, out)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	return root
}

// copyModule copies what `go install ./cmd/schepherd` needs from the working
// tree, so the test covers uncommitted changes as well.
func copyModule(t *testing.T, root, dst string) {
	t.Helper()

	for _, name := range []string{"go.mod", "go.sum", "LICENSE"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(dst, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, dir := range []string{"cmd", "internal"} {
		if err := os.CopyFS(filepath.Join(dst, dir), os.DirFS(filepath.Join(root, dir))); err != nil {
			t.Fatal(err)
		}
	}
}

// moduleEnv makes the go command fetch the module from the local repository
// through git (as it would from the public one) and every dependency from the
// host's module cache, without network access, into a private module cache
// and GOBIN.
func moduleEnv(t *testing.T, repo *gittest.Repo, gobin string) []string {
	t.Helper()

	hostCache := strings.TrimSpace(string(runGo(t, repo.Dir, env.Environ(), "env", "GOMODCACHE")))
	if hostCache == "" {
		t.Fatal("go env GOMODCACHE is empty")
	}

	scratch := t.TempDir()

	environ := gitx.Without(repo.Env,
		"GOFLAGS", "GOPROXY", "GONOPROXY", "GOPRIVATE", "GONOSUMDB", "GONOSUMCHECK", "GOSUMDB", "GOINSECURE",
		"GOPATH", "GOMODCACHE", "GOBIN", "GOWORK", "GOTOOLCHAIN", "GOVCS", "GOOS", "GOARCH", "CGO_ENABLED",
	)

	return append(environ,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url."+fileURL(repo.Dir)+".insteadOf",
		"GIT_CONFIG_VALUE_0="+publicURL,
		"GIT_TERMINAL_PROMPT=0",
		"GOPRIVATE="+modulePath,
		"GOPROXY="+fileURL(filepath.Join(hostCache, "cache", "download")),
		"GOSUMDB=off",
		"GOTOOLCHAIN=local",
		"GOWORK=off",
		"CGO_ENABLED=0",
		"GOFLAGS=-modcacherw -trimpath",
		"GOPATH="+filepath.Join(scratch, "gopath"),
		"GOMODCACHE="+filepath.Join(scratch, "modcache"),
		"GOBIN="+gobin,
	)
}

func fileURL(path string) string {
	slashed := filepath.ToSlash(path)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}

	return "file://" + slashed
}

func runGo(t *testing.T, dir string, environ []string, args ...string) []byte {
	t.Helper()

	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(t.Context(), "go", args...)
	cmd.Dir = dir
	cmd.Env = environ
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}

	return stdout.Bytes()
}
