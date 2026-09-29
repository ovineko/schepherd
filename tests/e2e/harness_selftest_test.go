//go:build e2e

package e2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ovineko/schepherd/tests/e2e/regproxy"
)

// The TestHarness_* tests check the harness itself: the fixtures, the
// helper API and the infrastructure, independently of any product scenario.

// TestHarness_Validators runs both validators of the validators image with
// no network against every instance fixture: valid ones pass, invalid ones
// fail, and the static schepherd binary runs inside the image.
func TestHarness_Validators(t *testing.T) {
	t.Parallel()

	stdinDir := t.TempDir()
	writeStdinFixtures(t, stdinDir)

	mounts := []mount{
		{Host: fixture("set-basic", "schemas"), Container: "/schemas"},
		{Host: fixture("instances"), Container: "/instances"},
		{Host: stdinDir, Container: "/stdin"},
		binMount(),
	}

	t.Run("schepherd binary", func(t *testing.T) {
		t.Parallel()

		res := dockerRunValidators(t, mounts, "/e2e/bin/schepherd", "version", "--json").ok(t)
		if !json.Valid(res.Stdout) {
			t.Fatalf("version --json is not JSON:\n%s", res)
		}
	})

	t.Run("stdin fixtures are valid alpha instances", func(t *testing.T) {
		t.Parallel()

		args := make([]string, 0, 3+len(stdinFixtures))
		args = append(args, "check-jsonschema", "--schemafile", "/schemas/alpha.json")

		for name := range stdinFixtures {
			args = append(args, "/stdin/"+name)
		}

		dockerRunValidators(t, mounts, args...).ok(t)
	})

	for _, id := range []string{"alpha", "beta", "gamma", "delta"} {
		entries, err := os.ReadDir(fixture("instances", id))
		if err != nil {
			t.Fatal(err)
		}

		for _, e := range entries {
			name := e.Name()
			valid := strings.HasPrefix(name, "valid.")

			t.Run(id+"/"+name, func(t *testing.T) {
				t.Parallel()

				schema, file := "/schemas/"+id+".json", "/instances/"+id+"/"+name
				if _, err := os.Stat(instance(id, name)); err != nil {
					t.Fatal(err)
				}

				want := map[bool]int{true: 0, false: 1}[valid]
				dockerRunValidators(t, mounts, "check-jsonschema", "--schemafile", schema, file).wantCode(t, want)

				if strings.HasSuffix(name, ".toml") {
					return
				}

				want = map[bool]int{true: 0, false: 2}[valid]
				dockerRunValidators(t, mounts, "jsonschema", "validate", schema, file).wantCode(t, want)
			})
		}
	}
}

// TestHarness_RawArtifacts pushes a hand-crafted but well-formed schema and
// catalog with pushRaw and checks that the direct registry helpers read them
// back and that the client accepts them, so hostile variants built with the
// same helpers fail for the reason a test intends.
func TestHarness_RawArtifacts(t *testing.T) {
	reg := suite.source
	path := repoPath(t)

	var content bytes.Buffer
	if err := json.Compact(&content, readFile(t, fixture("set-basic", "schemas", "gamma.json"))); err != nil {
		t.Fatal(err)
	}

	schema := pushRaw(t, reg, path, rawArtifact{
		Layers: []rawLayer{schemaLayer(mediaTypeSchemaGzip, gzipBytes(content.Bytes()), content.Bytes())},
	})

	doc, err := json.Marshal(catalogDoc{FormatVersion: catalogFormatVersion, Revision: "20260101.0000", Schemas: []catalogEntry{
		{ID: "raw", Name: "Raw gamma", FileMatch: []string{"raw.json"}, Artifact: descriptorOf(schema)},
	}})
	if err != nil {
		t.Fatal(err)
	}

	catalog := pushRawCatalog(t, reg, path, doc, "raw-catalog")

	if tags := reg.Tags(t, path); !slices.Equal(tags, []string{"raw-catalog"}) {
		t.Fatalf("tags %v", tags)
	}

	if tags := reg.Tags(t, path+"-missing"); tags != nil {
		t.Fatalf("tags of a missing repository: %v", tags)
	}

	m := reg.Manifest(t, path, "raw-catalog")
	if m.Digest != catalog.Digest.String() || m.Manifest.ArtifactType != artifactTypeCatalog {
		t.Fatalf("catalog manifest %s %q", m.Digest, m.Manifest.ArtifactType)
	}

	if got := reg.ManifestStatus(t, path, schema.Digest.String()); got != http.StatusOK {
		t.Fatalf("HEAD schema manifest: %d", got)
	}

	if got := reg.BlobStatus(t, path, digestOf([]byte("absent"))); got != http.StatusNotFound {
		t.Fatalf("HEAD missing blob: %d", got)
	}

	sb := sandboxOf(t)
	cfg := writeConfig(t, sb.Workspace, clientConfig{Repository: reg.Repo(path), Catalog: catalog.Digest.String()}.TOML())

	writeFiles(t, sb.Workspace, map[string]string{"nested/dir/raw.json": "{}\n"})

	resolved := decodeJSON[struct {
		Schema string `json:"schema"`
	}](t, cli(t, runOpts{}, "--config", cfg, "resolve", "--file", "nested/dir/raw.json", "--json").ok(t).Stdout)
	if resolved.Schema != "raw" {
		t.Fatalf("resolve picked %q", resolved.Schema)
	}

	res := cli(t, runOpts{}, "--config", cfg, "path", "raw").ok(t)
	materialized := strings.TrimSuffix(string(res.Stdout), "\n")

	if !bytes.Equal(readFile(t, materialized), content.Bytes()) {
		t.Fatalf("materialized %s differs from the pushed content", materialized)
	}

	if !strings.HasPrefix(materialized, sb.CacheDir+string(filepath.Separator)) || filepath.Base(materialized) != "schema.json" {
		t.Fatalf("materialized path %s is not a schema.json below %s", materialized, sb.CacheDir)
	}

	if !slices.ContainsFunc(fileTree(t, sb.CacheDir), func(p string) bool { return strings.HasSuffix(p, "/schema.json") }) {
		t.Fatalf("cache tree has no schema.json: %v", fileTree(t, sb.CacheDir))
	}

	if cat := cli(t, runOpts{}, "--config", cfg, "cat", "raw").ok(t); !bytes.Equal(cat.Stdout, content.Bytes()) {
		t.Fatal("cat output differs from the pushed content")
	}
}

// TestHarness_ServiceRestart stops and starts the mirror registry: its data
// survives, the direct address is rediscovered, the proxies keep their
// addresses, and faults and the down switch of a proxy behave as documented.
// It must not run in parallel with other tests.
func TestHarness_ServiceRestart(t *testing.T) {
	reg := suite.mirror
	px := newProxy(t, reg)
	path := repoPath(t)

	if px.Registry() != reg {
		t.Fatal("proxy reports another registry")
	}

	content := []byte(`{"type":"string"}`)
	schema := pushRaw(t, reg, path, rawArtifact{Layers: []rawLayer{schemaLayer(mediaTypeSchemaJSON, content, content)}})

	doc, err := json.Marshal(catalogDoc{FormatVersion: catalogFormatVersion, Revision: "20260101.0000", Schemas: []catalogEntry{
		{ID: "restart", Name: "Restart", Artifact: descriptorOf(schema)},
	}})
	if err != nil {
		t.Fatal(err)
	}

	catalog := pushRawCatalog(t, reg, path, doc).Digest.String()

	catalogVia := func(t *testing.T, repo string) result {
		t.Helper()

		cfg := writeConfig(t, t.TempDir(), clientConfig{Repository: repo, Catalog: catalog}.TOML())

		return cli(t, runOpts{Sandbox: newSandbox(t)}, "--config", cfg, "catalog", "--json")
	}

	stopService(t, serviceMirror)

	if serviceRunning(serviceMirror) {
		t.Fatal("mirror still running after stop")
	}

	catalogVia(t, px.Repo(path)).wantCode(t, 4)

	startService(t, serviceMirror)
	t.Logf("mirror address after restart: %s", reg.Host())

	for _, repo := range []string{reg.Repo(path), px.Repo(path), suite.mirrorProxy.Repo(path)} {
		if res := catalogVia(t, repo).ok(t); !bytes.Equal(res.Stdout, doc) {
			t.Fatalf("catalog via %s differs after the restart", repo)
		}
	}

	px.Reset(t)
	px.AddFault(t, regproxy.Fault{Name: "blob-503", Class: regproxy.ClassBlobGet, Action: regproxy.ActionStatus, Status: http.StatusServiceUnavailable})
	catalogVia(t, px.Repo(path)).wantCode(t, 4)

	if !slices.ContainsFunc(px.Records(t), func(r regproxy.Record) bool { return r.Fault == "blob-503" }) {
		t.Fatalf("no record shows the fault: %+v", px.Records(t))
	}

	px.ClearFaults()
	catalogVia(t, px.Repo(path)).ok(t)

	px.SetDown(t, true)
	catalogVia(t, px.Repo(path)).wantCode(t, 4)

	if stats := px.Stats(t); stats.ByClass[regproxy.ClassBlobGet] == 0 {
		t.Fatalf("stats %+v", stats)
	}
}

// TestHarness_AuthRegistry reaches the TLS registry with htpasswd through
// every credential path the harness offers: a credentials_file, a Docker
// credential helper on the sandbox PATH, and a plain-HTTP proxy in front of
// it. Passwords never reach the output, and the registry logs its user.
func TestHarness_AuthRegistry(t *testing.T) {
	reg := suite.auth
	if !reg.TLS() {
		t.Fatal("registry-auth is not a TLS registry")
	}

	path := repoPath(t)
	content := []byte(`{"type":"object"}`)
	schema := pushRaw(t, reg, path, rawArtifact{Layers: []rawLayer{schemaLayer(mediaTypeSchemaJSON, content, content)}})

	doc, err := json.Marshal(catalogDoc{FormatVersion: catalogFormatVersion, Revision: "20260101.0000", Schemas: []catalogEntry{
		{ID: "auth", Name: "Auth", Artifact: descriptorOf(schema)},
	}})
	if err != nil {
		t.Fatal(err)
	}

	catalog := pushRawCatalog(t, reg, path, doc).Digest.String()
	user := reg.User()

	run := func(t *testing.T, repo string, registries map[string]*registrySettings, env ...string) result {
		t.Helper()

		sb := newSandbox(t)
		cfg := writeConfig(t, sb.Workspace, clientConfig{Repository: repo, Catalog: catalog, Registries: registries}.TOML())
		res := cli(t, runOpts{Sandbox: sb, Env: env}, "--config", cfg, "catalog", "--json")

		if strings.Contains(string(res.Stdout)+string(res.Stderr), user.Password) {
			t.Fatalf("output contains the password:\n%s", res)
		}

		return res
	}

	t.Run("without credentials", func(t *testing.T) {
		run(t, reg.Repo(path), nil).wantCode(t, 4)
	})

	t.Run("credentials_file", func(t *testing.T) {
		creds := writeDockerConfig(t, t.TempDir(), map[string]credential{reg.Host(): user})
		res := run(t, reg.Repo(path), map[string]*registrySettings{reg.Host(): {CAFile: suite.pki.CAFile, CredentialsFile: creds}}).ok(t)

		if !bytes.Equal(res.Stdout, doc) {
			t.Fatal("catalog differs")
		}
	})

	t.Run("credential helper", func(t *testing.T) {
		dir := t.TempDir()
		db, log := filepath.Join(dir, "db.json"), filepath.Join(dir, "helper.log")
		writeJSONFile(t, dir, "db.json", map[string]map[string]string{reg.Host(): {"Username": user.Username, "Secret": user.Password}})

		sb := newSandbox(t)
		writeCredsStoreConfig(t, sb.DockerConfig, "e2e")
		cfg := writeConfig(t, sb.Workspace, clientConfig{Repository: reg.Repo(path), Catalog: catalog}.TOML())
		cli(t, runOpts{Sandbox: sb, Env: []string{"E2E_CREDHELPER_DB=" + db, "E2E_CREDHELPER_LOG=" + log}},
			"--config", cfg, "catalog", "--json").ok(t)

		if !strings.Contains(string(readFile(t, log)), `"op":"get"`) {
			t.Fatalf("helper log has no get:\n%s", readFile(t, log))
		}
	})

	t.Run("through a proxy", func(t *testing.T) {
		px := newProxy(t, reg)
		creds := writeDockerConfig(t, t.TempDir(), map[string]credential{px.Host(): user})
		run(t, px.Repo(path), map[string]*registrySettings{px.Host(): {PlainHTTP: true, CredentialsFile: creds}}).ok(t)

		if stats := px.Stats(t); stats.WithAuthorization == 0 {
			t.Fatalf("no authorized request through the proxy: %+v", stats)
		}
	})

	logs := serviceLogs(t, serviceAuth)
	if !strings.Contains(logs, user.Username) || strings.Contains(logs, user.Password) {
		t.Fatalf("registry-auth logs do not show user %s (or show its password)", user.Username)
	}

	if strings.Contains(serviceLogs(t, serviceAuth2), user.Username) {
		t.Fatal("registry-auth2 logs show the user of registry-auth")
	}
}

// TestHarness_Processes checks the process helpers with the testconsumer:
// asynchronous start, signals, waitGone and byte-exact stdin.
func TestHarness_Processes(t *testing.T) {
	logDir := t.TempDir()

	p := startBin(t, "testconsumer", runOpts{Env: []string{"TC_LOG_DIR=" + logDir, "TC_HANG=1"}})

	eventually(t, 10*time.Second, "the consumer record", func() bool {
		entries, _ := os.ReadDir(logDir)

		return len(entries) > 0
	})

	if err := p.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	if res := p.Wait(); res.Signal != syscall.SIGTERM || res.Err != nil {
		t.Fatalf("want death by SIGTERM:\n%s", res)
	}

	waitGone(t, p.PID(), 5*time.Second)

	if processAlive(p.PID()) {
		t.Fatal("process still alive")
	}

	stdinLog := t.TempDir()
	input := stdinFixtures["mixed.yaml"]
	runBin(t, "testconsumer", runOpts{Env: []string{"TC_LOG_DIR=" + stdinLog, "TC_READ_STDIN=1"}, Stdin: input}).ok(t)

	entries, err := os.ReadDir(stdinLog)
	if err != nil || len(entries) != 1 {
		t.Fatalf("records: %v %v", entries, err)
	}

	rec := decodeJSON[struct {
		Stdin string `json:"stdin"`
		Env   map[string]string
	}](t, readFile(t, filepath.Join(stdinLog, entries[0].Name())))

	if got, _ := base64.StdEncoding.DecodeString(rec.Stdin); !bytes.Equal(got, input) {
		t.Fatalf("stdin %q, want %q", got, input)
	}

	if path := rec.Env["PATH"]; !strings.HasPrefix(path, suite.bin+string(os.PathListSeparator)) {
		t.Fatalf("sandbox PATH %q does not start with the run's binaries", path)
	}
}

// TestHarness_NpmRegistry starts the npm profile, reaches verdaccio from the
// host and from containers on the compose network (including the node and
// bun images of the packaging scenarios), and checks that npmRegistry
// follows verdaccio to its new port after a restart. It must not run in
// parallel with other tests.
func TestHarness_NpmRegistry(t *testing.T) {
	ping := func(t *testing.T, base string) {
		t.Helper()

		if err := expectStatus(t.Context(), &http.Client{Timeout: 10 * time.Second}, base+"/-/ping", http.StatusOK); err != nil {
			t.Fatal(err)
		}
	}

	before := npmRegistry(t)
	ping(t, before)

	inside := "http://" + serviceNpm + ":4873/"
	network := dockerOpts{Network: composeNetwork(), Env: []string{"NPM_CONFIG_UPDATE_NOTIFIER=false"}}

	dockerRun(t, nodeImage, network, "npm", "ping", "--registry", inside).ok(t)

	if res := dockerRun(t, bunImage, dockerOpts{}, "bun", "--version").ok(t); !strings.HasPrefix(string(res.Stdout), "1.4.2") {
		t.Fatalf("bun image is not bun 1.4.2:\n%s", res)
	}

	stopService(t, serviceNpm)
	startService(t, serviceNpm)

	after := npmRegistry(t)
	t.Logf("verdaccio at %s before the restart, %s after", before, after)
	ping(t, after)
}

// TestHarness_HostTool runs developer tools with the host environment from
// the repository root and gets their streams and exit codes separately.
func TestHarness_HostTool(t *testing.T) {
	t.Parallel()

	gomod := hostTool(t, runOpts{}, "go", "env", "GOMOD").ok(t)
	if got := strings.TrimSpace(string(gomod.Stdout)); got != filepath.Join(suite.root, "go.mod") {
		t.Fatalf("go env GOMOD in the default directory = %q", got)
	}

	flags := hostTool(t, runOpts{Env: []string{"GOFLAGS=-count=1"}}, "go", "env", "GOFLAGS").ok(t)
	if got := strings.TrimSpace(string(flags.Stdout)); got != "-count=1" {
		t.Fatalf("runOpts.Env did not reach the tool: GOFLAGS = %q", got)
	}

	unknown := hostTool(t, runOpts{}, "go", "no-such-command").wantCode(t, 2)
	if len(unknown.Stdout) != 0 || !strings.Contains(string(unknown.Stderr), "unknown command") {
		t.Fatalf("streams of a failing tool:\n%s", unknown)
	}

	if res := hostTool(t, runOpts{}, ".tools/bin/jsonschema", "version").ok(t); len(bytes.TrimSpace(res.Stdout)) == 0 {
		t.Fatalf("a tool path relative to the repository root:\n%s", res)
	}
}

// TestHarness_DepsFixture prepares the dependency fixture against a local
// dependency server: only deps-root changes when the served document does.
func TestHarness_DepsFixture(t *testing.T) {
	dep := newDepServer(t)
	set := newDepsSet(t, dep.Host())

	first := prepareSet(t, set)

	entries := readPrepared(t, first).Entries

	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}

	if !slices.Equal(ids, []string{"deps-plain", "deps-root"}) {
		t.Fatalf("prepared IDs %v", ids)
	}

	root := preparedSchema(t, first, "deps-root")
	if !bytes.Contains(root, []byte("http://"+dep.Host()+"/dep.json")) || dep.Hits("/dep.json") == 0 {
		t.Fatalf("deps-root does not embed the served dependency (%d hits):\n%s", dep.Hits("/dep.json"), root)
	}

	dep.Set("/dep.json", readFile(t, fixture("deps", "served", "dep-v2.json")))
	second := prepareSet(t, set)

	if bytes.Equal(preparedSchema(t, second, "deps-root"), root) {
		t.Fatal("deps-root did not change with its dependency")
	}

	if !bytes.Equal(preparedSchema(t, second, "deps-plain"), preparedSchema(t, first, "deps-plain")) {
		t.Fatal("deps-plain changed although it has no dependency")
	}
}

// TestHarness_DriverReadiness checks that starting the stack depends on no
// tool inside the registry and verdaccio images: no service of any profile
// has a healthcheck (compose would run it inside the container, and up
// --wait would wait for it), none of the running containers has one from its
// image either, and readiness is only the harness's own HTTP probe.
func TestHarness_DriverReadiness(t *testing.T) {
	npmRegistry(t)

	out, err := suite.compose.run(t.Context(), "--profile", profileAuth, "--profile", profileNpm, "config", "--format", "json")
	if err != nil {
		t.Fatalf("compose config: %v\n%s", err, out)
	}

	cfg := decodeJSON[struct {
		Services map[string]struct {
			Healthcheck *struct {
				Test    []string `json:"test"`
				Disable bool     `json:"disable"`
			} `json:"healthcheck"`
		} `json:"services"`
	}](t, out)

	for _, name := range []string{serviceSource, serviceMirror, serviceAuth, serviceAuth2, serviceNpm} {
		svc, ok := cfg.Services[name]
		if !ok {
			t.Fatalf("compose.yaml has no service %s", name)
		}

		if hc := svc.Healthcheck; hc != nil && !hc.Disable {
			t.Errorf("service %s has a healthcheck that runs inside its image: %q", name, hc.Test)
		}
	}

	ids := dockerIDs(t.Context(), "ps", "--quiet", "--filter", "label=com.docker.compose.project="+suite.project)
	if len(ids) != 5 {
		t.Fatalf("want the 5 running services of the run, found containers %v", ids)
	}

	for _, id := range ids {
		res, err := docker(t.Context(), "inspect", "--format",
			`{{index .Config.Labels "com.docker.compose.service"}}|{{json .Config.Healthcheck}}|{{json .State.Health}}`, id)
		if err != nil {
			t.Fatalf("inspect %s: %v\n%s", id, err, res)
		}

		fields := strings.Split(trimOutput(res), "|")
		if len(fields) != 3 {
			t.Fatalf("inspect %s: %q", id, res)
		}

		if hc, health := fields[1], fields[2]; (hc != `{"Test":["NONE"]}` && hc != "null") || health != "null" {
			t.Errorf("container of %s has healthcheck %s and health state %s", fields[0], hc, health)
		}
	}
}

// TestHarness_PreparedImages checks the split between preparation and
// scenarios: every image of the run is in the local image store, a missing
// one is reported with the preparation command, dockerRun never pulls, and
// Go commands on the host cannot download modules.
func TestHarness_PreparedImages(t *testing.T) {
	t.Parallel()

	if got := strings.TrimSpace(string(hostTool(t, runOpts{}, "go", "env", "GOPROXY").ok(t).Stdout)); got != "off" {
		t.Errorf("host Go commands run with GOPROXY=%q, want off", got)
	}

	images, err := suiteImages(t.Context(), suite.dir, suite.jsonschema, suite.artifacts)
	if err != nil {
		t.Fatal(err)
	}

	refs := make([]string, 0, len(images))
	for _, img := range images {
		refs = append(refs, img.Ref)
	}

	for _, want := range []string{nodeImage, bunImage, pythonImage, rubyImage, validatorsImage(t)} {
		if !slices.Contains(refs, want) {
			t.Errorf("the prepared images %v lack %s", refs, want)
		}
	}

	if err := checkImages(t.Context(), images); err != nil {
		t.Fatal(err)
	}

	absent := "schepherd-e2e-absent:" + randomHex(8)

	err = checkImages(t.Context(), append(slices.Clone(images), suiteImage{Ref: absent, Purpose: "test"}))
	if err == nil || !strings.Contains(err.Error(), absent+" (test)") || !strings.Contains(err.Error(), "test:e2e:prepare") ||
		strings.Contains(err.Error(), nodeImage) {
		t.Fatalf("a missing image must be named together with the preparation step, and only the missing one: %v", err)
	}

	res := dockerRun(t, absent, dockerOpts{}, "true").wantCode(t, 125)
	if stderr := string(res.Stderr); !strings.Contains(stderr, "No such image") || strings.Contains(stderr, "Unable to find image") {
		t.Fatalf("dockerRun must fail on a missing image without trying to pull it:\n%s", res)
	}

	if imagePresent(t.Context(), absent) {
		t.Fatalf("%s appeared in the image store", absent)
	}
}
