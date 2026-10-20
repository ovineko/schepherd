package store

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/fault"
)

func cacheCatalogFixture(t *testing.T, s *Store, f *fixture) {
	t.Helper()
	for dgst, data := range f.remote.blobs {
		size := int64(len(data))
		if err := s.opts.Cache.WriteBlob(dgst, size, size, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
}

type catalogCacheFile struct {
	Data    string
	Mode    fs.FileMode
	ModTime time.Time
}

func catalogCacheSnapshot(t *testing.T, dir string) map[string]catalogCacheFile {
	t.Helper()
	files := make(map[string]catalogCacheFile)
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// A handle refreshes NTFS timestamps; directory enumeration and os.Stat can be stale.
		handle, err := os.Open(path)
		if err != nil {
			return err
		}
		info, statErr := handle.Stat()
		closeErr := handle.Close()
		if statErr != nil {
			return statErr
		}
		if closeErr != nil {
			return closeErr
		}
		file := catalogCacheFile{Mode: info.Mode(), ModTime: info.ModTime()}
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			file.Data = string(data)
		}
		files[path] = file
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestCatalogForCompletenessOfflineMissingAndCorrupt(t *testing.T) {
	for _, kind := range []string{"catalogIndex", "catalogMetadata", "catalogPayload"} {
		for _, damage := range []string{"missing", "different size", "same size", "non-regular"} {
			t.Run(kind+"/"+damage, func(t *testing.T) {
				f := newFixture(t)
				dir := t.TempDir()
				s, opened := f.store(t, dir, true)
				cacheCatalogFixture(t, s, f)
				ci, err := artifact.ParseCatalogIndex(f.remote.blobs[f.catalog], artifact.DefaultLimits())
				if err != nil {
					t.Fatal(err)
				}
				dgst, manifests := f.catalog, 0
				switch kind {
				case "catalogMetadata":
					dgst, manifests = f.metadata, 1
				case "catalogPayload":
					dgst, manifests = mustPayload(t, f).Digest.String(), 2
				}
				path := blobFile(dir, dgst)
				wantErr := cache.ErrCorrupt
				switch damage {
				case "missing":
					wantErr = cache.ErrNotFound
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case "non-regular":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(path, 0o755); err != nil {
						t.Fatal(err)
					}
				default:
					bad := []byte("corrupt")
					if damage == "same size" {
						bad = bytes.Clone(f.remote.blobs[dgst])
						bad[0] ^= 1
					}
					corruptFile(t, path, bad)
				}
				before := catalogCacheSnapshot(t, dir)
				for range 2 {
					loaded, err := s.CatalogForCompleteness(t.Context(), f.catalog)
					if err != nil {
						t.Fatal(err)
					}
					if loaded.Catalog != nil || loaded.Raw != nil || loaded.Digest != f.catalog {
						t.Fatalf("partial catalog invented data: %+v", loaded)
					}
					if loaded.Stats != (CatalogStats{CachedManifests: manifests}) {
						t.Fatalf("partial catalog stats = %+v", loaded.Stats)
					}
					if len(loaded.CatalogProblems) != 1 {
						t.Fatalf("problems = %+v, want only the known %s", loaded.CatalogProblems, kind)
					}
					problem := loaded.CatalogProblems[0]
					if problem.Kind != kind || problem.Digest != dgst || fault.KindOf(problem.Err) != fault.Offline || !errors.Is(problem.Err, wantErr) {
						t.Fatalf("problem = %+v, want %s wrapping %v", problem, kind, wantErr)
					}
					if kind == "catalogIndex" {
						if len(loaded.SchemaDescriptors) != 0 {
							t.Fatalf("unverified index supplied schemas: %+v", loaded.SchemaDescriptors)
						}
					} else if !reflect.DeepEqual(loaded.SchemaDescriptors, ci.Schemas) {
						t.Fatalf("verified schemas were lost: %+v", loaded.SchemaDescriptors)
					}
				}
				if *opened != 0 || f.remote.count() != 0 || !reflect.DeepEqual(before, catalogCacheSnapshot(t, dir)) {
					t.Fatal("offline completeness opened the registry or mutated the cache")
				}
				if _, err := s.Catalog(t.Context(), f.catalog); fault.KindOf(err) != fault.Offline {
					t.Fatalf("ordinary Catalog accepted an incomplete catalog: %v", err)
				}
			})
		}
	}
}

func TestCatalogForCompletenessDoesNotGuessDescendants(t *testing.T) {
	for _, removeIndex := range []bool{false, true} {
		t.Run(map[bool]string{false: "metadata and payload", true: "entire catalog"}[removeIndex], func(t *testing.T) {
			f := newFixture(t)
			dir := t.TempDir()
			s, opened := f.store(t, dir, true)
			cacheCatalogFixture(t, s, f)
			missing := []string{f.metadata, mustPayload(t, f).Digest.String()}
			kind, dgst, manifests := "catalogMetadata", f.metadata, 1
			if removeIndex {
				missing = append(missing, f.catalog)
				kind, dgst, manifests = "catalogIndex", f.catalog, 0
			}
			for _, dgst := range missing {
				if err := os.Remove(blobFile(dir, dgst)); err != nil {
					t.Fatal(err)
				}
			}
			before := catalogCacheSnapshot(t, dir)
			loaded, err := s.CatalogForCompleteness(t.Context(), f.catalog)
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded.CatalogProblems) != 1 || loaded.CatalogProblems[0].Kind != kind || loaded.CatalogProblems[0].Digest != dgst {
				t.Fatalf("unknown descendant was invented: %+v", loaded.CatalogProblems)
			}
			if loaded.Stats != (CatalogStats{CachedManifests: manifests}) || *opened != 0 || !reflect.DeepEqual(before, catalogCacheSnapshot(t, dir)) {
				t.Fatalf("unknown descendants affected verified-only work: %+v", loaded.Stats)
			}
		})
	}
}

func TestCatalogForCompletenessOnlineDelegatesAndOfflineCountsPerLoad(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	s, opened := f.store(t, dir, false)
	loaded, err := s.CatalogForCompleteness(t.Context(), f.catalog)
	if err != nil {
		t.Fatal(err)
	}
	bytesFetched := int64(len(f.remote.blobs[f.catalog]) + len(f.remote.blobs[f.metadata]) + len(f.remote.blobs[mustPayload(t, f).Digest.String()]))
	if loaded.Catalog == nil || loaded.Stats != (CatalogStats{FetchedManifests: 2, FetchedBlobs: 1, FetchedBytes: bytesFetched}) || *opened != 1 {
		t.Fatalf("online completeness = %+v, opened %d", loaded, *opened)
	}
	offline, offlineOpened := f.store(t, dir, true)
	ci, err := artifact.ParseCatalogIndex(f.remote.blobs[f.catalog], artifact.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	before := catalogCacheSnapshot(t, dir)
	for range 2 {
		loaded, err := offline.CatalogForCompleteness(t.Context(), f.catalog)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Catalog == nil || len(loaded.CatalogProblems) != 0 || loaded.Stats != (CatalogStats{CachedManifests: 2, CachedBlobs: 1}) || !reflect.DeepEqual(loaded.SchemaDescriptors, ci.Schemas) || !bytes.Equal(loaded.Raw, f.remote.blobs[mustPayload(t, f).Digest.String()]) {
			t.Fatalf("cached completeness = %+v", loaded)
		}
	}
	if *offlineOpened != 0 || !reflect.DeepEqual(before, catalogCacheSnapshot(t, dir)) {
		t.Fatal("warm offline completeness changed the cache or opened the registry")
	}
}

func TestCatalogForCompletenessManifestValidityAndSizeProblems(t *testing.T) {
	for _, kind := range []string{"catalogIndex", "catalogMetadata", "catalogPayload"} {
		for _, defect := range []string{"malformed", "declared size smaller", "declared size larger"} {
			if kind == "catalogIndex" && defect != "malformed" || kind == "catalogPayload" && defect == "malformed" {
				continue
			}
			for _, offline := range []bool{false, true} {
				t.Run(kind+"/"+defect+"/"+map[bool]string{false: "online", true: "offline"}[offline], func(t *testing.T) {
					f := newFixture(t)
					var index string
					delta := 1
					if defect == "declared size smaller" {
						delta = -1
					}
					switch kind {
					case "catalogIndex":
						index, _ = newerManifest(t, f, f.catalog, func(m map[string]any) { m["schemaVersion"] = 1 })
					case "catalogMetadata":
						dgst := f.metadata
						data := f.remote.blobs[dgst]
						if defect == "malformed" {
							dgst, data = newerManifest(t, f, dgst, func(m map[string]any) { m["schemaVersion"] = 1 })
							delta = 0
						}
						index, _ = newerManifest(t, f, f.catalog, func(m map[string]any) {
							setChild(0, "digest", dgst)(m)
							setChild(0, "size", len(data)+delta)(m)
						})
					case "catalogPayload":
						dgst := mustPayload(t, f).Digest.String()
						metadata, data := newerManifest(t, f, f.metadata, func(m map[string]any) {
							m["layers"].([]any)[0].(map[string]any)["size"] = len(f.remote.blobs[dgst]) + delta
						})
						index, _ = newerManifest(t, f, f.catalog, func(m map[string]any) {
							setChild(0, "digest", metadata)(m)
							setChild(0, "size", len(data))(m)
						})
					}
					dir := t.TempDir()
					s, opened := f.store(t, dir, offline)
					cacheCatalogFixture(t, s, f)
					before := catalogCacheSnapshot(t, dir)
					for range 2 {
						loaded, err := s.CatalogForCompleteness(t.Context(), index)
						if loaded != nil || fault.KindOf(err) != fault.Integrity || !errors.Is(err, artifact.ErrInvalid) || errors.Is(err, cache.ErrCorrupt) {
							t.Fatalf("invalid pinned catalog = %+v, %v", loaded, err)
						}
					}
					if *opened != 0 || f.remote.count() != 0 || !reflect.DeepEqual(before, catalogCacheSnapshot(t, dir)) {
						t.Fatal("invalid pinned catalog opened the registry or changed cache")
					}
				})
			}
		}
	}
}

func TestCatalogForCompletenessInvalidPinnedGraphRemainsFatal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata bool
		edit     func(map[string]any)
	}{
		{"duplicate metadata child", false, func(m map[string]any) {
			m["manifests"] = append(m["manifests"].([]any), m["manifests"].([]any)[0])
		}},
		{"duplicate schema child", false, func(m map[string]any) {
			m["manifests"] = append(m["manifests"].([]any), m["manifests"].([]any)[1])
		}},
		{"missing metadata child", false, func(m map[string]any) {
			m["manifests"] = m["manifests"].([]any)[1:]
		}},
		{"invalid child digest", false, setChild(1, "digest", "not-a-digest")},
		{"invalid child size", false, setChild(1, "size", 0)},
		{"external child URLs", false, setChild(1, "urls", []any{"https://example.invalid/schema"})},
		{"invalid metadata config", true, func(m map[string]any) {
			m["config"].(map[string]any)["size"] = 3
		}},
		{"duplicate metadata payload", true, func(m map[string]any) {
			m["layers"] = append(m["layers"].([]any), m["layers"].([]any)[0])
		}},
		{"invalid payload digest", true, func(m map[string]any) {
			m["layers"].([]any)[0].(map[string]any)["digest"] = "not-a-digest"
		}},
		{"invalid payload size", true, func(m map[string]any) {
			m["layers"].([]any)[0].(map[string]any)["size"] = 0
		}},
		{"external payload URLs", true, func(m map[string]any) {
			m["layers"].([]any)[0].(map[string]any)["urls"] = []any{"https://example.invalid/catalog"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, offline := range []bool{false, true} {
				t.Run(map[bool]string{false: "online", true: "offline"}[offline], func(t *testing.T) {
					f := newFixture(t)
					var index string
					if tc.metadata {
						metadata, data := newerManifest(t, f, f.metadata, tc.edit)
						index, _ = newerManifest(t, f, f.catalog, func(m map[string]any) {
							setChild(0, "digest", metadata)(m)
							setChild(0, "size", len(data))(m)
						})
					} else {
						index, _ = newerManifest(t, f, f.catalog, tc.edit)
					}
					dir := t.TempDir()
					s, opened := f.store(t, dir, offline)
					cacheCatalogFixture(t, s, f)
					before := catalogCacheSnapshot(t, dir)
					for range 2 {
						loaded, err := s.CatalogForCompleteness(t.Context(), index)
						if loaded != nil || fault.KindOf(err) != fault.Integrity || !errors.Is(err, artifact.ErrInvalid) || errors.Is(err, cache.ErrCorrupt) {
							t.Fatalf("invalid pinned graph = %+v, %v", loaded, err)
						}
					}
					if *opened != 0 || f.remote.count() != 0 || !reflect.DeepEqual(before, catalogCacheSnapshot(t, dir)) {
						t.Fatal("invalid pinned graph opened the registry or changed cache")
					}
				})
			}
		})
	}
}

func TestCatalogForCompletenessUnsupportedRemainsFatal(t *testing.T) {
	for _, artifactKind := range []string{"index", "metadata", "payload media type", "catalog format"} {
		t.Run(artifactKind, func(t *testing.T) {
			f := newFixture(t)
			index := f.catalog
			switch artifactKind {
			case "index":
				index, _ = newerManifest(t, f, f.catalog, setArtifactType("application/vnd.ovineko.schepherd.catalog.v3"))
			case "metadata", "payload media type":
				edit := setArtifactType("application/vnd.ovineko.schepherd.catalog-metadata.v3")
				if artifactKind == "payload media type" {
					edit = setPayloadType("application/vnd.ovineko.schepherd.catalog.v3+json")
				}
				metadata, data := newerManifest(t, f, f.metadata, edit)
				index, _ = newerManifest(t, f, f.catalog, func(m map[string]any) {
					setChild(0, "digest", metadata)(m)
					setChild(0, "size", len(data))(m)
				})
			case "catalog format":
				index, _ = f.addCatalog(t, []byte(`{"formatVersion":3}`), nil)
			}
			dir := t.TempDir()
			s, opened := f.store(t, dir, true)
			cacheCatalogFixture(t, s, f)
			before := catalogCacheSnapshot(t, dir)
			loaded, err := s.CatalogForCompleteness(t.Context(), index)
			if loaded != nil || fault.KindOf(err) != fault.Usage {
				t.Fatalf("unsupported catalog = %+v, %v", loaded, err)
			}
			if artifactKind != "catalog format" {
				assertUnsupported(t, err)
			}
			if *opened != 0 || !reflect.DeepEqual(before, catalogCacheSnapshot(t, dir)) {
				t.Fatal("unsupported catalog opened the registry or changed cache")
			}
		})
	}
}

func TestCatalogForCompletenessInvalidCatalogRemainsFatal(t *testing.T) {
	for _, raw := range []string{`{`, `{"formatVersion":2,"revision":"invalid","schemas":[]}`} {
		f := newFixture(t)
		index, _ := f.addCatalog(t, []byte(raw), nil)
		dir := t.TempDir()
		s, opened := f.store(t, dir, true)
		cacheCatalogFixture(t, s, f)
		before := catalogCacheSnapshot(t, dir)
		loaded, err := s.CatalogForCompleteness(t.Context(), index)
		if loaded != nil || fault.KindOf(err) != fault.Integrity || errors.Is(err, cache.ErrCorrupt) {
			t.Fatalf("invalid pinned catalog = %+v, %v", loaded, err)
		}
		if *opened != 0 || !reflect.DeepEqual(before, catalogCacheSnapshot(t, dir)) {
			t.Fatal("invalid pinned catalog opened the registry or changed cache")
		}
	}
}

func TestCatalogForCompletenessUnexpectedCacheFaultRemainsFatal(t *testing.T) {
	for _, stage := range []struct {
		name  string
		check int
	}{{"index", 1}, {"metadata", 2}, {"payload", 3}} {
		t.Run(stage.name, func(t *testing.T) {
			f := newFixture(t)
			dir := t.TempDir()
			s, opened := f.store(t, dir, true)
			cacheCatalogFixture(t, s, f)
			before := catalogCacheSnapshot(t, dir)
			ctx := &catalogCheckpointContext{Context: t.Context(), check: stage.check, action: func() {
				if err := s.opts.Cache.Close(); err != nil {
					t.Fatal(err)
				}
			}}
			loaded, err := s.CatalogForCompleteness(ctx, f.catalog)
			if loaded != nil || fault.KindOf(err) != fault.Internal || *opened != 0 {
				t.Fatalf("closed cache = %+v, %v (opened %d)", loaded, err, *opened)
			}
			if !reflect.DeepEqual(before, catalogCacheSnapshot(t, dir)) {
				t.Fatal("unexpected cache failure mutated the cache")
			}
		})
	}
}

func TestCatalogForCompletenessContextAndPinErrors(t *testing.T) {
	f := newFixture(t)
	for _, warm := range []bool{false, true} {
		t.Run(map[bool]string{false: "cold", true: "warm"}[warm], func(t *testing.T) {
			dir := t.TempDir()
			s, opened := f.store(t, dir, true)
			if warm {
				cacheCatalogFixture(t, s, f)
			}
			before := catalogCacheSnapshot(t, dir)
			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			deadline, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			defer cancel()
			for _, ctx := range []context.Context{canceled, deadline} {
				kind := fault.Registry
				if errors.Is(ctx.Err(), context.Canceled) {
					kind = fault.Canceled
				}
				loaded, err := s.CatalogForCompleteness(ctx, f.catalog)
				if loaded != nil || fault.KindOf(err) != kind || !errors.Is(err, ctx.Err()) {
					t.Fatalf("context error = %+v, %v, want %s", loaded, err, kind)
				}
			}
			loaded, err := s.CatalogForCompleteness(t.Context(), "not a digest")
			if loaded != nil || fault.KindOf(err) != fault.Usage {
				t.Fatalf("invalid pin error = %+v, %v, want %s", loaded, err, fault.Usage)
			}
			if *opened != 0 || !reflect.DeepEqual(before, catalogCacheSnapshot(t, dir)) {
				t.Fatal("context/pin error opened the registry or changed cache")
			}
		})
	}
}

type catalogCheckpointContext struct {
	//nolint:containedctx // Test-only decorator interrupts catalog reads at deterministic checkpoints.
	context.Context

	action func()
	check  int
	calls  int
}

func (c *catalogCheckpointContext) Err() error {
	c.calls++
	if c.calls == c.check {
		c.action()
	}
	return c.Context.Err()
}

func TestCatalogForCompletenessCancellationBetweenArtifacts(t *testing.T) {
	for _, check := range []int{2, 3, 4} {
		f := newFixture(t)
		dir := t.TempDir()
		s, opened := f.store(t, dir, true)
		cacheCatalogFixture(t, s, f)
		before := catalogCacheSnapshot(t, dir)
		base, cancel := context.WithCancel(t.Context())
		defer cancel()
		ctx := &catalogCheckpointContext{Context: base, check: check, action: cancel}
		_, err := s.CatalogForCompleteness(ctx, f.catalog)
		if fault.KindOf(err) != fault.Canceled || !errors.Is(err, context.Canceled) || *opened != 0 {
			t.Fatalf("cancellation at checkpoint %d = %v (opened %d)", check, err, *opened)
		}
		if !reflect.DeepEqual(before, catalogCacheSnapshot(t, dir)) {
			t.Fatal("canceled catalog load mutated the cache")
		}
	}
}
