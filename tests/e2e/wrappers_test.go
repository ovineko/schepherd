//go:build e2e

package e2e

import (
	"bytes"
	"encoding/base64"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// wrapperPlatforms maps what Python (platform.machine()) and the release
// tooling call a Linux CPU to the npm name releaseDistBinary takes and the
// CPU of the gem's libexec/schepherd-linux-<cpu> directory.
var wrapperPlatforms = map[string]struct{ npm, gemCPU string }{
	"x86_64":  {"linux-x64", "x64"},
	"aarch64": {"linux-arm64", "arm64"},
}

// wrapperArgs are passed through a launcher unchanged: spaces, an empty
// argument, quotes, shell syntax that must not be interpreted, non-ASCII.
var wrapperArgs = []string{"a b", "", `quote'"`, "$HOME", "*", "; exit 3", "ünï-日本", "--flag=x y"}

// wrapperOfflineArgs names a catalog that no cache holds; the binary exits
// with 6 without any network.
var wrapperOfflineArgs = []string{
	"--offline", "--cache-dir", "/tmp/cache", "--repository", "registry.invalid/e2e/schemas",
	"--catalog", "sha256:" + strings.Repeat("a", 64), "list",
}

// TestE41_Wrappers builds every release target with a GoReleaser snapshot
// and turns the binaries into the PyPI wheels and the RubyGems package of its
// snapshot version with `packages build`, as the release workflow's build job
// does: the wheels with the hatchling the preparation step cached for uv
// (UV_OFFLINE), the gem with gem build in the pinned Ruby image. pip resolves
// the wheel of the container's platform from a directory of all six and
// installs it from the local file into a virtual environment of the pinned
// Python image; gem install --local installs the gem, which holds all six
// binaries, into the pinned Ruby image. Both run without network. Each
// launcher runs exactly the GoReleaser binary of the container's platform,
// whose output and exit status (0 for version --json, 6 for an offline cache
// miss) they pass back unchanged. A stand-in binary that records what it
// received shows that arguments and a large binary stdin reach it unchanged
// and its exit status comes back: for the gem through SCHEPHERD_BINARY, for
// the wheel, whose launcher has no override, as the installed file itself.
func TestE41_Wrappers(t *testing.T) {
	t.Parallel()

	work := t.TempDir()
	src := filepath.Join(work, "src")

	releaseCopyWorktree(t, src)
	commit := releaseCommitSnapshot(t, src)

	hostTool(t, runOpts{Dir: src, Env: releaseGitEnv(), Timeout: 30 * time.Minute},
		".tools/bin/goreleaser", "build", "--snapshot", "--clean").ok(t)

	version := releaseSnapshotVersion(t, src, commit)
	packages := filepath.Join(work, "packages")

	hostTool(t, runOpts{Dir: src, Env: append(releaseGitEnv(), "GOWORK=off", "UV_OFFLINE=1"), Timeout: 10 * time.Minute}, "go", "run", "./tools/release",
		"packages", "build", "--dist", "dist", "--version", version, "--out", packages, "--kinds", "pypi,gem", "--ruby", "docker").ok(t)

	machine := strings.TrimSpace(string(dockerRun(t, pythonImage, dockerOpts{}, "python", "-c", "import platform; print(platform.machine())").ok(t).Stdout))

	platform, ok := wrapperPlatforms[machine]
	if !ok {
		t.Fatalf("no release target for the container CPU %q", machine)
	}

	binary := releaseDistBinary(t, src, platform.npm)

	t.Run("wheel", func(t *testing.T) {
		t.Parallel()
		wrapperCheckWheel(t, filepath.Join(packages, "pypi"), binary, machine, commit)
	})

	t.Run("gem", func(t *testing.T) {
		t.Parallel()
		wrapperCheckGem(t, filepath.Join(packages, "gem"), binary, platform.gemCPU, commit)
	})

	t.Logf("snapshot %s of commit %s: the wheel and the gem run the GoReleaser binary for linux/%s (%s)", version, commit[:7], machine, filepath.Base(filepath.Dir(binary)))
}

// wrapperCheckWheel installs the wheel with pip into a virtual environment
// of the Python image and runs its console script and python -m schepherd.
func wrapperCheckWheel(t *testing.T, wheels, binary, machine, commit string) {
	t.Helper()

	names, err := filepath.Glob(filepath.Join(wheels, "*.whl"))
	if err != nil || len(names) != 6 {
		t.Fatalf("packages build wrote %v (%v), want six wheels", names, err)
	}

	pep440 := strings.SplitN(filepath.Base(names[0]), "-", 3)[1]
	venv := t.TempDir()

	opts := dockerOpts{
		Mounts: []mount{
			{Host: wheels, Container: "/e2e/wheels"},
			{Host: venv, Container: "/e2e/venv", Writable: true},
			{Host: filepath.Dir(binary), Container: "/e2e/direct"},
		},
		Env: []string{"PIP_DISABLE_PIP_VERSION_CHECK=1", "PIP_NO_CACHE_DIR=1", "PYTHONDONTWRITEBYTECODE=1"},
	}

	// pip itself picks the wheel of this platform from all six by its tags.
	report := dockerRun(t, pythonImage, opts, "python", "-m", "pip", "install", "--dry-run", "--quiet", "--no-index",
		"--find-links", "/e2e/wheels", "--report", "-", "schepherd=="+pep440).ok(t)

	chosen := decodeJSON[struct {
		Install []struct {
			DownloadInfo struct {
				URL string `json:"url"`
			} `json:"download_info"`
		} `json:"install"`
	}](t, report.Stdout)

	if len(chosen.Install) != 1 || !regexp.MustCompile(`^file:///e2e/wheels/schepherd-[^/]+-py3-none-manylinux2014_`+machine+`\.[^/]*\.whl$`).MatchString(chosen.Install[0].DownloadInfo.URL) {
		t.Fatalf("pip chose %+v from the six wheels, want the manylinux wheel for %s", chosen.Install, machine)
	}

	chosenURL, err := url.Parse(chosen.Install[0].DownloadInfo.URL)
	if err != nil {
		t.Fatal(err)
	}

	wheel := chosenURL.Path

	dockerRun(t, pythonImage, opts, "python", "-m", "venv", "/e2e/venv").ok(t)
	dockerRun(t, pythonImage, opts, "/e2e/venv/bin/python", "-m", "pip", "install", "--no-index", "--no-deps", wheel).ok(t)

	located := strings.TrimSpace(string(dockerRun(t, pythonImage, opts, "/e2e/venv/bin/python", "-c",
		"import schepherd.main; print(schepherd.main.binary_path())").ok(t).Stdout))

	installed := wrapperHostPath(t, located, "/e2e/venv/", venv)
	if !strings.HasSuffix(located, "/site-packages/schepherd/bin/schepherd") || sha256Hex(readFile(t, installed)) != sha256Hex(readFile(t, binary)) {
		t.Fatalf("the launcher runs %s, want the GoReleaser binary %s installed in site-packages/schepherd/bin", located, binary)
	}

	launchers := map[string][]string{
		"console script":      {"/e2e/venv/bin/schepherd"},
		"python -m schepherd": {"/e2e/venv/bin/python", "-m", "schepherd"},
	}

	wrapperCompare(t, pythonImage, opts, launchers, commit)

	// The launcher has no override of its binary, so the stand-in takes the
	// place of the installed one for the last checks.
	writeExecutable(t, installed, readFile(t, binPath("testconsumer")))

	for name, prefix := range launchers {
		wrapperProbe(t, name, pythonImage, opts, prefix, nil)
	}
}

// wrapperCheckGem installs the gem with gem install --local into the Ruby
// image and runs its executable.
func wrapperCheckGem(t *testing.T, gems, binary, cpu, commit string) {
	t.Helper()

	names, err := filepath.Glob(filepath.Join(gems, "*.gem"))
	if err != nil || len(names) != 1 {
		t.Fatalf("packages build wrote %v (%v), want one gem", names, err)
	}

	home := t.TempDir()
	opts := dockerOpts{
		Mounts: []mount{
			{Host: gems, Container: "/e2e/gem"},
			{Host: home, Container: "/e2e/gems", Writable: true},
			{Host: filepath.Dir(binary), Container: "/e2e/direct"},
		},
		Env: []string{"GEM_HOME=/e2e/gems", "GEM_PATH=/e2e/gems"},
	}

	dockerRun(t, rubyImage, opts, "gem", "install", "--local", "--no-document", "--ignore-dependencies",
		"--install-dir", "/e2e/gems", "--bindir", "/e2e/gems/bin", "/e2e/gem/"+filepath.Base(names[0])).ok(t)

	located := strings.TrimSpace(string(dockerRun(t, rubyImage, opts, "ruby", "-e",
		`require "schepherd"; puts Schepherd.executable`).ok(t).Stdout))

	installed := wrapperHostPath(t, located, "/e2e/gems/", home)
	if !strings.HasSuffix(located, "/libexec/schepherd-linux-"+cpu+"/schepherd") || sha256Hex(readFile(t, installed)) != sha256Hex(readFile(t, binary)) {
		t.Fatalf("the executable runs %s, want the GoReleaser binary %s of libexec/schepherd-linux-%s", located, binary, cpu)
	}

	launchers := map[string][]string{"gem executable": {"/e2e/gems/bin/schepherd"}}

	wrapperCompare(t, rubyImage, opts, launchers, commit)

	for name, prefix := range launchers {
		wrapperProbe(t, name, rubyImage, opts, prefix, []string{"SCHEPHERD_BINARY=/e2e/bin/testconsumer"})
	}
}

// wrapperCompare runs each launcher and the binary directly, in the same
// image, and requires the same stdout, stderr and exit status: once for
// version --json of the snapshot and once for an offline cache miss.
func wrapperCompare(t *testing.T, image string, opts dockerOpts, launchers map[string][]string, commit string) {
	t.Helper()

	versionArgs := []string{"version", "--json"}

	direct := dockerRun(t, image, opts, append([]string{"/e2e/direct/schepherd"}, versionArgs...)...).ok(t)
	directMiss := dockerRun(t, image, opts, append([]string{"/e2e/direct/schepherd"}, wrapperOfflineArgs...)...).wantCode(t, 6)

	info := decodeJSON[struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
		Release bool   `json:"release"`
	}](t, direct.Stdout)
	if info.Version != "dev" || info.Release || info.Commit != commit {
		t.Fatalf("the snapshot binary reports %+v, want version dev, no release, commit %s", info, commit)
	}

	for name, prefix := range launchers {
		for _, c := range []struct {
			args []string
			want result
		}{{versionArgs, direct}, {wrapperOfflineArgs, directMiss}} {
			got := dockerRun(t, image, opts, append(slices.Clone(prefix), c.args...)...)
			if got.Code != c.want.Code || !bytes.Equal(got.Stdout, c.want.Stdout) || !bytes.Equal(got.Stderr, c.want.Stderr) {
				t.Errorf("%s does not pass the binary's outcome through\n--- launcher ---\n%s\n--- binary ---\n%s", name, got, c.want)
			}
		}
	}
}

// wrapperProbe runs a launcher whose binary is the testconsumer with
// wrapperArgs, a stdin larger than a pipe buffer holding every byte value
// and an exit status to return, and checks the record it wrote.
func wrapperProbe(t *testing.T, name, image string, base dockerOpts, prefix, env []string) {
	t.Helper()

	const status = 42

	logDir := t.TempDir()
	payload := releaseStdinPayload()

	opts := base
	opts.Mounts = append(slices.Clone(base.Mounts), binMount(), mount{Host: logDir, Container: "/e2e/log", Writable: true})
	opts.Env = slices.Concat(base.Env, env, []string{"TC_LOG_DIR=/e2e/log", "TC_READ_STDIN=1", "TC_EXIT=42"})
	opts.Stdin = payload

	if got := dockerRun(t, image, opts, append(slices.Clone(prefix), wrapperArgs...)...); got.Code != status {
		t.Errorf("%s returned exit status %d of a binary that exited with %d\n%s", name, got.Code, status, got)
	}

	records, err := filepath.Glob(filepath.Join(logDir, "*.json"))
	if err != nil || len(records) != 1 {
		t.Fatalf("%s: the binary wrote the records %v (%v), want exactly one", name, records, err)
	}

	rec := decodeJSON[struct {
		Argv  []string `json:"argv"`
		Stdin *string  `json:"stdin"`
	}](t, readFile(t, records[0]))

	if len(rec.Argv) == 0 || !slices.Equal(rec.Argv[1:], wrapperArgs) {
		t.Errorf("%s: the binary received the arguments %q, want %q", name, rec.Argv, wrapperArgs)
	}

	if rec.Stdin == nil {
		t.Fatalf("%s: the binary's record has no stdin", name)
	}

	got, err := base64.StdEncoding.DecodeString(*rec.Stdin)
	if err != nil || !bytes.Equal(got, payload) {
		t.Errorf("%s: the binary read %d bytes (sha256 %s) from stdin, want the %d-byte payload (sha256 %s): %v",
			name, len(got), sha256Hex(got), len(payload), sha256Hex(payload), err)
	}
}

// wrapperHostPath maps a path inside a container below the mount point
// prefix to the host directory mounted there.
func wrapperHostPath(t *testing.T, inside, prefix, host string) string {
	t.Helper()

	rel, ok := strings.CutPrefix(inside, prefix)
	if !ok || !filepath.IsLocal(filepath.FromSlash(rel)) {
		t.Fatalf("%q is not below %s", inside, prefix)
	}

	return filepath.Join(host, filepath.FromSlash(rel))
}

func writeExecutable(t *testing.T, path string, data []byte) {
	t.Helper()

	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestHarness_WrapperImages keeps the Python image of E41 on the base of the
// validators image.
func TestHarness_WrapperImages(t *testing.T) {
	t.Parallel()

	dockerfile := string(readFile(t, filepath.Join(suite.dir, "validators", "Dockerfile")))
	for _, from := range regexp.MustCompile(`(?m)^FROM (\S+)`).FindAllStringSubmatch(dockerfile, -1) {
		if from[1] != pythonImage {
			t.Errorf("validators/Dockerfile starts from %s, E41 installs the wheel into %s; update both together", from[1], pythonImage)
		}
	}
}
