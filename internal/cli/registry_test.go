package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
)

// testRegistry is an in-process OCI distribution server with several
// repositories, each optionally protected by its own basic-auth account.
type testRegistry struct {
	srv   *httptest.Server
	repos map[string]*testRepo
	mu    sync.Mutex
	// hang makes every request wait until the client gives up.
	hang     bool
	requests int
}

type testRepo struct {
	manifests map[string]testManifest
	blobs     map[string][]byte
	tags      map[string]string
	username  string
	password  string
	uploads   int
}

type testManifest struct {
	mediaType string
	data      []byte
}

func newTestRegistry(t *testing.T) *testRegistry {
	t.Helper()

	r := &testRegistry{repos: map[string]*testRepo{}}
	r.srv = httptest.NewServer(r)
	t.Cleanup(r.srv.Close)

	return r
}

var endpoints = []string{"/manifests/", "/blobs/uploads/", "/blobs/"}

func (r *testRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.requests++
	hang := r.hang
	r.mu.Unlock()

	if hang {
		<-req.Context().Done()

		return
	}

	rest, ok := strings.CutPrefix(req.URL.Path, "/v2/")
	if !ok {
		registryError(w, http.StatusNotFound, "NOT_FOUND")

		return
	}

	for _, endpoint := range endpoints {
		i := strings.Index(rest, endpoint)
		if i <= 0 {
			continue
		}

		r.mu.Lock()
		repo, known := r.repos[rest[:i]]
		r.mu.Unlock()

		if !known {
			registryError(w, http.StatusNotFound, "NAME_UNKNOWN")

			return
		}

		if user, pass, ok := req.BasicAuth(); repo.username != "" && (!ok || user != repo.username || pass != repo.password) {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			registryError(w, http.StatusUnauthorized, "UNAUTHORIZED")

			return
		}

		r.serve(w, req, rest[:i], repo, endpoint, rest[i+len(endpoint):])

		return
	}

	registryError(w, http.StatusNotFound, "NOT_FOUND")
}

func (r *testRegistry) host() string {
	u, err := url.Parse(r.srv.URL)
	if err != nil {
		panic(err)
	}

	return u.Host
}

// repo creates the repository path, protected by username and password when
// they are not empty.
func (r *testRegistry) repo(path, username, password string) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.repos[path] = &testRepo{
		manifests: map[string]testManifest{},
		blobs:     map[string][]byte{},
		tags:      map[string]string{},
		username:  username,
		password:  password,
	}

	return r.host() + "/" + path
}

func (r *testRegistry) setHang(hang bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.hang = hang
}

func (r *testRegistry) requestCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.requests
}

func (r *testRegistry) add(path string, p *artifact.Packed, tags ...string) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	repo := r.repos[path]
	dgst := p.Manifest.Descriptor.Digest.String()
	repo.manifests[dgst] = testManifest{mediaType: p.Manifest.Descriptor.MediaType, data: p.Manifest.Data}

	for _, b := range p.Blobs {
		repo.blobs[b.Descriptor.Digest.String()] = b.Data
	}

	for _, tag := range tags {
		repo.tags[tag] = dgst
	}

	return dgst
}

func (r *testRegistry) has(path, dgst string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	repo := r.repos[path]
	_, manifest := repo.manifests[dgst]
	_, blob := repo.blobs[dgst]

	return manifest || blob
}

func (r *testRegistry) tagged(path, tag string) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.repos[path].tags[tag]
}

func (r *testRegistry) serve(w http.ResponseWriter, req *http.Request, name string, repo *testRepo, endpoint, ref string) {
	switch {
	case endpoint == "/manifests/" && req.Method == http.MethodPut:
		data, err := io.ReadAll(req.Body)
		if err != nil {
			registryError(w, http.StatusBadRequest, "MANIFEST_INVALID")

			return
		}

		dgst := digest.FromBytes(data)

		r.mu.Lock()
		repo.manifests[dgst] = testManifest{mediaType: req.Header.Get("Content-Type"), data: data}
		if !strings.Contains(ref, ":") {
			repo.tags[ref] = dgst
		}
		r.mu.Unlock()

		w.Header().Set("Docker-Content-Digest", dgst)
		w.WriteHeader(http.StatusCreated)
	case endpoint == "/manifests/":
		r.mu.Lock()
		dgst := ref
		if !strings.Contains(ref, ":") {
			dgst = repo.tags[ref]
		}
		m, ok := repo.manifests[dgst]
		r.mu.Unlock()

		if !ok {
			registryError(w, http.StatusNotFound, "MANIFEST_UNKNOWN")

			return
		}

		w.Header().Set("Content-Type", m.mediaType)
		w.Header().Set("Docker-Content-Digest", dgst)
		writeBody(w, req, m.data)
	case endpoint == "/blobs/uploads/" && req.Method == http.MethodPost:
		r.mu.Lock()
		repo.uploads++
		id := repo.uploads
		r.mu.Unlock()

		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%d", name, id))
		w.WriteHeader(http.StatusAccepted)
	case endpoint == "/blobs/uploads/" && req.Method == http.MethodPut:
		data, err := io.ReadAll(req.Body)
		if err != nil || digest.FromBytes(data) != req.URL.Query().Get("digest") {
			registryError(w, http.StatusBadRequest, "DIGEST_INVALID")

			return
		}

		r.mu.Lock()
		repo.blobs[digest.FromBytes(data)] = data
		r.mu.Unlock()

		w.WriteHeader(http.StatusCreated)
	case endpoint == "/blobs/":
		r.mu.Lock()
		data, ok := repo.blobs[ref]
		r.mu.Unlock()

		if !ok {
			registryError(w, http.StatusNotFound, "BLOB_UNKNOWN")

			return
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		writeBody(w, req, data)
	default:
		registryError(w, http.StatusMethodNotAllowed, "UNSUPPORTED")
	}
}

func writeBody(w http.ResponseWriter, req *http.Request, data []byte) {
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)

	if req.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func registryError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": "test registry"}}})
}

// testSchemas are the schemas every published test catalog contains.
var testSchemas = map[string]string{
	"alpha": `{"type":"object"}`,
	"beta":  `{"type":"array"}`,
}

// publishCatalog pushes the test schemas and a catalog of them to path and
// returns the digest of the catalog index.
func (r *testRegistry) publishCatalog(t *testing.T, path string, tags ...string) string {
	t.Helper()

	entries := make([]catalog.Entry, 0, len(testSchemas))

	for _, id := range slices.Sorted(maps.Keys(testSchemas)) {
		packed, err := artifact.PackSchema([]byte(testSchemas[id]), nil)
		if err != nil {
			t.Fatal(err)
		}

		r.add(path, packed)

		entries = append(entries, catalog.Entry{
			ID:        id,
			Name:      id + ".json",
			FileMatch: []string{id + ".json"},
			Artifact: catalog.Descriptor{
				MediaType: artifact.ManifestMediaType,
				Digest:    packed.Manifest.Descriptor.Digest.String(),
				Size:      packed.Manifest.Descriptor.Size,
			},
		})
	}

	raw, err := catalog.Marshal(&catalog.Catalog{FormatVersion: catalog.FormatVersion, Revision: "20260924.1200", Schemas: entries})
	if err != nil {
		t.Fatal(err)
	}

	return r.publishRawCatalog(t, path, raw, tags...)
}

// publishRawCatalog pushes catalog bytes as they are, valid or not, with an
// index over the schema manifests they list as far as they can be read.
func (r *testRegistry) publishRawCatalog(t *testing.T, path string, raw []byte, tags ...string) string {
	t.Helper()

	var listed struct {
		Schemas []struct {
			Artifact catalog.Descriptor `json:"artifact"`
		} `json:"schemas"`
	}

	_ = json.Unmarshal(raw, &listed)

	schemas := []ocispec.Descriptor{}
	for _, e := range listed.Schemas {
		d := ocispec.Descriptor{MediaType: e.Artifact.MediaType, Digest: godigest.Digest(e.Artifact.Digest), Size: e.Artifact.Size}
		if !slices.ContainsFunc(schemas, func(o ocispec.Descriptor) bool { return o.Digest == d.Digest }) {
			schemas = append(schemas, d)
		}
	}

	packed, err := artifact.PackCatalog(raw, schemas)
	if err != nil {
		t.Fatal(err)
	}

	r.add(path, packed.Metadata)

	r.mu.Lock()
	defer r.mu.Unlock()

	repo := r.repos[path]
	dgst := packed.Index.Descriptor.Digest.String()
	repo.manifests[dgst] = testManifest{mediaType: artifact.IndexMediaType, data: packed.Index.Data}

	for _, tag := range tags {
		repo.tags[tag] = dgst
	}

	return dgst
}

// writeConfig writes a configuration file whose registries section lets the
// client talk plain HTTP to the test registry. extra is appended verbatim.
func (r *testRegistry) writeConfig(t *testing.T, extra string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "schepherd.toml")
	doc := "config_version = 1\n\n[registries." + strconv.Quote(r.host()) + "]\nplain_http = true\n\n" + extra

	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// isolateDockerConfig keeps the developer's Docker credentials out of reach.
func isolateDockerConfig(t *testing.T) {
	t.Helper()
	t.Setenv("DOCKER_CONFIG", t.TempDir())
}

// writeCredentials writes a Docker-format credentials file for one host.
func writeCredentials(t *testing.T, host, username, password string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "auth.json")
	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	doc := `{"auths":{` + strconv.Quote(host) + `:{"auth":` + strconv.Quote(auth) + `}}}`

	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}
