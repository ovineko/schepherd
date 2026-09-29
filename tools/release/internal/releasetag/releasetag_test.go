package releasetag

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/buildinfo/semver"
	"github.com/ovineko/schepherd/tools/release/internal/gittest"
)

func mustTag(t *testing.T, tag string) semver.Version {
	t.Helper()

	v, err := semver.ParseReleaseTag(tag)
	if err != nil {
		t.Fatal(err)
	}

	return v
}

func TestModulePath(t *testing.T) {
	accepted := map[string]string{
		"module github.com/ovineko/schepherd\n\ngo 1.27.1\n":            "github.com/ovineko/schepherd",
		"// comment\nmodule github.com/ovineko/schepherd/v2 // major\n": "github.com/ovineko/schepherd/v2",
		"module \"example.test/quoted\"\n":                              "example.test/quoted",
		"\tmodule\texample.test/tab\r\n":                                "example.test/tab",
	}

	for goMod, want := range accepted {
		if got, err := ModulePath([]byte(goMod)); err != nil || got != want {
			t.Errorf("ModulePath(%q) = %q, %v, want %q", goMod, got, err, want)
		}
	}

	for _, goMod := range []string{"", "go 1.27.1\n", "modules example.test/x\n"} {
		if got, err := ModulePath([]byte(goMod)); err == nil {
			t.Errorf("ModulePath(%q) = %q, want an error", goMod, got)
		}
	}
}

// TestCheckModulePath covers the rule of the go command: a module version
// vX.Y.Z with X >= 2 needs the module path suffix /vX, and v0 and v1 need
// none, so a tag the module path cannot serve is refused before a release
// of it reaches GitHub and npm.
func TestCheckModulePath(t *testing.T) {
	const module = "github.com/ovineko/schepherd"

	accepted := map[string]string{
		"v0.1.0":        module,
		"v1.0.0":        module,
		"v1.9.3-rc.1":   module,
		"v2.0.0":        module + "/v2",
		"v3.1.0-beta.1": module + "/v3",
		"v12.0.0":       module + "/v12",
	}

	for tag, path := range accepted {
		if err := CheckModulePath(path, mustTag(t, tag)); err != nil {
			t.Errorf("%s under %s: %v", tag, path, err)
		}
	}

	rejected := map[string]struct{ tag, path, want string }{
		"major 2 without suffix":  {"v2.0.0", module, "has no /v2 suffix"},
		"major 10 without suffix": {"v10.1.0", module, "has no /v10 suffix"},
		"pre-release of major 2":  {"v2.0.0-rc.1", module, "has no /v2 suffix"},
		"wrong suffix":            {"v3.0.0", module + "/v2", "only serves major version 2"},
		"suffix for major 1":      {"v1.4.0", module + "/v2", "only serves major version 2"},
		"suffix for major 0":      {"v0.9.0", module + "/v5", "only serves major version 5"},
		"suffix v1":               {"v1.0.0", module + "/v1", "invalid major version suffix"},
	}

	for name, tc := range rejected {
		err := CheckModulePath(tc.path, mustTag(t, tc.tag))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: CheckModulePath(%s, %s) = %v, want %q", name, tc.path, tc.tag, err, tc.want)
		}
	}

	err := CheckModulePath(module, mustTag(t, "v2.0.0"))
	if err == nil || !strings.Contains(err.Error(), module+"/v2") || !strings.Contains(err.Error(), "go install") {
		t.Errorf("the message must name the module path the release needs and why: %v", err)
	}
}

// TestCheckCommit reads go.mod from the tagged commit, not from the working
// tree, so what is checked is what the tag releases.
func TestCheckCommit(t *testing.T) {
	repo := gittest.Init(t)
	repo.Write("go.mod", "module github.com/ovineko/schepherd\n\ngo 1.27.1\n", 0o644)
	first := repo.CommitAll("first release")

	if err := CheckCommit(t.Context(), repo.Git, first, mustTag(t, "v0.1.0")); err != nil {
		t.Fatalf("a commit with go.mod: %v", err)
	}

	repo.Write("go.mod", "module github.com/ovineko/schepherd/v2\n\ngo 1.27.1\n", 0o644)

	if err := CheckCommit(t.Context(), repo.Git, first, mustTag(t, "v2.0.0")); err == nil || !strings.Contains(err.Error(), "has no /v2 suffix") {
		t.Errorf("major 2 under the committed module path without /v2: %v", err)
	}

	if err := os.Remove(filepath.Join(repo.Dir, "go.mod")); err != nil {
		t.Fatal(err)
	}

	noModule := repo.CommitAll("no go.mod")
	if err := CheckCommit(t.Context(), repo.Git, noModule, mustTag(t, "v0.1.2")); err == nil || !strings.Contains(err.Error(), "read go.mod") {
		t.Errorf("a commit without go.mod: %v", err)
	}
}
