//go:build e2e

package e2e

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/ovineko/schepherd/tests/e2e/regproxy"
)

// Wire contract of docs/oci-format.md. The suite spells these out instead of
// importing internal/artifact so it checks the documented format, not
// whatever the implementation currently produces.
const (
	mediaTypeManifest       = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeIndex          = "application/vnd.oci.image.index.v1+json"
	mediaTypeEmpty          = "application/vnd.oci.empty.v1+json"
	artifactTypeSchema      = "application/vnd.ovineko.schepherd.schema.v2"
	artifactTypeCatalog     = "application/vnd.ovineko.schepherd.catalog.v2"
	artifactTypeMetadata    = "application/vnd.ovineko.schepherd.catalog-metadata.v2"
	mediaTypeSchemaJSON     = "application/schema+json"
	mediaTypeSchemaGzip     = "application/vnd.ovineko.schepherd.schema.v2+gzip"
	mediaTypeNotice         = "application/vnd.ovineko.schepherd.notice.v2+text"
	mediaTypeCatalog        = "application/vnd.ovineko.schepherd.catalog.v2+json"
	catalogFormatVersion    = 2
	annotationContentDigest = "com.ovineko.schepherd.content.digest"
	annotationContentSize   = "com.ovineko.schepherd.content.size"
	annotationTitle         = "org.opencontainers.image.title"
	emptyConfigDigest       = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
)

// registry is one registry service of the compose project. Its direct
// address changes whenever the container restarts (see startService); read
// Host() again after a restart instead of keeping the old value.
type registry struct {
	service string
	tls     bool
	user    credential
	pki     *pki

	mu      sync.RWMutex
	host    string
	proxies []*proxy
}

// Host is the registry's direct address, 127.0.0.1:<port>. Requests to it
// bypass every proxy.
func (r *registry) Host() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.host
}

// Repo returns the repository reference host/path on the direct address.
func (r *registry) Repo(path string) string {
	return r.Host() + "/" + path
}

// TLS reports whether the registry serves HTTPS with the run's test CA and
// requires the credential User().
func (r *registry) TLS() bool {
	return r.tls
}

// User is the htpasswd credential of an auth registry (zero for plain ones).
func (r *registry) User() credential {
	return r.user
}

// fetchedManifest is a manifest or index read directly from a registry.
// Manifest and Index hold the body decoded both ways; only the members of
// the matching type are set.
type fetchedManifest struct {
	Body        []byte
	Digest      string
	ContentType string
	Manifest    ocispec.Manifest
	Index       ocispec.Index
}

// Manifest fetches a manifest or index by tag or digest directly from the
// registry, verifies its digest (against ref when ref is a digest, and
// against the Docker-Content-Digest header) and decodes it.
func (r *registry) Manifest(t *testing.T, repo, ref string) fetchedManifest {
	t.Helper()

	resp := r.request(t, http.MethodGet, "/v2/"+repo+"/manifests/"+ref,
		http.Header{"Accept": {mediaTypeManifest, mediaTypeIndex}})
	if resp.Status != http.StatusOK {
		t.Fatalf("GET manifest %s@%s from %s: status %d: %s", repo, ref, r.service, resp.Status, resp.Body)
	}

	got := digestOf(resp.Body)
	if strings.HasPrefix(ref, "sha256:") && got != ref {
		t.Fatalf("manifest %s@%s has digest %s", repo, ref, got)
	}

	if header := resp.Header.Get("Docker-Content-Digest"); header != "" && header != got {
		t.Fatalf("manifest %s@%s: Docker-Content-Digest %s, body digest %s", repo, ref, header, got)
	}

	m := fetchedManifest{Body: resp.Body, Digest: got, ContentType: resp.Header.Get("Content-Type")}
	if err := json.Unmarshal(resp.Body, &m.Manifest); err != nil {
		t.Fatalf("decode manifest %s@%s: %v\n%s", repo, ref, err, resp.Body)
	}

	if err := json.Unmarshal(resp.Body, &m.Index); err != nil {
		t.Fatalf("decode index %s@%s: %v\n%s", repo, ref, err, resp.Body)
	}

	return m
}

// fetchedCatalog is a catalog read directly from a registry: the index,
// its metadata manifest, the catalog.json bytes and their decoded document.
type fetchedCatalog struct {
	Index    fetchedManifest
	Metadata fetchedManifest
	Blob     []byte
	Doc      catalogDoc
}

// schemaChildren returns the schema manifest descriptors of the index: all
// children except the metadata manifest.
func (c fetchedCatalog) schemaChildren() []ocispec.Descriptor {
	return c.Index.Index.Manifests[1:]
}

// Catalog reads the catalog index ref (a tag or digest) and everything it
// needs from it directly from the registry and checks the envelope of
// docs/oci-format.md: an image index typed as a catalog whose first child
// is the metadata manifest with exactly one catalog.json layer.
func (r *registry) Catalog(t *testing.T, repo, ref string) fetchedCatalog {
	t.Helper()

	c := fetchedCatalog{Index: r.Manifest(t, repo, ref)}
	ix := c.Index.Index

	switch {
	case c.Index.ContentType != mediaTypeIndex || ix.MediaType != mediaTypeIndex || ix.SchemaVersion != 2:
		t.Fatalf("catalog %s@%s is not an image index (Content-Type %q):\n%s", repo, ref, c.Index.ContentType, c.Index.Body)
	case ix.ArtifactType != artifactTypeCatalog:
		t.Fatalf("catalog index %s@%s has artifactType %q, want %q", repo, ref, ix.ArtifactType, artifactTypeCatalog)
	case len(ix.Manifests) == 0 || ix.Manifests[0].ArtifactType != artifactTypeMetadata || ix.Manifests[0].MediaType != mediaTypeManifest:
		t.Fatalf("catalog index %s@%s does not start with its metadata manifest:\n%s", repo, ref, c.Index.Body)
	}

	c.Metadata = r.Manifest(t, repo, ix.Manifests[0].Digest.String())
	if m := c.Metadata.Manifest; m.ArtifactType != artifactTypeMetadata || len(m.Layers) != 1 || m.Layers[0].MediaType != mediaTypeCatalog {
		t.Fatalf("catalog metadata manifest %s:\n%s", c.Metadata.Digest, c.Metadata.Body)
	}

	c.Blob = r.Blob(t, repo, c.Metadata.Manifest.Layers[0].Digest.String())
	c.Doc = decodeJSON[catalogDoc](t, c.Blob)

	return c
}

// ManifestStatus returns the status of a HEAD request for a manifest, for
// example 200 or 404.
func (r *registry) ManifestStatus(t *testing.T, repo, ref string) int {
	t.Helper()

	return r.request(t, http.MethodHead, "/v2/"+repo+"/manifests/"+ref,
		http.Header{"Accept": {mediaTypeManifest, mediaTypeIndex}}).Status
}

// Blob fetches a blob directly from the registry and verifies its digest.
func (r *registry) Blob(t *testing.T, repo, dgst string) []byte {
	t.Helper()

	resp := r.request(t, http.MethodGet, "/v2/"+repo+"/blobs/"+dgst, nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("GET blob %s@%s from %s: status %d: %s", repo, dgst, r.service, resp.Status, resp.Body)
	}

	if got := digestOf(resp.Body); got != dgst {
		t.Fatalf("blob %s@%s has digest %s", repo, dgst, got)
	}

	return resp.Body
}

// BlobStatus returns the status of a HEAD request for a blob.
func (r *registry) BlobStatus(t *testing.T, repo, dgst string) int {
	t.Helper()

	return r.request(t, http.MethodHead, "/v2/"+repo+"/blobs/"+dgst, nil).Status
}

// Tags lists every tag of a repository, sorted; nil when the repository does
// not exist.
func (r *registry) Tags(t *testing.T, repo string) []string {
	t.Helper()

	var tags []string

	path := "/v2/" + repo + "/tags/list?n=1000"

	for path != "" {
		resp := r.request(t, http.MethodGet, path, nil)
		if resp.Status == http.StatusNotFound {
			return nil
		}

		if resp.Status != http.StatusOK {
			t.Fatalf("list tags of %s: status %d: %s", repo, resp.Status, resp.Body)
		}

		var page struct {
			Tags []string `json:"tags"`
		}

		if err := json.Unmarshal(resp.Body, &page); err != nil {
			t.Fatalf("decode tag list of %s: %v", repo, err)
		}

		tags = append(tags, page.Tags...)
		path = nextPage(resp.Header.Get("Link"))
	}

	slices.Sort(tags)

	return tags
}

func (r *registry) setHost(host string) {
	r.mu.Lock()
	moved := r.host != "" && r.host != host
	r.host = host
	proxies := slices.Clone(r.proxies)
	r.mu.Unlock()

	if moved {
		for _, p := range proxies {
			p.transport.CloseIdleConnections()
		}
	}
}

func (r *registry) baseURL() string {
	if r.tls {
		return "https://" + r.Host()
	}

	return "http://" + r.Host()
}

func (r *registry) httpClient() *http.Client {
	tr := &http.Transport{Proxy: nil, DisableCompression: true}
	if r.tls {
		tr.TLSClientConfig = r.pki.tlsConfig()
	}

	return &http.Client{Transport: tr, Timeout: time.Minute}
}

// waitReady polls /v2/ until it answers 200 (plain) or 401 (auth).
func (r *registry) waitReady(ctx context.Context, timeout time.Duration) error {
	want := http.StatusOK
	if r.tls {
		want = http.StatusUnauthorized
	}

	client := r.httpClient()
	defer client.CloseIdleConnections()

	if err := poll(ctx, timeout, func() error { return expectStatus(ctx, client, r.baseURL()+"/v2/", want) }); err != nil {
		return fmt.Errorf("%s is not ready: %w", r.service, err)
	}

	return nil
}

// httpResponse is a response of a direct registry request.
type httpResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

// request sends one request directly to the registry (with the auth
// registries' credential) and returns the response; path starts with /v2/.
func (r *registry) request(t *testing.T, method, path string, header http.Header) httpResponse {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, r.baseURL()+path, nil)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, path, err)
	}

	maps.Copy(req.Header, header)

	if r.tls {
		req.SetBasicAuth(r.user.Username, r.user.Password)
	}

	client := r.httpClient()
	defer client.CloseIdleConnections()

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s%s: %v", method, r.Host(), path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s%s: read body: %v", method, r.Host(), path, err)
	}

	return httpResponse{Status: resp.StatusCode, Header: resp.Header, Body: body}
}

// nextPage extracts the path of a distribution tag list Link header,
// `</v2/...>; rel="next"`.
func nextPage(link string) string {
	start, end := strings.IndexByte(link, '<'), strings.IndexByte(link, '>')
	if start < 0 || end <= start {
		return ""
	}

	u, err := url.Parse(link[start+1 : end])
	if err != nil {
		return ""
	}

	return u.RequestURI()
}

// writeDockerConfig writes dir/config.json in the Docker format with a
// static credential per host and returns its path. Use it as
// credentials_file or as the DOCKER_CONFIG directory.
func writeDockerConfig(t *testing.T, dir string, auths map[string]credential) string {
	t.Helper()

	entries := map[string]map[string]string{}
	for host, c := range auths {
		entries[host] = map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.Password))}
	}

	return writeJSONFile(t, dir, "config.json", map[string]any{"auths": entries})
}

// writeCredsStoreConfig writes dir/config.json that delegates every host to
// the credential helper docker-credential-<store> (docker-credential-e2e is
// on the sandbox PATH) and returns its path.
func writeCredsStoreConfig(t *testing.T, dir, store string) string {
	t.Helper()

	return writeJSONFile(t, dir, "config.json", map[string]any{"credsStore": store})
}

func writeJSONFile(t *testing.T, dir, name string, v any) string {
	t.Helper()

	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("encode %s: %v", name, err)
	}

	path := filepath.Join(dir, name)
	writeFile(t, path, append(data, '\n'))

	return path
}

// proxy is a counting, fault-injecting regproxy in front of one registry.
// Its address stays the same when the registry restarts on another port.
type proxy struct {
	p         *regproxy.Proxy
	host      string
	upstream  *registry
	transport *http.Transport
}

// startProxy starts a proxy whose upstream connections always dial the
// registry's current address; the URL host it forwards to is a placeholder.
// For TLS registries the proxy speaks HTTPS upstream and plain HTTP to its
// clients, which then need plain_http for the proxy's host.
func startProxy(r *registry) (*proxy, error) {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, r.Host())
		},
		MaxIdleConns:       100,
		IdleConnTimeout:    90 * time.Second,
		DisableCompression: true,
	}

	target := &url.URL{Scheme: "http", Host: r.service}
	if r.tls {
		target.Scheme = "https"
		tr.TLSClientConfig = r.pki.tlsConfig()
	}

	p := regproxy.New(target, regproxy.WithTransport(tr))

	base, err := p.Start()
	if err != nil {
		return nil, fmt.Errorf("start proxy for %s: %w", r.service, err)
	}

	px := &proxy{p: p, host: strings.TrimPrefix(base, "http://"), upstream: r, transport: tr}

	r.mu.Lock()
	r.proxies = append(r.proxies, px)
	r.mu.Unlock()

	return px, nil
}

// newProxy starts a private proxy for one test (closed when the test ends).
// Unlike the shared suite.sourceProxy and suite.mirrorProxy, its records and
// faults belong to this test alone, so it also works with t.Parallel.
func newProxy(t *testing.T, r *registry) *proxy {
	t.Helper()

	px, err := startProxy(r)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		px.p.Close()

		r.mu.Lock()
		r.proxies = slices.DeleteFunc(r.proxies, func(x *proxy) bool { return x == px })
		r.mu.Unlock()
	})

	return px
}

// Host is the proxy's address, 127.0.0.1:<port> (plain HTTP).
func (p *proxy) Host() string {
	return p.host
}

// Repo returns the repository reference host/path through the proxy.
func (p *proxy) Repo(path string) string {
	return p.host + "/" + path
}

// Registry is the registry behind the proxy.
func (p *proxy) Registry() *registry {
	return p.upstream
}

// Stats aggregates the records since the last Reset.
func (p *proxy) Stats(t *testing.T) regproxy.Stats {
	t.Helper()
	p.settle(t)

	return p.p.Stats()
}

// Records returns the records since the last Reset in arrival order.
func (p *proxy) Records(t *testing.T) []regproxy.Record {
	t.Helper()
	p.settle(t)

	return p.p.Records()
}

// Reset forgets all records. On a shared proxy this also drops what other
// tests recorded, which is harmless because tests using the shared proxies
// do not run in parallel.
func (p *proxy) Reset(t *testing.T) {
	t.Helper()
	p.settle(t)
	p.p.Reset()
}

// AddFault installs a fault until ClearFaults or the end of the test.
func (p *proxy) AddFault(t *testing.T, f regproxy.Fault) {
	t.Helper()
	p.p.AddFault(f)
	t.Cleanup(p.p.ClearFaults)
}

// ClearFaults removes every fault.
func (p *proxy) ClearFaults() {
	p.p.ClearFaults()
}

// SetDown makes the proxy answer 502 to everything while down is true; it
// is reset to false when the test ends.
func (p *proxy) SetDown(t *testing.T, down bool) {
	t.Helper()
	p.p.SetDown(down)
	t.Cleanup(func() { p.p.SetDown(false) })
}

// settle waits until the proxy handles no request, so records and stats
// include every exchange of a command that already exited.
func (p *proxy) settle(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := p.p.WaitIdle(ctx); err != nil {
		t.Fatalf("proxy %s: %v", p.host, err)
	}
}

// orasRepository returns an ORAS client for a repository on the registry's
// direct address, authenticated for the auth registries.
func (r *registry) orasRepository(t *testing.T, repo string) *remote.Repository {
	t.Helper()

	target, err := remote.NewRepository(r.Repo(repo))
	if err != nil {
		t.Fatalf("repository %s: %v", repo, err)
	}

	target.PlainHTTP = !r.tls
	client := &auth.Client{Client: r.httpClient(), Cache: auth.NewCache()}

	if r.tls {
		client.Credential = auth.StaticCredential(r.Host(), auth.Credential{Username: r.user.Username, Password: r.user.Password})
	}

	target.Client = client

	return target
}

// rawLayer is one layer of a hand-crafted artifact.
type rawLayer struct {
	MediaType   string
	Data        []byte
	Annotations map[string]string
	// Size overrides the declared size (the pushed blob keeps len(Data)).
	Size int64
}

// rawArtifact describes a hand-crafted, possibly hostile, artifact for
// pushRaw. Zero values give the envelope of a schema artifact: the schema
// artifact type and the OCI empty config.
type rawArtifact struct {
	ArtifactType string
	MediaType    string
	Config       *ocispec.Descriptor
	Layers       []rawLayer
	Annotations  map[string]string
	// Edit changes the decoded manifest object before it is serialized, for
	// shapes the fields cannot express (unknown members, urls, subject...).
	Edit func(manifest map[string]any)
}

// schemaLayer returns the payload layer of a schema artifact with the
// content annotations computed from content (the uncompressed schema);
// payload is what is stored, for example gzipBytes(content).
func schemaLayer(mediaType string, payload, content []byte) rawLayer {
	return rawLayer{
		MediaType: mediaType,
		Data:      payload,
		Annotations: map[string]string{
			annotationContentDigest: digestOf(content),
			annotationContentSize:   strconv.Itoa(len(content)),
			annotationTitle:         payloadTitle(mediaType),
		},
	}
}

// payloadTitle is the title annotation the schepherd-pack/1 recipe gives a
// payload layer. Titles are informational for clients (docs/oci-format.md),
// but they are part of the manifest bytes and therefore of every digest.
func payloadTitle(mediaType string) string {
	if mediaType == mediaTypeSchemaGzip {
		return "schema.json.gz"
	}

	return "schema.json"
}

// pushRaw pushes the blobs and the manifest of a hand-crafted artifact
// directly to the registry with ORAS, bypassing every check of the
// publisher, tags it with tags and returns the manifest descriptor.
func pushRaw(t *testing.T, r *registry, repo string, a rawArtifact, tags ...string) ocispec.Descriptor {
	t.Helper()

	emptyConfig := ocispec.Descriptor{MediaType: mediaTypeEmpty, Digest: emptyConfigDigest, Size: 2, Data: []byte("{}")}
	pushRawBlob(t, r, repo, mediaTypeEmpty, []byte("{}"))

	m := ocispec.Manifest{
		MediaType:    a.MediaType,
		ArtifactType: a.ArtifactType,
		Annotations:  a.Annotations,

		SchemaVersion: 2,
	}

	if m.MediaType == "" {
		m.MediaType = mediaTypeManifest
	}

	if m.ArtifactType == "" {
		m.ArtifactType = artifactTypeSchema
	}

	m.Config = emptyConfig
	if a.Config != nil {
		m.Config = *a.Config
	}

	for _, l := range a.Layers {
		desc := pushRawBlob(t, r, repo, l.MediaType, l.Data)
		desc.Annotations = l.Annotations

		if l.Size != 0 {
			desc.Size = l.Size
		}

		m.Layers = append(m.Layers, desc)
	}

	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}

	if a.Edit != nil {
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatalf("decode manifest: %v", err)
		}

		a.Edit(obj)

		if body, err = json.Marshal(obj); err != nil {
			t.Fatalf("encode edited manifest: %v", err)
		}
	}

	return pushRawManifest(t, r, repo, body, tags...)
}

// pushRawBlob uploads one blob (skipped when it already exists) and returns
// its descriptor.
func pushRawBlob(t *testing.T, r *registry, repo, mediaType string, data []byte) ocispec.Descriptor {
	t.Helper()

	desc := ocispec.Descriptor{MediaType: mediaType, Digest: digestFor(data), Size: int64(len(data))}
	target := r.orasRepository(t, repo)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	exists, err := target.Blobs().Exists(ctx, desc)
	if err != nil {
		t.Fatalf("check blob %s: %v", desc.Digest, err)
	}

	if !exists {
		if err := target.Blobs().Push(ctx, desc, bytes.NewReader(data)); err != nil {
			t.Fatalf("push blob %s: %v", desc.Digest, err)
		}
	}

	return desc
}

// pushRawManifest uploads manifest bytes as they are (the media type is taken
// from their mediaType member, default OCI image manifest), tags them and
// returns the descriptor.
func pushRawManifest(t *testing.T, r *registry, repo string, manifest []byte, tags ...string) ocispec.Descriptor {
	t.Helper()

	var head struct {
		MediaType string `json:"mediaType"`
	}

	_ = json.Unmarshal(manifest, &head)
	if head.MediaType == "" {
		head.MediaType = mediaTypeManifest
	}

	desc := ocispec.Descriptor{MediaType: head.MediaType, Digest: digestFor(manifest), Size: int64(len(manifest))}
	target := r.orasRepository(t, repo)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := target.Manifests().Push(ctx, desc, bytes.NewReader(manifest)); err != nil {
		t.Fatalf("push manifest %s: %v", desc.Digest, err)
	}

	for _, tag := range tags {
		if err := target.Tag(ctx, desc, tag); err != nil {
			t.Fatalf("tag %s: %v", tag, err)
		}
	}

	return desc
}

// pushRawCatalog pushes a catalog whose catalog.json is doc (any bytes,
// valid or not): a metadata manifest with doc as its only layer and an index
// over it and the schema manifests doc lists, as far as they can be read. It
// returns the index descriptor, whose digest is what clients use as
// --catalog.
func pushRawCatalog(t *testing.T, r *registry, repo string, doc []byte, tags ...string) ocispec.Descriptor {
	t.Helper()

	var listed catalogDoc

	_ = json.Unmarshal(doc, &listed)

	schemas := []ocispec.Descriptor{}
	seen := map[string]bool{}

	for _, e := range listed.Schemas {
		if seen[e.Artifact.Digest] || !digestPattern.MatchString(e.Artifact.Digest) {
			continue
		}

		seen[e.Artifact.Digest] = true
		schemas = append(schemas, ocispec.Descriptor{MediaType: e.Artifact.MediaType, Digest: digest.Digest(e.Artifact.Digest), Size: e.Artifact.Size})
	}

	return pushRawIndex(t, r, repo, pushRawMetadata(t, r, repo, doc), schemas, nil, tags...)
}

// pushRawMetadata pushes a catalog metadata manifest whose only layer is doc
// and returns its descriptor as an index child.
func pushRawMetadata(t *testing.T, r *registry, repo string, doc []byte) ocispec.Descriptor {
	t.Helper()

	desc := pushRaw(t, r, repo, rawArtifact{
		ArtifactType: artifactTypeMetadata,
		Layers:       []rawLayer{{MediaType: mediaTypeCatalog, Data: doc, Annotations: map[string]string{annotationTitle: "catalog.json"}}},
	})
	desc.ArtifactType = artifactTypeMetadata

	return desc
}

// pushRawIndex pushes a catalog index over the metadata manifest and the
// schema manifests (sorted by digest, as the publisher writes them), after
// edit changed the decoded index, and returns its descriptor.
func pushRawIndex(t *testing.T, r *registry, repo string, metadata ocispec.Descriptor, schemas []ocispec.Descriptor,
	edit func(index map[string]any), tags ...string,
) ocispec.Descriptor {
	t.Helper()

	children := make([]ocispec.Descriptor, 0, len(schemas)+1)
	children = append(children, metadata)

	for _, d := range slices.SortedFunc(slices.Values(schemas), func(a, b ocispec.Descriptor) int { return strings.Compare(string(a.Digest), string(b.Digest)) }) {
		children = append(children, ocispec.Descriptor{MediaType: d.MediaType, Digest: d.Digest, Size: d.Size})
	}

	body, err := json.Marshal(ocispec.Index{SchemaVersion: 2, MediaType: mediaTypeIndex, ArtifactType: artifactTypeCatalog, Manifests: children})
	if err != nil {
		t.Fatalf("encode index: %v", err)
	}

	if edit != nil {
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatalf("decode index: %v", err)
		}

		edit(obj)

		if body, err = json.Marshal(obj); err != nil {
			t.Fatalf("encode edited index: %v", err)
		}
	}

	return pushRawManifest(t, r, repo, body, tags...)
}

// catalogDoc is the catalog document of docs/oci-format.md, for decoding
// what the publisher produced and for encoding hand-crafted catalogs.
type catalogDoc struct {
	FormatVersion int            `json:"formatVersion"`
	Revision      string         `json:"revision"`
	Schemas       []catalogEntry `json:"schemas"`
}

type catalogEntry struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Dialect     string          `json:"dialect,omitempty"`
	FileMatch   []string        `json:"fileMatch,omitempty"`
	Artifact    descriptorDoc   `json:"artifact"`
	Provenance  json.RawMessage `json:"provenance,omitempty"`
}

type descriptorDoc struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// entry returns the catalog entry with the given ID or fails the test.
func (c catalogDoc) entry(t *testing.T, id string) catalogEntry {
	t.Helper()

	for _, e := range c.Schemas {
		if e.ID == id {
			return e
		}
	}

	t.Fatalf("catalog has no schema %q", id)

	return catalogEntry{}
}

// descriptorOf converts an OCI descriptor into a catalog artifact reference.
func descriptorOf(d ocispec.Descriptor) descriptorDoc {
	return descriptorDoc{MediaType: d.MediaType, Digest: d.Digest.String(), Size: d.Size}
}

// gzipBytes compresses data as a raw gzip stream (not a tar archive), the
// shape of a gzip schema payload.
func gzipBytes(data []byte) []byte {
	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(data)
	_ = zw.Close()

	return buf.Bytes()
}
