package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/internal/materialize"
)

func offlineMaterializeArgs(dir, pinned string) []string {
	return []string{"--quiet", "--offline", "--repository", "registry.example/org/schemas", "--catalog", pinned, "--cache-dir", dir, "materialize", "--json"}
}

func TestMaterializeOfflineMissingCacheDoesNotCreateLayout(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "missing root"
		if existing {
			name = "empty root"
		}

		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "cache")
			if existing {
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			pinned := digest.FromBytes([]byte("absent catalog index"))
			stdout, stderr, code := run(t, offlineMaterializeArgs(dir, pinned)...)
			assertAbsentCatalogSummary(t, stdout, stderr, code, dir, pinned)
			if existing {
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 0 {
					t.Fatalf("empty root changed: entries=%v, err=%v", entries, err)
				}
			} else if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("missing cache root was created: %v", err)
			}
		})
	}
}

func TestMaterializeOfflineFromEnvironmentDoesNotCreateCache(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	pinned := digest.FromBytes([]byte("absent catalog index"))
	args := []string{"--quiet", "--repository", "registry.example/org/schemas", "--catalog", pinned, "--cache-dir", dir, "materialize", "--json"}
	stdout, stderr, code := runWith(t, map[string]string{env.KeyOffline: "true"}, args...)
	assertAbsentCatalogSummary(t, stdout, stderr, code, dir, pinned)
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing cache root was created: %v", err)
	}
}

func TestMaterializeOfflineStartupPreservesStaleEntries(t *testing.T) {
	dir := t.TempDir()
	old := time.Unix(1, 0)
	for _, rel := range []string{
		"v1/tmp/stale",
		"v1/tmp/stale-directory/partial",
		"v1/quarantine/stale",
		"v1/quarantine/entry.q1.fixed/schema.json",
	} {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Dir(path), old, old); err != nil {
			t.Fatal(err)
		}
	}

	before := offlineCacheSnapshot(t, dir)
	pinned := digest.FromBytes([]byte("absent catalog index"))
	stdout, stderr, code := run(t, offlineMaterializeArgs(dir, pinned)...)
	assertAbsentCatalogSummary(t, stdout, stderr, code, dir, pinned)
	if after := offlineCacheSnapshot(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("offline startup changed cache: before=%v, after=%v", before, after)
	}
}

func TestMaterializeOfflinePopulatedCacheWithoutUnusedLayout(t *testing.T) {
	dir, pinned := seedOfflineMaterializeCache(t)
	assertCompleteReadOnlyMaterialize(t, dir, pinned)
}

func assertAbsentCatalogSummary(t *testing.T, stdout, stderr string, code int, dir, pinned string) {
	t.Helper()

	if code != 6 {
		t.Fatalf("offline materialize: code=%d, stdout=%q, stderr=%q", code, stdout, stderr)
	}
	var result materialize.Result
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("offline summary: %v\n%s", err, stdout)
	}
	if result.Complete || !result.Offline || result.CacheDir != dir || result.CatalogDigest != pinned ||
		len(result.Missing) != 1 || result.Missing[0].Kind != "catalogIndex" || result.Missing[0].State != "absent" || result.Missing[0].Digest != pinned {
		t.Fatalf("unexpected absent catalog summary: %+v", result)
	}
}

func assertCompleteReadOnlyMaterialize(t *testing.T, dir, pinned string) {
	t.Helper()

	before := offlineCacheSnapshot(t, dir)
	stdout, stderr, code := run(t, offlineMaterializeArgs(dir, pinned)...)
	if code != 0 {
		t.Fatalf("offline materialize: code=%d, stdout=%q, stderr=%q", code, stdout, stderr)
	}
	var result materialize.Result
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Complete || !result.Offline || result.CacheDir != dir || result.CatalogDigest != pinned ||
		result.Entries != 1 || result.Schemas != 1 || result.Notices != 1 || result.CachedSchemaFiles != 1 ||
		len(result.Missing) != 0 || result.FetchedManifests != 0 || result.FetchedBlobs != 0 || result.WrittenSchemaFiles != 0 || result.RepairedEntries != 0 {
		t.Fatalf("unexpected complete offline summary: %+v", result)
	}
	if after := offlineCacheSnapshot(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("offline verification changed cache: before=%v, after=%v", before, after)
	}
}

func seedOfflineMaterializeCache(t *testing.T) (dir, pinned string) {
	t.Helper()

	schema := []byte(`{"type":"object"}`)
	packed, err := artifact.PackSchema(schema, []byte("NOTICE\n"))
	if err != nil {
		t.Fatal(err)
	}
	entry := catalog.Entry{
		ID: "fixture", Name: "fixture.json",
		Artifact: catalog.Descriptor{
			MediaType: packed.Manifest.Descriptor.MediaType,
			Digest:    packed.Manifest.Descriptor.Digest.String(),
			Size:      packed.Manifest.Descriptor.Size,
		},
	}
	raw, err := catalog.Marshal(&catalog.Catalog{FormatVersion: catalog.FormatVersion, Revision: "20260924.1200", Schemas: []catalog.Entry{entry}})
	if err != nil {
		t.Fatal(err)
	}
	pc, err := artifact.PackCatalog(raw, []ocispec.Descriptor{packed.Manifest.Descriptor})
	if err != nil {
		t.Fatal(err)
	}

	dir = t.TempDir()
	c, err := cache.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	blobs := make([]artifact.Blob, 0, 3+len(pc.Metadata.Blobs)+len(packed.Blobs))
	blobs = append(blobs, pc.Index, pc.Metadata.Manifest, packed.Manifest)
	blobs = append(blobs, pc.Metadata.Blobs...)
	blobs = append(blobs, packed.Blobs...)
	for _, blob := range blobs {
		if err := c.WriteBlob(blob.Descriptor.Digest.String(), blob.Descriptor.Size, blob.Descriptor.Size, bytes.NewReader(blob.Data)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.WriteMaterialized(entry.Artifact.Digest, packed.ContentDigest, int64(len(schema)), func(w io.Writer) error {
		_, err := w.Write(schema)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"v1/locks", "v1/tmp", "v1/quarantine"} {
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
	}

	return dir, pc.Index.Descriptor.Digest.String()
}

type offlineCacheEntry struct {
	Data    string
	Mode    fs.FileMode
	ModTime time.Time
}

func offlineCacheSnapshot(t *testing.T, dir string) map[string]offlineCacheEntry {
	t.Helper()

	entries := make(map[string]offlineCacheEntry)
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
		snapshot := offlineCacheEntry{Mode: info.Mode(), ModTime: info.ModTime()}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			snapshot.Data = string(data)
		}
		entries[rel] = snapshot
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	return entries
}
