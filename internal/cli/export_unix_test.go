//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteExportReplacesSymlinkNotTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	link := filepath.Join(dir, "link.json")

	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
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
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("destination is still a symlink: %v", err)
	}

	if err := writeExport(dir, []byte(`{}`), true); err == nil {
		t.Error("a directory destination was accepted")
	}
}

func TestExportSchemaRemovesAStaleNoticeSymlinkNotItsTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	dest := filepath.Join(dir, "schema.json")

	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(target, dest+noticeSuffix); err != nil {
		t.Fatal(err)
	}

	if err := exportSchema(dest, []byte(`{}`), nil, true); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(dest + noticeSuffix); !os.IsNotExist(err) {
		t.Errorf("the stale notice symlink stayed: %v", err)
	}

	wantFile(t, target, "keep")

	if err := os.Symlink(target, dest+noticeSuffix); err != nil {
		t.Fatal(err)
	}

	if err := exportSchema(dest, []byte(`{}`), []byte("Notice\n"), true); err != nil {
		t.Fatal(err)
	}

	wantFile(t, target, "keep")
	wantFile(t, dest+noticeSuffix, "Notice\n")

	if info, err := os.Lstat(dest + noticeSuffix); err != nil || !info.Mode().IsRegular() {
		t.Errorf("the notice is not a regular file: %v", err)
	}
}
