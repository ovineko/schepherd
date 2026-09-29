package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"oras.land/oras-go/v2/registry/remote/errcode"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/registry"
)

const fakeRepo = "org/schemas"

type storedManifest struct {
	mediaType string
	data      []byte
}

// blobFault changes how the fake serves one blob body. The headers always
// announce the true digest, so only the body betrays the fault.
type blobFault int

const (
	serveIntact blobFault = iota
	// serveFlipped keeps the length and flips one byte.
	serveFlipped
	// serveTruncated sends half of the body without a Content-Length and
	// ends the response cleanly, so the transport reports no error.
	serveTruncated
	// serveTrailing appends one byte without a Content-Length.
	serveTrailing
)

// fakeRegistry is a minimal in-memory OCI distribution server for one
// repository, enough for ORAS copies, tags and verified fetches.
type fakeRegistry struct {
	manifests map[string]storedManifest
	blobs     map[string][]byte
	tags      map[string]string
	faults    map[string]blobFault
	// redirects answers GETs of a blob with a 307 to the URL stored under
	// its digest, as registries do with presigned storage URLs.
	redirects map[string]string
	// onPut runs before a manifest or blob upload is stored. When it returns
	// true the upload is dropped and the handler waits for the client to go
	// away, as a registry does for a request the client abandons.
	onPut   func(kind, ref string) bool
	uploads int
	mu      sync.Mutex
	// lenient accepts blob uploads whose body does not match the digest in
	// the request, like a registry that does not verify uploads.
	lenient bool
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{
		manifests: map[string]storedManifest{},
		blobs:     map[string][]byte{},
		tags:      map[string]string{},
		faults:    map[string]blobFault{},
		redirects: map[string]string{},
	}
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/v2/"+fakeRepo+"/")
	if !ok {
		writeRegistryError(w, http.StatusNotFound, errcode.ErrorCodeNameUnknown)

		return
	}

	switch {
	case strings.HasPrefix(rest, "manifests/"):
		ref := strings.TrimPrefix(rest, "manifests/")
		if r.Method == http.MethodPut {
			f.putManifest(w, r, ref)
		} else {
			f.serveManifest(w, r, ref)
		}
	case rest == "blobs/uploads/" && r.Method == http.MethodPost:
		f.startUpload(w)
	case strings.HasPrefix(rest, "blobs/uploads/") && r.Method == http.MethodPut:
		f.finishUpload(w, r)
	case strings.HasPrefix(rest, "blobs/"):
		f.serveBlob(w, r, strings.TrimPrefix(rest, "blobs/"))
	default:
		writeRegistryError(w, http.StatusNotFound, "UNSUPPORTED")
	}
}

func (f *fakeRegistry) start(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	return u.Host
}

func (f *fakeRegistry) serveManifest(w http.ResponseWriter, r *http.Request, ref string) {
	f.mu.Lock()
	dgst := ref
	if !strings.Contains(ref, ":") {
		dgst = f.tags[ref]
	}
	m, ok := f.manifests[dgst]
	f.mu.Unlock()

	if !ok {
		writeRegistryError(w, http.StatusNotFound, errcode.ErrorCodeManifestUnknown)

		return
	}

	w.Header().Set("Content-Type", m.mediaType)
	w.Header().Set("Docker-Content-Digest", dgst)
	w.Header().Set("Content-Length", strconv.Itoa(len(m.data)))
	w.WriteHeader(http.StatusOK)

	if r.Method != http.MethodHead {
		_, _ = w.Write(m.data)
	}
}

func (f *fakeRegistry) putManifest(w http.ResponseWriter, r *http.Request, ref string) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeRegistryError(w, http.StatusBadRequest, errcode.ErrorCodeManifestInvalid)

		return
	}

	if f.dropped(r, "manifest", ref) {
		return
	}

	dgst := digest.FromBytes(data)
	isDigest := strings.Contains(ref, ":")

	if isDigest && ref != dgst {
		writeRegistryError(w, http.StatusBadRequest, errcode.ErrorCodeDigestInvalid)

		return
	}

	f.mu.Lock()
	f.manifests[dgst] = storedManifest{mediaType: r.Header.Get("Content-Type"), data: data}
	if !isDigest {
		f.tags[ref] = dgst
	}
	f.mu.Unlock()

	w.Header().Set("Docker-Content-Digest", dgst)
	w.WriteHeader(http.StatusCreated)
}

func (f *fakeRegistry) serveBlob(w http.ResponseWriter, r *http.Request, dgst string) {
	f.mu.Lock()
	data, ok := f.blobs[dgst]
	served := f.faults[dgst]
	redirect := f.redirects[dgst]
	f.mu.Unlock()

	if !ok {
		writeRegistryError(w, http.StatusNotFound, errcode.ErrorCodeBlobUnknown)

		return
	}

	if redirect != "" && r.Method == http.MethodGet {
		http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)

		return
	}

	header := w.Header()
	header.Set("Content-Type", "application/octet-stream")
	header.Set("Docker-Content-Digest", dgst)

	if r.Method == http.MethodHead {
		header.Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)

		return
	}

	switch served {
	case serveFlipped:
		body := slices.Clone(data)
		body[len(body)/2] ^= 0x01
		header.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	case serveTruncated:
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = w.Write(data[:len(data)/2])
	case serveTrailing:
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = w.Write(append(slices.Clone(data), '\n'))
	case serveIntact:
		header.Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

func (f *fakeRegistry) startUpload(w http.ResponseWriter) {
	f.mu.Lock()
	f.uploads++
	id := f.uploads
	f.mu.Unlock()

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%d", fakeRepo, id))
	w.WriteHeader(http.StatusAccepted)
}

func (f *fakeRegistry) finishUpload(w http.ResponseWriter, r *http.Request) {
	dgst := r.URL.Query().Get("digest")

	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeRegistryError(w, http.StatusBadRequest, errcode.ErrorCodeBlobUploadInvalid)

		return
	}

	if f.dropped(r, "blob", dgst) {
		return
	}

	if !f.lenient && digest.FromBytes(data) != dgst {
		writeRegistryError(w, http.StatusBadRequest, errcode.ErrorCodeDigestInvalid)

		return
	}

	f.mu.Lock()
	f.blobs[dgst] = data
	f.mu.Unlock()

	w.Header().Set("Docker-Content-Digest", dgst)
	w.Header().Set("Location", "/v2/"+fakeRepo+"/blobs/"+dgst)
	w.WriteHeader(http.StatusCreated)
}

func (f *fakeRegistry) dropped(r *http.Request, kind, ref string) bool {
	f.mu.Lock()
	onPut := f.onPut
	f.mu.Unlock()

	if onPut == nil || !onPut(kind, ref) {
		return false
	}

	select {
	case <-r.Context().Done():
	case <-time.After(10 * time.Second):
	}

	return true
}

func (f *fakeRegistry) setOnPut(fn func(kind, ref string) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.onPut = fn
}

func (f *fakeRegistry) addBlob(data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.blobs[digest.FromBytes(data)] = slices.Clone(data)
}

func (f *fakeRegistry) addManifest(mediaType string, data []byte, tags ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	dgst := digest.FromBytes(data)
	f.manifests[dgst] = storedManifest{mediaType: mediaType, data: slices.Clone(data)}

	for _, tag := range tags {
		f.tags[tag] = dgst
	}
}

func (f *fakeRegistry) setFault(dgst string, served blobFault) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.faults[dgst] = served
}

func (f *fakeRegistry) setRedirect(dgst, target string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.redirects[dgst] = target
}

func (f *fakeRegistry) tagMap() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return maps.Clone(f.tags)
}

func (f *fakeRegistry) hasManifest(dgst string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	_, ok := f.manifests[dgst]

	return ok
}

func (f *fakeRegistry) hasBlob(dgst string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	_, ok := f.blobs[dgst]

	return ok
}

// assertContentIntact fails when any stored manifest or blob does not hash to
// the digest it is stored under.
func (f *fakeRegistry) assertContentIntact(t *testing.T) {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	for dgst, m := range f.manifests {
		if got := digest.FromBytes(m.data); got != dgst {
			t.Errorf("manifest %s holds content %s", dgst, got)
		}
	}

	for dgst, data := range f.blobs {
		if got := digest.FromBytes(data); got != dgst {
			t.Errorf("blob %s holds content %s", dgst, got)
		}
	}
}

func writeRegistryError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{{"code": code, "message": "fake registry"}},
	})
}

// openRepos starts one server per fake and opens their repositories with a
// client that never retries, so a failure surfaces on the first attempt.
func openRepos(t *testing.T, fakes ...*fakeRegistry) []*registry.Repo {
	t.Helper()

	hosts := map[string]registry.HostConfig{}
	names := make([]string, 0, len(fakes))

	for _, f := range fakes {
		host := f.start(t)
		hosts[host] = registry.HostConfig{PlainHTTP: true}
		names = append(names, host)
	}

	client := registry.NewClient(registry.Options{
		Hosts:       hosts,
		UserAgent:   "schepherd-test",
		RetryPolicy: &retry.GenericPolicy{Retryable: retry.DefaultPredicate, Backoff: retry.DefaultBackoff, MaxRetry: 0},
	})

	repos := make([]*registry.Repo, 0, len(fakes))

	for _, host := range names {
		repo, err := client.Open(registry.Repository{Host: host, Path: fakeRepo})
		if err != nil {
			t.Fatal(err)
		}

		repos = append(repos, repo)
	}

	return repos
}

func withTimeout(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()

	return context.WithTimeout(t.Context(), 30*time.Second)
}
