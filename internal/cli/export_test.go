package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
)

// withoutHardLinks makes every link fail the way FAT and exFAT do on Linux;
// before, when set, runs first, as a concurrent writer would.
func withoutHardLinks(t *testing.T, before func(newname string)) *int {
	t.Helper()

	calls := 0
	saved := linkFile
	linkFile = func(oldname, newname string) error {
		calls++

		if before != nil {
			before(newname)
		}

		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EPERM}
	}

	t.Cleanup(func() { linkFile = saved })

	return &calls
}

func TestWriteExportWithoutHardLinks(t *testing.T) {
	calls := withoutHardLinks(t, nil)
	dir := t.TempDir()
	dest := filepath.Join(dir, "schema.json")

	if err := writeExport(dest, []byte(`{"a":1}`), false); err != nil {
		t.Fatalf("export on a filesystem without hard links: %v", err)
	}

	if *calls != 1 {
		t.Fatalf("link attempted %d times, want 1", *calls)
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}

	if got, _ := os.ReadFile(dest); string(got) != `{"a":1}` {
		t.Errorf("destination = %q", got)
	}

	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644 like a linked export", info.Mode().Perm())
	}

	err = writeExport(dest, []byte(`{"a":2}`), false)
	if err == nil || fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second export without --force = %v", err)
	}

	if err := writeExport(dest, []byte(`{"a":3}`), true); err != nil {
		t.Fatal(err)
	}

	if got, _ := os.ReadFile(dest); string(got) != `{"a":3}` {
		t.Errorf("--force result %q", got)
	}

	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".schepherd-export-*")); len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

func TestWriteExportWithoutHardLinksNeverClobbersARacingWriter(t *testing.T) {
	withoutHardLinks(t, func(newname string) {
		if err := os.WriteFile(newname, []byte("theirs"), 0o600); err != nil {
			t.Error(err)
		}
	})

	dest := filepath.Join(t.TempDir(), "schema.json")

	err := writeExport(dest, []byte(`{"a":1}`), false)
	if err == nil || fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("export racing another writer = %v", err)
	}

	if got, _ := os.ReadFile(dest); string(got) != "theirs" {
		t.Errorf("the other writer's file was replaced by %q", got)
	}
}

func TestExportSchemaWritesTheNoticeNextToTheSchema(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "schema.json")
	notice := dest + noticeSuffix

	if err := exportSchema(dest, []byte(`{"a":1}`), []byte("Notice A\n"), false); err != nil {
		t.Fatal(err)
	}

	wantFile(t, dest, `{"a":1}`)
	wantFile(t, notice, "Notice A\n")

	err := exportSchema(dest, []byte(`{"a":2}`), []byte("Notice B\n"), false)
	if fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("export over an existing export = %v", err)
	}

	wantFile(t, dest, `{"a":1}`)
	wantFile(t, notice, "Notice A\n")

	if err := exportSchema(dest, []byte(`{"a":3}`), []byte("Notice C\n"), true); err != nil {
		t.Fatal(err)
	}

	wantFile(t, dest, `{"a":3}`)
	wantFile(t, notice, "Notice C\n")

	if err := exportSchema(dest, []byte(`{"a":4}`), nil, true); err != nil {
		t.Fatal(err)
	}

	wantFile(t, dest, `{"a":4}`)

	if _, err := os.Lstat(notice); !os.IsNotExist(err) {
		t.Errorf("--force without a notice kept the stale notice: %v", err)
	}

	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".schepherd-export-*")); len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

func TestExportSchemaWithoutNoticeKeepsAnExistingNoticeFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "schema.json")
	notice := dest + noticeSuffix

	if err := os.WriteFile(notice, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := exportSchema(dest, []byte(`{}`), nil, false)
	if fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), notice) {
		t.Fatalf("export next to an existing notice file = %v", err)
	}

	wantFile(t, notice, "mine")

	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Errorf("the schema was written although the export was refused: %v", err)
	}
}

// TestExportSchemaRemovesItsNoticeWhenTheSchemaLosesARace leaves nothing of
// its own behind when another writer creates the schema file first.
func TestExportSchemaRemovesItsNoticeWhenTheSchemaLosesARace(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "schema.json")

	withoutHardLinks(t, func(newname string) {
		if newname != dest {
			return
		}

		if err := os.WriteFile(newname, []byte("theirs"), 0o600); err != nil {
			t.Error(err)
		}
	})

	err := exportSchema(dest, []byte(`{}`), []byte("Notice\n"), false)
	if fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("export racing another writer = %v", err)
	}

	wantFile(t, dest, "theirs")

	if _, err := os.Lstat(dest + noticeSuffix); !os.IsNotExist(err) {
		t.Errorf("the notice of the refused export stayed: %v", err)
	}
}

// TestExportSchemaRollbackKeepsAnotherExportsNotice leaves a notice alone
// that another export installed after this one: a forced export replaces
// both files while this one is still writing its schema.
func TestExportSchemaRollbackKeepsAnotherExportsNotice(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "schema.json")

	withoutHardLinks(t, func(newname string) {
		if newname != dest {
			return
		}

		if err := exportSchema(dest, []byte(`{"b":1}`), []byte("Notice B\n"), true); err != nil {
			t.Error(err)
		}
	})

	err := exportSchema(dest, []byte(`{"a":1}`), []byte("Notice A\n"), false)
	if fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("export racing a forced export = %v", err)
	}

	wantFile(t, dest, `{"b":1}`)
	wantFile(t, dest+noticeSuffix, "Notice B\n")
}

// TestForcedExportChecksBothDestinationsFirst keeps the previous notice when
// the schema destination cannot be written.
func TestForcedExportChecksBothDestinationsFirst(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "schema.json")
	notice := dest + noticeSuffix

	if err := os.Mkdir(dest, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(notice, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, n := range [][]byte{[]byte("Notice\n"), nil} {
		err := exportSchema(dest, []byte(`{}`), n, true)
		if fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), "is a directory") {
			t.Fatalf("forced export onto a directory = %v", err)
		}

		wantFile(t, notice, "previous")
	}
}

// TestForcedExportRestoresThePreviousNotice puts the previous notice back
// when the schema cannot be put in place after the new notice was.
func TestForcedExportRestoresThePreviousNotice(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "schema.json")
	notice := dest + noticeSuffix

	if err := os.WriteFile(notice, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}

	previous, err := setAside(notice)
	if err != nil || previous == "" {
		t.Fatalf("setAside = %q, %v", previous, err)
	}

	if _, err := os.Lstat(notice); !os.IsNotExist(err) {
		t.Fatalf("the notice stayed in place: %v", err)
	}

	restoreNotice(notice, previous)
	wantFile(t, notice, "previous")

	if none, err := setAside(filepath.Join(dir, "missing")); err != nil || none != "" {
		t.Errorf("setAside of a missing file = %q, %v", none, err)
	}

	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".schepherd-export-*")); len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

func wantFile(t *testing.T, path, want string) {
	t.Helper()

	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Errorf("%s = %q, %v; want %q", path, got, err, want)
	}
}
