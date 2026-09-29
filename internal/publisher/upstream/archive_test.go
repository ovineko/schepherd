package upstream

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

func listTree(tb testing.TB, root string) []string {
	tb.Helper()

	var out []string

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}

		if rel == "." {
			return nil
		}

		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			rel += "/"
		}

		out = append(out, rel)

		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}

	return out
}

func TestFetchSchemaStore(t *testing.T) {
	tarball := gzipBytes(t, buildTar(t, validEntries()))
	ts := newTarballServer(t, tarball)
	dest := filepath.Join(t.TempDir(), "snapshot")

	snap, err := FetchSchemaStore(t.Context(), ts.fetcher, ts.base()+"/", testCommit, dest)
	if err != nil {
		t.Fatal(err)
	}

	if got := *ts.path.Load(); got != "/SchemaStore/schemastore/tar.gz/"+testCommit {
		t.Fatalf("requested path %q", got)
	}

	want := []string{
		"LICENSE",
		"NOTICE",
		"snapshot.json",
		"src/",
		"src/api/",
		"src/api/json/",
		"src/api/json/catalog.json",
		"src/negative_test/",
		"src/negative_test/a/",
		"src/negative_test/a/bad.json",
		"src/schemas/",
		"src/schemas/json/",
		"src/schemas/json/a.json",
		"src/schemas/json/b.json",
		"src/schemas/json/tsconfig.json",
		"src/test/",
		"src/test/a/",
		"src/test/a/one.json",
		"src/test/a/two.yaml",
		"src/test/b/",
		"src/test/b/nested/",
		"src/test/b/nested/deep.toml",
		"src/test/stray.json",
	}

	if got := listTree(t, dest); !slices.Equal(got, want) {
		t.Fatalf("extracted tree:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	if snap.Commit != testCommit || snap.TarballDigest != digest.FromBytes(tarball) {
		t.Fatalf("snapshot metadata = %s %s", snap.Commit, snap.TarballDigest)
	}

	if snap.Dir != dest {
		t.Fatalf("Dir = %s", snap.Dir)
	}

	meta, err := os.ReadFile(filepath.Join(dest, "snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}

	wantMeta := `{"commit":"` + testCommit + `","tarballDigest":"` + digest.FromBytes(tarball) + `","formatVersion":1}` + "\n"
	if string(meta) != wantMeta {
		t.Fatalf("metadata = %s", meta)
	}

	if entries, err := os.ReadDir(filepath.Dir(dest)); err != nil || len(entries) != 1 {
		t.Fatalf("temporary directories left behind: %v %v", entries, err)
	}
}

func TestFetchSchemaStoreRejectsMaliciousArchives(t *testing.T) {
	base := validEntries()

	with := func(extra ...tarEntry) []tarEntry {
		return append(slices.Clone(base), extra...)
	}

	tests := []struct {
		name    string
		entries []tarEntry
		want    string
	}{
		{name: "absolute path", entries: with(file("/etc/evil", "x")), want: "absolute path"},
		{name: "parent traversal", entries: with(file(top("../evil"), "x")), want: "not clean"},
		{name: "parent segment", entries: with(file(top("src/../../evil"), "x")), want: "not clean"},
		{name: "leading dot slash", entries: with(file("./"+top("LICENSE2"), "x")), want: "not clean"},
		{name: "other top directory", entries: with(file("other/src/schemas/json/x.json", "x")), want: "outside"},
		{name: "sibling prefix", entries: with(file("schemastore-"+testCommit+"x/a", "x")), want: "outside"},
		{name: "file as top", entries: []tarEntry{file("schemastore-"+testCommit, "x")}, want: "must be a directory"},
		{
			name:    "symlink",
			entries: with(tarEntry{name: top("src/schemas/json/link.json"), typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}),
			want:    "only files and directories",
		},
		{
			name:    "symlink outside selection",
			entries: with(tarEntry{name: top("docs/link"), typeflag: tar.TypeSymlink, linkname: "../.."}),
			want:    "only files and directories",
		},
		{
			name:    "hardlink",
			entries: with(tarEntry{name: top("src/test/a/hard.json"), typeflag: tar.TypeLink, linkname: top("LICENSE")}),
			want:    "only files and directories",
		},
		{name: "char device", entries: with(tarEntry{name: top("dev"), typeflag: tar.TypeChar}), want: "only files and directories"},
		{name: "block device", entries: with(tarEntry{name: top("blk"), typeflag: tar.TypeBlock}), want: "only files and directories"},
		{name: "fifo", entries: with(tarEntry{name: top("fifo"), typeflag: tar.TypeFifo}), want: "only files and directories"},
		{name: "duplicate file", entries: with(file(top("src/schemas/json/a.json"), "{}")), want: "twice"},
		{
			name: "global header for another commit",
			entries: append([]tarEntry{{
				name: "pax_global_header", typeflag: tar.TypeXGlobalHeader,
				pax: map[string]string{"comment": strings.Repeat("f", 40)},
			}}, base[1:]...),
			want: "is for commit",
		},
		{name: "backslash", entries: with(file(top(`src\evil`), "x")), want: "not portable"},
		{name: "colon", entries: with(file(top("src/test/a/c:d.json"), "x")), want: "not portable"},
		{name: "control character", entries: with(file(top("src/test/a/\x01.json"), "x")), want: "control characters"},
		{name: "missing catalog", entries: slices.DeleteFunc(slices.Clone(base), func(e tarEntry) bool {
			return strings.HasSuffix(e.name, "catalog.json")
		}), want: "no src/api/json/catalog.json"},
		{name: "missing license", entries: slices.DeleteFunc(slices.Clone(base), func(e tarEntry) bool {
			return strings.HasSuffix(e.name, "/LICENSE")
		}), want: "LICENSE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTarballServer(t, gzipBytes(t, buildTar(t, tt.entries)))
			parent := t.TempDir()

			_, err := FetchSchemaStore(t.Context(), ts.fetcher, ts.base(), testCommit, filepath.Join(parent, "snap"))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}

			if fault.KindOf(err) != fault.Integrity {
				t.Fatalf("kind = %v", fault.KindOf(err))
			}

			if got := listTree(t, parent); len(got) != 0 {
				t.Fatalf("files left behind: %v", got)
			}
		})
	}
}

func TestFetchSchemaStoreLimits(t *testing.T) {
	tarball := gzipBytes(t, buildTar(t, validEntries()))
	ts := newTarballServer(t, tarball)

	unlimited := archiveLimits{maxEntries: 1000, maxTotalBytes: 1 << 20, maxExtractedBytes: 1 << 20, maxFileBytes: 1 << 20}

	tests := []struct {
		name   string
		limits func(archiveLimits) archiveLimits
		want   string
	}{
		{name: "entries", limits: func(l archiveLimits) archiveLimits { l.maxEntries = 5; return l }, want: "more than 5 entries"},
		{name: "total", limits: func(l archiveLimits) archiveLimits { l.maxTotalBytes = 100; return l }, want: "more than 100 bytes"},
		{name: "extracted", limits: func(l archiveLimits) archiveLimits { l.maxExtractedBytes = 40; return l }, want: "exceed 40 bytes"},
		{name: "file", limits: func(l archiveLimits) archiveLimits { l.maxFileBytes = 20; return l }, want: "larger than 20 bytes"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := t.TempDir()

			_, err := fetchSchemaStore(t.Context(), ts.fetcher, ts.base(), testCommit, filepath.Join(parent, "snap"), tt.limits(unlimited))
			if err == nil || !strings.Contains(err.Error(), tt.want) || fault.KindOf(err) != fault.Integrity {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}

			if got := listTree(t, parent); len(got) != 0 {
				t.Fatalf("files left behind: %v", got)
			}
		})
	}
}

func TestFetchSchemaStoreErrors(t *testing.T) {
	ts := newTarballServer(t, gzipBytes(t, buildTar(t, validEntries())))

	t.Run("invalid commit", func(t *testing.T) {
		_, err := FetchSchemaStore(t.Context(), ts.fetcher, ts.base(), "master", filepath.Join(t.TempDir(), "s"))
		if fault.KindOf(err) != fault.Usage {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("uppercase commit", func(t *testing.T) {
		_, err := FetchSchemaStore(t.Context(), ts.fetcher, ts.base(), strings.ToUpper(testCommit), filepath.Join(t.TempDir(), "s"))
		if fault.KindOf(err) != fault.Usage {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("destination exists", func(t *testing.T) {
		_, err := FetchSchemaStore(t.Context(), ts.fetcher, ts.base(), testCommit, t.TempDir())
		if fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("missing parent", func(t *testing.T) {
		_, err := FetchSchemaStore(t.Context(), ts.fetcher, ts.base(), testCommit, filepath.Join(t.TempDir(), "a", "b"))
		if fault.KindOf(err) != fault.Usage {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, err := FetchSchemaStore(t.Context(), ts.fetcher, ts.srv.URL+"/elsewhere", strings.Repeat("a", 40), filepath.Join(t.TempDir(), "s"))
		if fault.KindOf(err) != fault.Registry {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("not gzip", func(t *testing.T) {
		bad := newTarballServer(t, []byte("plain text"))
		parent := t.TempDir()

		_, err := FetchSchemaStore(t.Context(), bad.fetcher, bad.base(), testCommit, filepath.Join(parent, "s"))
		if fault.KindOf(err) != fault.Integrity || len(listTree(t, parent)) != 0 {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("truncated", func(t *testing.T) {
		raw := buildTar(t, validEntries())
		bad := newTarballServer(t, gzipBytes(t, raw[:len(raw)/2]))
		parent := t.TempDir()

		_, err := FetchSchemaStore(t.Context(), bad.fetcher, bad.base(), testCommit, filepath.Join(parent, "s"))
		if fault.KindOf(err) != fault.Integrity || len(listTree(t, parent)) != 0 {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		parent := t.TempDir()

		_, err := FetchSchemaStore(ctx, ts.fetcher, ts.base(), testCommit, filepath.Join(parent, "s"))
		if fault.KindOf(err) != fault.Canceled || len(listTree(t, parent)) != 0 {
			t.Fatalf("error = %v (kind %v)", err, fault.KindOf(err))
		}
	})
}

func TestFetchSchemaStoreChecksGzipStream(t *testing.T) {
	raw := buildTar(t, validEntries())
	valid := gzipBytes(t, raw)

	corrupt := func(offset int) []byte {
		data := slices.Clone(valid)
		data[len(data)+offset] ^= 0xff

		return data
	}

	tests := []struct {
		name    string
		tarball []byte
		want    string
	}{
		{name: "crc32", tarball: corrupt(-8), want: "checksum"},
		{name: "length", tarball: corrupt(-1), want: "checksum"},
		{name: "garbage after the member", tarball: append(slices.Clone(valid), "garbage after the gzip member"...), want: "after the gzip stream"},
		{name: "second member", tarball: append(slices.Clone(valid), gzipBytes(t, []byte("more"))...), want: "after the gzip stream"},
		{name: "data after the archive", tarball: gzipBytes(t, append(slices.Clone(raw), "trailing data"...)), want: "after the end of the tar archive"},
		{name: "excessive padding", tarball: gzipBytes(t, append(slices.Clone(raw), make([]byte, maxTarPadding+1)...)), want: "bytes of padding"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTarballServer(t, tt.tarball)
			parent := t.TempDir()

			_, err := FetchSchemaStore(t.Context(), ts.fetcher, ts.base(), testCommit, filepath.Join(parent, "s"))
			if err == nil || fault.KindOf(err) != fault.Integrity || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}

			if got := listTree(t, parent); len(got) != 0 {
				t.Fatalf("files left behind: %v", got)
			}
		})
	}

	t.Run("record padding", func(t *testing.T) {
		padded := append(slices.Clone(raw), make([]byte, 10240-len(raw)%10240)...)
		ts := newTarballServer(t, gzipBytes(t, padded))

		if _, err := FetchSchemaStore(t.Context(), ts.fetcher, ts.base(), testCommit, filepath.Join(t.TempDir(), "s")); err != nil {
			t.Fatal(err)
		}
	})
}

func TestExtractCanceledMidway(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	dir := t.TempDir()

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = root.Close() })

	err = extract(ctx, gzipBytes(t, buildTar(t, validEntries())), testCommit, root, defaultArchiveLimits)
	if !errors.Is(err, context.Canceled) || fault.KindOf(err) != fault.Canceled {
		t.Fatalf("error = %v", err)
	}
}

func TestCheckEntryName(t *testing.T) {
	valid := []string{"a", "a/b", "schemastore-x/src/test/function/HttpGET(CRUD).json", "a/api-with-$json.json", "a/GitHub CLI/x.yml"}
	invalid := []string{"", "/a", "a/", "a//b", "./a", "a/./b", "a/../b", "..", `a\b`, "a:b", "a*b", "a?b", "a\x00b", "a\x7fb", "\xff"}

	for _, name := range valid {
		if err := checkEntryName(name); err != nil {
			t.Errorf("checkEntryName(%q) = %v", name, err)
		}
	}

	for _, name := range invalid {
		if err := checkEntryName(name); err == nil {
			t.Errorf("checkEntryName(%q) accepted", name)
		}
	}
}

func FuzzExtract(f *testing.F) {
	f.Add(buildTar(f, []tarEntry{
		{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader, pax: map[string]string{"comment": testCommit}},
		dir(top("")),
		file(top("LICENSE"), "L"),
		file(top("src/api/json/catalog.json"), "{}"),
		file(top("src/schemas/json/a.json"), "{}"),
		file(top("src/test/a/x.json"), "{}"),
		file(top("docs/skipped.md"), "s"),
	}))
	f.Add(buildTar(f, []tarEntry{dir(top("")), file(top("../x"), "1")}))
	f.Add(buildTar(f, []tarEntry{{name: top("l"), typeflag: tar.TypeSymlink, linkname: "/"}}))
	f.Add([]byte{})

	limits := archiveLimits{maxEntries: 64, maxTotalBytes: 1 << 16, maxExtractedBytes: 1 << 15, maxFileBytes: 1 << 12}

	f.Fuzz(func(t *testing.T, raw []byte) {
		parent := t.TempDir()
		target := filepath.Join(parent, "root")
		if err := os.Mkdir(target, 0o750); err != nil {
			t.Fatal(err)
		}

		root, err := os.OpenRoot(target)
		if err != nil {
			t.Fatal(err)
		}

		defer func() { _ = root.Close() }()

		var gz bytes.Buffer

		zw, _ := gzip.NewWriterLevel(&gz, gzip.NoCompression)
		_, _ = zw.Write(raw)
		_ = zw.Close()

		_ = extract(context.Background(), gz.Bytes(), testCommit, root, limits)

		for _, p := range listTree(t, parent) {
			if p == "root/" {
				continue
			}

			rel, ok := strings.CutPrefix(p, "root/")
			if !ok {
				t.Fatalf("wrote outside the root: %s", p)
			}

			if !strings.HasSuffix(rel, "/") && !selected(rel) {
				t.Fatalf("extracted unselected file %s", rel)
			}

			info, err := os.Lstat(filepath.Join(parent, filepath.FromSlash(strings.TrimSuffix(p, "/"))))
			if err != nil {
				t.Fatal(err)
			}

			if !info.Mode().IsRegular() && !info.IsDir() {
				t.Fatalf("created special file %s (%v)", p, info.Mode())
			}
		}
	})
}
