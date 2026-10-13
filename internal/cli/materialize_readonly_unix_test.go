//go:build !windows

package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestMaterializeOfflineRestoredReadOnlyCache(t *testing.T) {
	source, pinned := seedOfflineMaterializeCache(t)
	restored := t.TempDir()
	entries := offlineCacheSnapshot(t, source)
	for rel, entry := range entries {
		if !entry.Mode.IsRegular() {
			continue
		}
		path := filepath.Join(restored, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(entry.Data), 0o444); err != nil {
			t.Fatal(err)
		}
	}

	t.Cleanup(func() {
		if err := filepath.WalkDir(restored, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return os.Chmod(path, 0o755)
			}
			return os.Chmod(path, 0o644)
		}); err != nil {
			t.Errorf("restore writable test cache: %v", err)
		}
	})
	if err := filepath.WalkDir(restored, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o555)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	assertCompleteReadOnlyMaterialize(t, restored, pinned)
}
