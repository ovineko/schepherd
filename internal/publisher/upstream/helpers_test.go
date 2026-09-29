package upstream

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ovineko/schepherd/internal/publisher/httpfetch"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

const testCatalog = `{
  "$schema": "https://www.schemastore.org/schema-catalog.json",
  "version": 1,
  "schemas": [
    {"name": "A config", "description": "A", "fileMatch": ["a.json", "**/.a/*.yml"], "url": "https://www.schemastore.org/a.json"},
    {"name": "B", "description": "B", "url": "https://json.schemastore.org/b", "versions": {"1.0": "https://www.schemastore.org/b.json"}},
    {"name": "tsconfig", "description": "TS", "fileMatch": [], "url": "https://raw.githubusercontent.com/SchemaStore/schemastore/master/src/schemas/json/tsconfig.json"},
    {"name": "External", "description": "E", "fileMatch": ["external.json"], "url": "https://example.com/external.json"}
  ]
}`

type tarEntry struct {
	pax      map[string]string
	name     string
	body     string
	linkname string
	typeflag byte
}

func file(name, body string) tarEntry {
	return tarEntry{name: name, body: body, typeflag: tar.TypeReg}
}

func dir(name string) tarEntry {
	return tarEntry{name: name, typeflag: tar.TypeDir}
}

func top(rel string) string {
	return "schemastore-" + testCommit + "/" + rel
}

func validEntries() []tarEntry {
	return []tarEntry{
		{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader, pax: map[string]string{"comment": testCommit}},
		dir(top("")),
		file(top(".editorconfig"), "root = true\n"),
		file(top("LICENSE"), "Apache License\n"),
		file(top("NOTICE"), "Test notice\n"),
		file(top("cli.js"), "console.log(1)\n"),
		dir(top("src/")),
		dir(top("src/api/")),
		dir(top("src/api/json/")),
		file(top("src/api/json/catalog.json"), testCatalog),
		file(top("src/schema-validation.jsonc"), "{}"),
		dir(top("src/schemas/")),
		dir(top("src/schemas/json/")),
		file(top("src/schemas/json/a.json"), `{"type":"object"}`),
		file(top("src/schemas/json/b.json"), `{"$ref":"https://json.schemastore.org/a.json"}`),
		file(top("src/schemas/json/tsconfig.json"), `{}`),
		file(top("src/schemas/json/readme.md"), "# no\n"),
		file(top("src/schemas/json/sub/x.json"), `{}`),
		dir(top("src/test/")),
		file(top("src/test/a/two.yaml"), "a: 1\n"),
		file(top("src/test/a/one.json"), "{}"),
		file(top("src/test/b/nested/deep.toml"), "x = 1\n"),
		file(top("src/test/stray.json"), "{}"),
		file(top("src/negative_test/a/bad.json"), `{"a":`),
	}
}

func buildTar(tb testing.TB, entries []tarEntry) []byte {
	tb.Helper()

	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)

	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: 0o644, Linkname: e.linkname, PAXRecords: e.pax}
		if e.typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}

		if e.typeflag == tar.TypeXGlobalHeader {
			h = &tar.Header{Name: e.name, Typeflag: e.typeflag, PAXRecords: e.pax}
		}

		if err := tw.WriteHeader(h); err != nil {
			tb.Fatalf("write header %q: %v", e.name, err)
		}

		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				tb.Fatal(err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		tb.Fatal(err)
	}

	return buf.Bytes()
}

func gzipBytes(tb testing.TB, data []byte) []byte {
	tb.Helper()

	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		tb.Fatal(err)
	}

	if err := zw.Close(); err != nil {
		tb.Fatal(err)
	}

	return buf.Bytes()
}

type tarballServer struct {
	srv     *httptest.Server
	fetcher *httpfetch.Fetcher
	body    atomic.Pointer[[]byte]
	path    atomic.Pointer[string]
}

func newTarballServer(t *testing.T, body []byte) *tarballServer {
	t.Helper()

	ts := &tarballServer{}
	ts.body.Store(&body)

	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		ts.path.Store(&path)

		if !strings.HasSuffix(path, "/"+testCommit) {
			http.NotFound(w, r)

			return
		}

		w.Header().Set("Content-Type", "application/x-gzip")
		_, _ = w.Write(*ts.body.Load())
	}))
	t.Cleanup(ts.srv.Close)

	ts.fetcher = httpfetch.New(httpfetch.Policy{AllowHTTP: true, AllowPrivateHosts: []string{ts.srv.Listener.Addr().String()}})

	return ts
}

func (ts *tarballServer) base() string {
	return ts.srv.URL + "/SchemaStore/schemastore/tar.gz"
}
