package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHostPlatform(t *testing.T) {
	glibc := func(name string) bool { return strings.Contains(name, "ld-linux") }
	musl := func(name string) bool { return strings.Contains(name, "ld-musl") }
	none := func(string) bool { return false }

	cases := []struct {
		goos, goarch string
		exists       func(string) bool
		want         string
	}{
		{"linux", "amd64", glibc, "linux-x86_64"},
		{"linux", "amd64", musl, "linux-x86_64-musl"},
		{"linux", "amd64", none, "linux-x86_64"},
		{"linux", "arm64", musl, "linux-arm64-musl"},
		{"linux", "arm64", glibc, "linux-arm64"},
		{"darwin", "arm64", none, "darwin-arm64"},
		{"darwin", "amd64", none, "darwin-x86_64"},
		{"windows", "amd64", none, "windows-x86_64"},
	}

	for _, tc := range cases {
		got, err := hostPlatform(tc.goos, tc.goarch, tc.exists)
		if err != nil || got != tc.want {
			t.Errorf("hostPlatform(%s, %s) = %q, %v; want %q", tc.goos, tc.goarch, got, err, tc.want)
		}
	}

	for _, target := range [][2]string{{"windows", "arm64"}, {"linux", "386"}, {"freebsd", "amd64"}, {"plan9", "arm64"}} {
		if got, err := hostPlatform(target[0], target[1], none); err == nil {
			t.Errorf("hostPlatform(%s/%s) = %q, want an error", target[0], target[1], got)
		}
	}
}

func zipArchive(t *testing.T, entries map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer

	w := zip.NewWriter(&buf)
	for name, content := range entries {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func serve(t *testing.T, body []byte) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tool.zip" {
			http.NotFound(w, r)

			return
		}

		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	return srv
}

func TestDownloadVerifiesChecksumAndLimit(t *testing.T) {
	body := zipArchive(t, map[string]string{"pkg/bin/tool": "binary"})
	srv := serve(t, body)
	dir := t.TempDir()
	in := installer{client: srv.Client(), archiveLimit: 1 << 20, binaryLimit: 1 << 20}

	archive, err := in.download(t.Context(), srv.URL+"/tool.zip", sha(body), dir)
	if err != nil {
		t.Fatalf("download = %v", err)
	}

	if got, err := os.ReadFile(archive); err != nil || !bytes.Equal(got, body) {
		t.Errorf("archive content differs: %v", err)
	}

	if err := os.Remove(archive); err != nil {
		t.Fatal(err)
	}

	failures := map[string]struct {
		in   installer
		link string
		sum  string
	}{
		"checksum mismatch": {in: in, link: srv.URL + "/tool.zip", sum: strings.Repeat("0", 64)},
		"not found":         {in: in, link: srv.URL + "/missing.zip", sum: sha(body)},
		"too large":         {in: installer{client: srv.Client(), archiveLimit: int64(len(body) - 1)}, link: srv.URL + "/tool.zip", sum: sha(body)},
	}

	for name, tc := range failures {
		if _, err := tc.in.download(t.Context(), tc.link, tc.sum, dir); err == nil {
			t.Errorf("%s: download succeeded", name)
		}
	}

	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("failed downloads left files behind: %v %v", entries, err)
	}
}

func TestExtract(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.zip")

	data := zipArchive(t, map[string]string{
		"pkg/bin/tool":      "#!/bin/sh\necho 1.0.0\n",
		"pkg/share/doc.txt": "doc",
	})
	if err := os.WriteFile(archive, data, 0o600); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(dir, "tool")

	if err := extract(archive, "pkg/bin/tool", target, 1<<20); err != nil {
		t.Fatalf("extract = %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil || string(got) != "#!/bin/sh\necho 1.0.0\n" {
		t.Errorf("extracted %q, %v", got, err)
	}

	if info, err := os.Stat(target); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != executableMode {
		t.Errorf("mode = %v, %v", info.Mode(), err)
	}

	if err := extract(archive, "pkg/bin/other", target, 1<<20); err == nil {
		t.Error("extract of a missing entry succeeded")
	}

	if err := extract(archive, "pkg/bin/tool", filepath.Join(dir, "small"), 4); err == nil {
		t.Error("extract ignored the size limit")
	}

	if _, err := os.Stat(filepath.Join(dir, "small")); !os.IsNotExist(err) {
		t.Errorf("oversized entry was installed: %v", err)
	}

	if err := extract(filepath.Join(dir, "missing.zip"), "pkg/bin/tool", target, 1<<20); err == nil {
		t.Error("extract of a missing archive succeeded")
	}
}

// rewriteTransport sends every request to the test server, standing in for
// the release host without touching the network.
type rewriteTransport struct {
	target *url.URL
	seen   []string
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.seen = append(rt.seen, req.URL.String())

	clone := req.Clone(req.Context())
	clone.URL.Scheme = rt.target.Scheme
	clone.URL.Host = rt.target.Host
	clone.URL.Path = "/tool.zip"
	clone.Host = rt.target.Host

	return http.DefaultTransport.RoundTrip(clone)
}

func TestInstallRejectsArchiveNotMatchingPins(t *testing.T) {
	srv := serve(t, zipArchive(t, map[string]string{"jsonschema-16.12.0-linux-x86_64/bin/jsonschema": "not the pinned build"}))

	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	transport := &rewriteTransport{target: target}
	dest := filepath.Join(t.TempDir(), "bin")
	in := installer{client: &http.Client{Transport: transport}, archiveLimit: 1 << 20, binaryLimit: 1 << 20}

	_, err = in.install(t.Context(), options{dest: dest, platform: "linux-x86_64", force: true})
	if err == nil || !strings.Contains(err.Error(), "does not match pinned 9808618bc9579a11412154ab329f6db81bbd8e51e05167e3a6fab8e771f54183") {
		t.Fatalf("install = %v, want a checksum mismatch", err)
	}

	want := "https://github.com/sourcemeta/jsonschema/releases/download/v16.12.0/jsonschema-16.12.0-linux-x86_64.zip"
	if len(transport.seen) != 1 || transport.seen[0] != want {
		t.Errorf("requested %v, want %s", transport.seen, want)
	}

	if entries, err := os.ReadDir(dest); err != nil || len(entries) != 0 {
		t.Errorf("destination is not empty after a rejected download: %v %v", entries, err)
	}

	if _, err := in.install(t.Context(), options{dest: dest, platform: "windows-arm64"}); err == nil {
		t.Error("install for an unpinned platform succeeded")
	}
}

func TestRunRejectsBadArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if code := run(t.Context(), []string{"extra"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "unexpected arguments") {
		t.Errorf("run(extra) = %d, stderr %q", code, stderr.String())
	}

	stderr.Reset()

	if code := run(t.Context(), []string{"-nope"}, &stdout, &stderr); code != 2 {
		t.Errorf("run(-nope) = %d", code)
	}

	if stdout.Len() != 0 {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestModuleRoot(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b")

	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Chdir(nested)

	got, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}

	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}

	if resolved, err := filepath.EvalSymlinks(got); err != nil || resolved != want {
		t.Errorf("moduleRoot = %s, want %s", got, want)
	}
}

func TestBinaryName(t *testing.T) {
	if got := binaryName("jsonschema-16.12.0-windows-x86_64/bin/jsonschema.exe"); got != "jsonschema.exe" {
		t.Errorf("binaryName = %q", got)
	}
}
