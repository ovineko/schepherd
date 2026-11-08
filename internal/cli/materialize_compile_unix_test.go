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
		result.Compilation == nil || !reflect.DeepEqual(*result.Compilation, want) {
		t.Fatalf("materialize summary: %+v, compilation=%+v; want complete=%v compilation=%+v", result.Result, result.Compilation, complete, want)
	}
}

func TestMaterializeCompileOfflineMissingIsReadOnly(t *testing.T) {
	dir, pinned := seedOfflineMaterializeCache(t)
	marker := filepath.Join(t.TempDir(), "compiler-ran")
	executable := processorFixture(t, "printf invoked > '"+marker+"'\nexit 99\n")
	configPath := materializeCompileConfig(t, "sourcemeta", executable)
	for _, name := range []string{"locks", "tmp", "quarantine"} {
		if err := os.RemoveAll(filepath.Join(dir, "v1", name)); err != nil {
			t.Fatal(err)
		}
	}
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
	persistableStdout, persistableStderr, persistableCode := run(t, materializeCompileArgs(dir, pinned, configPath, "--compile-persistable")...)
	if persistableCode != 6 {
		t.Fatalf("missing persistable compilation: code=%d stdout=%q stderr=%q", persistableCode, persistableStdout, persistableStderr)
	}
	assertMaterializeCompilation(t, persistableStdout, false, compilationSummary{Missing: 1})
	if after := offlineCacheSnapshot(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("persistable verification changed cache: before=%v after=%v", before, after)
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
				got, err := (&app{}).compileCatalog(context.Background(), s, 1, []string{"fixture", "fixture"}, true)
				if err != nil || !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("prepare compilation: summary=%+v err=%v; want %+v", got, err, tc.want)
				}
				wantFile(t, marker, "x")
			}
			s.close()
			for _, name := range []string{"locks", "tmp", "quarantine"} {
				if err := os.RemoveAll(filepath.Join(dir, "v1", name)); err != nil {
					t.Fatal(err)
				}
			}
			before := offlineCacheSnapshot(t, dir)
			stdout, stderr, code := run(t, materializeCompileArgs(dir, pinned, configPath, "--compile-persistable", "--compile-schema", "fixture")...)
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

func TestMaterializeCompilePersistableSkipsInvocationLocalSettings(t *testing.T) {
	dir, pinned := seedOfflineMaterializeCache(t)
	marker := filepath.Join(t.TempDir(), "compiler-ran")
	executable := processorFixture(t, "printf invoked > '"+marker+"'\nexit 99\n")
	configPath := materializeCompileConfig(t, "sourcemeta", executable)
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("\n[processors.fixture.env]\nSELECTED = \"{file}\"\n")...)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	before := offlineCacheSnapshot(t, dir)
	stdout, stderr, code := run(t, materializeCompileArgs(dir, pinned, configPath, "--compile-persistable", "--json")...)
	if code != 0 {
		t.Fatalf("persistable preparation: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var result struct {
		materialize.Result

		Compilation *compilationSummary `json:"compilation"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode persistable summary: %v\n%s", err, stdout)
	}
	wantReasons := map[string]int64{"sourcemeta: processor settings depend on {file}": 1}
	if !result.Complete || result.Compilation == nil || result.Compilation.InvocationLocal != 1 || !reflect.DeepEqual(result.Compilation.InvocationLocalReasons, wantReasons) || result.Compilation.Missing != 0 {
		t.Fatalf("persistable summary: %+v compilation=%+v", result.Result, result.Compilation)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("invocation-local compiler was run: %v", err)
	}
	if after := offlineCacheSnapshot(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("persistable preparation changed readonly cache: before=%v after=%v", before, after)
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

func TestUnresolvedInternalReferenceIsPreparedUnsupportedAndFailsOfflineRun(t *testing.T) {
	schema := []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$ref":"#/$defs/missing"}`)
	dir, pinned := seedOfflineMaterializeCache(t, schema)
	native := processorFixture(t, `printf 'unexpected error: Could not resolve schema reference' >&2
exit 1
`)
	python := processorFixture(t, `case "$1" in
identity) printf '{"runtime":"python","version":"fixture"}' ;;
probe) printf '{"supported":false,"reason":"unresolved schema reference"}' ;;
validate) exit 9 ;;
*) exit 10 ;;
esac
`)
	javascript := processorFixture(t, `case "$1" in
identity) printf '{"runtime":"node","version":"fixture"}' ;;
compile|probe) printf '{"supported":false,"reason":"unresolved schema reference"}' ;;
validate) exit 9 ;;
*) exit 10 ;;
esac
`)
	workspace := t.TempDir()
	configPath := filepath.Join(workspace, "schepherd.toml")
	config := "config_version = 1\nworkspace = " + strconv.Quote(workspace) + "\n\n[runner]\nprocessors = [\"native\", \"python\", \"javascript\"]\nunsupported = \"error\"\n\n" +
		"[processors.native]\ntype = \"sourcemeta\"\ncommand = " + strconv.Quote(native) + "\ninherit_env = false\n\n" +
		"[processors.python]\ntype = \"check-jsonschema\"\ncommand = " + strconv.Quote(python) + "\ninherit_env = false\n\n" +
		"[processors.javascript]\ntype = \"ajv\"\ncommand = " + strconv.Quote(javascript) + "\ninherit_env = false\n"
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	s := materializeCompileSession(t, dir, pinned, configPath)
	prepared, err := (&app{}).compileCatalog(t.Context(), s, 1, nil, true)
	if err != nil || prepared.Unsupported != 3 || prepared.InvocationLocal != 0 || prepared.Missing != 0 {
		t.Fatalf("negative capability preparation: %+v err=%v", prepared, err)
	}
	s.close()
	for _, name := range []string{"locks", "tmp", "quarantine"} {
		if err := os.RemoveAll(filepath.Join(dir, "v1", name)); err != nil {
			t.Fatal(err)
		}
	}
	before := offlineCacheSnapshot(t, dir)
	input := filepath.Join(workspace, "fixture.json")
	if err := os.WriteFile(input, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := run(t, "--quiet", "--offline", "--config", configPath, "--workspace", workspace,
		"--repository", "registry.example/org/schemas", "--catalog", pinned, "--cache-dir", dir,
		"run", "--json", "--", input)
	evidence := stdout + stderr
	if code == 0 || !strings.Contains(evidence, "unsupported input") || !strings.Contains(evidence, "native:") || !strings.Contains(evidence, "python:") || !strings.Contains(evidence, "javascript:") {
		t.Fatalf("unsupported schema run was reported as valid: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if after := offlineCacheSnapshot(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("offline unsupported run changed prepared cache: before=%v after=%v", before, after)
	}
}

func TestMaterializeCompileInfrastructureFailureIsFatal(t *testing.T) {
	dir, pinned := seedOfflineMaterializeCache(t)
	executable := processorFixture(t, "if [ \"$1\" = identity ]; then printf '{\"version\":\"fixture-1\"}'; exit 0; fi\nprintf 'compiler infrastructure failed' >&2\nexit 1\n")
	configPath := materializeCompileConfig(t, "ajv", executable)
	s := materializeCompileSession(t, dir, pinned, configPath)
	s.cfg.Unsupported = "skip"
	got, err := (&app{}).compileCatalog(context.Background(), s, 1, nil, false)
	if err == nil || fault.KindOf(err) != fault.ConsumerStart || !reflect.DeepEqual(got, compilationSummary{}) {
		t.Fatalf("infrastructure failure: summary=%+v err=%v", got, err)
	}
	s.close()
	stdout, stderr, code := run(t, materializeCompileArgs(dir, pinned, configPath, "--compile")...)
	if code != 6 {
		t.Fatalf("failed compilation must remain unprepared: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	assertMaterializeCompilation(t, stdout, false, compilationSummary{Missing: 1})
}
