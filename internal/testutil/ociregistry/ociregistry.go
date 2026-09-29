// Package ociregistry serves an in-memory OCI distribution registry over plain
// HTTP on a loopback address, for tests only. It implements what Schepherd's
// client and mirror use: manifests by digest or tag, blobs by digest,
// monolithic blob uploads and tag listing, for any number of repositories.
// Uploads are checked against their digest like a real registry would, and
// the digest is computed here rather than with Schepherd's own code, so a
// digest bug in the code under test cannot hide itself.
package ociregistry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"testing"
)

const maxBodyBytes = 64 << 20

var (
	manifestPath = regexp.MustCompile(`^/v2/(.+)/manifests/([^/]+)$`)
	uploadPath   = regexp.MustCompile(`^/v2/(.+)/blobs/uploads/([^/]*)$`)
	blobPath     = regexp.MustCompile(`^/v2/(.+)/blobs/([^/]+)$`)
	tagsPath     = regexp.MustCompile(`^/v2/(.+)/tags/list$`)
	digestSyntax = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type manifest struct {
	mediaType string
	data      []byte
}

type repository struct {
	manifests map[string]manifest
	blobs     map[string][]byte
	tags      map[string]string
}

// Registry is a running in-memory registry. It is safe for concurrent use.
type Registry struct {
	server   *httptest.Server
	repos    map[string]*repository
	uploads  map[string]string
	requests int
	nextID   int
	mu       sync.Mutex
}

// New starts a registry that is shut down when the test ends.
func New(tb testing.TB) *Registry {
	tb.Helper()

	r := &Registry{repos: map[string]*repository{}, uploads: map[string]string{}}
	r.server = httptest.NewServer(http.HandlerFunc(r.serve))
	tb.Cleanup(r.Close)

	return r
}

// Host returns the "host:port" the registry listens on.
func (r *Registry) Host() string {
	return r.server.Listener.Addr().String()
}

// Requests returns how many HTTP requests the registry has received.
func (r *Registry) Requests() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.requests
}

// Close stops the registry; later connections are refused. It may be called
// more than once.
func (r *Registry) Close() {
	r.server.Close()
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.requests++
	r.mu.Unlock()

	path := req.URL.Path

	if path == "/v2/" || path == "/v2" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")

		return
	}

	if m := tagsPath.FindStringSubmatch(path); m != nil && req.Method == http.MethodGet {
		r.serveTags(w, m[1])

		return
	}

	if m := manifestPath.FindStringSubmatch(path); m != nil {
		r.serveManifest(w, req, m[1], m[2])

		return
	}

	if m := uploadPath.FindStringSubmatch(path); m != nil {
		r.serveUpload(w, req, m[1], m[2])

		return
	}

	if m := blobPath.FindStringSubmatch(path); m != nil {
		r.serveBlob(w, req, m[1], m[2])

		return
	}

	writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "unsupported endpoint "+req.Method+" "+path)
}

func (r *Registry) repo(name string, create bool) *repository {
	repo, ok := r.repos[name]
	if !ok && create {
		repo = &repository{manifests: map[string]manifest{}, blobs: map[string][]byte{}, tags: map[string]string{}}
		r.repos[name] = repo
	}

	return repo
}

func (r *Registry) serveManifest(w http.ResponseWriter, req *http.Request, name, ref string) {
	switch req.Method {
	case http.MethodGet, http.MethodHead:
		r.mu.Lock()

		var (
			m     manifest
			dgst  = ref
			found bool
		)

		if repo := r.repo(name, false); repo != nil {
			if !isDigest(ref) {
				dgst = repo.tags[ref]
			}

			m, found = repo.manifests[dgst]
		}
		r.mu.Unlock()

		if !found {
			writeError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown: "+ref)

			return
		}

		writeContent(w, req, m.mediaType, dgst, m.data)
	case http.MethodPut:
		data, ok := readBody(w, req)
		if !ok {
			return
		}

		dgst := digestOf(data)
		if isDigest(ref) && ref != dgst {
			writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "manifest digest is "+dgst+", not "+ref)

			return
		}

		r.mu.Lock()
		repo := r.repo(name, true)

		repo.manifests[dgst] = manifest{mediaType: req.Header.Get("Content-Type"), data: data}
		if !isDigest(ref) {
			repo.tags[ref] = dgst
		}
		r.mu.Unlock()

		w.Header().Set("Location", "/v2/"+name+"/manifests/"+dgst)
		w.Header().Set("Docker-Content-Digest", dgst)
		w.WriteHeader(http.StatusCreated)
	default:
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", req.Method+" on a manifest")
	}
}

func (r *Registry) serveBlob(w http.ResponseWriter, req *http.Request, name, dgst string) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", req.Method+" on a blob")

		return
	}

	r.mu.Lock()

	var (
		data  []byte
		found bool
	)

	if repo := r.repo(name, false); repo != nil {
		data, found = repo.blobs[dgst]
	}
	r.mu.Unlock()

	if !found {
		writeError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown: "+dgst)

		return
	}

	writeContent(w, req, "application/octet-stream", dgst, data)
}

// serveUpload accepts a POST that starts an upload (or completes it at once
// with ?digest=) and the PUT that completes it with the whole content.
// Chunked PATCH uploads and cross-repository mounts are not offered; a mount
// request gets a plain upload session, which clients fall back to.
func (r *Registry) serveUpload(w http.ResponseWriter, req *http.Request, name, id string) {
	switch {
	case req.Method == http.MethodPost && id == "" && req.URL.Query().Get("digest") == "":
		r.mu.Lock()
		r.nextID++
		id = strconv.Itoa(r.nextID)
		r.uploads[id] = name
		r.mu.Unlock()

		w.Header().Set("Location", "/v2/"+name+"/blobs/uploads/"+id)
		w.Header().Set("Docker-Upload-Uuid", id)
		w.WriteHeader(http.StatusAccepted)
	case req.Method == http.MethodPost && id == "":
		r.storeBlob(w, req, name)
	case req.Method == http.MethodPut && id != "":
		r.mu.Lock()
		owner, ok := r.uploads[id]
		delete(r.uploads, id)
		r.mu.Unlock()

		if !ok || owner != name {
			writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload "+id+" unknown")

			return
		}

		r.storeBlob(w, req, name)
	default:
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", req.Method+" on an upload")
	}
}

func (r *Registry) storeBlob(w http.ResponseWriter, req *http.Request, name string) {
	want := req.URL.Query().Get("digest")
	if !isDigest(want) {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "missing or unsupported digest "+strconv.Quote(want))

		return
	}

	data, ok := readBody(w, req)
	if !ok {
		return
	}

	if got := digestOf(data); got != want {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "uploaded content has digest "+got+", not "+want)

		return
	}

	r.mu.Lock()
	r.repo(name, true).blobs[want] = data
	r.mu.Unlock()

	w.Header().Set("Location", "/v2/"+name+"/blobs/"+want)
	w.Header().Set("Docker-Content-Digest", want)
	w.WriteHeader(http.StatusCreated)
}

func (r *Registry) serveTags(w http.ResponseWriter, name string) {
	r.mu.Lock()

	var tags []string

	repo := r.repo(name, false)
	if repo != nil {
		tags = make([]string, 0, len(repo.tags))
		for tag := range repo.tags {
			tags = append(tags, tag)
		}
	}
	r.mu.Unlock()

	if repo == nil {
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "repository "+name+" unknown")

		return
	}

	slices.Sort(tags)

	body, err := json.Marshal(struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}{Name: name, Tags: tags})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())

		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func readBody(w http.ResponseWriter, req *http.Request) ([]byte, bool) {
	data, err := io.ReadAll(io.LimitReader(req.Body, maxBodyBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", err.Error())

		return nil, false
	}

	if len(data) > maxBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "SIZE_INVALID", "body exceeds "+strconv.Itoa(maxBodyBytes)+" bytes")

		return nil, false
	}

	return data, true
}

func writeContent(w http.ResponseWriter, req *http.Request, mediaType, dgst string, data []byte) {
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Docker-Content-Digest", dgst)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)

	if req.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	body, err := json.Marshal(map[string][]map[string]string{"errors": {{"code": code, "message": message}}})
	if err != nil {
		http.Error(w, message, status)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func isDigest(s string) bool {
	return digestSyntax.MatchString(s)
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)

	return "sha256:" + hex.EncodeToString(sum[:])
}
