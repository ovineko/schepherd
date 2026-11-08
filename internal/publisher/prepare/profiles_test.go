package prepare

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

func TestCatalogFormatThreeRequiresProcessorConfiguration(t *testing.T) {
	_, err := Run(context.Background(), Options{CatalogFormat: catalog.MetadataFormatVersion})
	if err == nil || !strings.Contains(err.Error(), "requires --processors-config") {
		t.Fatalf("Run() error = %v, want processor configuration requirement", err)
	}
}

func TestProfileConfigurationRejectsDuplicateCanonicalProcessors(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "processors.toml")
	doc := `config_version = 1
[processors.native]
type = "sourcemeta"
command = "unused"
[processors.native_alias]
type = "sourcemeta"
command = "different"
[processors.python]
type = "check-jsonschema"
command = "unused"
[processors.ajv]
type = "ajv"
command = "unused"
`
	if err := os.WriteFile(configPath, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	err := profileSet(context.Background(), Options{ProcessorConfig: configPath}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "ambiguous sourcemeta processors") {
		t.Fatalf("profileSet() error = %v, want duplicate canonical processor error", err)
	}
}

func TestProfileSetRejectsInputDependentProcessorBeforeProfiling(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "processors.toml")
	doc := `config_version = 1
[processors.native]
type = "sourcemeta"
command = "unused"
[processors.python]
type = "check-jsonschema"
command = "unused"
[processors.python.env]
INPUT = "{file}"
[processors.ajv]
type = "ajv"
command = "unused"
`
	if err := os.WriteFile(configPath, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	err := profileSet(context.Background(), Options{ProcessorConfig: configPath}, nil, nil, nil)
	if fault.KindOf(err) != fault.Usage {
		t.Fatalf("input-dependent profile must be rejected as a usage error: %v", err)
	}
}

func TestCachedProfileRequiresExactSchemaToolAndSettings(t *testing.T) {
	want := catalog.Profile{Processor: "ajv", Tool: "tool-digest", SettingsDigest: "settings-digest", Result: "unsupported", Reason: "dialect unsupported"}
	previous := &state.State{Schemas: []state.Schema{{ID: "example", Entry: catalog.Entry{Metadata: &catalog.Metadata{SchemaDigest: "schema-digest", Profiles: []catalog.Profile{want}}}}}}

	got, ok := cachedProfile(previous, "example", "schema-digest", "ajv", "tool-digest", "settings-digest")
	if !ok || got != want {
		t.Fatalf("cachedProfile() = (%+v, %v), want (%+v, true)", got, ok, want)
	}

	for _, key := range []struct {
		name, schema, tool, settings string
	}{
		{"schema", "other-schema", "tool-digest", "settings-digest"},
		{"tool", "schema-digest", "other-tool", "settings-digest"},
		{"settings", "schema-digest", "tool-digest", "other-settings"},
	} {
		t.Run(key.name, func(t *testing.T) {
			if got, ok := cachedProfile(previous, "example", key.schema, "ajv", key.tool, key.settings); ok {
				t.Fatalf("cachedProfile() = (%+v, true), want cache miss", got)
			}
		})
	}
}
