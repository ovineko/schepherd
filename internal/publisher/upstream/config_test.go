package upstream

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()

	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadSchemaStoreConfig(t *testing.T) {
	valid := `kind = "upstream"
commit = "` + testCommit + `"
tarball_base_url = "https://codeload.github.com/SchemaStore/schemastore/tar.gz"
max_tarball_bytes = 67108864

[dependencies]
max_depth = 8
max_per_schema = 64
max_document_bytes = 16777216
max_total_bytes = 268435456
`

	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"schemastore.toml": valid})

	cfg, err := LoadSchemaStoreConfig(filepath.Join(dir, "schemastore.toml"))
	if err != nil {
		t.Fatal(err)
	}

	want := &SchemaStoreConfig{
		Commit:          testCommit,
		TarballBaseURL:  "https://codeload.github.com/SchemaStore/schemastore/tar.gz",
		MaxTarballBytes: 64 << 20,
		Dependencies:    DependencyLimits{MaxDepth: 8, MaxPerSchema: 64, MaxDocumentBytes: 16 << 20, MaxTotalBytes: 256 << 20},
	}

	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("config = %#v", cfg)
	}

	tests := []struct {
		name string
		from string
		to   string
		want string
	}{
		{name: "kind", from: `kind = "upstream"`, to: `kind = "local"`, want: "kind must be"},
		{name: "short commit", from: testCommit, to: "0123456", want: "40-hex"},
		{name: "branch", from: `commit = "` + testCommit + `"`, to: `commit = "master"`, want: "40-hex"},
		{name: "no base", from: `tarball_base_url = "https://codeload.github.com/SchemaStore/schemastore/tar.gz"`, to: "", want: "tarball_base_url is required"},
		{name: "bad base", from: "https://codeload", to: "ftp://codeload", want: "tarball_base_url"},
		{name: "zero depth", from: "max_depth = 8", to: "max_depth = 0", want: "max_depth must be a positive"},
		{name: "negative total", from: "max_total_bytes = 268435456", to: "max_total_bytes = -1", want: "max_total_bytes must be a positive"},
		{name: "missing limit", from: "max_per_schema = 64\n", to: "", want: "max_per_schema must be a positive"},
		{name: "document above total", from: "max_document_bytes = 16777216", to: "max_document_bytes = 300000000", want: "must not exceed"},
		{name: "unknown key", from: "max_depth = 8", to: "max_depth = 8\nmax_width = 1", want: "unknown key"},
		{name: "string limit", from: "max_depth = 8", to: `max_depth = "8"`, want: "line"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"schemastore.toml": strings.Replace(valid, tt.from, tt.to, 1)})

			_, err := LoadSchemaStoreConfig(filepath.Join(dir, "schemastore.toml"))
			if err == nil || fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}
