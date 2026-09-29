//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ovineko/schepherd/internal/cache/linktest"
	"github.com/ovineko/schepherd/internal/fault"
)

// The Windows counterpart of export_unix_test.go. Symlinks need administrator
// rights or developer mode; directory junctions need no privilege, and Go
// reports them as fs.ModeIrregular rather than fs.ModeSymlink.
func TestWriteExportReplacesSymlinkNotTarget(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target.json")
		link := filepath.Join(dir, "link.json")

		if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}

		ok, err := linktest.Symlink(target, link)
		if err != nil {
			t.Fatal(err)
		}

		if !ok {
			t.Skip("creating symlinks needs administrator rights or developer mode")
		}

		if err := writeExport(link, []byte(`{}`), false); err == nil {
			t.Fatal("a symlink destination was replaced without --force")
		}

		if err := writeExport(link, []byte(`{}`), true); err != nil {
			t.Fatal(err)
		}

		if got, _ := os.ReadFile(target); string(got) != "keep" {
			t.Errorf("symlink target was written through: %q", got)
		}

		info, err := os.Lstat(link)
		if err != nil || !info.Mode().IsRegular() {
			t.Errorf("destination is not a regular file: %v, %v", info, err)
		}

		if err := writeExport(dir, []byte(`{}`), true); err == nil {
			t.Error("a directory destination was accepted")
		}
	})

	t.Run("junction", func(t *testing.T) {
		outside := t.TempDir()
		link := filepath.Join(t.TempDir(), "link.json")

		if err := linktest.Junction(outside, link); err != nil {
			t.Fatal(err)
		}

		if err := writeExport(link, []byte(`{}`), false); fault.KindOf(err) != fault.Usage {
			t.Fatalf("a junction destination without --force: %v, want a usage error", err)
		}

		// os.Rename cannot replace a directory reparse point with a file, so
		// a usage error is as acceptable as a replaced link; writing into the
		// junction target is not.
		err := writeExport(link, []byte(`{}`), true)
		if err != nil && fault.KindOf(err) != fault.Usage {
			t.Errorf("a junction destination with --force: %v, want success or a usage error", err)
		}

		if err == nil {
			info, lstatErr := os.Lstat(link)
			if lstatErr != nil || !info.Mode().IsRegular() {
				t.Errorf("destination is not a regular file: %v, %v", info, lstatErr)
			}
		}

		entries, readErr := os.ReadDir(outside)
		if readErr != nil {
			t.Fatal(readErr)
		}

		if len(entries) != 0 {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}

			t.Errorf("the junction target was written through: %v", names)
		}
	})
}
