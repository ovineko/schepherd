package wrappers

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

type entry struct {
	name string
	data string
	mode int64
}

func tarball(t *testing.T, gzipped bool, entries ...entry) []byte {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)

	tw := tar.NewWriter(&buf)
	if gzipped {
		tw = tar.NewWriter(gz)
	}

	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}

		if _, err := tw.Write([]byte(e.data)); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if !gzipped {
		return buf.Bytes()
	}

	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func wheel(t *testing.T, entries ...entry) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)

	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		h.SetMode(0o644)

		if e.mode&0o100 != 0 {
			h.SetMode(0o755)
		}

		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := w.Write([]byte(e.data)); err != nil {
			t.Fatal(err)
		}
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func staged(files map[string]string, exec ...string) map[string]member {
	want := map[string]member{}
	for name, data := range files {
		want[name] = member{sum: sha256.Sum256([]byte(data)), exec: strings.Contains(strings.Join(exec, "\n"), name)}
	}

	return want
}

func TestCheckPackage(t *testing.T) {
	want := staged(map[string]string{"package.json": "{}", "bin/schepherd": "ELF", "LICENSE": "MIT"}, "bin/schepherd")
	good := []entry{{"package.json", "{}", 0o644}, {"bin/schepherd", "ELF", 0o755}, {"LICENSE", "MIT", 0o644}}

	prefixed := func(entries []entry) []entry {
		out := make([]entry, 0, len(entries))
		for _, e := range entries {
			out = append(out, entry{"package/" + e.name, e.data, e.mode})
		}

		return out
	}

	dir := t.TempDir()
	check := func(name string, data []byte, generated ...string) error {
		t.Helper()

		file := filepath.Join(dir, name)
		writeFile(t, file, data)

		return checkPackage(file, want, generated)
	}

	if err := check("ok.tgz", tarball(t, true, prefixed(good)...)); err != nil {
		t.Errorf("npm tarball: %v", err)
	}

	if err := check("ok.whl", wheel(t, slices.Concat(good, []entry{{"x.dist-info/RECORD", "anything", 0o644}})...), "x.dist-info/RECORD"); err != nil {
		t.Errorf("wheel: %v", err)
	}

	gem := tarball(t, false, entry{"metadata.gz", "spec", 0o444}, entry{"data.tar.gz", string(tarball(t, true, good...)), 0o444})
	if err := check("ok.gem", gem); err != nil {
		t.Errorf("gem: %v", err)
	}

	for want, entries := range map[string][]entry{
		"bin/schepherd differs from its source":         {good[0], {"bin/schepherd", "PE", 0o755}, good[2]},
		"bin/schepherd is executable: false, want true": {good[0], {"bin/schepherd", "ELF", 0o644}, good[2]},
		"LICENSE is missing":                            good[:2],
		"schema.json is not allowed":                    append(good, entry{"schema.json", "{}", 0o644}),
		"package.json is executable: true, want false":  {{"package.json", "{}", 0o755}, good[1], good[2]},
		`unexpected entry "../../etc/passwd"`:           slices.Concat(good, []entry{{"../../etc/passwd", "x", 0o644}}),
	} {
		// checkPackage does not compare execute bits on Windows.
		if runtime.GOOS == "windows" && strings.Contains(want, "is executable") {
			continue
		}

		if err := check("bad.tgz", tarball(t, true, prefixed(entries)...)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: %v, want %q", entries, err, want)
		}
	}

	if err := check("missing.whl", wheel(t, good...), "x.dist-info/WHEEL"); err == nil || !strings.Contains(err.Error(), "x.dist-info/WHEEL is missing") {
		t.Errorf("a wheel without a generated file: %v", err)
	}

	if err := check("empty.gem", tarball(t, false, entry{"metadata.gz", "spec", 0o444})); err == nil || !strings.Contains(err.Error(), "no data.tar.gz") {
		t.Errorf("a gem without data: %v", err)
	}

	truncated := tarball(t, true, prefixed(good)...)
	if err := check("truncated.tgz", truncated[:len(truncated)-4]); err == nil {
		t.Error("a tarball without its gzip trailer passed")
	}
}
