package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/env"
)

// effective is the part of "config check --json" the precedence rules decide.
type effective struct {
	Repository string   `json:"repository"`
	Catalog    string   `json:"catalog"`
	Workspace  string   `json:"workspace"`
	CacheDir   string   `json:"cacheDir"`
	Timeout    string   `json:"timeout"`
	Files      []string `json:"files"`
	Offline    bool     `json:"offline"`
}

func checkConfig(t *testing.T, vars map[string]string, args ...string) effective {
	t.Helper()

	stdout, stderr, code := runWith(t, vars, append(args, "config", "check", "--json")...)
	if code != 0 {
		t.Fatalf("config check: exit %d, stderr %s", code, stderr)
	}

	var got effective
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("config check --json: %v\n%s", err, stdout)
	}

	return got
}

func digestOf(c byte) string {
	return "sha256:" + strings.Repeat(string(c), 64)
}

func TestPrecedenceThroughFlagsAndEnvironment(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	file := filepath.Join(dir, "schepherd.toml")
	doc := `config_version = 1
workspace = "file-ws"
cache_dir = "file-cache"
offline = true

[catalog]
repository = "file.example/org/schemas"
digest = "` + digestOf('f') + `"
`

	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	fromFile := effective{
		Repository: "file.example/org/schemas", Catalog: digestOf('f'), Workspace: filepath.Join(dir, "file-ws"),
		CacheDir: filepath.Join(dir, "file-cache"), Timeout: "10m0s", Files: []string{file}, Offline: true,
	}

	envDir, flagDir := t.TempDir(), t.TempDir()
	allEnv := map[string]string{
		env.KeyConfig:     file,
		env.KeyRepository: "env.example/org/schemas",
		env.KeyCatalog:    digestOf('e'),
		env.KeyCacheDir:   filepath.Join(envDir, "cache"),
		env.KeyWorkspace:  filepath.Join(envDir, "ws"),
		env.KeyOffline:    "false",
		env.KeyTimeout:    "90s",
	}
	fromEnv := effective{
		Repository: "env.example/org/schemas", Catalog: digestOf('e'), Workspace: filepath.Join(envDir, "ws"),
		CacheDir: filepath.Join(envDir, "cache"), Timeout: "1m30s", Files: []string{file}, Offline: false,
	}
	allFlags := []string{
		"--config", file, "--repository", "flag.example/org/schemas", "--catalog", digestOf('c'),
		"--cache-dir", filepath.Join(flagDir, "cache"), "--workspace", filepath.Join(flagDir, "ws"), "--offline=true", "--timeout", "2m",
	}
	fromFlags := effective{
		Repository: "flag.example/org/schemas", Catalog: digestOf('c'), Workspace: filepath.Join(flagDir, "ws"),
		CacheDir: filepath.Join(flagDir, "cache"), Timeout: "2m0s", Files: []string{file}, Offline: true,
	}

	cases := []struct {
		vars map[string]string
		name string
		args []string
		want effective
	}{
		{
			name: "defaults without a file",
			want: effective{Workspace: cwd, Timeout: "10m0s", Files: []string{}},
		},
		{
			name: "--config",
			args: []string{"--config", file},
			want: fromFile,
		},
		{
			name: "SCHEPHERD_CONFIG when --config is not given",
			vars: map[string]string{env.KeyConfig: file},
			want: fromFile,
		},
		{
			name: "--config beats SCHEPHERD_CONFIG",
			vars: map[string]string{env.KeyConfig: filepath.Join(dir, "missing.toml")},
			args: []string{"--config", file},
			want: fromFile,
		},
		{
			name: "blank SCHEPHERD_CONFIG is unset",
			vars: map[string]string{env.KeyConfig: "  "},
			want: effective{Workspace: cwd, Timeout: "10m0s", Files: []string{}},
		},
		{
			name: "every variable beats the file",
			vars: allEnv,
			want: fromEnv,
		},
		{
			name: "every flag beats the environment",
			vars: allEnv,
			args: allFlags,
			want: fromFlags,
		},
		{
			name: "unset variables keep the file",
			vars: map[string]string{env.KeyConfig: file, env.KeyOffline: unset, env.KeyTimeout: unset, env.KeyCatalog: unset},
			want: fromFile,
		},
		{
			name: "--offline=false beats SCHEPHERD_OFFLINE=true",
			vars: map[string]string{env.KeyOffline: "true"},
			args: []string{"--offline=false"},
			want: effective{Workspace: cwd, Timeout: "10m0s", Files: []string{}, Offline: false},
		},
		{
			name: "SCHEPHERD_OFFLINE=1 without a flag",
			vars: map[string]string{env.KeyOffline: "1"},
			want: effective{Workspace: cwd, Timeout: "10m0s", Files: []string{}, Offline: true},
		},
		{
			name: "SCHEPHERD_OFFLINE=0 beats the file",
			vars: map[string]string{env.KeyOffline: "0"},
			args: []string{"--config", file},
			want: effective{
				Repository: fromFile.Repository, Catalog: fromFile.Catalog, Workspace: fromFile.Workspace,
				CacheDir: fromFile.CacheDir, Timeout: "10m0s", Files: []string{file}, Offline: false,
			},
		},
		{
			name: "--offline beats SCHEPHERD_OFFLINE=false",
			vars: map[string]string{env.KeyOffline: "false"},
			args: []string{"--offline"},
			want: effective{Workspace: cwd, Timeout: "10m0s", Files: []string{}, Offline: true},
		},
		{
			name: "relative values resolve against the working directory",
			vars: map[string]string{env.KeyCacheDir: "env-cache"},
			args: []string{"--workspace", "flag-ws"},
			want: effective{Workspace: filepath.Join(cwd, "flag-ws"), CacheDir: filepath.Join(cwd, "env-cache"), Timeout: "10m0s", Files: []string{}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkConfig(t, tc.vars, tc.args...); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("effective configuration\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestInvalidEnvironmentIsAUsageError(t *testing.T) {
	cases := []struct {
		vars      map[string]string
		fragments []string
		args      []string
	}{
		{vars: map[string]string{env.KeyOffline: "yes"}, fragments: []string{env.KeyOffline + `="yes" is not a boolean`}},
		{vars: map[string]string{env.KeyTimeout: "0s"}, fragments: []string{env.KeyTimeout + `="0s" is not a positive duration`}},
		{vars: map[string]string{env.KeyTimeout: "-1s"}, fragments: []string{env.KeyTimeout}},
		{vars: map[string]string{env.KeyTimeout: "soon"}, fragments: []string{env.KeyTimeout}},
		{vars: map[string]string{env.KeyCatalog: "catalog-latest"}, fragments: []string{env.KeyCatalog, "schepherd pin"}},
		{vars: map[string]string{env.KeyRepository: "https://registry.example/org"}, fragments: []string{env.KeyRepository, "without scheme"}},
		{vars: map[string]string{env.KeyCacheDir: filepath.Join(t.TempDir(), "c"), env.KeyConfig: filepath.Join(t.TempDir(), "missing.toml")}, fragments: []string{"missing.toml"}},
		// docs/cli.md promises these two stay errors even when the flag
		// overrides the value.
		{vars: map[string]string{env.KeyOffline: "yes"}, args: []string{"--offline=false"}, fragments: []string{env.KeyOffline + `="yes" is not a boolean`}},
		{vars: map[string]string{env.KeyTimeout: "soon"}, args: []string{"--timeout", "1m"}, fragments: []string{env.KeyTimeout + `="soon" is not a positive duration`}},
	}

	for _, tc := range cases {
		t.Run(strings.Join(append(slices.Clone(tc.args), tc.fragments[0]), " "), func(t *testing.T) {
			stdout, stderr, code := runWith(t, tc.vars, append(slices.Clone(tc.args), "config", "check")...)
			if code != 2 || stdout != "" || !strings.HasPrefix(stderr, "schepherd: usage error: ") {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
			}

			for _, fragment := range tc.fragments {
				if !strings.Contains(stderr, fragment) {
					t.Errorf("stderr %q does not contain %q", stderr, fragment)
				}
			}
		})
	}

	_, stderr, code := runWith(t, map[string]string{env.KeyCatalog: "catalog-latest"}, "--catalog", digestOf('a'), "config", "check")
	if code != 0 {
		t.Errorf("--catalog must win over an invalid %s, which is then never read: exit %d, %s", env.KeyCatalog, code, stderr)
	}

	_, stderr, code = runWith(t, map[string]string{env.KeyRepository: "https://registry.example/org"}, "--repository", "registry.example/org/schemas", "config", "check")
	if code != 0 {
		t.Errorf("--repository must win over an invalid %s, which is then never checked: exit %d, %s", env.KeyRepository, code, stderr)
	}
}
