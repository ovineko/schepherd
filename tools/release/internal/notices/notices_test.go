package notices

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/internal/publisher/state"
	"github.com/ovineko/schepherd/tools/release/internal/licenses"
)

const fixturePolicy = `[[rules]]
id = "schemastore"
decision = "allow"
hosts = ["json.schemastore.org"]
license = "Apache-2.0"
reason = "SchemaStore files"
`

// fixtureRepository is a repository whose client links a local MIT module
// through a replacement, so that neither network nor module cache is needed.
func fixtureRepository(t *testing.T) string {
	t.Helper()

	root := t.TempDir()

	for name, content := range map[string]string{
		"go.mod":                "module example.test/app\n\ngo 1.26\n\nrequire example.test/dep v0.0.0\n\nreplace example.test/dep => ./dep\n",
		"cmd/schepherd/main.go": "package main\n\nimport \"example.test/dep\"\n\nfunc main() { dep.Run() }\n",
		"dep/go.mod":            "module example.test/dep\n\ngo 1.26\n",
		"dep/dep.go":            "package dep\n\nfunc Run() {}\n",
		"dep/LICENSE":           mitLicense,
		DataFile:                fixtureData,
		PolicyFile:              fixturePolicy,
	} {
		writeFile(t, filepath.Join(root, filepath.FromSlash(name)), content)
	}

	return root
}

func fixtureOptions(root string) Options {
	return Options{Root: root, Targets: []licenses.Target{linuxAMD64, windowsAMD64}, Env: append(env.Environ(), "GOTOOLCHAIN=local")}
}

func saveState(t *testing.T, path string, st *state.State) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := state.Save(path, st); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateAndCheck(t *testing.T) {
	root := fixtureRepository(t)
	opts := fixtureOptions(root)

	before, err := Generate(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}

	text := string(before)
	for _, want := range []string{
		`(?m)^## Linked into the .schepherd. client binary$[^#]+^\| Go standard library +\| go1\.[^|]+\| BSD-3-Clause \|$`,
		`(?m)^## Linked into the .schepherd. client binary$[^#]+^\| .example\.test/dep. +\| v0\.0\.0 => \./dep +\| MIT +\|$`,
		`Release targets: linux/amd64, windows/amd64\.`,
		regexp.QuoteMeta(noCatalog),
	} {
		if !regexp.MustCompile(want).MatchString(text) {
			t.Errorf("generated notices do not match %s:\n%s", want, text)
		}
	}

	st := fixtureState(t, false, fixtureSchema{id: "alpha", license: "Apache-2.0", rules: []string{"schemastore"}})
	elsewhere := filepath.Join(t.TempDir(), "state.json")
	saveState(t, elsewhere, st)

	withState := opts
	withState.State = elsewhere

	after, err := Generate(t.Context(), withState)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(after), "### SchemaStore repository (Apache-2.0)") || !strings.Contains(string(after), "- `alpha`") {
		t.Errorf("notices generated from --state lack the catalog:\n%s", after)
	}

	if v, err := Check(t.Context(), opts); err != nil || len(v) != 1 || !strings.Contains(v[0], File+" is missing") {
		t.Errorf("missing notices: violations %v, err %v", v, err)
	}

	writeFile(t, filepath.Join(root, File), strings.ReplaceAll(string(before), "\n", "\r\n"))

	if v, err := Check(t.Context(), opts); err != nil || len(v) != 0 {
		t.Errorf("up-to-date notices checked out with CRLF: violations %v, err %v", v, err)
	}

	saveState(t, filepath.Join(root, filepath.FromSlash(StateFile)), st)

	v, err := Check(t.Context(), opts)
	if err != nil || len(v) != 1 || !strings.Contains(v[0], "out of date in section Schemas distributed through the catalog;") {
		t.Errorf("a newly recorded catalog: violations %v, err %v", v, err)
	}

	writeFile(t, filepath.Join(root, File), string(after))

	if v, err := Check(t.Context(), opts); err != nil || len(v) != 0 {
		t.Errorf("notices generated from the recorded state: violations %v, err %v", v, err)
	}

	writeFile(t, filepath.Join(root, "dep", "LICENSE"), iscLicense)

	if _, err := Check(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "example.test/dep v0.0.0 => ./dep: LICENSE: the license text is not recognized") {
		t.Errorf("an unrecognized license of a client module: err = %v", err)
	}
}
