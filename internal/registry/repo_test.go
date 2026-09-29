package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

const testRepoPath = "org/schemas"

func TestFetchManifestByDigest(t *testing.T) {
	isolateDockerConfig(t)

	manifest := testManifest(t)
	big := testManifest(t, ocispec.Descriptor{
		MediaType: "application/octet-stream",
		Digest:    godigest.Digest(testDigest),
		Size:      2,
		Annotations: map[string]string{
			"padding": strings.Repeat("x", 4096),
		},
	})
	sha512 := "sha512:" + strings.Repeat("a", 128)

	cases := []struct {
		setup     func(f *fakeRegistry) string
		name      string
		mediaType string
		wantBody  []byte
		fragments []string
		limit     int64
		wantKind  fault.Kind
	}{
		{
			name: "verified",
			setup: func(f *fakeRegistry) string {
				return f.addManifest(ocispec.MediaTypeImageManifest, manifest).Digest.String()
			},
			wantBody: manifest,
		},
		{
			name: "verified index",
			setup: func(f *fakeRegistry) string {
				return f.addManifest(ocispec.MediaTypeImageIndex, manifest).Digest.String()
			},
			mediaType: ocispec.MediaTypeImageIndex,
			wantBody:  manifest,
		},
		{
			name: "an index where a manifest is expected",
			setup: func(f *fakeRegistry) string {
				return f.addManifest(ocispec.MediaTypeImageIndex, manifest).Digest.String()
			},
			wantKind:  fault.Integrity,
			fragments: []string{`media type is "application/vnd.oci.image.index.v1+json", want "application/vnd.oci.image.manifest.v1+json"`},
		},
		{
			name: "a manifest where an index is expected",
			setup: func(f *fakeRegistry) string {
				return f.addManifest(ocispec.MediaTypeImageManifest, manifest).Digest.String()
			},
			mediaType: ocispec.MediaTypeImageIndex,
			wantKind:  fault.Integrity,
			fragments: []string{`want "application/vnd.oci.image.index.v1+json"`},
		},
		{
			name: "verified without Docker-Content-Digest",
			setup: func(f *fakeRegistry) string {
				f.omitDigestHeader = true

				return f.addManifest(ocispec.MediaTypeImageManifest, manifest).Digest.String()
			},
			wantBody: manifest,
		},
		{
			name: "tampered body with a correct Docker-Content-Digest header",
			setup: func(f *fakeRegistry) string {
				f.tamperManifests = true

				return f.addManifest(ocispec.MediaTypeImageManifest, manifest).Digest.String()
			},
			wantKind:  fault.Integrity,
			fragments: []string{"received content has digest " + digest.FromBytes(tamper(manifest))},
		},
		{
			name: "tampered body without Docker-Content-Digest",
			setup: func(f *fakeRegistry) string {
				f.tamperManifests = true
				f.omitDigestHeader = true

				return f.addManifest(ocispec.MediaTypeImageManifest, manifest).Digest.String()
			},
			wantKind:  fault.Integrity,
			fragments: []string{"does not match the requested descriptor"},
		},
		{
			name: "declared size above the limit",
			setup: func(f *fakeRegistry) string {
				return f.addManifest(ocispec.MediaTypeImageManifest, big).Digest.String()
			},
			limit:     1024,
			wantKind:  fault.Integrity,
			fragments: []string{"exceeds the limit of 1024 bytes"},
		},
		{
			name: "streamed body above the limit behind an understated size",
			setup: func(f *fakeRegistry) string {
				f.chunkedManifests = true
				f.headSize = 100

				return f.addManifest(ocispec.MediaTypeImageManifest, big).Digest.String()
			},
			limit:     1024,
			wantKind:  fault.Integrity,
			fragments: []string{"manifest exceeds the limit of 1024 bytes"},
		},
		{
			name: "default limit from options",
			setup: func(f *fakeRegistry) string {
				return f.addManifest(ocispec.MediaTypeImageManifest, big).Digest.String()
			},
			wantKind:  fault.Integrity,
			fragments: []string{"exceeds the limit of 2048 bytes"},
		},
		{
			name: "not an OCI image manifest",
			setup: func(f *fakeRegistry) string {
				return f.addManifest("application/vnd.docker.distribution.manifest.v2+json", manifest).Digest.String()
			},
			wantKind:  fault.Integrity,
			fragments: []string{"media type is \"application/vnd.docker.distribution.manifest.v2+json\""},
		},
		{
			name:      "missing",
			setup:     func(*fakeRegistry) string { return testDigest },
			wantKind:  fault.Registry,
			fragments: []string{"not found (404)"},
		},
		{
			name:      "unsupported digest algorithm",
			setup:     func(*fakeRegistry) string { return sha512 },
			wantKind:  fault.Usage,
			fragments: []string{"unsupported digest algorithm"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeRegistry(testRepoPath)
			dgst := tc.setup(fake)
			srv := fake.start(t)
			host := hostOf(t, srv)

			client := NewClient(Options{Hosts: plainHosts(t, srv), RetryPolicy: noRetry(), MaxManifestBytes: 2048})
			repo := mustOpen(t, client, host, testRepoPath)

			mediaType := tc.mediaType
			if mediaType == "" {
				mediaType = ocispec.MediaTypeImageManifest
			}

			body, err := repo.FetchManifestByDigest(context.Background(), dgst, mediaType, tc.limit)
			if tc.wantBody != nil {
				if err != nil {
					t.Fatalf("FetchManifestByDigest: %v", err)
				}

				if !bytes.Equal(body, tc.wantBody) {
					t.Fatalf("body = %q, want %q", body, tc.wantBody)
				}

				return
			}

			if body != nil {
				t.Errorf("body = %q, want nil on error", body)
			}

			requireKind(t, err, tc.wantKind)
			requireMessage(t, err, append(tc.fragments, "fetch manifest "+dgst+" from "+host+"/"+testRepoPath)...)

			if tc.name == "missing" && !IsNotFound(err) {
				t.Errorf("IsNotFound(%v) = false", err)
			}
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("disk full")
}

func TestFetchTo(t *testing.T) {
	isolateDockerConfig(t)

	payload := bytes.Repeat([]byte("schema payload "), 1<<16)

	cases := []struct {
		setup     func(f *fakeRegistry, desc *ocispec.Descriptor)
		name      string
		fragments []string
		wantKind  fault.Kind
		wantOK    bool
	}{
		{name: "verified", wantOK: true},
		{
			name:      "tampered",
			setup:     func(f *fakeRegistry, _ *ocispec.Descriptor) { f.tamperBlobs = true },
			wantKind:  fault.Integrity,
			fragments: []string{"content does not match its descriptor"},
		},
		{
			name:      "truncated",
			setup:     func(f *fakeRegistry, _ *ocispec.Descriptor) { f.truncateBlobs = true },
			wantKind:  fault.Registry,
			fragments: []string{"the connection ended before the content was complete"},
		},
		{
			name:      "trailing data",
			setup:     func(f *fakeRegistry, _ *ocispec.Descriptor) { f.trailingBlobByte = true },
			wantKind:  fault.Integrity,
			fragments: []string{"content does not match its descriptor"},
		},
		{
			name:      "size differs from the descriptor",
			setup:     func(_ *fakeRegistry, desc *ocispec.Descriptor) { desc.Size++ },
			wantKind:  fault.Integrity,
			fragments: []string{"does not match the requested descriptor"},
		},
		{
			name: "missing",
			setup: func(f *fakeRegistry, _ *ocispec.Descriptor) {
				f.mu.Lock()
				clear(f.blobs)
				f.mu.Unlock()
			},
			wantKind:  fault.Registry,
			fragments: []string{"not found (404)"},
		},
		{
			name:      "invalid descriptor",
			setup:     func(_ *fakeRegistry, desc *ocispec.Descriptor) { desc.Digest = "sha256:abc" },
			wantKind:  fault.Integrity,
			fragments: []string{"invalid digest"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeRegistry(testRepoPath)
			desc := fake.addBlob(payload)
			desc.MediaType = "application/vnd.example.schema.v1+json"

			if tc.setup != nil {
				tc.setup(fake, &desc)
			}

			srv := fake.start(t)
			repo := mustOpen(t, newTestClient(plainHosts(t, srv)), hostOf(t, srv), testRepoPath)

			var buf bytes.Buffer

			err := repo.FetchTo(context.Background(), desc, &buf)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("FetchTo: %v", err)
				}

				if !bytes.Equal(buf.Bytes(), payload) {
					t.Fatalf("FetchTo wrote %d bytes, want the %d-byte payload", buf.Len(), len(payload))
				}

				return
			}

			requireKind(t, err, tc.wantKind)
			requireMessage(t, err, tc.fragments...)
			requireMessage(t, err, "fetch "+desc.Digest.String()+" from "+hostOf(t, srv))
		})
	}

	t.Run("writer failure is local", func(t *testing.T) {
		fake := newFakeRegistry(testRepoPath)
		desc := fake.addBlob(payload)
		srv := fake.start(t)
		repo := mustOpen(t, newTestClient(plainHosts(t, srv)), hostOf(t, srv), testRepoPath)

		err := repo.FetchTo(context.Background(), desc, failingWriter{})
		requireKind(t, err, fault.Internal)
		requireMessage(t, err, "disk full")
	})

	t.Run("canceled", func(t *testing.T) {
		fake := newFakeRegistry(testRepoPath)
		desc := fake.addBlob(payload)
		srv := fake.start(t)
		repo := mustOpen(t, newTestClient(plainHosts(t, srv)), hostOf(t, srv), testRepoPath)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := repo.FetchTo(ctx, desc, &bytes.Buffer{})
		requireKind(t, err, fault.Canceled)
	})
}

func TestPushIsIdempotent(t *testing.T) {
	isolateDockerConfig(t)

	fake := newFakeRegistry(testRepoPath)
	srv := fake.start(t)
	repo := mustOpen(t, newTestClient(plainHosts(t, srv)), hostOf(t, srv), testRepoPath)
	ctx := context.Background()

	blob := []byte(`{"type":"object"}`)
	blobDesc := ocispec.Descriptor{
		MediaType: "application/vnd.example.schema.v1+json",
		Digest:    godigest.Digest(digest.FromBytes(blob)),
		Size:      int64(len(blob)),
	}

	for i, want := range []bool{true, false} {
		pushed, err := repo.PushBlob(ctx, blobDesc, blob)
		if err != nil {
			t.Fatalf("PushBlob #%d: %v", i+1, err)
		}

		if pushed != want {
			t.Errorf("PushBlob #%d pushed = %v, want %v", i+1, pushed, want)
		}
	}

	if got := fake.count(http.MethodPost, "/v2/"+testRepoPath+"/blobs/uploads/"); got != 1 {
		t.Errorf("blob uploads started = %d, want 1", got)
	}

	manifest := testManifest(t, blobDesc)
	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    godigest.Digest(digest.FromBytes(manifest)),
		Size:      int64(len(manifest)),
	}

	for i, want := range []bool{true, false} {
		pushed, err := repo.PushManifest(ctx, manifestDesc, manifest)
		if err != nil {
			t.Fatalf("PushManifest #%d: %v", i+1, err)
		}

		if pushed != want {
			t.Errorf("PushManifest #%d pushed = %v, want %v", i+1, pushed, want)
		}
	}

	if got := fake.count(http.MethodPut, "/v2/"+testRepoPath+"/manifests/"); got != 1 {
		t.Errorf("manifest PUTs = %d, want 1", got)
	}

	got, err := repo.FetchManifestByDigest(ctx, manifestDesc.Digest.String(), ocispec.MediaTypeImageManifest, 0)
	if err != nil || !bytes.Equal(got, manifest) {
		t.Fatalf("pushed manifest reads back as %q, %v", got, err)
	}

	mismatched := blobDesc
	mismatched.Size++

	_, err = repo.PushBlob(ctx, mismatched, blob)
	requireKind(t, err, fault.Integrity)
	requireMessage(t, err, "data does not match the descriptor")

	_, err = repo.PushManifest(ctx, blobDesc, manifest)
	requireKind(t, err, fault.Integrity)
}

func TestEnsureTag(t *testing.T) {
	isolateDockerConfig(t)

	fake := newFakeRegistry(testRepoPath)
	first := fake.addManifest(ocispec.MediaTypeImageManifest, testManifest(t))
	second := fake.addManifest(ocispec.MediaTypeImageManifest, testManifest(t, ocispec.DescriptorEmptyJSON, ocispec.DescriptorEmptyJSON))
	srv := fake.start(t)
	repo := mustOpen(t, newTestClient(plainHosts(t, srv)), hostOf(t, srv), testRepoPath)
	ctx := context.Background()

	created, err := repo.EnsureTag(ctx, first, "catalog-20260101.1")
	if err != nil || !created {
		t.Fatalf("EnsureTag on a new tag = %v, %v; want true, nil", created, err)
	}

	if got := fake.tagged("catalog-20260101.1"); got != first.Digest.String() {
		t.Fatalf("tag points to %q, want %s", got, first.Digest)
	}

	created, err = repo.EnsureTag(ctx, first, "catalog-20260101.1")
	if err != nil || created {
		t.Fatalf("EnsureTag with the same digest = %v, %v; want false, nil", created, err)
	}

	created, err = repo.EnsureTag(ctx, second, "catalog-20260101.1")
	if created {
		t.Error("EnsureTag reported a moved tag as created")
	}

	requireKind(t, err, fault.Integrity)
	requireMessage(t, err, "tag catalog-20260101.1 already points to "+first.Digest.String()+"; refusing to move it to "+second.Digest.String())

	if got := fake.tagged("catalog-20260101.1"); got != first.Digest.String() {
		t.Fatalf("tag moved to %q", got)
	}

	_, err = repo.EnsureTag(ctx, first, "bad tag")
	requireKind(t, err, fault.Usage)
}

func TestResolveAndTag(t *testing.T) {
	isolateDockerConfig(t)

	fake := newFakeRegistry(testRepoPath)
	desc := fake.addManifest(ocispec.MediaTypeImageManifest, testManifest(t), "v1")
	srv := fake.start(t)
	repo := mustOpen(t, newTestClient(plainHosts(t, srv)), hostOf(t, srv), testRepoPath)
	ctx := context.Background()

	got, err := repo.Resolve(ctx, "v1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if got.Digest != desc.Digest || got.Size != desc.Size || got.MediaType != desc.MediaType {
		t.Errorf("Resolve = %+v, want %+v", got, desc)
	}

	_, err = repo.Resolve(ctx, "v2")
	requireKind(t, err, fault.Registry)
	requireMessage(t, err, `resolve tag "v2" in `+hostOf(t, srv)+"/"+testRepoPath, "not found (404)")

	if !IsNotFound(err) {
		t.Errorf("IsNotFound(%v) = false", err)
	}

	_, err = repo.Resolve(ctx, testDigest)
	requireKind(t, err, fault.Usage)

	if err := repo.Tag(ctx, desc, "v2"); err != nil {
		t.Fatalf("Tag: %v", err)
	}

	if got := fake.tagged("v2"); got != desc.Digest.String() {
		t.Errorf("v2 points to %q, want %s", got, desc.Digest)
	}

	requireKind(t, repo.Tag(ctx, desc, "-bad"), fault.Usage)

	exists, err := repo.Exists(ctx, desc)
	if err != nil || !exists {
		t.Errorf("Exists(present) = %v, %v", exists, err)
	}

	absent := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: godigest.Digest(testDigest), Size: 2}

	exists, err = repo.Exists(ctx, absent)
	if err != nil || exists {
		t.Errorf("Exists(absent) = %v, %v", exists, err)
	}
}

func TestTags(t *testing.T) {
	isolateDockerConfig(t)

	fake := newFakeRegistry(testRepoPath)
	fake.addManifest(ocispec.MediaTypeImageManifest, testManifest(t), "b", "catalog-20260101.2", "a", "catalog-20260101.10")
	srv := fake.start(t)
	client := newTestClient(plainHosts(t, srv))
	ctx := context.Background()

	tags, err := mustOpen(t, client, hostOf(t, srv), testRepoPath).Tags(ctx)
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}

	if want := []string{"a", "b", "catalog-20260101.10", "catalog-20260101.2"}; !slices.Equal(tags, want) {
		t.Errorf("Tags = %q, want %q", tags, want)
	}

	tags, err = mustOpen(t, client, hostOf(t, srv), "org/missing").Tags(ctx)
	if err != nil {
		t.Fatalf("Tags of a missing repository: %v", err)
	}

	if tags == nil || len(tags) != 0 {
		t.Errorf("Tags of a missing repository = %#v, want an empty list", tags)
	}

	fake.setStatus(http.StatusForbidden)

	_, err = mustOpen(t, client, hostOf(t, srv), testRepoPath).Tags(ctx)
	requireKind(t, err, fault.Registry)
	requireMessage(t, err, "list tags of "+hostOf(t, srv)+"/"+testRepoPath, "access denied")
}

func TestTokenServiceFailureIsNotAMissingTag(t *testing.T) {
	isolateDockerConfig(t)

	start := func(t *testing.T, tokenStatus int) (*fakeRegistry, *Repo, ocispec.Descriptor) {
		t.Helper()

		fake := newFakeRegistry(testRepoPath)
		fake.bearerToken = "valid-token"
		fake.tokenStatus = tokenStatus
		desc := fake.addManifest(ocispec.MediaTypeImageManifest, testManifest(t), "v1")
		srv := fake.start(t)

		return fake, mustOpen(t, newTestClient(plainHosts(t, srv)), hostOf(t, srv), testRepoPath), desc
	}
	ctx := context.Background()

	t.Run("token issued", func(t *testing.T) {
		_, repo, desc := start(t, 0)

		tags, err := repo.Tags(ctx)
		if err != nil || !slices.Equal(tags, []string{"v1"}) {
			t.Fatalf("Tags = %q, %v", tags, err)
		}

		got, err := repo.Resolve(ctx, "v1")
		if err != nil || got.Digest != desc.Digest {
			t.Fatalf("Resolve = %v, %v", got.Digest, err)
		}
	})

	t.Run("token service answers 404", func(t *testing.T) {
		fake, repo, desc := start(t, http.StatusNotFound)

		tags, err := repo.Tags(ctx)
		if tags != nil {
			t.Errorf("Tags = %q, want nil on error", tags)
		}

		requireKind(t, err, fault.Registry)
		requireMessage(t, err, "list tags of", "token service or storage backend", "(404)")

		if IsNotFound(err) {
			t.Errorf("IsNotFound(%v) = true for a token service failure", err)
		}

		_, err = repo.Resolve(ctx, "v2")
		requireKind(t, err, fault.Registry)

		if IsNotFound(err) {
			t.Errorf("IsNotFound(%v) = true for a token service failure", err)
		}

		created, err := repo.EnsureTag(ctx, desc, "v2")
		if created {
			t.Error("EnsureTag created a tag although authentication failed")
		}

		requireKind(t, err, fault.Registry)

		if got := fake.count(http.MethodPut, "/v2/"+testRepoPath+"/manifests/"); got != 0 {
			t.Errorf("EnsureTag sent %d manifest PUTs after an authentication failure", got)
		}
	})
}

func TestTagsTreatsOnlyTheTagListAsTheRepository(t *testing.T) {
	repo := &Repo{name: Repository{Host: "registry.example", Path: "org/schemas"}}
	repo.target = &remote.Repository{Reference: registry.Reference{Registry: "registry.example", Repository: "org/schemas"}}

	response := func(rawURL string, status int, codes ...string) error {
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Fatal(err)
		}

		resp := &errcode.ErrorResponse{Method: http.MethodGet, URL: u, StatusCode: status}
		for _, code := range codes {
			resp.Errors = append(resp.Errors, errcode.Error{Code: code})
		}

		return fmt.Errorf("GET %q: %w", "https://registry.example/v2/org/schemas/tags/list", resp)
	}

	cases := []struct {
		err  error
		name string
		want bool
	}{
		{name: "tag list 404", err: response("https://registry.example/v2/org/schemas/tags/list", http.StatusNotFound), want: true},
		{name: "tag list NAME_UNKNOWN", err: response("https://registry.example/v2/org/schemas/tags/list", http.StatusBadRequest, errcode.ErrorCodeNameUnknown), want: true},
		{name: "token service 404", err: response("https://registry.example/token?scope=x", http.StatusNotFound)},
		{name: "token service NAME_UNKNOWN", err: response("https://auth.example/token", http.StatusUnauthorized, errcode.ErrorCodeNameUnknown)},
		{name: "another repository", err: response("https://registry.example/v2/org/other/tags/list", http.StatusNotFound)},
		{name: "another host", err: response("https://mirror.example/v2/org/schemas/tags/list", http.StatusNotFound)},
		{name: "not a response", err: fmt.Errorf("x: %w", errdef.ErrNotFound)},
	}

	for _, tc := range cases {
		if got := repo.isMissingRepository(tc.err); got != tc.want {
			t.Errorf("%s: isMissingRepository = %v, want %v", tc.name, got, tc.want)
		}
	}
}

type alreadyExistsTarget struct {
	pushes int
}

func (*alreadyExistsTarget) Exists(context.Context, ocispec.Descriptor) (bool, error) {
	return false, nil
}

func (a *alreadyExistsTarget) Push(context.Context, ocispec.Descriptor, io.Reader) error {
	a.pushes++

	return fmt.Errorf("push: %w", errdef.ErrAlreadyExists)
}

func TestPushTreatsAlreadyExistsAsNotPushed(t *testing.T) {
	data := []byte("{}")
	desc := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: godigest.Digest(digest.FromBytes(data)), Size: 2}
	target := &alreadyExistsTarget{}
	repo := &Repo{name: Repository{Host: "registry.example", Path: "a"}}

	pushed, err := repo.push(context.Background(), target, desc, data, "push")
	if err != nil || pushed {
		t.Fatalf("push = %v, %v; want false, nil", pushed, err)
	}

	if target.pushes != 1 {
		t.Errorf("pushes = %d, want 1", target.pushes)
	}
}
