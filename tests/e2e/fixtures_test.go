//go:build e2e

package e2e

import (
	"bytes"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// stdinFixtures are byte-exact documents for the stdin runner mode. They
// live here rather than in fixtures/ because the repository's formatters
// normalize line endings and final newlines of text files. All of them are
// valid alpha instances.
var stdinFixtures = map[string][]byte{
	"crlf.json":                []byte("{\r\n  \"name\": \"crlf\",\r\n  \"port\": 8080\r\n}\r\n"),
	"comments.yaml":            []byte("# leading comment\nname: comments # trailing comment\n# between\nport: 8081\n"),
	"unicode.json":             []byte("{\"name\":\"café ☃ \U0001F411\",\"tags\":[\"üñîçødé\",\"∑∞\"]}\n"),
	"no-trailing-newline.json": []byte(`{"name":"no-newline","port":1}`),
	"mixed.yaml":               []byte("# CRLF, a comment, Unicode and no final newline\r\nname: \"müxed ✓\"\r\ntags: [a, b] # inline\r\nport: 443"),
}

// writeStdinFixtures writes every stdin fixture into dir and returns the
// absolute path per fixture name.
func writeStdinFixtures(t *testing.T, dir string) map[string]string {
	t.Helper()

	paths := make(map[string]string, len(stdinFixtures))

	for name, data := range stdinFixtures {
		path := filepath.Join(dir, name)
		writeFile(t, path, data)
		paths[name] = path
	}

	return paths
}

// instance returns the path of fixtures/instances/<schema>/<name>, for
// example instance("beta", "valid.toml").
func instance(schema, name string) string {
	return fixture("instances", schema, name)
}

// depServer serves dependency documents for the deps fixture over plain
// HTTP on 127.0.0.1; Set replaces a document between two preparations.
type depServer struct {
	mu    sync.Mutex
	files map[string][]byte
	hits  map[string]int
	srv   *httptest.Server
}

// newDepServer starts a server (closed at the end of the test) that serves
// fixtures/deps/served/dep.json as /dep.json.
func newDepServer(t *testing.T) *depServer {
	t.Helper()

	d := &depServer{
		files: map[string][]byte{"/dep.json": readFile(t, fixture("deps", "served", "dep.json"))},
		hits:  map[string]int{},
	}

	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		data, ok := d.files[r.URL.Path]
		d.hits[r.URL.Path]++
		d.mu.Unlock()

		if !ok {
			http.NotFound(w, r)

			return
		}

		w.Header().Set("Content-Type", "application/schema+json")
		_, _ = w.Write(data)
	}))
	t.Cleanup(d.srv.Close)

	return d
}

// Host is the server's 127.0.0.1:<port>.
func (d *depServer) Host() string {
	return strings.TrimPrefix(d.srv.URL, "http://")
}

// Set serves data at path (for example "/dep.json").
func (d *depServer) Set(path string, data []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.files[path] = bytes.Clone(data)
}

// Hits returns how often path was requested.
func (d *depServer) Hits(path string) int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.hits[path]
}

// newDepsSet copies fixtures/deps and fills in the dependency server's
// host:port wherever the template says {{DEP_HOST}}.
func newDepsSet(t *testing.T, depHost string) string {
	t.Helper()

	if _, _, err := net.SplitHostPort(depHost); err != nil {
		t.Fatalf("dependency host %q: %v", depHost, err)
	}

	set := newSet(t, "deps")

	err := filepath.WalkDir(set, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		return os.WriteFile(path, bytes.ReplaceAll(data, []byte("{{DEP_HOST}}"), []byte(depHost)), 0o644)
	})
	if err != nil {
		t.Fatalf("render deps set: %v", err)
	}

	return set
}
