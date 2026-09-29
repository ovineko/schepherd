package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestPrecedence(t *testing.T) {
	doc := `config_version = 1
workspace = "file-ws"
cache_dir = "file-cache"
offline = true

[catalog]
repository = "file.example/org/schemas"
digest = "` + testDigest + `"
`
	otherDigest := "sha256:" + "ab" + testDigest[len("sha256:")+2:]
	flagDigest := "sha256:" + "cd" + testDigest[len("sha256:")+2:]

	absWorkspace := filepath.Join(t.TempDir(), "abs-ws")

	type want struct {
		workspace, cacheDir, repository, catalog string
		timeout                                  time.Duration
		offline                                  bool
	}

	cases := []struct {
		name  string
		flags Overrides
		env   Overrides
		want  want
	}{
		{
			name: "file values when nothing overrides",
			want: want{workspace: "file-ws", cacheDir: "file-cache", repository: "file.example/org/schemas", catalog: testDigest, offline: true, timeout: DefaultTimeout},
		},
		{
			name:  "flag false beats file true",
			flags: Overrides{Offline: new(false)},
			want:  want{workspace: "file-ws", cacheDir: "file-cache", repository: "file.example/org/schemas", catalog: testDigest, offline: false, timeout: DefaultTimeout},
		},
		{
			name: "env beats file",
			env: Overrides{
				Workspace: new("env-ws"), CacheDir: new("env-cache"), Repository: new("env.example/org/schemas"),
				Catalog: new(otherDigest), Offline: new(false), Timeout: new(time.Minute),
			},
			want: want{workspace: "cwd/env-ws", cacheDir: "cwd/env-cache", repository: "env.example/org/schemas", catalog: otherDigest, offline: false, timeout: time.Minute},
		},
		{
			name: "flag beats env",
			flags: Overrides{
				Workspace: new("flag-ws"), CacheDir: new("flag-cache"), Repository: new("flag.example/org/schemas"),
				Catalog: new(flagDigest), Offline: new(true), Timeout: new(2 * time.Minute),
			},
			env: Overrides{
				Workspace: new("env-ws"), CacheDir: new("env-cache"), Repository: new("env.example/org/schemas"),
				Catalog: new(otherDigest), Offline: new(false), Timeout: new(time.Minute),
			},
			want: want{workspace: "cwd/flag-ws", cacheDir: "cwd/flag-cache", repository: "flag.example/org/schemas", catalog: flagDigest, offline: true, timeout: 2 * time.Minute},
		},
		{
			name:  "unset flag keeps env and file",
			flags: Overrides{Repository: new("flag.example/org/schemas")},
			env:   Overrides{Offline: new(false)},
			want:  want{workspace: "file-ws", cacheDir: "file-cache", repository: "flag.example/org/schemas", catalog: testDigest, offline: false, timeout: DefaultTimeout},
		},
		{
			name:  "absolute override paths are kept",
			flags: Overrides{Workspace: new(absWorkspace)},
			want:  want{workspace: absWorkspace, cacheDir: "file-cache", repository: "file.example/org/schemas", catalog: testDigest, offline: true, timeout: DefaultTimeout},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeTree(t, map[string]string{"conf/schepherd.toml": doc})
			cwd := filepath.Join(dir, "cwd")

			cfg, err := Load(LoadOptions{Path: filepath.Join(dir, "conf", "schepherd.toml"), Cwd: cwd, Flags: tc.flags, Env: tc.env})
			if err != nil {
				t.Fatal(err)
			}

			resolve := func(p string) string {
				switch {
				case filepath.IsAbs(p):
					return filepath.Clean(p)
				case len(p) > 4 && p[:4] == "cwd/":
					return filepath.Join(cwd, p[4:])
				default:
					return filepath.Join(dir, "conf", p)
				}
			}

			got := want{workspace: cfg.Workspace, cacheDir: cfg.CacheDir, repository: cfg.Repository, catalog: cfg.Catalog, offline: cfg.Offline, timeout: cfg.Timeout}
			exp := tc.want
			exp.workspace, exp.cacheDir = resolve(exp.workspace), resolve(exp.cacheDir)

			if got != exp {
				t.Errorf("got %+v\nwant %+v", got, exp)
			}
		})
	}
}

func TestOverrideDefaults(t *testing.T) {
	cwd := t.TempDir()

	cfg, err := Load(LoadOptions{Cwd: cwd, Env: Overrides{Workspace: new("ws"), Offline: new(true)}, Flags: Overrides{CacheDir: new("c")}})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Workspace != filepath.Join(cwd, "ws") || cfg.CacheDir != filepath.Join(cwd, "c") || !cfg.Offline {
		t.Errorf("got %+v", cfg)
	}
}

func TestOverrideValidation(t *testing.T) {
	cases := []struct {
		name      string
		flags     Overrides
		env       Overrides
		fragments []string
	}{
		{"empty workspace flag", Overrides{Workspace: new("")}, Overrides{}, []string{"--workspace must not be empty"}},
		{"empty cache env", Overrides{}, Overrides{CacheDir: new("")}, []string{"SCHEPHERD_CACHE_DIR must not be empty"}},
		{"zero timeout flag", Overrides{Timeout: new(time.Duration(0))}, Overrides{}, []string{"--timeout must be a positive duration"}},
		{"negative timeout env", Overrides{}, Overrides{Timeout: new(-time.Second)}, []string{"SCHEPHERD_TIMEOUT must be a positive duration"}},
		{"tag as catalog flag", Overrides{Catalog: new("catalog-latest")}, Overrides{}, []string{"--catalog: ", "looks like a tag", "schepherd pin"}},
		{"tag as catalog env", Overrides{}, Overrides{Catalog: new("registry.example/org/schemas:catalog-latest")}, []string{"SCHEPHERD_CATALOG: ", "looks like a tag", "schepherd pin"}},
		{"empty catalog flag", Overrides{Catalog: new("")}, Overrides{}, []string{"--catalog: catalog digest must not be empty", "schepherd pin"}},
		{"repository with tag", Overrides{Repository: new("r.example/org/schemas:v1")}, Overrides{}, []string{"--repository: ", "must not contain a tag"}},
		{"empty repository env", Overrides{}, Overrides{Repository: new("")}, []string{"SCHEPHERD_REPOSITORY: repository must not be empty"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(LoadOptions{Cwd: t.TempDir(), Flags: tc.flags, Env: tc.env})
			wantUsage(t, err, tc.fragments...)
		})
	}
}

func TestInterpolation(t *testing.T) {
	doc := `config_version = 1
workspace = "${ROOT}/ws"
cache_dir = "${CACHE_REL}"

[catalog]
repository = "${REGISTRY}/org/${NAME}"
digest = "${DIGEST}"

[registries."r.example"]
ca_file = "${CERTS}/ca.pem"
credentials_file = "$${LITERAL}.json"

[runner]
command = "${BIN}"
args = ["${ARG}", "{files...}"]
`
	root := t.TempDir()
	vars := map[string]string{
		"ROOT": root, "CACHE_REL": "rel-cache", "REGISTRY": "r.example:5000", "NAME": "schemas",
		"DIGEST": testDigest, "CERTS": filepath.Join(root, "certs"),
	}

	cfg, dir := mustLoadDoc(t, doc, vars)

	checks := map[string][2]string{
		"workspace":        {cfg.Workspace, filepath.Join(root, "ws")},
		"relative expands": {cfg.CacheDir, filepath.Join(dir, "rel-cache")},
		"repository":       {cfg.Repository, "r.example:5000/org/schemas"},
		"catalog":          {cfg.Catalog, testDigest},
		"ca_file":          {cfg.Registries["r.example"].CAFile, filepath.Join(root, "certs", "ca.pem")},
		"escape":           {cfg.Registries["r.example"].CredentialsFile, filepath.Join(dir, "${LITERAL}.json")},
		"runner raw":       {cfg.Runner.Command, "${BIN}"},
		"runner args raw":  {cfg.Runner.Args[0], "${ARG}"},
	}

	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %q, want %q", name, c[0], c[1])
		}
	}
}

func TestInterpolationErrors(t *testing.T) {
	cases := []struct {
		name      string
		doc       string
		vars      map[string]string
		fragments []string
	}{
		{
			name:      "unset variable in workspace",
			doc:       "config_version = 1\nworkspace = \"${NOPE}\"\n",
			fragments: []string{":2:1: workspace: environment variable is not set: ${NOPE}"},
		},
		{
			name:      "unset variable in registry path",
			doc:       "config_version = 1\n[registries.localhost]\nca_file = \"${NOPE}\"\n",
			fragments: []string{":3:1: registries.localhost.ca_file: ", "${NOPE}"},
		},
		{
			name:      "empty expansion",
			doc:       "config_version = 1\ncache_dir = \"${EMPTY}\"\n",
			vars:      map[string]string{"EMPTY": ""},
			fragments: []string{"cache_dir: expands to an empty path"},
		},
		{
			name:      "expanded digest is a tag",
			doc:       "config_version = 1\n[catalog]\ndigest = \"${D}\"\n",
			vars:      map[string]string{"D": "catalog-latest"},
			fragments: []string{":3:1: catalog.digest: ", "looks like a tag", "schepherd pin"},
		},
		{
			name:      "expanded repository has a tag",
			doc:       "config_version = 1\n[catalog]\nrepository = \"${R}\"\n",
			vars:      map[string]string{"R": "r.example/org/schemas:latest"},
			fragments: []string{"catalog.repository: ", "must not contain a tag"},
		},
		{
			name:      "expanded value is never scanned again",
			doc:       "config_version = 1\n[catalog]\ndigest = \"${D}\"\n",
			vars:      map[string]string{"D": "${OTHER}", "OTHER": testDigest},
			fragments: []string{":3:1: catalog.digest: \"${OTHER}\" looks like a tag"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := loadDoc(t, tc.doc, tc.vars)
			wantUsage(t, err, tc.fragments...)
		})
	}
}

func TestOverriddenFieldsAreNotInterpolated(t *testing.T) {
	doc := `config_version = 1
workspace = "${UNSET_WS}"
cache_dir = "${UNSET_CACHE}"

[catalog]
repository = "${UNSET_REPO}"
digest = "${UNSET_DIGEST}"
`
	cwd := t.TempDir()

	cases := []struct {
		name  string
		flags Overrides
		env   Overrides
	}{
		{"by flags", Overrides{Workspace: new("w"), CacheDir: new("c"), Repository: new("r.example/x"), Catalog: new(testDigest)}, Overrides{}},
		{"by env", Overrides{}, Overrides{Workspace: new("w"), CacheDir: new("c"), Repository: new("r.example/x"), Catalog: new(testDigest)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeTree(t, map[string]string{"s.toml": doc})

			cfg, err := Load(LoadOptions{Path: filepath.Join(dir, "s.toml"), Cwd: cwd, Flags: tc.flags, Env: tc.env, Lookup: lookupFrom(nil)})
			if err != nil {
				t.Fatal(err)
			}

			if cfg.Workspace != filepath.Join(cwd, "w") || cfg.Repository != "r.example/x" || cfg.Catalog != testDigest {
				t.Errorf("got %+v", cfg)
			}
		})
	}

	dir := writeTree(t, map[string]string{"s.toml": doc})
	_, err := Load(LoadOptions{Path: filepath.Join(dir, "s.toml"), Cwd: cwd, Flags: Overrides{Workspace: new("w")}})
	wantUsage(t, err, "${UNSET_CACHE}")
}

func TestCatalogDigest(t *testing.T) {
	cases := []struct {
		value     string
		fragments []string
	}{
		{"catalog-latest", []string{":3:1: catalog.digest: \"catalog-latest\" looks like a tag", "schepherd pin"}},
		{"registry.example/org/schemas:catalog-latest", []string{"looks like a tag", "schepherd pin"}},
		{"repo:tag", []string{"looks like a tag", "schepherd pin"}},
		{"registry.example/org/schemas@" + testDigest, []string{"is a full reference", "schepherd pin"}},
		{"sha256:0123", []string{"invalid digest", "schepherd pin"}},
		{"sha512:" + testDigest[7:] + testDigest[7:], []string{"unsupported digest algorithm", "schepherd pin"}},
		{"sha256:" + "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789", []string{"lowercase", "schepherd pin"}},
	}

	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			_, _, err := loadDoc(t, "config_version = 1\n[catalog]\ndigest = \""+tc.value+"\"\n", nil)
			wantUsage(t, err, tc.fragments...)
		})
	}

	cfg, _ := mustLoadDoc(t, "config_version = 1\n[catalog]\ndigest = \""+testDigest+"\"\n", nil)
	if cfg.Catalog != testDigest {
		t.Errorf("Catalog = %q", cfg.Catalog)
	}
}

func TestRepository(t *testing.T) {
	valid := []string{"r.example/org/schemas", "localhost:5000/schemas", "[::1]:5000/a/b", "registry-1.example.com/a__b/c-d/e.f", "localhost:65535/x", "[fe80::1]:1/x"}
	for _, repo := range valid {
		if err := checkRepository(repo); err != nil {
			t.Errorf("checkRepository(%q) = %v", repo, err)
		}
	}

	invalid := map[string]string{
		"":                             "must not be empty",
		"https://r.example/org":        "without scheme",
		"r.example/org@" + testDigest:  "must not contain a digest",
		"r.example/org/schemas:latest": "must not contain a tag",
		"r.example":                    "host[:port]/path",
		"r.example/":                   "host[:port]/path",
		"r.example//x":                 "path component \"\"",
		"r.example/Org":                "path component \"Org\"",
		"-bad.example/x":               "host[:port]/path",
		"r.example/a b":                "path component",
		"Registry.example/org/schemas": `repository "Registry.example/org/schemas": registry hosts and repository paths must be lower case; write host "Registry.example" as "registry.example"`,
		"LOCALHOST:5000/org":           `write host "LOCALHOST:5000" as "localhost:5000"`,
		"[FE80::1]:5000/x":             `write host "[FE80::1]:5000" as "[fe80::1]:5000"`,
		"localhost:0/x":                `repository "localhost:0/x": host "localhost:0": the port must be between 1 and 65535`,
		"localhost:65536/x":            "the port must be between 1 and 65535",
		"[1.2.3.4]/x":                  `host "[1.2.3.4]": "1.2.3.4" is not an IPv6 address`,
		"[:]:5000/x":                   `":" is not an IPv6 address`,
	}

	for repo, fragment := range invalid {
		_, _, err := loadDoc(t, "config_version = 1\n[catalog]\nrepository = \""+repo+"\"\n", nil)
		if repo == "" {
			fragment = "catalog.repository: must not be empty"
		}

		wantUsage(t, err, fragment)
	}
}

func TestWithoutCatalogSkipsTheCatalogSection(t *testing.T) {
	doc := `config_version = 1

[catalog]
repository = "${UNSET_REPO}"
digest = "${DIGEST}"

[registries."r.example/org"]
plain_http = true
credentials_file = "${CREDS}"
`
	dir := writeTree(t, map[string]string{"s.toml": doc})
	path := filepath.Join(dir, "s.toml")

	for name, vars := range map[string]map[string]string{
		"unset digest": {"CREDS": "auth.json"},
		"empty digest": {"CREDS": "auth.json", "DIGEST": ""},
		"tag digest":   {"CREDS": "auth.json", "DIGEST": "catalog-latest"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(LoadOptions{
				Path: path, Cwd: dir, Lookup: lookupFrom(vars), WithoutCatalog: true,
				Flags: Overrides{Catalog: new("catalog-latest")}, Env: Overrides{Repository: new("not a repository")},
			})
			if err != nil {
				t.Fatal(err)
			}

			if cfg.Repository != "" || cfg.Catalog != "" {
				t.Errorf("repository %q catalog %q, want both empty", cfg.Repository, cfg.Catalog)
			}

			if got := cfg.Registries["r.example/org"]; !got.PlainHTTP || got.CredentialsFile != filepath.Join(dir, "auth.json") {
				t.Errorf("registries = %+v", cfg.Registries)
			}

			_, err = Load(LoadOptions{Path: path, Cwd: dir, Lookup: lookupFrom(vars)})
			wantUsage(t, err, "catalog.")
		})
	}

	_, err := Load(LoadOptions{Path: path, Cwd: dir, Lookup: lookupFrom(nil), WithoutCatalog: true})
	wantUsage(t, err, "credentials_file", "${CREDS}")

	literal := writeTree(t, map[string]string{"s.toml": "config_version = 1\n[catalog]\ndigest = \"catalog-latest\"\n"})
	_, err = Load(LoadOptions{Path: filepath.Join(literal, "s.toml"), Cwd: literal, WithoutCatalog: true})
	wantUsage(t, err, "looks like a tag")
}
