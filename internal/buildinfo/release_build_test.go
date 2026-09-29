package buildinfo

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type releaseBuild struct {
	ID      string   `yaml:"id"`
	Main    string   `yaml:"main"`
	Flags   []string `yaml:"flags"`
	Ldflags []string `yaml:"ldflags"`
	Env     []string `yaml:"env"`
}

// clientReleaseBuild returns the client build of the release configuration
// with its templates filled in, so the test builds exactly what a release
// with these values would.
func clientReleaseBuild(t *testing.T, root string, values map[string]string) releaseBuild {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(root, ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Builds []releaseBuild `yaml:"builds"`
	}

	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf(".goreleaser.yaml: %v", err)
	}

	i := slices.IndexFunc(doc.Builds, func(b releaseBuild) bool { return b.ID == "schepherd" })
	if i < 0 {
		t.Fatal(".goreleaser.yaml has no build with id schepherd")
	}

	b := doc.Builds[i]
	pairs := make([]string, 0, 2*len(values))

	for name, value := range values {
		pairs = append(pairs, "{{ ."+name+" }}", value)
	}

	fill := strings.NewReplacer(pairs...)

	for j, flag := range b.Ldflags {
		b.Ldflags[j] = fill.Replace(flag)
		if strings.Contains(b.Ldflags[j], "{{") {
			t.Fatalf("ldflags entry %q uses a template this test does not fill", flag)
		}
	}

	return b
}

// TestReleaseLdflags builds the client with the ldflags of the release
// configuration and checks what the binary reports, so a renamed variable or
// a version that does not reach the binary unchanged cannot ship unnoticed.
func TestReleaseLdflags(t *testing.T) {
	const (
		release    = "0.2.0-rc.1"
		fullCommit = "89abcdef0123456789abcdef0123456789abcdef"
		commitTime = "2026-09-23T08:09:10Z"
	)

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	b := clientReleaseBuild(t, root, map[string]string{"Version": release, "FullCommit": fullCommit, "CommitDate": commitTime})

	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go command is needed to build the client: %v", err)
	}

	bin := filepath.Join(t.TempDir(), "schepherd")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	args := slices.Concat([]string{"build"}, b.Flags, []string{"-buildvcs=false", "-ldflags", strings.Join(b.Ldflags, " "), "-o", bin, b.Main})
	build := exec.CommandContext(t.Context(), goBin, args...)
	build.Dir = root
	build.Env = append(build.Environ(), b.Env...)

	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	plain, err := exec.CommandContext(t.Context(), bin, "version").Output()
	if err != nil || string(plain) != release+"\n" {
		t.Fatalf("version = %q, %v; want %q", plain, err, release+"\n")
	}

	out, err := exec.CommandContext(t.Context(), bin, "version", "--json").Output()
	if err != nil {
		t.Fatalf("version --json: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("version --json is not JSON: %v\n%s", err, out)
	}

	if got["version"] != release || got["release"] != true || got["commit"] != fullCommit ||
		got["commitDate"] != commitTime || got["goVersion"] == "" {
		t.Errorf("version --json = %s", out)
	}

	for _, member := range []string{"catalogFormatVersions", "configFormatVersions"} {
		if _, ok := got[member]; !ok {
			t.Errorf("version --json lacks %s: %s", member, out)
		}
	}

	if _, ok := got["distributionVersion"]; ok || len(got) != 7 {
		t.Errorf("version --json has other members than version, commit, commitDate, goVersion, release and the format versions: %s", out)
	}
}

// TestSnapshotVersionIsNeverARelease fills the snapshot version template of
// the release configuration and checks that a binary linked with it reports
// "dev": CI and local snapshots must not be mistaken for releases.
func TestSnapshotVersionIsNeverARelease(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(root, ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Snapshot struct {
			VersionTemplate string `yaml:"version_template"`
		} `yaml:"snapshot"`
	}

	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf(".goreleaser.yaml: %v", err)
	}

	for _, short := range []string{"0123456", "89abcde", "1234567"} {
		snapshot := strings.NewReplacer("{{ .ShortCommit }}", short).Replace(doc.Snapshot.VersionTemplate)
		if snapshot == "" || strings.Contains(snapshot, "{{") {
			t.Fatalf("snapshot.version_template %q uses a template this test does not fill", doc.Snapshot.VersionTemplate)
		}

		inject(t, snapshot, "", "")

		if info := Get(); info.Version != DevVersion || info.Release {
			t.Errorf("a snapshot build linked with %q reports %+v, want a dev build", snapshot, info)
		}
	}
}
