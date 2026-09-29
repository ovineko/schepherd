package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/buildinfo/semver"
	"github.com/ovineko/schepherd/tools/release/internal/gittest"
)

// writeGoMod writes a go.mod declaring module path and returns its path.
func writeGoMod(t *testing.T, path string) string {
	t.Helper()

	name := filepath.Join(t.TempDir(), "go.mod")
	if err := os.WriteFile(name, []byte("module "+path+"\n\ngo 1.27.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	return name
}

const modulePath = "github.com/ovineko/schepherd"

func TestVersionParse(t *testing.T) {
	goMod := writeGoMod(t, modulePath)

	cases := map[string]versionInfo{
		"0.1.0": {
			Version: "0.1.0", Tag: "v0.1.0", NpmDistTag: "latest", PypiVersion: "0.1.0", GemVersion: "0.1.0",
		},
		"v0.1.0": {
			Version: "0.1.0", Tag: "v0.1.0", NpmDistTag: "latest", PypiVersion: "0.1.0", GemVersion: "0.1.0",
		},
		"1.2.3": {
			Version: "1.2.3", Tag: "v1.2.3", NpmDistTag: "latest", PypiVersion: "1.2.3", GemVersion: "1.2.3",
		},
		"v0.2.0-rc.1": {
			Version: "0.2.0-rc.1", Tag: "v0.2.0-rc.1", NpmDistTag: "next", PypiVersion: "0.2.0rc1", GemVersion: "0.2.0.rc.1", Prerelease: true,
		},
		"1.0.0-beta.2": {
			Version: "1.0.0-beta.2", Tag: "v1.0.0-beta.2", NpmDistTag: "next", PypiVersion: "1.0.0b2", GemVersion: "1.0.0.beta.2", Prerelease: true,
		},
		"0.3.0-alpha.10": {
			Version: "0.3.0-alpha.10", Tag: "v0.3.0-alpha.10", NpmDistTag: "next", PypiVersion: "0.3.0a10", GemVersion: "0.3.0.alpha.10", Prerelease: true,
		},
	}

	for input, want := range cases {
		code, stdout, stderr := execute(t, nil, "version", "parse", "--go-mod", goMod, input)
		if code != 0 {
			t.Fatalf("parse %s: exit %d: %s", input, code, stderr)
		}

		var got versionInfo
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatal(err)
		}

		if got != want {
			t.Errorf("parse %s = %+v, want %+v", input, got, want)
		}
	}

	for _, input := range []string{
		"", "0.1", "v0.1", "01.0.0", "0.1.0+build", "v0.1.0+dirty", "0.0.0", "0.0.0-snapshot-abc1234",
		"v0.1.1-0.20260925080000-0123456789ab", "20260923.1", "0.20260923.01", "catalog-20260924.0905", "V0.1.0",
	} {
		if code, stdout, stderr := execute(t, nil, "version", "parse", "--go-mod", goMod, input); code != 1 || stdout != "" || stderr == "" {
			t.Errorf("parse %q: exit %d, stdout %q, stderr %q", input, code, stdout, stderr)
		}
	}

	// Pre-releases that PyPI or RubyGems cannot express in SemVer order.
	for input, registry := range map[string]string{
		"0.2.0-preview.1": "PEP 440", "1.0.0-beta": "PEP 440", "0.2.0-rc1": "PEP 440", "0.2.0-RC.1": "PEP 440",
		"0.2.0-rc.1.2": "PEP 440", "0.2.0-rc.0": "RubyGems",
	} {
		code, stdout, stderr := execute(t, nil, "version", "parse", "--go-mod", goMod, input)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "has no "+registry+" form") {
			t.Errorf("parse %q: exit %d, stdout %q, stderr %q, want a refusal for %s", input, code, stdout, stderr, registry)
		}
	}

	if code, _, _ := execute(t, nil, "version", "parse"); code != 2 {
		t.Errorf("missing argument: exit %d, want 2", code)
	}

	if code, stdout, stderr := execute(t, nil, "version", "parse", "--go-mod", filepath.Join(t.TempDir(), "go.mod"), "0.1.0"); code != 1 || stdout != "" || !strings.Contains(stderr, "module path") {
		t.Errorf("without a go.mod: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

// TestVersionParseRefusesMajorVersionsTheModuleCannotServe covers the Go
// module channel of a release: the tag vX.Y.Z is also the module version,
// and the go command needs the module path suffix /vX from major version 2
// on.
func TestVersionParseRefusesMajorVersionsTheModuleCannotServe(t *testing.T) {
	plain := writeGoMod(t, modulePath)

	for _, input := range []string{"v2.0.0", "2.0.0-rc.1", "v10.0.0"} {
		code, stdout, stderr := execute(t, nil, "version", "parse", "--go-mod", plain, input)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "has no /v") || !strings.Contains(stderr, modulePath+"/v") {
			t.Errorf("parse %s under %s: exit %d, stdout %q, stderr %q", input, modulePath, code, stdout, stderr)
		}
	}

	v2 := writeGoMod(t, modulePath+"/v2")

	if code, stdout, stderr := execute(t, nil, "version", "parse", "--go-mod", v2, "v2.0.0"); code != 0 || !strings.Contains(stdout, `"tag":"v2.0.0"`) {
		t.Errorf("parse v2.0.0 under %s/v2: exit %d, stdout %q, stderr %q", modulePath, code, stdout, stderr)
	}

	for _, input := range []string{"v1.0.0", "v3.0.0"} {
		if code, _, stderr := execute(t, nil, "version", "parse", "--go-mod", v2, input); code != 1 || !strings.Contains(stderr, "only serves major version 2") {
			t.Errorf("parse %s under %s/v2: exit %d, stderr %q", input, modulePath, code, stderr)
		}
	}
}

func TestVerifyTag(t *testing.T) {
	existing := []string{"v0.1.0", "v0.1.1", "v0.2.0-rc.1", "v0.3.0-beta.1", "catalog-20260924.0905", "not-a-version", "v0.20260923.01", ""}

	accepted := map[string]string{
		"first release":                  "v0.1.0",
		"patch after the newest release": "v0.1.2",
		"release of a pre-release":       "v0.2.0",
		"next pre-release":               "v0.2.0-rc.2",
		"pre-release of the next minor":  "v0.4.0-alpha",
		"lower than a pre-release":       "v0.1.5",
		"major":                          "v1.0.0",
	}

	for name, tag := range accepted {
		others := existing
		if name == "first release" {
			others = []string{"catalog-20260924.0905", tag}
		}

		v, err := verifyTag(tag, others)
		if err != nil || v.Tag() != tag {
			t.Errorf("%s: verifyTag(%s) = %q, %v", name, tag, v.Tag(), err)
		}
	}

	rejected := map[string]struct{ tag, want string }{
		"older than the newest release":             {"v0.1.0-rc.9", "older than the existing release v0.1.1"},
		"release below the newest release":          {"v0.0.9", "older than the existing release v0.1.1"},
		"invalid syntax":                            {"v0.1.02", "leading zero"},
		"build metadata":                            {"v0.1.2+meta", "build metadata"},
		"snapshot version":                          {"v0.0.0-snapshot-0123456", "0.0.0 is reserved"},
		"catalog tag":                               {"catalog-20260924.1432", "must start with"},
		"pseudo-version":                            {"v0.1.2-0.20260925080000-0123456789ab", "pseudo-version"},
		"same version as a tag with build metadata": {"v0.5.0", "duplicates the version of the existing tag v0.5.0+x"},
	}

	for name, tc := range rejected {
		others := existing
		if name == "same version as a tag with build metadata" {
			others = append(others, "v0.5.0+x")
		}

		if _, err := verifyTag(tc.tag, others); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: verifyTag(%s) error = %v, want it to mention %q", name, tc.tag, err, tc.want)
		}
	}
}

// writeRelease writes the go.mod of the client module and a file that
// changes with every call, so each commit that may be tagged differs.
func writeRelease(repo *gittest.Repo, tags ...string) {
	repo.T.Helper()

	repo.Write("go.mod", "module "+modulePath+"\n\ngo 1.27.1\n", 0o644)
	repo.Write("CHANGES.txt", strings.Join(tags, " ")+"\n", 0o644)
}

func TestVersionVerifyTagCommand(t *testing.T) {
	repo := gittest.Init(t)
	writeRelease(repo, "v0.1.0")
	repo.CommitAll("initial")

	repo.Run("tag", "v0.1.0")
	repo.Run("tag", "catalog-20260924.0905")

	writeRelease(repo, "v0.1.1", "v0.2.0-rc.1")
	repo.CommitAll("next")

	for _, tag := range []string{"v0.1.1", "v0.2.0-rc.1", "catalog-20260924.1432"} {
		repo.Run("tag", tag)
	}

	code, stdout, stderr := execute(t, repo.Env, "version", "verify-tag", "--repo", repo.Dir, "--base", "main", "v0.1.1")
	if code != 0 || stdout != "0.1.1\n" {
		t.Errorf("valid tag: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	code, stdout, stderr = execute(t, repo.Env, "version", "verify-tag", "--repo", repo.Dir, "--base", "main", "--json", "v0.2.0-rc.1")

	var info versionInfo
	if err := json.Unmarshal([]byte(stdout), &info); code != 0 || err != nil ||
		info != (versionInfo{
			Version: "0.2.0-rc.1", Tag: "v0.2.0-rc.1", NpmDistTag: "next", PypiVersion: "0.2.0rc1", GemVersion: "0.2.0.rc.1",
			PreviousTag: "v0.1.1", Prerelease: true,
		}) {
		t.Errorf("pre-release --json: exit %d, stdout %q (%v), stderr %q", code, stdout, err, stderr)
	}

	// The first release, in a repository without any earlier release tag, as
	// the public repository will be for v0.1.0.
	fresh := gittest.Init(t)
	writeRelease(fresh, "v0.1.0")
	fresh.CommitAll("initial public commit")
	fresh.Run("tag", "v0.1.0")

	code, stdout, stderr = execute(t, fresh.Env, "version", "verify-tag", "--repo", fresh.Dir, "--base", "main", "--json", "v0.1.0")

	info = versionInfo{}
	if err := json.Unmarshal([]byte(stdout), &info); code != 0 || err != nil ||
		info != (versionInfo{
			Version: "0.1.0", Tag: "v0.1.0", NpmDistTag: "latest", PypiVersion: "0.1.0", GemVersion: "0.1.0",
		}) {
		t.Errorf("first release: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	// A tag that passes every other gate, but whose pre-release has no PyPI
	// form, fails before the release builds anything for it.
	writeRelease(repo, "v0.3.0-preview.1")
	repo.CommitAll("a preview")
	repo.Run("tag", "v0.3.0-preview.1")

	for _, args := range [][]string{{"v0.3.0-preview.1"}, {"--json", "v0.3.0-preview.1"}} {
		code, stdout, stderr = execute(t, repo.Env, append([]string{"version", "verify-tag", "--repo", repo.Dir, "--base", "main"}, args...)...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "cannot be published to PyPI") || !strings.Contains(stderr, "-rc.N") {
			t.Errorf("verify-tag %v: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}

	repo.Run("tag", "v0.1.0-rc.1")

	code, stdout, stderr = execute(t, repo.Env, "version", "verify-tag", "--repo", repo.Dir, "--base", "main", "v0.1.0-rc.1")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "older than the existing release v0.1.1") {
		t.Errorf("tag below the newest release: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	code, _, stderr = execute(t, repo.Env, "version", "verify-tag", "--repo", repo.Dir, "--base", "main", "v0.1.2")
	if code != 1 || !strings.Contains(stderr, "not a tag of this repository") {
		t.Errorf("missing tag: exit %d, stderr %q", code, stderr)
	}

	code, _, stderr = execute(t, repo.Env, "version", "verify-tag", "--repo", repo.Dir, "v0.1.1")
	if code != 1 || !strings.Contains(stderr, "base origin/main is not a commit") {
		t.Errorf("missing default base: exit %d, stderr %q", code, stderr)
	}

	for _, args := range [][]string{
		{"version", "verify-tag"},
		{"version", "verify-tag", "--repo", repo.Dir, "--nope", "v0.1.1"},
		{"version", "verify-tag", "--repo", repo.Dir, "v0.1.1", "v0.1.2"},
		{"version", "verify-tag", "--repo", repo.Dir, "--base=--all", "v0.1.1"},
		{"version", "verify-tag", "--repo", repo.Dir, "--base", "", "v0.1.1"},
	} {
		if code, _, _ := execute(t, repo.Env, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}

	if code, _, _ := execute(t, repo.Env, "version", "verify-tag", "--repo", repo.Dir, "--base", "main", "0.1.1"); code != 1 {
		t.Errorf("tag without the v prefix: exit %d, want 1", code)
	}
}

// TestVerifyTagRequiresACommitOfTheBase covers a tag pushed on a commit that
// never reached the base branch, such as one of an unmerged branch.
func TestVerifyTagRequiresACommitOfTheBase(t *testing.T) {
	repo := gittest.Init(t)
	writeRelease(repo, "v0.1.0")
	repo.CommitAll("initial")

	repo.Run("checkout", "-q", "-b", "feature")
	repo.Write("feature.txt", "feature\n", 0o644)
	feature := repo.CommitAll("unmerged feature")
	repo.Run("tag", "v0.1.0")
	repo.Run("checkout", "-q", "main")

	code, stdout, stderr := execute(t, repo.Env, "version", "verify-tag", "--repo", repo.Dir, "--base", "main", "v0.1.0")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "not reachable from main") || !strings.Contains(stderr, feature) {
		t.Errorf("tag on an unmerged commit: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	repo.Run("merge", "-q", "--ff-only", "feature")

	if code, stdout, stderr := execute(t, repo.Env, "version", "verify-tag", "--repo", repo.Dir, "--base", "main", "v0.1.0"); code != 0 || stdout != "0.1.0\n" {
		t.Errorf("tag on a merged commit: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

// TestVerifyTagChecksTheTaggedCommit covers the gate verify-tag applies to
// the content of the tagged commit: a module path that serves its major
// version.
func TestVerifyTagChecksTheTaggedCommit(t *testing.T) {
	repo := gittest.Init(t)
	writeRelease(repo, "v0.1.0")
	repo.CommitAll("first release")
	repo.Run("tag", "v0.1.0")

	writeRelease(repo, "v2.0.0")
	repo.CommitAll("major version 2")
	repo.Run("tag", "v2.0.0")

	code, stdout, stderr := execute(t, repo.Env, "version", "verify-tag", "--repo", repo.Dir, "--base", "main", "--json", "v2.0.0")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "has no /v2 suffix") {
		t.Errorf("major version 2 without /v2: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	writeRelease(repo, "v2.0.1")
	repo.Write("go.mod", "module "+modulePath+"/v2\n\ngo 1.27.1\n", 0o644)
	repo.CommitAll("module path for major version 2")
	repo.Run("tag", "v2.0.1")

	code, stdout, stderr = execute(t, repo.Env, "version", "verify-tag", "--repo", repo.Dir, "--base", "main", "--json", "v2.0.1")

	var info versionInfo
	if err := json.Unmarshal([]byte(stdout), &info); code != 0 || err != nil || info.Version != "2.0.1" {
		t.Errorf("major version 2 under /v2: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

// TestPreviousTag keeps catalog tags, higher versions and, for a release,
// pre-releases out of the start of the changelog.
func TestPreviousTag(t *testing.T) {
	tags := []string{"v0.1.0", "catalog-20260924.0905", "v0.2.0-rc.1", "v0.2.0-rc.2", "v0.3.0", "", "v0.1.1"}

	for tag, want := range map[string]string{
		"v0.1.0":      "",
		"v0.2.0-rc.2": "v0.2.0-rc.1",
		"v0.2.0-rc.1": "v0.1.1",
		"v0.2.0":      "v0.1.1",
		"v0.3.1":      "v0.3.0",
	} {
		v, err := semver.ParseReleaseTag(tag)
		if err != nil {
			t.Fatal(err)
		}

		if got := previousTag(v, tags); got != want {
			t.Errorf("previousTag(%s) = %q, want %q", tag, got, want)
		}
	}
}

func TestVersionSnapshot(t *testing.T) {
	dist := func(version string) string {
		dir := t.TempDir()

		metadata := `{"project_name":"schepherd","tag":"catalog-20260924.0905","version":"` + version + `","commit":"0123456789abcdef0123456789abcdef01234567"}`
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(metadata), 0o600); err != nil {
			t.Fatal(err)
		}

		return dir
	}

	for _, version := range []string{"0.0.0-snapshot-0123456", "0.0.0-snapshot-89abcdef0123"} {
		if code, stdout, stderr := execute(t, nil, "version", "snapshot", "--dist", dist(version)); code != 0 || stdout != version+"\n" {
			t.Errorf("snapshot %s: exit %d, stdout %q, stderr %q", version, code, stdout, stderr)
		}
	}

	for _, version := range []string{
		"0.1.0", "0.2.0-rc.1", "0.0.0", "0.0.0-snapshot-", "0.0.0-snapshot-XYZ", "0.0.0-snapshot.0123456",
		"0.0.0-snapshot-0123456+x", "0.20260923.1-SNAPSHOT-0123456", "",
	} {
		if code, stdout, stderr := execute(t, nil, "version", "snapshot", "--dist", dist(version)); code != 1 || stdout != "" || stderr == "" {
			t.Errorf("snapshot %q: exit %d, stdout %q, stderr %q", version, code, stdout, stderr)
		}
	}

	if code, _, stderr := execute(t, nil, "version", "snapshot", "--dist", t.TempDir()); code != 1 || !strings.Contains(stderr, "metadata") {
		t.Errorf("missing metadata: exit %d, stderr %q", code, stderr)
	}
}
