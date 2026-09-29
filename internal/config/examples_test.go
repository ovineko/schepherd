package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ovineko/schepherd/internal/runner"
)

// TestExamplesLoad keeps the documented example configurations loadable: each
// one must load with its documented variables set and pass the runner checks
// that run before any consumer starts.
func TestExamplesLoad(t *testing.T) {
	dir := filepath.Join("..", "..", "examples")

	files, err := filepath.Glob(filepath.Join(dir, "*.toml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no example configurations found in %s: %v", dir, err)
	}

	vars := map[string]string{
		"SCHEPHERD_REPOSITORY": "registry.example/org/schemas",
		"SCHEPHERD_CATALOG":    "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"JSONSCHEMA_BIN":       "/opt/validators/jsonschema",
		"CHECK_JSONSCHEMA_BIN": "/opt/validators/check-jsonschema",
	}
	lookup := func(name string) (string, bool) {
		v, ok := vars[name]

		return v, ok
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			cfg, err := Load(LoadOptions{Path: file, Cwd: cwd, Lookup: lookup})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			if cfg.Repository != vars["SCHEPHERD_REPOSITORY"] || cfg.Catalog != vars["SCHEPHERD_CATALOG"] {
				t.Errorf("catalog = %s@%s", cfg.Repository, cfg.Catalog)
			}

			if cfg.Workspace != cwd {
				t.Errorf("workspace = %s, want the current directory %s", cfg.Workspace, cwd)
			}

			if err := cfg.CheckLocalSchemas(); err != nil {
				t.Errorf("local schemas: %v", err)
			}

			if filepath.Base(file) == "base.schepherd.toml" {
				return
			}

			if !cfg.RunnerConfigured {
				t.Fatal("the example configures no runner")
			}

			if err := runner.ValidateSpec(cfg.Runner); err != nil {
				t.Errorf("runner: %v", err)
			}

			if err := cfg.CheckRunnerEnv(lookup); err != nil {
				t.Errorf("runner environment: %v", err)
			}
		})
	}
}
