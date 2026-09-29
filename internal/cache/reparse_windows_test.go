//go:build windows

package cache

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/cache/linktest"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

// The Unix attack scenarios in symlink_unix_test.go, repeated with the links
// Windows offers: directory junctions, which any user can create, and
// symlinks, which need administrator rights or developer mode and are
// skipped without them. Go reports a junction as fs.ModeIrregular, so these
// tests also pin that the cache refuses what is neither a regular file nor a
// real directory, not only what is reported as a symlink.

type planter struct {
	plant func(t *testing.T, target, link string)
	name  string
}

func plantJunction(t *testing.T, target, link string) {
	t.Helper()

	if err := linktest.Junction(target, link); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(link)
	if err != nil || info.Mode()&fs.ModeIrregular == 0 || info.IsDir() {
		t.Fatalf("planted junction %s reads as %v, %v; want an irregular entry", link, info, err)
	}
}

func plantSymlink(t *testing.T, target, link string) {
	t.Helper()

	ok, err := linktest.Symlink(target, link)
	if err != nil {
		t.Fatal(err)
	}

	if !ok {
		t.Skip("creating symlinks needs administrator rights or developer mode")
	}
}

// directoryLinks plant a link to a directory.
var directoryLinks = []planter{
	{name: "junction", plant: plantJunction},
	{name: "symlink", plant: plantSymlink},
}

func isReparsePoint(info fs.FileInfo) bool {
	return info.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0
}

func assertReparseQuarantined(t *testing.T, c *Cache, prefix string) {
	t.Helper()

	quarantine := filepath.Join(c.Dir(), "v1", "quarantine")

	names := listDir(t, quarantine)
	if len(names) != 1 || !strings.HasPrefix(names[0], prefix+".") {
		t.Fatalf("quarantine holds %v, want one entry for %s", names, prefix)
	}

	info, err := os.Lstat(filepath.Join(quarantine, names[0]))
	if err != nil {
		t.Fatal(err)
	}

	if !isReparsePoint(info) {
		t.Fatalf("quarantined entry is %s, want the planted link itself", info.Mode())
	}
}

func TestWindowsOpenRefusesLinkedLayout(t *testing.T) {
	for _, link := range directoryLinks {
		for _, rel := range []string{"v1", "v1/tmp", "v1/blobs/sha256", "v1/materialized", "v1/locks", "v1/quarantine"} {
			t.Run(link.name+" "+rel, func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "cache")
				outside := t.TempDir()
				path := filepath.Join(dir, filepath.FromSlash(rel))

				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}

				link.plant(t, outside, path)

				c, err := Open(dir)
				if err == nil {
					_ = c.Close()
				}

				assertKind(t, err, fault.Usage)

				if !strings.Contains(err.Error(), path) {
					t.Fatalf("error must name the offending path %s: %v", path, err)
				}

				assertEmptyDir(t, outside)
			})
		}
	}
}

func TestWindowsLayoutSwappedForLinkAfterOpen(t *testing.T) {
	for _, link := range directoryLinks {
		t.Run(link.name, func(t *testing.T) {
			c := openTestCache(t)
			outside := t.TempDir()
			size := int64(len(testSchema))

			for _, rel := range []string{"v1/materialized/sha256", "v1/blobs/sha256"} {
				path := filepath.Join(c.Dir(), filepath.FromSlash(rel))
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}

				link.plant(t, outside, path)
			}

			if _, err := c.WriteMaterialized(testManifest, testContent, size, writeAll(testSchema)); err == nil {
				t.Fatal("WriteMaterialized through a planted link succeeded")
			}

			if err := c.WriteBlob(testContent, size, size, bytes.NewReader(testSchema)); err == nil {
				t.Fatal("WriteBlob through a planted link succeeded")
			}

			if _, err := c.ReadBlob(testContent, size); err == nil || errors.Is(err, ErrNotFound) {
				t.Fatalf("ReadBlob through a planted link: %v", err)
			}

			assertEmptyDir(t, outside)
			assertNoTmp(t, c)
		})
	}
}

func TestWindowsMaterializedDirectoryLinkToOutside(t *testing.T) {
	size := int64(len(testSchema))

	for _, link := range directoryLinks {
		t.Run(link.name, func(t *testing.T) {
			c := openTestCache(t)
			outside := t.TempDir()

			if err := os.WriteFile(filepath.Join(outside, schemaFile), testSchema, 0o644); err != nil {
				t.Fatal(err)
			}

			link.plant(t, outside, materializedDirPath(c, testManifest))

			assertIs(t, c.VerifyMaterialized(testManifest, testContent, size), ErrCorrupt)

			_, err := c.ReadMaterialized(testManifest, testContent, size)
			assertIs(t, err, ErrCorrupt)

			path, err := c.WriteMaterialized(testManifest, testContent, size, writeAll(testSchema))
			if err != nil {
				t.Fatal(err)
			}

			if names := listDir(t, outside); len(names) != 1 || names[0] != schemaFile {
				t.Fatalf("outside directory changed: %v", names)
			}

			info, err := os.Stat(filepath.Join(outside, schemaFile))
			if err != nil || info.Mode().Perm() != 0o666 {
				t.Fatalf("outside file was modified: %v, %v", info, err)
			}

			assertReparseQuarantined(t, c, digest.Hex(testManifest)+".materialized")

			info, err = os.Lstat(materializedDirPath(c, testManifest))
			if err != nil || !info.IsDir() || isReparsePoint(info) {
				t.Fatalf("materialized directory: %v, %v", info, err)
			}

			assertReadOnly(t, path)

			if err := c.VerifyMaterialized(testManifest, testContent, size); err != nil {
				t.Fatal(err)
			}

			assertNoTmp(t, c)
		})
	}
}

func TestWindowsLinkAtSchemaFile(t *testing.T) {
	size := int64(len(testSchema))

	tests := []struct {
		plant func(t *testing.T, c *Cache, outside, link string)
		name  string
	}{
		{name: "junction to outside directory", plant: func(t *testing.T, _ *Cache, outside, link string) {
			t.Helper()
			plantJunction(t, outside, link)
		}},
		{name: "symlink to outside file", plant: func(t *testing.T, _ *Cache, outside, link string) {
			t.Helper()

			victim := filepath.Join(outside, "victim.json")
			if err := os.WriteFile(victim, testSchema, 0o644); err != nil {
				t.Fatal(err)
			}

			plantSymlink(t, victim, link)
		}},
		{name: "dangling symlink to outside", plant: func(t *testing.T, _ *Cache, outside, link string) {
			t.Helper()
			plantSymlink(t, filepath.Join(outside, "created-by-attack.json"), link)
		}},
		{name: "relative symlink inside the root", plant: func(t *testing.T, c *Cache, _, link string) {
			t.Helper()

			if err := c.WriteBlob(testContent, size, size, bytes.NewReader(testSchema)); err != nil {
				t.Fatal(err)
			}

			plantSymlink(t, filepath.Join("..", "..", "..", "blobs", "sha256", digest.Hex(testContent)), link)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := openTestCache(t)
			outside := t.TempDir()

			if err := os.MkdirAll(materializedDirPath(c, testManifest), 0o755); err != nil {
				t.Fatal(err)
			}

			tt.plant(t, c, outside, c.SchemaPath(testManifest))

			before := listDir(t, outside)

			assertIs(t, c.VerifyMaterialized(testManifest, testContent, size), ErrCorrupt)

			path, err := c.WriteMaterialized(testManifest, testContent, size, writeAll(testSchema))
			if err != nil {
				t.Fatal(err)
			}

			assertReadOnly(t, path)

			if after := listDir(t, outside); len(after) != len(before) {
				t.Fatalf("outside directory changed: %v -> %v", before, after)
			}

			for _, name := range before {
				info, err := os.Stat(filepath.Join(outside, name))
				if err != nil || info.Mode().Perm() != 0o666 {
					t.Fatalf("outside file %s was modified: %v, %v", name, info, err)
				}
			}

			assertReparseQuarantined(t, c, digest.Hex(testManifest)+".schema")

			if err := c.VerifyBlob(testContent, size); err != nil && !errors.Is(err, ErrNotFound) {
				t.Fatalf("the blob a link pointed at must stay intact: %v", err)
			}
		})
	}
}

func TestWindowsLinkAtBlob(t *testing.T) {
	data := []byte("blob content")
	dgst := digest.FromBytes(data)

	tests := []struct {
		plant func(t *testing.T, outside, link string)
		name  string
	}{
		{name: "junction to outside directory", plant: plantJunction},
		{name: "symlink to outside file", plant: func(t *testing.T, outside, link string) {
			t.Helper()

			victim := filepath.Join(outside, "victim")
			if err := os.WriteFile(victim, data, 0o644); err != nil {
				t.Fatal(err)
			}

			plantSymlink(t, victim, link)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := openTestCache(t)
			outside := t.TempDir()

			tt.plant(t, outside, blobPath(c, dgst))

			before := listDir(t, outside)

			_, err := c.ReadBlob(dgst, 100)
			assertIs(t, err, ErrCorrupt)
			assertIs(t, c.VerifyBlob(dgst, -1), ErrCorrupt)

			_, err = c.OpenBlob(dgst)
			assertIs(t, err, ErrCorrupt)

			if err := c.WriteBlob(dgst, -1, 100, bytes.NewReader(data)); err != nil {
				t.Fatal(err)
			}

			assertReparseQuarantined(t, c, digest.Hex(dgst))
			assertReadOnly(t, blobPath(c, dgst))

			if after := listDir(t, outside); len(after) != len(before) {
				t.Fatalf("outside directory changed: %v -> %v", before, after)
			}

			for _, name := range before {
				info, err := os.Stat(filepath.Join(outside, name))
				if err != nil || info.Mode().Perm() != 0o666 {
					t.Fatalf("outside file %s was modified: %v, %v", name, info, err)
				}
			}

			if err := c.VerifyBlob(dgst, int64(len(data))); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWindowsLockReplacesSymlinkAtLockPath(t *testing.T) {
	c := openTestCache(t)
	outside := t.TempDir()
	lockPath := filepath.Join(c.Dir(), "v1", "locks", "key.lock")
	plantSymlink(t, filepath.Join(outside, "planted.lock"), lockPath)

	release, err := c.Lock(t.Context(), "key")
	if err != nil {
		t.Fatal(err)
	}

	release()

	assertEmptyDir(t, outside)

	info, err := os.Lstat(lockPath)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("lock path after Lock: %v, %v", info, err)
	}
}

func TestWindowsLockRefusesJunctionAtLockPath(t *testing.T) {
	c := openTestCache(t)
	outside := t.TempDir()
	lockPath := filepath.Join(c.Dir(), "v1", "locks", "key.lock")
	plantJunction(t, outside, lockPath)

	release, err := c.Lock(t.Context(), "key")
	if err == nil {
		release()
		t.Fatal("Lock accepted a junction as its lock file")
	}

	if !strings.Contains(err.Error(), lockPath) {
		t.Errorf("error must name the lock path %s: %v", lockPath, err)
	}

	assertEmptyDir(t, outside)
}

func TestWindowsLockRefusesLinkedLocksDirectory(t *testing.T) {
	for _, link := range directoryLinks {
		t.Run(link.name, func(t *testing.T) {
			c := openTestCache(t)
			outside := t.TempDir()
			locks := filepath.Join(c.Dir(), "v1", "locks")

			if err := os.Remove(locks); err != nil {
				t.Fatal(err)
			}

			link.plant(t, outside, locks)

			if release, err := c.Lock(t.Context(), "key"); err == nil {
				release()
				t.Fatal("Lock through a linked locks directory succeeded")
			}

			assertEmptyDir(t, outside)
		})
	}
}

func TestWindowsStoredFilesAreReadOnly(t *testing.T) {
	c := openTestCache(t)
	size := int64(len(testSchema))

	if err := c.WriteBlob(testContent, size, size, bytes.NewReader(testSchema)); err != nil {
		t.Fatal(err)
	}

	path, err := c.WriteMaterialized(testManifest, testContent, size, writeAll(testSchema))
	if err != nil {
		t.Fatal(err)
	}

	for _, stored := range []string{blobPath(c, testContent), path} {
		assertReadOnly(t, stored)

		if err := os.WriteFile(stored, []byte("overwritten"), 0o644); err == nil {
			t.Fatalf("%s could be overwritten", stored)
		}
	}

	if err := c.VerifyBlob(testContent, size); err != nil {
		t.Fatal(err)
	}
}

// A corrupt entry keeps its read-only attribute, which on Windows forbids
// renaming another file over it; the repair must still replace it.
func TestWindowsCorruptReadOnlyEntriesAreReplaced(t *testing.T) {
	c := openTestCache(t)
	size := int64(len(testSchema))

	if err := c.WriteBlob(testContent, size, size, bytes.NewReader(testSchema)); err != nil {
		t.Fatal(err)
	}

	path, err := c.WriteMaterialized(testManifest, testContent, size, writeAll(testSchema))
	if err != nil {
		t.Fatal(err)
	}

	for _, stored := range []string{blobPath(c, testContent), path} {
		makeWritable(t, stored)

		if err := os.WriteFile(stored, bytes.ToUpper(testSchema), 0o644); err != nil {
			t.Fatal(err)
		}

		if err := os.Chmod(stored, 0o444); err != nil {
			t.Fatal(err)
		}
	}

	assertIs(t, c.VerifyBlob(testContent, size), ErrCorrupt)
	assertIs(t, c.VerifyMaterialized(testManifest, testContent, size), ErrCorrupt)

	if err := c.WriteBlob(testContent, size, size, bytes.NewReader(testSchema)); err != nil {
		t.Fatal(err)
	}

	if _, err := c.WriteMaterialized(testManifest, testContent, size, writeAll(testSchema)); err != nil {
		t.Fatal(err)
	}

	if err := c.VerifyBlob(testContent, size); err != nil {
		t.Fatal(err)
	}

	if err := c.VerifyMaterialized(testManifest, testContent, size); err != nil {
		t.Fatal(err)
	}

	if names := listDir(t, filepath.Join(c.Dir(), "v1", "quarantine")); len(names) != 2 {
		t.Fatalf("quarantine holds %v, want both corrupt entries", names)
	}

	assertNoTmp(t, c)
}
