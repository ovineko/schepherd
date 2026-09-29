//go:build e2e

package e2e

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/tests/e2e/regproxy"
)

const (
	// integrityBombFlood is what a gzip bomb expands to: the largest value
	// any limit may be configured to (1 GiB), so no configuration can accept it.
	integrityBombFlood = 1 << 30
	// integrityMaxRSS bounds the peak memory of a client refusing a hostile
	// artifact; a client that inflated a bomb in memory would exceed it.
	integrityMaxRSS = 256 << 20
	// integrityDiskSlack is what a sandbox may hold while a bomb is refused
	// besides the payload blob and the declared content: manifests, the
	// catalog, lock files and the configuration.
	integrityDiskSlack = 1 << 20
	// integrityDeclaredBomb is the declared content size of the bomb whose
	// payload is smaller than its declared content, so that only inflating
	// past the declared size can reveal it.
	integrityDeclaredBomb = 2 << 20
	integrityConcurrency  = 8
	// integrityRusageThread is RUSAGE_THREAD, which the syscall package does
	// not name; the suite runs on Linux only (setUp).
	integrityRusageThread = 1
)

// integrityHostile is one hand-crafted schema artifact of E20.
type integrityHostile struct {
	id     string
	layers []rawLayer
	// sizeDelta is added to the real manifest size in the catalog entry.
	sizeDelta int64
	bomb      bool
}

// TestE20_ResourceLimits pushes hostile artifacts with ORAS into a dedicated
// repository and checks that the client refuses each of them with exit code
// 5 without materializing anything, and that every documented limit is
// enforced at its boundary.
func TestE20_ResourceLimits(t *testing.T) {
	t.Parallel()

	reg := suite.source
	path := repoPath(t)

	good := []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","title":"good","type":"object"}`)
	goodGzip := integrityCompact(t, readFile(t, fixture("set-basic", "schemas", "gamma.json")))
	goodGzipPayload := gzipBytes(goodGzip)

	goodDesc := pushRaw(t, reg, path, rawArtifact{Layers: []rawLayer{schemaLayer(mediaTypeSchemaJSON, good, good)}})
	goodGzipDesc := pushRaw(t, reg, path, rawArtifact{Layers: []rawLayer{schemaLayer(mediaTypeSchemaGzip, goodGzipPayload, goodGzip)}})

	// A gzip payload larger than its content is valid (gzip adds a header
	// and a trailer), so a client cannot refuse the bombs by comparing the
	// payload size with the declared content size: it has to inflate.
	tinyGzip := []byte(`{"type":"object"}`)
	tinyGzipPayload := gzipBytes(tinyGzip)

	if len(tinyGzipPayload) <= len(tinyGzip) {
		t.Fatalf("the tiny gzip payload (%d bytes) must be larger than its content (%d bytes)", len(tinyGzipPayload), len(tinyGzip))
	}

	tinyGzipDesc := pushRaw(t, reg, path, rawArtifact{Layers: []rawLayer{schemaLayer(mediaTypeSchemaGzip, tinyGzipPayload, tinyGzip)}})

	wellFormed := []catalogEntry{
		{ID: "good", Name: "good", Artifact: descriptorOf(goodDesc)},
		{ID: "good-gzip", Name: "good gzip", Artifact: descriptorOf(goodGzipDesc)},
	}
	entries := append(slices.Clone(wellFormed), catalogEntry{ID: "tiny-gzip", Name: "tiny gzip", Artifact: descriptorOf(tinyGzipDesc)})

	hostile := integrityHostiles(t)
	for _, h := range hostile {
		desc := descriptorOf(pushRaw(t, reg, path, rawArtifact{Layers: h.layers}))
		desc.Size += h.sizeDelta
		entries = append(entries, catalogEntry{ID: h.id, Name: h.id, Artifact: desc})
	}

	catalog, _ := integrityPushCatalog(t, reg, path, entries)
	config := func(catalog ocispec.Descriptor, limits string) string {
		extra := ""
		if limits != "" {
			extra = "[limits]\n" + limits
		}

		return clientConfig{Repository: reg.Repo(path), Catalog: catalog.Digest.String(), Extra: extra}.TOML()
	}

	t.Run("well-formed artifacts in the same catalog", func(t *testing.T) {
		t.Parallel()

		sb := sandboxOf(t)
		cfg := writeConfig(t, sb.Workspace, config(catalog, ""))

		for _, s := range []struct {
			id   string
			desc ocispec.Descriptor
			want []byte
		}{
			{"good", goodDesc, good},
			{"good-gzip", goodGzipDesc, goodGzip},
			{"tiny-gzip", tinyGzipDesc, tinyGzip},
		} {
			got := integrityOutputPath(cli(t, runOpts{}, "--config", cfg, "path", s.id).ok(t))
			if wantPath := integrityMaterializedPath(sb.CacheDir, s.desc.Digest.String()); got != wantPath {
				t.Fatalf("path %s printed %s, want %s", s.id, got, wantPath)
			}

			if !bytes.Equal(readFile(t, got), s.want) {
				t.Fatalf("materialized %s differs from the pushed content", s.id)
			}
		}
	})

	t.Run("hostile schema artifacts", func(t *testing.T) {
		t.Parallel()

		for _, h := range hostile {
			t.Run(h.id, func(t *testing.T) {
				t.Parallel()
				integrityRefused(t, config(catalog, ""), h, "good-gzip")
			})
		}
	})

	// The limit cases use a catalog of the well-formed schemas only: a
	// client may refuse a whole catalog that lists a manifest larger than
	// max_manifest_bytes, so the hostile entries must not interfere.
	limitsCatalog, doc := integrityPushCatalog(t, reg, path, wellFormed)
	limitsMetadata := int64(len(reg.Catalog(t, path, limitsCatalog.Digest.String()).Metadata.Body))

	largestID, largest := "good", goodDesc
	if goodGzipDesc.Size > goodDesc.Size {
		largestID, largest = "good-gzip", goodGzipDesc
	}

	manifestsFit := int(max(limitsCatalog.Size, limitsMetadata, goodDesc.Size, goodGzipDesc.Size))

	t.Run("limits", func(t *testing.T) {
		t.Parallel()

		cases := []struct {
			name   string
			limits string
			args   []string
			want   int
		}{
			{"catalog bytes below the catalog: catalog", integrityLimit("max_catalog_bytes", len(doc)-1), []string{"catalog", "--json"}, 5},
			{"catalog bytes below the catalog: list", integrityLimit("max_catalog_bytes", len(doc)-1), []string{"list", "--json"}, 5},
			{"catalog bytes below the catalog: path", integrityLimit("max_catalog_bytes", len(doc)-1), []string{"path", "good"}, 5},
			{"catalog bytes equal to the catalog", integrityLimit("max_catalog_bytes", len(doc)), []string{"list", "--json"}, 0},
			{"catalog entries below the count", integrityLimit("max_catalog_entries", len(wellFormed)-1), []string{"list", "--json"}, 5},
			{"catalog entries equal to the count", integrityLimit("max_catalog_entries", len(wellFormed)), []string{"list", "--json"}, 0},
			{"manifest bytes below the catalog index", integrityLimit("max_manifest_bytes", int(limitsCatalog.Size)-1), []string{"list", "--json"}, 5},
			{"manifest bytes below the catalog metadata manifest", integrityLimit("max_manifest_bytes", int(limitsMetadata)-1), []string{"list", "--json"}, 5},
			{"manifest bytes below the largest schema manifest", integrityLimit("max_manifest_bytes", int(largest.Size)-1), []string{"path", largestID}, 5},
			{"manifest bytes equal to the largest manifest: list", integrityLimit("max_manifest_bytes", manifestsFit), []string{"list", "--json"}, 0},
			{"manifest bytes equal to the largest manifest: path", integrityLimit("max_manifest_bytes", manifestsFit), []string{"path", largestID}, 0},
			{"payload bytes below a plain payload", integrityLimit("max_payload_bytes", len(good)-1), []string{"path", "good"}, 5},
			{"schema bytes below a plain schema", integrityLimit("max_schema_bytes", len(good)-1), []string{"path", "good"}, 5},
			{"payload and schema bytes equal to a plain schema", integrityLimit("max_payload_bytes", len(good)) +
				integrityLimit("max_schema_bytes", len(good)), []string{"path", "good"}, 0},
			{"payload bytes below a gzip payload", integrityLimit("max_payload_bytes", len(goodGzipPayload)-1), []string{"path", "good-gzip"}, 5},
			{"schema bytes below a gzip schema whose payload fits", integrityLimit("max_payload_bytes", len(goodGzipPayload)) +
				integrityLimit("max_schema_bytes", len(goodGzip)-1), []string{"path", "good-gzip"}, 5},
			{"payload and schema bytes equal to a gzip schema", integrityLimit("max_payload_bytes", len(goodGzipPayload)) +
				integrityLimit("max_schema_bytes", len(goodGzip)), []string{"path", "good-gzip"}, 0},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				sb := sandboxOf(t)
				cfg := writeConfig(t, sb.Workspace, config(limitsCatalog, tc.limits))
				res := cli(t, runOpts{}, append([]string{"--config", cfg}, tc.args...)...).wantCode(t, tc.want)

				if tc.want == 0 {
					return
				}

				if len(res.Stdout) != 0 || len(bytes.TrimSpace(res.Stderr)) == 0 {
					t.Fatalf("a refused command must print nothing on stdout and a reason on stderr:\n%s", res)
				}

				if left := integrityMaterializedEntries(t, sb.CacheDir); len(left) != 0 {
					t.Fatalf("materialized entries after a refused command: %v", left)
				}
			})
		}
	})
}

// integrityHostiles builds the hostile schema artifacts of E20. Each has the
// schema envelope and differs from a valid artifact in one respect.
func integrityHostiles(t *testing.T) []integrityHostile {
	t.Helper()

	small := func(title string) []byte {
		return []byte(`{"title":"` + title + `","type":"string"}`)
	}

	notice := rawLayer{MediaType: mediaTypeNotice, Data: []byte("MIT License (e2e fixture)\n")}

	prefix := small("bomb-valid-prefix")
	prefixed := integrityGzipFlood(t, prefix, ' ')
	// The declared content is exactly the first integrityDeclaredBomb bytes
	// of the flood, so its digest matches and only the byte after it is
	// wrong.
	declared := append(slices.Clone(prefix), bytes.Repeat([]byte{' '}, integrityDeclaredBomb-len(prefix))...)

	if len(prefixed) >= len(declared) {
		t.Fatalf("the bomb payload (%d bytes) must be smaller than its declared content (%d bytes)", len(prefixed), len(declared))
	}

	return []integrityHostile{
		{id: "bomb", bomb: true, layers: []rawLayer{
			schemaLayer(mediaTypeSchemaGzip, integrityGzipFlood(t, nil, 0), small("bomb")),
		}},
		{id: "bomb-valid-prefix", bomb: true, layers: []rawLayer{
			schemaLayer(mediaTypeSchemaGzip, prefixed, prefix),
		}},
		{id: "bomb-declared-2mib", bomb: true, layers: []rawLayer{
			schemaLayer(mediaTypeSchemaGzip, prefixed, declared),
		}},
		{id: "extra-layer", layers: []rawLayer{
			schemaLayer(mediaTypeSchemaJSON, small("extra-layer"), small("extra-layer")),
			{MediaType: "application/octet-stream", Data: []byte("extra")},
		}},
		{id: "layer-json", layers: []rawLayer{
			schemaLayer("application/json", small("layer-json"), small("layer-json")),
		}},
		{id: "layer-tar-gzip", layers: []rawLayer{
			schemaLayer("application/vnd.oci.image.layer.v1.tar+gzip", gzipBytes(small("layer-tar-gzip")), small("layer-tar-gzip")),
		}},
		{id: "notice-first", layers: []rawLayer{
			notice,
			schemaLayer(mediaTypeSchemaJSON, small("notice-first"), small("notice-first")),
		}},
		{id: "size-larger", sizeDelta: 1, layers: []rawLayer{
			schemaLayer(mediaTypeSchemaJSON, small("size-larger"), small("size-larger")),
		}},
		{id: "size-smaller", sizeDelta: -1, layers: []rawLayer{
			schemaLayer(mediaTypeSchemaJSON, small("size-smaller"), small("size-smaller")),
		}},
		{id: "two-payloads", layers: []rawLayer{
			schemaLayer(mediaTypeSchemaJSON, small("two-payloads-1"), small("two-payloads-1")),
			schemaLayer(mediaTypeSchemaJSON, small("two-payloads-2"), small("two-payloads-2")),
		}},
	}
}

// integrityRefused runs path, cat and export for a hostile schema in a
// fresh sandbox: each must exit 5 with an empty stdout, and nothing may be
// materialized or exported. For a gzip bomb, path must also stay far below
// the memory, disk and CPU cost of inflating it; wellFormed is a schema of
// the same catalog whose cold fetch costs what refusing the bomb may cost
// besides inflating up to the declared size.
func integrityRefused(t *testing.T, config string, h integrityHostile, wellFormed string) {
	t.Helper()

	sb := sandboxOf(t)
	cfg := writeConfig(t, sb.Workspace, config)
	workspace := fileTree(t, sb.Workspace)

	res, use := integrityRun(t, sb, "--config", cfg, "path", h.id)
	res.wantCode(t, 5)
	t.Logf("path %s (peak memory %d MiB, CPU %v, peak disk %d bytes in %d samples): %s",
		h.id, use.rss>>20, use.cpu, use.peakDisk, use.samples, bytes.TrimSpace(res.Stderr))

	if len(res.Stdout) != 0 || len(bytes.TrimSpace(res.Stderr)) == 0 {
		t.Errorf("path %s must print nothing on stdout and a reason on stderr:\n%s", h.id, res)
	}

	if use.rss > integrityMaxRSS {
		t.Errorf("path %s peaked at %d MiB of memory, want at most %d MiB", h.id, use.rss>>20, integrityMaxRSS>>20)
	}

	if h.bomb {
		integrityBombBounded(t, config, h, use, wellFormed)
	}

	if cat := cli(t, runOpts{}, "--config", cfg, "cat", h.id); cat.Code != 5 || len(cat.Stdout) != 0 {
		t.Errorf("cat %s: want exit code 5 and nothing on stdout, got:\n%s", h.id, cat)
	}

	dest := filepath.Join(sb.Workspace, "exported.json")
	if exp := cli(t, runOpts{}, "--config", cfg, "export", h.id, dest); exp.Code != 5 || len(exp.Stdout) != 0 {
		t.Errorf("export %s: want exit code 5, got:\n%s", h.id, exp)
	}

	if after := fileTree(t, sb.Workspace); !slices.Equal(after, workspace) {
		t.Errorf("the workspace changed from %v to %v", workspace, after)
	}

	if left := integrityMaterializedEntries(t, sb.CacheDir); len(left) != 0 {
		t.Errorf("materialized entries after refusing %s: %v", h.id, left)
	}
}

// TestE21_Concurrency starts eight "path gamma" processes on one cold cache,
// holding their registry requests until all of them have started, while a
// reader polls the final path: the processes overlap, every one succeeds
// with the same path, the file verifies, and the reader sees the file go
// from absent to complete without ever seeing partial content.
func TestE21_Concurrency(t *testing.T) {
	t.Parallel()

	reg := suite.source
	px := newProxy(t, reg)
	path := repoPath(t)

	pub := publishSet(t, px.Repo(path), newSet(t, "set-basic"), "--now", "20260101.0000")
	want := preparedSchema(t, pub.Prepared, "gamma")

	gamma := integrityCatalogOf(t, reg, path, pub.CatalogDigest).entry(t, "gamma")

	payload := reg.Manifest(t, path, gamma.Artifact.Digest).Manifest.Layers[0]
	if payload.MediaType != mediaTypeSchemaGzip || payload.Annotations[annotationContentDigest] != digestOf(want) {
		t.Fatalf("gamma's payload is not the gzip-packed prepared schema: %+v", payload)
	}

	sb := sandboxOf(t)
	gate := integrityNewGate(t, px.Host())
	cfg := writeConfig(t, sb.Workspace, clientConfig{
		Repository: gate.host + "/" + path,
		Catalog:    pub.CatalogDigest,
		Registries: map[string]*registrySettings{gate.host: {PlainHTTP: true}},
	}.TOML())
	final := integrityMaterializedPath(sb.CacheDir, gamma.Artifact.Digest)

	if _, err := os.Lstat(sb.CacheDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the cache is not cold: %v", err)
	}

	px.Reset(t)

	stop := make(chan struct{})
	watching := make(chan struct{})
	reports := make(chan integrityReaderReport, 1)

	go func() { reports <- integrityWatch(final, want, watching, stop) }()

	<-watching

	procs := make([]*process, integrityConcurrency)
	for i := range procs {
		procs[i] = startBin(t, "schepherd", runOpts{}, "--config", cfg, "path", "gamma")
	}

	results := make([]result, len(procs))

	var wg sync.WaitGroup
	for i, p := range procs {
		wg.Go(func() { results[i] = p.Wait() })
	}

	// With a cold cache no process can finish before the gate opens, so all
	// of them are running when it does.
	eventually(t, time.Minute, "a request held at the gate", func() bool { return gate.held.Load() > 0 })
	gate.Open()
	wg.Wait()

	close(stop)

	report := <-reports

	for i, res := range results {
		if res.Code != 0 || string(res.Stdout) != final+"\n" {
			t.Errorf("process %d: want exit code 0 and %s, got:\n%s", i, final, res)
		}
	}

	lastStart, firstEnd := procs[0].start, procs[0].start.Add(results[0].Duration)
	for i, p := range procs {
		if p.start.After(lastStart) {
			lastStart = p.start
		}

		if end := p.start.Add(results[i].Duration); end.Before(firstEnd) {
			firstEnd = end
		}
	}

	t.Logf("reader: %d complete reads, %d misses; requests held at the gate: %d; all %d processes ran together for %v; payload downloads: %d",
		report.complete, report.missing, gate.held.Load(), len(procs), firstEnd.Sub(lastStart),
		integrityCount(px.Records(t), regproxy.ClassBlobGet, payload.Digest.String()))

	if !lastStart.Before(firstEnd) {
		t.Errorf("the processes did not overlap: the last one started %v after the first one ended", lastStart.Sub(firstEnd))
	}

	if len(report.partial) != 0 || len(report.errors) != 0 {
		t.Errorf("the reader saw partial content (lengths %v of %d) or errors %v at %s", report.partial, len(want), report.errors, final)
	}

	if report.missing == 0 || report.complete == 0 {
		t.Errorf("the reader found the file missing %d times and complete %d times; it must see both to have watched the file appear",
			report.missing, report.complete)
	}

	integrityVerifyFile(t, final, want)

	again := cli(t, runOpts{}, "--config", cfg, "path", "gamma").ok(t)
	if integrityOutputPath(again) != final {
		t.Fatalf("a later path printed %q, want %s", again.Stdout, final)
	}

	if cat := cli(t, runOpts{}, "--config", cfg, "cat", "gamma").ok(t); !bytes.Equal(cat.Stdout, want) {
		t.Fatalf("cat gamma differs from the prepared schema")
	}
}

type integrityReaderReport struct {
	complete int
	missing  int
	partial  []int
	errors   []string
}

// integrityWatch reads path in a tight loop until stop is closed and
// classifies every read: absent, complete (equal to want) or anything else.
// It closes watching after the first read.
func integrityWatch(path string, want []byte, watching chan<- struct{}, stop <-chan struct{}) integrityReaderReport {
	var r integrityReaderReport

	for first := true; ; first = false {
		if !first {
			select {
			case <-stop:
				return r
			default:
			}
		}

		data, err := os.ReadFile(path)

		switch {
		case errors.Is(err, fs.ErrNotExist):
			r.missing++
		case err != nil:
			if len(r.errors) < 10 {
				r.errors = append(r.errors, err.Error())
			}
		case bytes.Equal(data, want):
			r.complete++
		default:
			if len(r.partial) < 10 {
				r.partial = append(r.partial, len(data))
			}
		}

		if first {
			close(watching)
		}
	}
}

// integrityGate is a plain-HTTP reverse proxy that holds every request
// until Open.
type integrityGate struct {
	host string
	// held counts the requests that arrived before Open.
	held atomic.Int64
	open chan struct{}
	once sync.Once
}

// integrityNewGate starts a gate in front of the plain-HTTP registry (or
// proxy) at upstream; it opens and closes when the test ends.
func integrityNewGate(t *testing.T, upstream string) *integrityGate {
	t.Helper()

	g := &integrityGate{open: make(chan struct{})}
	target := &url.URL{Scheme: "http", Host: upstream}
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = r.In.Host
		},
		Transport: &http.Transport{Proxy: nil, DisableCompression: true},
		ErrorLog:  log.New(io.Discard, "", 0),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-g.open:
		default:
			g.held.Add(1)

			select {
			case <-g.open:
			case <-r.Context().Done():
				return
			}
		}

		rp.ServeHTTP(w, r)
	}))

	t.Cleanup(srv.Close)
	t.Cleanup(g.Open)

	g.host = strings.TrimPrefix(srv.URL, "http://")

	return g
}

// Open lets every held and later request through.
func (g *integrityGate) Open() {
	g.once.Do(func() { close(g.open) })
}

// integrityVerifyFile checks a materialized schema: a read-only regular file
// (not a symlink) with exactly the expected bytes.
func integrityVerifyFile(t *testing.T, path string, want []byte) {
	t.Helper()

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("materialized schema: %v", err)
	}

	if !info.Mode().IsRegular() || info.Mode().Perm()&0o222 != 0 {
		t.Errorf("materialized schema %s has mode %v, want a read-only regular file", path, info.Mode())
	}

	if got := readFile(t, path); !bytes.Equal(got, want) {
		t.Errorf("materialized schema %s: %d bytes with digest %s, want %d bytes with digest %s",
			path, len(got), digestOf(got), len(want), digestOf(want))
	}
}

// TestE22_PathSafety checks that nothing from a registry or planted in the
// cache makes the client write outside its cache: title annotations never
// name files, a symlink planted in place of a materialized entry is not
// written through, and export writes only where its destination resolves.
func TestE22_PathSafety(t *testing.T) {
	t.Parallel()

	reg := suite.source
	path := repoPath(t)

	content := []byte(`{"$schema":"http://json-schema.org/draft-07/schema#","title":"path safety","type":"object"}`)
	large := integrityCompact(t, readFile(t, fixture("set-basic", "schemas", "gamma.json")))
	outside := t.TempDir()

	titled := []struct {
		id, mediaType, title string
		content              []byte
	}{
		{"title-absolute", mediaTypeSchemaJSON, filepath.Join(outside, "escape"), content},
		{"title-escapes-cache", mediaTypeSchemaJSON, strings.Repeat("../", 5) + "escape", content},
		{"title-gzip", mediaTypeSchemaGzip, "../../escape.gz", large},
		{"title-parent", mediaTypeSchemaJSON, "../../escape", content},
	}

	plain := pushRaw(t, reg, path, rawArtifact{Layers: []rawLayer{schemaLayer(mediaTypeSchemaJSON, content, content)}})
	entries := make([]catalogEntry, 0, 1+len(titled))
	entries = append(entries, catalogEntry{ID: "plain", Name: "plain", Artifact: descriptorOf(plain)})
	digests := map[string]string{}

	for _, s := range titled {
		payload := s.content
		if s.mediaType == mediaTypeSchemaGzip {
			payload = gzipBytes(s.content)
		}

		layer := schemaLayer(s.mediaType, payload, s.content)
		layer.Annotations[annotationTitle] = s.title

		desc := pushRaw(t, reg, path, rawArtifact{Layers: []rawLayer{layer}})
		digests[s.id] = desc.Digest.String()
		entries = append(entries, catalogEntry{ID: s.id, Name: s.id, Artifact: descriptorOf(desc)})
	}

	catalog, _ := integrityPushCatalog(t, reg, path, entries)
	config := clientConfig{Repository: reg.Repo(path), Catalog: catalog.Digest.String()}.TOML()

	integrityBackdate(t, outside)

	t.Run("title annotations", func(t *testing.T) {
		t.Parallel()

		for _, s := range titled {
			t.Run(s.id, func(t *testing.T) {
				t.Parallel()
				integrityTitleSafe(t, config, s.id, s.title, digests[s.id], s.content, outside)
			})
		}
	})

	t.Run("planted symlink", func(t *testing.T) {
		t.Parallel()

		cases := []struct {
			name  string
			files map[string]string
		}{
			{"to an empty directory", map[string]string{"sentinel.txt": "sentinel\n"}},
			{"to a directory holding the verified schema", map[string]string{"schema.json": string(content), "sentinel.txt": "sentinel\n"}},
			{"to a directory holding a forged schema", map[string]string{"schema.json": `{"title":"forged"}`, "sentinel.txt": "sentinel\n"}},
		}

		for _, tc := range cases {
			for _, command := range []string{"path", "cat"} {
				t.Run(command+" "+tc.name, func(t *testing.T) {
					t.Parallel()
					integrityPlantedSymlink(t, config, command, plain.Digest.String(), content, tc.files)
				})
			}
		}
	})

	t.Run("export through a symlinked parent", func(t *testing.T) {
		t.Parallel()

		sb := sandboxOf(t)
		cfg := writeConfig(t, sb.Workspace, config)
		target := writeFiles(t, t.TempDir(), map[string]string{"sentinel.txt": "sentinel\n"})
		link := filepath.Join(sb.Workspace, "linked")

		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}

		sandboxBefore := integrityFreeze(t, sb)

		cli(t, runOpts{}, "--config", cfg, "export", "plain", filepath.Join("linked", "exported.json")).ok(t)

		if got, want := fileTree(t, target), []string{"exported.json", "sentinel.txt"}; !slices.Equal(got, want) {
			t.Fatalf("the symlink's target holds %v, want %v", got, want)
		}

		exported := filepath.Join(target, "exported.json")

		info, err := os.Lstat(exported)
		if err != nil {
			t.Fatal(err)
		}

		if !info.Mode().IsRegular() || !bytes.Equal(readFile(t, exported), content) {
			t.Fatalf("exported file %s has mode %v; want a regular file with the schema", exported, info.Mode())
		}

		if dest, err := os.Readlink(link); err != nil || dest != target {
			t.Fatalf("the parent symlink changed: %q, %v", dest, err)
		}

		if diff := integritySnapshotDiff(sandboxBefore, integrityOutsideCache(t, sb)); len(diff) != 0 {
			t.Fatalf("the sandbox (workspace, TMPDIR, ...) changed outside the cache: %v", diff)
		}

		materialized := integrityOutputPath(cli(t, runOpts{}, "--config", cfg, "path", "plain").ok(t))
		if a, b := integrityStat(t, materialized), integrityStat(t, exported); os.SameFile(a, b) {
			t.Fatalf("the export %s is the cache file %s, not an independent copy", exported, materialized)
		}
	})
}

// integrityTitleSafe fetches a schema whose payload title tries to escape
// and checks that only .../<manifest hex>/schema.json appears, nothing
// changes outside the cache and no file named after the title exists.
func integrityTitleSafe(t *testing.T, config, id, title, manifestDigest string, want []byte, outside string) {
	t.Helper()

	sb := sandboxOf(t)
	cfg := writeConfig(t, sb.Workspace, config)
	final := integrityMaterializedPath(sb.CacheDir, manifestDigest)

	bases := []string{sb.Workspace, sb.CacheDir, filepath.Dir(final), filepath.Dir(filepath.Dir(final)), sb.Dir}
	candidates := integrityTitleTargets(title, bases)
	sandboxBefore := integrityFreeze(t, sb)
	outsideBefore := integritySnapshot(t, outside)
	existed := integrityExisting(candidates)

	res := cli(t, runOpts{}, "--config", cfg, "path", id).ok(t)
	if got := integrityOutputPath(res); got != final {
		t.Fatalf("path %s printed %s, want %s", id, got, final)
	}

	integrityVerifyFile(t, final, want)

	if cat := cli(t, runOpts{}, "--config", cfg, "cat", id).ok(t); !bytes.Equal(cat.Stdout, want) {
		t.Fatalf("cat %s differs from the pushed schema", id)
	}

	if entry := fileTree(t, filepath.Dir(final)); !slices.Equal(entry, []string{"schema.json"}) {
		t.Errorf("the materialized directory holds %v, want only schema.json", entry)
	}

	if diff := integritySnapshotDiff(sandboxBefore, integrityOutsideCache(t, sb)); len(diff) != 0 {
		t.Errorf("the sandbox changed outside the cache: %v", diff)
	}

	if diff := integritySnapshotDiff(outsideBefore, integritySnapshot(t, outside)); len(diff) != 0 {
		t.Errorf("the directory outside the cache changed: %v", diff)
	}

	if now := integrityExisting(candidates); !slices.Equal(now, existed) {
		t.Errorf("files named by the title appeared: %v (before: %v)", now, existed)
	}

	for _, p := range fileTree(t, filepath.Dir(sb.Dir)) {
		if strings.Contains(p, "escape") {
			t.Errorf("found %s below the test's temporary directory", p)
		}
	}
}

// integrityPlantedSymlink replaces the materialized directory of a schema
// with a symlink to a directory outside the cache before the first fetch and
// runs command (path or cat) for it. The outside directory must stay
// unchanged, modification times included, so that even a file created and
// removed again through the symlink shows; the client may repair the entry
// inside the cache or fail with a clear error naming it.
func integrityPlantedSymlink(t *testing.T, config, command, manifestDigest string, want []byte, files map[string]string) {
	t.Helper()

	sb := sandboxOf(t)
	cfg := writeConfig(t, sb.Workspace, config)
	outside := writeFiles(t, t.TempDir(), files)
	final := integrityMaterializedPath(sb.CacheDir, manifestDigest)
	entry := filepath.Dir(final)

	if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(outside, entry); err != nil {
		t.Fatal(err)
	}

	integrityBackdate(t, outside)

	before := integritySnapshot(t, outside)
	res := cli(t, runOpts{}, "--config", cfg, command, "plain")

	if diff := integritySnapshotDiff(before, integritySnapshot(t, outside)); len(diff) != 0 {
		t.Errorf("the symlink's target changed: %v", diff)
	}

	if res.Code != 0 {
		if len(res.Stdout) != 0 || !strings.Contains(string(res.Stderr), strings.TrimPrefix(manifestDigest, "sha256:")) {
			t.Fatalf("%s failed without a clear error naming the planted entry %s:\n%s", command, entry, res)
		}

		t.Logf("%s refused the planted symlink:\n%s", command, res.Stderr)

		return
	}

	wantOut := want
	if command == "path" {
		wantOut = []byte(final + "\n")
	}

	if !bytes.Equal(res.Stdout, wantOut) {
		t.Fatalf("%s printed %q, want %q", command, clip(res.Stdout), clip(wantOut))
	}

	info, err := os.Lstat(entry)
	if err != nil {
		t.Fatalf("the repaired entry: %v", err)
	}

	if !info.IsDir() {
		t.Fatalf("the repaired entry %s is %v, not a real directory", entry, info.Mode())
	}

	if resolved, err := filepath.EvalSymlinks(final); err != nil || resolved != final {
		t.Fatalf("%s resolves to %q (%v), not to itself inside the cache", final, resolved, err)
	}

	integrityVerifyFile(t, final, want)
}

// integrityPushCatalog pushes a catalog document with the entries sorted by
// ID and returns the catalog manifest descriptor and the document bytes.
func integrityPushCatalog(t *testing.T, reg *registry, path string, entries []catalogEntry) (ocispec.Descriptor, []byte) {
	t.Helper()

	sorted := slices.SortedFunc(slices.Values(entries), func(a, b catalogEntry) int { return strings.Compare(a.ID, b.ID) })

	doc, err := json.Marshal(catalogDoc{FormatVersion: catalogFormatVersion, Revision: "20260101.0000", Schemas: sorted})
	if err != nil {
		t.Fatal(err)
	}

	return pushRawCatalog(t, reg, path, doc), doc
}

// integrityCatalogOf fetches and decodes a published catalog directly from
// the registry.
func integrityCatalogOf(t *testing.T, reg *registry, path, catalogDigest string) catalogDoc {
	t.Helper()

	return reg.Catalog(t, path, catalogDigest).Doc
}

// integrityGzipFlood returns a gzip stream of prefix followed by
// integrityBombFlood copies of fill: about 1 MiB that inflates to 1 GiB.
func integrityGzipFlood(t *testing.T, prefix []byte, fill byte) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}

	chunk := bytes.Repeat([]byte{fill}, 1<<20)

	_, err = zw.Write(prefix)
	for i := 0; err == nil && i < integrityBombFlood/len(chunk); i++ {
		_, err = zw.Write(chunk)
	}

	if err != nil {
		t.Fatal(err)
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

// integrityUsage is what one schepherd run cost.
type integrityUsage struct {
	rss int64
	cpu time.Duration
	// peakDisk is the largest total size of the regular files below the
	// sandbox seen while the process ran and right after it exited.
	peakDisk int64
	samples  int
}

// integrityRun runs schepherd in sb and measures its peak resident memory,
// its CPU time and the peak disk usage of the sandbox.
func integrityRun(t *testing.T, sb *sandbox, args ...string) (result, integrityUsage) {
	t.Helper()

	done := make(chan struct{})
	disk := make(chan integrityUsage, 1)

	go func() {
		var u integrityUsage

		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()

		for {
			u.peakDisk = max(u.peakDisk, integrityDiskUsage(sb.Dir))
			u.samples++

			select {
			case <-done:
				u.peakDisk = max(u.peakDisk, integrityDiskUsage(sb.Dir))
				u.samples++
				disk <- u

				return
			case <-tick.C:
			}
		}
	}()

	p := startBin(t, "schepherd", runOpts{Sandbox: sb}, args...)
	res := p.Wait()

	close(done)

	use := <-disk

	if res.Err != nil {
		t.Fatalf("run: %v\n%s", res.Err, res)
	}

	ru, ok := p.cmd.ProcessState.SysUsage().(*syscall.Rusage)
	if !ok || ru.Maxrss <= 0 {
		t.Fatalf("no resource usage for %v: %#v", res.Args, p.cmd.ProcessState.SysUsage())
	}

	use.rss = ru.Maxrss << 10
	use.cpu = time.Duration(ru.Utime.Nano() + ru.Stime.Nano())

	return res, use
}

// integrityBombBounded checks what refusing the bomb h cost: the sandbox
// never held more than the payload blob and the declared content (plus
// slack), and the CPU time stayed below that of a cold fetch of the
// well-formed schema plus half of what inflating the whole bomb takes.
func integrityBombBounded(t *testing.T, config string, h integrityHostile, use integrityUsage, wellFormed string) {
	t.Helper()

	payload := h.layers[0]

	declared, err := strconv.ParseInt(payload.Annotations[annotationContentSize], 10, 64)
	if err != nil {
		t.Fatalf("declared content size of %s: %v", h.id, err)
	}

	if maxDisk := int64(len(payload.Data)) + declared + integrityDiskSlack; use.peakDisk > maxDisk {
		t.Errorf("the sandbox held up to %d bytes while path %s ran, want at most %d (payload %d, declared content %d)",
			use.peakDisk, h.id, maxDisk, len(payload.Data), declared)
	}

	inflate := integrityInflateCPU(t, payload.Data)

	other := newSandbox(t)
	res, base := integrityRun(t, other, "--config", writeConfig(t, other.Workspace, config), "path", wellFormed)
	res.ok(t)

	t.Logf("CPU: refusing %s %v, cold fetch of %s %v, inflating the whole bomb %v", h.id, use.cpu, wellFormed, base.cpu, inflate)

	if budget := base.cpu + inflate/2; use.cpu > budget {
		t.Errorf("path %s used %v of CPU, want at most %v: a cold fetch of %s used %v and inflating the whole bomb takes %v",
			h.id, use.cpu, budget, wellFormed, base.cpu, inflate)
	}
}

// integrityInflateCPU inflates a whole gzip payload on a locked thread and
// returns that thread's CPU time, the least a client inflating the bomb
// completely would spend. It fails unless the payload really is a bomb.
func integrityInflateCPU(t *testing.T, payload []byte) time.Duration {
	t.Helper()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	before := integrityThreadCPU(t)

	zr, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("inflate the bomb: %v", err)
	}

	n, err := io.Copy(io.Discard, zr)
	spent := integrityThreadCPU(t) - before

	if err != nil {
		t.Fatalf("inflate the bomb: %v", err)
	}

	if n < integrityBombFlood {
		t.Fatalf("the bomb inflates to %d bytes, want at least %d", n, integrityBombFlood)
	}

	return spent
}

func integrityThreadCPU(t *testing.T) time.Duration {
	t.Helper()

	var ru syscall.Rusage
	if err := syscall.Getrusage(integrityRusageThread, &ru); err != nil {
		t.Fatalf("getrusage: %v", err)
	}

	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func integrityCompact(t *testing.T, data []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func integrityLimit(key string, value int) string {
	return key + " = " + strconv.Itoa(value) + "\n"
}

func integrityOutputPath(res result) string {
	return strings.TrimSuffix(string(res.Stdout), "\n")
}

// integrityMaterializedPath is where the cache keeps the schema.json of a
// schema manifest (v1/materialized/sha256/<manifest hex>/schema.json).
func integrityMaterializedPath(cacheDir, manifestDigest string) string {
	return filepath.Join(cacheDir, "v1", "materialized", "sha256", strings.TrimPrefix(manifestDigest, "sha256:"), "schema.json")
}

// integrityMaterializedEntries lists everything below the materialized
// directory of a cache except the sha256 directory itself.
func integrityMaterializedEntries(t *testing.T, cacheDir string) []string {
	t.Helper()

	return slices.DeleteFunc(fileTree(t, filepath.Join(cacheDir, "v1", "materialized")), func(p string) bool { return p == "sha256/" })
}

// integrityDiskUsage sums the sizes of the regular files below root. It
// runs while a client creates and removes files, so entries that vanish
// during the walk are skipped.
func integrityDiskUsage(root string) int64 {
	var total int64

	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil //nolint:nilerr // a vanished entry holds no bytes
		}

		if info, err := d.Info(); err == nil {
			total += info.Size()
		}

		return nil
	})

	return total
}

// integritySnapshot maps every entry below root (and root itself) to its
// type and: for directories, mode and modification time; for files, mode,
// digest and modification time; for symlinks, the target. With the tree
// backdated first (integrityBackdate), a file created and removed again, or
// renamed in or out of a directory, still shows as a new directory mtime.
func integritySnapshot(t *testing.T, root string) map[string]string {
	t.Helper()

	out := map[string]string{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		info, err := os.Lstat(path)
		if err != nil {
			return err
		}

		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}

			out[rel] = "symlink " + target
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}

			out[rel] = fmt.Sprintf("file %v %s %s", info.Mode(), digestOf(data), info.ModTime().Format(time.RFC3339Nano))
		default:
			out[rel] = fmt.Sprintf("%v %s", info.Mode(), info.ModTime().Format(time.RFC3339Nano))
		}

		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}

	return out
}

// integrityBackdate sets the access and modification times of every
// directory and regular file below root (and root itself) to one long
// past, so that any later change shows in integritySnapshot.
func integrityBackdate(t *testing.T, root string) {
	t.Helper()

	past := time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink != 0 {
			return err
		}

		return os.Chtimes(path, past, past)
	})
	if err != nil {
		t.Fatalf("backdate %s: %v", root, err)
	}
}

// integrityFreeze creates the parent of the cache directory (as on a system
// where $XDG_CACHE_HOME exists), so that creating the cache changes no
// directory outside it, backdates the whole sandbox and returns its
// snapshot without the cache.
func integrityFreeze(t *testing.T, sb *sandbox) map[string]string {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(sb.CacheDir), 0o700); err != nil {
		t.Fatal(err)
	}

	integrityBackdate(t, sb.Dir)

	return integrityOutsideCache(t, sb)
}

// integritySnapshotDiff lists the entries that differ between two
// snapshots, with their states before and after.
func integritySnapshotDiff(before, after map[string]string) []string {
	keys := append(slices.Collect(maps.Keys(before)), slices.Collect(maps.Keys(after))...)
	slices.Sort(keys)

	var out []string

	for _, rel := range slices.Compact(keys) {
		if b, a := before[rel], after[rel]; b != a {
			out = append(out, fmt.Sprintf("%s: %q -> %q", rel, b, a))
		}
	}

	return out
}

// integrityOutsideCache snapshots a sandbox without the cache and its
// parent directory, which the client creates or writes into.
func integrityOutsideCache(t *testing.T, sb *sandbox) map[string]string {
	t.Helper()

	cacheParent := filepath.Dir(sb.CacheDir)
	snap := integritySnapshot(t, sb.Dir)

	maps.DeleteFunc(snap, func(rel, _ string) bool {
		abs := filepath.Join(sb.Dir, rel)

		return abs == cacheParent || abs == sb.CacheDir || strings.HasPrefix(abs, sb.CacheDir+string(filepath.Separator))
	})

	return snap
}

// integrityTitleTargets lists where a client that used the title (or the
// title without a .gz suffix) as a file name relative to one of bases, or as
// an absolute path, would have written.
func integrityTitleTargets(title string, bases []string) []string {
	names := []string{title}
	if trimmed, ok := strings.CutSuffix(title, ".gz"); ok {
		names = append(names, trimmed)
	}

	var out []string

	for _, name := range names {
		if filepath.IsAbs(name) {
			out = append(out, filepath.Clean(name))

			continue
		}

		for _, base := range bases {
			out = append(out, filepath.Join(base, name))
		}
	}

	slices.Sort(out)

	return slices.Compact(out)
}

// integrityExisting returns the paths that exist (as anything, including a
// dangling symlink).
func integrityExisting(paths []string) []string {
	var out []string

	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			out = append(out, p)
		}
	}

	return out
}

func integrityStat(t *testing.T, path string) os.FileInfo {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	return info
}

// integrityCount counts the records of one request class whose path ends
// with ref.
func integrityCount(records []regproxy.Record, class, ref string) int {
	n := 0

	for _, r := range records {
		if r.Class == class && strings.HasSuffix(r.Path, "/"+ref) {
			n++
		}
	}

	return n
}
