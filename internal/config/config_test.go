package config

import (
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/runner"
)

func TestLoadWithoutFileUsesDefaults(t *testing.T) {
	cwd := t.TempDir()

	cfg, err := Load(LoadOptions{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Files) != 0 || cfg.Workspace != cwd || cfg.CacheDir != "" || cfg.Offline || cfg.Timeout != DefaultTimeout {
		t.Errorf("unexpected defaults: %+v", cfg)
	}

	if cfg.Repository != "" || cfg.Catalog != "" || len(cfg.Registries) != 0 || len(cfg.Mappings) != 0 {
		t.Errorf("unexpected catalog defaults: %+v", cfg)
	}

	if cfg.Limits != DefaultLimits() {
		t.Errorf("limits = %+v", cfg.Limits)
	}

	want := runner.Spec{
		Env: map[string]string{}, Mode: runner.ModeBatch, Cwd: "{workspace}", CwdBase: cwd, Args: []string{},
		Timeout: 5 * time.Minute, Jobs: 1, MaxArgsBytes: runner.DefaultMaxArgsBytes(), InheritEnv: true,
	}
	if cfg.RunnerConfigured || !reflect.DeepEqual(cfg.Runner, want) {
		t.Errorf("runner = %+v (configured %v), want %+v", cfg.Runner, cfg.RunnerConfigured, want)
	}
}

func TestDefaultLimits(t *testing.T) {
	want := Limits{MaxManifestBytes: 4 << 20, MaxCatalogBytes: 32 << 20, MaxPayloadBytes: 64 << 20, MaxSchemaBytes: 64 << 20, MaxCatalogEntries: 20000}
	if got := DefaultLimits(); got != want {
		t.Errorf("DefaultLimits() = %+v, want %+v", got, want)
	}
}

func TestLoadRejectsRelativeCwd(t *testing.T) {
	_, err := Load(LoadOptions{Cwd: "relative"})
	if fault.KindOf(err) != fault.Internal {
		t.Errorf("err = %v, want internal", err)
	}
}

func TestLoadResolvesRelativeConfigPathAgainstCwd(t *testing.T) {
	dir := writeTree(t, map[string]string{"sub/schepherd.toml": "config_version = 1\nworkspace = \"ws\"\n"})

	cfg, err := Load(LoadOptions{Path: filepath.Join("sub", "schepherd.toml"), Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	if want := filepath.Join(dir, "sub", "schepherd.toml"); !slices.Equal(cfg.Files, []string{want}) {
		t.Errorf("Files = %q, want %q", cfg.Files, want)
	}

	if want := filepath.Join(dir, "sub", "ws"); cfg.Workspace != want {
		t.Errorf("Workspace = %q, want %q", cfg.Workspace, want)
	}
}

func TestLoadFullDocument(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("..", "..", "testdata", "configs", "valid", "full.toml"))
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Dir(path)

	cfg, err := Load(LoadOptions{Path: path, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Workspace != filepath.Join(dir, "workspace") || cfg.CacheDir != filepath.Join(dir, ".cache", "schepherd") || !cfg.Offline {
		t.Errorf("paths: %+v", cfg)
	}

	if cfg.Repository != "registry.example:5000/org/schemas" || cfg.Catalog != testDigest {
		t.Errorf("catalog: %q %q", cfg.Repository, cfg.Catalog)
	}

	wantLimits := Limits{MaxManifestBytes: 1048576, MaxCatalogBytes: 2097152, MaxCatalogEntries: 500, MaxPayloadBytes: 4194304, MaxSchemaBytes: 8388608}
	if cfg.Limits != wantLimits {
		t.Errorf("limits = %+v", cfg.Limits)
	}

	wantRegistries := map[string]Registry{
		"registry.example:5000": {CAFile: filepath.Join(dir, "certs", "ca.pem"), CredentialsFile: filepath.Join(dir, "docker", "config.json")},
		"localhost":             {PlainHTTP: true},
	}
	if !reflect.DeepEqual(cfg.Registries, wantRegistries) {
		t.Errorf("registries = %+v", cfg.Registries)
	}

	wantRunner := runner.Spec{
		Env:          map[string]string{"NO_COLOR": "1", "SCHEMA_REF": "{schema-ref}", "CACHE": "{cache}"},
		Mode:         runner.ModePerFile,
		Command:      "{workspace}/bin/validate",
		Cwd:          "{workspace}",
		CwdBase:      dir,
		Args:         []string{"--schema", "{schema}", "--id={schema-id}", "{file}"},
		Timeout:      90 * time.Second,
		Jobs:         4,
		MaxArgsBytes: 65536,
		FailFast:     true,
		InheritEnv:   false,
	}
	if !cfg.RunnerConfigured || !reflect.DeepEqual(cfg.Runner, wantRunner) {
		t.Errorf("runner = %+v, want %+v", cfg.Runner, wantRunner)
	}

	wantMappings := []Mapping{
		{Schema: "company-config", FileMatch: []string{"config/company.json", "!config/legacy/**"}},
		{Schema: "github-workflow", FileMatch: []string{"**/.github/workflows/*.yml"}},
	}
	if !reflect.DeepEqual(cfg.Mappings, wantMappings) {
		t.Errorf("mappings = %+v", cfg.Mappings)
	}
}

func TestLoadLayeredFixture(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "testdata", "configs", "valid", "layered"))
	if err != nil {
		t.Fatal(err)
	}

	base := filepath.Join(root, "base")
	project := filepath.Join(root, "project")

	cfg, err := Load(LoadOptions{Path: filepath.Join(project, "schepherd.toml"), Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	wantFiles := []string{filepath.Join(base, "common.toml"), filepath.Join(base, "base.toml"), filepath.Join(project, "schepherd.toml")}
	if !slices.Equal(cfg.Files, wantFiles) {
		t.Errorf("Files = %q, want %q", cfg.Files, wantFiles)
	}

	if cfg.Workspace != root {
		t.Errorf("Workspace = %q, want %q (relative to the base that declared it)", cfg.Workspace, root)
	}

	if want := filepath.Join(project, ".cache"); cfg.CacheDir != want {
		t.Errorf("CacheDir = %q, want %q", cfg.CacheDir, want)
	}

	if cfg.Repository != "registry.example/org/schemas" || cfg.Catalog != testDigest {
		t.Errorf("catalog: %q %q", cfg.Repository, cfg.Catalog)
	}

	wantLimits := DefaultLimits()
	wantLimits.MaxCatalogEntries = 1000
	wantLimits.MaxManifestBytes = 1048576
	wantLimits.MaxSchemaBytes = 2097152

	if cfg.Limits != wantLimits {
		t.Errorf("limits = %+v, want %+v", cfg.Limits, wantLimits)
	}

	wantRegistry := Registry{CAFile: filepath.Join(base, "certs", "ca.pem"), CredentialsFile: filepath.Join(project, "auth", "config.json")}
	if got := cfg.Registries["registry.example"]; got != wantRegistry {
		t.Errorf("registry = %+v, want %+v", got, wantRegistry)
	}

	wantRunner := runner.Spec{
		Env:          map[string]string{"FROM_COMMON": "common", "SHARED": "project", "BASE_ONLY": "1"},
		Mode:         runner.ModeBatch,
		Command:      "validator",
		Cwd:          "tools",
		CwdBase:      base,
		Args:         []string{"validate", "{schema}", "{files...}"},
		Timeout:      30 * time.Second,
		Jobs:         2,
		MaxArgsBytes: runner.DefaultMaxArgsBytes(),
		InheritEnv:   true,
	}
	if !reflect.DeepEqual(cfg.Runner, wantRunner) {
		t.Errorf("runner = %+v, want %+v", cfg.Runner, wantRunner)
	}

	if want := []Mapping{{Schema: "project-schema", FileMatch: []string{"project/*.json"}}}; !reflect.DeepEqual(cfg.Mappings, want) {
		t.Errorf("mappings = %+v, want %+v", cfg.Mappings, want)
	}
}

func TestExtendsMerge(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"shared/a.toml": `config_version = 1
workspace = "a-ws"
cache_dir = "a-cache"
offline = true

[limits]
max_manifest_bytes = 100
max_catalog_bytes = 200

[registries."r.example"]
ca_file = "a-ca.pem"
plain_http = true

[runner]
command = "a-cmd"
args = ["a1", "{files...}", "a2"]
cwd = "a-cwd"

[runner.env]
A = "a"
SHARED = "a"

[[mappings]]
file_match = ["a.json"]
schema = "a"

[[mappings]]
file_match = ["a2.json"]
schema = "a2"
`,
		"other/b.toml": `config_version = 1
workspace = "b-ws"

[limits]
max_catalog_bytes = 300

[registries."r.example"]
credentials_file = "b-auth.json"

[runner.env]
SHARED = "b"
B = "b"
`,
		"project/c.toml": `config_version = 1
extends = ["../shared/a.toml", "../other/b.toml"]
offline = false

[runner]
args = ["{files...}"]

[runner.env]
C = "c"

[[mappings]]
file_match = ["c.json"]
schema = "c"
`,
	})

	cfg, err := Load(LoadOptions{Path: filepath.Join(dir, "project", "c.toml"), Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	shared, other := filepath.Join(dir, "shared"), filepath.Join(dir, "other")

	checks := []struct {
		name      string
		got, want any
	}{
		{"files", cfg.Files, []string{filepath.Join(shared, "a.toml"), filepath.Join(other, "b.toml"), filepath.Join(dir, "project", "c.toml")}},
		{"later base overrides earlier base", cfg.Workspace, filepath.Join(other, "b-ws")},
		{"path keeps the origin of its declaring base", cfg.CacheDir, filepath.Join(shared, "a-cache")},
		{"child scalar overrides base with false", cfg.Offline, false},
		{"limits merge by key", [2]int64{cfg.Limits.MaxManifestBytes, cfg.Limits.MaxCatalogBytes}, [2]int64{100, 300}},
		{"registry fields merge with their own origins", cfg.Registries["r.example"], Registry{PlainHTTP: true, CAFile: filepath.Join(shared, "a-ca.pem"), CredentialsFile: filepath.Join(other, "b-auth.json")}},
		{"runner.args replace", cfg.Runner.Args, []string{"{files...}"}},
		{"runner scalars survive a partial child table", cfg.Runner.Command, "a-cmd"},
		{"cwd base is the declaring file", [2]string{cfg.Runner.Cwd, cfg.Runner.CwdBase}, [2]string{"a-cwd", shared}},
		{"runner.env merges by key", cfg.Runner.Env, map[string]string{"A": "a", "B": "b", "C": "c", "SHARED": "b"}},
		{"mappings replace", cfg.Mappings, []Mapping{{Schema: "c", FileMatch: []string{"c.json"}}}},
	}

	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s: got %#v, want %#v", c.name, c.got, c.want)
		}
	}
}

func TestExtendsEmptyArraysReplace(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"base.toml": `config_version = 1
[runner]
command = "v"
mode = "stdin"
args = ["x", "y"]

[[mappings]]
file_match = ["a.json"]
schema = "a"
`,
		"child.toml": `config_version = 1
extends = ["base.toml"]
mappings = []

[runner]
args = []
`,
	})

	cfg, err := Load(LoadOptions{Path: filepath.Join(dir, "child.toml"), Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Runner.Args) != 0 || len(cfg.Mappings) != 0 {
		t.Errorf("empty arrays must replace: args %q mappings %+v", cfg.Runner.Args, cfg.Mappings)
	}
}

func TestExtendsDiamondAndAbsolutePaths(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"d.toml": "config_version = 1\nworkspace = \"d\"\n",
		"b.toml": "config_version = 1\nextends = [\"d.toml\"]\nworkspace = \"b\"\n",
		"c.toml": "config_version = 1\nextends = [\"d.toml\"]\n",
	})

	top := fmt.Sprintf("config_version = 1\nextends = [%q, %q]\n", filepath.Join(dir, "b.toml"), filepath.Join(dir, "c.toml"))
	topDir := writeTree(t, map[string]string{"a.toml": top})

	cfg, err := Load(LoadOptions{Path: filepath.Join(topDir, "a.toml"), Cwd: topDir})
	if err != nil {
		t.Fatal(err)
	}

	wantFiles := []string{filepath.Join(dir, "d.toml"), filepath.Join(dir, "b.toml"), filepath.Join(dir, "c.toml"), filepath.Join(topDir, "a.toml")}
	if !slices.Equal(cfg.Files, wantFiles) {
		t.Errorf("Files = %q, want %q", cfg.Files, wantFiles)
	}

	// c inherits workspace from d, and c's merged view applies after b's.
	if want := filepath.Join(dir, "d"); cfg.Workspace != want {
		t.Errorf("Workspace = %q, want %q", cfg.Workspace, want)
	}
}

func TestExtendsErrors(t *testing.T) {
	chain := func(n int) map[string]string {
		files := map[string]string{}
		for i := range n {
			files[fmt.Sprintf("f%02d.toml", i)] = fmt.Sprintf("config_version = 1\nextends = [\"f%02d.toml\"]\n", i+1)
		}

		files[fmt.Sprintf("f%02d.toml", n)] = "config_version = 1\n"

		return files
	}

	cases := []struct {
		files     map[string]string
		name      string
		fragments []string
	}{
		{
			name:      "cycle",
			files:     map[string]string{"f00.toml": "config_version = 1\nextends = [\"b.toml\"]\n", "b.toml": "config_version = 1\nextends = [\"f00.toml\"]\n"},
			fragments: []string{"b.toml:2:1: extends[0]: extends cycle: ", "f00.toml -> ", "b.toml -> ", "f00.toml"},
		},
		{
			name:      "self",
			files:     map[string]string{"f00.toml": "config_version = 1\nextends = [\"./f00.toml\"]\n"},
			fragments: []string{"f00.toml:2:1: extends[0]: extends cycle: ", "f00.toml -> "},
		},
		{
			name:      "missing base",
			files:     map[string]string{"f00.toml": "config_version = 1\n\nextends = [\"nope.toml\"]\n"},
			fragments: []string{"f00.toml:3:1: extends[0]: cannot read base configuration", "nope.toml"},
		},
		{
			name:      "directory base",
			files:     map[string]string{"f00.toml": "config_version = 1\nextends = [\"sub\"]\n", "sub/x.toml": ""},
			fragments: []string{"extends[0]: cannot read base configuration", "is not a regular file"},
		},
		{
			name:      "remote base",
			files:     map[string]string{"f00.toml": "config_version = 1\nextends = [\"https://example.com/b.toml\"]\n"},
			fragments: []string{"f00.toml:2:1: extends[0]: ", "not a local file"},
		},
		{
			name:      "scheme without slashes",
			files:     map[string]string{"f00.toml": "config_version = 1\nextends = [\"file:b.toml\"]\n"},
			fragments: []string{"extends[0]", "not a local file", "./file:b.toml"},
		},
		{
			name:      "empty entry",
			files:     map[string]string{"f00.toml": "config_version = 1\nextends = [\"\"]\n"},
			fragments: []string{"extends[0]: must not be empty"},
		},
		{
			name:      "wrong element type",
			files:     map[string]string{"f00.toml": "config_version = 1\nextends = [1]\n"},
			fragments: []string{"extends[0]: expected a string, got an integer"},
		},
		{
			name:      "not an array",
			files:     map[string]string{"f00.toml": "config_version = 1\nextends = \"b.toml\"\n"},
			fragments: []string{"f00.toml:2:1: extends: expected an array, got a string"},
		},
		{
			name:      "depth above limit",
			files:     chain(maxExtendsDepth + 1),
			fragments: []string{"f16.toml:2:1: extends[0]: extends chain is deeper than 16 levels"},
		},
		{
			name:      "invalid base names the base file",
			files:     map[string]string{"f00.toml": "config_version = 1\nextends = [\"b.toml\"]\n", "b.toml": "config_version = 1\n\n[runner]\ncmd = \"x\"\n"},
			fragments: []string{"b.toml:4:1: runner.cmd: unknown key"},
		},
		{
			name:      "base without version",
			files:     map[string]string{"f00.toml": "config_version = 1\nextends = [\"b.toml\"]\n", "b.toml": "offline = true\n"},
			fragments: []string{"b.toml: config_version: required key is missing"},
		},
		{
			name:      "base with another version",
			files:     map[string]string{"f00.toml": "config_version = 1\nextends = [\"b.toml\"]\n", "b.toml": "config_version = 2\n"},
			fragments: []string{"b.toml:1:1: config_version: unsupported configuration version 2"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeTree(t, tc.files)
			_, err := Load(LoadOptions{Path: filepath.Join(dir, "f00.toml"), Cwd: dir})
			wantUsage(t, err, tc.fragments...)
		})
	}
}

func TestExtendsDepthLimitIsInclusive(t *testing.T) {
	files := map[string]string{}
	for i := range maxExtendsDepth {
		files[fmt.Sprintf("f%02d.toml", i)] = fmt.Sprintf("config_version = 1\nextends = [\"f%02d.toml\"]\n", i+1)
	}

	files[fmt.Sprintf("f%02d.toml", maxExtendsDepth)] = "config_version = 1\nworkspace = \"deepest\"\n"
	dir := writeTree(t, files)

	cfg, err := Load(LoadOptions{Path: filepath.Join(dir, "f00.toml"), Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Files) != maxExtendsDepth+1 || cfg.Workspace != filepath.Join(dir, "deepest") {
		t.Errorf("Files = %d, Workspace = %q", len(cfg.Files), cfg.Workspace)
	}
}

func TestExtendsFanOutIsBounded(t *testing.T) {
	files := map[string]string{}
	for i := range 10 {
		next := fmt.Sprintf("f%02d.toml", i+1)
		files[fmt.Sprintf("f%02d.toml", i)] = fmt.Sprintf("config_version = 1\nextends = [%q, %q]\n", next, next)
	}

	files["f10.toml"] = "config_version = 1\n"
	dir := writeTree(t, files)

	_, err := Load(LoadOptions{Path: filepath.Join(dir, "f00.toml"), Cwd: dir})
	wantUsage(t, err, "extends expands to more than 256 files")
}

func TestLoadTopLevelFileErrors(t *testing.T) {
	dir := writeTree(t, map[string]string{"big.toml": "config_version = 1\n" + strings.Repeat("# padding\n", maxFileBytes/10+1)})

	_, err := Load(LoadOptions{Path: filepath.Join(dir, "missing.toml"), Cwd: dir})
	wantUsage(t, err, "cannot read configuration file", "missing.toml")

	_, err = Load(LoadOptions{Path: dir, Cwd: dir})
	wantUsage(t, err, "is not a regular file")

	_, err = Load(LoadOptions{Path: filepath.Join(dir, "big.toml"), Cwd: dir})
	wantUsage(t, err, "is larger than 1048576 bytes")
}

func TestKeysAreCheckedCaseSensitively(t *testing.T) {
	cases := []struct {
		doc       string
		fragments []string
	}{
		{"config_version = 1\n\n[Runner]\ncommand = \"x\"\n", []string{":3:2: Runner: unknown key (keys are case-sensitive; did you mean \"runner\"?)"}},
		{"config_version = 1\n[runner]\ncmd = \"x\"\n", []string{":3:1: runner.cmd: unknown key"}},
		{"Config_version = 1\n", []string{":1:1: Config_version: unknown key", "did you mean \"config_version\"", "config_version: required key is missing"}},
		{"config_version = 1\nverbose = true\n", []string{":2:1: verbose: unknown key"}},
		{"config_version = 1\n[catalog]\nDigest = \"x\"\n", []string{"catalog.Digest: unknown key", "did you mean \"digest\""}},
		{"config_version = 1\n[limits]\nmax_bytes = 1\n", []string{":3:1: limits.max_bytes: unknown key"}},
		{"config_version = 1\n[registries.\"r.example\"]\ncaFile = \"x\"\n", []string{":3:1: registries.\"r.example\".caFile: unknown key"}},
		{"config_version = 1\n[runner.env]\nOK = \"1\"\n[runner.Env]\nX = \"1\"\n", []string{"runner.Env: unknown key", "did you mean \"env\""}},
		{"config_version = 1\n[[mappings]]\nschema = \"a\"\nfile_match = [\"a\"]\n\n[[mappings]]\nschema = \"b\"\nFile_match = [\"b\"]\n", []string{":8:1: mappings[1].File_match: unknown key", ":6:3: mappings[1].file_match: required key is missing"}},
		{"config_version = 1\nmappings = [{schema = \"a\", file_match = [\"a\"], extra = 1}]\n", []string{":2:48: mappings[0].extra: unknown key"}},
		{"config_version = 1\nrunner.cmd = \"x\"\n", []string{":2:8: runner.cmd: unknown key"}},
		{"config_version = 1\n[[mappings]]\nschema = \"a\"\nfile_match = [\"a\"]\n[mappings.sub]\nx = 1\n", []string{":5:11: mappings[0].sub: unknown key"}},
	}

	for i, tc := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			_, _, err := loadDoc(t, tc.doc, nil)
			wantUsage(t, err, tc.fragments...)
		})
	}
}

func TestTypeErrors(t *testing.T) {
	cases := []struct {
		doc      string
		fragment string
	}{
		{"config_version = 1\noffline = \"yes\"\n", ":2:1: offline: expected a boolean, got a string"},
		{"config_version = 1\nworkspace = 1\n", "workspace: expected a string, got an integer"},
		{"config_version = 1\ncache_dir = []\n", "cache_dir: expected a string, got an array"},
		{"config_version = 1\ncatalog = \"x\"\n", "catalog: expected a table, got a string"},
		{"config_version = 1\n[limits]\nmax_schema_bytes = 1.5\n", "limits.max_schema_bytes: expected an integer, got a float"},
		{"config_version = 1\nregistries = 1\n", "registries: expected a table, got an integer"},
		{"config_version = 1\n[registries]\nlocalhost = true\n", "registries.localhost: expected a table, got a boolean"},
		{"config_version = 1\n[registries.localhost]\nplain_http = \"true\"\n", "registries.localhost.plain_http: expected a boolean, got a string"},
		{"config_version = 1\n[runner]\njobs = \"4\"\n", ":3:1: runner.jobs: expected an integer, got a string"},
		{"config_version = 1\n[runner]\nargs = \"x\"\n", "runner.args: expected an array, got a string"},
		{"config_version = 1\n[runner]\nargs = [\"x\", 2]\n", "runner.args[1]: expected a string, got an integer"},
		{"config_version = 1\n[runner]\ntimeout = 60\n", "runner.timeout: expected a string, got an integer"},
		{"config_version = 1\n[runner]\nfail_fast = 1979-05-27\n", "runner.fail_fast: expected a boolean, got a date or time"},
		{"config_version = 1\n[runner.env]\nDEBUG = 1\n", "runner.env.DEBUG: expected a string, got an integer"},
		{"config_version = 1\nmappings = {schema = \"a\"}\n", "mappings: expected an array, got a table"},
		{"config_version = 1\nmappings = [\"a\"]\n", "mappings[0]: expected a table, got a string"},
		{"config_version = 1\n[[mappings]]\nschema = 1\nfile_match = [\"a\"]\n", "mappings[0].schema: expected a string, got an integer"},
		{"config_version = 1\n[[mappings]]\nschema = \"a\"\nfile_match = \"a\"\n", "mappings[0].file_match: expected an array, got a string"},
	}

	for _, tc := range cases {
		t.Run(tc.fragment, func(t *testing.T) {
			_, _, err := loadDoc(t, tc.doc, nil)
			wantUsage(t, err, tc.fragment)
		})
	}
}

func TestConfigVersion(t *testing.T) {
	cases := []struct {
		doc      string
		fragment string
	}{
		{"", "config_version: required key is missing; add config_version = 1"},
		{"offline = true\n", "config_version: required key is missing"},
		{"config_version = 2\nunknown = 1\n", ":1:1: config_version: unsupported configuration version 2"},
		{"config_version = 0\n", "unsupported configuration version 0"},
		{"config_version = \"1\"\n", "config_version: expected an integer, got a string"},
		{"config_version = 1.0\n", "config_version: expected an integer, got a float"},
	}

	for _, tc := range cases {
		t.Run(tc.fragment, func(t *testing.T) {
			_, _, err := loadDoc(t, tc.doc, nil)
			wantUsage(t, err, tc.fragment)

			if strings.Contains(tc.doc, "unknown") && strings.Contains(err.Error(), "unknown key") {
				t.Errorf("a document of another version must not be checked further: %v", err)
			}
		})
	}
}

func TestInvalidTOML(t *testing.T) {
	cases := []struct {
		doc      string
		fragment string
	}{
		{"config_version = 1\noffline = \n", ":2:11: invalid TOML: "},
		{"config_version = 1\nconfig_version = 1\n", ":2:1: invalid TOML: key config_version is already defined"},
		{"config_version = 1\n[runner]\n[runner]\n", ":3:2: invalid TOML: "},
	}

	for _, tc := range cases {
		_, _, err := loadDoc(t, tc.doc, nil)
		wantUsage(t, err, tc.fragment)
	}
}

func TestProblemsAreSortedAndCapped(t *testing.T) {
	var doc strings.Builder

	doc.WriteString("config_version = 1\n")

	for i := range maxProblems + 5 {
		fmt.Fprintf(&doc, "unknown_%02d = 1\n", i)
	}

	_, _, err := loadDoc(t, doc.String(), nil)
	wantUsage(t, err, ":2:1: unknown_00: unknown key\n", ":3:1: unknown_01: unknown key", "5 more problems")

	if strings.Contains(err.Error(), "unknown_24") {
		t.Errorf("problems beyond the cap must be summarized: %v", err)
	}
}

func TestValueValidation(t *testing.T) {
	cases := []struct {
		doc      string
		fragment string
	}{
		{"config_version = 1\nworkspace = \"\"\n", "workspace: must not be empty"},
		{"config_version = 1\ncache_dir = \"{cache}\"\n", "cache_dir: placeholder {cache} is not allowed here"},
		{"config_version = 1\nworkspace = \"{workspace}\"\n", "workspace: placeholder {workspace} is not allowed here"},
		{"config_version = 1\nworkspace = \"a\\u0000b\"\n", "workspace: must not contain NUL characters"},
		{"config_version = 1\nworkspace = \"${1BAD}\"\n", "workspace: template syntax error"},
		{"config_version = 1\n[limits]\nmax_payload_bytes = 0\n", "limits.max_payload_bytes: must be between 1 and 1073741824, got 0"},
		{"config_version = 1\n[limits]\nmax_schema_bytes = 1073741825\n", "must be between 1 and 1073741824"},
		{"config_version = 1\n[limits]\nmax_catalog_entries = 1000001\n", "limits.max_catalog_entries: must be between 1 and 1000000"},
		{"config_version = 1\n[limits]\nmax_manifest_bytes = -1\n", "limits.max_manifest_bytes: must be between 1"},
		{"config_version = 1\n[registries.\"https://r.example\"]\nplain_http = true\n", "registries.\"https://r.example\": registry key must be host[:port]"},
		{"config_version = 1\n[registries.\"r.example/Org\"]\nplain_http = true\n", "registries.\"r.example/Org\": registry key must be host[:port] or host[:port]/path without scheme, tag or digest; path component \"Org\""},
		{"config_version = 1\n[registries.\"r.example/org/\"]\nplain_http = true\n", "registry key must be host[:port] or host[:port]/path without scheme, tag or digest; path component \"\""},
		{"config_version = 1\n[registries.\"r.example/org/schemas:v1\"]\nplain_http = true\n", "registry key must be host[:port] or host[:port]/path without scheme, tag or digest"},
		{"config_version = 1\n[registries.\"LOCALHOST:1\"]\nplain_http = true\n", "registries.\"LOCALHOST:1\": registry hosts and repository paths must be lower case; write host \"LOCALHOST:1\" as \"localhost:1\""},
		{"config_version = 1\n[registries.\"GHCR.io/org\"]\nca_file = \"ca.pem\"\n", "registries.\"GHCR.io/org\": registry hosts and repository paths must be lower case; write host \"GHCR.io\" as \"ghcr.io\""},
		{"config_version = 1\n[registries.\"[FE80::1]:5000\"]\ncredentials_file = \"auth.json\"\n", "registries.\"[FE80::1]:5000\": registry hosts and repository paths must be lower case"},
		{"config_version = 1\n[registries.\"localhost:0\"]\nplain_http = true\n", "registries.\"localhost:0\": host \"localhost:0\": the port must be between 1 and 65535"},
		{"config_version = 1\n[registries.\"[1.2.3.4]:5000/org\"]\nplain_http = true\n", "registries.\"[1.2.3.4]:5000/org\": host \"[1.2.3.4]:5000\": \"1.2.3.4\" is not an IPv6 address"},
		{"config_version = 1\n[registries.localhost]\nca_file = \"\"\n", "registries.localhost.ca_file: must not be empty"},
		{"config_version = 1\n[registries.localhost]\ncredentials_file = \"{workspace}/c.json\"\n", "credentials_file: placeholder {workspace} is not allowed here"},
		{"config_version = 1\n[catalog]\nrepository = \"\"\n", "catalog.repository: must not be empty"},
		{"config_version = 1\n[catalog]\nrepository = \"{cache}\"\n", "catalog.repository: placeholder {cache} is not allowed here"},
	}

	for _, tc := range cases {
		t.Run(tc.fragment, func(t *testing.T) {
			_, _, err := loadDoc(t, tc.doc, nil)
			wantUsage(t, err, tc.fragment)
		})
	}
}

func TestRegistryHosts(t *testing.T) {
	cfg, _ := mustLoadDoc(t, `config_version = 1
[registries."localhost:5000"]
plain_http = true
[registries."[::1]:5000"]
plain_http = true
[registries."registry-1.example.com"]
plain_http = false
`, nil)

	if len(cfg.Registries) != 3 || !cfg.Registries["localhost:5000"].PlainHTTP || !cfg.Registries["[::1]:5000"].PlainHTTP {
		t.Errorf("registries = %+v", cfg.Registries)
	}
}

func TestRegistryRepositoryPrefixes(t *testing.T) {
	cfg, dir := mustLoadDoc(t, `config_version = 1
[registries."ghcr.io"]
ca_file = "host.pem"
[registries."ghcr.io/org-a"]
credentials_file = "a.json"
[registries."localhost:5000/org-b/team"]
plain_http = true
credentials_file = "b.json"
`, nil)

	want := map[string]Registry{
		"ghcr.io":                   {CAFile: filepath.Join(dir, "host.pem")},
		"ghcr.io/org-a":             {CredentialsFile: filepath.Join(dir, "a.json")},
		"localhost:5000/org-b/team": {PlainHTTP: true, CredentialsFile: filepath.Join(dir, "b.json")},
	}
	if !reflect.DeepEqual(cfg.Registries, want) {
		t.Errorf("registries = %+v, want %+v", cfg.Registries, want)
	}
}
