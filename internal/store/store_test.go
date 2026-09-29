package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

type fakeRemote struct {
	blobs    map[string][]byte
	tamper   map[string][]byte
	cutOff   map[string]int
	requests []string
	mu       sync.Mutex
}

func (f *fakeRemote) FetchManifestByDigest(_ context.Context, dgst, _ string, limit int64) ([]byte, error) {
	f.record("manifest " + dgst)

	data, ok := f.blobs[dgst]
	if !ok {
		return nil, fault.New(fault.Registry, "%s not found (404)", dgst)
	}

	if int64(len(data)) > limit {
		return nil, fault.New(fault.Integrity, "too large")
	}

	return data, nil
}

func (f *fakeRemote) FetchTo(_ context.Context, desc ocispec.Descriptor, w io.Writer) error {
	f.record("fetch " + desc.Digest.String())

	if bad, ok := f.tamper[desc.Digest.String()]; ok {
		_, err := w.Write(bad)

		return err
	}

	f.mu.Lock()
	cut := f.cutOff[desc.Digest.String()] > 0
	if cut {
		f.cutOff[desc.Digest.String()]--
	}
	f.mu.Unlock()

	if cut {
		data := f.blobs[desc.Digest.String()]
		_, _ = w.Write(data[:len(data)/2])

		return fault.Wrap(fault.Registry, io.ErrUnexpectedEOF, "fetch %s", desc.Digest)
	}

	data, ok := f.blobs[desc.Digest.String()]
	if !ok {
		return fault.New(fault.Registry, "%s not found (404)", desc.Digest)
	}

	_, err := w.Write(data)

	return err
}

func (f *fakeRemote) record(what string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.requests = append(f.requests, what)
}

func (f *fakeRemote) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.requests)
}

type fixture struct {
	remote  *fakeRemote
	catalog string
	// metadata is the digest of the catalog metadata manifest.
	metadata string
	entries  map[string]*catalog.Entry
	content  map[string][]byte
}

func largeSchema() []byte {
	var b strings.Builder

	b.WriteString(`{"type":"object","properties":{`)

	for i := range 300 {
		if i > 0 {
			b.WriteByte(',')
		}

		b.WriteString(`"p`)
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`":{"type":"string","description":"repeated text for compression"}`)
	}

	b.WriteString(`}}`)

	return []byte(b.String())
}

// fixtureNotices are the notice layers of the fixture schemas; alpha has
// none.
var fixtureNotices = map[string][]byte{"gamma": []byte("Gamma fixture notice. MIT License.\n")}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	f := &fixture{
		remote:  &fakeRemote{blobs: map[string][]byte{}, tamper: map[string][]byte{}, cutOff: map[string]int{}},
		entries: map[string]*catalog.Entry{},
		content: map[string][]byte{
			"alpha": []byte(`{"type":"object"}`),
			"gamma": largeSchema(),
		},
	}

	entries := make([]catalog.Entry, 0, 2)
	descs := make([]ocispec.Descriptor, 0, 2)

	for _, id := range []string{"alpha", "gamma"} {
		packed, err := artifact.PackSchema(f.content[id], fixtureNotices[id])
		if err != nil {
			t.Fatal(err)
		}

		f.add(packed)
		descs = append(descs, packed.Manifest.Descriptor)

		entries = append(entries, catalog.Entry{
			ID:       id,
			Name:     id + ".json",
			Artifact: catalog.Descriptor{MediaType: artifact.ManifestMediaType, Digest: packed.Manifest.Descriptor.Digest.String(), Size: packed.Manifest.Descriptor.Size},
		})
	}

	raw, err := catalog.Marshal(&catalog.Catalog{FormatVersion: catalog.FormatVersion, Revision: "20260924.1200", Schemas: entries})
	if err != nil {
		t.Fatal(err)
	}

	f.catalog, f.metadata = f.addCatalog(t, raw, descs)

	for i := range entries {
		f.entries[entries[i].ID] = &entries[i]
	}

	return f
}

// addCatalog packs raw into a catalog index over schemas, adds it to the
// remote and returns the digests of the index and of its metadata manifest.
func (f *fixture) addCatalog(t *testing.T, raw []byte, schemas []ocispec.Descriptor) (index, metadata string) {
	t.Helper()

	packed, err := artifact.PackCatalog(raw, schemas)
	if err != nil {
		t.Fatal(err)
	}

	f.add(packed.Metadata)
	f.remote.blobs[packed.Index.Descriptor.Digest.String()] = packed.Index.Data

	return packed.Index.Descriptor.Digest.String(), packed.Metadata.Manifest.Descriptor.Digest.String()
}

func (f *fixture) add(p *artifact.Packed) {
	f.remote.blobs[p.Manifest.Descriptor.Digest.String()] = p.Manifest.Data

	for _, b := range p.Blobs {
		f.remote.blobs[b.Descriptor.Digest.String()] = b.Data
	}
}

func (f *fixture) store(t *testing.T, dir string, offline bool) (*Store, *int) {
	t.Helper()

	c, err := cache.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = c.Close() })

	opened := 0

	return New(Options{
		Cache:          c,
		Repository:     "registry.example/org/schemas",
		Offline:        offline,
		ArtifactLimits: artifact.DefaultLimits(),
		CatalogLimits:  catalog.DefaultLimits(),
		Open: func() (Remote, error) {
			opened++

			return f.remote, nil
		},
	}), &opened
}

func TestColdThenWarm(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	ctx := context.Background()

	s, _ := f.store(t, dir, false)

	loaded, err := s.Catalog(ctx, f.catalog)
	if err != nil {
		t.Fatal(err)
	}

	if len(loaded.Catalog.Schemas) != 2 {
		t.Fatalf("catalog has %d schemas", len(loaded.Catalog.Schemas))
	}

	sch, err := s.Materialize(ctx, f.entries["gamma"])
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(sch.Path)
	if err != nil || !bytes.Equal(got, f.content["gamma"]) {
		t.Fatalf("materialized gamma differs: %v", err)
	}

	if sch.Ref != "registry.example/org/schemas@"+f.entries["gamma"].Artifact.Digest {
		t.Errorf("Ref = %s", sch.Ref)
	}

	before := f.remote.count()

	warm, opened := f.store(t, dir, false)
	if _, err := warm.Catalog(ctx, f.catalog); err != nil {
		t.Fatal(err)
	}

	again, err := warm.Materialize(ctx, f.entries["gamma"])
	if err != nil {
		t.Fatal(err)
	}

	if again.Path != sch.Path {
		t.Errorf("path changed: %s vs %s", again.Path, sch.Path)
	}

	if f.remote.count() != before || *opened != 0 {
		t.Errorf("warm cache contacted the registry: %d new requests, opened %d times", f.remote.count()-before, *opened)
	}
}

func TestLazyFetchOnlyRequestedSchema(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	s, _ := f.store(t, t.TempDir(), false)
	if _, err := s.Catalog(ctx, f.catalog); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Materialize(ctx, f.entries["alpha"]); err != nil {
		t.Fatal(err)
	}

	for _, r := range f.remote.requests {
		if strings.Contains(r, f.entries["gamma"].Artifact.Digest) {
			t.Errorf("gamma was fetched while only alpha was requested: %s", r)
		}
	}
}

func TestOfflineMissAndHit(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	ctx := context.Background()

	offline, _ := f.store(t, dir, true)

	_, err := offline.Catalog(ctx, f.catalog)
	if fault.KindOf(err) != fault.Offline || !strings.Contains(err.Error(), f.catalog) {
		t.Fatalf("offline cold catalog = %v", err)
	}

	online, _ := f.store(t, dir, false)
	if _, err := online.Catalog(ctx, f.catalog); err != nil {
		t.Fatal(err)
	}

	offline, opened := f.store(t, dir, true)
	if _, err := offline.Catalog(ctx, f.catalog); err != nil {
		t.Fatalf("offline warm catalog: %v", err)
	}

	_, err = offline.Materialize(ctx, f.entries["alpha"])
	if fault.KindOf(err) != fault.Offline || !strings.Contains(err.Error(), f.entries["alpha"].Artifact.Digest) {
		t.Fatalf("offline miss = %v", err)
	}

	if *opened != 0 {
		t.Errorf("offline store opened the registry %d times", *opened)
	}
}

func TestCorruptMaterializedRepairedOnlineRefusedOffline(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	ctx := context.Background()

	s, _ := f.store(t, dir, false)

	sch, err := s.Materialize(ctx, f.entries["alpha"])
	if err != nil {
		t.Fatal(err)
	}

	corruptFile(t, sch.Path, []byte(`{"type":"array"}`))

	offline, _ := f.store(t, dir, true)
	if _, err := offline.Materialize(ctx, f.entries["alpha"]); fault.KindOf(err) != fault.Offline {
		t.Fatalf("offline with corrupt file = %v, want offline error", err)
	}

	repaired, err := s.Materialize(ctx, f.entries["alpha"])
	if err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(repaired.Path)
	if !bytes.Equal(got, f.content["alpha"]) {
		t.Errorf("online repair produced %q", got)
	}
}

func TestCorruptPayloadBlobRefetched(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	ctx := context.Background()

	s, _ := f.store(t, dir, false)

	sch, err := s.Materialize(ctx, f.entries["gamma"])
	if err != nil {
		t.Fatal(err)
	}

	manifest := f.remote.blobs[f.entries["gamma"].Artifact.Digest]

	sm, err := artifact.ParseSchemaManifest(manifest, artifact.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	corruptFile(t, filepath.Join(dir, "v1", "blobs", "sha256", digest.Hex(sm.Payload.Digest.String())), []byte("garbage"))
	corruptFile(t, sch.Path, []byte("{}"))

	offline, _ := f.store(t, dir, true)
	if _, err := offline.Materialize(ctx, f.entries["gamma"]); fault.KindOf(err) != fault.Offline {
		t.Fatalf("offline with corrupt payload = %v", err)
	}

	again, err := s.Materialize(ctx, f.entries["gamma"])
	if err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(again.Path)
	if !bytes.Equal(got, f.content["gamma"]) {
		t.Error("gamma not repaired")
	}
}

func TestTamperedPayloadIsRejectedAndNotCached(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	ctx := context.Background()

	sm, err := artifact.ParseSchemaManifest(f.remote.blobs[f.entries["alpha"].Artifact.Digest], artifact.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	f.remote.tamper[sm.Payload.Digest.String()] = []byte(`{"type":"tampered"}`)

	s, _ := f.store(t, dir, false)

	_, err = s.Materialize(ctx, f.entries["alpha"])
	if fault.KindOf(err) != fault.Integrity {
		t.Fatalf("tampered payload = %v, want integrity error", err)
	}

	c, err := cache.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	if _, err := os.Stat(c.SchemaPath(f.entries["alpha"].Artifact.Digest)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("materialized file exists after a failed fetch: %v", err)
	}

	if _, err := c.ReadBlob(sm.Payload.Digest.String(), 1<<20); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("tampered payload was cached: %v", err)
	}
}

func TestOfflineRebuildsMissingMaterializedFromVerifiedBlobs(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	ctx := context.Background()

	s, _ := f.store(t, dir, false)

	sch, err := s.Materialize(ctx, f.entries["gamma"])
	if err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(filepath.Dir(sch.Path)); err != nil {
		t.Fatal(err)
	}

	offline, opened := f.store(t, dir, true)

	again, err := offline.Materialize(ctx, f.entries["gamma"])
	if err != nil {
		t.Fatalf("offline rebuild: %v", err)
	}

	got, _ := os.ReadFile(again.Path)
	if !bytes.Equal(got, f.content["gamma"]) || *opened != 0 {
		t.Errorf("offline rebuild wrong (opened %d)", *opened)
	}
}

func TestInvalidArtifactFromRegistry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	metadata := f.remote.blobs[f.metadata]

	entry := &catalog.Entry{ID: "wrong", Name: "wrong", Artifact: catalog.Descriptor{
		MediaType: artifact.ManifestMediaType,
		Digest:    f.metadata,
		Size:      int64(len(metadata)),
	}}

	s, _ := f.store(t, t.TempDir(), false)
	if _, err := s.Materialize(ctx, entry); fault.KindOf(err) != fault.Integrity {
		t.Fatalf("catalog metadata manifest used as schema = %v", err)
	}
}

func TestConcurrentMaterialize(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	ctx := context.Background()

	var wg sync.WaitGroup

	paths := make([]string, 8)
	errs := make([]error, 8)

	for i := range paths {
		wg.Go(func() {
			s, _ := f.store(t, dir, false)

			sch, err := s.Materialize(ctx, f.entries["gamma"])
			if err != nil {
				errs[i] = err

				return
			}

			paths[i] = sch.Path
		})
	}

	wg.Wait()

	for i := range paths {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}

		if paths[i] != paths[0] {
			t.Fatalf("paths differ: %s vs %s", paths[i], paths[0])
		}
	}

	got, _ := os.ReadFile(paths[0])
	if !bytes.Equal(got, f.content["gamma"]) {
		t.Error("concurrent materialization produced wrong content")
	}
}

func corruptFile(t *testing.T, path string, data []byte) {
	t.Helper()

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(fmt.Errorf("corrupt %s: %w", path, err))
	}
}

func payloadDigest(t *testing.T, f *fixture, id string) string {
	t.Helper()

	sm, err := artifact.ParseSchemaManifest(f.remote.blobs[f.entries[id].Artifact.Digest], artifact.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	return sm.Payload.Digest.String()
}

// TestCorruptPayloadBehindIntactSchemaIsIgnored keeps a verified schema.json
// usable when its payload blob, which it no longer needs, is damaged: offline
// and online alike serve it without a request. The damage matters only once
// schema.json has to be rebuilt, which is refused offline and repairs the
// blob online.
func TestCorruptPayloadBehindIntactSchemaIsIgnored(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	ctx := context.Background()

	s, _ := f.store(t, dir, false)

	sch, err := s.Materialize(ctx, f.entries["gamma"])
	if err != nil {
		t.Fatal(err)
	}

	payload := payloadDigest(t, f, "gamma")
	blob := filepath.Join(dir, "v1", "blobs", "sha256", digest.Hex(payload))
	corruptFile(t, blob, []byte("garbage"))

	before := f.remote.count()

	for _, offline := range []bool{true, false} {
		st, opened := f.store(t, dir, offline)

		again, err := st.Materialize(ctx, f.entries["gamma"])
		if err != nil || *opened != 0 {
			t.Fatalf("offline=%v with a corrupt payload blob behind a verified schema.json: %v (opened %d)", offline, err, *opened)
		}

		if data, err := st.ReadSchema(again); err != nil || !bytes.Equal(data, f.content["gamma"]) {
			t.Fatalf("offline=%v: schema differs: %v", offline, err)
		}
	}

	if f.remote.count() != before {
		t.Errorf("a verified schema.json sent %d request(s)", f.remote.count()-before)
	}

	if got, _ := os.ReadFile(blob); string(got) != "garbage" {
		t.Error("the unused payload blob was touched")
	}

	if err := os.RemoveAll(filepath.Dir(sch.Path)); err != nil {
		t.Fatal(err)
	}

	offline, opened := f.store(t, dir, true)
	if _, err := offline.Materialize(ctx, f.entries["gamma"]); fault.KindOf(err) != fault.Offline || *opened != 0 {
		t.Fatalf("offline rebuild from a corrupt payload blob = %v (opened %d), want an offline error", err, *opened)
	}

	online, _ := f.store(t, dir, false)
	if _, err := online.Materialize(ctx, f.entries["gamma"]); err != nil {
		t.Fatalf("online rebuild: %v", err)
	}

	if got, err := os.ReadFile(blob); err != nil || digest.FromBytes(got) != payload {
		t.Errorf("payload blob not repaired: %v", err)
	}

	warm, opened := f.store(t, dir, true)
	if _, err := warm.Materialize(ctx, f.entries["gamma"]); err != nil || *opened != 0 {
		t.Errorf("offline after repair: %v (opened %d)", err, *opened)
	}
}

func TestMissingPayloadBehindIntactSchemaIsFine(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	ctx := context.Background()

	s, _ := f.store(t, dir, false)
	if _, err := s.Materialize(ctx, f.entries["gamma"]); err != nil {
		t.Fatal(err)
	}

	blob := filepath.Join(dir, "v1", "blobs", "sha256", digest.Hex(payloadDigest(t, f, "gamma")))
	if err := os.Remove(blob); err != nil {
		t.Fatal(err)
	}

	offline, opened := f.store(t, dir, true)
	if _, err := offline.Materialize(ctx, f.entries["gamma"]); err != nil || *opened != 0 {
		t.Errorf("offline without the payload blob: %v (opened %d)", err, *opened)
	}
}

func TestCutOffPayloadDownloadIsRetried(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.remote.cutOff[payloadDigest(t, f, "gamma")] = payloadAttempts - 1

	s, _ := f.store(t, t.TempDir(), false)

	sch, err := s.Materialize(ctx, f.entries["gamma"])
	if err != nil {
		t.Fatalf("download cut off %d times: %v", payloadAttempts-1, err)
	}

	got, _ := os.ReadFile(sch.Path)
	if !bytes.Equal(got, f.content["gamma"]) {
		t.Error("wrong content after retries")
	}

	g := newFixture(t)
	g.remote.cutOff[payloadDigest(t, g, "gamma")] = payloadAttempts

	s2, _ := g.store(t, t.TempDir(), false)
	if _, err := s2.Materialize(ctx, g.entries["gamma"]); fault.KindOf(err) != fault.Registry {
		t.Errorf("download cut off every time = %v, want a registry error", err)
	}
}

type deniedRemote struct {
	*fakeRemote

	payload string
	denied  int
}

func (d *deniedRemote) FetchTo(ctx context.Context, desc ocispec.Descriptor, w io.Writer) error {
	if desc.Digest.String() == d.payload {
		d.denied++

		return fault.New(fault.Registry, "fetch %s: access denied (403)", desc.Digest)
	}

	return d.fakeRemote.FetchTo(ctx, desc, w)
}

func TestRegistryRejectionIsNotRetried(t *testing.T) {
	f := newFixture(t)
	remote := &deniedRemote{fakeRemote: f.remote, payload: payloadDigest(t, f, "gamma")}

	c, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = c.Close() })

	s := New(Options{
		Cache:          c,
		Repository:     "registry.example/org/schemas",
		ArtifactLimits: artifact.DefaultLimits(),
		CatalogLimits:  catalog.DefaultLimits(),
		Open:           func() (Remote, error) { return remote, nil },
	})

	if _, err := s.Materialize(context.Background(), f.entries["gamma"]); fault.KindOf(err) != fault.Registry {
		t.Fatalf("denied payload = %v, want a registry error", err)
	}

	if remote.denied != 1 {
		t.Errorf("a 403 was requested %d times, want exactly once", remote.denied)
	}
}

func noticeDigest(t *testing.T, f *fixture, id string) string {
	t.Helper()

	sm, err := artifact.ParseSchemaManifest(f.remote.blobs[f.entries[id].Artifact.Digest], artifact.DefaultLimits())
	if err != nil || sm.Notice == nil {
		t.Fatalf("%s has no notice layer: %v", id, err)
	}

	return sm.Notice.Digest.String()
}

// TestNotice fetches a schema's notice only on request, serves it from the
// cache afterwards, also offline, and treats a missing or damaged copy like
// any other cache entry.
func TestNotice(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	ctx := context.Background()

	s, _ := f.store(t, dir, false)

	alpha, err := s.Materialize(ctx, f.entries["alpha"])
	if err != nil {
		t.Fatal(err)
	}

	gamma, err := s.Materialize(ctx, f.entries["gamma"])
	if err != nil {
		t.Fatal(err)
	}

	notice := noticeDigest(t, f, "gamma")
	blob := filepath.Join(dir, "v1", "blobs", "sha256", digest.Hex(notice))

	if _, err := os.Lstat(blob); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("materializing gamma cached its notice: %v", err)
	}

	before := f.remote.count()

	if text, err := s.Notice(ctx, alpha); err != nil || text != nil {
		t.Fatalf("notice of alpha = %q, %v; want none", text, err)
	}

	if f.remote.count() != before {
		t.Fatal("a schema without a notice sent a request")
	}

	offline, opened := f.store(t, dir, true)
	if _, err := offline.Notice(ctx, gamma); fault.KindOf(err) != fault.Offline || !strings.Contains(err.Error(), notice) || *opened != 0 {
		t.Fatalf("offline notice miss = %v (opened %d), want an offline error naming %s", err, *opened, notice)
	}

	text, err := s.Notice(ctx, gamma)
	if err != nil || !bytes.Equal(text, fixtureNotices["gamma"]) {
		t.Fatalf("notice of gamma = %q, %v", text, err)
	}

	before = f.remote.count()

	offline, opened = f.store(t, dir, true)
	if text, err := offline.Notice(ctx, gamma); err != nil || !bytes.Equal(text, fixtureNotices["gamma"]) || *opened != 0 {
		t.Fatalf("offline cached notice = %q, %v (opened %d)", text, err, *opened)
	}

	if f.remote.count() != before {
		t.Error("a cached notice sent a request")
	}

	corruptFile(t, blob, []byte("Forged notice.\n"))

	if _, err := offline.Notice(ctx, gamma); fault.KindOf(err) != fault.Offline {
		t.Fatalf("offline corrupt notice = %v, want an offline error", err)
	}

	online, _ := f.store(t, dir, false)
	if text, err := online.Notice(ctx, gamma); err != nil || !bytes.Equal(text, fixtureNotices["gamma"]) {
		t.Fatalf("online repair of the notice = %q, %v", text, err)
	}

	if got, err := os.ReadFile(blob); err != nil || digest.FromBytes(got) != notice {
		t.Errorf("notice blob not repaired: %v", err)
	}
}

func TestTamperedNoticeIsRejectedAndNotCached(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	ctx := context.Background()

	notice := noticeDigest(t, f, "gamma")
	f.remote.tamper[notice] = []byte("Forged notice.\n")

	s, _ := f.store(t, dir, false)

	sch, err := s.Materialize(ctx, f.entries["gamma"])
	if err != nil {
		t.Fatal(err)
	}

	if text, err := s.Notice(ctx, sch); fault.KindOf(err) != fault.Integrity {
		t.Fatalf("tampered notice = %q, %v; want an integrity error", text, err)
	}

	if _, err := os.Lstat(filepath.Join(dir, "v1", "blobs", "sha256", digest.Hex(notice))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tampered notice was cached: %v", err)
	}
}
