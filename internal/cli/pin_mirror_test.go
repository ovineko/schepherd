package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/env"
)

func TestPinAndMirrorDoNotNeedTheCatalogDigest(t *testing.T) {
	isolateDockerConfig(t)

	reg := newTestRegistry(t)
	src := reg.repo("org/schemas", "", "")
	dgst := reg.publishCatalog(t, "org/schemas", "catalog-latest")
	cfg := reg.writeConfig(t, "[catalog]\nrepository = \"${"+env.KeyRepository+"}\"\ndigest = \"${"+env.KeyCatalog+"}\"\n")

	for name, value := range map[string]string{"unset": unset, "empty": ""} {
		t.Run(name, func(t *testing.T) {
			vars := map[string]string{env.KeyRepository: value, env.KeyCatalog: value}

			stdout, stderr, code := runWith(t, vars, "--config", cfg, "pin", "--json", src+":catalog-latest")
			if code != 0 {
				t.Fatalf("pin: exit %d, stderr %s", code, stderr)
			}

			var pinned pinResult
			if err := json.Unmarshal([]byte(stdout), &pinned); err != nil || pinned.Digest != dgst || pinned.Revision != "20260924.1200" || pinned.Schemas != len(testSchemas) {
				t.Fatalf("pin --json = %s (%v)", stdout, err)
			}

			dstPath := "mirror-" + name + "/schemas"
			dst := reg.repo(dstPath, "", "")

			stdout, stderr, code = runWith(t, vars, "--config", cfg, "mirror", src+"@"+dgst, dst)
			if code != 0 || stdout != dst+"@"+dgst+"\n" || !reg.has(dstPath, dgst) {
				t.Fatalf("mirror: exit %d, stdout %q, stderr %s", code, stdout, stderr)
			}

			vars[env.KeyRepository] = src

			_, stderr, code = runWith(t, vars, "--config", cfg, "--cache-dir", t.TempDir(), "path", "alpha")
			if code != 2 || !strings.Contains(stderr, "catalog.digest") {
				t.Errorf("path without a digest: exit %d, stderr %q; want a usage error naming catalog.digest", code, stderr)
			}
		})
	}
}

func TestMirrorUsesPerRepositoryCredentials(t *testing.T) {
	isolateDockerConfig(t)

	reg := newTestRegistry(t)
	host := reg.host()
	src := reg.repo("org-a/schemas", "alice", "secret-a")
	dst := reg.repo("org-b/schemas", "bob", "secret-b")
	dgst := reg.publishCatalog(t, "org-a/schemas")

	section := func(key, credentials string) string {
		return "[registries." + strconv.Quote(key) + "]\nplain_http = true\ncredentials_file = " + strconv.Quote(credentials) + "\n\n"
	}

	cfg := reg.writeConfig(t,
		section(host+"/org-a", writeCredentials(t, host, "alice", "secret-a"))+
			section(host+"/org-b", writeCredentials(t, host, "bob", "secret-b")))

	stdout, stderr, code := run(t, "--config", cfg, "mirror", "--json", src+"@"+dgst, dst)
	if code != 0 {
		t.Fatalf("mirror: exit %d, stderr %s", code, stderr)
	}

	var result struct {
		Destination   string `json:"destination"`
		CatalogDigest string `json:"catalogDigest"`
		Schemas       int    `json:"schemas"`
	}

	if err := json.Unmarshal([]byte(stdout), &result); err != nil || result.Destination != dst || result.CatalogDigest != dgst || result.Schemas != len(testSchemas) {
		t.Fatalf("mirror --json = %s (%v)", stdout, err)
	}

	if reg.tagged("org-b/schemas", "catalog-20260924.1200") != dgst {
		t.Error("the destination has no catalog tag")
	}

	stdout, stderr, code = run(t, "--config", cfg, "--cache-dir", t.TempDir(), "--repository", dst, "--catalog", dgst, "cat", "beta")
	if code != 0 || stdout != testSchemas["beta"] {
		t.Errorf("cat from the mirror: exit %d, stdout %q, stderr %s", code, stdout, stderr)
	}
}

func TestUnsupportedCatalogFormatVersionStopsEveryCommand(t *testing.T) {
	isolateDockerConfig(t)

	reg := newTestRegistry(t)
	repo := reg.repo("org/schemas", "", "")
	future := reg.publishRawCatalog(t, "org/schemas", []byte(`{"formatVersion":3,"revision":"20260924.1","schemas":{"alpha":{"layout":"new"}}}`))
	current := reg.publishCatalog(t, "org/schemas")

	marker := filepath.Join(t.TempDir(), "consumer-ran")
	cfg := reg.writeConfig(t, consumerRunner(t, marker, 0))
	input := filepath.Join(t.TempDir(), "alpha.json")

	if err := os.WriteFile(input, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	mirrorPath := "mirror/schemas"
	mirrorRepo := reg.repo(mirrorPath, "", "")

	commands := []struct {
		name    string
		command []string
	}{
		{"list", []string{"list"}},
		{"path", []string{"path", "alpha"}},
		{"catalog", []string{"catalog", "--json"}},
		{"run with --schema", []string{"run", "--schema", "alpha", "--", input}},
		{"run with matching", []string{"run", "--", input}},
		{"pin", []string{"pin", repo + "@" + future}},
		{"mirror", []string{"mirror", repo + "@" + future, mirrorRepo}},
	}

	for _, tc := range commands {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--config", cfg, "--cache-dir", t.TempDir(), "--repository", repo, "--catalog", future}, tc.command...)

			stdout, stderr, code := run(t, args...)
			if code != 2 || stdout != "" || !strings.Contains(stderr, "unsupported catalog formatVersion 3") {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit 2 for the unsupported format", code, stdout, stderr)
			}

			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the consumer was started")
			}
		})
	}

	if reg.has(mirrorPath, future) {
		t.Error("mirror copied a catalog it cannot read")
	}

	_, stderr, code := run(t, "--config", cfg, "--cache-dir", t.TempDir(), "--repository", repo, "--catalog", current, "run", "--schema", "alpha", "--", input)
	if _, err := os.Stat(marker); code != 0 || err != nil {
		t.Errorf("the same run with a current catalog did not start the consumer: exit %d, %v, stderr %s", code, err, stderr)
	}
}
