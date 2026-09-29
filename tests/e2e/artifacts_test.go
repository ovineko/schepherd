//go:build e2e

package e2e

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/tests/e2e/regproxy"
)

// artifactsFixtureIDs are the schema IDs of fixtures/set-basic, sorted.
var artifactsFixtureIDs = []string{"alpha", "beta", "delta", "dup1", "dup2", "gamma"}

// artifactsSet is set-basic published into the calling test's repository,
// with the catalog and every schema manifest read back directly from the
// registry.
type artifactsSet struct {
	reg          *registry
	path         string
	pub          publishResult
	catalog      catalogDoc
	metadata     string
	catalogLayer ocispec.Descriptor
	manifests    map[string]fetchedManifest
}

func artifactsPublish(t *testing.T, reg *registry) *artifactsSet {
	t.Helper()

	p := repoPath(t)
	pub := publishSet(t, reg.Repo(p), newSet(t, "set-basic"), "--now", "20260101.0000")

	entries := readPrepared(t, pub.Prepared).Entries

	preparedIDs := make([]string, 0, len(entries))
	for _, e := range entries {
		preparedIDs = append(preparedIDs, e.ID)
	}

	slices.Sort(preparedIDs)

	if !slices.Equal(preparedIDs, artifactsFixtureIDs) {
		t.Fatalf("fixture changed: prepared schemas %v, want %v", preparedIDs, artifactsFixtureIDs)
	}

	c := reg.Catalog(t, p, pub.CatalogDigest)

	s := &artifactsSet{
		reg:          reg,
		path:         p,
		pub:          pub,
		catalog:      c.Doc,
		metadata:     c.Metadata.Digest,
		catalogLayer: c.Metadata.Manifest.Layers[0],
		manifests:    map[string]fetchedManifest{},
	}

	for _, e := range s.catalog.Schemas {
		if _, dup := s.manifests[e.ID]; dup {
			t.Fatalf("the catalog lists schema %s twice", e.ID)
		}

		sm := reg.Manifest(t, p, e.Artifact.Digest)
		if len(sm.Manifest.Layers) == 0 {
			t.Fatalf("schema %s has no layers:\n%s", e.ID, sm.Body)
		}

		s.manifests[e.ID] = sm
	}

	if got := slices.Sorted(maps.Keys(s.manifests)); !slices.Equal(got, artifactsFixtureIDs) {
		t.Fatalf("the catalog lists the schemas %v, want the prepared %v", got, artifactsFixtureIDs)
	}

	return s
}

// payload is the payload layer (the first layer) of a schema artifact.
func (s *artifactsSet) payload(id string) ocispec.Descriptor {
	return s.manifests[id].Manifest.Layers[0]
}

func (s *artifactsSet) prepared(t *testing.T, id string) []byte {
	t.Helper()

	return preparedSchema(t, s.pub.Prepared, id)
}

// config writes a client configuration that pins the set's catalog in repo
// (the registry's direct address or one of its proxies) into sb.
func (s *artifactsSet) config(t *testing.T, sb *sandbox, repo string) string {
	t.Helper()

	return writeConfig(t, sb.Workspace, clientConfig{Repository: repo, Catalog: s.pub.CatalogDigest}.TOML())
}

// labels names every digest of the snapshot: the catalog index, metadata
// manifest and payload, and the manifest, payload and notice layer of every
// schema.
func (s *artifactsSet) labels() map[string]string {
	out := map[string]string{
		s.pub.CatalogDigest:            "catalog index",
		s.metadata:                     "catalog metadata manifest",
		s.catalogLayer.Digest.String(): "catalog payload",
	}

	for id, m := range s.manifests {
		out[m.Digest] = id + " manifest"

		for i, l := range m.Manifest.Layers {
			if i == 0 {
				out[l.Digest.String()] = id + " payload"
			} else {
				out[l.Digest.String()] = "notice layer"
			}
		}
	}

	return out
}

// schemaDigests lists the manifest and layer digests of every schema except
// the given IDs; layers shared with an excepted schema (the notice) stay in.
func (s *artifactsSet) schemaDigests(except ...string) []string {
	var out []string

	for id, m := range s.manifests {
		if slices.Contains(except, id) {
			for _, l := range m.Manifest.Layers[1:] {
				out = append(out, l.Digest.String())
			}

			continue
		}

		out = append(out, m.Digest)
		for _, l := range m.Manifest.Layers {
			out = append(out, l.Digest.String())
		}
	}

	slices.Sort(out)

	return slices.Compact(out)
}

// artifactsRequest is one proxied request with the repository and the
// reference (tag or digest) it addressed.
type artifactsRequest struct {
	regproxy.Record

	Repo string
	Ref  string
}

func (r artifactsRequest) String() string {
	return r.Method + " " + r.Path + " -> " + strconv.Itoa(r.Status) + " (" + r.Class + ")"
}

func artifactsRequests(recs []regproxy.Record) []artifactsRequest {
	out := make([]artifactsRequest, 0, len(recs))

	for _, rec := range recs {
		req := artifactsRequest{Record: rec}

		if rest, ok := strings.CutPrefix(rec.Path, "/v2/"); ok {
			for _, sep := range []string{"/manifests/", "/blobs/"} {
				if i := strings.LastIndex(rest, sep); i >= 0 {
					req.Repo, req.Ref = rest[:i], rest[i+len(sep):]

					break
				}
			}
		}

		out = append(out, req)
	}

	return out
}

// artifactsOnly fails the test unless every request is a ping or a read
// (GET or HEAD) by digest of one of the allowed digests in repository repo.
// Tag lists, reads by tag and writes are all unexpected.
func artifactsOnly(t *testing.T, reqs []artifactsRequest, repo string, allowed []string, labels map[string]string) {
	t.Helper()

	reads := []string{regproxy.ClassManifestGet, regproxy.ClassManifestHead, regproxy.ClassBlobGet, regproxy.ClassBlobHead}

	for _, r := range reqs {
		switch {
		case r.Class == regproxy.ClassPing:
		case !slices.Contains(reads, r.Class):
			t.Errorf("unexpected request: %s", r)
		case r.Repo != repo:
			t.Errorf("request outside repository %s: %s", repo, r)
		case !slices.Contains(allowed, r.Ref):
			label := labels[r.Ref]
			if label == "" {
				label = "an unknown reference"
			}

			t.Errorf("request for %s: %s", label, r)
		}
	}
}

// artifactsCount counts the requests of one class for one of refs.
func artifactsCount(reqs []artifactsRequest, class string, refs ...string) int {
	n := 0

	for _, r := range reqs {
		if r.Class == class && slices.Contains(refs, r.Ref) {
			n++
		}
	}

	return n
}

// artifactsBlobBytes sums the body bytes of successful blob-get responses
// for one digest, as they crossed the network.
func artifactsBlobBytes(reqs []artifactsRequest, dgst string) int64 {
	var n int64

	for _, r := range reqs {
		if r.Class == regproxy.ClassBlobGet && r.Ref == dgst && r.Status >= 200 && r.Status < 300 {
			n += r.RespBytes
		}
	}

	return n
}

func artifactsDump(reqs []artifactsRequest) string {
	lines := make([]string, 0, len(reqs))
	for _, r := range reqs {
		lines = append(lines, "  "+r.String())
	}

	return strings.Join(lines, "\n")
}

// artifactsCacheFiles lists the non-directory entries below a cache root
// (see fileTree for the format).
func artifactsCacheFiles(t *testing.T, root string) []string {
	t.Helper()

	var out []string

	for _, e := range fileTree(t, root) {
		if !strings.HasSuffix(e, "/") {
			out = append(out, e)
		}
	}

	return out
}

// artifactsSchemaFiles returns the cache entries named schema.json.
func artifactsSchemaFiles(files []string) []string {
	var out []string

	for _, f := range files {
		if path.Base(f) == "schema.json" {
			out = append(out, f)
		}
	}

	return out
}

// artifactsMentions returns the cache entries whose path contains the hex
// part of one of the digests.
func artifactsMentions(files, digests []string) []string {
	var out []string

	for _, f := range files {
		for _, d := range digests {
			if strings.Contains(f, strings.TrimPrefix(d, "sha256:")) {
				out = append(out, f)

				break
			}
		}
	}

	return out
}

// artifactsPathOf checks the output contract of "path": an absolute path
// of a schema.json below the cache root, one newline and nothing else. It
// returns the path.
func artifactsPathOf(t *testing.T, res result, cacheDir string) string {
	t.Helper()

	p, ok := strings.CutSuffix(string(res.Stdout), "\n")
	if !ok || p == "" || strings.ContainsAny(p, "\n\x00") {
		t.Fatalf("path output is not one line:\n%s", res)
	}

	if !filepath.IsAbs(p) || filepath.Base(p) != "schema.json" || !strings.HasPrefix(p, cacheDir+string(filepath.Separator)) {
		t.Fatalf("path %q is not an absolute schema.json below the cache %s", p, cacheDir)
	}

	return p
}

// artifactsDecodeStrict decodes exactly one JSON value that has no members
// beyond the fields of T.
func artifactsDecodeStrict[T any](t *testing.T, data []byte) T {
	t.Helper()

	var v T

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %T: %v\n%s", v, err, clip(data))
	}

	if _, err := dec.Token(); err != io.EOF {
		t.Fatalf("output has more than one JSON value:\n%s", clip(data))
	}

	return v
}

// artifactsSnapshot describes every entry below dir (content, inode, mode
// and modification time of files; targets of symlinks) so a test can assert
// that nothing changed.
func artifactsSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()

	out := map[string]string{}

	for _, e := range fileTree(t, dir) {
		name, _, _ := strings.Cut(e, " -> ")

		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(strings.TrimSuffix(name, "/"))))
		if err != nil {
			t.Fatalf("stat %s: %v", e, err)
		}

		desc := info.Mode().String() + " " + info.ModTime().String()
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			desc += " inode " + strconv.FormatUint(st.Ino, 10)
		}

		if info.Mode().IsRegular() {
			desc += " " + digestOf(readFile(t, filepath.Join(dir, filepath.FromSlash(name))))
		}

		out[e] = desc
	}

	return out
}

// artifactsWantFile asserts that p is a regular file (not a symlink) with
// exactly want as content.
func artifactsWantFile(t *testing.T, p string, want []byte) {
	t.Helper()

	info, err := os.Lstat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}

	if !info.Mode().IsRegular() {
		t.Fatalf("%s is %v, want a regular file", p, info.Mode())
	}

	if got := readFile(t, p); !bytes.Equal(got, want) {
		t.Fatalf("%s has %d bytes (%s), want %d bytes (%s)", p, len(got), digestOf(got), len(want), digestOf(want))
	}
}

// TestE02_CatalogOnly checks that catalog-level commands (list, patterns,
// resolve) fetch the catalog and nothing else: with a cold cache the proxy
// sees no request for a schema manifest or payload and the cache holds no
// materialized schema afterwards (docs/cli.md, "These commands fetch the
// catalog only").
func TestE02_CatalogOnly(t *testing.T) {
	set := artifactsPublish(t, suite.source)
	px := newProxy(t, suite.source)
	labels := set.labels()
	allowed := []string{set.pub.CatalogDigest, set.metadata, set.catalogLayer.Digest.String()}

	schemaManifests := make([]string, 0, len(set.manifests))
	schemaLayers := make([]string, 0, 2*len(set.manifests))

	for _, m := range set.manifests {
		schemaManifests = append(schemaManifests, m.Digest)

		for _, l := range m.Manifest.Layers {
			schemaLayers = append(schemaLayers, l.Digest.String())
		}
	}

	commands := []struct {
		name  string
		args  []string
		check func(t *testing.T, sb *sandbox, stdout []byte)
	}{
		{"list", []string{"list", "--json"}, set.checkList},
		{"patterns", []string{"patterns", "--json"}, set.checkPatterns},
		{"resolve", []string{"resolve", "--file", ".github/workflows/ci.yml", "--json"}, set.checkResolve},
	}

	for _, c := range commands {
		t.Run(c.name, func(t *testing.T) {
			sb := sandboxOf(t)
			cfg := set.config(t, sb, px.Repo(set.path))

			px.Reset(t)
			res := cli(t, runOpts{}, append([]string{"--config", cfg}, c.args...)...).ok(t)
			reqs := artifactsRequests(px.Records(t))

			c.check(t, sb, res.Stdout)

			if artifactsCount(reqs, regproxy.ClassManifestGet, set.pub.CatalogDigest) == 0 {
				t.Errorf("the catalog index was not fetched through the proxy:\n%s", artifactsDump(reqs))
			}

			manifestGets := artifactsCount(reqs, regproxy.ClassManifestGet, schemaManifests...)
			blobGets := artifactsCount(reqs, regproxy.ClassBlobGet, schemaLayers...)

			if manifestGets != 0 || blobGets != 0 {
				t.Errorf("%d manifest-get for schema manifests and %d blob-get for schema layers, want 0 and 0", manifestGets, blobGets)
			}

			artifactsOnly(t, reqs, set.path, allowed, labels)

			files := artifactsCacheFiles(t, sb.CacheDir)
			if got := artifactsSchemaFiles(files); len(got) != 0 {
				t.Errorf("the cache holds materialized schemas: %v", got)
			}

			if got := artifactsMentions(files, set.schemaDigests()); len(got) != 0 {
				t.Errorf("the cache holds schema artifacts: %v", got)
			}

			if got := artifactsMentions(files, []string{set.pub.CatalogDigest}); len(got) == 0 {
				t.Errorf("the cache %s does not hold the catalog index; entries: %v", sb.CacheDir, files)
			}
		})
	}
}

// The items of list and patterns have no schemaPath or shadows here: those
// belong to local schemas, and E02 configures none, so the strict decoding
// also proves that catalog items never carry them.
type artifactsListItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Dialect     string `json:"dialect"`
	Digest      string `json:"digest"`
	Origin      string `json:"origin"`
}

type artifactsPatternItem struct {
	ID        string        `json:"id"`
	FileMatch []string      `json:"fileMatch"`
	Artifact  descriptorDoc `json:"artifact"`
	Origin    string        `json:"origin"`
}

type artifactsResolution struct {
	File     string        `json:"file"`
	Path     string        `json:"path"`
	Schema   string        `json:"schema"`
	Origin   string        `json:"origin"`
	Artifact descriptorDoc `json:"artifact"`
}

// checkList asserts `list --json`: one {id, name, description?, dialect?,
// digest, origin} item per catalog entry, digest being the schema manifest
// digest and origin "catalog".
func (s *artifactsSet) checkList(t *testing.T, _ *sandbox, stdout []byte) {
	t.Helper()

	items := artifactsDecodeStrict[[]artifactsListItem](t, stdout)
	got := map[string]artifactsListItem{}

	for _, it := range items {
		got[it.ID] = it
	}

	want := map[string]artifactsListItem{}
	for _, e := range s.catalog.Schemas {
		want[e.ID] = artifactsListItem{ID: e.ID, Name: e.Name, Description: e.Description, Dialect: e.Dialect, Digest: e.Artifact.Digest, Origin: "catalog"}
	}

	if len(items) != len(want) || !maps.Equal(got, want) {
		t.Errorf("list --json:\n%s\nwant the items %+v", stdout, want)
	}
}

// checkPatterns asserts `patterns --json`: {id, fileMatch, artifact, origin}
// for every schema with fileMatch (all but delta), with the catalog's
// descriptors and origin "catalog".
func (s *artifactsSet) checkPatterns(t *testing.T, _ *sandbox, stdout []byte) {
	t.Helper()

	items := artifactsDecodeStrict[[]artifactsPatternItem](t, stdout)
	want := map[string]artifactsPatternItem{}

	for _, e := range s.catalog.Schemas {
		if len(e.FileMatch) > 0 {
			want[e.ID] = artifactsPatternItem{ID: e.ID, FileMatch: e.FileMatch, Artifact: e.Artifact, Origin: "catalog"}
		}
	}

	if _, ok := want["delta"]; ok || len(want) != 5 {
		t.Fatalf("fixture changed: schemas with fileMatch %v", slices.Sorted(maps.Keys(want)))
	}

	got := map[string]artifactsPatternItem{}

	for _, it := range items {
		if _, dup := got[it.ID]; dup {
			t.Errorf("patterns --json lists %s twice:\n%s", it.ID, stdout)
		}

		got[it.ID] = it
	}

	if ids, wantIDs := slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)); len(items) != len(want) || !slices.Equal(ids, wantIDs) {
		t.Errorf("patterns --json lists %d items for the schemas %v, want one item for each of %v:\n%s", len(items), ids, wantIDs, stdout)
	}

	for id, it := range got {
		if w, ok := want[id]; ok && (!slices.Equal(it.FileMatch, w.FileMatch) || it.Artifact != w.Artifact || it.Origin != w.Origin) {
			t.Errorf("patterns --json item %+v, want %+v", it, w)
		}
	}
}

// checkResolve asserts `resolve --file .github/workflows/ci.yml --json`
// run in the workspace root, where the file does not exist.
func (s *artifactsSet) checkResolve(t *testing.T, sb *sandbox, stdout []byte) {
	t.Helper()

	got := artifactsDecodeStrict[artifactsResolution](t, stdout)
	rel := ".github/workflows/ci.yml"

	files := []string{filepath.Join(sb.Workspace, filepath.FromSlash(rel))}
	if resolved, err := filepath.EvalSymlinks(sb.Workspace); err == nil {
		files = append(files, filepath.Join(resolved, filepath.FromSlash(rel)))
	}

	want := artifactsResolution{Path: rel, Schema: "alpha", Origin: "catalog", Artifact: s.catalog.entry(t, "alpha").Artifact}
	file := got.File
	got.File = ""

	if got != want || !slices.Contains(files, file) {
		t.Errorf("resolve --json:\n%s\nwant %+v with file one of %q", stdout, want, files)
	}
}

// TestE03_OnlyRequestedSchema checks that `path alpha` with a cold cache
// downloads the catalog, alpha's manifest and alpha's payload and nothing
// that belongs to another schema, by digest in the proxy records.
func TestE03_OnlyRequestedSchema(t *testing.T) {
	set := artifactsPublish(t, suite.source)
	px := newProxy(t, suite.source)
	sb := sandboxOf(t)
	cfg := set.config(t, sb, px.Repo(set.path))

	alphaManifest := set.manifests["alpha"].Digest
	alphaPayload := set.payload("alpha").Digest.String()

	res := cli(t, runOpts{}, "--config", cfg, "path", "alpha").ok(t)
	p := artifactsPathOf(t, res, sb.CacheDir)
	reqs := artifactsRequests(px.Records(t))

	if got := readFile(t, p); !bytes.Equal(got, set.prepared(t, "alpha")) {
		t.Errorf("%s differs from the prepared alpha schema", p)
	}

	if artifactsCount(reqs, regproxy.ClassManifestGet, alphaManifest) == 0 || artifactsCount(reqs, regproxy.ClassBlobGet, alphaPayload) == 0 {
		t.Errorf("alpha's manifest %s and payload %s were not both fetched through the proxy:\n%s", alphaManifest, alphaPayload, artifactsDump(reqs))
	}

	allowed := []string{set.pub.CatalogDigest, set.metadata, set.catalogLayer.Digest.String(), alphaManifest, alphaPayload}
	artifactsOnly(t, reqs, set.path, allowed, set.labels())

	if t.Failed() {
		t.Logf("requests of path alpha:\n%s", artifactsDump(reqs))
	}

	files := artifactsCacheFiles(t, sb.CacheDir)

	rel, err := filepath.Rel(sb.CacheDir, p)
	if err != nil {
		t.Fatal(err)
	}

	if got := artifactsSchemaFiles(files); !slices.Equal(got, []string{filepath.ToSlash(rel)}) {
		t.Errorf("materialized schemas in the cache: %v, want only %s", got, rel)
	}

	for _, d := range []string{alphaManifest, alphaPayload} {
		if len(artifactsMentions(files, []string{d})) == 0 {
			t.Errorf("no cache entry is named after %s (%s); entries: %v", d, set.labels()[d], files)
		}
	}

	if got := artifactsMentions(files, set.schemaDigests("alpha")); len(got) != 0 {
		t.Errorf("the cache holds artifacts of other schemas: %v", got)
	}
}

// TestE04_WarmCacheNoNetwork checks that a warm `path alpha` prints the
// same path without any request (no manifest HEAD, no ping, no token or
// authentication exchange, no credential helper run), also when the
// registry is unreachable.
func TestE04_WarmCacheNoNetwork(t *testing.T) {
	t.Run("plain registry", func(t *testing.T) {
		set := artifactsPublish(t, suite.source)
		px := newProxy(t, suite.source)
		cfg := set.config(t, sandboxOf(t), px.Repo(set.path))

		artifactsWarmCache(t, set, px, cfg, nil, nil)
	})

	t.Run("auth registry with a credential helper", func(t *testing.T) {
		set := artifactsPublish(t, suite.auth)
		px := newProxy(t, suite.auth)
		sb := sandboxOf(t)
		user := suite.auth.User()

		dir := t.TempDir()
		db := writeJSONFile(t, dir, "db.json", map[string]map[string]string{px.Host(): {"Username": user.Username, "Secret": user.Password}})
		helperLog := filepath.Join(dir, "helper.log")
		writeCredsStoreConfig(t, sb.DockerConfig, "e2e")

		cfg := set.config(t, sb, px.Repo(set.path))
		env := []string{"E2E_CREDHELPER_DB=" + db, "E2E_CREDHELPER_LOG=" + helperLog}

		artifactsWarmCache(t, set, px, cfg, env, func(t *testing.T, cold regproxy.Stats) func(t *testing.T) {
			t.Helper()

			if cold.AuthChallenges == 0 || cold.WithAuthorization == 0 {
				t.Fatalf("the cold run did not authenticate through the proxy: %+v", cold)
			}

			before := readFile(t, helperLog)
			if !bytes.Contains(before, []byte(`"op":"get"`)) {
				t.Fatalf("the cold run did not ask the credential helper:\n%s", before)
			}

			return func(t *testing.T) {
				t.Helper()

				if after := readFile(t, helperLog); !bytes.Equal(after, before) {
					t.Errorf("the credential helper ran on a warm cache:\n%s", after[len(before):])
				}
			}
		})
	})
}

// artifactsWarmCache warms the cache with `path alpha` through px, then
// runs `path alpha` again from another directory and, with px down, `path`
// and `cat`: every warm run must succeed with identical output and without
// a single proxied request. afterCold (optional) inspects the cold run and
// returns a check that runs after every warm run.
func artifactsWarmCache(t *testing.T, set *artifactsSet, px *proxy, cfg string, env []string,
	afterCold func(t *testing.T, cold regproxy.Stats) func(t *testing.T),
) {
	t.Helper()

	sb := sandboxOf(t)
	secrets := []string{suite.auth.User().Password, suite.auth2.User().Password}

	run := func(t *testing.T, dir string, args ...string) result {
		t.Helper()

		res := cli(t, runOpts{Env: env, Dir: dir}, append([]string{"--config", cfg}, args...)...)
		for _, secret := range secrets {
			if strings.Contains(string(res.Stdout)+string(res.Stderr), secret) {
				t.Fatalf("output contains a registry password:\n%s", res)
			}
		}

		return res.ok(t)
	}

	cold := run(t, "", "path", "alpha")
	p := artifactsPathOf(t, cold, sb.CacheDir)

	stats := px.Stats(t)
	if stats.Total == 0 {
		t.Fatalf("the cold run made no request through the proxy")
	}

	check := func(*testing.T) {}
	if afterCold != nil {
		check = afterCold(t, stats)
	}

	sub := filepath.Join(sb.Workspace, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	noRequests := func(t *testing.T, what string) {
		t.Helper()

		if reqs := artifactsRequests(px.Records(t)); len(reqs) != 0 {
			t.Errorf("%s made %d requests, want 0:\n%s", what, len(reqs), artifactsDump(reqs))
		}

		check(t)
	}

	px.Reset(t)

	if warm := run(t, sub, "path", "alpha"); !bytes.Equal(warm.Stdout, cold.Stdout) {
		t.Errorf("warm path %q, cold path %q", warm.Stdout, cold.Stdout)
	}

	noRequests(t, "the warm path alpha")

	px.SetDown(t, true)

	if down := run(t, "", "path", "alpha"); !bytes.Equal(down.Stdout, cold.Stdout) {
		t.Errorf("path with the proxy down %q, cold path %q", down.Stdout, cold.Stdout)
	}

	if cat := run(t, "", "cat", "alpha"); !bytes.Equal(cat.Stdout, set.prepared(t, "alpha")) {
		t.Errorf("cat alpha with the proxy down differs from the prepared schema:\n%s", cat.Stdout)
	}

	noRequests(t, "path and cat with the proxy down")

	if got := readFile(t, p); !bytes.Equal(got, set.prepared(t, "alpha")) {
		t.Errorf("%s differs from the prepared alpha schema", p)
	}
}

// TestE05_CatExport checks `cat` (exact schema bytes, nothing else on
// stdout) and `export` (independent copy next to the notice layer as
// <destination>.NOTICE, never overwrites without --force, --force replaces a
// symlink instead of following it).
func TestE05_CatExport(t *testing.T) {
	set := artifactsPublish(t, suite.source)
	sb := sandboxOf(t)
	cfg := set.config(t, sb, suite.source.Repo(set.path))
	alpha, beta := set.prepared(t, "alpha"), set.prepared(t, "beta")
	alphaNotice, betaNotice := set.notice(t, "alpha"), set.notice(t, "beta")

	run := func(t *testing.T, args ...string) result {
		t.Helper()

		return cli(t, runOpts{Sandbox: sb}, append([]string{"--config", cfg}, args...)...)
	}

	t.Run("cat prints the prepared bytes and nothing else", func(t *testing.T) {
		for _, id := range []string{"alpha", "gamma"} {
			want := set.prepared(t, id)
			if res := run(t, "cat", id).ok(t); !bytes.Equal(res.Stdout, want) {
				t.Errorf("cat %s printed %d bytes (%s), want the %d prepared bytes (%s)",
					id, len(res.Stdout), digestOf(res.Stdout), len(want), digestOf(want))
			}
		}
	})

	materialized := artifactsPathOf(t, run(t, "path", "alpha").ok(t), sb.CacheDir)
	dir := t.TempDir()
	dest := filepath.Join(dir, "alpha.json")

	t.Run("export writes an independent copy", func(t *testing.T) {
		run(t, "export", "alpha", dest).ok(t)
		artifactsWantFile(t, dest, alpha)
		artifactsWantFile(t, dest+".NOTICE", alphaNotice)

		a, errA := os.Stat(dest)
		b, errB := os.Stat(materialized)

		if errA != nil || errB != nil {
			t.Fatalf("stat: %v, %v", errA, errB)
		}

		if os.SameFile(a, b) {
			t.Errorf("%s is the materialized cache file itself", dest)
		}

		if got := fileTree(t, dir); !slices.Equal(got, []string{"alpha.json", "alpha.json.NOTICE"}) {
			t.Errorf("export left %v in the destination directory", got)
		}
	})

	t.Run("without --force an existing file is kept", func(t *testing.T) {
		before := artifactsSnapshot(t, dir)

		for _, id := range []string{"alpha", "beta"} {
			res := run(t, "export", id, dest).wantCode(t, 2)
			if len(res.Stdout) != 0 || len(res.Stderr) == 0 {
				t.Errorf("export %s over an existing file: want empty stdout and an error on stderr:\n%s", id, res)
			}

			if after := artifactsSnapshot(t, dir); !maps.Equal(after, before) {
				t.Errorf("export %s without --force changed the destination directory:\nbefore %v\nafter  %v", id, before, after)
			}
		}
	})

	t.Run("--force replaces the file", func(t *testing.T) {
		res := run(t, "export", "beta", dest, "--force", "--json").ok(t)
		artifactsWantFile(t, dest, beta)
		artifactsWantFile(t, dest+".NOTICE", betaNotice)

		var out struct {
			ID     string `json:"id"`
			Origin string `json:"origin"`
			Path   string `json:"path"`
			Notice string `json:"notice"`
		}

		if err := json.Unmarshal(res.Stdout, &out); err != nil || out.ID != "beta" || out.Origin != "catalog" || out.Path != dest || out.Notice != dest+".NOTICE" {
			t.Errorf("export --json = %+v (%v), want beta at %s with its notice:\n%s", out, err, dest, res)
		}

		if got := fileTree(t, dir); !slices.Equal(got, []string{"alpha.json", "alpha.json.NOTICE"}) {
			t.Errorf("export --force left %v in the destination directory", got)
		}
	})

	t.Run("a symlink destination is replaced, never followed", func(t *testing.T) {
		artifactsExportSymlinks(t, run, alpha, alphaNotice)
	})
}

// notice is the notice layer (the second layer) of a schema artifact, read
// directly from the registry.
func (s *artifactsSet) notice(t *testing.T, id string) []byte {
	t.Helper()

	layers := s.manifests[id].Manifest.Layers
	if len(layers) != 2 {
		t.Fatalf("schema %s has %d layers, want a payload and a notice", id, len(layers))
	}

	return s.reg.Blob(t, s.path, layers[1].Digest.String())
}

// artifactsExportSymlinks exports over a symlink to a file and over a
// dangling symlink, both pointing into another directory: without --force
// both stay as they are (exit 2); with --force each becomes a regular file
// while the other directory stays unchanged.
func artifactsExportSymlinks(t *testing.T, run func(t *testing.T, args ...string) result, alpha, notice []byte) {
	t.Helper()

	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "target.json"), []byte(`{"sentinel":true}`))

	links := t.TempDir()
	targets := map[string]string{
		filepath.Join(links, "link.json"):     filepath.Join(outside, "target.json"),
		filepath.Join(links, "dangling.json"): filepath.Join(outside, "missing.json"),
	}

	for link, target := range targets {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}

	outsideBefore := artifactsSnapshot(t, outside)
	linksBefore := artifactsSnapshot(t, links)

	for link := range targets {
		res := run(t, "export", "alpha", link).wantCode(t, 2)
		if len(res.Stdout) != 0 {
			t.Errorf("export over %s printed to stdout:\n%s", link, res)
		}
	}

	if got := artifactsSnapshot(t, links); !maps.Equal(got, linksBefore) {
		t.Errorf("export without --force changed the symlinks:\nbefore %v\nafter  %v", linksBefore, got)
	}

	if got := artifactsSnapshot(t, outside); !maps.Equal(got, outsideBefore) {
		t.Errorf("export without --force wrote through a symlink:\nbefore %v\nafter  %v", outsideBefore, got)
	}

	for link := range targets {
		run(t, "export", "alpha", link, "--force").ok(t)
		artifactsWantFile(t, link, alpha)
		artifactsWantFile(t, link+".NOTICE", notice)
	}

	if got := artifactsSnapshot(t, outside); !maps.Equal(got, outsideBefore) {
		t.Errorf("export --force wrote through a symlink:\nbefore %v\nafter  %v", outsideBefore, got)
	}

	if got := fileTree(t, links); !slices.Equal(got, []string{"dangling.json", "dangling.json.NOTICE", "link.json", "link.json.NOTICE"}) {
		t.Errorf("export --force left %v next to the symlinks", got)
	}
}

// TestE06_Compression checks the schepherd-pack/1 compression rules on the
// wire: gamma's payload is a raw gzip stream that crosses the network in
// fewer bytes than the schema it materializes to, the materialized file is
// the prepared JSON byte for byte, small schemas and the catalog are stored
// uncompressed.
func TestE06_Compression(t *testing.T) {
	set := artifactsPublish(t, suite.source)
	prepared := set.prepared(t, "gamma")
	payload := set.payload("gamma")

	t.Run("gamma's payload layer is gzip", func(t *testing.T) {
		artifactsCheckGzipPayload(t, set, payload, prepared)
	})

	t.Run("small schemas are stored uncompressed", func(t *testing.T) {
		for _, id := range artifactsFixtureIDs {
			if id == "gamma" {
				continue
			}

			if _, ok := set.manifests[id]; !ok {
				t.Errorf("the catalog has no schema %s", id)

				continue
			}

			data := set.prepared(t, id)
			if l := set.payload(id); len(data) >= 4096 || l.MediaType != mediaTypeSchemaJSON || l.Digest.String() != digestOf(data) {
				t.Errorf("%s (%d bytes): payload %s %s, want %s %s", id, len(data), l.MediaType, l.Digest, mediaTypeSchemaJSON, digestOf(data))
			}
		}
	})

	t.Run("the catalog layer is uncompressed", func(t *testing.T) {
		if set.catalogLayer.MediaType != mediaTypeCatalog {
			t.Errorf("catalog layer media type %q, want %q", set.catalogLayer.MediaType, mediaTypeCatalog)
		}

		blob := set.reg.Blob(t, set.path, set.catalogLayer.Digest.String())
		if bytes.HasPrefix(blob, []byte{0x1f, 0x8b}) || !json.Valid(blob) {
			t.Errorf("catalog layer is not plain JSON:\n%s", clip(blob))
		}

		doc := decodeJSON[catalogDoc](t, blob)

		ids := make([]string, 0, len(doc.Schemas))
		for _, e := range doc.Schemas {
			ids = append(ids, e.ID)
		}

		slices.Sort(ids)

		if doc.FormatVersion != catalogFormatVersion || !slices.Equal(ids, artifactsFixtureIDs) {
			t.Errorf("catalog layer decodes to formatVersion %d with the schemas %v, want %d and %v", doc.FormatVersion, ids, catalogFormatVersion, artifactsFixtureIDs)
		}
	})

	t.Run("network bytes and the materialized file", func(t *testing.T) {
		px := newProxy(t, suite.source)
		sb := sandboxOf(t)
		cfg := set.config(t, sb, px.Repo(set.path))

		p := artifactsPathOf(t, cli(t, runOpts{}, "--config", cfg, "path", "gamma").ok(t), sb.CacheDir)
		data := readFile(t, p)

		if !bytes.Equal(data, prepared) || !json.Valid(data) {
			t.Errorf("%s (%d bytes) is not the prepared gamma JSON (%d bytes)", p, len(data), len(prepared))
		}

		reqs := artifactsRequests(px.Records(t))

		n := artifactsBlobBytes(reqs, payload.Digest.String())
		if n == 0 {
			t.Fatalf("gamma's payload %s was not fetched through the proxy:\n%s", payload.Digest, artifactsDump(reqs))
		}

		if n >= int64(len(data)) {
			t.Errorf("gamma's payload took %d network bytes, want fewer than the %d materialized bytes", n, len(data))
		}
	})
}

// artifactsCheckGzipPayload asserts that a payload layer is a raw gzip
// stream (no tar, zero modification time, no file name) of the prepared
// schema, at least 10 % smaller, with the content annotations of the
// uncompressed bytes.
func artifactsCheckGzipPayload(t *testing.T, set *artifactsSet, payload ocispec.Descriptor, prepared []byte) {
	t.Helper()

	if payload.MediaType != mediaTypeSchemaGzip {
		t.Fatalf("payload media type %q, want %q", payload.MediaType, mediaTypeSchemaGzip)
	}

	if len(prepared) < 4096 {
		t.Fatalf("fixture changed: gamma has only %d bytes", len(prepared))
	}

	if got, want := payload.Annotations[annotationContentDigest], digestOf(prepared); got != want {
		t.Errorf("content digest annotation %q, want %q", got, want)
	}

	if got, want := payload.Annotations[annotationContentSize], strconv.Itoa(len(prepared)); got != want {
		t.Errorf("content size annotation %q, want %q", got, want)
	}

	blob := set.reg.Blob(t, set.path, payload.Digest.String())
	if int64(len(blob)) != payload.Size || int64(len(blob))*10 > int64(len(prepared))*9 {
		t.Errorf("gzip payload has %d bytes (descriptor %d), want at most 90 %% of %d", len(blob), payload.Size, len(prepared))
	}

	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("payload is not gzip: %v", err)
	}

	if !zr.ModTime.IsZero() || zr.Name != "" || zr.Comment != "" || len(zr.Extra) != 0 {
		t.Errorf("gzip header carries metadata: %+v", zr.Header)
	}

	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompress payload: %v", err)
	}

	if !bytes.Equal(data, prepared) {
		t.Errorf("decompressed payload (%d bytes) differs from the prepared schema (%d bytes)", len(data), len(prepared))
	}
}
