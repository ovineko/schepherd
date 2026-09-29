package cache

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/fault"
)

func TestDefaultDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", base)

	userCache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}

	got, err := DefaultDir()
	if err != nil {
		t.Fatal(err)
	}

	if want := filepath.Join(userCache, "schepherd"); got != want {
		t.Fatalf("DefaultDir() = %q, want %q", got, want)
	}

	if runtime.GOOS == "linux" {
		t.Setenv("XDG_CACHE_HOME", "")
		t.Setenv("HOME", "")

		_, err := DefaultDir()
		assertKind(t, err, fault.Usage)
	}
}

func TestOpenCreatesLayout(t *testing.T) {
	c := openTestCache(t)

	if !filepath.IsAbs(c.Dir()) {
		t.Fatalf("Dir() = %q is not absolute", c.Dir())
	}

	for _, rel := range []string{"v1/blobs/sha256", "v1/materialized/sha256", "v1/locks", "v1/tmp", "v1/quarantine"} {
		info, err := os.Lstat(filepath.Join(c.Dir(), filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}

		if !info.IsDir() {
			t.Fatalf("%s is not a directory", rel)
		}
	}

	again := openCacheAt(t, c.Dir())
	if again.Dir() != c.Dir() {
		t.Fatalf("reopen: Dir() = %q, want %q", again.Dir(), c.Dir())
	}
}

func TestOpenRelativeDir(t *testing.T) {
	t.Chdir(t.TempDir())

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	c := openCacheAt(t, filepath.Join("rel", "..", "rel", "cache"))

	if want := filepath.Join(wd, "rel", "cache"); c.Dir() != want {
		t.Fatalf("Dir() = %q, want %q", c.Dir(), want)
	}
}

func TestOpenUnusableDir(t *testing.T) {
	base := t.TempDir()

	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	layoutFile := filepath.Join(base, "layout")
	if err := os.MkdirAll(filepath.Join(layoutFile, "v1"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(layoutFile, "v1", "tmp"), []byte("user data"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		dir  string
	}{
		{name: "empty", dir: ""},
		{name: "regular file", dir: file},
		{name: "below a regular file", dir: filepath.Join(file, "cache")},
		{name: "layout entry is a file", dir: layoutFile},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Open(tt.dir)
			if err == nil {
				_ = c.Close()
			}

			assertKind(t, err, fault.Usage)
		})
	}

	data, err := os.ReadFile(filepath.Join(layoutFile, "v1", "tmp"))
	if err != nil || string(data) != "user data" {
		t.Fatalf("Open must not touch a foreign file: %q, %v", data, err)
	}
}

func TestOpenRemovesStaleTemporaryFiles(t *testing.T) {
	c := openTestCache(t)
	tmp := filepath.Join(c.Dir(), "v1", "tmp")
	old := time.Now().Add(-25 * time.Hour)

	for _, name := range []string{"old-file", "old-readonly", "fresh-file"} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.MkdirAll(filepath.Join(tmp, "old-dir", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(filepath.Join(tmp, "old-readonly"), 0o444); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"old-file", "old-readonly", "old-dir"} {
		if err := os.Chtimes(filepath.Join(tmp, name), old, old); err != nil {
			t.Fatal(err)
		}
	}

	openCacheAt(t, c.Dir())

	names := listDir(t, tmp)
	if len(names) != 1 || names[0] != "fresh-file" {
		t.Fatalf("after reopen tmp holds %v, want only fresh-file", names)
	}
}

// TestOpenRemovesOldQuarantine expires quarantined entries a day after they
// were moved aside, whatever their modification time, and entries of older
// versions without a recorded time by their modification time. Read-only
// files and directories go too.
func TestOpenRemovesOldQuarantine(t *testing.T) {
	c := openTestCache(t)
	quarantine := filepath.Join(c.Dir(), "v1", "quarantine")
	now := time.Now()
	old := now.Add(-25 * time.Hour)
	hex := strings.Repeat("a", 64)

	// A blob quarantined just now keeps the old modification time of its
	// first write; only the time in its name counts.
	fresh := quarantineName(hex, now)
	entries := []struct {
		name    string
		modTime time.Time
		kept    bool
	}{
		{name: quarantineName(hex, old), modTime: now},
		{name: fresh, modTime: old, kept: true},
		{name: quarantineName(hex+".schema", old), modTime: now},
		{name: quarantineName(hex+".schema", now), modTime: now, kept: true},
		{name: hex + ".legacyoldtoken", modTime: old},
		{name: hex + ".legacyfreshtoken", modTime: now, kept: true},
		{name: hex + ".qnotanumber.token", modTime: old},
	}

	var want []string

	for _, e := range entries {
		p := filepath.Join(quarantine, e.name)
		if err := os.WriteFile(p, []byte(e.name), 0o444); err != nil {
			t.Fatal(err)
		}

		if err := os.Chtimes(p, e.modTime, e.modTime); err != nil {
			t.Fatal(err)
		}

		if e.kept {
			want = append(want, e.name)
		}
	}

	oldDir := filepath.Join(quarantine, quarantineName(hex+".materialized", old))
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(oldDir, "schema.json"), []byte("{}"), 0o444); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(oldDir, 0o555); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(oldDir, 0o755) })

	openCacheAt(t, c.Dir())

	slices.Sort(want)

	if got := listDir(t, quarantine); !slices.Equal(got, want) {
		t.Fatalf("after reopen quarantine holds %v, want %v", got, want)
	}
}

func TestQuarantinedAt(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)

	for _, prefix := range []string{strings.Repeat("0", 64), strings.Repeat("0", 64) + ".schema", strings.Repeat("0", 64) + ".materialized"} {
		got, ok := quarantinedAt(quarantineName(prefix, at))
		if !ok || !got.Equal(at) {
			t.Errorf("quarantinedAt(quarantineName(%q)) = %v, %v", prefix, got, ok)
		}
	}

	for _, name := range []string{"", "x", "abc.token", "abc.schema.token", "abc.q.token", "abc.q-1.token", "abc.qx1.token"} {
		if got, ok := quarantinedAt(name); ok {
			t.Errorf("quarantinedAt(%q) = %v", name, got)
		}
	}
}
