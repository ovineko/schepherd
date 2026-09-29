//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/schepherd/tests/e2e/regproxy"
)

const (
	perfSchemas = 1000
	perfTimeout = 20 * time.Minute
)

// perfReport is the JSON document a TestPerf_* test writes to the artifacts
// directory (perf/<test>.json, with the same numbers as a Markdown table in
// perf/<test>.md). It holds measurements only; nothing in it is asserted
// against the wall clock.
type perfReport struct {
	Test          string          `json:"test"`
	Platform      string          `json:"platform"`
	Schemas       int             `json:"schemas"`
	StartedAt     time.Time       `json:"startedAt"`
	CatalogDigest string          `json:"catalogDigest"`
	CatalogBytes  int             `json:"catalogBytes"`
	Steps         []perfStep      `json:"steps"`
	Publish       json.RawMessage `json:"publish,omitempty"`
	Mirror        json.RawMessage `json:"mirror,omitempty"`
	MirrorNoop    json.RawMessage `json:"mirrorNoop,omitempty"`
}

// perfStep is one measured command. Traffic is keyed by the registry the
// proxy stands in front of ("source", "mirror"); CacheBytes is the size of
// the client's cache directory after the step (absent for steps that use
// none).
type perfStep struct {
	Name        string                 `json:"name"`
	Command     []string               `json:"command"`
	DurationMs  float64                `json:"durationMs"`
	ExitCode    int                    `json:"exitCode"`
	StdoutBytes int                    `json:"stdoutBytes"`
	CacheBytes  *int64                 `json:"cacheBytes,omitempty"`
	Traffic     map[string]perfTraffic `json:"traffic,omitempty"`
}

// perfTraffic is what one proxy saw during a step: RequestBytes were sent to
// the registry (uploads), ResponseBytes came back (downloads), BlobBytesOut
// is the part of ResponseBytes that were blob bodies.
type perfTraffic struct {
	Requests      int            `json:"requests"`
	ByClass       map[string]int `json:"byClass"`
	RequestBytes  int64          `json:"requestBytes"`
	ResponseBytes int64          `json:"responseBytes"`
	BlobBytesOut  int64          `json:"blobBytesOut"`
}

type perfMirrorResult struct {
	CatalogDigest   string `json:"catalogDigest"`
	Schemas         int    `json:"schemas"`
	CopiedManifests int    `json:"copiedManifests"`
	CopiedBlobs     int    `json:"copiedBlobs"`
}

// TestPerf_SyntheticCatalog publishes a synthetic local source of
// perfSchemas small schemas with the real publisher and measures the
// client's catalog, discovery, path and mirror operations through private
// proxies. It writes perf/<test>.json to the artifacts directory and runs
// only when E2E_PERF asks for it.
func TestPerf_SyntheticCatalog(t *testing.T) {
	if !perfEnabled() {
		t.Logf("%s is off; set %s=1 to run the measurements", envPerf, envPerf)

		return
	}

	src := newProxy(t, suite.source)
	dst := newProxy(t, suite.mirror)
	srcRepo := src.Repo(repoPath(t))
	dstRepo := dst.Repo(repoPath(t, "mirror"))
	sourceOnly := map[string]*proxy{"source": src}
	both := map[string]*proxy{"source": src, "mirror": dst}

	rep := &perfReport{
		Test:      t.Name(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		Schemas:   perfSchemas,
		StartedAt: time.Now().UTC(),
	}

	t.Cleanup(func() { perfWriteReport(t, rep) })

	set := perfWriteSet(t, perfSchemas)
	prepared := filepath.Join(t.TempDir(), "prepared")

	rep.run(t, "prepare", nil, "", func() result {
		return publisher(t, runOpts{Timeout: perfTimeout}, "prepare", "--source", filepath.Join(set, "source.toml"),
			"--out", prepared, "--jsonschema", suite.jsonschema, "--json")
	})

	pub := rep.run(t, "publish", sourceOnly, "", func() result {
		return publisher(t, runOpts{Timeout: perfTimeout}, "publish", "--prepared", prepared, "--repository", srcRepo,
			"--registry-config", publisherRegistryConfig(t), "--now", "20260101.0000", "--json")
	})

	published := decodeJSON[publishResult](t, pub.Stdout)
	rep.Publish = json.RawMessage(pub.Stdout)
	rep.CatalogDigest = published.CatalogDigest

	if published.Status != "published" || published.UploadedSchemas != perfSchemas {
		t.Fatalf("publish did not push %d schemas:\n%s", perfSchemas, pub.Stdout)
	}

	cache := sandboxOf(t).CacheDir
	cfg := writeConfig(t, sandboxOf(t).Workspace, clientConfig{Repository: srcRepo, Catalog: published.CatalogDigest}.TOML())
	client := func(args ...string) func() result {
		return func() result {
			return cli(t, runOpts{Timeout: perfTimeout}, append([]string{"--config", cfg}, args...)...)
		}
	}

	catalog := rep.run(t, "catalog (cold cache)", sourceOnly, cache, client("catalog", "--json"))
	rep.CatalogBytes = len(catalog.Stdout)

	doc := perfCheckCatalog(t, repoPath(t), published, catalog.Stdout)

	list := rep.run(t, "list (catalog cached)", sourceOnly, cache, client("list", "--json"))
	perfCheckList(t, doc, list.Stdout)

	patterns := rep.run(t, "patterns (catalog cached)", sourceOnly, cache, client("patterns", "--json"))
	perfCheckPatterns(t, doc, patterns.Stdout)

	target := perfID(perfSchemas / 2)
	resolveFile := "deploy/" + target + ".json"

	resolved := rep.run(t, "resolve (catalog cached)", sourceOnly, cache, client("resolve", "--file", resolveFile, "--json"))
	got := decodeJSON[perfResolution](t, resolved.Stdout)
	if want := (perfResolution{File: got.File, Path: resolveFile, Schema: target, Origin: "catalog", Artifact: doc.entry(t, target).Artifact}); got.File == "" || got != want {
		t.Fatalf("resolve --file %s --json:\n%s\nwant %+v with the absolute file", resolveFile, resolved.Stdout, want)
	}

	cold := rep.run(t, "path (catalog cached, schema cold)", sourceOnly, cache, client("path", target))
	warm := rep.run(t, "path (warm)", sourceOnly, cache, client("path", target))

	if string(cold.Stdout) != string(warm.Stdout) {
		t.Fatalf("warm path %q differs from cold path %q", warm.Stdout, cold.Stdout)
	}

	if n := rep.last().Traffic["source"].Requests; n != 0 {
		t.Fatalf("warm path made %d requests", n)
	}

	perfCheckMaterialized(t, prepared, target, cold.Stdout)

	empty := newSandbox(t)
	emptyCfg := writeConfig(t, empty.Workspace, clientConfig{Repository: srcRepo, Catalog: published.CatalogDigest}.TOML())
	last := perfID(perfSchemas - 1)
	fresh := rep.run(t, "path (empty cache)", sourceOnly, empty.CacheDir, func() result {
		return cli(t, runOpts{Sandbox: empty, Timeout: perfTimeout}, "--config", emptyCfg, "path", last)
	})

	perfCheckMaterialized(t, prepared, last, fresh.Stdout)

	mirrorArgs := []string{"mirror", srcRepo + "@" + published.CatalogDigest, dstRepo, "--json"}

	first := rep.run(t, "mirror", both, cache, client(mirrorArgs...))
	rep.Mirror = json.RawMessage(first.Stdout)

	if m := decodeJSON[perfMirrorResult](t, first.Stdout); m.CatalogDigest != published.CatalogDigest || m.Schemas != perfSchemas {
		t.Fatalf("first mirror did not copy the snapshot of %d schemas:\n%s", perfSchemas, first.Stdout)
	}

	perfCheckMirrorTags(t, repoPath(t, "mirror"), published, doc)

	second := rep.run(t, "mirror (repeated, no-op)", both, cache, client(mirrorArgs...))
	rep.MirrorNoop = json.RawMessage(second.Stdout)

	if m := decodeJSON[perfMirrorResult](t, second.Stdout); m.CopiedManifests != 0 || m.CopiedBlobs != 0 {
		t.Fatalf("repeated mirror copied content:\n%s", second.Stdout)
	}

	if byClass := rep.last().Traffic["mirror"].ByClass; byClass[regproxy.ClassBlobUpload]+byClass[regproxy.ClassManifestPut] != 0 {
		t.Fatalf("repeated mirror wrote to the destination: %v", byClass)
	}
}

type perfResolution struct {
	File     string        `json:"file"`
	Path     string        `json:"path"`
	Schema   string        `json:"schema"`
	Origin   string        `json:"origin"`
	Artifact descriptorDoc `json:"artifact"`
}

type perfListItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Dialect     string `json:"dialect,omitempty"`
	Digest      string `json:"digest"`
}

type perfPatternItem struct {
	ID        string        `json:"id"`
	FileMatch []string      `json:"fileMatch"`
	Artifact  descriptorDoc `json:"artifact"`
}

// perfCheckCatalog asserts that `catalog --json` printed the published
// catalog blob byte for byte and that it lists the synthetic schemas in ID
// order, and returns the decoded catalog.
func perfCheckCatalog(t *testing.T, path string, published publishResult, stdout []byte) catalogDoc {
	t.Helper()

	if blob := suite.source.Catalog(t, path, published.CatalogDigest).Blob; !bytes.Equal(stdout, blob) {
		t.Fatalf("catalog --json (%d bytes) differs from the published catalog blob (%d bytes)", len(stdout), len(blob))
	}

	doc := decodeJSON[catalogDoc](t, stdout)
	if doc.Revision != published.Revision || len(doc.Schemas) != perfSchemas {
		t.Fatalf("catalog revision %q with %d schemas, want %q with %d", doc.Revision, len(doc.Schemas), published.Revision, perfSchemas)
	}

	for i, e := range doc.Schemas {
		if e.ID != perfID(i) || !digestPattern.MatchString(e.Artifact.Digest) {
			t.Fatalf("catalog entry %d: %+v, want %s with a manifest digest", i, e, perfID(i))
		}
	}

	return doc
}

// perfCheckList asserts `list --json`: one item per catalog entry with its
// name and manifest digest.
func perfCheckList(t *testing.T, doc catalogDoc, stdout []byte) {
	t.Helper()

	items := decodeJSON[[]perfListItem](t, stdout)
	if len(items) != len(doc.Schemas) {
		t.Fatalf("list returned %d schemas, want %d", len(items), len(doc.Schemas))
	}

	got := map[string]perfListItem{}
	for _, it := range items {
		got[it.ID] = it
	}

	for _, e := range doc.Schemas {
		want := perfListItem{ID: e.ID, Name: e.Name, Description: e.Description, Dialect: e.Dialect, Digest: e.Artifact.Digest}
		if got[e.ID] != want {
			t.Fatalf("list item %+v, want %+v", got[e.ID], want)
		}
	}
}

// perfCheckPatterns asserts `patterns --json`: one item per catalog entry
// (every synthetic schema has fileMatch) with the catalog's patterns and
// descriptor.
func perfCheckPatterns(t *testing.T, doc catalogDoc, stdout []byte) {
	t.Helper()

	items := decodeJSON[[]perfPatternItem](t, stdout)
	if len(items) != len(doc.Schemas) {
		t.Fatalf("patterns returned %d items, want %d", len(items), len(doc.Schemas))
	}

	got := map[string]perfPatternItem{}
	for _, it := range items {
		got[it.ID] = it
	}

	for _, e := range doc.Schemas {
		want := []string{e.ID + ".json", "**/" + e.ID + "/*.yaml"}
		if it := got[e.ID]; !slices.Equal(e.FileMatch, want) || !slices.Equal(it.FileMatch, want) || it.Artifact != e.Artifact {
			t.Fatalf("patterns item %+v for the catalog entry %+v, want fileMatch %v", it, e, want)
		}
	}
}

// perfCheckMaterialized asserts that the file printed by `path id` holds
// the prepared schema.
func perfCheckMaterialized(t *testing.T, prepared, id string, stdout []byte) {
	t.Helper()

	file := strings.TrimSuffix(string(stdout), "\n")
	if file == "" {
		t.Fatalf("path %s printed nothing", id)
	}

	if !bytes.Equal(readFile(t, file), preparedSchema(t, prepared, id)) {
		t.Fatalf("path %s: %s differs from the prepared schema", id, file)
	}
}

// perfCheckMirrorTags asserts that the destination holds exactly the
// revision tag of the mirrored snapshot, pointing at its index, and that the
// index there references every schema manifest the catalog lists.
func perfCheckMirrorTags(t *testing.T, path string, published publishResult, doc catalogDoc) {
	t.Helper()

	if got, want := suite.mirror.Tags(t, path), []string{"catalog-" + published.Revision}; !slices.Equal(got, want) {
		t.Fatalf("the mirror has the tags %v, want exactly %v", got, want)
	}

	c := suite.mirror.Catalog(t, path, "catalog-"+published.Revision)
	if c.Index.Digest != published.CatalogDigest {
		t.Fatalf("mirror tag catalog-%s points to %s, want %s", published.Revision, c.Index.Digest, published.CatalogDigest)
	}

	children := map[string]bool{}
	for _, d := range c.schemaChildren() {
		children[d.Digest.String()] = true
	}

	for _, e := range doc.Schemas {
		if !children[e.Artifact.Digest] {
			t.Errorf("the mirrored index does not reference %s (%s)", e.Artifact.Digest, e.ID)
		}
	}
}

// run resets the proxies, runs one command that must succeed and records
// its duration, output size, the traffic every proxy saw and, when cache is
// set, the size of that cache directory afterwards.
func (r *perfReport) run(t *testing.T, name string, proxies map[string]*proxy, cache string, cmd func() result) result {
	t.Helper()

	for _, p := range proxies {
		p.Reset(t)
	}

	res := cmd()
	step := perfStep{
		Name:        name,
		Command:     res.Args,
		DurationMs:  float64(res.Duration.Microseconds()) / 1000,
		ExitCode:    res.Code,
		StdoutBytes: len(res.Stdout),
	}

	if len(proxies) > 0 {
		step.Traffic = map[string]perfTraffic{}
	}

	for label, p := range proxies {
		stats := p.Stats(t)
		traffic := perfTraffic{Requests: stats.Total, ByClass: stats.ByClass, BlobBytesOut: stats.BlobBytesOut}

		for _, rec := range p.Records(t) {
			traffic.RequestBytes += rec.ReqBytes
			traffic.ResponseBytes += rec.RespBytes
		}

		step.Traffic[label] = traffic
	}

	if cache != "" {
		size := perfDirSize(t, cache)
		step.CacheBytes = &size
	}

	r.Steps = append(r.Steps, step)
	t.Logf("%s: %.1f ms, exit %d, %d stdout bytes, traffic %+v", name, step.DurationMs, res.Code, len(res.Stdout), step.Traffic)

	return res.ok(t)
}

func (r *perfReport) last() perfStep {
	return r.Steps[len(r.Steps)-1]
}

func perfWriteReport(t *testing.T, rep *perfReport) {
	t.Helper()

	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Errorf("encode measurements: %v", err)

		return
	}

	path, err := writeArtifact(filepath.Join("perf", t.Name()+".json"), append(data, '\n'))
	if err != nil {
		t.Errorf("write measurements: %v", err)

		return
	}

	table, err := writeArtifact(filepath.Join("perf", t.Name()+".md"), []byte(rep.markdown()))
	if err != nil {
		t.Errorf("write measurements: %v", err)

		return
	}

	t.Logf("measurements written to %s and %s", path, table)
}

// markdown renders the measurements as a table with one row per step and
// registry: the bytes sent to and received from each registry are the
// transfer sizes of that step.
func (r *perfReport) markdown() string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s\n\n%d schemas on %s, started %s; catalog %s, %d bytes.\n\n",
		r.Test, r.Schemas, r.Platform, r.StartedAt.Format(time.RFC3339), r.CatalogDigest, r.CatalogBytes)
	b.WriteString("| Step | Duration (ms) | Registry | Requests | Bytes sent | Bytes received | Blob bytes received | Cache bytes after |\n")
	b.WriteString("| --- | ---: | --- | ---: | ---: | ---: | ---: | ---: |\n")

	for _, s := range r.Steps {
		cache := "-"
		if s.CacheBytes != nil {
			cache = strconv.FormatInt(*s.CacheBytes, 10)
		}

		if len(s.Traffic) == 0 {
			fmt.Fprintf(&b, "| %s | %.1f | - | - | - | - | - | %s |\n", s.Name, s.DurationMs, cache)

			continue
		}

		for _, label := range slices.Sorted(maps.Keys(s.Traffic)) {
			tr := s.Traffic[label]
			fmt.Fprintf(&b, "| %s | %.1f | %s | %d | %d | %d | %d | %s |\n",
				s.Name, s.DurationMs, label, tr.Requests, tr.RequestBytes, tr.ResponseBytes, tr.BlobBytesOut, cache)
		}
	}

	return b.String()
}

// perfDirSize returns the total size of the regular files below dir, 0 when
// dir does not exist.
func perfDirSize(t *testing.T, dir string) int64 {
	t.Helper()

	var size int64

	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		size += info.Size()

		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("size of %s: %v", dir, err)
	}

	return size
}

func perfID(i int) string {
	return fmt.Sprintf("perf-%04d", i)
}

// perfWriteSet writes a local source with n small, distinct schemas (each
// with two fileMatch patterns) and the fixture license policy, and returns
// its directory.
func perfWriteSet(t *testing.T, n int) string {
	t.Helper()

	dir := t.TempDir()

	var src strings.Builder

	src.WriteString("kind = \"local\"\nname = \"e2e-perf\"\npolicy = \"licenses.toml\"\n")

	for i := range n {
		id := perfID(i)
		fmt.Fprintf(&src, "\n[[entries]]\nid = %q\nname = %q\ndescription = %q\nurl = %q\nfile = %q\nfile_match = [%q, %q]\n",
			id, id+".json", fmt.Sprintf("Synthetic schema %d of %d", i+1, n),
			"https://schemas.example.com/e2e/perf/"+id+".json", "schemas/"+id+".json",
			id+".json", "**/"+id+"/*.yaml")

		schema := fmt.Sprintf(`{"$schema":"https://json-schema.org/draft/2020-12/schema","title":"Perf %d",`+
			`"type":"object","required":["index"],"properties":{"index":{"const":%d},"name":{"type":"string","maxLength":%d}}}`,
			i, i, 16+i)
		writeFile(t, filepath.Join(dir, "schemas", id+".json"), []byte(schema+"\n"))
	}

	writeFile(t, filepath.Join(dir, "source.toml"), []byte(src.String()))
	writeFile(t, filepath.Join(dir, "licenses.toml"), readFile(t, fixture("set-basic", "licenses.toml")))

	return dir
}

// TestHarness_PerfReport checks the measurement documents of TestPerf_*
// without running the measurements: each step keeps the bytes sent to and
// received from every registry and the cache size after it, in the JSON and
// in the Markdown table.
func TestHarness_PerfReport(t *testing.T) {
	t.Parallel()

	cacheDir := t.TempDir()
	writeFile(t, filepath.Join(cacheDir, "a", "blob"), bytes.Repeat([]byte("x"), 1000))
	writeFile(t, filepath.Join(cacheDir, "b"), []byte("12345"))

	size := perfDirSize(t, cacheDir)
	if size != 1005 {
		t.Fatalf("perfDirSize = %d, want 1005", size)
	}

	if missing := perfDirSize(t, filepath.Join(cacheDir, "missing")); missing != 0 {
		t.Fatalf("perfDirSize of a missing directory = %d", missing)
	}

	rep := perfReport{Test: "TestPerf_X", Schemas: 2, Platform: "linux/amd64", CatalogDigest: "sha256:c", CatalogBytes: 42, Steps: []perfStep{
		{Name: "prepare", DurationMs: 1.5},
		{Name: "path (catalog cached, schema cold)", DurationMs: 2, CacheBytes: &size, Traffic: map[string]perfTraffic{
			"source": {Requests: 2, RequestBytes: 300, ResponseBytes: 900, BlobBytesOut: 700},
		}},
		{Name: "mirror", DurationMs: 3, Traffic: map[string]perfTraffic{
			"source": {Requests: 4, RequestBytes: 400, ResponseBytes: 5000, BlobBytesOut: 4000},
			"mirror": {Requests: 6, RequestBytes: 4800, ResponseBytes: 600},
		}},
	}}

	md := rep.markdown()
	for _, line := range []string{
		"| prepare | 1.5 | - | - | - | - | - | - |",
		"| path (catalog cached, schema cold) | 2.0 | source | 2 | 300 | 900 | 700 | 1005 |",
		"| mirror | 3.0 | mirror | 6 | 4800 | 600 | 0 | - |",
		"| mirror | 3.0 | source | 4 | 400 | 5000 | 4000 | - |",
	} {
		if !strings.Contains(md, line+"\n") {
			t.Errorf("the table lacks %q:\n%s", line, md)
		}
	}

	data, err := json.Marshal(rep.Steps[1])
	if err != nil {
		t.Fatal(err)
	}

	for _, field := range []string{`"cacheBytes":1005`, `"requestBytes":300`, `"responseBytes":900`, `"blobBytesOut":700`} {
		if !bytes.Contains(data, []byte(field)) {
			t.Errorf("the JSON step lacks %s: %s", field, data)
		}
	}
}
