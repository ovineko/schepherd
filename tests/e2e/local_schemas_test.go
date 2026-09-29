//go:build e2e

package e2e

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Paths inside the validators container (E40).
const (
	localContainerBin       = "/e2e/bin/schepherd"
	localCheckJSONSchemaBin = "/usr/local/bin/check-jsonschema"
	localContainerWorkspace = "/e2e/ws"
	localContainerReport    = "/e2e/report"
	localContainerCache     = "/e2e/cache"
	// localNoCache is below the read-only root file system of the container:
	// a command that tried to create or open its cache there would fail.
	localNoCache = "/e2e/no-cache"
)

// localRunner is the documented check-jsonschema runner (E31).
const localRunner = `
[runner]
mode = "batch"
command = "${CHECK_JSONSCHEMA_BIN}"
args = ["--schemafile", "{schema}", "{files...}"]
`

// localServiceSchema declares the workspace's own service schema. The
// configuration lives in tools/, so the path is relative to that directory
// and not to the working directory.
const localServiceSchema = `
[schemas.service]
path = "../schemas/service.schema.json"
file_match = ["services/*.json"]
`

// localAlphaOverride replaces the catalog schema alpha of set-basic.
const localAlphaOverride = `
[schemas.alpha]
path = "../schemas/alpha-override.schema.json"
file_match = ["alpha.json", "owned/*.json"]
`

// localReport is the part of a run report (docs/runner.md) E40 checks.
type localReport struct {
	Tasks []struct {
		SchemaID  string   `json:"schemaId"`
		SchemaRef string   `json:"schemaRef"`
		Origin    string   `json:"origin"`
		Status    string   `json:"status"`
		Files     []string `json:"files"`
		ExitCode  int      `json:"exitCode"`
	} `json:"tasks"`
	ExitCode int `json:"exitCode"`
}

// localTask is one expected task of a run report; files are relative to the
// container workspace.
type localTask struct {
	id, ref, origin, status string
	files                   []string
}

// localWorkspace copies fixtures/local (a repository with its own schemas in
// schemas/: service.schema.json takes its port definition from the sibling
// common.schema.json through a relative $ref, alpha-override.schema.json
// replaces the catalog's alpha) into a fresh directory with symlinks
// resolved, so paths the client prints compare as strings.
func localWorkspace(t *testing.T) string {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	ws := filepath.Join(dir, "ws")
	copyTree(t, fixture("local"), ws)

	return ws
}

// localRun runs "schepherd --config <config> <args...> run --report <file>
// <runArgs...>" inside the validators container without network, with the
// workspace mounted read-only as the working directory, and returns the
// result and the report.
func localRun(t *testing.T, ws, config string, mounts []mount, args, runArgs []string) (result, localReport) {
	t.Helper()

	reports := t.TempDir()
	full := slices.Concat(
		[]string{localContainerBin, "--config", config}, args,
		[]string{"run", "--report", localContainerReport + "/report.json"}, runArgs,
	)

	res := dockerRun(t, validatorsImage(t), dockerOpts{
		Mounts: append([]mount{
			binMount(),
			{Host: ws, Container: localContainerWorkspace},
			{Host: reports, Container: localContainerReport, Writable: true},
		}, mounts...),
		Network: "none",
		Workdir: localContainerWorkspace,
		Env:     []string{"CHECK_JSONSCHEMA_BIN=" + localCheckJSONSchemaBin},
	}, full...)

	return res, decodeJSON[localReport](t, readFile(t, filepath.Join(reports, "report.json")))
}

// localHostOpts runs the client on the host in the workspace. The runner is
// only started inside the container, but config check resolves its
// variables on the host too.
func localHostOpts(ws string) runOpts {
	return runOpts{Dir: ws, Env: []string{"CHECK_JSONSCHEMA_BIN=" + localCheckJSONSchemaBin}}
}

func localWantTasks(t *testing.T, res result, rep localReport, code int, want []localTask) {
	t.Helper()

	res.wantCode(t, code)

	if rep.ExitCode != code || len(rep.Tasks) != len(want) {
		t.Fatalf("report: want exitCode %d and %d task(s), got %+v\n%s", code, len(want), rep, res)
	}

	for i, w := range want {
		files := make([]string, 0, len(w.files))
		for _, f := range w.files {
			files = append(files, localContainerWorkspace+"/"+f)
		}

		got := rep.Tasks[i]
		if got.SchemaID != w.id || got.SchemaRef != w.ref || got.Origin != w.origin || got.Status != w.status || !slices.Equal(got.Files, files) {
			t.Fatalf("report task %d = %+v, want %s (%s, %s) %s on %q\n%s", i, got, w.id, w.ref, w.origin, w.status, files, res)
		}
	}
}

func localNoCacheDir(t *testing.T, dir string) {
	t.Helper()

	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("local schemas must not touch the cache, but %s exists (%v)", dir, err)
	}
}

// TestE40_LocalSchemas uses JSON Schemas that live in the project's own
// repository, named in [schemas] by paths relative to the configuration:
// without any registry or catalog, offline, inside the validators container
// with a read-only root file system and no network, check-jsonschema
// validates against the local file in place and resolves its relative $ref
// to a sibling file; the cache is never created. A local schema with the ID
// of a catalog entry replaces that entry: the real validator sees the
// override, the other catalog schemas still come from the pinned catalog.
func TestE40_LocalSchemas(t *testing.T) {
	t.Parallel()

	t.Run("repository schemas without registry", func(t *testing.T) {
		t.Parallel()

		ws := localWorkspace(t)
		sb := sandboxOf(t)
		cfg := filepath.Join(ws, "tools", "schepherd.toml")
		writeFile(t, cfg, []byte("config_version = 1\n"+localServiceSchema+localRunner))

		schema := filepath.Join(ws, "schemas", "service.schema.json")
		o := localHostOpts(ws)

		if res := cli(t, o, "--config", cfg, "--offline", "path", "service").ok(t); string(res.Stdout) != schema+"\n" {
			t.Fatalf("path must print the local file itself, not a copy:\n%s", res)
		}

		if res := cli(t, o, "--config", cfg, "cat", "service").ok(t); !bytes.Equal(res.Stdout, readFile(t, schema)) {
			t.Fatalf("cat must print the local file byte for byte:\n%s", res)
		}

		resolved := decodeJSON[map[string]any](t, cli(t, o, "--config", cfg, "resolve", "--json", "--file", "services/frontend.json").ok(t).Stdout)
		if resolved["schema"] != "service" || resolved["origin"] != "local" || resolved["schemaPath"] != schema || resolved["artifact"] != nil {
			t.Fatalf("resolve through a local file_match = %v", resolved)
		}

		list := decodeJSON[[]map[string]any](t, cli(t, o, "--config", cfg, "--offline", "list", "--json").ok(t).Stdout)
		if len(list) != 1 || list[0]["id"] != "service" || list[0]["origin"] != "local" || list[0]["schemaPath"] != schema || list[0]["shadows"] != false {
			t.Fatalf("list without a catalog must show only the local schema: %v", list)
		}

		cli(t, o, "--config", cfg, "config", "check").ok(t)

		// A shared base names the schema relative to its own directory
		// through ${NAME}; the configuration that extends it sets only
		// file_match, and the path keeps the base as its origin.
		writeFile(t, filepath.Join(ws, "shared", "base.toml"),
			[]byte("config_version = 1\n\n[schemas.service]\npath = \"${LOCAL_SCHEMAS}/service.schema.json\"\nfile_match = [\"legacy/*.json\"]\n"))

		extended := filepath.Join(ws, "tools", "extended.toml")
		writeFile(t, extended, []byte("config_version = 1\nextends = [\"../shared/base.toml\"]\n\n[schemas.service]\nfile_match = [\"services/*.json\"]\n"))

		eo := o
		eo.Env = append(slices.Clone(o.Env), "LOCAL_SCHEMAS=../schemas")

		if res := cli(t, eo, "--config", extended, "path", "service").ok(t); string(res.Stdout) != schema+"\n" {
			t.Fatalf("a path from an extended base must stay relative to the base:\n%s", res)
		}

		if res := cli(t, eo, "--config", extended, "resolve", "--file", "services/frontend.json").ok(t); string(res.Stdout) != "service\n" {
			t.Fatalf("file_match of the extending file must apply:\n%s", res)
		}

		cli(t, eo, "--config", extended, "resolve", "--file", "legacy/frontend.json").wantCode(t, 3)

		res := cli(t, o, "--config", cfg, "path", "alpha").wantCode(t, 2)
		if !strings.Contains(string(res.Stderr), "no catalog is configured") {
			t.Fatalf("a catalog ID without a catalog must fail clearly:\n%s", res)
		}

		localNoCacheDir(t, sb.CacheDir)

		container := localContainerWorkspace + "/tools/schepherd.toml"
		ref := "local:schemas/service.schema.json"

		for _, offline := range [][]string{{"--offline"}, {}} {
			args := append(slices.Clone(offline), "--cache-dir", localNoCache)

			res, rep := localRun(t, ws, container, nil, args, []string{"--", "services/frontend.json"})
			localWantTasks(t, res, rep, 0, []localTask{{id: "service", ref: ref, origin: "local", status: "ok", files: []string{"services/frontend.json"}}})

			if !strings.Contains(string(res.Stdout), "ok -- validation done") {
				t.Fatalf("check-jsonschema did not validate against the local schema:\n%s", res)
			}

			res, rep = localRun(t, ws, container, nil, args, []string{"--", "services/low-port.json"})
			localWantTasks(t, res, rep, 1, []localTask{{id: "service", ref: ref, origin: "local", status: "failed", files: []string{"services/low-port.json"}}})

			// Only common.schema.json forbids port 80, so this error proves
			// that the relative $ref was resolved next to the local file.
			if out := string(res.Stdout); !strings.Contains(out, "low-port.json") || !strings.Contains(out, "1024") {
				t.Fatalf("check-jsonschema must reject port 80 through the sibling's minimum of 1024:\n%s", res)
			}
		}

		res, _ = localRun(t, ws, container, nil, []string{"--offline", "--cache-dir", localNoCache}, []string{"--schema", "service", "--", "owned/alpha.json"})
		if res.Code != 1 || !strings.Contains(string(res.Stdout), "'port' is a required property") {
			t.Fatalf("--schema with a local ID must validate against the local file:\n%s", res)
		}
	})

	t.Run("local override shadows a catalog schema", func(t *testing.T) {
		t.Parallel()

		repo := suite.source.Repo(repoPath(t))
		published := publishSet(t, repo, newSet(t, "set-basic"), "--now", "20260101.0000")
		ws := localWorkspace(t)
		sb := sandboxOf(t)
		o := localHostOpts(ws)

		writeFile(t, filepath.Join(ws, "alpha.json"), readFile(t, instance("alpha", "valid.json")))
		writeFile(t, filepath.Join(ws, "config", "app", "settings.toml"), readFile(t, instance("beta", "valid.toml")))
		writeFile(t, filepath.Join(ws, ".github", "workflows", "ci.yml"), []byte("name: ci\n"))

		hostCfg := filepath.Join(ws, "tools", "schepherd.toml")
		writeFile(t, hostCfg, []byte(clientConfig{Repository: repo, Catalog: published.CatalogDigest, Extra: localAlphaOverride + localRunner}.TOML()))

		none := map[string]*registrySettings{}
		for host := range defaultRegistrySettings() {
			none[host] = nil
		}

		writeFile(t, filepath.Join(ws, "tools", "offline.toml"), []byte(clientConfig{
			Repository: repo, Catalog: published.CatalogDigest, Registries: none, Extra: localAlphaOverride + localRunner,
		}.TOML()))

		override := filepath.Join(ws, "schemas", "alpha-override.schema.json")

		if res := cli(t, o, "--config", hostCfg, "path", "alpha").ok(t); string(res.Stdout) != override+"\n" {
			t.Fatalf("path alpha must print the local override:\n%s", res)
		}

		if res := cli(t, o, "--config", hostCfg, "cat", "alpha").ok(t); !bytes.Equal(res.Stdout, readFile(t, override)) {
			t.Fatalf("cat alpha must print the local override:\n%s", res)
		}

		localNoCacheDir(t, sb.CacheDir)

		list := decodeJSON[[]map[string]any](t, cli(t, o, "--config", hostCfg, "list", "--json").ok(t).Stdout)
		digests := map[string]string{}
		alphas := 0

		for _, item := range list {
			if item["id"] == "alpha" {
				alphas++
			}

			if item["origin"] == "catalog" {
				digests[item["id"].(string)] = item["digest"].(string)
			}
		}

		if alphas != 1 || list[0]["id"] != "alpha" || list[0]["origin"] != "local" || list[0]["shadows"] != true || list[0]["schemaPath"] != override ||
			list[0]["digest"] != nil || digests["beta"] == "" || digests["gamma"] == "" {
			t.Fatalf("list must show the override once instead of the catalog's alpha: %v", list)
		}

		// The override replaces the catalog entry with its fileMatch, so the
		// catalog's workflow pattern for alpha no longer applies.
		cli(t, o, "--config", hostCfg, "resolve", "--file", ".github/workflows/ci.yml").wantCode(t, 3)

		cli(t, o, "--config", hostCfg, "path", "beta").ok(t)

		cache := []mount{{Host: sb.CacheDir, Container: localContainerCache, Writable: true}}
		args := []string{"--offline", "--cache-dir", localContainerCache}
		container := localContainerWorkspace + "/tools/offline.toml"
		overrideRef := "local:schemas/alpha-override.schema.json"

		// owned/alpha.json has an "owner", which the catalog's alpha forbids
		// (additionalProperties: false) and the override requires.
		res, rep := localRun(t, ws, container, cache, args, []string{"--", "owned/alpha.json", "config/app/settings.toml"})
		localWantTasks(t, res, rep, 0, []localTask{
			{id: "alpha", ref: overrideRef, origin: "local", status: "ok", files: []string{"owned/alpha.json"}},
			{id: "beta", ref: repo + "@" + digests["beta"], origin: "catalog", status: "ok", files: []string{"config/app/settings.toml"}},
		})

		res, rep = localRun(t, ws, container, cache, args, []string{"--", "alpha.json", "config/app/settings.toml"})
		localWantTasks(t, res, rep, 1, []localTask{
			{id: "alpha", ref: overrideRef, origin: "local", status: "failed", files: []string{"alpha.json"}},
			{id: "beta", ref: repo + "@" + digests["beta"], origin: "catalog", status: "ok", files: []string{"config/app/settings.toml"}},
		})

		if !strings.Contains(string(res.Stdout), "'owner' is a required property") {
			t.Fatalf("the catalog-valid alpha.json must fail against the override:\n%s", res)
		}
	})
}
