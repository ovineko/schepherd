package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/tools/release/internal/licenses"
	"github.com/ovineko/schepherd/tools/release/internal/notices"
)

// fixtureRoot is a repository whose client links only the standard library,
// with the inputs of the notices and no published catalog.
func fixtureRoot(t *testing.T) string {
	t.Helper()

	root := t.TempDir()

	for name, content := range map[string]string{
		"go.mod":                "module example.test/app\n\ngo 1.26\n",
		"cmd/schepherd/main.go": "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println() }\n",
		notices.DataFile:        "[schemastore]\nrules = [\"schemastore\"]\nnotice = \"Example Store\"\n",
		notices.PolicyFile: "[[rules]]\nid = \"schemastore\"\ndecision = \"allow\"\nhosts = [\"json.schemastore.org\"]\n" +
			"license = \"Apache-2.0\"\nreason = \"SchemaStore files\"\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return root
}

// fixtureEnv keeps the go command on the local toolchain, since the fixture's
// go line names none.
func fixtureEnv() []string {
	return append(env.Environ(), "GOTOOLCHAIN=local")
}

// TestGeneratedFilesCommands runs generate and check of both generated files
// on a fixture: generate writes what check accepts, and check names a file
// that no longer matches.
func TestGeneratedFilesCommands(t *testing.T) {
	root := fixtureRoot(t)

	for _, tc := range []struct{ command, file, ok string }{
		{"licenses", licenses.File, "third-party licenses OK\n"},
		{"notices", notices.File, "third-party notices OK\n"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			path := filepath.Join(root, tc.file)

			code, stdout, stderr := execute(t, fixtureEnv(), tc.command, "generate", "--root", root)
			if code != 0 || strings.TrimSpace(stdout) != path {
				t.Fatalf("generate: exit %d, stdout %q, stderr %q", code, stdout, stderr)
			}

			if code, stdout, stderr := execute(t, fixtureEnv(), tc.command, "check", "--root", root); code != 0 || stdout != tc.ok {
				t.Errorf("check after generate: exit %d, stdout %q, stderr %q", code, stdout, stderr)
			}

			if err := os.WriteFile(path, []byte("edited\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			if code, _, stderr := execute(t, fixtureEnv(), tc.command, "check", "--root", root); code != 1 || !strings.Contains(stderr, tc.file) {
				t.Errorf("check of an edited file: exit %d, stderr %q", code, stderr)
			}

			if code, _, stderr := execute(t, nil, tc.command); code != 2 || !strings.Contains(stderr, "requires a command") {
				t.Errorf("%s without a command: exit %d, stderr %q", tc.command, code, stderr)
			}
		})
	}
}

// TestRepositoryFilesAreCurrent is the one gate that keeps the committed
// THIRD_PARTY_LICENSES.txt and THIRD_PARTY_NOTICES.md in step with their
// inputs in local test runs. CI's OS matrix skips it; there the GoReleaser
// before hooks of the packaging job run the same checks once.
func TestRepositoryFilesAreCurrent(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	violations, err := licenses.Check(t.Context(), licenses.Options{Root: root, Env: env.Environ()})
	if err != nil {
		t.Fatal(err)
	}

	noticeViolations, err := notices.Check(t.Context(), notices.Options{Root: root, Env: env.Environ()})
	if err != nil {
		t.Fatal(err)
	}

	if violations = append(violations, noticeViolations...); len(violations) > 0 {
		t.Errorf("run `go run ./tools/release licenses generate` and `go run ./tools/release notices generate`:\n%s",
			strings.Join(violations, "\n"))
	}
}
