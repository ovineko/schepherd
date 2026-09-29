package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaURL = "https://schepherd.invalid/api/config.schema.json"

var fixturesRoot = filepath.Join("..", "..", "testdata", "configs")

// invalidFixtures names the problem every fixture under invalid/ must be
// rejected for, so that a fixture cannot pass by failing for another reason.
var invalidFixtures = map[string][]string{
	"schema/unknown-top-key.toml":          {"unknown-top-key.toml:2:1: verbose: unknown key"},
	"schema/wrong-case-table.toml":         {"Runner: unknown key (keys are case-sensitive; did you mean \"runner\"?)"},
	"schema/unknown-runner-key.toml":       {"unknown-runner-key.toml:4:1: runner.cmd: unknown key"},
	"schema/version-missing.toml":          {"config_version: required key is missing"},
	"schema/version-2.toml":                {"unsupported configuration version 2"},
	"schema/version-string.toml":           {"config_version: expected an integer, got a string"},
	"schema/wrong-type-offline.toml":       {"offline: expected a boolean, got a string"},
	"schema/wrong-type-jobs.toml":          {"runner.jobs: expected an integer, got a string"},
	"schema/jobs-out-of-range.toml":        {"runner.jobs: must be between 1 and 64, got 65"},
	"schema/limits-zero.toml":              {"limits.max_payload_bytes: must be between 1 and 1073741824, got 0"},
	"schema/limits-too-large.toml":         {"limits.max_catalog_entries: must be between 1 and 1000000, got 1000001"},
	"schema/bad-mode.toml":                 {"runner.mode: must be one of batch, per-file, stdin"},
	"schema/empty-command.toml":            {"runner.command: must not be empty"},
	"schema/timeout-not-duration.toml":     {"runner.timeout: must be a positive duration"},
	"schema/timeout-integer.toml":          {"runner.timeout: expected a string, got an integer"},
	"schema/env-bad-name.toml":             {"invalid environment variable name \"A=B\""},
	"schema/env-not-string.toml":           {"runner.env.DEBUG: expected a string, got an integer"},
	"schema/digest-tag.toml":               {"catalog.digest: \"catalog-latest\" looks like a tag", "schepherd pin"},
	"schema/digest-reference.toml":         {"looks like a tag", "schepherd pin"},
	"schema/digest-short.toml":             {"catalog.digest: invalid digest", "schepherd pin"},
	"schema/repository-tag.toml":           {"catalog.repository: ", "must not contain a tag"},
	"schema/repository-scheme.toml":        {"without scheme"},
	"schema/repository-empty.toml":         {"catalog.repository: must not be empty"},
	"schema/remote-extends.toml":           {"extends[0]: \"https://example.com/base.toml\" is not a local file"},
	"schema/registry-bad-host.toml":        {"registry key must be host[:port]"},
	"schema/registry-unknown-key.toml":     {"registries.\"registry.example\".insecure: unknown key"},
	"schema/mapping-bad-id.toml":           {"mappings[0].schema: schema id \"Company Config\" must match"},
	"schema/mapping-dotdot-id.toml":        {"must not contain \"..\""},
	"schema/mapping-missing-schema.toml":   {"mappings[0].schema: required key is missing"},
	"schema/mapping-empty-file-match.toml": {"mappings[0].file_match: must list at least one pattern"},
	"schema/mapping-unknown-key.toml":      {"mappings[0].fileMatch: unknown key"},
	"load/cycle-a.toml":                    {"extends cycle"},
	"load/cycle-b.toml":                    {"extends cycle"},
	"load/self.toml":                       {"self.toml:2:1: extends[0]: extends cycle"},
	"load/missing-base.toml":               {"missing-base.toml:2:1: extends[0]: cannot read base configuration", "does-not-exist.toml"},
	"load/invalid-base.toml":               {"unknown-runner-key.toml:4:1: runner.cmd: unknown key"},
	"load/batch-without-files.toml":        {"batch mode needs exactly one argument"},
	"load/per-file-without-file.toml":      {"per-file mode needs {file}"},
	"load/file-in-batch.toml":              {"{file} is not available in batch mode"},
	"load/files-inside-arg.toml":           {"runner.args[0]: {files...} must be a whole argument"},
	"load/unknown-placeholder.toml":        {"runner.args[0]: template syntax error", "unknown placeholder {nope}"},
	"load/placeholder-in-workspace.toml":   {"workspace: placeholder {cache} is not allowed here"},
	"load/lone-brace.toml":                 {"runner.command: template syntax error"},
	"load/timeout-zero.toml":               {"runner.timeout: must be a positive duration"},
	"load/bad-glob.toml":                   {"mappings[0].file_match[0]: invalid pattern \"config/[.json\""},
	"load/directory-pattern.toml":          {"directory patterns are not supported"},
}

var fixtureVars = map[string]string{
	"SCHEPHERD_REPOSITORY": "registry.example/org/schemas",
	"SCHEPHERD_CATALOG":    testDigest,
	"JSONSCHEMA_BIN":       "/opt/jsonschema/bin/jsonschema",
	"API_TOKEN":            "token",
}

type fixture struct {
	rel  string
	path string
}

func fixtures(tb testing.TB) []fixture {
	tb.Helper()

	var out []fixture

	err := filepath.WalkDir(fixturesRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".toml" {
			return err
		}

		rel, err := filepath.Rel(fixturesRoot, path)
		if err != nil {
			return err
		}

		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}

		out = append(out, fixture{rel: filepath.ToSlash(rel), path: abs})

		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}

	if len(out) == 0 {
		tb.Fatalf("no fixtures under %s", fixturesRoot)
	}

	return out
}

type denyLoader struct{}

func (denyLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("the configuration schema must be self-contained, but it references %s", url)
}

func compileConfigSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("..", "..", "api", "config.schema.json"))
	if err != nil {
		t.Fatal(err)
	}

	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	c := jsonschema.NewCompiler()
	c.UseLoader(denyLoader{})

	if err := c.AddResource(schemaURL, doc); err != nil {
		t.Fatal(err)
	}

	schema, err := c.Compile(schemaURL)
	if err != nil {
		t.Fatalf("compile api/config.schema.json: %v", err)
	}

	return schema
}

func tomlAsJSON(t *testing.T, data []byte) any {
	t.Helper()

	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%q: %v", data, err)
	}

	if doc == nil {
		doc = map[string]any{}
	}

	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}

	return value
}

func TestFixturesAgainstSchema(t *testing.T) {
	schema := compileConfigSchema(t)

	for _, fx := range fixtures(t) {
		t.Run(fx.rel, func(t *testing.T) {
			data, err := os.ReadFile(fx.path)
			if err != nil {
				t.Fatal(err)
			}

			err = schema.Validate(tomlAsJSON(t, data))
			rejected := strings.HasPrefix(fx.rel, "invalid/schema/")

			switch {
			case rejected && err == nil:
				t.Error("the schema accepts a fixture it must reject")
			case !rejected && err != nil:
				t.Errorf("the schema rejects a fixture it must accept: %v", err)
			}
		})
	}
}

func TestFixturesLoad(t *testing.T) {
	seen := map[string]bool{}
	cwd := t.TempDir()

	for _, fx := range fixtures(t) {
		t.Run(fx.rel, func(t *testing.T) {
			vars := map[string]string{"PROJECT_ROOT": cwd, "HOME": cwd}
			maps.Copy(vars, fixtureVars)

			cfg, err := Load(LoadOptions{Path: fx.path, Cwd: cwd, Lookup: lookupFrom(vars)})

			name, invalid := strings.CutPrefix(fx.rel, "invalid/")
			if !invalid {
				if err != nil {
					t.Fatalf("valid fixture rejected: %v", err)
				}

				if err := cfg.CheckRunnerEnv(lookupFrom(vars)); err != nil {
					t.Errorf("runner environment: %v", err)
				}

				return
			}

			seen[name] = true

			fragments, ok := invalidFixtures[name]
			if !ok {
				t.Fatalf("no expected error recorded for %s", name)
			}

			wantUsage(t, err, fragments...)
		})
	}

	for name := range invalidFixtures {
		if !seen[name] {
			t.Errorf("expected error recorded for missing fixture %s", name)
		}
	}
}

// TestSchemaAgreesWithDecoder holds the schema and the per-file decoder to the
// same verdict. Rules the schema leaves to the loader (placeholders per field,
// template syntax, runner mode requirements, globs, zero durations, NUL
// characters, extends graphs, port ranges and IPv6 literals of registry
// hosts) are tested against the loader alone.
func TestSchemaAgreesWithDecoder(t *testing.T) {
	schema := compileConfigSchema(t)

	const v1 = "config_version = 1\n"

	upper := strings.ToUpper(testDigest[len("sha256:"):])

	cases := []struct {
		doc   string
		valid bool
	}{
		{"", false},
		{"config_version = 2", false},
		{"config_version = \"1\"", false},
		{v1 + "Offline = true", false},
		{v1 + "[runner]\ncmd = \"x\"", false},
		{v1 + "[catalog]\ndigest = \"${D}\"\nrepository = \"${R}\"", true},
		{v1 + "[catalog]\ndigest = \"prefix-${D}\"", true},
		{v1 + "[catalog]\ndigest = \"sha256:${HEX}\"", true},
		{v1 + "[catalog]\ndigest = \"$${A}${D}\"", true},
		{v1 + "[catalog]\ndigest = \"$${D}\"", false},
		{v1 + "[catalog]\ndigest = \"$$${D}\"", false},
		{v1 + "[catalog]\ndigest = \"${1D}\"", false},
		{v1 + "[catalog]\ndigest = \"${D\"", false},
		{v1 + "[catalog]\ndigest = \"" + testDigest + "\"", true},
		{v1 + "[catalog]\ndigest = \"sha256:" + upper + "\"", false},
		{v1 + "[catalog]\ndigest = \"catalog-latest\"", false},
		{v1 + "[catalog]\ndigest = \"r.example/org@" + testDigest + "\"", false},
		{v1 + "[catalog]\nrepository = \"localhost:5000/a/b\"", true},
		{v1 + "[catalog]\nrepository = \"[::1]:5000/a__b/c-d/e.f\"", true},
		{v1 + "[catalog]\nrepository = \"r.example/${NAME}\"", true},
		{v1 + "[catalog]\nrepository = \"$${R}\"", false},
		{v1 + "[catalog]\nrepository = \"r.example\"", false},
		{v1 + "[catalog]\nrepository = \"r.example/Org\"", false},
		{v1 + "[catalog]\nrepository = \"Registry.example/org\"", false},
		{v1 + "[catalog]\nrepository = \"[FE80::1]:5000/org\"", false},
		{v1 + "[catalog]\nrepository = \"[fe80::1]:5000/org\"", true},
		{v1 + "[catalog]\nrepository = \"r.example/org/schemas:latest\"", false},
		{v1 + "[catalog]\nrepository = \"https://r.example/org\"", false},
		{v1 + "[catalog]\nrepository = \"\"", false},
		{v1 + "[registries.\"[::1]:5000\"]\nplain_http = true", true},
		{v1 + "[registries.\"https://r.example\"]\nplain_http = true", false},
		{v1 + "[registries.\"r.example/org\"]\nplain_http = true", true},
		{v1 + "[registries.\"localhost:5000/a__b/c-d/e.f\"]\nplain_http = true", true},
		{v1 + "[registries.\"r.example/Org\"]\nplain_http = true", false},
		{v1 + "[registries.\"LOCALHOST:1\"]\nplain_http = true", false},
		{v1 + "[registries.\"GHCR.io/org\"]\nplain_http = true", false},
		{v1 + "[registries.\"[FE80::1]:5000\"]\nplain_http = true", false},
		{v1 + "[registries.\"localhost:1\"]\nplain_http = true", true},
		{v1 + "[registries.\"[fe80::1]:5000/org\"]\nplain_http = true", true},
		{v1 + "[registries.\"r.example/org/\"]\nplain_http = true", false},
		{v1 + "[registries.\"r.example//org\"]\nplain_http = true", false},
		{v1 + "[registries.\"r.example/org:v1\"]\nplain_http = true", false},
		{v1 + "[registries.\"r.example/org@" + testDigest + "\"]\nplain_http = true", false},
		{v1 + "[registries.\"r.example\"]\ninsecure = true", false},
		{v1 + "workspace = \"\"", false},
		{v1 + "[runner]\nmode = \"per-file\"", true},
		{v1 + "[runner]\nmode = \"serial\"", false},
		{v1 + "[runner]\ncommand = \"\"", false},
		{v1 + "[runner]\ntimeout = \"1h30m\"", true},
		{v1 + "[runner]\ntimeout = \"90\"", false},
		{v1 + "[runner]\ntimeout = \"-1s\"", false},
		{v1 + "[runner]\njobs = 64", true},
		{v1 + "[runner]\njobs = 0", false},
		{v1 + "[runner]\njobs = 65", false},
		{v1 + "[runner]\nmax_args_bytes = 0", false},
		{v1 + "[runner.env]\nNO_COLOR = \"1\"", true},
		{v1 + "[runner.env]\n\"\" = \"x\"", false},
		{v1 + "[runner.env]\n\"A=B\" = \"x\"", false},
		{v1 + "[runner.env]\nDEBUG = 1", false},
		{v1 + "extends = [\"./a:b.toml\", \"../base.toml\", \"C:/base.toml\"]", true},
		{v1 + "extends = [\"file:///etc/base.toml\"]", false},
		{v1 + "extends = [\"\"]", false},
		{v1 + "[[mappings]]\nfile_match = [\"a\"]\nschema = \"a\"", true},
		{v1 + "[[mappings]]\nfile_match = [\"a\"]\nschema = \"a..b\"", false},
		{v1 + "[[mappings]]\nfile_match = [\"a\"]\nschema = \"A\"", false},
		{v1 + "[[mappings]]\nfile_match = []\nschema = \"a\"", false},
		{v1 + "[[mappings]]\nschema = \"a\"", false},
		{v1 + "[schemas]", true},
		{v1 + "[schemas.a]\npath = \"a.json\"", true},
		{v1 + "[schemas.\"company-config\"]\npath = \"../schemas/c.json\"\nfile_match = [\"config/company.json\", \"!x/**\"]", true},
		{v1 + "[schemas.a]\npath = \"${DIR}/a.json\"", true},
		{v1 + "[schemas.a]\npath = \"./a:b.json\"", true},
		{v1 + "[schemas.a]\nfile_match = [\"a\"]", true},
		{v1 + "[schemas.a]", true},
		{v1 + "[schemas.a]\npath = \"\"", false},
		{v1 + "[schemas.a]\npath = 1", false},
		{v1 + "[schemas.a]\npath = \"https://schemas.example/a.json\"", false},
		{v1 + "[schemas.a]\npath = \"a.json\"\nfile_match = []", false},
		{v1 + "[schemas.a]\npath = \"a.json\"\nfile_match = [\"\"]", false},
		{v1 + "[schemas.a]\npath = \"a.json\"\nurl = \"x\"", false},
		{v1 + "[schemas.A]\npath = \"a.json\"", false},
		{v1 + "[schemas.\"a..b\"]\npath = \"a.json\"", false},
		{v1 + "[schemas.\"-a\"]\npath = \"a.json\"", false},
		{v1 + "schemas = [\"a\"]", false},
		{v1 + "[limits]\nmax_schema_bytes = 1073741824", true},
		{v1 + "[limits]\nmax_schema_bytes = 1073741825", false},
		{v1 + "[limits]\nmax_catalog_entries = 0", false},
	}

	for _, tc := range cases {
		data := []byte(tc.doc + "\n")
		schemaErr := schema.Validate(tomlAsJSON(t, data))
		_, decodeErr := parseLayer("case.toml", data)

		if (schemaErr == nil) != tc.valid || (decodeErr == nil) != tc.valid {
			t.Errorf("%q: want valid = %v; schema: %v; decoder: %v", tc.doc, tc.valid, schemaErr, decodeErr)
		}
	}
}
