package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

func completeDescriptor(entry *catalog.Entry) ocispec.Descriptor {
	return ocispec.Descriptor{MediaType: entry.Artifact.MediaType, Digest: godigest.Digest(entry.Artifact.Digest), Size: entry.Artifact.Size}
}

func completeManifest(t *testing.T, f *fixture, id string) *artifact.SchemaManifest {
	t.Helper()
	sm, err := artifact.ParseSchemaManifest(f.remote.blobs[f.entries[id].Artifact.Digest], artifact.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return sm
}

func TestEnsureOfflineUnexpectedErrorsRemainFatal(t *testing.T) {
	for _, operation := range []string{"manifest", "payload", "schema file", "notice"} {
		t.Run(operation, func(t *testing.T) {
			f := newFixture(t)
			s, opened := f.store(t, t.TempDir(), true)
			if err := s.opts.Cache.Close(); err != nil {
				t.Fatal(err)
			}
			entry := f.entries["gamma"]
			sm := completeManifest(t, f, "gamma")
			var err error
			switch operation {
			case "manifest":
				_, _, err = s.EnsureManifest(t.Context(), entry, completeDescriptor(entry))
			case "payload":
				_, err = s.EnsurePayload(t.Context(), entry.ID, sm)
			case "schema file":
				_, err = s.EnsureSchemaFile(t.Context(), entry, sm)
			case "notice":
				_, err = s.EnsureNotice(t.Context(), entry.ID, *sm.Notice)
			}
			if err == nil || fault.KindOf(err) != fault.Internal || *opened != 0 {
				t.Fatalf("closed cache = %v (opened %d), want internal error without registry access", err, *opened)
			}
		})
	}
}

func TestEnsureOfflineUnsupportedManifestIsNotMissing(t *testing.T) {
	f := newFixture(t)
	dgst, data := newerManifest(t, f, f.entries["alpha"].Artifact.Digest, setArtifactType("application/vnd.ovineko.schepherd.schema.v3"))
	dir := t.TempDir()
	s, opened := f.store(t, dir, true)
	if err := s.opts.Cache.WriteBlob(dgst, int64(len(data)), int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	entry := entryFor("alpha", dgst, len(data))
	_, outcome, err := s.EnsureManifest(t.Context(), entry, completeDescriptor(entry))
	assertUnsupported(t, err)
	if outcome != (EnsureOutcome{}) || *opened != 0 || len(quarantined(t, dir)) != 0 {
		t.Fatalf("unsupported manifest changed cache or fetched: %+v, opened %d", outcome, *opened)
	}
	if err := s.opts.Cache.VerifyBlob(dgst, int64(len(data))); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureManifestInvalidPinnedArtifactIsFatal(t *testing.T) {
	for name, edit := range map[string]func(map[string]any){
		"invalid schema version": func(m map[string]any) { m["schemaVersion"] = 1 },
		"invalid config": func(m map[string]any) {
			m["config"].(map[string]any)["size"] = 3
		},
		"duplicate payload": func(m map[string]any) {
			m["layers"] = append(m["layers"].([]any), m["layers"].([]any)[0])
		},
		"invalid payload digest": func(m map[string]any) {
			m["layers"].([]any)[0].(map[string]any)["digest"] = "not-a-digest"
		},
		"invalid payload size": func(m map[string]any) {
			m["layers"].([]any)[0].(map[string]any)["size"] = 0
		},
		"missing content digest": func(m map[string]any) {
			delete(m["layers"].([]any)[0].(map[string]any)["annotations"].(map[string]any), artifact.AnnotationContentDigest)
		},
		"invalid content size": func(m map[string]any) {
			m["layers"].([]any)[0].(map[string]any)["annotations"].(map[string]any)[artifact.AnnotationContentSize] = "017"
		},
		"external payload URLs": func(m map[string]any) {
			m["layers"].([]any)[0].(map[string]any)["urls"] = []any{"https://example.invalid/schema"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, offline := range []bool{false, true} {
				t.Run(map[bool]string{false: "online", true: "offline"}[offline], func(t *testing.T) {
					f := newFixture(t)
					dgst, data := newerManifest(t, f, f.entries["alpha"].Artifact.Digest, edit)
					dir := t.TempDir()
					s, opened := f.store(t, dir, offline)
					if err := s.opts.Cache.WriteBlob(dgst, int64(len(data)), int64(len(data)), bytes.NewReader(data)); err != nil {
						t.Fatal(err)
					}
					entry := entryFor("alpha", dgst, len(data))
					before := catalogCacheSnapshot(t, dir)
					for range 2 {
						sm, outcome, err := s.EnsureManifest(t.Context(), entry, completeDescriptor(entry))
						if sm != nil || outcome != (EnsureOutcome{}) || fault.KindOf(err) != fault.Integrity || !errors.Is(err, artifact.ErrInvalid) || errors.Is(err, cache.ErrCorrupt) {
							t.Fatalf("invalid pinned schema manifest = %+v, %+v, %v", sm, outcome, err)
						}
					}
					if *opened != 0 || f.remote.count() != 0 || !reflect.DeepEqual(before, catalogCacheSnapshot(t, dir)) {
						t.Fatal("invalid pinned schema manifest opened the registry or changed cache")
					}
				})
			}
		})
	}
}

func TestEnsureManifestOfflinePhysicalCacheGaps(t *testing.T) {
	for _, damage := range []string{"missing", "same size", "different size", "non-regular"} {
		t.Run(damage, func(t *testing.T) {
			f := newFixture(t)
			entry := f.entries["alpha"]
			dir := t.TempDir()
			s, opened := f.store(t, dir, true)
			cacheCatalogFixture(t, s, f)
			desc := completeDescriptor(entry)
			path := blobFile(dir, entry.Artifact.Digest)
			wantErr := cache.ErrCorrupt
			switch damage {
			case "missing":
				wantErr = cache.ErrNotFound
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "same size":
				data := bytes.Clone(f.remote.blobs[entry.Artifact.Digest])
				data[0] ^= 1
				corruptFile(t, path, data)
			case "non-regular":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			case "different size":
				corruptFile(t, path, []byte("damaged"))
			}
			before := catalogCacheSnapshot(t, dir)
			sm, outcome, err := s.EnsureManifest(t.Context(), entry, desc)
			if sm != nil || outcome != (EnsureOutcome{}) || fault.KindOf(err) != fault.Offline || !errors.Is(err, wantErr) {
				t.Fatalf("physical schema manifest gap = %+v, %+v, %v", sm, outcome, err)
			}
			if *opened != 0 || f.remote.count() != 0 || !reflect.DeepEqual(before, catalogCacheSnapshot(t, dir)) {
				t.Fatal("offline physical schema manifest gap opened the registry or changed cache")
			}
		})
	}
}

func TestEnsureDigestValidDeclaredSizeMismatchIsFatal(t *testing.T) {
	for _, role := range []string{"manifest", "payload", "notice"} {
		for _, delta := range []int64{-1, 1} {
			direction := map[int64]string{-1: "smaller", 1: "larger"}[delta]
			for _, offline := range []bool{false, true} {
				t.Run(role+"/"+direction+"/"+map[bool]string{false: "online", true: "offline"}[offline], func(t *testing.T) {
					f := newFixture(t)
					dir := t.TempDir()
					s, opened := f.store(t, dir, offline)
					cacheCatalogFixture(t, s, f)
					entry := f.entries["gamma"]
					sm := completeManifest(t, f, entry.ID)
					desc := completeDescriptor(entry)
					switch role {
					case "manifest":
						desc.Size += delta
						entry.Artifact.Size = desc.Size
					case "payload":
						sm.Payload.Size += delta
					case "notice":
						sm.Notice.Size += delta
					}
					before := catalogCacheSnapshot(t, dir)
					for range 2 {
						var outcome EnsureOutcome
						var err error
						switch role {
						case "manifest":
							var parsed *artifact.SchemaManifest
							parsed, outcome, err = s.EnsureManifest(t.Context(), entry, desc)
							if parsed != nil {
								t.Fatalf("size-invalid manifest supplied parsed data: %+v", parsed)
							}
						case "payload":
							outcome, err = s.EnsurePayload(t.Context(), entry.ID, sm)
						case "notice":
							outcome, err = s.EnsureNotice(t.Context(), entry.ID, *sm.Notice)
						}
						if outcome != (EnsureOutcome{}) || fault.KindOf(err) != fault.Integrity || !errors.Is(err, artifact.ErrInvalid) || errors.Is(err, cache.ErrCorrupt) {
							t.Fatalf("digest-valid size mismatch = %+v, %v", outcome, err)
						}
					}
					if *opened != 0 || f.remote.count() != 0 || !reflect.DeepEqual(before, catalogCacheSnapshot(t, dir)) {
						t.Fatal("digest-valid size mismatch opened the registry or changed cache")
					}
				})
			}
		}
	}
}

func TestEnsureSchemaFileOfflineDoesNotReconstruct(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	online, _ := f.store(t, dir, false)
	entry := f.entries["gamma"]
	sch, err := online.Materialize(t.Context(), entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sch.Path); err != nil {
		t.Fatal(err)
	}
	offline, opened := f.store(t, dir, true)
	sm := completeManifest(t, f, "gamma")
	outcome, err := offline.EnsureSchemaFile(t.Context(), entry, sm)
	if fault.KindOf(err) != fault.Offline || !errors.Is(err, cache.ErrNotFound) || outcome != (EnsureOutcome{}) || *opened != 0 {
		t.Fatalf("offline missing schema = %+v, %v (opened %d)", outcome, err, *opened)
	}
	if _, err := os.Lstat(sch.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("offline verification reconstructed schema: %v", err)
	}
	if len(quarantined(t, dir)) != 0 {
		t.Fatal("offline verification quarantined content")
	}
	if _, err := offline.Materialize(t.Context(), entry); err != nil {
		t.Fatalf("lazy Materialize no longer reconstructs offline: %v", err)
	}
}

func TestEnsureCachedContextErrors(t *testing.T) {
	f := newFixture(t)
	s, _ := f.store(t, t.TempDir(), false)
	entry := f.entries["gamma"]
	sch, err := s.Materialize(t.Context(), entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Notice(t.Context(), sch); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Catalog(t.Context(), f.catalog); err != nil {
		t.Fatal(err)
	}
	sm := completeManifest(t, f, "gamma")
	before := f.remote.count()
	for _, deadline := range []bool{false, true} {
		name, kind := "canceled", fault.Canceled
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if deadline {
			name, kind = "deadline", fault.Registry
			ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			defer cancel()
		}
		t.Run(name, func(t *testing.T) {
			operations := map[string]func() error{
				"manifest":    func() error { _, _, err := s.EnsureManifest(ctx, entry, completeDescriptor(entry)); return err },
				"payload":     func() error { _, err := s.EnsurePayload(ctx, entry.ID, sm); return err },
				"schema file": func() error { _, err := s.EnsureSchemaFile(ctx, entry, sm); return err },
				"notice":      func() error { _, err := s.EnsureNotice(ctx, entry.ID, *sm.Notice); return err },
				"catalog":     func() error { _, err := s.Catalog(ctx, f.catalog); return err },
			}
			for operation, run := range operations {
				if err := run(); !errors.Is(err, ctx.Err()) || fault.KindOf(err) != kind {
					t.Errorf("cached %s = %v, want %s wrapping %v", operation, err, kind, ctx.Err())
				}
			}
		})
	}
	if f.remote.count() != before {
		t.Fatal("interrupted cached ensures fetched artifacts")
	}
}

type observedWaitContext struct {
	//nolint:containedctx // Test-only decorator observes cache-lock waits without adding production hooks.
	context.Context

	waiting chan struct{}
	once    sync.Once
}

func (c *observedWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestEnsureManifestReusesCacheAfterWaitingForLock(t *testing.T) {
	f := newFixture(t)
	s, opened := f.store(t, t.TempDir(), false)
	entry := f.entries["gamma"]
	desc := completeDescriptor(entry)
	release, err := s.opts.Cache.Lock(t.Context(), digest.Hex(entry.Artifact.Digest))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	base, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx := &observedWaitContext{Context: base, waiting: make(chan struct{})}
	type result struct {
		sm      *artifact.SchemaManifest
		outcome EnsureOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() { sm, outcome, err := s.EnsureManifest(ctx, entry, desc); done <- result{sm, outcome, err} }()
	select {
	case <-ctx.waiting:
	case got := <-done:
		t.Fatalf("ensure did not wait for manifest lock: %+v", got)
	case <-base.Done():
		t.Fatal("ensure never waited for manifest lock")
	}
	data := f.remote.blobs[entry.Artifact.Digest]
	if err := s.opts.Cache.WriteBlob(entry.Artifact.Digest, desc.Size, desc.Size, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	release()
	got := <-done
	if got.err != nil || got.sm == nil || got.outcome != (EnsureOutcome{}) || *opened != 0 || f.remote.count() != 0 {
		t.Fatalf("manifest lock reuse = %+v (opened %d, requests %d)", got, *opened, f.remote.count())
	}
}

func TestEnsureSchemaFileConcurrentRepairKeepsHealthyReplacement(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	s, _ := f.store(t, dir, false)
	entry := f.entries["gamma"]
	sch, err := s.Materialize(t.Context(), entry)
	if err != nil {
		t.Fatal(err)
	}
	sm := completeManifest(t, f, "gamma")
	corruptFile(t, sch.Path, []byte("broken schema"))
	release, err := s.opts.Cache.Lock(t.Context(), digest.Hex(entry.Artifact.Digest))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	base, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx := &observedWaitContext{Context: base, waiting: make(chan struct{})}
	type result struct {
		outcome EnsureOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() { outcome, err := s.EnsureSchemaFile(ctx, entry, sm); done <- result{outcome, err} }()
	select {
	case <-ctx.waiting:
	case got := <-done:
		t.Fatalf("ensure did not serialize schema repair: %+v", got)
	case <-base.Done():
		t.Fatal("ensure never waited for schema repair lock")
	}
	if _, err := s.opts.Cache.WriteMaterialized(entry.Artifact.Digest, sm.ContentDigest, sm.ContentSize, func(w io.Writer) error {
		_, err := w.Write(f.content["gamma"])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(sch.Path)
	if err != nil {
		t.Fatal(err)
	}
	quarantineBefore := quarantined(t, dir)
	if err := os.Remove(blobFile(dir, sm.Payload.Digest.String())); err != nil {
		t.Fatal(err)
	}
	release()
	got := <-done
	if got.err != nil || got.outcome != (EnsureOutcome{}) {
		t.Fatalf("healthy concurrent replacement not reused: %+v", got)
	}
	after, err := os.Stat(sch.Path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("healthy schema was replaced or lost: %v", err)
	}
	if !reflect.DeepEqual(quarantineBefore, quarantined(t, dir)) {
		t.Fatal("stale failed verification quarantined healthy schema")
	}
	if err := s.opts.Cache.VerifyMaterialized(entry.Artifact.Digest, sm.ContentDigest, sm.ContentSize); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureOutcomesColdWarmAndRepair(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	s, _ := f.store(t, dir, false)
	entry := f.entries["gamma"]
	sm := completeManifest(t, f, "gamma")
	operations := []struct {
		name   string
		path   string
		size   int64
		ensure func() (EnsureOutcome, error)
	}{
		{"manifest", blobFile(dir, entry.Artifact.Digest), entry.Artifact.Size, func() (EnsureOutcome, error) {
			_, outcome, err := s.EnsureManifest(t.Context(), entry, completeDescriptor(entry))
			return outcome, err
		}},
		{"payload", blobFile(dir, sm.Payload.Digest.String()), sm.Payload.Size, func() (EnsureOutcome, error) { return s.EnsurePayload(t.Context(), entry.ID, sm) }},
		{"schema file", s.opts.Cache.SchemaPath(entry.Artifact.Digest), 0, func() (EnsureOutcome, error) { return s.EnsureSchemaFile(t.Context(), entry, sm) }},
		{"notice", blobFile(dir, sm.Notice.Digest.String()), sm.Notice.Size, func() (EnsureOutcome, error) { return s.EnsureNotice(t.Context(), entry.ID, *sm.Notice) }},
	}
	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			want := EnsureOutcome{Fetched: op.size > 0, Written: true, FetchedBytes: op.size}
			if got, err := op.ensure(); err != nil || got != want {
				t.Fatalf("cold = %+v, %v; want %+v", got, err, want)
			}
			if got, err := op.ensure(); err != nil || got != (EnsureOutcome{}) {
				t.Fatalf("warm = %+v, %v", got, err)
			}
			corruptFile(t, op.path, []byte("damaged"))
			want.Repaired = true
			if got, err := op.ensure(); err != nil || got != want {
				t.Fatalf("repair = %+v, %v; want %+v", got, err, want)
			}
		})
	}
}

func TestCatalogStatsColdWarmPartialAndDistinctRepairs(t *testing.T) {
	f := newFixture(t)
	payload := mustPayload(t, f).Digest.String()
	allBytes := int64(len(f.remote.blobs[f.catalog]) + len(f.remote.blobs[f.metadata]) + len(f.remote.blobs[payload]))
	for _, tc := range []struct {
		name    string
		damaged []string
		missing string
		want    CatalogStats
	}{
		{"warm", nil, "", CatalogStats{CachedManifests: 2, CachedBlobs: 1}},
		{"missing payload", nil, payload, CatalogStats{CachedManifests: 2, FetchedBlobs: 1, FetchedBytes: int64(len(f.remote.blobs[payload]))}},
		{"repair index", []string{f.catalog}, "", CatalogStats{FetchedManifests: 1, CachedManifests: 1, CachedBlobs: 1, RepairedEntries: 1, FetchedBytes: int64(len(f.remote.blobs[f.catalog]))}},
		{"repair metadata", []string{f.metadata}, "", CatalogStats{FetchedManifests: 1, CachedManifests: 1, CachedBlobs: 1, RepairedEntries: 1, FetchedBytes: int64(len(f.remote.blobs[f.metadata]))}},
		{"repair payload", []string{payload}, "", CatalogStats{CachedManifests: 2, FetchedBlobs: 1, RepairedEntries: 1, FetchedBytes: int64(len(f.remote.blobs[payload]))}},
		{"repair all", []string{f.catalog, f.metadata, payload}, "", CatalogStats{FetchedManifests: 2, FetchedBlobs: 1, RepairedEntries: 3, FetchedBytes: allBytes}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s, _ := f.store(t, dir, false)
			cold, err := s.Catalog(t.Context(), f.catalog)
			if err != nil {
				t.Fatal(err)
			}
			if want := (CatalogStats{FetchedManifests: 2, FetchedBlobs: 1, FetchedBytes: allBytes}); cold.Stats != want {
				t.Fatalf("cold stats = %+v, want %+v", cold.Stats, want)
			}
			for _, dgst := range tc.damaged {
				corruptFile(t, blobFile(dir, dgst), []byte("damaged"))
			}
			if tc.missing != "" {
				if err := os.Remove(blobFile(dir, tc.missing)); err != nil {
					t.Fatal(err)
				}
			}
			before := f.remote.count()
			loaded, err := s.Catalog(t.Context(), f.catalog)
			if err != nil || loaded == nil {
				t.Fatalf("catalog recovery: %v", err)
			}
			if loaded.Stats != tc.want {
				t.Fatalf("stats = %+v, want %+v", loaded.Stats, tc.want)
			}
			if got := f.remote.count() - before; got != tc.want.FetchedManifests+tc.want.FetchedBlobs {
				t.Errorf("requests = %d, stats claim %d", got, tc.want.FetchedManifests+tc.want.FetchedBlobs)
			}
			offline, opened := f.store(t, dir, true)
			warm, err := offline.Catalog(t.Context(), f.catalog)
			if err != nil || warm == nil || warm.Stats != (CatalogStats{CachedManifests: 2, CachedBlobs: 1}) || *opened != 0 {
				t.Fatalf("offline after load = %+v, %v (opened %d)", warm, err, *opened)
			}
		})
	}
}

func TestEnsureOfflineCorruptArtifactsAreVerificationOnly(t *testing.T) {
	for _, kind := range []string{"manifest", "payload", "schema file", "notice"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			dir := t.TempDir()
			online, _ := f.store(t, dir, false)
			entry := f.entries["gamma"]
			sch, err := online.Materialize(t.Context(), entry)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := online.Notice(t.Context(), sch); err != nil {
				t.Fatal(err)
			}
			sm := completeManifest(t, f, "gamma")
			offline, opened := f.store(t, dir, true)
			var path string
			var ensure func() (EnsureOutcome, error)
			switch kind {
			case "manifest":
				path = blobFile(dir, entry.Artifact.Digest)
				ensure = func() (EnsureOutcome, error) {
					_, outcome, err := offline.EnsureManifest(t.Context(), entry, completeDescriptor(entry))
					return outcome, err
				}
			case "payload":
				path = blobFile(dir, sm.Payload.Digest.String())
				ensure = func() (EnsureOutcome, error) { return offline.EnsurePayload(t.Context(), entry.ID, sm) }
			case "schema file":
				path = sch.Path
				ensure = func() (EnsureOutcome, error) { return offline.EnsureSchemaFile(t.Context(), entry, sm) }
			case "notice":
				path = blobFile(dir, sm.Notice.Digest.String())
				ensure = func() (EnsureOutcome, error) { return offline.EnsureNotice(t.Context(), entry.ID, *sm.Notice) }
			}
			damaged := []byte("damaged")
			corruptFile(t, path, damaged)
			outcome, err := ensure()
			if fault.KindOf(err) != fault.Offline || !errors.Is(err, cache.ErrCorrupt) || outcome != (EnsureOutcome{}) || *opened != 0 {
				t.Fatalf("offline corrupt %s = %+v, %v (opened %d)", kind, outcome, err, *opened)
			}
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, damaged) {
				t.Fatalf("offline verification changed corrupt artifact: %v", err)
			}
			if len(quarantined(t, dir)) != 0 {
				t.Fatal("offline verification quarantined content")
			}
		})
	}
}
