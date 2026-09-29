package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/ovineko/schepherd/internal/fault"
)

// signedQuery is the query of a presigned storage URL: an S3 signature and
// credential scope plus an Azure SAS signature.
const signedQuery = "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAEXAMPLE%2F20260925%2Fus-east-1%2Fs3%2Faws4_request" +
	"&X-Amz-Signature=SECRETSIGNATURE123&sig=SECRETSAS456&se=2099-01-01"

// requireNoQuery fails when the message of err shows any part of a URL
// query or fragment.
func requireNoQuery(t *testing.T, err error) {
	t.Helper()

	msg := err.Error()
	for _, leak := range []string{"?", "#", "SECRET", "X-Amz", "AKIAEXAMPLE", "sig=", "se=2099"} {
		if strings.Contains(msg, leak) {
			t.Errorf("error shows %q from a URL query or fragment: %s", leak, msg)
		}
	}
}

func TestClassifyRedactsURLQueries(t *testing.T) {
	const storage = "https://storage.example/bucket/blob"

	signed := storage + "?" + signedQuery + "#frag"
	signedURL, err := url.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}

	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	forbidden := &errcode.ErrorResponse{Method: http.MethodGet, URL: signedURL, StatusCode: http.StatusForbidden}

	cases := []struct {
		err       error
		name      string
		fragments []string
		want      fault.Kind
	}{
		{
			name:      "url.Error",
			err:       &url.Error{Op: "Get", URL: signed, Err: dialErr},
			want:      fault.Registry,
			fragments: []string{"network error", `Get "` + storage + `"`},
		},
		{
			name:      "relative Location of a refused redirect",
			err:       &url.Error{Op: "Get", URL: "/bucket/blob?" + signedQuery, Err: errors.New("stopped after 10 redirects")},
			want:      fault.Registry,
			fragments: []string{`Get "/bucket/blob": stopped after 10 redirects`},
		},
		{
			name:      "query with a quote and a space",
			err:       &url.Error{Op: "Get", URL: storage + `?sig=SECRET "a b"&` + signedQuery, Err: dialErr},
			want:      fault.Registry,
			fragments: []string{`Get "` + storage + `"`},
		},
		{
			name:      "error response of a storage backend",
			err:       fmt.Errorf("fetch sha256:x: %w", forbidden),
			want:      fault.Registry,
			fragments: []string{"token service or storage backend storage.example answered: access denied (403)", `GET "` + storage + `"`},
		},
		{
			name:      "untyped ORAS message",
			err:       fmt.Errorf("%s %q: mismatch Content-Length", http.MethodGet, signedURL),
			want:      fault.Integrity,
			fragments: []string{`GET "` + storage + `": mismatch Content-Length`},
		},
		{
			name:      "unquoted URL",
			err:       fmt.Errorf("upstream said %s is gone", signed),
			want:      fault.Registry,
			fragments: []string{"upstream said " + storage + " is gone"},
		},
		{
			name:      "joined errors",
			err:       errors.Join(&url.Error{Op: "Put", URL: "https://registry.example/v2/org/schemas/blobs/uploads/1?_state=SECRETSTATE&digest=sha256:x", Err: dialErr}, forbidden),
			want:      fault.Registry,
			fragments: []string{`Put "https://registry.example/v2/org/schemas/blobs/uploads/1"`, `GET "` + storage + `"`},
		},
		{
			name:      "nested classified error",
			err:       fmt.Errorf("GET %q: failed to resolve credential: %w", signed, fault.New(fault.Usage, "bad credentials_file")),
			want:      fault.Usage,
			fragments: []string{`GET "` + storage + `": failed to resolve credential: bad credentials_file`},
		},
		{
			name:      "outermost classified error",
			err:       fault.Wrap(fault.Integrity, &url.Error{Op: "Get", URL: signed, Err: dialErr}, "source content"),
			want:      fault.Integrity,
			fragments: []string{`source content: Get "` + storage + `"`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.err, "fetch sha256:x from registry.example/org/schemas")
			requireKind(t, got, tc.want)
			requireMessage(t, got, tc.fragments...)
			requireNoQuery(t, got)

			if !errors.Is(got, tc.err) {
				t.Error("classified error does not wrap the cause")
			}
		})
	}
}

func TestStripQueries(t *testing.T) {
	cases := map[string]string{
		"":                                "",
		"no URL here? really#not":         "no URL here? really#not",
		"https://r.example/v2/":           "https://r.example/v2/",
		"see https://r.example/a?b=c now": "see https://r.example/a now",
		"at http://r.example/a#frag":      "at http://r.example/a",
		`GET "https://s.example/b?sig=x\"y z": failed? yes`:  `GET "https://s.example/b": failed? yes`,
		`"https://a.example/x?1" and "http://b.example/y?2"`: `"https://a.example/x" and "http://b.example/y"`,
		`unterminated "https://a.example/x?sig=1 rest`:       `unterminated "https://a.example/x`,
		`escape at the end "https://a.example/x?s=\`:         `escape at the end "https://a.example/x`,
		"://?x": "://",
		`GET "https://user:pw@s.example/b@c?sig=1": failed`: `GET "https://s.example/b@c": failed`,
		"at https://user@r.example/a?x=1 now":               "at https://r.example/a now",
	}

	for in, want := range cases {
		if got := stripQueries(in); got != want {
			t.Errorf("stripQueries(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactReturnsCleanErrorsUnchanged(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "https://registry.example/v2/org/schemas/manifests/sha256:x", Err: io.ErrUnexpectedEOF}
	if got := redacted(err); got != nil {
		t.Errorf("redacted rewrote an error without a query: %v", got)
	}

	classified := fault.New(fault.Integrity, "already classified")
	//nolint:errorlint // identity: the error must pass through unchanged
	if got := Classify(classified, "op"); got != error(classified) {
		t.Errorf("Classify changed an already classified error without a query: %v", got)
	}
}

func TestCheckRedirectRefusalOmitsTheQuery(t *testing.T) {
	first := httptest.NewRequest(http.MethodGet, "https://registry.example/v2/org/schemas/blobs/sha256:x", nil)

	target, err := url.Parse("http://user:pass@storage.example/blob?" + signedQuery + "#frag")
	if err != nil {
		t.Fatal(err)
	}

	err = checkRedirect(&http.Request{Method: http.MethodGet, URL: target, Header: http.Header{}}, []*http.Request{first})
	if err == nil {
		t.Fatal("redirect from HTTPS to HTTP was followed")
	}

	if got, want := err.Error(), "refusing redirect from HTTPS to http://storage.example/blob"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}

// refusedURL returns the base URL of a port nothing listens on.
func refusedURL(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	return "http://" + addr
}

// deniedURL returns the base URL of a storage backend that refuses every
// request with 403 and echoes the query in an XML body, as S3 does for a
// signature it does not accept.
func deniedURL(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<Error><Code>SignatureDoesNotMatch</Code><Query>"+r.URL.RawQuery+"</Query></Error>")
	}))
	t.Cleanup(srv.Close)

	return srv.URL
}

// TestSignedRedirectFailuresDoNotShowTheSignature follows a registry that
// redirects blob and manifest downloads to presigned storage URLs whose hop
// then fails.
func TestSignedRedirectFailuresDoNotShowTheSignature(t *testing.T) {
	isolateDockerConfig(t)

	storages := []struct {
		base      func(t *testing.T) string
		name      string
		fragments []string
	}{
		{name: "connection refused", base: refusedURL, fragments: []string{"network error", refusedText()}},
		{name: "403", base: deniedURL, fragments: []string{"token service or storage backend", "access denied (403)"}},
	}

	for _, storage := range storages {
		t.Run("blob/"+storage.name, func(t *testing.T) {
			base := storage.base(t)
			fake := newFakeRegistry(testRepoPath)
			fake.redirectBlobsTo = base + "/storage"
			fake.redirectQuery = signedQuery
			desc := fake.addBlob([]byte("payload"))
			srv := fake.start(t)

			repo := mustOpen(t, newTestClient(plainHosts(t, srv)), hostOf(t, srv), testRepoPath)

			err := repo.FetchTo(context.Background(), desc, &bytes.Buffer{})
			requireKind(t, err, fault.Registry)
			requireMessage(t, err, append(storage.fragments, `"`+base+"/storage/v2/"+testRepoPath+"/blobs/"+desc.Digest.String()+`"`)...)
			requireNoQuery(t, err)
		})

		t.Run("manifest/"+storage.name, func(t *testing.T) {
			base := storage.base(t)
			fake := newFakeRegistry(testRepoPath)
			fake.redirectManifestsTo = base + "/storage"
			fake.redirectQuery = signedQuery
			desc := fake.addManifest(ocispec.MediaTypeImageManifest, testManifest(t))
			srv := fake.start(t)

			repo := mustOpen(t, newTestClient(plainHosts(t, srv)), hostOf(t, srv), testRepoPath)

			_, err := repo.FetchManifestByDigest(context.Background(), desc.Digest.String(), ocispec.MediaTypeImageManifest, 0)
			requireKind(t, err, fault.Registry)
			requireMessage(t, err, append(storage.fragments, `"`+base+"/storage/v2/"+testRepoPath+"/manifests/"+desc.Digest.String()+`"`)...)
			requireNoQuery(t, err)
		})
	}
}

func TestRefusedDowngradeDoesNotShowTheSignature(t *testing.T) {
	isolateDockerConfig(t)

	base := refusedURL(t)
	fake := newFakeRegistry(testRepoPath)
	fake.redirectBlobsTo = base + "/storage"
	fake.redirectQuery = signedQuery
	desc := fake.addBlob([]byte("payload"))
	srv := fake.startTLS(t)
	host := hostOf(t, srv)

	client := newTestClient(map[string]HostConfig{host: {CAFile: writeCAFile(t, srv)}})

	err := mustOpen(t, client, host, testRepoPath).FetchTo(context.Background(), desc, &bytes.Buffer{})
	requireKind(t, err, fault.Registry)
	requireMessage(t, err, "refusing redirect from HTTPS to "+base+"/storage/v2/"+testRepoPath+"/blobs/"+desc.Digest.String())
	requireNoQuery(t, err)
}

func FuzzStripQueries(f *testing.F) {
	for _, seed := range []string{
		"",
		`Get "https://s.example/b?` + signedQuery + `": dial tcp: connection refused`,
		`GET "https://user:pw@s.example/b@c?sig=1#f": failed`,
		`"a://b?\"c d" and e://f?g h`,
		"://?x",
		`q://"x@a://b?c`,
		`"://\#"#00`,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, text string) {
		once := stripQueries(text)
		if len(once) > len(text) {
			t.Fatalf("stripQueries(%q) = %q grew the text", text, once)
		}

		if twice := stripQueries(once); twice != once {
			t.Fatalf("stripQueries is not idempotent: %q -> %q -> %q", text, once, twice)
		}
	})
}

// refusedText is how the operating system words a refused TCP connection.
func refusedText() string {
	if runtime.GOOS == "windows" {
		return "actively refused"
	}

	return "connection refused"
}
