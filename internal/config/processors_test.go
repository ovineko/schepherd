package config

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/runner"
)

func TestProcessorDefaultsAndSelection(t *testing.T) {
	cfg, dir := mustLoadDoc(t, `config_version=1
[processors.z]
type="ajv"
command="/validators/ajv"
[processors.b]
type="sourcemeta"
command="/validators/jsonschema"
[processors.a]
type="sourcemeta"
command="/validators/jsonschema"
[schemas.sample]
path="schema.json"
processor="z"
[[mappings]]
schema="sample"
file_match=["special.json"]
processor="b"
`, nil)
	if !reflect.DeepEqual(cfg.ProcessorOrder, []string{"a", "b", "z"}) {
		t.Fatalf("order: %v", cfg.ProcessorOrder)
	}
	if !cfg.RunnerConfigured || cfg.Unsupported != "error" {
		t.Fatalf("runner not configured: %+v", cfg)
	}
	if cfg.Processors["a"].Spec.Mode != runner.ModePerFile {
		t.Fatal("typed processor is not per-file")
	}
	for path, want := range map[string]string{"special.json": "b", "other.json": "z"} {
		got, err := cfg.ProcessorFor(filepath.Join(dir, path), "sample")
		if err != nil || got != want {
			t.Fatalf("%s: %q %v", path, got, err)
		}
	}
}

func TestProcessorExtendsMergesFields(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"base.toml": `config_version=1
[processors.validator]
type="custom"
command="${VALIDATOR}"
args=["{file}"]
cwd="relative"
[processors.validator.env]
A="one"
`,
		"config.toml": `config_version=1
extends=["base.toml"]
[runner]
processors=["validator"]
unsupported="skip"
[processors.validator]
output="ajv-json"
[processors.validator.env]
B="two"
`,
	})
	cfg, err := Load(LoadOptions{Path: filepath.Join(dir, "config.toml"), Cwd: dir, Lookup: lookupFrom(nil)})
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Processors["validator"]
	if p.Spec.Command != "${VALIDATOR}" || p.Spec.CwdBase != dir || p.Spec.Env["A"] != "one" || p.Spec.Env["B"] != "two" || cfg.Unsupported != "skip" {
		t.Fatalf("merged processor: %+v", p)
	}
	wantUsage(t, cfg.CheckRunnerEnv(nil), "${VALIDATOR}", "processors.validator.command")
	data, err := json.Marshal(cfg.Summary())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "${VALIDATOR}") || !strings.Contains(string(data), `"output":"ajv-json"`) {
		t.Fatalf("summary: %s", data)
	}
}

func TestProcessorConfigurationRejectsUnusedOrAmbiguousSettings(t *testing.T) {
	cases := []string{
		`[processors.p]
type="sourcemeta"
command="v"
args=["{file}"]`,
		`[processors.p]
type="custom"
command="v"`,
		`[processors.p]
type="ajv"
command="v"
convert="yq"`,
		`[runner]
processors=["missing"]`,
		`[runner]
command="v"
args=["{files...}"]
[processors.p]
type="ajv"
command="v"`,
		`[processors.p]
Type="ajv"
command="v"`,
		`[runner]
processors=["p","p"]
[processors.p]
type="ajv"
command="v"`,
		`[runner]
args=["unused"]
[processors.p]
type="ajv"
command="v"`,
	}
	for _, doc := range cases {
		t.Run(doc, func(t *testing.T) {
			_, _, err := loadDoc(t, "config_version=1\n"+doc, nil)
			if err == nil || fault.KindOf(err) != fault.Usage {
				t.Fatalf("wanted usage error, got %v", err)
			}
		})
	}
	cfg, dir := mustLoadDoc(t, `config_version=1
[processors.a]
type="ajv"
command="v"
[processors.b]
type="ajv"
command="v"
[[mappings]]
schema="s"
file_match=["*.json"]
processor="a"
[[mappings]]
schema="s"
file_match=["*.json"]
processor="b"
[runner]
unsupported="skip"
`, nil)
	_, err := cfg.ProcessorFor(filepath.Join(dir, "input.json"), "s")
	if err == nil || fault.KindOf(err) != fault.NotFound {
		t.Fatalf("ambiguous mapping: %v", err)
	}
}

func TestProcessorExplicitOrderAndCachePlaceholder(t *testing.T) {
	cfg, _ := mustLoadDoc(t, `config_version=1
[runner]
processors=["javascript","native"]
[processors.native]
type="sourcemeta"
command="{cache}/jsonschema"
[processors.javascript]
type="ajv"
command="ajv"
`, nil)
	if !reflect.DeepEqual(cfg.ProcessorOrder, []string{"javascript", "native"}) {
		t.Fatalf("order: %v", cfg.ProcessorOrder)
	}
	if !cfg.RunnerUses("cache") {
		t.Fatal("processor cache dependency lost")
	}
}
