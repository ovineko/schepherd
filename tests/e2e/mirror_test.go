//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"oras.land/oras-go/v2"

	"github.com/ovineko/schepherd/tests/e2e/regproxy"
)

// mirrorSetIDs are the schema IDs of the set-basic fixture.
var mirrorSetIDs = []string{"alpha", "beta", "delta", "dup1", "dup2", "gamma"}

// mirrorResultKeys are the members of the "schepherd mirror --json" output
// documented in docs/mirroring.md, sorted.
var mirrorResultKeys = []string{
	"catalogDigest", "copiedBlobs", "copiedManifests", "destination", "revision", "schemas",
	"skippedBlobs", "skippedManifests", "source", "tagsCreated", "tagsExisting",
}

// mirrorResult is the --json output of schepherd mirror.
type mirrorResult struct {
	Source           string `json:"source"`
	Destination      string `json:"destination"`
	CatalogDigest    string `json:"catalogDigest"`
	Revision         string `json:"revision"`
	Schemas          int    `json:"schemas"`
	CopiedManifests  int    `json:"copiedManifests"`
	SkippedManifests int    `json:"skippedManifests"`
	CopiedBlobs      int    `json:"copiedBlobs"`
	SkippedBlobs     int    `json:"skippedBlobs"`
	TagsCreated      int    `json:"tagsCreated"`
	TagsExisting     int    `json:"tagsExisting"`
}

// mirrorConfig writes a client configuration that only carries the
// registry tables of every suite registry and live proxy. Create private
// proxies before calling it.
func mirrorConfig(t *testing.T) string {
	t.Helper()

	return writeConfig(t, t.TempDir(), clientConfig{}.TOML())
}

func mirrorRun(t *testing.T, o runOpts, cfg, source, destination string) result {
	t.Helper()

	return cli(t, o, "--config", cfg, "mirror", source, destination, "--json")
}

// mirrorOK runs a mirror that must succeed and decodes its result, checking
// that the output has exactly the documented members.
func mirrorOK(t *testing.T, cfg, source, destination string) mirrorResult {
	t.Helper()

	res := mirrorRun(t, runOpts{}, cfg, source, destination).ok(t)

	keys := slices.Sorted(maps.Keys(decodeJSON[map[string]json.RawMessage](t, res.Stdout)))
	if !slices.Equal(keys, mirrorResultKeys) {
		t.Fatalf("mirror --json members %v, want %v\n%s", keys, mirrorResultKeys, res)
	}

	return decodeJSON[mirrorResult](t, res.Stdout)
}

// mirrorGraph is a catalog snapshot read directly from one registry: the
// catalog index, its metadata manifest and payload and every schema manifest
// it lists.
type mirrorGraph struct {
	Index       fetchedManifest
	Metadata    fetchedManifest
	CatalogBlob []byte
	Doc         catalogDoc
	// Schemas maps a schema manifest digest to its manifest.
	Schemas map[string]fetchedManifest
}

// mirrorReadGraph reads a published set-basic snapshot and fails unless the
// catalog lists exactly mirrorSetIDs, each with its own schema manifest that
// the index references, so loops over the graph cannot pass on an empty or
// truncated catalog.
func mirrorReadGraph(t *testing.T, reg *registry, repo, catalogDigest string) mirrorGraph {
	t.Helper()

	c := reg.Catalog(t, repo, catalogDigest)
	g := mirrorGraph{Index: c.Index, Metadata: c.Metadata, CatalogBlob: c.Blob, Doc: c.Doc, Schemas: map[string]fetchedManifest{}}

	ids := make([]string, 0, len(g.Doc.Schemas))

	for _, e := range g.Doc.Schemas {
		ids = append(ids, e.ID)

		if _, ok := g.Schemas[e.Artifact.Digest]; !ok {
			g.Schemas[e.Artifact.Digest] = reg.Manifest(t, repo, e.Artifact.Digest)
		}
	}

	slices.Sort(ids)

	if !slices.Equal(ids, mirrorSetIDs) {
		t.Fatalf("catalog %s lists schemas %v, want %v", catalogDigest, ids, mirrorSetIDs)
	}

	if len(g.Schemas) != len(mirrorSetIDs) {
		t.Fatalf("catalog %s references %d distinct schema manifests, want %d", catalogDigest, len(g.Schemas), len(mirrorSetIDs))
	}

	children := make([]string, 0, len(g.Schemas))
	for _, d := range c.schemaChildren() {
		children = append(children, d.Digest.String())
	}

	if !slices.Equal(slices.Sorted(slices.Values(children)), g.schemaDigests()) {
		t.Fatalf("catalog index %s references %v, catalog.json lists %v", catalogDigest, children, g.schemaDigests())
	}

	for dgst, m := range g.Schemas {
		if len(m.Manifest.Layers) == 0 {
			t.Fatalf("schema manifest %s has no payload layer:\n%s", dgst, m.Body)
		}
	}

	return g
}

// mirrorRef returns the last path segment of a recorded request: the digest
// or tag of a manifest or blob request.
func mirrorRef(r regproxy.Record) string {
	return r.Path[strings.LastIndexByte(r.Path, '/')+1:]
}

// mirrorAssertServed fails unless px served the manifest and the payload of
// every schema in g with status 200 since its last Reset.
func mirrorAssertServed(t *testing.T, px *proxy, g mirrorGraph) {
	t.Helper()

	fetched := map[string]int{}

	for _, r := range px.Records(t) {
		if (r.Class == regproxy.ClassManifestGet || r.Class == regproxy.ClassBlobGet) && r.Status == http.StatusOK {
			fetched[mirrorRef(r)]++
		}
	}

	for _, id := range mirrorSetIDs {
		dgst := g.Doc.entry(t, id).Artifact.Digest
		payload := g.Schemas[dgst].Manifest.Layers[0].Digest.String()

		if fetched[dgst] == 0 || fetched[payload] == 0 {
			t.Errorf("%s: mirror proxy served manifest %s %d times and payload %s %d times", id, dgst, fetched[dgst], payload, fetched[payload])
		}
	}
}

// schemaDigests returns the unique schema manifest digests, sorted.
func (g mirrorGraph) schemaDigests() []string {
	return slices.Sorted(maps.Keys(g.Schemas))
}

// manifestDigests returns the digests of the index, the metadata manifest
// and every schema manifest, sorted.
func (g mirrorGraph) manifestDigests() []string {
	return slices.Sorted(slices.Values(append(g.schemaDigests(), g.Index.Digest, g.Metadata.Digest)))
}

// blobDigests returns every blob the snapshot references (configs, payloads,
// notices, the catalog document), unique and sorted.
func (g mirrorGraph) blobDigests() []string {
	set := map[string]bool{}

	for _, m := range append(slices.Collect(maps.Values(g.Schemas)), g.Metadata) {
		set[m.Manifest.Config.Digest.String()] = true
		for _, l := range m.Manifest.Layers {
			set[l.Digest.String()] = true
		}
	}

	return slices.Sorted(maps.Keys(set))
}

// catalogTags returns the only tag a mirror gives the snapshot:
// catalog-<revision>. Schema manifests need no tags; the index references
// them.
func (g mirrorGraph) catalogTags() []string {
	return []string{"catalog-" + g.Doc.Revision}
}

// mirrorAssertCopy fetches the snapshot g (read from the source) directly
// from the destination and compares every manifest and blob byte for byte.
func mirrorAssertCopy(t *testing.T, g mirrorGraph, srcReg *registry, srcPath string, dstReg *registry, dstPath string) {
	t.Helper()

	for _, m := range []fetchedManifest{g.Index, g.Metadata} {
		got := dstReg.Manifest(t, dstPath, m.Digest)
		if got.Digest != m.Digest || !bytes.Equal(got.Body, m.Body) {
			t.Errorf("destination catalog manifest %s differs from the source:\n--- source ---\n%s\n--- destination ---\n%s",
				got.Digest, m.Body, got.Body)
		}

		if got.ContentType != m.ContentType {
			t.Errorf("destination serves %s as %q, the source as %q", m.Digest, got.ContentType, m.ContentType)
		}
	}

	for dgst, m := range g.Schemas {
		got := dstReg.Manifest(t, dstPath, dgst)
		if !bytes.Equal(got.Body, m.Body) || got.ContentType != m.ContentType {
			t.Errorf("destination schema manifest %s differs from the source", dgst)
		}
	}

	for _, dgst := range g.blobDigests() {
		if !bytes.Equal(dstReg.Blob(t, dstPath, dgst), srcReg.Blob(t, srcPath, dgst)) {
			t.Errorf("destination blob %s differs from the source", dgst)
		}
	}
}

// mirrorWrites are the successful writes a destination proxy recorded.
type mirrorWrites struct {
	// Manifests are the digests of manifests pushed by digest.
	Manifests []string
	// Tags are the references of manifests pushed by tag.
	Tags []string
	// Blobs are the digests of completed blob uploads.
	Blobs []string
	// BlobHeads counts blob existence checks by response status.
	BlobHeads map[int]int
}

func mirrorWritesOf(records []regproxy.Record) mirrorWrites {
	w := mirrorWrites{BlobHeads: map[int]int{}}

	for _, r := range records {
		switch r.Class {
		case regproxy.ClassManifestPut:
			if r.Status != http.StatusCreated {
				continue
			}

			ref := mirrorRef(r)
			if strings.HasPrefix(ref, "sha256:") {
				w.Manifests = append(w.Manifests, ref)
			} else {
				w.Tags = append(w.Tags, ref)
			}
		case regproxy.ClassBlobUpload:
			if r.Status != http.StatusCreated {
				continue
			}

			q, _ := url.ParseQuery(r.Query)
			w.Blobs = append(w.Blobs, q.Get("digest"))
		case regproxy.ClassBlobHead:
			w.BlobHeads[r.Status]++
		}
	}

	return w
}

// mirrorTagsWithout returns tags without the ones in drop.
func mirrorTagsWithout(tags []string, drop ...string) []string {
	return slices.DeleteFunc(slices.Clone(tags), func(tag string) bool { return slices.Contains(drop, tag) })
}

// mirrorSchemaPath runs "path id" and returns the printed path.
func mirrorSchemaPath(t *testing.T, o runOpts, cfg, id string) string {
	t.Helper()

	res := cli(t, o, "--config", cfg, "path", id).ok(t)

	p, ok := strings.CutSuffix(string(res.Stdout), "\n")
	if !ok || strings.Contains(p, "\n") || !filepath.IsAbs(p) {
		t.Fatalf("path %s printed %q, want one absolute path and a newline", id, res.Stdout)
	}

	return p
}

// TestE11_FullMirror mirrors a published snapshot into another registry
// under a different repository prefix and compares both copies directly
// through the registry API.
func TestE11_FullMirror(t *testing.T) {
	srcPath := repoPath(t)
	dstPath := repoPath(t, "mirror") + "/internal/team/schemas"
	dstProxy := newProxy(t, suite.mirror)

	srcRepo := suite.source.Repo(srcPath)
	dstRepo := dstProxy.Repo(dstPath)

	pub := publishSet(t, srcRepo, newSet(t, "set-basic"), "--now", "20260111.0000", "--update-latest")
	src := mirrorReadGraph(t, suite.source, srcPath, pub.CatalogDigest)

	res := mirrorOK(t, mirrorConfig(t), srcRepo+"@"+pub.CatalogDigest, dstRepo)
	writes := mirrorWritesOf(dstProxy.Records(t))
	t.Logf("mirror result %+v", res)

	t.Run("catalog index bytes and digest are identical", func(t *testing.T) {
		srcCatalog := suite.source.Manifest(t, srcPath, pub.CatalogDigest)
		dstCatalog := suite.mirror.Manifest(t, dstPath, pub.CatalogDigest)

		if srcCatalog.Digest != pub.CatalogDigest || dstCatalog.Digest != pub.CatalogDigest {
			t.Fatalf("catalog digests: source %s, destination %s, published %s", srcCatalog.Digest, dstCatalog.Digest, pub.CatalogDigest)
		}

		if !bytes.Equal(srcCatalog.Body, dstCatalog.Body) {
			t.Fatalf("catalog index bytes differ:\n--- source ---\n%s\n--- destination ---\n%s", srcCatalog.Body, dstCatalog.Body)
		}

		if byTag := suite.mirror.Manifest(t, dstPath, "catalog-"+pub.Revision); byTag.Digest != pub.CatalogDigest {
			t.Fatalf("destination catalog-%s points to %s, want %s", pub.Revision, byTag.Digest, pub.CatalogDigest)
		}
	})

	t.Run("every schema manifest and blob is identical", func(t *testing.T) {
		mirrorAssertCopy(t, src, suite.source, srcPath, suite.mirror, dstPath)
	})

	t.Run("result", func(t *testing.T) {
		want := mirrorResult{
			Source:           srcRepo,
			Destination:      dstRepo,
			CatalogDigest:    pub.CatalogDigest,
			Revision:         pub.Revision,
			Schemas:          len(mirrorSetIDs),
			CopiedManifests:  len(writes.Manifests),
			SkippedManifests: 0,
			CopiedBlobs:      len(src.blobDigests()),
			SkippedBlobs:     0,
			TagsCreated:      1,
			TagsExisting:     0,
		}
		if res != want {
			t.Fatalf("mirror result\n got %+v\nwant %+v (copied and skipped counts as recorded by the destination proxy)", res, want)
		}
	})

	t.Run("every manifest and blob was pushed, manifests exactly once", func(t *testing.T) {
		if got := slices.Sorted(slices.Values(writes.Manifests)); !slices.Equal(got, src.manifestDigests()) {
			t.Errorf("manifests pushed by digest %v, want each of %v once", got, src.manifestDigests())
		}

		if got := slices.Sorted(slices.Values(writes.Blobs)); !slices.Equal(got, src.blobDigests()) {
			t.Errorf("blobs uploaded %v, want each distinct blob of %v exactly once", got, src.blobDigests())
		}

		if got := slices.Sorted(slices.Values(writes.Tags)); !slices.Equal(got, src.catalogTags()) {
			t.Errorf("tags pushed %v, want %v", got, src.catalogTags())
		}
	})

	t.Run("tags match the source except catalog-latest", func(t *testing.T) {
		srcTags := suite.source.Tags(t, srcPath)
		if !slices.Contains(srcTags, "catalog-latest") {
			t.Fatalf("source tags %v lack catalog-latest (published with --update-latest)", srcTags)
		}

		if got, want := suite.mirror.Tags(t, dstPath), mirrorTagsWithout(srcTags, "catalog-latest"); !slices.Equal(got, want) {
			t.Fatalf("destination tags %v, want %v", got, want)
		}

		if got := suite.mirror.Tags(t, dstPath); !slices.Equal(got, src.catalogTags()) {
			t.Fatalf("destination tags %v, want %v", got, src.catalogTags())
		}
	})

	t.Run("the client reads the same catalog from the mirror", func(t *testing.T) {
		cfg := writeConfig(t, t.TempDir(), clientConfig{Repository: suite.mirror.Repo(dstPath), Catalog: pub.CatalogDigest}.TOML())

		out := cli(t, runOpts{}, "--config", cfg, "catalog", "--json").ok(t)
		if !bytes.Equal(out.Stdout, src.CatalogBlob) {
			t.Fatalf("catalog --json from the mirror differs from the source catalog blob\n%s", out)
		}
	})
}

// TestE12_SourceIndependence switches the source registry off and fetches
// every schema of the snapshot from the mirror with a cleared cache.
func TestE12_SourceIndependence(t *testing.T) {
	srcPath := repoPath(t)
	dstPath := repoPath(t, "mirror")
	srcRepo := suite.sourceProxy.Repo(srcPath)

	pub := publishSet(t, srcRepo, newSet(t, "set-basic"), "--now", "20260112.0000")
	mirrorOK(t, mirrorConfig(t), srcRepo+"@"+pub.CatalogDigest, suite.mirror.Repo(dstPath))

	src := mirrorReadGraph(t, suite.source, srcPath, pub.CatalogDigest)
	sb := newSandbox(t)
	o := runOpts{Sandbox: sb}

	srcCfg := writeConfig(t, t.TempDir(), clientConfig{Repository: srcRepo, Catalog: pub.CatalogDigest}.TOML())

	fromSource := map[string]string{}
	for _, id := range mirrorSetIDs {
		fromSource[id] = mirrorSchemaPath(t, o, srcCfg, id)
	}

	dstProxy := newProxy(t, suite.mirror)
	dstCfg := writeConfig(t, t.TempDir(), clientConfig{Repository: dstProxy.Repo(dstPath), Catalog: pub.CatalogDigest}.TOML())

	stoppedHost := suite.source.Host()

	suite.sourceProxy.SetDown(t, true)
	stopService(t, serviceSource)

	if serviceRunning(serviceSource) {
		t.Fatalf("%s still runs after the stop", serviceSource)
	}

	if conn, err := net.DialTimeout("tcp", stoppedHost, 2*time.Second); err == nil {
		_ = conn.Close()

		t.Fatalf("%s still accepts connections at %s after the stop", serviceSource, stoppedHost)
	}

	clearCache := func() {
		t.Helper()

		if err := os.RemoveAll(sb.CacheDir); err != nil {
			t.Fatalf("clear the client cache: %v", err)
		}

		if _, err := os.Lstat(sb.CacheDir); !os.IsNotExist(err) {
			t.Fatalf("cache %s still exists: %v", sb.CacheDir, err)
		}
	}

	clearCache()
	suite.sourceProxy.Reset(t)
	dstProxy.Reset(t)

	t.Run("path from the mirror", func(t *testing.T) {
		for _, id := range mirrorSetIDs {
			p := mirrorSchemaPath(t, o, dstCfg, id)
			if p != fromSource[id] {
				t.Errorf("%s: path from the mirror %s, from the source %s (the path depends only on the manifest digest)", id, p, fromSource[id])
			}

			if got := readFile(t, p); !bytes.Equal(got, preparedSchema(t, pub.Prepared, id)) {
				t.Errorf("%s: materialized file differs from the prepared schema", id)
			}
		}

		mirrorAssertServed(t, dstProxy, src)
	})

	clearCache()
	dstProxy.Reset(t)

	t.Run("cat from the mirror", func(t *testing.T) {
		for _, id := range mirrorSetIDs {
			res := cli(t, o, "--config", dstCfg, "cat", id).ok(t)
			if !bytes.Equal(res.Stdout, preparedSchema(t, pub.Prepared, id)) {
				t.Errorf("%s: cat output differs from the prepared schema\n%s", id, res)
			}
		}

		mirrorAssertServed(t, dstProxy, src)

		if out := cli(t, o, "--config", dstCfg, "catalog", "--json").ok(t); !bytes.Equal(out.Stdout, src.CatalogBlob) {
			t.Errorf("catalog --json from the mirror differs from the source catalog blob\n%s", out)
		}
	})

	t.Run("the source proxy saw no request", func(t *testing.T) {
		if recs := suite.sourceProxy.Records(t); len(recs) != 0 {
			t.Fatalf("source proxy recorded %d requests while the source was off: %+v", len(recs), recs)
		}
	})

	startService(t, serviceSource)
	suite.sourceProxy.SetDown(t, false)

	if got := suite.source.Manifest(t, srcPath, pub.CatalogDigest).Digest; got != pub.CatalogDigest {
		t.Fatalf("source catalog after the restart: %s", got)
	}
}

// TestE13_RepeatMirror mirrors the same snapshot twice; the second run
// finds everything in place and writes nothing.
func TestE13_RepeatMirror(t *testing.T) {
	srcProxy := newProxy(t, suite.source)
	dstProxy := newProxy(t, suite.mirror)
	srcPath, dstPath := repoPath(t), repoPath(t, "mirror")

	pub := publishSet(t, suite.source.Repo(srcPath), newSet(t, "set-basic"), "--now", "20260113.0000")
	src := mirrorReadGraph(t, suite.source, srcPath, pub.CatalogDigest)

	cfg := mirrorConfig(t)
	source := srcProxy.Repo(srcPath) + "@" + pub.CatalogDigest
	destination := dstProxy.Repo(dstPath)

	first := mirrorOK(t, cfg, source, destination)
	if first.SkippedManifests != 0 || first.CopiedManifests != len(src.manifestDigests()) || first.TagsCreated != 1 || first.TagsExisting != 0 {
		t.Fatalf("first mirror into an empty repository: %+v", first)
	}

	tagsBefore := suite.mirror.Tags(t, dstPath)

	srcProxy.Reset(t)
	dstProxy.Reset(t)

	second := mirrorOK(t, cfg, source, destination)
	t.Logf("first mirror %+v, second %+v", first, second)

	t.Run("result", func(t *testing.T) {
		want := mirrorResult{
			Source:           first.Source,
			Destination:      first.Destination,
			CatalogDigest:    pub.CatalogDigest,
			Revision:         pub.Revision,
			Schemas:          first.Schemas,
			CopiedManifests:  0,
			SkippedManifests: 1,
			CopiedBlobs:      0,
			SkippedBlobs:     0,
			TagsCreated:      0,
			TagsExisting:     1,
		}
		if second != want {
			t.Fatalf("second mirror\n got %+v\nwant %+v", second, want)
		}
	})

	t.Run("the destination received no write", func(t *testing.T) {
		checked := map[string]bool{}

		for _, r := range dstProxy.Records(t) {
			if (r.Class == regproxy.ClassManifestHead || r.Class == regproxy.ClassManifestGet) && r.Status == http.StatusOK {
				checked[mirrorRef(r)] = true
			}
		}

		if !checked[pub.CatalogDigest] {
			t.Fatalf("destination proxy recorded no successful check of the catalog index %s, so it did not see the second mirror: %+v",
				pub.CatalogDigest, dstProxy.Records(t))
		}

		stats := dstProxy.Stats(t)
		for _, class := range []string{regproxy.ClassBlobUpload, regproxy.ClassManifestPut, regproxy.ClassManifestDelete} {
			if n := stats.ByClass[class]; n != 0 {
				t.Errorf("destination proxy recorded %d %s requests: %+v", n, class, dstProxy.Records(t))
			}
		}

		if got := suite.mirror.Tags(t, dstPath); !slices.Equal(got, tagsBefore) {
			t.Errorf("destination tags changed from %v to %v", tagsBefore, got)
		}
	})

	t.Run("present schemas are not read from the source", func(t *testing.T) {
		srcRecords := srcProxy.Records(t)

		for _, want := range []struct{ class, ref string }{
			{regproxy.ClassManifestGet, pub.CatalogDigest},
			{regproxy.ClassManifestGet, src.Metadata.Digest},
			{regproxy.ClassBlobGet, src.Metadata.Manifest.Layers[0].Digest.String()},
		} {
			if !slices.ContainsFunc(srcRecords, func(r regproxy.Record) bool {
				return r.Class == want.class && r.Status == http.StatusOK && mirrorRef(r) == want.ref
			}) {
				t.Fatalf("source proxy recorded no %s of the catalog %s with status 200, so it did not see the second mirror: %+v",
					want.class, want.ref, srcRecords)
			}
		}

		schemaContent := map[string]bool{}

		for dgst, m := range src.Schemas {
			schemaContent[dgst] = true
			for _, l := range m.Manifest.Layers {
				schemaContent[l.Digest.String()] = true
			}
		}

		for _, r := range srcRecords {
			if schemaContent[mirrorRef(r)] {
				t.Errorf("source proxy served %s %s although the destination has it", r.Method, r.Path)
			}
		}
	})

	mirrorAssertCopy(t, src, suite.source, srcPath, suite.mirror, dstPath)
}

// TestE14_PartialFailure makes the destination refuse one schema manifest,
// checks that no catalog tag appears for the incomplete snapshot and that
// a second run completes it.
func TestE14_PartialFailure(t *testing.T) {
	dstProxy := newProxy(t, suite.mirror)
	srcPath, dstPath := repoPath(t), repoPath(t, "mirror")

	pub := publishSet(t, suite.source.Repo(srcPath), newSet(t, "set-basic"), "--now", "20260114.0000")
	src := mirrorReadGraph(t, suite.source, srcPath, pub.CatalogDigest)
	victim := src.Doc.entry(t, "beta").Artifact.Digest

	const faultName = "e14-schema-manifest-put"

	dstProxy.AddFault(t, regproxy.Fault{
		Name:         faultName,
		Class:        regproxy.ClassManifestPut,
		PathContains: "/manifests/" + victim,
		Status:       http.StatusInternalServerError,
	})

	cfg := mirrorConfig(t)
	source := suite.source.Repo(srcPath) + "@" + pub.CatalogDigest
	destination := dstProxy.Repo(dstPath)
	catalogTags := src.catalogTags()

	failed := mirrorRun(t, runOpts{}, cfg, source, destination)
	t.Logf("interrupted mirror: exit code %d, stderr: %s", failed.Code, bytes.TrimSpace(failed.Stderr))

	t.Run("the interrupted mirror fails with exit 4", func(t *testing.T) {
		failed.wantCode(t, 4)

		if len(failed.Stdout) != 0 {
			t.Fatalf("want an empty stdout:\n%s", failed)
		}

		if !slices.ContainsFunc(strings.Split(string(failed.Stderr), "\n"), func(line string) bool {
			return strings.Contains(line, "error") && strings.Contains(line, victim)
		}) {
			t.Fatalf("want an error on stderr that names the refused schema manifest %s:\n%s", victim, failed)
		}
	})

	failedRecords := dstProxy.Records(t)

	t.Run("the fault hit the schema manifest", func(t *testing.T) {
		if !slices.ContainsFunc(failedRecords, func(r regproxy.Record) bool { return r.Fault == faultName }) {
			t.Fatalf("no request of the mirror hit the fault: %+v", failedRecords)
		}
	})

	t.Run("no catalog in the destination", func(t *testing.T) {
		tags := suite.mirror.Tags(t, dstPath)
		if len(tags) != 0 {
			t.Errorf("destination has tags %v after the failed mirror", tags)
		}

		for _, dgst := range []string{pub.CatalogDigest, victim} {
			if status := suite.mirror.ManifestStatus(t, dstPath, dgst); status != http.StatusNotFound {
				t.Errorf("destination manifest %s: HEAD status %d, want 404", dgst, status)
			}
		}

		for _, r := range failedRecords {
			ref := mirrorRef(r)
			if r.Class == regproxy.ClassManifestPut && (ref == pub.CatalogDigest || slices.Contains(catalogTags, ref)) {
				t.Errorf("the failed mirror pushed the catalog: %s %s (status %d)", r.Method, r.Path, r.Status)
			}
		}
	})

	presentManifests := map[string]bool{}

	for _, dgst := range src.schemaDigests() {
		presentManifests[dgst] = suite.mirror.ManifestStatus(t, dstPath, dgst) == http.StatusOK
	}

	metadataPresent := suite.mirror.ManifestStatus(t, dstPath, src.Metadata.Digest) == http.StatusOK

	presentBlobs := map[string]bool{}
	for _, dgst := range src.blobDigests() {
		presentBlobs[dgst] = suite.mirror.BlobStatus(t, dstPath, dgst) == http.StatusOK
	}

	victimPayload := src.Schemas[victim].Manifest.Layers[0].Digest.String()

	if presentManifests[victim] || !slices.Contains(slices.Collect(maps.Values(presentManifests)), true) {
		t.Fatalf("the failed mirror left schema manifests %v (victim %s); the rerun needs the victim missing and another schema present to show that it continues",
			presentManifests, victim)
	}

	if !presentBlobs[victimPayload] {
		t.Fatalf("the failed mirror did not upload the payload %s of %s before its manifest push; the rerun cannot show that it skips present blobs",
			victimPayload, victim)
	}

	dstProxy.ClearFaults()
	dstProxy.Reset(t)

	res := mirrorOK(t, cfg, source, destination)
	records := dstProxy.Records(t)
	writes := mirrorWritesOf(records)
	t.Logf("rerun result %+v", res)

	t.Run("the rerun continues where the first run stopped", func(t *testing.T) {
		if !slices.ContainsFunc(records, func(r regproxy.Record) bool {
			return r.Class == regproxy.ClassBlobHead && r.Status == http.StatusOK && mirrorRef(r) == victimPayload
		}) {
			t.Errorf("the rerun did not find the present payload %s of %s with a HEAD: %+v", victimPayload, victim, records)
		}

		if res.SkippedBlobs == 0 || res.SkippedBlobs != writes.BlobHeads[http.StatusOK] {
			t.Errorf("rerun skippedBlobs %d; the destination proxy answered %d blob HEADs with 200", res.SkippedBlobs, writes.BlobHeads[http.StatusOK])
		}

		var wantPushed []string

		skipped := 0

		for _, dgst := range src.schemaDigests() {
			if presentManifests[dgst] {
				skipped++
			} else {
				wantPushed = append(wantPushed, dgst)
			}
		}

		if metadataPresent {
			skipped++
		} else {
			wantPushed = append(wantPushed, src.Metadata.Digest)
		}

		wantPushed = slices.Sorted(slices.Values(append(wantPushed, pub.CatalogDigest)))

		if got := slices.Sorted(slices.Values(writes.Manifests)); !slices.Equal(got, wantPushed) {
			t.Errorf("manifests pushed by the rerun %v, want %v", got, wantPushed)
		}

		for _, dgst := range writes.Blobs {
			if presentBlobs[dgst] {
				t.Errorf("the rerun uploaded blob %s again although the destination had it", dgst)
			}
		}

		if res.CatalogDigest != pub.CatalogDigest || res.Revision != pub.Revision || res.Schemas != len(mirrorSetIDs) ||
			res.CopiedManifests != len(writes.Manifests) || res.SkippedManifests != skipped ||
			res.CopiedBlobs != len(writes.Blobs) || res.TagsCreated != len(writes.Tags) ||
			res.TagsCreated != 1 || res.TagsExisting != 0 {
			t.Errorf("rerun result %+v; recorded %d manifest pushes, %d blob uploads, %d tag pushes; %d manifests were present",
				res, len(writes.Manifests), len(writes.Blobs), len(writes.Tags), skipped)
		}
	})

	t.Run("tags present after the rerun", func(t *testing.T) {
		tags := suite.mirror.Tags(t, dstPath)
		if !slices.Equal(tags, suite.source.Tags(t, srcPath)) || !slices.Equal(tags, catalogTags) {
			t.Fatalf("destination tags %v, want %v", tags, catalogTags)
		}

		for _, tag := range catalogTags {
			if got := suite.mirror.Manifest(t, dstPath, tag).Digest; got != pub.CatalogDigest {
				t.Errorf("destination %s points to %s, want %s", tag, got, pub.CatalogDigest)
			}
		}
	})

	mirrorAssertCopy(t, src, suite.source, srcPath, suite.mirror, dstPath)
}

// TestE15_Retention checks that a snapshot needs no tags besides its
// revision tag: the publisher and the mirror create no schema-sha256-<hex>
// or catalog-sha256-<hex> tags, and in both registries the catalog index
// references the metadata manifest and every schema manifest, which is what
// keeps them reachable for registries and their garbage collectors.
func TestE15_Retention(t *testing.T) {
	srcPath, dstPath := repoPath(t), repoPath(t, "mirror")

	pub := publishSet(t, suite.source.Repo(srcPath), newSet(t, "set-basic"), "--now", "20260115.0000", "--update-latest")
	src := mirrorReadGraph(t, suite.source, srcPath, pub.CatalogDigest)

	mirrorOK(t, mirrorConfig(t), suite.source.Repo(srcPath)+"@"+pub.CatalogDigest, suite.mirror.Repo(dstPath))

	for _, loc := range []struct {
		reg  *registry
		path string
		tags []string
	}{
		{suite.source, srcPath, []string{"catalog-" + pub.Revision, "catalog-latest"}},
		{suite.mirror, dstPath, src.catalogTags()},
	} {
		t.Run(loc.reg.service, func(t *testing.T) {
			if tags := loc.reg.Tags(t, loc.path); !slices.Equal(tags, loc.tags) {
				t.Errorf("tags %v, want only %v", tags, loc.tags)
			}

			c := loc.reg.Catalog(t, loc.path, "catalog-"+pub.Revision)
			if c.Index.Digest != pub.CatalogDigest {
				t.Fatalf("catalog-%s points to %s, want %s", pub.Revision, c.Index.Digest, pub.CatalogDigest)
			}

			checkIndex(t, c)

			referenced := []string{c.Metadata.Digest}
			for _, d := range c.schemaChildren() {
				referenced = append(referenced, d.Digest.String())

				if status := loc.reg.ManifestStatus(t, loc.path, d.Digest.String()); status != http.StatusOK {
					t.Errorf("referenced schema manifest %s: HEAD status %d", d.Digest, status)
				}
			}

			if got, want := slices.Sorted(slices.Values(referenced)), mirrorWithoutIndex(src.manifestDigests(), pub.CatalogDigest); !slices.Equal(got, want) {
				t.Errorf("the index references %v, want the metadata manifest and every schema manifest %v", got, want)
			}
		})
	}
}

func mirrorWithoutIndex(digests []string, index string) []string {
	return slices.DeleteFunc(slices.Clone(digests), func(d string) bool { return d == index })
}

// TestE42_GenericCopy copies a published catalog with the plain ORAS
// library (oras.Copy with its default options, no Schepherd code) into
// another registry and pins and uses the copy: the catalog is one OCI graph,
// so a generic copy is complete and byte-identical.
func TestE42_GenericCopy(t *testing.T) {
	srcPath, dstPath := repoPath(t), repoPath(t, "generic")

	pub := publishSet(t, suite.source.Repo(srcPath), newSet(t, "set-basic"), "--now", "20260116.0000")
	src := mirrorReadGraph(t, suite.source, srcPath, pub.CatalogDigest)
	tag := "catalog-" + pub.Revision

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	root, err := oras.Copy(ctx, suite.source.orasRepository(t, srcPath), tag, suite.mirror.orasRepository(t, dstPath), tag, oras.DefaultCopyOptions)
	if err != nil {
		t.Fatalf("copy %s with ORAS: %v", tag, err)
	}

	t.Run("the copy is byte-identical and carries no retention tags", func(t *testing.T) {
		if root.Digest.String() != pub.CatalogDigest || root.MediaType != mediaTypeIndex {
			t.Fatalf("ORAS copied the root %+v, want the catalog index %s", root, pub.CatalogDigest)
		}

		mirrorAssertCopy(t, src, suite.source, srcPath, suite.mirror, dstPath)

		for _, loc := range []struct {
			reg  *registry
			path string
		}{{suite.source, srcPath}, {suite.mirror, dstPath}} {
			if tags := loc.reg.Tags(t, loc.path); !slices.Equal(tags, []string{tag}) {
				t.Errorf("%s tags %v, want only %s", loc.reg.service, tags, tag)
			}
		}
	})

	dstRepo := suite.mirror.Repo(dstPath)
	cfg := mirrorConfig(t)

	t.Run("pin resolves the copy to the same digest", func(t *testing.T) {
		res := cli(t, runOpts{}, "--config", cfg, "pin", dstRepo+":"+tag, "--json").ok(t)

		var got struct {
			Repository string `json:"repository"`
			Tag        string `json:"tag"`
			Digest     string `json:"digest"`
			Revision   string `json:"revision"`
			Schemas    int    `json:"schemas"`
		}

		if err := json.Unmarshal(res.Stdout, &got); err != nil {
			t.Fatalf("pin --json output: %v\n%s", err, res)
		}

		if got.Repository != dstRepo || got.Tag != tag || got.Digest != pub.CatalogDigest || got.Revision != pub.Revision || got.Schemas != len(mirrorSetIDs) {
			t.Fatalf("pin resolved %+v, want digest %s, revision %s and %d schemas", got, pub.CatalogDigest, pub.Revision, len(mirrorSetIDs))
		}
	})

	t.Run("every schema resolves from the copy", func(t *testing.T) {
		o := runOpts{Sandbox: newSandbox(t)}
		copyCfg := writeConfig(t, t.TempDir(), clientConfig{Repository: dstRepo, Catalog: pub.CatalogDigest}.TOML())

		if out := cli(t, o, "--config", copyCfg, "catalog", "--json").ok(t); !bytes.Equal(out.Stdout, src.CatalogBlob) {
			t.Errorf("catalog --json from the copy differs from the source catalog blob\n%s", out)
		}

		for _, id := range mirrorSetIDs {
			p := mirrorSchemaPath(t, o, copyCfg, id)
			if !bytes.Equal(readFile(t, p), preparedSchema(t, pub.Prepared, id)) {
				t.Errorf("%s: materialized file differs from the prepared schema", id)
			}
		}
	})
}
