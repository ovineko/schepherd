package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ovineko/schepherd/internal/digest"
)

// testRegistry is a minimal OCI distribution server on 127.0.0.1 holding
// any number of repositories, enough for the ORAS operations the publisher
// uses: manifest and blob HEAD/GET, monolithic blob uploads, manifest PUT by
// digest or tag, and tag listing.
type testRegistry struct {
	srv       *httptest.Server
	blobs     map[string][]byte
	manifests map[string]testManifest
	tags      map[string]map[string]string
	writes    int
	requests  int
	mu        sync.Mutex
}

type testManifest struct {
	mediaType string
	data      []byte
}

func newTestRegistry(t *testing.T) *testRegistry {
	t.Helper()

	r := &testRegistry{blobs: map[string][]byte{}, manifests: map[string]testManifest{}, tags: map[string]map[string]string{}}
	r.srv = httptest.NewServer(r)
	t.Cleanup(r.srv.Close)

	return r
}

func (r *testRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.requests++
	r.mu.Unlock()

	path := strings.TrimPrefix(req.URL.Path, "/v2/")
	if req.URL.Path == "/v2/" {
		w.WriteHeader(http.StatusOK)

		return
	}

	for _, endpoint := range []string{"/manifests/", "/blobs/uploads/", "/blobs/", "/tags/list"} {
		repo, ref, ok := strings.Cut(path, endpoint)
		if !ok || repo == "" {
			continue
		}

		switch {
		case endpoint == "/manifests/" && req.Method == http.MethodPut:
			r.putManifest(w, req, repo, ref)
		case endpoint == "/manifests/":
			r.getManifest(w, req, repo, ref)
		case endpoint == "/blobs/uploads/" && req.Method == http.MethodPost:
			w.Header().Set("Location", "/v2/"+repo+"/blobs/uploads/session")
			w.WriteHeader(http.StatusAccepted)
		case endpoint == "/blobs/uploads/" && req.Method == http.MethodPut:
			r.putBlob(w, req)
		case endpoint == "/blobs/":
			r.getBlob(w, req, ref)
		default:
			r.listTags(w, repo)
		}

		return
	}

	registryError(w, http.StatusNotFound, "UNSUPPORTED")
}

func registryError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"errors":[{"code":%q,"message":%q}]}`, code, strings.ToLower(code))
}

func (r *testRegistry) host() string {
	return strings.TrimPrefix(r.srv.URL, "http://")
}

func (r *testRegistry) putManifest(w http.ResponseWriter, req *http.Request, repo, ref string) {
	data, err := io.ReadAll(req.Body)
	if err != nil {
		registryError(w, http.StatusBadRequest, "MANIFEST_INVALID")

		return
	}

	dgst := digest.FromBytes(data)
	if strings.HasPrefix(ref, "sha256:") && ref != dgst {
		registryError(w, http.StatusBadRequest, "DIGEST_INVALID")

		return
	}

	r.mu.Lock()
	r.writes++
	r.manifests[dgst] = testManifest{mediaType: req.Header.Get("Content-Type"), data: data}

	if !strings.HasPrefix(ref, "sha256:") {
		if r.tags[repo] == nil {
			r.tags[repo] = map[string]string{}
		}

		r.tags[repo][ref] = dgst
	}
	r.mu.Unlock()

	w.Header().Set("Docker-Content-Digest", dgst)
	w.WriteHeader(http.StatusCreated)
}

func (r *testRegistry) getManifest(w http.ResponseWriter, req *http.Request, repo, ref string) {
	r.mu.Lock()
	dgst := ref
	if !strings.HasPrefix(ref, "sha256:") {
		dgst = r.tags[repo][ref]
	}
	m, ok := r.manifests[dgst]
	r.mu.Unlock()

	if !ok {
		registryError(w, http.StatusNotFound, "MANIFEST_UNKNOWN")

		return
	}

	w.Header().Set("Content-Type", m.mediaType)
	w.Header().Set("Docker-Content-Digest", dgst)
	w.Header().Set("Content-Length", strconv.Itoa(len(m.data)))
	w.WriteHeader(http.StatusOK)

	if req.Method == http.MethodGet {
		_, _ = w.Write(m.data)
	}
}

func (r *testRegistry) putBlob(w http.ResponseWriter, req *http.Request) {
	data, err := io.ReadAll(req.Body)
	dgst := req.URL.Query().Get("digest")

	if err != nil || digest.FromBytes(data) != dgst {
		registryError(w, http.StatusBadRequest, "DIGEST_INVALID")

		return
	}

	r.mu.Lock()
	r.writes++
	r.blobs[dgst] = data
	r.mu.Unlock()

	w.Header().Set("Docker-Content-Digest", dgst)
	w.WriteHeader(http.StatusCreated)
}

func (r *testRegistry) getBlob(w http.ResponseWriter, req *http.Request, dgst string) {
	r.mu.Lock()
	data, ok := r.blobs[dgst]
	r.mu.Unlock()

	if !ok {
		registryError(w, http.StatusNotFound, "BLOB_UNKNOWN")

		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", dgst)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)

	if req.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

func (r *testRegistry) listTags(w http.ResponseWriter, repo string) {
	r.mu.Lock()
	tags, ok := r.tags[repo]
	names := slices.Sorted(maps.Keys(tags))
	r.mu.Unlock()

	if !ok {
		registryError(w, http.StatusNotFound, "NAME_UNKNOWN")

		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": names})
}

func (r *testRegistry) tag(repo, tag string) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.tags[repo][tag]
}

func (r *testRegistry) writeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.writes
}

func (r *testRegistry) requestCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.requests
}
