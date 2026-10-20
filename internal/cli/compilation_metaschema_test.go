package cli

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/processor"
	"github.com/ovineko/schepherd/internal/runner"
)

func TestCompilationObservesChangedCustomMetaschema(t *testing.T) {
	validator := filepath.Join("..", "..", ".tools", "bin", "jsonschema")
	if runtime.GOOS == "windows" {
		validator += ".exe"
	}
	validator, err := filepath.Abs(validator)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(validator); err != nil {
		t.Fatalf("install the pinned validator with task tools: %v", err)
	}
	for _, nested := range []bool{false, true} {
		name := "root"
		if nested {
			name = "nested"
		}
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			meta := filepath.Join(workspace, "meta.json")
			path := filepath.ToSlash(meta)
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
			metaURI := (&url.URL{Scheme: "file", Path: path}).String()
			root := map[string]any{"$schema": metaURI, "type": "integer"}
			input := `"wrong"`
			if nested {
				root["$id"] = "https://example.test/nested-schema"
				root = map[string]any{
					"$schema": "https://json-schema.org/draft/2020-12/schema",
					"type":    "object", "properties": map[string]any{"value": root},
				}
				input = `{"value":"wrong"}`
			}
			schema, err := json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(workspace, "schema.json"), string(schema))
			writeTestFile(t, filepath.Join(workspace, "input.json"), input)
			config := filepath.Join(workspace, "schepherd.toml")
			writeTestFile(t, config, "config_version = 1\ncache_dir = "+strconv.Quote(filepath.Join(workspace, "cache"))+"\n"+
				"[processors.native]\ntype = \"sourcemeta\"\ninherit_env = false\ncommand = "+strconv.Quote(validator)+"\n"+
				"[schemas.example]\npath = \"schema.json\"\nfile_match = [\"input.json\"]\n")
			for _, validation := range []bool{false, true, false} {
				vocabulary := map[string]bool{"https://json-schema.org/draft/2020-12/vocab/core": true}
				if validation {
					vocabulary["https://json-schema.org/draft/2020-12/vocab/validation"] = true
				}
				metaBytes, err := json.Marshal(map[string]any{
					"$schema": "https://json-schema.org/draft/2020-12/schema", "$id": metaURI,
					"$vocabulary": vocabulary, "type": []string{"object", "boolean"},
				})
				if err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, meta, string(metaBytes))
				stdout, stderr, code := run(t, "--config", config, "--workspace", workspace, "--quiet", "run", "--json", "--", filepath.Join(workspace, "input.json"))
				report := decodeRunJSON(t, stdout, stderr, code)
				want := 0
				if validation {
					want = 2
				}
				if code != want || len(report.Tasks) != 1 || report.Tasks[0].ExitCode != want {
					t.Fatalf("metaschema validation=%v: exit=%d want=%d report=%s", validation, code, want, stdout)
				}
				if validation && (report.Tasks[0].Stdout.Text == nil || !strings.Contains(*report.Tasks[0].Stdout.Text, "integer")) {
					t.Fatalf("metaschema validation vocabulary was ignored: %s", stdout)
				}
			}
		})
	}
}

func TestCompilationCachesSchemaPropertyWithoutMistakingItForDialect(t *testing.T) {
	validator, err := filepath.Abs(filepath.Join("..", "..", ".tools", "bin", "jsonschema"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		validator += ".exe"
	}
	workspace := t.TempDir()
	schema := filepath.Join(workspace, "schema.json")
	data := []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"$schema":{"type":"string"}},"required":["$schema"]}`)
	writeTestFile(t, schema, string(data))
	c, err := cache.Open(filepath.Join(workspace, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	definition := processor.Definition{Type: "sourcemeta", Spec: runner.Spec{Name: "native"}}
	task := runner.Task{Path: validator, Dir: workspace, Processor: runner.Processor{Type: "sourcemeta"}}
	compiler := &processor.Compiler{Cache: c, RequirePersistent: true}
	prepared, err := compiler.Prepare(t.Context(), definition, task, schema, data, true)
	compiler.Close()
	if err != nil {
		t.Fatal(err)
	}
	compiler = &processor.Compiler{Cache: c, VerifyOnly: true}
	defer compiler.Close()
	verified, err := compiler.Prepare(t.Context(), definition, task, schema, data, true)
	if err != nil || verified.Path != prepared.Path {
		t.Fatalf("schema with literal $schema property was not persistently prepared: %+v %v", verified, err)
	}
	input := filepath.Join(workspace, "input.json")
	config := filepath.Join(workspace, "schepherd.toml")
	writeTestFile(t, config, "config_version = 1\ncache_dir = "+strconv.Quote(c.Dir())+"\n[processors.native]\ntype = \"sourcemeta\"\ninherit_env = false\ncommand = "+strconv.Quote(validator)+"\n[schemas.example]\npath = \"schema.json\"\nfile_match = [\"input.json\"]\n")
	for _, document := range []struct {
		text string
		exit int
	}{
		{`{"$schema":"application dialect"}`, 0},
		{`{"$schema":7}`, 2},
	} {
		writeTestFile(t, input, document.text)
		stdout, stderr, code := run(t, "--config", config, "--workspace", workspace, "--quiet", "run", "--json", "--", input)
		report := decodeRunJSON(t, stdout, stderr, code)
		if code != document.exit || len(report.Tasks) != 1 {
			t.Fatalf("$schema property validation: exit=%d want=%d report=%s", code, document.exit, stdout)
		}
	}
}
