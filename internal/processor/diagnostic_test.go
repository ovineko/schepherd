package processor

import (
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeDiagnosticPathsReplacesOnlyDeclaredLocalPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "work root")
	schema := filepath.Join(root, "schema.json")
	executable := filepath.Join(root, "bin", "validator")
	message := "failed reading " + schema + " with " + executable + " in " + root + "; see https://schemas.example" + schema
	got := NormalizeDiagnosticPaths(message,
		PathSubstitution{Path: root, Token: "{workspace}"},
		PathSubstitution{Path: schema, Token: "{schema}"},
		PathSubstitution{Path: executable, Token: "{processor}"},
	)
	want := "failed reading {schema} with {processor} in {workspace}; see https://schemas.example" + schema
	if got != want {
		t.Fatalf("normalized diagnostic = %q, want %q", got, want)
	}

	fileURIPath := filepath.ToSlash(schema)
	if filepath.VolumeName(schema) != "" {
		fileURIPath = "/" + fileURIPath
	}
	fileURI := (&url.URL{Scheme: "file", Path: fileURIPath}).String()
	got = NormalizeDiagnosticPaths("cannot read "+fileURI, PathSubstitution{Path: schema, Token: "{schema}"})
	if !strings.Contains(got, "file://{schema}") {
		t.Fatalf("escaped local file URI was not normalized: %q", got)
	}
}

func TestNormalizeDiagnosticPathsPreservesMeaningfulDifferences(t *testing.T) {
	root := filepath.Join(t.TempDir(), "work")
	path := filepath.Join(root, "schema.json")
	replacement := PathSubstitution{Path: path, Token: "{schema}"}
	first := NormalizeDiagnosticPaths("compile "+path+": draft is unsupported", replacement)
	second := NormalizeDiagnosticPaths("compile "+path+": invalid regular expression", replacement)
	if first == second || first != "compile {schema}: draft is unsupported" || second != "compile {schema}: invalid regular expression" {
		t.Fatalf("normalization erased diagnostic differences: %q / %q", first, second)
	}
}
