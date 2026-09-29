package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/env"
)

// TestConfigCheckRefusesUpperCaseHosts: the registry client refuses
// upper-case repositories and matches [registries] keys verbatim, so
// "config check" must fail for them instead of reporting ok for a
// configuration that every network command rejects or silently ignores.
func TestConfigCheckRefusesUpperCaseHosts(t *testing.T) {
	const rule = "registry hosts and repository paths must be lower case"

	digest := "sha256:" + strings.Repeat("a", 64)

	cases := []struct {
		vars      map[string]string
		name      string
		config    string
		fragments []string
	}{
		{
			name:      "catalog.repository",
			config:    "config_version = 1\n[catalog]\nrepository = \"Registry.example/org/schemas\"\ndigest = \"" + digest + "\"\n",
			fragments: []string{"catalog.repository", rule, `write host "Registry.example" as "registry.example"`},
		},
		{
			name:      "registries key",
			config:    "config_version = 1\n[registries.\"LOCALHOST:1\"]\nplain_http = true\n",
			fragments: []string{`registries."LOCALHOST:1"`, rule, `write host "LOCALHOST:1" as "localhost:1"`},
		},
		{
			name:      env.KeyRepository,
			config:    "config_version = 1\n",
			vars:      map[string]string{env.KeyRepository: "Registry.example/org/schemas"},
			fragments: []string{env.KeyRepository, rule},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "schepherd.toml")
			if err := os.WriteFile(path, []byte(tc.config), 0o600); err != nil {
				t.Fatal(err)
			}

			for _, args := range [][]string{{"config", "check"}, {"config", "check", "--json"}} {
				stdout, stderr, code := runWith(t, tc.vars, append([]string{"--config", path}, args...)...)
				if code != 2 || stdout != "" || !strings.HasPrefix(stderr, "schepherd: usage error: ") {
					t.Fatalf("%s: exit %d, stdout %q, stderr %q", strings.Join(args, " "), code, stdout, stderr)
				}

				for _, fragment := range tc.fragments {
					if !strings.Contains(stderr, fragment) {
						t.Errorf("%s: stderr %q does not contain %q", strings.Join(args, " "), stderr, fragment)
					}
				}
			}

			lower := strings.NewReplacer("Registry.example", "registry.example", "LOCALHOST", "localhost").Replace(tc.config)
			if err := os.WriteFile(path, []byte(lower), 0o600); err != nil {
				t.Fatal(err)
			}

			vars := map[string]string{}
			for key, value := range tc.vars {
				vars[key] = strings.ToLower(value)
			}

			if stdout, stderr, code := runWith(t, vars, "--config", path, "config", "check"); code != 0 {
				t.Errorf("the lower-case configuration fails: exit %d, stdout %q, stderr %q", code, stdout, stderr)
			}
		})
	}
}
