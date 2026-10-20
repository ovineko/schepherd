//go:build !windows

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/config"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/materialize"
	"github.com/ovineko/schepherd/internal/store"
)

func materializeCompileConfig(t *testing.T, kind, executable string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schepherd.toml")
	body := "config_version = 1\n[processors.fixture]\ntype = " + strconv.Quote(kind) + "\ncommand = " + strconv.Quote(executable) + "\ninherit_env = false\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func materializeCompileArgs(dir, pinned, configPath string, extra ...string) []string {
	args := append([]string{"--config", configPath, "--workspace", filepath.Dir(configPath)}, offlineMaterializeArgs(dir, pinned)...)
	return append(args, extra...)
}

func materializeCompileSession(t *testing.T, dir, pinned, configPath string) *session {
	t.Helper()
	cfg, err := config.Load(config.LoadOptions{Path: configPath, Cwd: filepath.Dir(configPath)})
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	st := store.New(store.Options{
		Cache: c, Offline: true, Repository: "registry.example/org/schemas",
		ArtifactLimits: artifactLimits(cfg), CatalogLimits: catalogLimits(cfg),
		Open: func() (store.Remote, error) {
			t.Error("compilation attempted to open the registry")
			return nil, errors.New("unexpected registry access")
		},
	})
	loaded, err := st.Catalog(context.Background(), pinned)
	if err != nil {
		t.Fatal(err)
	}
	return &session{cfg: cfg, cache: c, store: st, catalog: loaded}
}

func assertMaterializeCompilation(t *testing.T, stdout string, complete bool, want compilationSummary) {
	t.Helper()
	var result struct {
		materialize.Result

		Compilation *compilationSummary `json:"compilation"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode materialize summary: %v\n%s", err, stdout)
	}
	if result.Complete != complete || !result.Offline || result.Entries != 1 || result.Schemas != 1 ||
		result.Compilation == nil || *result.Compilation != want {
		t.Fatalf("materialize summary: %+v, compilation=%+v; want complete=%v compilation=%+v", result.Result, result.Compilation, complete, want)
	}
}

func TestMaterializeCompileOfflineMissingIsReadOnly(t *testing.T) {
	dir, pinned := seedOfflineMaterializeCache(t)
	marker := filepath.Join(t.TempDir(), "compiler-ran")
	executable := processorFixture(t, "printf invoked > '"+marker+"'\nexit 99\n")
	configPath := materializeCompileConfig(t, "sourcemeta", executable)
	before := offlineCacheSnapshot(t, dir)
	stdout, stderr, code := run(t, materializeCompileArgs(dir, pinned, configPath, "--compile")...)
	if code != 6 {
		t.Fatalf("missing compilation: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	assertMaterializeCompilation(t, stdout, false, compilationSummary{Missing: 1})
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("offline verification executed compiler: %v", err)
	}
	if after := offlineCacheSnapshot(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("offline verification changed cache: before=%v after=%v", before, after)
	}
}

func TestMaterializeCompilePreparedStateAndReuse(t *testing.T) {
	for _, tc := range []struct {
		name, kind, output string
		want               compilationSummary
	}{
		{"artifact", "ajv", `printf '{"supported":true,"template":"module.exports = () => true"}'`, compilationSummary{Artifacts: 1}},
		{"capability", "check-jsonschema", `printf '{"supported":true}'`, compilationSummary{Capabilities: 1}},
		{"unsupported", "sourcemeta", "printf 'unsupported vocabulary' >&2; exit 3", compilationSummary{Unsupported: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, pinned := seedOfflineMaterializeCache(t)
			marker := filepath.Join(t.TempDir(), "compilations")
			executable := processorFixture(t, "if [ \"$1\" = identity ]; then printf '{\"version\":\"fixture-1\"}'; exit 0; fi\nprintf x >> '"+marker+"'\n"+tc.output+"\n")
			configPath := materializeCompileConfig(t, tc.kind, executable)
			s := materializeCompileSession(t, dir, pinned, configPath)
			for range 2 {
				got, err := (&app{}).compileCatalog(context.Background(), s, 1, []string{"fixture", "fixture"})
				if err != nil || got != tc.want {
					t.Fatalf("prepare compilation: summary=%+v err=%v; want %+v", got, err, tc.want)
				}
				wantFile(t, marker, "x")
			}
			s.close()
			before := offlineCacheSnapshot(t, dir)
			stdout, stderr, code := run(t, materializeCompileArgs(dir, pinned, configPath, "--compile", "--compile-schema", "fixture")...)
			if code != 0 {
				t.Fatalf("prepared offline verification: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			assertMaterializeCompilation(t, stdout, true, tc.want)
			wantFile(t, marker, "x")
			if after := offlineCacheSnapshot(t, dir); !reflect.DeepEqual(before, after) {
				t.Fatalf("prepared offline verification changed cache: before=%v after=%v", before, after)
			}
		})
	}
}

func TestMaterializeCompileSchemaSelectionErrors(t *testing.T) {
	dir, pinned := seedOfflineMaterializeCache(t)
	marker := filepath.Join(t.TempDir(), "compiler-ran")
	executable := processorFixture(t, "printf invoked > '"+marker+"'\nexit 99\n")
	configPath := materializeCompileConfig(t, "sourcemeta", executable)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"requires compile", []string{"--compile-schema", "fixture"}, "requires --compile"},
		{"unknown ID", []string{"--compile", "--compile-schema", "absent"}, `schema "absent" is not in catalog`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := offlineCacheSnapshot(t, dir)
			stdout, stderr, code := run(t, materializeCompileArgs(dir, pinned, configPath, tc.args...)...)
			if code == 0 || !strings.Contains(stderr, tc.want) {
				t.Fatalf("selection error: code=%d stdout=%q stderr=%q; want %q", code, stdout, stderr, tc.want)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("invalid selection executed compiler: %v", err)
			}
			if after := offlineCacheSnapshot(t, dir); !reflect.DeepEqual(before, after) {
				t.Fatalf("invalid selection changed cache: before=%v after=%v", before, after)
			}
		})
	}
}

func TestMaterializeCompileInfrastructureFailureIsFatal(t *testing.T) {
	dir, pinned := seedOfflineMaterializeCache(t)
	executable := processorFixture(t, "if [ \"$1\" = identity ]; then printf '{\"version\":\"fixture-1\"}'; exit 0; fi\nprintf 'compiler infrastructure failed' >&2\nexit 1\n")
	configPath := materializeCompileConfig(t, "ajv", executable)
	s := materializeCompileSession(t, dir, pinned, configPath)
	s.cfg.Unsupported = "skip"
	got, err := (&app{}).compileCatalog(context.Background(), s, 1, nil)
	if err == nil || fault.KindOf(err) != fault.ConsumerStart || got != (compilationSummary{}) {
		t.Fatalf("infrastructure failure: summary=%+v err=%v", got, err)
	}
	s.close()
	stdout, stderr, code := run(t, materializeCompileArgs(dir, pinned, configPath, "--compile")...)
	if code != 6 {
		t.Fatalf("failed compilation must remain unprepared: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	assertMaterializeCompilation(t, stdout, false, compilationSummary{Missing: 1})
}
