//go:build !windows && realvalidators

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/publisher/prepare"
)

func TestPrepareV3RetainsSourcemetaCompileExclusion(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	jsonschema := filepath.Join(root, ".tools", "bin", "jsonschema")
	python := filepath.Join(root, "tools", "check-jsonschema", ".venv", "bin", "schepherd-check-jsonschema")
	ajv := filepath.Join(root, "tools", "ajv", "index.mjs")
	for _, path := range []string{jsonschema, python, ajv} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("real validator fixture requires %s: %v", path, err)
		}
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("real AJV fixture requires node: %v", err)
	}
	var firstReason string
	for run := range 2 {

		dir := t.TempDir()
		schema := `{"$schema":"http://json-schema.org/draft-07/schema#","$defs":{"flag":{"type":"boolean"}},"type":"object","properties":{"enabled":{"$ref":"#/$defs/flag"}}}`
		if err := os.WriteFile(filepath.Join(dir, "schema.json"), []byte(schema), 0o600); err != nil {
			t.Fatal(err)
		}
		source := `kind = "local"
name = "v3-profile-fixture"

[[entries]]
id = "v3-profile-fixture"
name = "Compatibility fixture"
url = "https://schemas.example/compatibility.json"
file = "schema.json"
file_match = ["compatibility.json"]
license = "MIT"
`
		sourcePath := filepath.Join(dir, "source.toml")
		if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		processors := fmt.Sprintf(`config_version = 1
workspace = %q

[processors.sourcemeta]
type = "sourcemeta"
command = %q

[processors.python]
type = "check-jsonschema"
command = %q

[processors.ajv]
type = "ajv"
command = %q
`, dir, jsonschema, python, ajv)
		processorsPath := filepath.Join(dir, "processors.toml")
		if err := os.WriteFile(processorsPath, []byte(processors), 0o600); err != nil {
			t.Fatal(err)
		}

		out := filepath.Join(dir, "prepared")
		runCLI(t, "prepare", "--quiet", "--source", sourcePath, "--out", out, "--jsonschema", bundler(t), "--catalog-format", "3", "--processors-config", processorsPath, "--json").want(t, 0)
		set, err := prepare.Load(out)
		if err != nil {
			t.Fatal(err)
		}
		if set.Document.CatalogFormat != 3 || len(set.Document.Entries) != 1 {
			t.Fatalf("prepared catalog format/entries = %d/%d", set.Document.CatalogFormat, len(set.Document.Entries))
		}
		metadata := set.Document.Entries[0].Metadata
		if metadata == nil {
			t.Fatal("prepared entry has no compatibility metadata")
		}
		profiles := metadata.Profiles
		if len(profiles) != 3 {
			t.Fatalf("compatibility profiles = %+v", profiles)
		}
		for _, want := range []struct{ processor, result string }{{"sourcemeta", "unsupported"}, {"check-jsonschema", "supported"}, {"ajv", "supported"}} {
			index := slices.IndexFunc(profiles, func(profile catalog.Profile) bool { return profile.Processor == want.processor })
			if index < 0 || profiles[index].Result != want.result {
				t.Errorf("profile %s = %+v, want result %s", want.processor, profiles, want.result)
			}
		}
		index := slices.IndexFunc(profiles, func(profile catalog.Profile) bool { return profile.Processor == "sourcemeta" })
		if index < 0 || profiles[index].Reason == "" {
			t.Fatalf("Sourcemeta exclusion has no reason: %+v", profiles)
		}
		reason := profiles[index].Reason
		if run == 0 {
			firstReason = reason
		} else if reason != firstReason {
			t.Fatalf("Sourcemeta reason changed across temporary roots: %q != %q", reason, firstReason)
		}
		for _, local := range []string{dir, jsonschema, python, ajv} {
			if strings.Contains(reason, local) {
				t.Errorf("Sourcemeta reason leaks local path %q: %s", local, reason)
			}
		}
	}
}
