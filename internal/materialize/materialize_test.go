package materialize

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/store"
)

// remote serves the packed artifacts from memory and counts requests.
type remote struct {
	blobs    map[string][]byte
	requests atomic.Int64
}

func (r *remote) FetchManifestByDigest(_ context.Context, dgst, _ string, _ int64) ([]byte, error) {
	r.requests.Add(1)

	data, ok := r.blobs[dgst]
	if !ok {
		return nil, fault.New(fault.Registry, "%s not found", dgst)
	}

	return data, nil
}

func (r *remote) FetchTo(_ context.Context, desc ocispec.Descriptor, w io.Writer) error {
	r.requests.Add(1)

	data, ok := r.blobs[desc.Digest.String()]
	if !ok {
		return fault.New(fault.Registry, "%s not found", desc.Digest)
	}

	_, err := w.Write(data)

	return err
}

// fixture is a small published catalog: two schemas, one shared manifest
// (two entries point at it), one notice.
type fixture struct {
	rem       *remote
	digest    string
	entries   int
	schemas   int // distinct manifests
	notices   int // distinct notice blobs
	content   map[string][]byte
	manifests map[string]*catalog.Entry // manifest digest -> representative
}

// packedItem is one published schema.
type packedItem struct {
	p *artifact.Packed
	e catalog.Entry
}

func newFixture(t *testing.T, sharedManifest bool) *fixture {
	t.Helper()

	f := &fixture{
		rem:       &remote{blobs: map[string][]byte{}},
		content:   map[string][]byte{},
		manifests: map[string]*catalog.Entry{},
	}

	var all []packedItem

	add := func(id string, notice []byte) {
		t.Helper()

		body := []byte(`{"type":"object","title":"` + id + `"}`)
		f.content[id] = body

		p, err := artifact.PackSchema(body, notice)
		if err != nil {
			t.Fatal(err)
		}

		f.rem.blobs[p.Manifest.Descriptor.Digest.String()] = p.Manifest.Data
		for _, b := range p.Blobs {
			f.rem.blobs[b.Descriptor.Digest.String()] = b.Data
		}

		all = append(all, packedItem{p: p, e: catalog.Entry{
			ID:       id,
			Name:     id + ".json",
			Artifact: catalog.Descriptor{MediaType: artifact.ManifestMediaType, Digest: p.Manifest.Descriptor.Digest.String(), Size: p.Manifest.Descriptor.Size},
		}})
	}

	notices := 0
	if sharedManifest {
		// delta and echo share one manifest: pack once from delta's body and
		// let echo's entry point at the same descriptor.
		add("delta", []byte("Delta notice. MIT License.\n"))
		notices++

		echo := all[0].e
		echo.ID = "echo"
		all = append(all, packedItem{p: all[0].p, e: echo})
		// The catalog index lists each manifest once; the shared manifest is
		// deduplicated when packing the catalog (descriptors are compacted).
	} else {
		add("alpha", nil)
		add("gamma", []byte("Gamma notice. MIT License.\n"))
		notices++
	}

	f.entries = len(all)
	f.schemas = len(distinctManifests(all))
	f.notices = notices

	entries := make([]catalog.Entry, 0, len(all))
	seen := map[string]struct{}{}
	var descs []ocispec.Descriptor
	for _, p := range all {
		entries = append(entries, p.e)
		f.manifests[p.e.Artifact.Digest] = &entries[len(entries)-1]
		if _, dup := seen[p.e.Artifact.Digest]; dup {
			continue
		}
		seen[p.e.Artifact.Digest] = struct{}{}
		descs = append(descs, p.p.Manifest.Descriptor)
	}

	raw, err := catalog.Marshal(&catalog.Catalog{FormatVersion: catalog.FormatVersion, Revision: "20260924.1200", Schemas: entries})
	if err != nil {
		t.Fatal(err)
	}

	cat, err := artifact.PackCatalog(raw, descs)
	if err != nil {
		t.Fatal(err)
	}

	f.rem.blobs[cat.Metadata.Manifest.Descriptor.Digest.String()] = cat.Metadata.Manifest.Data
	for _, b := range cat.Metadata.Blobs {
		f.rem.blobs[b.Descriptor.Digest.String()] = b.Data
	}
	f.rem.blobs[cat.Index.Descriptor.Digest.String()] = cat.Index.Data

	f.digest = cat.Index.Descriptor.Digest.String()

	return f
}

// store opens a store over a fresh cache dir.
func (f *fixture) store(t *testing.T, dir string, offline bool) *store.Store {
	t.Helper()

	c, err := cache.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = c.Close() })

	return store.New(store.Options{
		Cache:          c,
		Repository:     "registry.example/org/schemas",
		Offline:        offline,
		ArtifactLimits: artifact.DefaultLimits(),
		CatalogLimits:  catalog.DefaultLimits(),
		Open: func() (store.Remote, error) {
			return f.rem, nil
		},
	})
}

func TestColdRunCompletesAndFetchesEverything(t *testing.T) {
	f := newFixture(t, true)

	st := f.store(t, t.TempDir(), false)

	lc, err := st.Catalog(t.Context(), f.digest)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Run(t.Context(), st, lc, Options{})
	if err != nil {
		t.Fatal(err)
	}

	if !res.Complete || len(res.Missing) != 0 {
		t.Fatalf("cold run = %+v, want complete", res)
	}

	if res.Entries != 2 || res.Schemas != 1 {
		t.Fatalf("entries=%d schemas=%d, want 2 entries sharing 1 manifest", res.Entries, res.Schemas)
	}

	if res.Notices != 1 {
		t.Fatalf("notices=%d, want 1", res.Notices)
	}

	if res.WrittenSchemaFiles != 1 {
		t.Fatalf("writtenSchemaFiles=%d, want 1", res.WrittenSchemaFiles)
	}
}

// distinctManifests counts the distinct schema manifests of the packed set.
func distinctManifests(all []packedItem) map[string]struct{} {
	seen := map[string]struct{}{}
	for _, p := range all {
		seen[p.e.Artifact.Digest] = struct{}{}
	}
	return seen
}

func TestWarmRunFetchesNothing(t *testing.T) {
	f := newFixture(t, false)
	dir := t.TempDir()

	st := f.store(t, dir, false)

	lc, err := st.Catalog(t.Context(), f.digest)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Run(t.Context(), st, lc, Options{}); err != nil {
		t.Fatal(err)
	}

	before := f.rem.requests.Load()

	st2 := f.store(t, dir, false)
	lc2, err := st2.Catalog(t.Context(), f.digest)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Run(t.Context(), st2, lc2, Options{})
	if err != nil {
		t.Fatal(err)
	}

	if after := f.rem.requests.Load(); after != before {
		t.Fatalf("warm run sent %d registry request(s)", after-before)
	}

	if res.FetchedManifests != 0 || res.FetchedBlobs != 0 || res.WrittenSchemaFiles != 0 {
		t.Fatalf("warm run = %+v, want everything cached", res)
	}

	if !res.Complete {
		t.Fatal("warm run is not complete")
	}
}

func TestOfflineVerificationReportsMissing(t *testing.T) {
	f := newFixture(t, false)
	dir := t.TempDir()

	st := f.store(t, dir, false)

	lc, err := st.Catalog(t.Context(), f.digest)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Run(t.Context(), st, lc, Options{}); err != nil {
		t.Fatal(err)
	}

	// Break one notice blob; offline verification must name exactly it.
	noticePath := ""

	// Find the notice blob by scanning for its known content.
	const noticeText = "Gamma notice. MIT License.\n"
	if err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}

		data, rerr := os.ReadFile(p)
		if rerr == nil && string(data) == noticeText {
			noticePath = p

			return filepath.SkipAll
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if noticePath == "" {
		t.Fatal("notice blob not found in the cache")
	}

	if err := os.Remove(noticePath); err != nil {
		t.Fatal(err)
	}

	before := f.rem.requests.Load()

	sto := f.store(t, dir, true)

	lco, err := sto.Catalog(t.Context(), f.digest)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Run(t.Context(), sto, lco, Options{})
	if err != nil {
		t.Fatalf("offline verification run: %v", err)
	}

	if after := f.rem.requests.Load(); after != before {
		t.Fatalf("offline verification sent %d registry request(s)", after-before)
	}

	if res.Complete || len(res.Missing) != 1 {
		t.Fatalf("offline verification = %+v, want exactly one missing entry", res)
	}

	m := res.Missing[0]
	if m.Kind != "notice" || m.State != "absent" {
		t.Fatalf("missing entry = %+v, want an absent notice", m)
	}

	if !errors.Is(err, nil) || res.Complete {
		t.Fatal("expected incomplete result")
	}
}

func TestOfflineRefusesEmptyCache(t *testing.T) {
	f := newFixture(t, false)
	dir := t.TempDir()

	// Populate only the catalog: load it online, then delete everything but
	// the catalog blobs by running offline verification on a store whose
	// cache was emptied — simpler: fresh dir, offline store, load catalog
	// online first into the same dir, then remove materialized trees.
	st := f.store(t, dir, false)

	if _, err := st.Catalog(t.Context(), f.digest); err != nil {
		t.Fatal(err)
	}

	sto := f.store(t, dir, true)

	lc, err := sto.Catalog(t.Context(), f.digest)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Run(t.Context(), sto, lc, Options{})
	if err != nil {
		t.Fatal(err)
	}

	if res.Complete || len(res.Missing) != 2 {
		t.Fatalf("offline verification of an empty schema cache = %+v (missing %d), want 2 missing manifests", res, len(res.Missing))
	}

	for _, m := range res.Missing {
		if m.Kind != "manifest" || m.State != "absent" {
			t.Fatalf("missing entry = %+v, want absent manifests", m)
		}
	}
}
