package materialize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/store"
)

func schemaItem(t *testing.T, id, body string, notice []byte) packedItem {
	t.Helper()

	p, err := artifact.PackSchema([]byte(body), notice)
	if err != nil {
		t.Fatal(err)
	}

	return packedItem{p: p, e: catalog.Entry{
		ID:       id,
		Name:     id + ".json",
		Artifact: catalog.Descriptor{MediaType: p.Manifest.Descriptor.MediaType, Digest: p.Manifest.Descriptor.Digest.String(), Size: p.Manifest.Descriptor.Size},
	}}
}

func mutateManifest(t *testing.T, item packedItem, change func(*ocispec.Manifest)) packedItem {
	t.Helper()

	var manifest ocispec.Manifest
	if err := json.Unmarshal(item.p.Manifest.Data, &manifest); err != nil {
		t.Fatal(err)
	}

	change(&manifest)
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	p := *item.p
	p.Manifest.Data = data
	p.Manifest.Descriptor.Digest = godigest.Digest(digest.FromBytes(data))
	p.Manifest.Descriptor.Size = int64(len(data))
	item.p = &p
	item.e.Artifact.Digest = p.Manifest.Descriptor.Digest.String()
	item.e.Artifact.Size = p.Manifest.Descriptor.Size

	return item
}

func testCache(t *testing.T, dir string) *cache.Cache {
	t.Helper()

	c, err := cache.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = c.Close() })

	return c
}

func blobPath(dir, dgst string) string {
	return filepath.Join(dir, "v1", "blobs", "sha256", digest.Hex(dgst))
}

func corruptFile(t *testing.T, path string) {
	t.Helper()

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("damaged"), 0o644); err != nil {
		t.Fatal(err)
	}
}

type cachedFile struct {
	Data []byte
	Mode fs.FileMode
}

func cacheSnapshot(t *testing.T, dir string) map[string]cachedFile {
	t.Helper()

	files := map[string]cachedFile{}
	if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}

		file := cachedFile{Mode: info.Mode()}
		if info.Mode().IsRegular() {
			file.Data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}

		files[rel] = file

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	return files
}

func loadAndRun(t *testing.T, f *fixture, dir string, offline bool, opts Options) (*store.LoadedCatalog, *Result) {
	t.Helper()

	st := f.store(t, dir, offline)
	lc, err := st.Catalog(t.Context(), f.digest)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Run(t.Context(), st, lc, opts)
	if err != nil {
		t.Fatal(err)
	}

	return lc, res
}

func TestOfflinePropagatesUnsupportedManifest(t *testing.T) {
	item := schemaItem(t, "alpha", `{"type":"object"}`, nil)
	item = mutateManifest(t, item, func(manifest *ocispec.Manifest) {
		manifest.ArtifactType = "application/vnd.ovineko.schepherd.schema.v3"
	})
	f := fixtureFromItems(t, []packedItem{item})
	dir := t.TempDir()
	st := f.store(t, dir, false)
	if _, err := st.Catalog(t.Context(), f.digest); err != nil {
		t.Fatal(err)
	}

	c := testCache(t, dir)
	desc := item.p.Manifest.Descriptor
	if err := c.WriteBlob(desc.Digest.String(), desc.Size, desc.Size, bytes.NewReader(item.p.Manifest.Data)); err != nil {
		t.Fatal(err)
	}

	before := cacheSnapshot(t, dir)
	requests := f.rem.requests.Load()
	sto := f.store(t, dir, true)
	lc, err := sto.Catalog(t.Context(), f.digest)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Run(t.Context(), sto, lc, Options{Concurrency: 1})
	if !errors.Is(err, artifact.ErrUnsupported) || fault.KindOf(err) != fault.Usage {
		t.Fatalf("unsupported manifest: result=%+v error=%v, want unsupported usage error", res, err)
	}

	if res != nil && res.Complete {
		t.Fatal("unsupported manifest reported a complete result")
	}

	if f.rem.requests.Load() != requests || !reflect.DeepEqual(before, cacheSnapshot(t, dir)) {
		t.Fatal("offline unsupported-manifest verification changed the cache or contacted the registry")
	}
}

func TestOfflinePropagatesCacheIOError(t *testing.T) {
	f := newFixture(t, true)
	dir := t.TempDir()
	lc, _ := loadAndRun(t, f, dir, false, Options{})
	c := testCache(t, dir)
	st := store.New(store.Options{Cache: c, Offline: true, ArtifactLimits: artifact.DefaultLimits()})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	before := cacheSnapshot(t, dir)
	res, err := Run(t.Context(), st, lc, Options{})
	if err == nil || fault.KindOf(err) != fault.Internal {
		t.Fatalf("closed cache: result=%+v error=%v, want internal I/O error", res, err)
	}

	if res != nil && res.Complete {
		t.Fatal("cache I/O failure reported a complete result")
	}

	if !reflect.DeepEqual(before, cacheSnapshot(t, dir)) {
		t.Fatal("offline I/O failure changed the cache")
	}
}

func TestOfflineChecksEveryKnownArtifactWithoutWriting(t *testing.T) {
	for _, state := range []string{"absent", "corrupt", "schemaFileOnly"} {
		t.Run(state, func(t *testing.T) {
			f := newFixture(t, true)
			dir := t.TempDir()
			lc, _ := loadAndRun(t, f, dir, false, Options{})
			entry := lc.Catalog.Schemas[0]
			sm, err := artifact.ParseSchemaManifest(f.rem.blobs[entry.Artifact.Digest], artifact.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}

			c := testCache(t, dir)
			paths := []string{c.SchemaPath(entry.Artifact.Digest)}
			want := []MissingEntry{{Kind: "schemaFile", Digest: entry.Artifact.Digest, State: "absent", IDs: []string{"delta", "echo"}}}
			if state != "schemaFileOnly" {
				paths = append(paths, blobPath(dir, sm.Payload.Digest.String()), blobPath(dir, sm.Notice.Digest.String()))
				want = append(want,
					MissingEntry{Kind: "payload", Digest: sm.Payload.Digest.String(), State: state, IDs: []string{"delta", "echo"}},
					MissingEntry{Kind: "notice", Digest: sm.Notice.Digest.String(), State: state, IDs: []string{"delta", "echo"}},
				)
				want[0].State = state
			}

			for _, path := range paths {
				if state == "corrupt" {
					corruptFile(t, path)
				} else if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}

			slices.SortFunc(want, func(a, b MissingEntry) int { return strings.Compare(a.Kind, b.Kind) })
			before := cacheSnapshot(t, dir)
			requests := f.rem.requests.Load()
			_, res := loadAndRun(t, f, dir, true, Options{Concurrency: 4})
			if res.Complete || !reflect.DeepEqual(res.Missing, want) {
				t.Fatalf("verification = %+v, want missing %+v", res, want)
			}

			if res.WrittenSchemaFiles != 0 || res.RepairedEntries != 0 || res.FetchedBytes != 0 {
				t.Fatalf("offline verification reported writes or transfers: %+v", res)
			}

			if f.rem.requests.Load() != requests || !reflect.DeepEqual(before, cacheSnapshot(t, dir)) {
				t.Fatal("offline verification changed the cache or contacted the registry")
			}
		})
	}
}

func sharedItems(t *testing.T) []packedItem {
	t.Helper()

	notice := []byte("Shared notice. MIT License.\n")
	alpha := schemaItem(t, "alpha", `{"type":"object","title":"shared"}`, notice)
	bravo := schemaItem(t, "bravo", `{"type":"object","title":"shared"}`, nil)
	charlie := schemaItem(t, "charlie", `{"type":"string"}`, notice)
	zeta := alpha
	zeta.e.ID = "zeta"

	return []packedItem{alpha, bravo, charlie, zeta}
}

func TestSharedNoticeMissingIDsAreUniqueAndSorted(t *testing.T) {
	for _, concurrency := range []int{1, 4} {
		t.Run(strconv.Itoa(concurrency), func(t *testing.T) {
			items := sharedItems(t)
			f := fixtureFromItems(t, items)
			dir := t.TempDir()
			loadAndRun(t, f, dir, false, Options{Concurrency: concurrency})
			sm, err := artifact.ParseSchemaManifest(items[0].p.Manifest.Data, artifact.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}

			if err := os.Remove(blobPath(dir, sm.Notice.Digest.String())); err != nil {
				t.Fatal(err)
			}

			before := cacheSnapshot(t, dir)
			_, res := loadAndRun(t, f, dir, true, Options{Concurrency: concurrency})
			want := []MissingEntry{{Kind: "notice", Digest: sm.Notice.Digest.String(), State: "absent", IDs: []string{"alpha", "charlie", "zeta"}}}
			if res.Complete || !reflect.DeepEqual(res.Missing, want) {
				t.Fatalf("shared notice = %+v, want %+v", res, want)
			}

			if res.CachedBlobs != 3 {
				t.Fatalf("cached blobs=%d, want catalog plus two distinct payloads", res.CachedBlobs)
			}

			if !reflect.DeepEqual(before, cacheSnapshot(t, dir)) {
				t.Fatal("shared missing notice changed the offline cache")
			}
		})
	}
}

func TestSharedNoticeRejectsConflictingSizes(t *testing.T) {
	for _, offline := range []bool{false, true} {
		for _, concurrency := range []int{1, 4} {
			t.Run(fmt.Sprintf("offline=%t/concurrency=%d", offline, concurrency), func(t *testing.T) {
				items := sharedItems(t)
				dir := t.TempDir()
				loadAndRun(t, fixtureFromItems(t, items), dir, false, Options{})
				items[2] = mutateManifest(t, items[2], func(manifest *ocispec.Manifest) {
					manifest.Layers[1].Size++
				})
				f := fixtureFromItems(t, items)
				st := f.store(t, dir, false)
				if _, err := st.Catalog(t.Context(), f.digest); err != nil {
					t.Fatal(err)
				}

				c := testCache(t, dir)
				p := items[2].p
				desc := p.Manifest.Descriptor
				if err := c.WriteBlob(desc.Digest.String(), desc.Size, desc.Size, bytes.NewReader(p.Manifest.Data)); err != nil {
					t.Fatal(err)
				}

				if _, err := c.WriteMaterialized(desc.Digest.String(), p.ContentDigest, p.ContentSize, func(w io.Writer) error {
					_, err := io.WriteString(w, `{"type":"string"}`)

					return err
				}); err != nil {
					t.Fatal(err)
				}

				before := cacheSnapshot(t, dir)
				st = f.store(t, dir, offline)
				lc, err := st.Catalog(t.Context(), f.digest)
				if err != nil {
					t.Fatal(err)
				}

				res, err := Run(t.Context(), st, lc, Options{Concurrency: concurrency})
				if err == nil || fault.KindOf(err) != fault.Integrity {
					t.Fatalf("conflicting notice: result=%+v error=%v, want integrity error", res, err)
				}

				if res != nil && res.Complete {
					t.Fatal("conflicting notice sizes reported a complete result")
				}

				if offline && !reflect.DeepEqual(before, cacheSnapshot(t, dir)) {
					t.Fatal("conflicting notice verification changed the offline cache")
				}
			})
		}
	}
}

type waitingContext struct {
	//nolint:containedctx // Test-only decorator observes shared waits without adding production hooks.
	context.Context

	waiting chan struct{}
	once    sync.Once
}

func (c *waitingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })

	return c.Context.Done()
}

func TestSharedBlobWaitsAndRetainsFailure(t *testing.T) {
	tracker := &blobTracker{}
	cnt := &counters{}
	desc := ocispec.Descriptor{Digest: godigest.Digest(digest.FromBytes([]byte("notice"))), Size: 6}
	started := make(chan struct{})
	release := make(chan struct{})
	releaseWork := sync.OnceFunc(func() { close(release) })
	defer releaseWork()
	first := make(chan error, 1)
	second := make(chan error, 1)
	var calls atomic.Int64
	wantErr := fault.Wrap(fault.Offline, cache.ErrNotFound, "missing notice")
	ensure := func() (store.EnsureOutcome, error) {
		calls.Add(1)
		close(started)
		<-release

		return store.EnsureOutcome{}, wantErr
	}

	go func() { first <- tracker.ensure(t.Context(), "alpha", desc, true, cnt, ensure) }()
	<-started
	ctx := &waitingContext{Context: t.Context(), waiting: make(chan struct{})}
	go func() { second <- tracker.ensure(ctx, "bravo", desc, true, cnt, ensure) }()
	select {
	case <-ctx.waiting:
	case err := <-second:
		t.Fatalf("shared verification returned before the owner finished: %v", err)
	}
	releaseWork()

	for _, got := range []error{<-first, <-second, tracker.ensure(t.Context(), "charlie", desc, true, cnt, ensure)} {
		if !errors.Is(got, wantErr) {
			t.Fatalf("shared result error=%v, want retained %v", got, wantErr)
		}
	}

	if calls.Load() != 1 || cnt.fetchedBlobs.Load() != 0 || cnt.cachedBlobs.Load() != 0 || tracker.noticeCount() != 0 {
		t.Fatalf("failed shared blob: calls=%d counters=%+v notices=%d", calls.Load(), cnt, tracker.noticeCount())
	}
}

func TestCachedRunHonorsCancellationAndDeadlines(t *testing.T) {
	for _, offline := range []bool{false, true} {
		for _, mode := range []string{"canceled", "deadline", "duringProcessing"} {
			t.Run(fmt.Sprintf("offline=%t/%s", offline, mode), func(t *testing.T) {
				f := fixtureFromItems(t, sharedItems(t))
				dir := t.TempDir()
				loadAndRun(t, f, dir, false, Options{})
				st := f.store(t, dir, offline)
				lc, err := st.Catalog(t.Context(), f.digest)
				if err != nil {
					t.Fatal(err)
				}

				var ctx context.Context
				var cancel context.CancelFunc
				wantErr := context.Canceled
				wantKind := fault.Canceled
				opts := Options{Concurrency: 1}
				if mode == "deadline" {
					ctx, cancel = context.WithDeadline(t.Context(), time.Unix(0, 0))
					wantErr = context.DeadlineExceeded
					wantKind = fault.Registry
				} else {
					ctx, cancel = context.WithCancel(t.Context())
					if mode == "duringProcessing" {
						opts.Log = func(string, ...any) { cancel() }
					} else {
						cancel()
					}
				}
				defer cancel()

				before := cacheSnapshot(t, dir)
				requests := f.rem.requests.Load()
				res, err := Run(ctx, st, lc, opts)
				if !errors.Is(err, wantErr) || fault.KindOf(err) != wantKind {
					t.Fatalf("cached cancellation: result=%+v error=%v, want %v (%s)", res, err, wantErr, wantKind)
				}

				if res != nil && res.Complete {
					t.Fatal("canceled cached run reported a complete result")
				}

				if f.rem.requests.Load() != requests || !reflect.DeepEqual(before, cacheSnapshot(t, dir)) {
					t.Fatal("canceled cached run changed the cache or contacted the registry")
				}
			})
		}
	}
}

func TestMissingArtifactsUnionIDsAndSortDeterministically(t *testing.T) {
	for _, concurrency := range []int{1, 4} {
		t.Run(strconv.Itoa(concurrency), func(t *testing.T) {
			items := sharedItems(t)
			items = append(items, schemaItem(t, "delta", `{"type":"object","title":"shared"}`, []byte("Other notice.\n")))
			slices.SortFunc(items, func(a, b packedItem) int { return strings.Compare(a.e.ID, b.e.ID) })
			f := fixtureFromItems(t, items)
			dir := t.TempDir()
			lc, _ := loadAndRun(t, f, dir, false, Options{Concurrency: concurrency})
			graph := graphOf(t, f, lc)
			var want []MissingEntry
			byArtifact := map[missingKey]int{}
			for _, entry := range lc.Catalog.Schemas {
				sm := graph.schemas[entry.Artifact.Digest]
				artifacts := []MissingEntry{{Kind: "payload", Digest: sm.Payload.Digest.String(), State: "absent"}}
				if sm.Notice != nil {
					artifacts = append(artifacts, MissingEntry{Kind: "notice", Digest: sm.Notice.Digest.String(), State: "absent"})
				}
				for _, missing := range artifacts {
					key := missingKey{kind: missing.Kind, digest: missing.Digest}
					i, ok := byArtifact[key]
					if !ok {
						byArtifact[key] = len(want)
						i = len(want)
						want = append(want, missing)
						if err := os.Remove(blobPath(dir, missing.Digest)); err != nil {
							t.Fatal(err)
						}
					}
					want[i].IDs = append(want[i].IDs, entry.ID)
				}
			}

			slices.SortFunc(want, func(a, b MissingEntry) int {
				if a.Kind != b.Kind {
					return strings.Compare(a.Kind, b.Kind)
				}

				return strings.Compare(a.Digest, b.Digest)
			})
			if len(want) != 4 {
				t.Fatalf("fixture has %d distinct missing artifacts, want two payloads and two notices", len(want))
			}

			before := cacheSnapshot(t, dir)
			_, res := loadAndRun(t, f, dir, true, Options{Concurrency: concurrency})
			if res.Complete || !reflect.DeepEqual(res.Missing, want) {
				t.Fatalf("missing artifacts = %+v, want %+v", res, want)
			}

			if res.CachedBlobs != 1 || !reflect.DeepEqual(before, cacheSnapshot(t, dir)) {
				t.Fatalf("offline run changed the cache or counted missing blobs as cached: %+v", res)
			}
		})
	}
}

func TestSharedBlobWaitHonorsCancellation(t *testing.T) {
	tracker := &blobTracker{}
	cnt := &counters{}
	desc := ocispec.Descriptor{Digest: godigest.Digest(digest.FromBytes([]byte("notice"))), Size: 6}
	started := make(chan struct{})
	release := make(chan struct{})
	releaseWork := sync.OnceFunc(func() { close(release) })
	defer releaseWork()
	first := make(chan error, 1)
	second := make(chan error, 1)
	var calls atomic.Int64
	ensure := func() (store.EnsureOutcome, error) {
		calls.Add(1)
		close(started)
		<-release

		return store.EnsureOutcome{}, nil
	}

	go func() { first <- tracker.ensure(t.Context(), "alpha", desc, true, cnt, ensure) }()
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waiting := &waitingContext{Context: ctx, waiting: make(chan struct{})}
	go func() { second <- tracker.ensure(waiting, "bravo", desc, true, cnt, ensure) }()
	select {
	case <-waiting.waiting:
	case err := <-second:
		t.Fatalf("shared verification returned before the owner finished: %v", err)
	}
	cancel()
	if err := <-second; !errors.Is(err, context.Canceled) || fault.KindOf(err) != fault.Canceled {
		t.Fatalf("canceled shared wait = %v, want classified cancellation", err)
	}

	releaseWork()
	if err := <-first; err != nil {
		t.Fatal(err)
	}

	if calls.Load() != 1 || cnt.cachedBlobs.Load() != 1 || tracker.noticeCount() != 1 {
		t.Fatalf("canceled shared wait duplicated work: calls=%d cached blobs=%d notices=%d", calls.Load(), cnt.cachedBlobs.Load(), tracker.noticeCount())
	}
}

func TestOfflinePartialCatalogVerifiesOnlyKnownGraph(t *testing.T) {
	for _, concurrency := range []int{1, 4} {
		for _, kind := range []string{"catalogIndex", "catalogMetadata", "catalogPayload"} {
			for _, state := range []string{"absent", "corrupt"} {
				t.Run(fmt.Sprintf("%d/%s/%s", concurrency, kind, state), func(t *testing.T) {
					items := sharedItems(t)
					f := fixtureFromItems(t, items)
					dir := t.TempDir()
					lc, _ := loadAndRun(t, f, dir, false, Options{})
					graph := graphOf(t, f, lc)
					index, err := artifact.ParseCatalogIndex(f.rem.blobs[f.digest], artifact.DefaultLimits())
					if err != nil {
						t.Fatal(err)
					}

					catalogDigest := f.digest
					catalogManifests := 0
					switch kind {
					case "catalogMetadata":
						catalogDigest = index.Metadata.Digest.String()
						catalogManifests = 1
					case "catalogPayload":
						catalogDigest = graph.catalogJSON
						catalogManifests = 2
					}
					alphaDigest := items[0].e.Artifact.Digest
					bravoDigest := items[1].e.Artifact.Digest
					alpha := graph.schemas[alphaDigest]
					c := testCache(t, dir)
					paths := []string{
						blobPath(dir, catalogDigest),
						blobPath(dir, bravoDigest),
						blobPath(dir, alpha.Payload.Digest.String()),
						blobPath(dir, alpha.Notice.Digest.String()),
						c.SchemaPath(alphaDigest),
					}
					for _, path := range paths {
						if state == "corrupt" {
							corruptFile(t, path)
						} else if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
					}

					want := []MissingEntry{{Kind: kind, Digest: catalogDigest, State: state, IDs: []string{}}}
					wantStats := runStats{}
					wantSchemas := 0
					if kind != "catalogIndex" {
						want = append(want,
							MissingEntry{Kind: "manifest", Digest: bravoDigest, State: state, IDs: []string{}},
							MissingEntry{Kind: "payload", Digest: alpha.Payload.Digest.String(), State: state, IDs: []string{}},
							MissingEntry{Kind: "notice", Digest: alpha.Notice.Digest.String(), State: state, IDs: []string{}},
							MissingEntry{Kind: "schemaFile", Digest: alphaDigest, State: state, IDs: []string{}},
						)
						wantSchemas = 3
						wantStats = runStats{CachedManifests: catalogManifests + 2, CachedBlobs: 1, CachedSchemaFiles: 1}
					}
					slices.SortFunc(want, func(a, b MissingEntry) int {
						if a.Kind != b.Kind {
							return strings.Compare(a.Kind, b.Kind)
						}
						return strings.Compare(a.Digest, b.Digest)
					})

					before := cacheSnapshot(t, dir)
					requests := f.rem.requests.Load()
					st := f.store(t, dir, true)
					partial, err := st.CatalogForCompleteness(t.Context(), f.digest)
					if err != nil {
						t.Fatal(err)
					}
					if partial.Catalog != nil {
						t.Fatal("unavailable catalog payload produced a catalog")
					}
					for range 2 {
						res, err := Run(t.Context(), st, partial, Options{Concurrency: concurrency})
						if err != nil {
							t.Fatal(err)
						}
						if res.Complete || res.Entries != 0 || res.Revision != "" || res.Schemas != wantSchemas || res.Notices != 0 || res.CatalogDigest != f.digest || !res.Offline {
							t.Fatalf("partial catalog summary: %+v", res)
						}
						if !reflect.DeepEqual(res.Missing, want) || !reflect.DeepEqual(statsOf(res), wantStats) {
							t.Fatalf("partial catalog result: %+v, want missing %+v and stats %+v", res, want, wantStats)
						}
						raw, err := json.Marshal(res)
						if err != nil {
							t.Fatal(err)
						}
						var decoded Result
						if err := json.Unmarshal(raw, &decoded); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(decoded.Missing, want) || bytes.Contains(raw, []byte(`"ids":null`)) {
							t.Fatalf("unknown IDs must encode as empty arrays: %s", raw)
						}
					}
					if f.rem.requests.Load() != requests || !reflect.DeepEqual(before, cacheSnapshot(t, dir)) {
						t.Fatal("partial catalog verification changed the cache or contacted the registry")
					}
				})
			}
		}
	}
}

func TestOfflinePartialCatalogCountsHealthySchemasAndSharedLayers(t *testing.T) {
	for _, kind := range []string{"catalogMetadata", "catalogPayload"} {
		t.Run(kind, func(t *testing.T) {
			f := fixtureFromItems(t, sharedItems(t))
			dir := t.TempDir()
			lc, _ := loadAndRun(t, f, dir, false, Options{})
			graph := graphOf(t, f, lc)
			index, err := artifact.ParseCatalogIndex(f.rem.blobs[f.digest], artifact.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			dgst := graph.catalogJSON
			cachedManifests := len(graph.manifests)
			if kind == "catalogMetadata" {
				dgst = index.Metadata.Digest.String()
				cachedManifests--
			}
			if err := os.Remove(blobPath(dir, dgst)); err != nil {
				t.Fatal(err)
			}
			st := f.store(t, dir, true)
			partial, err := st.CatalogForCompleteness(t.Context(), f.digest)
			if err != nil {
				t.Fatal(err)
			}
			res, err := Run(t.Context(), st, partial, Options{Concurrency: 4})
			if err != nil {
				t.Fatal(err)
			}
			want := []MissingEntry{{Kind: kind, Digest: dgst, State: "absent", IDs: []string{}}}
			wantStats := runStats{
				CachedManifests:   cachedManifests,
				CachedBlobs:       len(graph.blobs) - 1,
				CachedSchemaFiles: len(graph.schemas),
			}
			if res.Complete || res.Entries != 0 || res.Revision != "" || res.Schemas != 3 || res.Notices != 1 || !reflect.DeepEqual(res.Missing, want) || !reflect.DeepEqual(statsOf(res), wantStats) {
				t.Fatalf("healthy indexed graph with unavailable catalog: %+v, want missing %+v and stats %+v", res, want, wantStats)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			canceled, err := Run(ctx, st, partial, Options{Concurrency: 1, Log: func(string, ...any) { cancel() }})
			if canceled != nil || !errors.Is(err, context.Canceled) || fault.KindOf(err) != fault.Canceled {
				t.Fatalf("partial catalog cancellation: result=%+v error=%v", canceled, err)
			}
		})
	}
}

func TestOfflinePartialCatalogUnexpectedSchemaErrorsRemainFatal(t *testing.T) {
	f := fixtureFromItems(t, sharedItems(t))
	dir := t.TempDir()
	lc, _ := loadAndRun(t, f, dir, false, Options{})
	graph := graphOf(t, f, lc)
	if err := os.Remove(blobPath(dir, graph.catalogJSON)); err != nil {
		t.Fatal(err)
	}
	c := testCache(t, dir)
	st := store.New(store.Options{Cache: c, Offline: true, ArtifactLimits: artifact.DefaultLimits(), CatalogLimits: catalog.DefaultLimits()})
	partial, err := st.CatalogForCompleteness(t.Context(), f.digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := Run(t.Context(), st, partial, Options{Concurrency: 1})
	if res != nil || err == nil || fault.KindOf(err) != fault.Internal {
		t.Fatalf("partial catalog unexpected cache fault: result=%+v error=%v", res, err)
	}
}
