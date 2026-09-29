//go:build unix

package cache

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

// Symlink creation needs privileges on Windows, so the attack scenarios run
// on Unix only; the defence itself (os.Root plus Lstat checks) is portable.

func symlink(t *testing.T, target, link string) {
	t.Helper()

	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func assertLinkQuarantined(t *testing.T, c *Cache, prefix string) {
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

	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("quarantined entry is %s, want the planted symlink", info.Mode())
	}
}

func TestMaterializedDirectorySymlinkToOutside(t *testing.T) {
	size := int64(len(testSchema))

	tests := []struct {
		target func(t *testing.T, c *Cache, outside string) string
		name   string
	}{
		{name: "absolute", target: func(_ *testing.T, _ *Cache, outside string) string { return outside }},
		{name: "relative escaping", target: func(t *testing.T, c *Cache, outside string) string {
			t.Helper()

			rel, err := filepath.Rel(filepath.Join(c.Dir(), "v1", "materialized", "sha256"), outside)
			if err != nil {
				t.Fatal(err)
			}

			return rel
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := openTestCache(t)
			outside := t.TempDir()
			symlink(t, tt.target(t, c, outside), materializedDirPath(c, testManifest))

			err := c.VerifyMaterialized(testManifest, testContent, size)
			assertIs(t, err, ErrCorrupt)

			_, err = c.ReadMaterialized(testManifest, testContent, size)
			assertIs(t, err, ErrCorrupt)

			path, err := c.WriteMaterialized(testManifest, testContent, size, writeAll(testSchema))
			if err != nil {
				t.Fatal(err)
			}

			assertEmptyDir(t, outside)
			assertLinkQuarantined(t, c, digest.Hex(testManifest)+".materialized")

			info, err := os.Lstat(materializedDirPath(c, testManifest))
			if err != nil || !info.IsDir() {
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

func TestSchemaFileSymlink(t *testing.T) {
	size := int64(len(testSchema))

	tests := []struct {
		target    func(t *testing.T, c *Cache, outside string) string
		name      string
		linksBlob bool
	}{
		{name: "absolute to outside file", target: func(t *testing.T, _ *Cache, outside string) string {
			t.Helper()

			victim := filepath.Join(outside, "victim.json")
			if err := os.WriteFile(victim, testSchema, 0o644); err != nil {
				t.Fatal(err)
			}

			return victim
		}},
		{name: "dangling absolute to outside", target: func(_ *testing.T, _ *Cache, outside string) string {
			return filepath.Join(outside, "created-by-attack.json")
		}},
		{name: "relative inside the root", linksBlob: true, target: func(t *testing.T, c *Cache, _ string) string {
			t.Helper()

			if err := c.WriteBlob(testContent, size, size, bytes.NewReader(testSchema)); err != nil {
				t.Fatal(err)
			}

			return filepath.Join("..", "..", "..", "blobs", "sha256", digest.Hex(testContent))
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := openTestCache(t)
			outside := t.TempDir()

			if err := os.MkdirAll(materializedDirPath(c, testManifest), 0o755); err != nil {
				t.Fatal(err)
			}

			symlink(t, tt.target(t, c, outside), c.SchemaPath(testManifest))

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
				if err != nil || info.Mode().Perm() != 0o644 {
					t.Fatalf("outside file %s was modified: %v, %v", name, info, err)
				}
			}

			names := listDir(t, filepath.Join(c.Dir(), "v1", "quarantine"))
			if len(names) != 1 {
				t.Fatalf("quarantine holds %v", names)
			}

			if tt.linksBlob {
				if err := c.VerifyBlob(testContent, size); err != nil {
					t.Fatalf("the blob the link pointed at must stay intact: %v", err)
				}
			}
		})
	}
}

func TestBlobSymlinkToOutside(t *testing.T) {
	c := openTestCache(t)
	outside := t.TempDir()
	data := []byte("blob content")
	dgst := digest.FromBytes(data)

	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, data, 0o644); err != nil {
		t.Fatal(err)
	}

	symlink(t, victim, blobPath(c, dgst))

	_, err := c.ReadBlob(dgst, 100)
	assertIs(t, err, ErrCorrupt)
	assertIs(t, c.VerifyBlob(dgst, -1), ErrCorrupt)

	_, err = c.OpenBlob(dgst)
	assertIs(t, err, ErrCorrupt)

	if err := c.WriteBlob(dgst, -1, 100, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	assertLinkQuarantined(t, c, digest.Hex(dgst))
	assertReadOnly(t, blobPath(c, dgst))

	info, err := os.Stat(victim)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("outside file was modified: %v, %v", info, err)
	}

	if names := listDir(t, outside); len(names) != 1 {
		t.Fatalf("outside directory changed: %v", names)
	}
}

func TestOpenRefusesSymlinkedLayout(t *testing.T) {
	for _, rel := range []string{"v1", "v1/tmp", "v1/blobs/sha256", "v1/materialized", "v1/locks", "v1/quarantine"} {
		t.Run(rel, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "cache")
			outside := t.TempDir()
			link := filepath.Join(dir, filepath.FromSlash(rel))

			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}

			symlink(t, outside, link)

			c, err := Open(dir)
			if err == nil {
				_ = c.Close()
			}

			assertKind(t, err, fault.Usage)

			if !strings.Contains(err.Error(), link) {
				t.Fatalf("error must name the offending path: %v", err)
			}

			assertEmptyDir(t, outside)
		})
	}
}

func TestLayoutSwappedForSymlinkAfterOpen(t *testing.T) {
	c := openTestCache(t)
	outside := t.TempDir()
	size := int64(len(testSchema))

	for _, rel := range []string{"v1/materialized/sha256", "v1/blobs/sha256"} {
		path := filepath.Join(c.Dir(), filepath.FromSlash(rel))
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}

		symlink(t, outside, path)
	}

	if _, err := c.WriteMaterialized(testManifest, testContent, size, writeAll(testSchema)); err == nil {
		t.Fatal("WriteMaterialized through a planted symlink succeeded")
	}

	if err := c.WriteBlob(testContent, size, size, bytes.NewReader(testSchema)); err == nil {
		t.Fatal("WriteBlob through a planted symlink succeeded")
	}

	if _, err := c.ReadBlob(testContent, size); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("ReadBlob through a planted symlink: %v", err)
	}

	assertEmptyDir(t, outside)
	assertNoTmp(t, c)
}

func TestLockReplacesSymlinkAtLockPath(t *testing.T) {
	c := openTestCache(t)
	outside := t.TempDir()
	lockPath := filepath.Join(c.Dir(), "v1", "locks", "key.lock")
	symlink(t, filepath.Join(outside, "planted.lock"), lockPath)

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

func TestLockSymlinkSwappedInWhileWaiting(t *testing.T) {
	c := openTestCache(t)
	outside := t.TempDir()
	lockPath := filepath.Join(c.Dir(), "v1", "locks", "key.lock")

	busy := make(chan struct{}, 1)
	c.lockBusy = func() {
		select {
		case busy <- struct{}{}:
		default:
		}
	}

	release, err := c.Lock(t.Context(), "key")
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		err     error
		release func()
	}

	second := make(chan result, 1)

	go func() {
		r, err := c.Lock(t.Context(), "key")
		second <- result{release: r, err: err}
	}()

	waitBusy := func() {
		t.Helper()

		select {
		case <-busy:
		case got := <-second:
			if got.release != nil {
				got.release()
			}

			t.Fatalf("second Lock returned while the first is held: %v", got.err)
		}
	}

	waitBusy()

	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}

	symlink(t, filepath.Join(outside, "planted.lock"), lockPath)

	for range 3 {
		waitBusy()
	}

	assertEmptyDir(t, outside)
	release()

	var got result
	select {
	case got = <-second:
	case <-time.After(10 * time.Second):
		t.Fatal("second Lock did not return after the first was released")
	}

	if got.err != nil {
		t.Fatal(got.err)
	}

	defer got.release()

	assertEmptyDir(t, outside)

	info, err := os.Lstat(lockPath)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("lock path after Lock: %v, %v", info, err)
	}
}

func TestLockRefusesSymlinkedLocksDirectory(t *testing.T) {
	c := openTestCache(t)
	outside := t.TempDir()
	locks := filepath.Join(c.Dir(), "v1", "locks")

	if err := os.Remove(locks); err != nil {
		t.Fatal(err)
	}

	symlink(t, outside, locks)

	if _, err := c.Lock(t.Context(), "key"); err == nil {
		t.Fatal("Lock through a symlinked locks directory succeeded")
	}

	assertEmptyDir(t, outside)
}

func TestStoredFilesAreReadOnly(t *testing.T) {
	c := openTestCache(t)
	size := int64(len(testSchema))

	if err := c.WriteBlob(testContent, size, size, bytes.NewReader(testSchema)); err != nil {
		t.Fatal(err)
	}

	path, err := c.WriteMaterialized(testManifest, testContent, size, writeAll(testSchema))
	if err != nil {
		t.Fatal(err)
	}

	assertReadOnly(t, blobPath(c, testContent))
	assertReadOnly(t, path)
}
