package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/interp"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func lookupFrom(vars map[string]string) interp.LookupFunc {
	return func(name string) (string, bool) {
		v, ok := vars[name]

		return v, ok
	}
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()

	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func loadDoc(t *testing.T, doc string, vars map[string]string) (*Config, string, error) {
	t.Helper()

	dir := writeTree(t, map[string]string{"schepherd.toml": doc})
	cfg, err := Load(LoadOptions{Path: filepath.Join(dir, "schepherd.toml"), Cwd: dir, Lookup: lookupFrom(vars)})

	return cfg, dir, err
}

func mustLoadDoc(t *testing.T, doc string, vars map[string]string) (*Config, string) {
	t.Helper()

	cfg, dir, err := loadDoc(t, doc, vars)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	return cfg, dir
}

func wantUsage(t *testing.T, err error, fragments ...string) {
	t.Helper()

	if err == nil {
		t.Fatalf("expected an error containing %q, got nil", fragments)
	}

	if kind := fault.KindOf(err); kind != fault.Usage {
		t.Errorf("error kind = %v, want usage: %v", kind, err)
	}

	for _, f := range fragments {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("error %q does not contain %q", err.Error(), f)
		}
	}
}
