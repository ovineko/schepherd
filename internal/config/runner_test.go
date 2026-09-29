package config

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/runner"
)

func TestRunnerDefaultsWithPartialTable(t *testing.T) {
	cfg, dir := mustLoadDoc(t, "config_version = 1\n[runner]\ncommand = \"v\"\nargs = [\"{files...}\"]\n", nil)

	want := runner.Spec{
		Env: map[string]string{}, Mode: runner.ModeBatch, Command: "v", Cwd: "{workspace}", CwdBase: dir,
		Args: []string{"{files...}"}, Timeout: 5 * time.Minute, Jobs: 1, MaxArgsBytes: runner.DefaultMaxArgsBytes(), InheritEnv: true,
	}
	if !cfg.RunnerConfigured || !reflect.DeepEqual(cfg.Runner, want) {
		t.Errorf("runner = %+v, want %+v", cfg.Runner, want)
	}
}

func TestRunnerWithoutCommandIsNotConfigured(t *testing.T) {
	cfg, _ := mustLoadDoc(t, "config_version = 1\n[runner]\nmode = \"per-file\"\nargs = [\"{schema}\"]\n", nil)

	if cfg.RunnerConfigured || cfg.Runner.Mode != runner.ModePerFile {
		t.Errorf("runner = %+v, configured %v", cfg.Runner, cfg.RunnerConfigured)
	}
}

func TestRunnerCwdBaseFollowsDeclaringFile(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"base/b.toml":    "config_version = 1\n[runner]\ncwd = \"tools\"\n",
		"project/p.toml": "config_version = 1\nextends = [\"../base/b.toml\"]\n[runner]\ncommand = \"v\"\nargs = [\"{files...}\"]\n",
		"other/o.toml":   "config_version = 1\nextends = [\"../base/b.toml\"]\n[runner]\ncwd = \"{workspace}/sub\"\ncommand = \"v\"\nargs = [\"{files...}\"]\n",
	})

	cfg, err := Load(LoadOptions{Path: filepath.Join(dir, "project", "p.toml"), Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Runner.Cwd != "tools" || cfg.Runner.CwdBase != filepath.Join(dir, "base") {
		t.Errorf("cwd %q base %q", cfg.Runner.Cwd, cfg.Runner.CwdBase)
	}

	cfg, err = Load(LoadOptions{Path: filepath.Join(dir, "other", "o.toml"), Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Runner.Cwd != "{workspace}/sub" || cfg.Runner.CwdBase != filepath.Join(dir, "other") {
		t.Errorf("cwd %q base %q", cfg.Runner.Cwd, cfg.Runner.CwdBase)
	}
}

func TestRunnerValidation(t *testing.T) {
	cases := []struct {
		body     string
		fragment string
	}{
		{"mode = \"parallel\"", ":3:1: runner.mode: must be one of batch, per-file, stdin, got \"parallel\""},
		{"mode = \"\"", "runner.mode: must not be empty"},
		{"command = \"\"", "runner.command: must not be empty"},
		{"command = \"v {\"", "runner.command: template syntax error"},
		{"command = \"{schema}\"", "runner.command: placeholder {schema} is not allowed here (allowed: {workspace}, {cache})"},
		{"cwd = \"{file}\"", "runner.cwd: placeholder {file} is not allowed here"},
		{"cwd = \"\"", "runner.cwd: must not be empty"},
		{"args = [\"{nope}\"]", "runner.args[0]: template syntax error"},
		{"args = [\"x\", \"--in={files...}\"]", ":3:1: runner.args[1]: {files...} must be a whole argument"},
		{"args = [\"a\\u0000b\"]", "runner.args[0]: must not contain NUL characters"},
		{"timeout = \"soon\"", "runner.timeout: must be a positive duration such as \"90s\" or \"5m\", got \"soon\""},
		{"timeout = \"0s\"", "runner.timeout: must be a positive duration"},
		{"timeout = \"-5m\"", "runner.timeout: must be a positive duration"},
		{"jobs = 0", "runner.jobs: must be between 1 and 64, got 0"},
		{"jobs = 65", "runner.jobs: must be between 1 and 64, got 65"},
		{"max_args_bytes = 0", "runner.max_args_bytes: must be between 1 and 1073741824"},
		{"env = { \"A=B\" = \"x\" }", "runner.env.\"A=B\": invalid environment variable name \"A=B\""},
		{"env = { \"\" = \"x\" }", "invalid environment variable name \"\""},
		{"env = { FILES = \"{files...}\" }", "runner.env.FILES: placeholder {files...} is not allowed here"},
		{"command = \"v\"\nargs = [\"{schema}\"]", "schepherd.toml: runner.args: batch mode needs exactly one argument that is exactly {files...}"},
		{"command = \"v\"\nargs = [\"{files...}\", \"{files...}\"]", "batch mode needs exactly one argument"},
		{"command = \"v\"\nargs = [\"{file}\", \"{files...}\"]", "{file} is not available in batch mode"},
		{"command = \"v\"\nmode = \"per-file\"\nargs = [\"{schema}\"]", "per-file mode needs {file} in runner.args or runner.env"},
		{"command = \"v\"\nmode = \"stdin\"\nargs = [\"{files...}\"]", "{files...} is only available in batch mode"},
	}

	for _, tc := range cases {
		t.Run(tc.fragment, func(t *testing.T) {
			_, _, err := loadDoc(t, "config_version = 1\n[runner]\n"+tc.body+"\n", nil)
			wantUsage(t, err, tc.fragment)
		})
	}
}

func TestRunnerModes(t *testing.T) {
	cases := map[string]string{
		"batch":            "mode = \"batch\"\nargs = [\"{schema}\", \"{files...}\"]",
		"per-file via env": "mode = \"per-file\"\nargs = [\"{schema}\"]\nenv = { INPUT = \"{file}\" }",
		"stdin":            "mode = \"stdin\"\nargs = [\"--schema\", \"{schema}\"]",
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, _ := mustLoadDoc(t, "config_version = 1\n[runner]\ncommand = \"v\"\n"+body+"\n", nil)
			if !cfg.RunnerConfigured {
				t.Error("runner not configured")
			}
		})
	}
}

func TestMappings(t *testing.T) {
	cases := []struct {
		body     string
		fragment string
	}{
		{"file_match = [\"a.json\"]\nschema = \"Company\"", ":4:1: mappings[0].schema: schema id \"Company\" must match"},
		{"file_match = [\"a.json\"]\nschema = \"a..b\"", "must not contain \"..\""},
		{"file_match = [\"a.json\"]\nschema = \"-a\"", "schema id \"-a\" must match"},
		{"file_match = [\"a.json\"]\nschema = \"" + strings.Repeat("a", 129) + "\"", "must match"},
		{"file_match = [\"a.json\"]", ":2:3: mappings[0].schema: required key is missing"},
		{"schema = \"a\"", "mappings[0].file_match: required key is missing"},
		{"file_match = []\nschema = \"a\"", "mappings[0].file_match: must list at least one pattern"},
		{"file_match = [\"ok.json\", \"config/[.json\"]\nschema = \"a\"", ":3:1: mappings[0].file_match[1]: invalid pattern \"config/[.json\""},
		{"file_match = [\"config/\"]\nschema = \"a\"", "directory patterns are not supported"},
		{"file_match = [\"\"]\nschema = \"a\"", "invalid pattern \"\": empty"},
		{"file_match = [\"a.json\"]\nschema = \"a\"\nfileMatch = [\"b\"]", "mappings[0].fileMatch: unknown key"},
	}

	for _, tc := range cases {
		t.Run(tc.fragment, func(t *testing.T) {
			_, _, err := loadDoc(t, "config_version = 1\n[[mappings]]\n"+tc.body+"\n", nil)
			wantUsage(t, err, tc.fragment)
		})
	}

	cfg, _ := mustLoadDoc(t, "config_version = 1\nmappings = [{ file_match = [\"a.json\", \"!b/a.json\"], schema = \"a.b-c_d\" }]\n", nil)
	if want := []Mapping{{Schema: "a.b-c_d", FileMatch: []string{"a.json", "!b/a.json"}}}; !reflect.DeepEqual(cfg.Mappings, want) {
		t.Errorf("mappings = %+v", cfg.Mappings)
	}
}

func TestCheckRunnerEnv(t *testing.T) {
	cfg, _ := mustLoadDoc(t, `config_version = 1
[runner]
command = "${BIN}"
cwd = "${WORK}/x"
args = ["$${LITERAL}", "${ARG}", "{files...}"]

[runner.env]
TOKEN = "${SECRET}"
PLAIN = "1"
`, nil)

	if err := cfg.CheckRunnerEnv(lookupFrom(map[string]string{"BIN": "/b", "WORK": "/w", "ARG": "a", "SECRET": "s"})); err != nil {
		t.Errorf("all set: %v", err)
	}

	err := cfg.CheckRunnerEnv(lookupFrom(map[string]string{"WORK": "/w"}))
	wantUsage(t, err, "runner uses unset environment variables: ${BIN} in runner.command, ${ARG} in runner.args[1], ${SECRET} in runner.env.TOKEN")

	if strings.Contains(err.Error(), "LITERAL") || strings.Contains(err.Error(), "WORK") {
		t.Errorf("unexpected variable reported: %v", err)
	}

	wantUsage(t, cfg.CheckRunnerEnv(nil), "${BIN}", "${WORK}")

	emptyCwd, _ := mustLoadDoc(t, "config_version = 1\n[runner]\ncommand = \"${BIN}\"\ncwd = \"${WORK}\"\nargs = [\"{files...}\"]\n", nil)
	wantUsage(t, emptyCwd.CheckRunnerEnv(lookupFrom(map[string]string{"BIN": "/b", "WORK": ""})), "runner.cwd expands to an empty string")
	wantUsage(t, emptyCwd.CheckRunnerEnv(lookupFrom(map[string]string{"BIN": "", "WORK": "/w"})), "runner.command expands to an empty string")

	placeholderCwd, _ := mustLoadDoc(t, "config_version = 1\n[runner]\ncommand = \"v\"\ncwd = \"${WORK}{workspace}\"\nargs = [\"{files...}\"]\n", nil)
	if err := placeholderCwd.CheckRunnerEnv(lookupFrom(map[string]string{"WORK": ""})); err != nil {
		t.Errorf("a cwd with a placeholder is never empty: %v", err)
	}

	empty, err := Load(LoadOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	if err := empty.CheckRunnerEnv(nil); err != nil {
		t.Errorf("defaults: %v", err)
	}

	broken := &Config{Runner: runner.Spec{Command: "{"}}
	wantUsage(t, broken.CheckRunnerEnv(nil), "runner.command")
}

func TestSummary(t *testing.T) {
	cfg, dir := mustLoadDoc(t, `config_version = 1
cache_dir = "cache"

[catalog]
repository = "r.example/org/schemas"
digest = "`+testDigest+`"

[registries.localhost]
plain_http = true

[runner]
command = "${BIN}"
args = ["{schema}", "{files...}"]

[runner.env]
TOKEN = "${SECRET}"

[[mappings]]
file_match = ["a.json"]
schema = "a"
`, map[string]string{"BIN": "/very/secret/bin", "SECRET": "hunter2"})

	data, err := json.Marshal(cfg.Summary())
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(data), "hunter2") || strings.Contains(string(data), "/very/secret/bin") {
		t.Errorf("summary leaks environment values: %s", data)
	}

	r, ok := got["runner"].(map[string]any)
	if !ok {
		t.Fatalf("runner missing: %s", data)
	}

	checks := map[string][2]any{
		"files":          {got["files"], []any{filepath.Join(dir, "schepherd.toml")}},
		"workspace":      {got["workspace"], dir},
		"cacheDir":       {got["cacheDir"], filepath.Join(dir, "cache")},
		"offline":        {got["offline"], false},
		"timeout":        {got["timeout"], "10m0s"},
		"repository":     {got["repository"], "r.example/org/schemas"},
		"catalog":        {got["catalog"], testDigest},
		"limits":         {got["limits"].(map[string]any)["maxCatalogEntries"], float64(20000)},
		"registries":     {got["registries"], map[string]any{"localhost": map[string]any{"plainHttp": true}}},
		"mappings":       {got["mappings"], []any{map[string]any{"schema": "a", "fileMatch": []any{"a.json"}}}},
		"runner.command": {r["command"], "${BIN}"},
		"runner.env":     {r["env"], map[string]any{"TOKEN": "${SECRET}"}},
		"runner.args":    {r["args"], []any{"{schema}", "{files...}"}},
		"runner.cwd":     {r["cwd"], "{workspace}"},
		"runner.timeout": {r["timeout"], "5m0s"},
		"configured":     {r["configured"], true},
	}

	for name, c := range checks {
		if !reflect.DeepEqual(c[0], c[1]) {
			t.Errorf("%s = %#v, want %#v", name, c[0], c[1])
		}
	}

	defaults, err := Load(LoadOptions{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	data, err = json.Marshal(defaults.Summary())
	if err != nil {
		t.Fatal(err)
	}

	for _, fragment := range []string{`"files":[]`, `"mappings":[]`, `"registries":{}`, `"args":[]`, `"env":{}`, `"configured":false`} {
		if !strings.Contains(string(data), fragment) {
			t.Errorf("default summary %s lacks %s", data, fragment)
		}
	}
}
