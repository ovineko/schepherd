package store

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

func catalogRepairIndexes(t *testing.T, f *fixture, child string) []string {
	t.Helper()
	metadata := f.metadata
	metadataData := f.remote.blobs[metadata]
	if child == "payload" {
		metadata, metadataData = newerManifest(t, f, metadata, func(m map[string]any) {
			m["annotations"] = map[string]string{"org.example.fixture": "alternate metadata"}
		})
		if _, err := artifact.ParseCatalogMetadata(metadataData, artifact.DefaultLimits()); err != nil {
			t.Fatal(err)
		}
	}
	index, data := newerManifest(t, f, f.catalog, func(m map[string]any) {
		m["annotations"] = map[string]string{"org.example.fixture": "alternate index"}
		for _, value := range m["manifests"].([]any) {
			child := value.(map[string]any)
			if child["digest"] == f.metadata {
				child["digest"] = metadata
				child["size"] = len(metadataData)
			}
		}
	})
	parsed, err := artifact.ParseCatalogIndex(data, artifact.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if index == f.catalog || parsed.Metadata.Digest.String() != metadata {
		t.Fatal("fixture did not produce a distinct index with the intended metadata")
	}
	if child == "payload" && metadata == f.metadata {
		t.Fatal("payload fixture did not produce distinct metadata")
	}
	return []string{f.catalog, index}
}

func TestCatalogConcurrentChildRepairReusesVerifiedReplacement(t *testing.T) {
	for _, child := range []string{"metadata", "payload"} {
		for _, damage := range []string{"missing", "corrupt"} {
			t.Run(child+"/"+damage, func(t *testing.T) {
				f := newFixture(t)
				indexes := catalogRepairIndexes(t, f, child)
				dir := t.TempDir()
				first, firstOpened := f.store(t, dir, false)
				second, secondOpened := f.store(t, dir, false)
				stores := []*Store{first, second}
				cacheCatalogFixture(t, first, f)
				baseline := make([]*LoadedCatalog, len(indexes))
				for i, index := range indexes {
					loaded, err := stores[i].Catalog(t.Context(), index)
					if err != nil {
						t.Fatal(err)
					}
					baseline[i] = loaded
				}
				dgst := f.metadata
				if child == "payload" {
					dgst = mustPayload(t, f).Digest.String()
				}
				path := blobFile(dir, dgst)
				data := f.remote.blobs[dgst]
				if damage == "missing" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				} else {
					corruptFile(t, path, bytes.Repeat([]byte{'!'}, len(data)))
				}
				release, err := first.opts.Cache.Lock(t.Context(), digest.Hex(dgst))
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				base, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				type result struct {
					loaded *LoadedCatalog
					err    error
				}
				contexts := make([]*observedWaitContext, len(indexes))
				done := make([]chan result, len(indexes))
				for i, index := range indexes {
					ctx := &observedWaitContext{Context: base, waiting: make(chan struct{})}
					contexts[i] = ctx
					finished := make(chan result, 1)
					done[i] = finished
					go func(s *Store, index string) {
						loaded, err := s.Catalog(ctx, index)
						finished <- result{loaded, err}
					}(stores[i], index)
				}
				for i, ctx := range contexts {
					select {
					case <-ctx.waiting:
					case got := <-done[i]:
						t.Fatalf("catalog %d did not wait for the %s digest lock: %+v", i, child, got)
					case <-base.Done():
						t.Fatalf("catalog %d never waited for the %s digest lock", i, child)
					}
				}
				if f.remote.count() != 0 {
					t.Fatal("catalog accessed the registry before acquiring the child digest lock")
				}
				size := int64(len(data))
				if err := first.opts.Cache.WriteBlob(dgst, size, size, bytes.NewReader(data)); err != nil {
					t.Fatal(err)
				}
				before, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				quarantineBefore := quarantined(t, dir)
				release()
				for i, finished := range done {
					select {
					case got := <-finished:
						if got.err != nil || got.loaded == nil {
							t.Fatalf("catalog %d failed after the replacement: %+v", i, got)
						}
						loaded := got.loaded
						if loaded.Digest != indexes[i] || !bytes.Equal(loaded.Raw, baseline[i].Raw) || !reflect.DeepEqual(loaded.Catalog, baseline[i].Catalog) {
							t.Fatalf("catalog %d returned different catalog data after concurrent repair", i)
						}
						if len(loaded.CatalogProblems) != 0 || !reflect.DeepEqual(loaded.SchemaDescriptors, baseline[i].SchemaDescriptors) {
							t.Fatalf("catalog %d returned an incomplete graph after concurrent repair", i)
						}
						if want := (CatalogStats{CachedManifests: 2, CachedBlobs: 1}); loaded.Stats != want {
							t.Fatalf("catalog %d stats = %+v, want %+v", i, loaded.Stats, want)
						}
					case <-base.Done():
						t.Fatalf("catalog %d did not finish after the child digest lock was released", i)
					}
				}
				if *firstOpened != 0 || *secondOpened != 0 || f.remote.count() != 0 {
					t.Fatalf("healthy replacement was refetched (opened %d and %d, requests %d)", *firstOpened, *secondOpened, f.remote.count())
				}
				after, err := os.Stat(path)
				if err != nil || !os.SameFile(before, after) {
					t.Fatalf("another index's verified replacement was removed or rewritten: %v", err)
				}
				if !reflect.DeepEqual(quarantineBefore, quarantined(t, dir)) {
					t.Fatal("a stale child lookup quarantined another command's verified replacement")
				}
			})
		}
	}
}

func TestCatalogFailedChildRepairPreservesExistingEntry(t *testing.T) {
	for _, child := range []string{"metadata", "payload"} {
		t.Run(child, func(t *testing.T) {
			f := newFixture(t)
			dir := t.TempDir()
			s, opened := f.store(t, dir, false)
			cacheCatalogFixture(t, s, f)
			dgst := f.metadata
			if child == "payload" {
				dgst = mustPayload(t, f).Digest.String()
			}
			path := blobFile(dir, dgst)
			corrupted := bytes.Repeat([]byte{'!'}, len(f.remote.blobs[dgst]))
			corruptFile(t, path, corrupted)
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			quarantineBefore := quarantined(t, dir)
			delete(f.remote.blobs, dgst)
			loaded, err := s.Catalog(t.Context(), f.catalog)
			if loaded != nil || fault.KindOf(err) != fault.Registry {
				t.Fatalf("failed repair returned catalog %+v, error %v; want a registry failure", loaded, err)
			}
			if *opened != 1 || f.remote.count() != 1 {
				t.Fatalf("failed repair did unrelated remote work (opened %d, requests %d)", *opened, f.remote.count())
			}
			after, err := os.Stat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("failed repair removed or replaced the existing cache entry: %v", err)
			}
			remaining, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(remaining, corrupted) {
				t.Fatalf("failed repair changed the existing cache bytes: %v", err)
			}
			if !reflect.DeepEqual(quarantineBefore, quarantined(t, dir)) {
				t.Fatal("failed repair quarantined the cache entry before a replacement was fetched")
			}
		})
	}
}
