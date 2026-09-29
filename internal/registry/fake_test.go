package registry

import (
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
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
	"oras.land/oras-go/v2/registry/remote/errcode"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

const fakeTokenPath = "/token"

type fakeRequest struct {
	Method        string
	Path          string
	Authorization string
	UserAgent     string
}

type fakeManifest struct {
	mediaType string
	data      []byte
}

// fakeRegistry is a minimal OCI distribution server for one repository.
// Behaviour switches must be set before the server receives requests.
type fakeRegistry struct {
	manifests map[string]fakeManifest
	blobs     map[string][]byte
	tags      map[string]string

	repo     string
	username string
	password string
	// redirectBlobsTo answers blob GETs with a 307 to this base URL.
	redirectBlobsTo string
	// redirectManifestsTo answers manifest GETs with a 307 to this base URL.
	redirectManifestsTo string
	// redirectQuery is the query of every redirect target, such as the
	// signature of a presigned storage URL.
	redirectQuery string
	// bearerToken, when set, protects every request with a bearer challenge
	// whose realm is /token on the same server.
	bearerToken string

	requests []fakeRequest
	uploads  int
	// headSize overrides the Content-Length reported for manifest HEADs.
	headSize int64
	// status makes every authorized request fail with this status.
	status int
	// tokenStatus makes the token endpoint fail with this status.
	tokenStatus int

	mu sync.Mutex

	omitDigestHeader bool
	tamperManifests  bool
	chunkedManifests bool
	tamperBlobs      bool
	truncateBlobs    bool
	trailingBlobByte bool
}

func newFakeRegistry(repo string) *fakeRegistry {
	return &fakeRegistry{
		repo:      repo,
		manifests: map[string]fakeManifest{},
		blobs:     map[string][]byte{},
		tags:      map[string]string{},
	}
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{
		Method:        r.Method,
		Path:          r.URL.Path,
		Authorization: r.Header.Get("Authorization"),
		UserAgent:     r.Header.Get("User-Agent"),
	})
	f.mu.Unlock()

	if r.URL.Path == fakeTokenPath {
		f.serveToken(w)

		return
	}

	if f.bearerToken != "" && r.Header.Get("Authorization") != "Bearer "+f.bearerToken {
		w.Header().Set("WWW-Authenticate", `Bearer realm="http://`+r.Host+fakeTokenPath+`",service="fake"`)
		writeRegistryError(w, http.StatusUnauthorized, errcode.ErrorCodeUnauthorized)

		return
	}

	if f.username != "" {
		user, pass, ok := r.BasicAuth()
		if !ok || user != f.username || pass != f.password {
			w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
			writeRegistryError(w, http.StatusUnauthorized, errcode.ErrorCodeUnauthorized)

			return
		}
	}

	f.mu.Lock()
	status := f.status
	f.mu.Unlock()

	if status != 0 {
		writeRegistryError(w, status, "UNKNOWN")

		return
	}

	rest, ok := strings.CutPrefix(r.URL.Path, "/v2/"+f.repo+"/")
	if !ok {
		writeRegistryError(w, http.StatusNotFound, errcode.ErrorCodeNameUnknown)

		return
	}

	switch {
	case strings.HasPrefix(rest, "manifests/"):
		f.serveManifest(w, r, strings.TrimPrefix(rest, "manifests/"))
	case rest == "blobs/uploads/" && r.Method == http.MethodPost:
		f.startUpload(w)
	case strings.HasPrefix(rest, "blobs/uploads/") && r.Method == http.MethodPut:
		f.finishUpload(w, r)
	case strings.HasPrefix(rest, "blobs/"):
		f.serveBlob(w, r, strings.TrimPrefix(rest, "blobs/"))
	case rest == "tags/list":
		f.serveTags(w)
	default:
		writeRegistryError(w, http.StatusNotFound, "UNSUPPORTED")
	}
}

func (f *fakeRegistry) serveToken(w http.ResponseWriter) {
	if f.tokenStatus != 0 {
		writeRegistryError(w, f.tokenStatus, "NOT_FOUND")

		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": f.bearerToken})
}

func (f *fakeRegistry) start(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	return srv
}

// startTLS silences the server log because the tests deliberately fail
// handshakes against it.
func (f *fakeRegistry) startTLS(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewUnstartedServer(f)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return srv
}

func (f *fakeRegistry) addBlob(data []byte) ocispec.Descriptor {
	f.mu.Lock()
	defer f.mu.Unlock()

	dgst := digest.FromBytes(data)
	f.blobs[dgst] = slices.Clone(data)

	return ocispec.Descriptor{MediaType: "application/octet-stream", Digest: godigest.Digest(dgst), Size: int64(len(data))}
}

func (f *fakeRegistry) addManifest(mediaType string, data []byte, tags ...string) ocispec.Descriptor {
	f.mu.Lock()
	defer f.mu.Unlock()

	dgst := digest.FromBytes(data)
	f.manifests[dgst] = fakeManifest{mediaType: mediaType, data: slices.Clone(data)}

	for _, tag := range tags {
		f.tags[tag] = dgst
	}

	return ocispec.Descriptor{MediaType: mediaType, Digest: godigest.Digest(dgst), Size: int64(len(data))}
}

func (f *fakeRegistry) setStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.status = status
}

func (f *fakeRegistry) tagged(tag string) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.tags[tag]
}

func (f *fakeRegistry) recorded() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.requests)
}

func (f *fakeRegistry) count(method, pathPrefix string) int {
	n := 0

	for _, req := range f.recorded() {
		if req.Method == method && strings.HasPrefix(req.Path, pathPrefix) {
			n++
		}
	}

	return n
}

func (f *fakeRegistry) serveManifest(w http.ResponseWriter, r *http.Request, ref string) {
	if r.Method == http.MethodPut {
		f.putManifest(w, r, ref)

		return
	}

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

	if r.Method == http.MethodGet && f.redirectManifestsTo != "" {
		f.redirect(w, r, f.redirectManifestsTo)

		return
	}

	header := w.Header()
	header.Set("Content-Type", m.mediaType)

	if !f.omitDigestHeader {
		header.Set("Docker-Content-Digest", dgst)
	}

	if r.Method == http.MethodHead {
		size := int64(len(m.data))
		if f.headSize > 0 {
			size = f.headSize
		}

		header.Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)

		return
	}

	body := m.data
	if f.tamperManifests {
		body = tamper(body)
	}

	if f.chunkedManifests {
		writeChunked(w, body)

		return
	}

	header.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// redirect sends a 307 to the request path under base, with redirectQuery
// as the query when it is set.
func (f *fakeRegistry) redirect(w http.ResponseWriter, r *http.Request, base string) {
	target := base + r.URL.Path
	if f.redirectQuery != "" {
		target += "?" + f.redirectQuery
	}

	http.Redirect(w, r, target, http.StatusTemporaryRedirect)
}

func (f *fakeRegistry) putManifest(w http.ResponseWriter, r *http.Request, ref string) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeRegistryError(w, http.StatusBadRequest, errcode.ErrorCodeManifestInvalid)

		return
	}

	dgst := digest.FromBytes(data)
	isDigest := strings.Contains(ref, ":")

	if isDigest && ref != dgst {
		writeRegistryError(w, http.StatusBadRequest, errcode.ErrorCodeDigestInvalid)

		return
	}

	f.mu.Lock()
	f.manifests[dgst] = fakeManifest{mediaType: r.Header.Get("Content-Type"), data: data}
	if !isDigest {
		f.tags[ref] = dgst
	}
	f.mu.Unlock()

	w.Header().Set("Docker-Content-Digest", dgst)
	w.Header().Set("Location", r.URL.Path)
	w.WriteHeader(http.StatusCreated)
}

func (f *fakeRegistry) serveBlob(w http.ResponseWriter, r *http.Request, dgst string) {
	f.mu.Lock()
	data, ok := f.blobs[dgst]
	f.mu.Unlock()

	if !ok {
		writeRegistryError(w, http.StatusNotFound, errcode.ErrorCodeBlobUnknown)

		return
	}

	if r.Method == http.MethodGet && f.redirectBlobsTo != "" {
		f.redirect(w, r, f.redirectBlobsTo)

		return
	}

	header := w.Header()
	header.Set("Content-Type", "application/octet-stream")
	header.Set("Docker-Content-Digest", dgst)

	switch {
	case r.Method == http.MethodHead:
		header.Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
	case f.tamperBlobs:
		header.Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(tamper(data))
	case f.truncateBlobs:
		header.Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data[:len(data)/2])
	case f.trailingBlobByte:
		writeChunked(w, append(slices.Clone(data), 'x'))
	default:
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

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%d", f.repo, id))
	w.WriteHeader(http.StatusAccepted)
}

func (f *fakeRegistry) finishUpload(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeRegistryError(w, http.StatusBadRequest, errcode.ErrorCodeBlobUploadInvalid)

		return
	}

	dgst := r.URL.Query().Get("digest")
	if digest.FromBytes(data) != dgst {
		writeRegistryError(w, http.StatusBadRequest, errcode.ErrorCodeDigestInvalid)

		return
	}

	f.mu.Lock()
	f.blobs[dgst] = data
	f.mu.Unlock()

	w.Header().Set("Docker-Content-Digest", dgst)
	w.Header().Set("Location", "/v2/"+f.repo+"/blobs/"+dgst)
	w.WriteHeader(http.StatusCreated)
}

func (f *fakeRegistry) serveTags(w http.ResponseWriter) {
	f.mu.Lock()
	tags := make([]string, 0, len(f.tags))
	for tag := range f.tags {
		tags = append(tags, tag)
	}
	f.mu.Unlock()

	slices.Sort(tags)
	slices.Reverse(tags)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": f.repo, "tags": tags})
}

// blobMirror stands in for a storage backend a registry redirects blob
// downloads to. It records every Authorization header it receives, and a
// request under /hop/ is redirected once more to the same path under /final/
// on the same server.
type blobMirror struct {
	source         *fakeRegistry
	authorizations []string
	mu             sync.Mutex
	challenge      bool
}

func (b *blobMirror) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	b.authorizations = append(b.authorizations, r.Header.Get("Authorization"))
	b.mu.Unlock()

	if rest, ok := strings.CutPrefix(r.URL.Path, "/hop/"); ok {
		http.Redirect(w, r, "/final/"+rest, http.StatusTemporaryRedirect)

		return
	}

	if b.challenge {
		w.Header().Set("WWW-Authenticate", `Basic realm="mirror"`)
		writeRegistryError(w, http.StatusUnauthorized, errcode.ErrorCodeUnauthorized)

		return
	}

	dgst := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]

	b.source.mu.Lock()
	data, ok := b.source.blobs[dgst]
	b.source.mu.Unlock()

	if !ok {
		writeRegistryError(w, http.StatusNotFound, errcode.ErrorCodeBlobUnknown)

		return
	}

	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

func (b *blobMirror) seen() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return slices.Clone(b.authorizations)
}

func writeRegistryError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{{"code": code, "message": "fake registry"}},
	})
}

func writeChunked(w http.ResponseWriter, body []byte) {
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	_, _ = w.Write(body)
}

func tamper(data []byte) []byte {
	out := slices.Clone(data)
	out[len(out)-1] ^= 0x01

	return out
}

func testManifest(t *testing.T, layers ...ocispec.Descriptor) []byte {
	t.Helper()

	if len(layers) == 0 {
		layers = []ocispec.Descriptor{ocispec.DescriptorEmptyJSON}
	}

	data, err := json.Marshal(ocispec.Manifest{
		SchemaVersion: 2,
		MediaType:     ocispec.MediaTypeImageManifest,
		Config:        ocispec.DescriptorEmptyJSON,
		Layers:        layers,
	})
	if err != nil {
		t.Fatal(err)
	}

	return data
}

func hostOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	return u.Host
}

func noRetry() retry.Policy {
	return &retry.GenericPolicy{Retryable: retry.DefaultPredicate, Backoff: retry.DefaultBackoff, MaxRetry: 0}
}

func newTestClient(hosts map[string]HostConfig) *Client {
	return NewClient(Options{Hosts: hosts, RetryPolicy: noRetry(), UserAgent: "schepherd-test"})
}

func plainHosts(t *testing.T, servers ...*httptest.Server) map[string]HostConfig {
	t.Helper()

	hosts := map[string]HostConfig{}
	for _, srv := range servers {
		hosts[hostOf(t, srv)] = HostConfig{PlainHTTP: true}
	}

	return hosts
}

func mustOpen(t *testing.T, c *Client, host, path string) *Repo {
	t.Helper()

	repo, err := c.Open(Repository{Host: host, Path: path})
	if err != nil {
		t.Fatalf("Open(%s/%s): %v", host, path, err)
	}

	return repo
}

// isolateDockerConfig points ORAS's Docker config discovery at an empty
// temporary directory so no test can read the developer's credentials.
func isolateDockerConfig(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)

	return dir
}

func writeDockerConfig(t *testing.T, path string, creds map[string][2]string) {
	t.Helper()

	auths := map[string]map[string]string{}
	for host, pair := range creds {
		auths[host] = map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(pair[0] + ":" + pair[1]))}
	}

	data, err := json.Marshal(map[string]any{"auths": auths})
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeRawDockerConfig(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeCAFile(t *testing.T, srv *httptest.Server) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "ca.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func requireKind(t *testing.T, err error, want fault.Kind) {
	t.Helper()

	if err == nil {
		t.Fatalf("got no error, want a %s error", want)
	}

	if _, ok := errors.AsType[*fault.Error](err); !ok {
		t.Fatalf("error %q (%T) is not classified", err, err)
	}

	if got := fault.KindOf(err); got != want {
		t.Fatalf("kind = %s, want %s (error: %v)", got, want, err)
	}
}

func requireMessage(t *testing.T, err error, fragments ...string) {
	t.Helper()

	for _, fragment := range fragments {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error %q does not contain %q", err, fragment)
		}
	}
}
