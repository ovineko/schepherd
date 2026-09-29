package config

import (
	"path/filepath"
	"testing"

	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/internal/registry"
)

const lowerCaseRule = "registry hosts and repository paths must be lower case"

// TestUpperCaseRepositoryHostFromEverySource refuses an upper-case registry
// host in the repository no matter which source sets it, so "config check"
// fails exactly where every network command would.
func TestUpperCaseRepositoryHostFromEverySource(t *testing.T) {
	const upper, lower = "Registry.example/org/schemas", "registry.example/org/schemas"

	dir := writeTree(t, map[string]string{"s.toml": "config_version = 1\n[catalog]\nrepository = \"${REPO}\"\n"})
	path := filepath.Join(dir, "s.toml")

	cases := []struct {
		opts   func(repo string) LoadOptions
		name   string
		source string
	}{
		{name: "flag", source: "--repository", opts: func(repo string) LoadOptions {
			return LoadOptions{Cwd: dir, Flags: Overrides{Repository: new(repo)}}
		}},
		{name: "environment", source: env.KeyRepository, opts: func(repo string) LoadOptions {
			return LoadOptions{Cwd: dir, Env: Overrides{Repository: new(repo)}}
		}},
		{name: "file template", source: "catalog.repository", opts: func(repo string) LoadOptions {
			return LoadOptions{Path: path, Cwd: dir, Lookup: lookupFrom(map[string]string{"REPO": repo})}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(tc.opts(upper))
			wantUsage(t, err, tc.source, lowerCaseRule, `write host "Registry.example" as "registry.example"`)

			cfg, err := Load(tc.opts(lower))
			if err != nil {
				t.Fatalf("lower-case repository: %v", err)
			}

			if cfg.Repository != lower {
				t.Errorf("Repository = %q, want %q", cfg.Repository, lower)
			}
		})
	}
}

// TestHostsAgreeWithTheRegistryClient holds the loader to the registry
// client's verdict on repositories and [registries] keys: every value the
// client refuses must already fail "config check", so a value the loader
// accepts works with the client. The loader may be stricter about host
// syntax, as for a host that starts with '-'.
func TestHostsAgreeWithTheRegistryClient(t *testing.T) {
	valid := []string{
		"r.example", "registry-1.example.com", "localhost", "localhost:1", "localhost:5000", "localhost:65535",
		"127.0.0.1:5000", "[::1]", "[::1]:5000", "[fe80::1]:5000", "[::ffff:1.2.3.4]",
	}
	invalid := []string{
		"R.example", "LOCALHOST:5000", "Registry-1.Example.com", "[FE80::1]:5000", "[::FFFF:1.2.3.4]",
		"localhost:0", "localhost:65536", "localhost:99999", "localhost:", "[1.2.3.4]", "[:]", "[.]", "[]",
		"-r.example", "r.example-", "r_example", "r.example:port",
	}

	for _, host := range valid {
		if err := checkRegistryKey(host); err != nil {
			t.Errorf("registry key %q: %v", host, err)
		}

		if err := checkRepository(host + "/org"); err != nil {
			t.Errorf("repository %q: %v", host+"/org", err)
		}

		if _, err := registry.ParseRepository(host + "/org"); err != nil {
			t.Errorf("registry client: %v", err)
		}
	}

	for _, host := range append(valid, invalid...) {
		for _, path := range []string{"org", "org/schemas", "Org"} {
			repo := host + "/" + path
			_, clientErr := registry.ParseRepository(repo)

			if clientErr == nil {
				continue
			}

			if checkRepository(repo) == nil {
				t.Errorf("the loader accepts repository %q, which the registry client refuses: %v", repo, clientErr)
			}

			if checkRegistryKey(repo) == nil {
				t.Errorf("the loader accepts registry key %q, which never matches a repository the client accepts: %v", repo, clientErr)
			}
		}

		parsed, clientErr := registry.ParseRepository(host + "/x")
		if (clientErr != nil || parsed.Host != host) && checkRegistryKey(host) == nil {
			t.Errorf("the loader accepts registry key %q, which never matches a repository host the client accepts: %v", host, clientErr)
		}
	}
}
