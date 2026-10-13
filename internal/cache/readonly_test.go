package cache

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/fault"
)

func openReadOnlyAt(t *testing.T, dir string) *Cache {
	t.Helper()

	c, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = c.Close() })

	return c
}

func TestOpenReadOnlyMissingRootAndLayout(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "missing root"
		if existing {
			name = "missing layout"
		}

		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "cache")
			if existing {
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			c := openReadOnlyAt(t, dir)
			if c.Dir() != dir || c.SchemaPath(testManifest) == "" {
				t.Fatalf("missing cache paths: Dir=%q, SchemaPath=%q", c.Dir(), c.SchemaPath(testManifest))
			}

			_, err := c.ReadBlob(testContent, int64(len(testSchema)))
			assertIs(t, err, ErrNotFound)
			assertIs(t, c.VerifyBlob(testContent, -1), ErrNotFound)
			reader, err := c.OpenBlob(testContent)
			assertIs(t, err, ErrNotFound)
			if reader != nil {
				_ = reader.Close()
				t.Fatal("absent blob returned a reader")
			}

			_, err = c.ReadMaterialized(testManifest, testContent, int64(len(testSchema)))
			assertIs(t, err, ErrNotFound)
			assertIs(t, c.VerifyMaterialized(testManifest, testContent, int64(len(testSchema))), ErrNotFound)
			assertReadOnlyMutations(t, c)
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}

			if existing {
				assertEmptyDir(t, dir)
			} else {
				assertAbsent(t, dir)
			}
		})
	}
}

func TestOpenReadOnlyPreservesLifecycleEntries(t *testing.T) {
	dir := t.TempDir()
	old := time.Unix(1, 0)
	for _, rel := range []string{
		filepath.Join(tmpDir, "stale"),
		filepath.Join(tmpDir, "stale-directory", "partial"),
		filepath.Join(quarantineDir, "stale"),
		filepath.Join(quarantineDir, "entry.q1.fixed", "schema.json"),
	} {
		path := filepath.Join(dir, rel)
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

	before := readOnlySnapshot(t, dir)
	c := openReadOnlyAt(t, dir)
	assertIs(t, c.VerifyBlob(testContent, -1), ErrNotFound)
	assertReadOnlyMutations(t, c)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if after := readOnlySnapshot(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("read-only cache changed: before=%v, after=%v", before, after)
	}
}

func TestOpenReadOnlyExistingEntriesWithoutUnusedLayout(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if err := c.WriteBlob(testContent, int64(len(testSchema)), int64(len(testSchema)), bytes.NewReader(testSchema)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WriteMaterialized(testManifest, testContent, int64(len(testSchema)), writeAll(testSchema)); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{locksDir, tmpDir, quarantineDir} {
		if err := os.Remove(filepath.Join(dir, rel)); err != nil {
			t.Fatal(err)
		}
	}

	before := readOnlySnapshot(t, dir)
	ro := openReadOnlyAt(t, dir)
	assertReadOnlyReads(t, ro)
	assertReadOnlyMutations(t, ro)
	if after := readOnlySnapshot(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("read-only cache changed: before=%v, after=%v", before, after)
	}
}

func TestOpenReadOnlyUnusableRootReturnsIOError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := OpenReadOnly(path)
	if c != nil {
		_ = c.Close()
		t.Fatal("a regular file was accepted as a cache root")
	}
	assertKind(t, err, fault.Usage)
	if _, ok := errors.AsType[*os.PathError](err); !ok {
		t.Fatalf("cache opening lost the filesystem error: %v", err)
	}
}

func TestOpenReadOnlyPreservesCorruptEntryErrors(t *testing.T) {
	dir := t.TempDir()
	blob := filepath.Join(dir, blobsDir, "0000000000000000000000000000000000000000000000000000000000000000")
	if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}

	before := readOnlySnapshot(t, dir)
	c := openReadOnlyAt(t, dir)
	badDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	_, err := c.ReadBlob(badDigest, 100)
	assertIs(t, err, ErrCorrupt)
	assertIs(t, c.VerifyBlob(badDigest, -1), ErrCorrupt)
	r, err := c.OpenBlob(badDigest)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(r)
	assertIs(t, err, ErrCorrupt)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if after := readOnlySnapshot(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("read-only corrupt lookup changed cache: before=%v, after=%v", before, after)
	}
}

func assertReadOnlyReads(t *testing.T, c *Cache) {
	t.Helper()

	size := int64(len(testSchema))
	data, err := c.ReadBlob(testContent, size)
	if err != nil || !bytes.Equal(data, testSchema) {
		t.Fatalf("ReadBlob: %s, %v", data, err)
	}
	if err := c.VerifyBlob(testContent, size); err != nil {
		t.Fatal(err)
	}
	r, err := c.OpenBlob(testContent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	data, err = io.ReadAll(r)
	if err != nil || !bytes.Equal(data, testSchema) {
		t.Fatalf("OpenBlob: %s, %v", data, err)
	}
	data, err = c.ReadMaterialized(testManifest, testContent, size)
	if err != nil || !bytes.Equal(data, testSchema) {
		t.Fatalf("ReadMaterialized: %s, %v", data, err)
	}
	if err := c.VerifyMaterialized(testManifest, testContent, size); err != nil {
		t.Fatal(err)
	}
}

func assertReadOnlyMutations(t *testing.T, c *Cache) {
	t.Helper()

	size := int64(len(testSchema))
	src := bytes.NewReader(testSchema)
	assertReadOnlyError(t, c.WriteBlob(testContent, size, size, src))
	if src.Len() != len(testSchema) {
		t.Fatal("read-only WriteBlob consumed its source")
	}

	called := false
	path, err := c.WriteMaterialized(testManifest, testContent, size, func(io.Writer) error {
		called = true
		return nil
	})
	assertReadOnlyError(t, err)
	if path != "" || called {
		t.Fatalf("read-only WriteMaterialized returned path=%q, called fill=%t", path, called)
	}

	assertReadOnlyError(t, c.QuarantineBlob(testContent))
	assertReadOnlyError(t, c.QuarantineMaterialized(testManifest))
	release, err := c.Lock(t.Context(), "readonly")
	assertReadOnlyError(t, err)
	if release != nil {
		release()
		t.Fatal("read-only Lock returned a release function")
	}
}

func assertReadOnlyError(t *testing.T, err error) {
	t.Helper()
	assertIs(t, err, ErrReadOnly)
	assertKind(t, err, fault.Internal)
}

type readOnlyEntry struct {
	Data    string
	Mode    fs.FileMode
	ModTime time.Time
}

func readOnlySnapshot(t *testing.T, dir string) map[string]readOnlyEntry {
	t.Helper()

	entries := make(map[string]readOnlyEntry)
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
		snapshot := readOnlyEntry{Mode: info.Mode(), ModTime: info.ModTime()}
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
