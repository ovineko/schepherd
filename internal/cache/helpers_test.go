package cache

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

func openTestCache(t *testing.T) *Cache {
	t.Helper()

	return openCacheAt(t, filepath.Join(t.TempDir(), "cache"))
}

func openCacheAt(t *testing.T, dir string) *Cache {
	t.Helper()

	c, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%q): %v", dir, err)
	}

	t.Cleanup(func() { _ = c.Close() })

	return c
}

func blobPath(c *Cache, dgst string) string {
	return filepath.Join(c.Dir(), "v1", "blobs", "sha256", digest.Hex(dgst))
}

func materializedDirPath(c *Cache, manifestDigest string) string {
	return filepath.Join(c.Dir(), "v1", "materialized", "sha256", digest.Hex(manifestDigest))
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names
}

func assertNoTmp(t *testing.T, c *Cache) {
	t.Helper()

	if names := listDir(t, filepath.Join(c.Dir(), "v1", "tmp")); len(names) != 0 {
		t.Fatalf("temporary files left behind: %v", names)
	}
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s: want absent, got err=%v", path, err)
	}
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()

	if names := listDir(t, dir); len(names) != 0 {
		t.Fatalf("%s must stay empty, has %v", dir, names)
	}
}

func assertIs(t *testing.T, err, target error) {
	t.Helper()

	if !errors.Is(err, target) {
		t.Fatalf("want error wrapping %q, got %v", target, err)
	}
}

func assertKind(t *testing.T, err error, kind fault.Kind) {
	t.Helper()

	if err == nil {
		t.Fatalf("want %s error, got nil", kind)
	}

	if got := fault.KindOf(err); got != kind {
		t.Fatalf("want kind %s, got %s (%v)", kind, got, err)
	}
}

func assertUnclassified(t *testing.T, err error) {
	t.Helper()

	if classified, ok := errors.AsType[*fault.Error](err); ok {
		t.Fatalf("want an unclassified error, got kind %s (%v)", classified.Kind, err)
	}
}

func makeWritable(t *testing.T, path string) {
	t.Helper()

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertReadOnly relies on Go reporting Windows read-only files as 0444 too.
func assertReadOnly(t *testing.T, path string) {
	t.Helper()

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o444 {
		t.Fatalf("%s has mode %s, want a regular file with mode 0444", path, info.Mode())
	}
}
