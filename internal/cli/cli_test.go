package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	return runWith(t, nil, args...)
}

func TestVersionJSON(t *testing.T) {
	stdout, stderr, code := run(t, "version", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("version --json: code %d, stderr %q", code, stderr)
	}

	var v struct {
		Version               string `json:"version"`
		CatalogFormatVersions []int  `json:"catalogFormatVersions"`
		ConfigFormatVersions  []int  `json:"configFormatVersions"`
	}

	if err := json.Unmarshal([]byte(stdout), &v); err != nil {
		t.Fatalf("version --json is not JSON: %v\n%s", err, stdout)
	}

	if v.Version != "dev" || len(v.CatalogFormatVersions) != 1 || v.CatalogFormatVersions[0] != 2 || len(v.ConfigFormatVersions) != 1 {
		t.Errorf("version output = %+v", v)
	}

	plain, _, code := run(t, "version")
	if code != 0 || plain != "dev\n" {
		t.Errorf("version = %q (code %d)", plain, code)
	}
}

func TestUsageErrorsExitTwoWithEmptyStdout(t *testing.T) {
	cache := t.TempDir()
	digest := "sha256:" + strings.Repeat("a", 64)

	cases := map[string][]string{
		"unknown flag":          {"--nope", "version"},
		"unknown command":       {"frobnicate"},
		"path without id":       {"path"},
		"no repository":         {"--cache-dir", cache, "--catalog", digest, "path", "x"},
		"no catalog":            {"--cache-dir", cache, "--repository", "registry.example/org/schemas", "path", "x"},
		"tag instead of digest": {"--cache-dir", cache, "--repository", "registry.example/org/schemas", "--catalog", "catalog-latest", "path", "x"},
		"repository with tag":   {"--cache-dir", cache, "--repository", "registry.example/org/schemas:v1", "--catalog", digest, "path", "x"},
		"negative timeout":      {"--timeout", "-1s", "--cache-dir", cache, "--repository", "registry.example/org/schemas", "--catalog", digest, "path", "x"},
		"resolve without file":  {"resolve"},
		"run without files":     {"run"},
		"mirror by tag":         {"mirror", "registry.example/org/schemas:catalog-latest", "mirror.example/team/schemas"},
		"missing config file":   {"--config", filepath.Join(cache, "missing.toml"), "config", "check"},
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, code := run(t, args...)
			if code != 2 {
				t.Fatalf("exit code %d, want 2; stderr: %s", code, stderr)
			}

			if stdout != "" {
				t.Errorf("stdout must stay empty on errors, got %q", stdout)
			}

			if !strings.HasPrefix(stderr, "schepherd: usage error: ") {
				t.Errorf("stderr = %q", stderr)
			}
		})
	}

	_, stderr, _ := run(t, "--cache-dir", cache, "--repository", "registry.example/org/schemas", "--catalog", "catalog-latest", "path", "x")
	if !strings.Contains(stderr, "schepherd pin") {
		t.Errorf("a tag instead of a digest must point to pin: %q", stderr)
	}
}

func TestOfflineCacheMissExitsSix(t *testing.T) {
	stdout, stderr, code := run(t, "--offline", "--cache-dir", t.TempDir(),
		"--repository", "registry.example/org/schemas", "--catalog", "sha256:"+strings.Repeat("b", 64), "path", "x")
	if code != 6 || stdout != "" || !strings.Contains(stderr, strings.Repeat("b", 64)) {
		t.Fatalf("offline miss: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestConfigCheckWithoutFile(t *testing.T) {
	stdout, stderr, code := run(t, "config", "check")
	if code != 0 || !strings.Contains(stdout, "defaults only") {
		t.Fatalf("config check: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestWriteExportNeverOverwritesWithoutForce(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "schema.json")

	if err := writeExport(dest, []byte(`{"a":1}`), false); err != nil {
		t.Fatal(err)
	}

	if err := writeExport(dest, []byte(`{"a":2}`), false); err == nil {
		t.Fatal("existing file overwritten without --force")
	}

	if got, _ := os.ReadFile(dest); string(got) != `{"a":1}` {
		t.Fatalf("destination changed to %q", got)
	}

	if err := writeExport(dest, []byte(`{"a":3}`), true); err != nil {
		t.Fatal(err)
	}

	if got, _ := os.ReadFile(dest); string(got) != `{"a":3}` {
		t.Fatalf("--force result %q", got)
	}

	leftovers, _ := filepath.Glob(filepath.Join(dir, ".schepherd-export-*"))
	if len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

func TestInputPathsRejectsDirectoriesAndMissingFiles(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.json")

	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := inputPaths([]string{dir}); err == nil {
		t.Error("a directory was accepted as input")
	}

	if _, err := inputPaths([]string{filepath.Join(dir, "missing.json")}); err == nil {
		t.Error("a missing file was accepted as input")
	}

	paths, err := inputPaths([]string{file})
	if err != nil || len(paths) != 1 || !filepath.IsAbs(paths[0]) {
		t.Errorf("inputPaths = %v, %v", paths, err)
	}
}
