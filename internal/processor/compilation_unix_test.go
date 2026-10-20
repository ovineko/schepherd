//go:build !windows

package processor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/runner"
)

type compilationFixture struct {
	dir        string
	executable string
	schema     string
	marker     string
	cacheDir   string
	definition Definition
	task       runner.Task
}

func newCompilationFixture(t *testing.T) *compilationFixture {
	t.Helper()
	f := &compilationFixture{dir: t.TempDir()}
	f.executable = filepath.Join(f.dir, "validator")
	f.schema = filepath.Join(f.dir, "schema.json")
	f.marker = filepath.Join(f.dir, "compilations")
	f.cacheDir = filepath.Join(f.dir, "cache")
	f.definition = Definition{Type: "sourcemeta", Spec: runner.Spec{
		Name: "fixture", Env: map[string]string{"FAKE_SETTING": "strict"},
	}}
	f.task = runner.Task{
		Path: f.executable, Dir: f.dir,
		Processor: runner.Processor{Name: "fixture", Type: "sourcemeta"},
		Env:       []string{"PATH=/usr/bin:/bin", "FAKE_COUNTER=" + f.marker, "FAKE_SETTING=strict"},
	}
	f.writeExecutable(t, "one")
	writeCompilationFile(t, f.schema, []byte(`{"const":7}`), 0o600)
	return f
}

func writeCompilationFile(t *testing.T, path string, data []byte, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func (f *compilationFixture) writeExecutable(t *testing.T, revision string) {
	t.Helper()
	body := `#!/bin/sh
set -eu
revision='` + revision + `'
case "$1" in
identity)
  printf '{"processor":"fixture","revision":"%s"}' "$revision"
  ;;
compile|probe)
  printf '%s\n' "$1" >> "$FAKE_COUNTER"
  schema=$(cat "$2")
  case "$schema" in
    '{"$ref":"number.json"}') schema=$(cat "${2%/*}/number.json") ;;
  esac
  case "$schema" in
    '{"type":"unsupported"}')
      if [ "$1" = probe ]; then
        printf '{"supported":false,"reason":"unsupported fixture type"}'
        exit 0
      fi
      printf 'unsupported fixture type' >&2
      exit 3
      ;;
  esac
  if [ -n "${FAKE_FAIL_ONCE:-}" ] && [ ! -e "$FAKE_FAIL_ONCE" ]; then
    : > "$FAKE_FAIL_ONCE"
    printf 'compiler initialization failed' >&2
    exit 1
  fi
  if [ -n "${FAKE_OVERSIZE_ONCE:-}" ] && [ ! -e "$FAKE_OVERSIZE_ONCE" ]; then
    : > "$FAKE_OVERSIZE_ONCE"
    printf '{"const":7,"padding":"'
    dd if=/dev/zero bs=1048576 count=64 2>/dev/null
    printf '"}'
    exit 0
  fi
  expected=${schema#'{"const":'}
  expected=${expected%?}
  case "$expected" in ''|*[!0-9]*) exit 9 ;; esac
  if [ "$1" = probe ]; then
    printf '{"supported":true}'
  else
    printf '{"const":%s,"compiler":"%s","setting":"%s"}' "$expected" "$revision" "$FAKE_SETTING"
  fi
  ;;
validate)
  input=$3
  shift 3
  template=
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --template|--compiled) template=$2; shift 2 ;;
      --configuration) shift 2 ;;
      *) shift ;;
    esac
  done
  [ -n "$template" ] || exit 9
  compiled=$(cat "$template")
  expected=${compiled#'{"const":'}
  expected=${expected%%,*}
  actual=$(cat "$input")
  if [ "$actual" = "$expected" ]; then
    printf '{"valid":true,"errors":[]}'
  else
    printf '{"valid":false,"errors":[{"message":"const mismatch"}]}'
    exit 1
  fi
  ;;
*) exit 9 ;;
esac
`
	writeCompilationFile(t, f.executable, []byte(body), 0o700)
}

func openCompilationCache(t *testing.T, dir string, readOnly bool) *cache.Cache {
	t.Helper()
	var c *cache.Cache
	var err error
	if readOnly {
		c, err = cache.OpenReadOnly(dir)
	} else {
		c, err = cache.Open(dir)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func (f *compilationFixture) prepare(t *testing.T, c *cache.Cache, verifyOnly bool) (Prepared, error) {
	t.Helper()
	data, err := os.ReadFile(f.schema)
	if err != nil {
		t.Fatal(err)
	}
	compiler := &Compiler{Cache: c, VerifyOnly: verifyOnly}
	t.Cleanup(compiler.Close)
	return compiler.Prepare(context.Background(), f.definition, f.task, f.schema, data, false)
}

func (f *compilationFixture) count(t *testing.T, want int) {
	t.Helper()
	data, err := os.ReadFile(f.marker)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "\n"); got != want {
		t.Fatalf("expensive compilation/probe calls = %d, want %d: %q", got, want, data)
	}
}

func assertCompilationTemplate(t *testing.T, prepared Prepared, value int, revision, setting string) {
	t.Helper()
	if prepared.Reason != "" || prepared.Path == "" {
		t.Fatalf("expected compatible compiled template, got %+v", prepared)
	}
	data, err := os.ReadFile(prepared.Path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Const    int    `json:"const"`
		Compiler string `json:"compiler"`
		Setting  string `json:"setting"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("compiled template: %q: %v", data, err)
	}
	if got.Const != value || got.Compiler != revision || got.Setting != setting {
		t.Fatalf("compiled template = %s, want const=%d compiler=%q setting=%q", data, value, revision, setting)
	}
}

func (f *compilationFixture) validate(t *testing.T, prepared Prepared, input string, wantValid bool) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.json")
	writeCompilationFile(t, path, []byte(input), 0o600)
	task := f.task
	task.Args = []string{f.executable, "validate", f.schema, path}
	prepared.Apply(&task)
	cmd := exec.CommandContext(context.Background(), task.Path, task.Args[1:]...)
	cmd.Dir, cmd.Env = task.Dir, task.Env
	data, err := cmd.Output()
	if wantValid && err != nil {
		t.Fatalf("valid input rejected: %s: %v", data, err)
	}
	if !wantValid {
		exit, ok := errors.AsType[*exec.ExitError](err)
		if !ok || exit.ExitCode() != 1 {
			t.Fatalf("invalid input did not produce validation failure: %s: %v", data, err)
		}
	}
	var result struct {
		Valid *bool `json:"valid"`
	}
	if err := json.Unmarshal(data, &result); err != nil || result.Valid == nil || *result.Valid != wantValid {
		t.Fatalf("validation result = %q, want valid=%v: %v", data, wantValid, err)
	}
}

func TestCompilationReusedAcrossCompilerInstances(t *testing.T) {
	f := newCompilationFixture(t)
	c := openCompilationCache(t, f.cacheDir, false)
	for range 2 {
		prepared, err := f.prepare(t, c, false)
		if err != nil {
			t.Fatal(err)
		}
		assertCompilationTemplate(t, prepared, 7, "one", "strict")
		f.validate(t, prepared, "7", true)
		f.validate(t, prepared, "8", false)
	}
	f.count(t, 1)
}

func TestCompilationInvalidatedByCompilerInputs(t *testing.T) {
	for _, change := range []string{"schema", "executable", "environment"} {
		t.Run(change, func(t *testing.T) {
			f := newCompilationFixture(t)
			c := openCompilationCache(t, f.cacheDir, false)
			first, err := f.prepare(t, c, false)
			if err != nil {
				t.Fatal(err)
			}
			assertCompilationTemplate(t, first, 7, "one", "strict")
			value, revision, setting := 7, "one", "strict"
			switch change {
			case "schema":
				writeCompilationFile(t, f.schema, []byte(`{"const":8}`), 0o600)
				value = 8
			case "executable":
				f.writeExecutable(t, "two")
				revision = "two"
			case "environment":
				f.definition.Spec.Env["FAKE_SETTING"] = "lenient"
				f.task.Env = []string{"PATH=/usr/bin:/bin", "FAKE_COUNTER=" + f.marker, "FAKE_SETTING=lenient"}
				setting = "lenient"
			}
			for range 2 {
				prepared, err := f.prepare(t, c, false)
				if err != nil {
					t.Fatal(err)
				}
				assertCompilationTemplate(t, prepared, value, revision, setting)
				f.validate(t, prepared, "8", change == "schema")
			}
			f.count(t, 2)
		})
	}
}

func TestCompilationFailureIsNotNegativeCached(t *testing.T) {
	f := newCompilationFixture(t)
	f.task.Env = append(f.task.Env, "FAKE_FAIL_ONCE="+filepath.Join(f.dir, "failed-once"))
	c := openCompilationCache(t, f.cacheDir, false)
	if prepared, err := f.prepare(t, c, false); err == nil || fault.KindOf(err) != fault.ConsumerStart || prepared.Reason != "" {
		t.Fatalf("compiler initialization failure must remain an error: %+v %v", prepared, err)
	}
	for range 2 {
		prepared, err := f.prepare(t, c, false)
		if err != nil {
			t.Fatal(err)
		}
		assertCompilationTemplate(t, prepared, 7, "one", "strict")
		f.validate(t, prepared, "7", true)
	}
	f.count(t, 2)
}

func TestCompilationKnownIncompatibilityIsCached(t *testing.T) {
	f := newCompilationFixture(t)
	writeCompilationFile(t, f.schema, []byte(`{"type":"unsupported"}`), 0o600)
	c := openCompilationCache(t, f.cacheDir, false)
	var reason string
	for range 2 {
		prepared, err := f.prepare(t, c, false)
		if err != nil || prepared.Path != "" || !strings.Contains(prepared.Reason, "unsupported fixture type") {
			t.Fatalf("expected declared incompatibility: %+v %v", prepared, err)
		}
		if reason != "" && prepared.Reason != reason {
			t.Fatalf("cached incompatibility changed: %q -> %q", reason, prepared.Reason)
		}
		reason = prepared.Reason
	}
	f.count(t, 1)
}

func TestCompilationProbeDecisionIsCached(t *testing.T) {
	for _, supported := range []bool{true, false} {
		name := "unsupported"
		if supported {
			name = "supported"
		}
		t.Run(name, func(t *testing.T) {
			f := newCompilationFixture(t)
			f.definition.Type = "check-jsonschema"
			if !supported {
				writeCompilationFile(t, f.schema, []byte(`{"type":"unsupported"}`), 0o600)
			}
			c := openCompilationCache(t, f.cacheDir, false)
			for range 2 {
				prepared, err := f.prepare(t, c, false)
				if err != nil || prepared.Path != "" || (prepared.Reason == "") != supported {
					t.Fatalf("cached capability supported=%v: %+v %v", supported, prepared, err)
				}
				if !supported && prepared.Reason != "unsupported fixture type" {
					t.Fatalf("wrong incompatibility reason: %q", prepared.Reason)
				}
			}
			f.count(t, 1)
		})
	}
}

func TestCompilationCorruptArtifactRefusedOfflineAndRepairedWritable(t *testing.T) {
	f := newCompilationFixture(t)
	c := openCompilationCache(t, f.cacheDir, false)
	prepared, err := f.prepare(t, c, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(prepared.Path, 0o600); err != nil {
		t.Fatal(err)
	}
	writeCompilationFile(t, prepared.Path, []byte(`{"const":9,"compiler":"one","setting":"strict"}`), 0o600)
	before := compilationTree(t, f.cacheDir)
	if result, err := f.prepare(t, c, true); err == nil || fault.KindOf(err) != fault.Offline || result.Path != "" {
		t.Fatalf("verify-only accepted corrupt compiler artifact: %+v %v", result, err)
	}
	f.count(t, 1)
	if after := compilationTree(t, f.cacheDir); !reflect.DeepEqual(before, after) {
		t.Fatal("verify-only changed the corrupt cache")
	}
	repaired, err := f.prepare(t, c, false)
	if err != nil {
		t.Fatal(err)
	}
	assertCompilationTemplate(t, repaired, 7, "one", "strict")
	f.validate(t, repaired, "7", true)
	f.validate(t, repaired, "9", false)
	verified, err := f.prepare(t, c, true)
	if err != nil {
		t.Fatal(err)
	}
	assertCompilationTemplate(t, verified, 7, "one", "strict")
	f.count(t, 2)
}

type compilationTreeEntry struct {
	Mode    fs.FileMode
	ModTime time.Time
	Data    string
}

func compilationTree(t *testing.T, root string) map[string]compilationTreeEntry {
	t.Helper()
	entries := make(map[string]compilationTreeEntry)
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		item := compilationTreeEntry{Mode: info.Mode(), ModTime: info.ModTime()}
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			item.Data = string(data)
		}
		entries[rel] = item
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestCompilationRelocatedReadOnlyCacheHitDoesNotWriteOrCompile(t *testing.T) {
	f := newCompilationFixture(t)
	c := openCompilationCache(t, f.cacheDir, false)
	prepared, err := f.prepare(t, c, false)
	if err != nil {
		t.Fatal(err)
	}
	assertCompilationTemplate(t, prepared, 7, "one", "strict")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	relocated := filepath.Join(f.dir, "relocated-cache")
	if err := os.Rename(f.cacheDir, relocated); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(relocated, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(path, 0o755)
			}
			return err
		})
	})
	if err := filepath.WalkDir(relocated, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o444)
		if entry.IsDir() {
			mode = 0o555
		}
		return os.Chmod(path, mode)
	}); err != nil {
		t.Fatal(err)
	}
	before := compilationTree(t, relocated)
	readOnly := openCompilationCache(t, relocated, true)
	for _, verifyOnly := range []bool{false, true} {
		prepared, err := f.prepare(t, readOnly, verifyOnly)
		if err != nil {
			t.Fatal(err)
		}
		assertCompilationTemplate(t, prepared, 7, "one", "strict")
		if !strings.HasPrefix(prepared.Path, relocated+string(filepath.Separator)) {
			t.Fatalf("artifact retained old cache location: %q", prepared.Path)
		}
		f.validate(t, prepared, "7", true)
		f.validate(t, prepared, "8", false)
	}
	f.count(t, 1)
	if after := compilationTree(t, relocated); !reflect.DeepEqual(before, after) {
		t.Fatal("relocated read-only cache changed on a hit")
	}
}

func TestCompilationLocalExternalReferencesStayInvocationLocal(t *testing.T) {
	f := newCompilationFixture(t)
	writeCompilationFile(t, f.schema, []byte(`{"$ref":"number.json"}`), 0o600)
	reference := filepath.Join(f.dir, "number.json")
	writeCompilationFile(t, reference, []byte(`{"const":7}`), 0o600)
	c := openCompilationCache(t, f.cacheDir, false)
	first, err := f.prepare(t, c, false)
	if err != nil {
		t.Fatalf("compile must preserve the original local reference base: %v", err)
	}
	assertCompilationTemplate(t, first, 7, "one", "strict")
	f.validate(t, first, "7", true)
	writeCompilationFile(t, reference, []byte(`{"const":8}`), 0o600)
	second, err := f.prepare(t, c, false)
	if err != nil {
		t.Fatal(err)
	}
	assertCompilationTemplate(t, second, 8, "one", "strict")
	f.validate(t, second, "7", false)
	f.validate(t, second, "8", true)
	f.count(t, 2)
	before := compilationTree(t, f.cacheDir)
	if result, err := f.prepare(t, c, true); err == nil || result.Path != "" {
		t.Fatalf("verify-only accepted unproven local reference dependencies: %+v %v", result, err)
	}
	f.count(t, 2)
	if after := compilationTree(t, f.cacheDir); !reflect.DeepEqual(before, after) {
		t.Fatal("verify-only wrote an invocation-local compilation")
	}
}

func TestCompilationOversizedArtifactIsRejectedWithoutCaching(t *testing.T) {
	f := newCompilationFixture(t)
	f.task.Env = append(f.task.Env, "FAKE_OVERSIZE_ONCE="+filepath.Join(f.dir, "oversized-once"))
	c := openCompilationCache(t, f.cacheDir, false)
	if prepared, err := f.prepare(t, c, false); err == nil || fault.KindOf(err) != fault.Integrity || !strings.Contains(err.Error(), "compilation output exceeds limit") || prepared.Path != "" {
		t.Fatalf("oversized compiled artifact must be rejected: %+v %v", prepared, err)
	}
	for range 2 {
		prepared, err := f.prepare(t, c, false)
		if err != nil {
			t.Fatal(err)
		}
		assertCompilationTemplate(t, prepared, 7, "one", "strict")
		f.validate(t, prepared, "7", true)
		f.validate(t, prepared, "8", false)
	}
	f.count(t, 2)
}

func TestCompilationIgnoresUnrelatedRuntimeEnvironment(t *testing.T) {
	f := newCompilationFixture(t)
	c := openCompilationCache(t, f.cacheDir, false)
	first, err := f.prepare(t, c, false)
	if err != nil {
		t.Fatal(err)
	}
	f.task.Env = append(f.task.Env, "NODE_OPTIONS=--no-warnings", "PYTHONUNBUFFERED=1")
	second, err := f.prepare(t, c, false)
	if err != nil || second.Path != first.Path {
		t.Fatalf("native compilation changed for unrelated runtime settings: %+v %v", second, err)
	}
	assertCompilationTemplate(t, second, 7, "one", "strict")
	f.validate(t, second, "7", true)
	f.validate(t, second, "8", false)
	f.count(t, 1)
}

func TestCompilationLocationsAreStableWithoutChangingLiteralValues(t *testing.T) {
	snapshot := filepath.Join(t.TempDir(), "schema.json")
	base := (&url.URL{Scheme: "file", Path: filepath.ToSlash(snapshot)}).String()
	schemaDigest := digest.FromBytes([]byte(`{"type":"integer"}`))
	artifact, err := json.Marshal([]any{[]any{base + "#/type", 9007199254740993}, map[string]any{"literal": "keep " + base}})
	if err != nil {
		t.Fatal(err)
	}
	portable, err := portableTemplate(artifact, snapshot, schemaDigest)
	if err != nil {
		t.Fatal(err)
	}
	var result []json.RawMessage
	if err := json.Unmarshal(portable, &result); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(result[0], []byte("urn:schepherd:schema:"+digest.Hex(schemaDigest)+"#/type")) || !bytes.Contains(result[0], []byte("9007199254740993")) {
		t.Fatalf("compiler locations or numeric values changed: %s", portable)
	}
	var literal map[string]string
	if err := json.Unmarshal(result[1], &literal); err != nil || literal["literal"] != "keep "+base {
		t.Fatalf("schema literal was rewritten: %s %v", result[1], err)
	}
}

func TestCompilationCustomMetaschemaNeverBecomesPreparedCatalogState(t *testing.T) {
	for _, schema := range []string{
		`{"$schema":"file:///meta.json","const":7}`,
		`{"$schema":"https://json-schema.org/draft/2020-12/schema","$defs":{"nested":{"$schema":"file:///meta.json"}},"const":7}`,
	} {
		t.Run(schema, func(t *testing.T) {
			f := newCompilationFixture(t)
			data := []byte(schema)
			writeCompilationFile(t, f.schema, data, 0o600)
			c := openCompilationCache(t, f.cacheDir, false)
			compiler := &Compiler{Cache: c, RequirePersistent: true}
			defer compiler.Close()
			prepared, err := compiler.Prepare(context.Background(), f.definition, f.task, f.schema, data, true)
			if err == nil || prepared.Path != "" {
				t.Fatalf("catalog's custom metaschema became prepared without a dependency hash: %+v %v", prepared, err)
			}
			f.count(t, 0)
			compiler.VerifyOnly = true
			prepared, err = compiler.Prepare(context.Background(), f.definition, f.task, f.schema, data, true)
			if err == nil || prepared.Path != "" {
				t.Fatalf("offline preparation accepted unhashed metaschema dependencies: %+v %v", prepared, err)
			}
			f.count(t, 0)
		})
	}
}
