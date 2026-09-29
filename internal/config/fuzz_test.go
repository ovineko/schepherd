package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/match"
	"github.com/ovineko/schepherd/internal/registry"
	"github.com/ovineko/schepherd/internal/runner"
)

const maxFuzzBytes = 64 << 10

func FuzzLoad(f *testing.F) {
	for _, fx := range fixtures(f) {
		data, err := os.ReadFile(fx.path)
		if err != nil {
			f.Fatal(err)
		}

		f.Add(data)
	}

	for _, seed := range []string{
		"",
		"config_version = 1\nextends = [\"schepherd.toml\"]\n",
		"config_version = 1\nmappings = [{schema = \"a\", file_match = [\"a\"]}, {schema = \"b\"}]\n",
		"config_version = 1\n[[mappings]]\n[mappings.x]\n[[mappings]]\n",
		"config_version = 1\n[registries.\"a.b\".x]\ny = 1\n",
		"config_version = 1\n[registries.\"a.b:5000/org/team\"]\nplain_http = true\n[registries.\"a.b/Org\"]\n",
		"config_version = 1\n[registries.\"A.b:0\"]\n[registries.\"[FE80::1]:65536\"]\n[registries.\"[1.2.3.4]\"]\n",
		"config_version = 1\n[catalog]\nrepository = \"Registry.example:5000/org\"\n",
		"config_version = 1\nworkspace = \"${SET}/${UNSET}\"\n",
		"config_version = 1\n[catalog]\ndigest = \"${DIGEST}\"\n",
		"config_version = 1\n[runner]\ncommand = \"{workspace}/v\"\nargs = [\"{files...}\"]\nenv = { \"é\" = \"{file}\" }\n",
		"config_version = 1\n[schemas.a]\npath = \"${SET}/a.json\"\nfile_match = [\"a.json\", \"!b/**\"]\n[schemas.\"b.c\"]\npath = \"./b:c.json\"\n",
		"config_version = 1\nschemas = { a = { path = \"a\" }, B = {}, c = { path = \"\" } }\n",
	} {
		f.Add([]byte(seed))
	}

	lookup := lookupFrom(map[string]string{"SET": "value", "DIGEST": testDigest})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxFuzzBytes {
			return
		}

		dir := t.TempDir()
		path := filepath.Join(dir, "schepherd.toml")

		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}

		opts := LoadOptions{Path: path, Cwd: dir, Lookup: lookup}

		cfg, err := Load(opts)

		// Bases outside the temporary directory may change between reads,
		// so determinism is only asserted for self-contained documents.
		if !bytes.Contains(data, []byte("extends")) {
			again, errAgain := Load(opts)
			if (err == nil) != (errAgain == nil) || (err != nil && err.Error() != errAgain.Error()) || !reflect.DeepEqual(cfg, again) {
				t.Fatalf("Load is not deterministic: %v / %v", err, errAgain)
			}
		}

		if err != nil {
			if kind := fault.KindOf(err); kind != fault.Usage {
				t.Fatalf("error kind %v, want usage: %v", kind, err)
			}

			return
		}

		checkInvariants(t, cfg, path)
	})
}

// matchesClientRepositories reports whether the registry client, which
// compares keys verbatim, can apply key: it is the host of or a path prefix
// of repositories the client accepts.
func matchesClientRepositories(key string) bool {
	if repo, err := registry.ParseRepository(key + "/x"); err == nil && repo.Host == key {
		return true
	}

	repo, err := registry.ParseRepository(key)

	return err == nil && repo.String() == key
}

func checkInvariants(t *testing.T, cfg *Config, path string) {
	t.Helper()

	if len(cfg.Files) == 0 || cfg.Files[len(cfg.Files)-1] != path {
		t.Fatalf("Files = %q, want the top file last", cfg.Files)
	}

	if !filepath.IsAbs(cfg.Workspace) || (cfg.CacheDir != "" && !filepath.IsAbs(cfg.CacheDir)) {
		t.Fatalf("paths not absolute: %q %q", cfg.Workspace, cfg.CacheDir)
	}

	if cfg.Catalog != "" {
		if err := checkDigest(cfg.Catalog); err != nil {
			t.Fatalf("accepted catalog %q: %v", cfg.Catalog, err)
		}
	}

	if cfg.Repository != "" {
		if err := checkRepository(cfg.Repository); err != nil {
			t.Fatalf("accepted repository %q: %v", cfg.Repository, err)
		}

		if _, err := registry.ParseRepository(cfg.Repository); err != nil {
			t.Fatalf("accepted repository %q that the registry client refuses: %v", cfg.Repository, err)
		}
	}

	l := cfg.Limits
	if l.MaxManifestBytes < 1 || l.MaxCatalogBytes < 1 || l.MaxPayloadBytes < 1 || l.MaxSchemaBytes < 1 || l.MaxCatalogEntries < 1 {
		t.Fatalf("limits not positive: %+v", l)
	}

	for key, reg := range cfg.Registries {
		if checkRegistryKey(key) != nil || (reg.CAFile != "" && !filepath.IsAbs(reg.CAFile)) || (reg.CredentialsFile != "" && !filepath.IsAbs(reg.CredentialsFile)) {
			t.Fatalf("registry %q: %+v", key, reg)
		}

		if !matchesClientRepositories(key) {
			t.Fatalf("accepted registry key %q that never matches a repository the registry client accepts", key)
		}
	}

	if cfg.RunnerConfigured {
		if err := runner.ValidateSpec(cfg.Runner); err != nil {
			t.Fatalf("configured runner fails validation: %v", err)
		}
	}

	for _, m := range cfg.Mappings {
		if checkSchemaID(m.Schema) != nil || len(m.FileMatch) == 0 {
			t.Fatalf("accepted mapping %+v", m)
		}

		for _, p := range m.FileMatch {
			if match.ValidatePattern(p) != nil {
				t.Fatalf("accepted pattern %q", p)
			}
		}
	}

	for i, ls := range cfg.LocalSchemas {
		if checkSchemaID(ls.ID) != nil || !filepath.IsAbs(ls.Path) || filepath.Clean(ls.Path) != ls.Path || !slices.Contains(cfg.Files, ls.DeclaredIn) {
			t.Fatalf("accepted local schema %+v", ls)
		}

		if i > 0 && cfg.LocalSchemas[i-1].ID >= ls.ID {
			t.Fatalf("local schemas are not sorted by unique id: %q before %q", cfg.LocalSchemas[i-1].ID, ls.ID)
		}

		for _, p := range ls.FileMatch {
			if match.ValidatePattern(p) != nil {
				t.Fatalf("accepted pattern %q", p)
			}
		}
	}

	if err := cfg.CheckLocalSchemas(); err != nil && fault.KindOf(err) != fault.Usage {
		t.Fatalf("CheckLocalSchemas: %v", err)
	}

	if err := cfg.CheckRunnerEnv(lookupFrom(nil)); err != nil && fault.KindOf(err) != fault.Usage {
		t.Fatalf("CheckRunnerEnv: %v", err)
	}

	if _, err := json.Marshal(cfg.Summary()); err != nil {
		t.Fatalf("Summary: %v", err)
	}
}
