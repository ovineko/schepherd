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

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/tests/e2e/regproxy"
)

// TestE16_StrictOfflineWarm warms a cache online through a private proxy in
// front of the TLS registry with htpasswd (credentials from a credsStore
// helper), then proves that --offline path, cat and list need nothing else:
// inside the validators container with no network at all, and on the host
// with every registry stopped and the proxy down. The helper never runs and
// the proxy records no request.
func TestE16_StrictOfflineWarm(t *testing.T) {
	reg := suite.auth
	snap := offlinePublish(t, reg)
	px := newProxy(t, reg)
	sb := sandboxOf(t)

	helperDir := t.TempDir()
	helperLog := filepath.Join(helperDir, "host.log")
	containerLog := filepath.Join(helperDir, "container.log")
	user := reg.User()

	writeJSONFile(t, helperDir, "db.json", map[string]map[string]string{px.Host(): {"Username": user.Username, "Secret": user.Password}})
	writeCredsStoreConfig(t, sb.DockerConfig, "e2e")

	cfg := writeConfig(t, sb.Workspace, clientConfig{Repository: px.Repo(snap.path), Catalog: snap.pub.CatalogDigest}.TOML())
	hostEnv := []string{"E2E_CREDHELPER_DB=" + filepath.Join(helperDir, "db.json"), "E2E_CREDHELPER_LOG=" + helperLog}
	onHostWith := func(cfg string) func(t *testing.T, args ...string) result {
		return func(t *testing.T, args ...string) result {
			t.Helper()

			return cli(t, runOpts{Sandbox: sb, Env: hostEnv}, append([]string{"--config", cfg}, args...)...)
		}
	}
	onHost := onHostWith(cfg)

	// The offline runs also get a configuration that names no endpoint but
	// the proxy, which records every request it receives.
	confDir := t.TempDir()
	registries := map[string]*registrySettings{}

	for host := range defaultRegistrySettings() {
		registries[host] = nil
	}

	registries[px.Host()] = &registrySettings{PlainHTTP: true}
	proxyOnly := writeConfig(t, confDir, clientConfig{Repository: px.Repo(snap.path), Catalog: snap.pub.CatalogDigest, Registries: registries}.TOML())

	warm := map[string]string{}

	for _, id := range snap.ids() {
		p := offlineSchemaPath(t, onHost(t, "path", id).ok(t))
		if !bytes.Equal(readFile(t, p), snap.prepared(t, id)) {
			t.Fatalf("%s: materialized %s differs from the prepared schema", id, p)
		}

		warm[id] = p
	}

	list := onHost(t, "list", "--json").ok(t).Stdout

	// Without this the empty helper log below would prove nothing.
	offlineHelperAsked(t, helperLog, px.Host())

	px.Reset(t)
	px.SetDown(t, true)

	services := []string{serviceAuth, serviceAuth2, serviceMirror, serviceSource}
	for _, s := range services {
		stopService(t, s)
	}

	check := func(t *testing.T, run func(t *testing.T, args ...string) result, cacheRoot string) {
		t.Helper()

		for _, id := range snap.ids() {
			p := offlineSchemaPath(t, run(t, "--offline", "path", id).ok(t))
			if rel, want := strings.TrimPrefix(p, cacheRoot), strings.TrimPrefix(warm[id], sb.CacheDir); !strings.HasPrefix(p, cacheRoot+"/") || rel != want {
				t.Errorf("%s: offline path %s, want %s below %s", id, p, want, cacheRoot)
			}

			if cat := run(t, "--offline", "cat", id).ok(t); !bytes.Equal(cat.Stdout, snap.prepared(t, id)) {
				t.Errorf("%s: offline cat differs from the prepared schema:\n%s", id, cat)
			}
		}

		if res := run(t, "--offline", "list", "--json").ok(t); !bytes.Equal(res.Stdout, list) {
			t.Errorf("offline list --json differs from the online one\n--- offline ---\n%s\n--- online ---\n%s", res.Stdout, list)
		}
	}

	t.Run("validators container without network", func(t *testing.T) {
		opts := dockerOpts{
			Network: "none",
			Mounts: []mount{
				binMount(),
				{Host: sb.CacheDir, Container: "/cache", Writable: true},
				{Host: confDir, Container: "/config"},
				{Host: sb.DockerConfig, Container: "/docker"},
				{Host: helperDir, Container: "/helper", Writable: true},
			},
			Env: []string{
				"PATH=/e2e/bin:/usr/local/bin:/usr/bin:/bin",
				"DOCKER_CONFIG=/docker",
				"E2E_CREDHELPER_DB=/helper/db.json",
				"E2E_CREDHELPER_LOG=/helper/" + filepath.Base(containerLog),
			},
		}

		// Without this the empty container log below would prove nothing:
		// the helper resolves on this PATH, reads its database and logs.
		probe := opts
		probe.Stdin = []byte(px.Host() + "\n")

		if res := dockerRun(t, validatorsImage(t), probe, "docker-credential-e2e", "get"); res.Code != 0 || !bytes.Contains(res.Stdout, []byte(user.Username)) {
			t.Fatalf("docker-credential-e2e does not answer inside the container: exit code %d (%v)\n%s", res.Code, res.Err, res.Stderr)
		}

		offlineHelperAsked(t, containerLog, px.Host())

		check(t, func(t *testing.T, args ...string) result {
			t.Helper()

			full := append([]string{"/e2e/bin/schepherd", "--config", "/config/" + filepath.Base(proxyOnly), "--cache-dir", "/cache"}, args...)

			return dockerRun(t, validatorsImage(t), opts, full...)
		}, "/cache")
	})

	t.Run("host with the warm-up configuration", func(t *testing.T) {
		check(t, onHost, sb.CacheDir)
	})

	t.Run("host with only the proxy configured", func(t *testing.T) {
		check(t, onHostWith(proxyOnly), sb.CacheDir)
	})

	if stats := px.Stats(t); stats.Total != 0 {
		t.Errorf("offline commands reached the proxy: %+v\n%+v", stats, px.Records(t))
	}

	offlineEmptyLog(t, helperLog)
	offlineEmptyLog(t, containerLog)

	for _, s := range services {
		startService(t, s)
	}
}

// TestE17_OfflineMiss warms the catalog (and alpha) but not beta: an offline
// request for beta fails with exit code 6 without any registry request,
// names beta's manifest digest and prints nothing on stdout.
func TestE17_OfflineMiss(t *testing.T) {
	t.Parallel()

	snap := offlinePublish(t, suite.source)
	c := offlineClientFor(t, snap)
	beta := snap.manifestDigest(t, "beta")

	alpha := offlineSchemaPath(t, c.run(t, "path", "alpha").ok(t))

	offlineAbsent(t, offlineMaterializedFile(c.sb.CacheDir, beta))
	offlineAbsent(t, offlineBlobFile(c.sb.CacheDir, beta))
	c.px.Reset(t)

	if got := offlineSchemaPath(t, c.run(t, "--offline", "path", "alpha").ok(t)); got != alpha {
		t.Fatalf("offline path alpha = %s, online %s", got, alpha)
	}

	for _, cmd := range []string{"path", "cat"} {
		res := c.run(t, "--offline", cmd, "beta").wantCode(t, 6)

		if len(res.Stdout) != 0 {
			t.Errorf("%s beta: stdout is not empty:\n%s", cmd, res)
		}

		if !strings.Contains(string(res.Stderr), beta) {
			t.Errorf("%s beta: stderr does not name the manifest digest %s:\n%s", cmd, beta, res)
		}

		t.Logf("offline %s beta: exit code 6: %s", cmd, bytes.TrimSpace(res.Stderr))
	}

	if stats := c.px.Stats(t); stats.Total != 0 {
		t.Errorf("offline commands reached the proxy: %+v\n%+v", stats, c.px.Records(t))
	}

	offlineAbsent(t, offlineMaterializedFile(c.sb.CacheDir, beta))
	offlineAbsent(t, offlineBlobFile(c.sb.CacheDir, beta))
}

// TestE18_CacheCorruption damages a materialized schema.json and,
// separately, a cached payload blob: an online path repairs a damaged
// schema.json, an offline path on a damaged copy fails with exit code 6. A
// damaged payload blob next to an intact schema.json is not needed, so it
// neither fails offline commands nor causes a request; with schema.json gone
// path has to rebuild from it, which fails offline and repairs it online.
// alpha is stored uncompressed and gamma gzip-packed.
func TestE18_CacheCorruption(t *testing.T) {
	t.Parallel()

	snap := offlinePublish(t, suite.source)

	for _, id := range []string{"alpha", "gamma"} {
		t.Run("materialized "+id, func(t *testing.T) {
			t.Parallel()
			offlineMaterializedCorruption(t, snap, id)
		})

		t.Run("payload blob "+id, func(t *testing.T) {
			t.Parallel()
			offlinePayloadCorruption(t, snap, id)
		})

		t.Run("payload blob without schema.json "+id, func(t *testing.T) {
			t.Parallel()
			offlinePayloadRebuild(t, snap, id)
		})
	}
}

// TestE19_CorruptTransport corrupts and then truncates the payload blob on
// the wire: the client fails without leaving the blob or schema.json in the
// cache, and succeeds once the faults are gone.
func TestE19_CorruptTransport(t *testing.T) {
	t.Parallel()

	snap := offlinePublish(t, suite.source)

	for _, id := range []string{"alpha", "gamma"} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()

			c := offlineClientFor(t, snap)
			payload := snap.payload(t, id).Digest.String()
			blob := offlineBlobFile(c.sb.CacheDir, payload)
			materialized := offlineMaterializedFile(c.sb.CacheDir, snap.manifestDigest(t, id))

			faulty := func(t *testing.T, name string, action regproxy.Action) result {
				t.Helper()

				c.px.ClearFaults()
				c.px.AddFault(t, regproxy.Fault{Name: name, Class: regproxy.ClassBlobGet, PathContains: payload, Action: action})
				c.px.Reset(t)

				res := c.run(t, "path", id)

				if !slices.ContainsFunc(c.px.Records(t), func(r regproxy.Record) bool { return r.Fault == name }) {
					t.Fatalf("%s: the fault never applied to the payload %s:\n%s\n%+v", name, payload, res, c.px.Records(t))
				}

				if len(res.Stdout) != 0 {
					t.Errorf("%s: stdout is not empty:\n%s", name, res)
				}

				offlineAbsent(t, blob)
				offlineAbsent(t, materialized)
				offlineNoEntryNamed(t, c.sb, strings.TrimPrefix(payload, "sha256:"))

				if left := fileTree(t, filepath.Join(c.sb.CacheDir, "v1", "tmp")); len(left) != 0 {
					t.Errorf("%s: v1/tmp keeps %v", name, left)
				}

				return res
			}

			corrupted := faulty(t, "corrupt-payload", regproxy.ActionCorruptBody).wantCode(t, 5)
			t.Logf("corrupted payload: exit code 5: %s", bytes.TrimSpace(corrupted.Stderr))

			if res := faulty(t, "truncate-payload", regproxy.ActionTruncateBody); res.Code != 4 && res.Code != 5 {
				t.Errorf("truncated payload: want exit code 4 or 5, got:\n%s", res)
			} else {
				t.Logf("truncated payload: exit code %d after %v: %s", res.Code, res.Duration, bytes.TrimSpace(res.Stderr))
			}

			c.px.ClearFaults()

			if got := offlineSchemaPath(t, c.run(t, "path", id).ok(t)); got != materialized {
				t.Fatalf("path %s, want %s", got, materialized)
			}

			if !bytes.Equal(readFile(t, materialized), snap.prepared(t, id)) {
				t.Fatalf("materialized %s differs from the prepared schema", materialized)
			}

			offlineVerifies(t, blob, payload)
		})
	}
}

func offlineMaterializedCorruption(t *testing.T, snap offlineSnapshot, id string) {
	t.Helper()

	c := offlineClientFor(t, snap)
	want := snap.prepared(t, id)

	p := offlineSchemaPath(t, c.run(t, "path", id).ok(t))
	if !bytes.Equal(readFile(t, p), want) {
		t.Fatalf("materialized %s differs from the prepared schema", p)
	}

	offlineCorrupt(t, p)

	if got := offlineSchemaPath(t, c.run(t, "path", id).ok(t)); got != p {
		t.Fatalf("path after the repair %s, want %s", got, p)
	}

	if !bytes.Equal(readFile(t, p), want) {
		t.Fatalf("online path did not repair %s", p)
	}

	if info, err := os.Stat(p); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm()&0o222 != 0 {
		t.Errorf("repaired %s is not read-only: %v", p, info.Mode())
	}

	if res := c.run(t, "cat", id).ok(t); !bytes.Equal(res.Stdout, want) {
		t.Fatalf("cat after the repair differs from the prepared schema:\n%s", res)
	}

	offlineWarmNotice(t, c.run, id)
	c.px.Reset(t)
	offlineServes(t, c.run, p, id, want)

	offlineCorrupt(t, p)
	offlineRefuses(t, c.run, id, "a corrupt schema.json")

	if stats := c.px.Stats(t); stats.Total != 0 {
		t.Errorf("offline commands reached the proxy: %+v", stats)
	}
}

// offlinePayloadCorruption damages the cached payload blob and leaves the
// materialized schema.json intact: schema.json no longer needs the blob, so
// path, cat and export keep working offline and online without a request,
// and the damaged blob is left as it is.
func offlinePayloadCorruption(t *testing.T, snap offlineSnapshot, id string) {
	t.Helper()

	c := offlineClientFor(t, snap)
	want := snap.prepared(t, id)
	payload := snap.payload(t, id).Digest.String()
	blob := offlineBlobFile(c.sb.CacheDir, payload)

	p := offlineSchemaPath(t, c.run(t, "path", id).ok(t))
	offlineVerifies(t, blob, payload)
	offlineWarmNotice(t, c.run, id)

	offlineCorrupt(t, blob)
	damaged := readFile(t, blob)
	c.px.Reset(t)

	offlineServes(t, c.run, p, id, want)

	if got := offlineSchemaPath(t, c.run(t, "path", id).ok(t)); got != p {
		t.Fatalf("online path next to a corrupt payload blob %s, want %s", got, p)
	}

	if res := c.run(t, "cat", id).ok(t); !bytes.Equal(res.Stdout, want) {
		t.Fatalf("online cat next to a corrupt payload blob differs from the prepared schema:\n%s", res)
	}

	if stats := c.px.Stats(t); stats.Total != 0 {
		t.Errorf("commands on a verified schema.json reached the proxy: %+v\n%+v", stats, c.px.Records(t))
	}

	if !bytes.Equal(readFile(t, p), want) {
		t.Fatalf("%s changed", p)
	}

	if !bytes.Equal(readFile(t, blob), damaged) {
		t.Errorf("the unused payload blob %s was rewritten", blob)
	}
}

// offlinePayloadRebuild damages the cached payload blob and removes the
// materialized schema.json, so path has to rebuild it from the blob.
func offlinePayloadRebuild(t *testing.T, snap offlineSnapshot, id string) {
	t.Helper()

	c := offlineClientFor(t, snap)
	want := snap.prepared(t, id)
	payload := snap.payload(t, id).Digest.String()
	blob := offlineBlobFile(c.sb.CacheDir, payload)

	p := offlineSchemaPath(t, c.run(t, "path", id).ok(t))
	offlineVerifies(t, blob, payload)

	// With the payload intact an offline rebuild succeeds, so the exit code 6
	// below is caused by the corruption, not by the missing schema.json.
	offlineRemoveMaterialized(t, p)
	c.px.Reset(t)

	if got := offlineSchemaPath(t, c.run(t, "--offline", "path", id).ok(t)); got != p || !bytes.Equal(readFile(t, p), want) {
		t.Fatalf("offline rebuild from the verified payload: path %s (want %s) or content differs", got, p)
	}

	if stats := c.px.Stats(t); stats.Total != 0 {
		t.Errorf("the offline rebuild reached the proxy: %+v", stats)
	}

	offlineCorrupt(t, blob)
	offlineRemoveMaterialized(t, p)

	if got := offlineSchemaPath(t, c.run(t, "path", id).ok(t)); got != p {
		t.Fatalf("path after the repair %s, want %s", got, p)
	}

	if !bytes.Equal(readFile(t, p), want) {
		t.Fatalf("online path rebuilt %s with wrong content", p)
	}

	offlineVerifies(t, blob, payload)

	offlineCorrupt(t, blob)
	offlineRemoveMaterialized(t, p)
	c.px.Reset(t)

	res := c.run(t, "--offline", "path", id)
	if res.Code != 6 || len(res.Stdout) != 0 {
		t.Errorf("offline path on a corrupt payload blob without schema.json: want exit code 6 and empty stdout, got:\n%s", res)
	}

	offlineAbsent(t, p)

	if stats := c.px.Stats(t); stats.Total != 0 {
		t.Errorf("offline commands reached the proxy: %+v", stats)
	}
}

// offlineServes is the control of offlineRefuses: on the same cache, before
// it is damaged, --offline path, cat and export succeed, so an exit code 6
// afterwards comes from the damage and not from a cache miss.
func offlineServes(t *testing.T, run func(t *testing.T, args ...string) result, p, id string, want []byte) {
	t.Helper()

	if got := offlineSchemaPath(t, run(t, "--offline", "path", id).ok(t)); got != p {
		t.Fatalf("offline path on the intact cache %s, want %s", got, p)
	}

	if res := run(t, "--offline", "cat", id).ok(t); !bytes.Equal(res.Stdout, want) {
		t.Fatalf("offline cat on the intact cache differs from the prepared schema:\n%s", res)
	}

	dest := filepath.Join(t.TempDir(), "exported.json")
	run(t, "--offline", "export", id, dest).ok(t)

	if !bytes.Equal(readFile(t, dest), want) {
		t.Fatalf("offline export on the intact cache differs from the prepared schema")
	}

	if len(readFile(t, dest+".NOTICE")) == 0 {
		t.Fatalf("offline export on the intact cache wrote an empty notice")
	}
}

// offlineWarmNotice exports id once online: the notice layer is fetched only
// by export, so an offline export needs it in the cache.
func offlineWarmNotice(t *testing.T, run func(t *testing.T, args ...string) result, id string) {
	t.Helper()

	run(t, "export", id, filepath.Join(t.TempDir(), "warm.json")).ok(t)
}

// offlineRefuses requires --offline path, cat and export to fail with exit
// code 6, print nothing on stdout and create no export file.
func offlineRefuses(t *testing.T, run func(t *testing.T, args ...string) result, id, damage string) {
	t.Helper()

	dest := filepath.Join(t.TempDir(), "exported.json")

	for _, args := range [][]string{{"path", id}, {"cat", id}, {"export", id, dest}} {
		res := run(t, append([]string{"--offline"}, args...)...)
		if res.Code != 6 || len(res.Stdout) != 0 {
			t.Errorf("offline %s on %s: want exit code 6 and empty stdout, got:\n%s", args[0], damage, res)
		} else {
			t.Logf("offline %s on %s: exit code 6: %s", args[0], damage, bytes.TrimSpace(res.Stderr))
		}
	}

	offlineAbsent(t, dest)
	offlineAbsent(t, dest+".NOTICE")
}

// offlineSnapshot is set-basic published into the calling test's repository,
// with the catalog document read back directly from the registry.
type offlineSnapshot struct {
	reg     *registry
	path    string
	pub     publishResult
	catalog catalogDoc
}

func offlinePublish(t *testing.T, reg *registry) offlineSnapshot {
	t.Helper()

	path := repoPath(t)
	pub := publishSet(t, reg.Repo(path), newSet(t, "set-basic"), "--now", "20260101.0000")

	doc := reg.Catalog(t, path, pub.CatalogDigest).Doc
	snap := offlineSnapshot{reg: reg, path: path, pub: pub, catalog: doc}

	entries := readPrepared(t, pub.Prepared).Entries
	want := make([]string, 0, len(entries))

	for _, e := range entries {
		want = append(want, e.ID)
	}

	got := snap.ids()
	slices.Sort(got)
	slices.Sort(want)

	if len(want) == 0 || !slices.Equal(got, want) {
		t.Fatalf("catalog lists schemas %v, want the prepared set %v", got, want)
	}

	for _, id := range got {
		snap.manifestDigest(t, id)
	}

	return snap
}

func (s offlineSnapshot) ids() []string {
	ids := make([]string, 0, len(s.catalog.Schemas))
	for _, e := range s.catalog.Schemas {
		ids = append(ids, e.ID)
	}

	return ids
}

func (s offlineSnapshot) manifestDigest(t *testing.T, id string) string {
	t.Helper()

	d := s.catalog.entry(t, id).Artifact.Digest
	offlineSHA256(t, id+" manifest", d)

	return d
}

// payload is the payload layer descriptor of a schema, read directly from
// the registry.
func (s offlineSnapshot) payload(t *testing.T, id string) ocispec.Descriptor {
	t.Helper()

	m := s.reg.Manifest(t, s.path, s.manifestDigest(t, id))
	if len(m.Manifest.Layers) == 0 {
		t.Fatalf("%s: schema manifest has no layers:\n%s", id, m.Body)
	}

	offlineSHA256(t, id+" payload", m.Manifest.Layers[0].Digest.String())

	return m.Manifest.Layers[0]
}

func (s offlineSnapshot) prepared(t *testing.T, id string) []byte {
	t.Helper()

	return preparedSchema(t, s.pub.Prepared, id)
}

// offlineClient is a fresh sandbox (and so a cold cache) reading a snapshot
// through a private proxy.
type offlineClient struct {
	sb  *sandbox
	px  *proxy
	cfg string
}

func offlineClientFor(t *testing.T, snap offlineSnapshot) *offlineClient {
	t.Helper()

	sb := newSandbox(t)
	px := newProxy(t, snap.reg)
	cfg := writeConfig(t, sb.Workspace, clientConfig{Repository: px.Repo(snap.path), Catalog: snap.pub.CatalogDigest}.TOML())

	return &offlineClient{sb: sb, px: px, cfg: cfg}
}

func (c *offlineClient) run(t *testing.T, args ...string) result {
	t.Helper()

	return cli(t, runOpts{Sandbox: c.sb}, append([]string{"--config", c.cfg}, args...)...)
}

// offlineSchemaPath returns the path a successful path command printed: one
// absolute path and a newline, nothing else.
func offlineSchemaPath(t *testing.T, res result) string {
	t.Helper()

	out := string(res.Stdout)
	p := strings.TrimSuffix(out, "\n")

	if p == out || strings.ContainsAny(p, "\n\x00") || !filepath.IsAbs(p) || filepath.Base(p) != "schema.json" {
		t.Fatalf("stdout is not one absolute schema.json path and a newline:\n%s", res)
	}

	return p
}

func offlineBlobFile(cacheRoot, dgst string) string {
	return filepath.Join(cacheRoot, "v1", "blobs", "sha256", strings.TrimPrefix(dgst, "sha256:"))
}

func offlineMaterializedFile(cacheRoot, manifestDigest string) string {
	return filepath.Join(cacheRoot, "v1", "materialized", "sha256", strings.TrimPrefix(manifestDigest, "sha256:"), "schema.json")
}

// offlineCorrupt inverts the middle byte of a cache file in place, keeping
// its size, so only a digest check can notice.
func offlineCorrupt(t *testing.T, path string) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(path, info.Mode().Perm()|0o200); err != nil {
		t.Fatal(err)
	}

	data := readFile(t, path)
	if len(data) == 0 {
		t.Fatalf("%s is empty", path)
	}

	data[len(data)/2] ^= 0xff

	if err := os.WriteFile(path, data, info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
}

// offlineRemoveMaterialized deletes the directory of a materialized
// schema.json, as a user or the OS may do with any part of the cache.
func offlineRemoveMaterialized(t *testing.T, schemaFile string) {
	t.Helper()

	dir := filepath.Dir(schemaFile)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
}

func offlineAbsent(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s exists (or cannot be checked: %v)", path, err)
	}
}

// offlineNoEntryNamed fails if a blob, materialized or quarantined entry of
// the cache is named after hex.
func offlineNoEntryNamed(t *testing.T, sb *sandbox, hex string) {
	t.Helper()

	for _, p := range fileTree(t, sb.CacheDir) {
		if (strings.HasPrefix(p, "v1/blobs/") || strings.HasPrefix(p, "v1/materialized/") || strings.HasPrefix(p, "v1/quarantine/")) && strings.Contains(p, hex) {
			t.Errorf("cache entry %s is left for %s", p, hex)
		}
	}
}

// offlineSHA256 fails unless d is a well-formed sha256 digest: the
// assertions built from a digest (a substring of stderr, a cache file name)
// would hold trivially for an empty one.
func offlineSHA256(t *testing.T, what, d string) {
	t.Helper()

	if dg := digest.Digest(d); dg.Algorithm() != digest.SHA256 || dg.Validate() != nil {
		t.Fatalf("%s digest %q is not a sha256 digest", what, d)
	}
}

func offlineVerifies(t *testing.T, path, dgst string) {
	t.Helper()

	if got := digestOf(readFile(t, path)); got != dgst {
		t.Fatalf("%s has digest %s, want %s", path, got, dgst)
	}
}

// offlineHelperAsked fails unless docker-credential-e2e logged a get for host
// in logFile, then removes the log so it can show that nothing ran later.
func offlineHelperAsked(t *testing.T, logFile, host string) {
	t.Helper()

	log := string(readFile(t, logFile))
	if !strings.Contains(log, `"op":"get"`) || !strings.Contains(log, host) {
		t.Fatalf("docker-credential-e2e never logged a get for %s in %s:\n%s", host, logFile, log)
	}

	if err := os.Remove(logFile); err != nil {
		t.Fatal(err)
	}
}

func offlineEmptyLog(t *testing.T, path string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}

	if err != nil || len(data) != 0 {
		t.Errorf("docker-credential-e2e ran in offline mode (%s, %v):\n%s", path, err, data)
	}
}
