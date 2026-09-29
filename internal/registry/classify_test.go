package registry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/ovineko/schepherd/internal/fault"
)

func TestClassify(t *testing.T) {
	const op = "fetch sha256:x from registry.example/org/schemas"

	registryURL := &url.URL{Scheme: "https", Host: "registry.example", Path: "/v2/org/schemas/manifests/v1"}
	status := func(code int) error {
		return &errcode.ErrorResponse{Method: http.MethodGet, URL: registryURL, StatusCode: code}
	}
	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}

	cases := []struct {
		err       error
		name      string
		fragments []string
		want      fault.Kind
	}{
		{name: "canceled", err: fmt.Errorf("GET: %w", context.Canceled), want: fault.Canceled},
		{name: "deadline", err: &url.Error{Op: "Get", URL: registryURL.String(), Err: context.DeadlineExceeded}, want: fault.Registry, fragments: []string{"timed out"}},
		{name: "not found", err: fmt.Errorf("v1: %w", errdef.ErrNotFound), want: fault.Registry, fragments: []string{"not found (404)"}},
		{name: "401", err: status(http.StatusUnauthorized), want: fault.Registry, fragments: []string{"authentication required or credentials rejected"}},
		{name: "403", err: status(http.StatusForbidden), want: fault.Registry, fragments: []string{"access denied"}},
		{name: "404", err: status(http.StatusNotFound), want: fault.Registry, fragments: []string{"not found (404)"}},
		{name: "429", err: status(http.StatusTooManyRequests), want: fault.Registry, fragments: []string{"rate limited"}},
		{name: "502", err: status(http.StatusBadGateway), want: fault.Registry, fragments: []string{"registry server error (502)"}},
		{name: "400", err: status(http.StatusBadRequest), want: fault.Registry, fragments: []string{"registry rejected the request (400)"}},
		{
			name:      "token service 404",
			err:       notFoundAt("https://auth.example/token?service=registry.example"),
			want:      fault.Registry,
			fragments: []string{"token service or storage backend auth.example answered: not found (404)"},
		},
		{
			name:      "basic credentials missing",
			err:       fmt.Errorf("GET %q: %w", registryURL, auth.ErrBasicCredentialNotFound),
			want:      fault.Registry,
			fragments: []string{"requires credentials but none are configured for it"},
		},
		{
			name:      "unknown authority",
			err:       &url.Error{Op: "Get", URL: registryURL.String(), Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}},
			want:      fault.Registry,
			fragments: []string{"TLS verification of registry registry.example failed", "ca_file"},
		},
		{name: "hostname mismatch", err: x509.HostnameError{Host: "registry.example", Certificate: &x509.Certificate{}}, want: fault.Registry, fragments: []string{"TLS verification", "ca_file"}},
		{name: "mismatched digest", err: content.ErrMismatchedDigest, want: fault.Integrity, fragments: []string{"content does not match its descriptor"}},
		{name: "trailing data", err: content.ErrTrailingData, want: fault.Integrity},
		{name: "invalid descriptor size", err: content.ErrInvalidDescriptorSize, want: fault.Integrity},
		{name: "size limit", err: fmt.Errorf("manifest: %w", errdef.ErrSizeExceedsLimit), want: fault.Integrity},
		{name: "truncated", err: fmt.Errorf("read: %w", io.ErrUnexpectedEOF), want: fault.Registry, fragments: []string{"the connection ended"}},
		{name: "network", err: &url.Error{Op: "Get", URL: registryURL.String(), Err: dialErr}, want: fault.Registry, fragments: []string{"network error"}},
		{name: "invalid reference", err: fmt.Errorf("%w: invalid tag", errdef.ErrInvalidReference), want: fault.Usage},
		{name: "unknown", err: errors.New("something odd"), want: fault.Registry, fragments: []string{"something odd"}},
		{
			name: "nested classification keeps its kind",
			err:  fmt.Errorf("GET %q: failed to resolve credential: %w", registryURL, fault.New(fault.Usage, "bad credentials_file")),
			want: fault.Usage,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.err, op)
			requireKind(t, got, tc.want)
			requireMessage(t, got, tc.fragments...)

			if !strings.HasPrefix(got.Error(), op) {
				t.Errorf("message %q does not start with the operation", got)
			}

			if !errors.Is(got, tc.err) {
				t.Errorf("classified error does not wrap the cause")
			}
		})
	}

	if Classify(nil, op) != nil {
		t.Error("Classify(nil) != nil")
	}

	classified := fault.New(fault.Integrity, "already classified")
	if got := Classify(classified, op); !errors.Is(got, classified) || got.Error() != "already classified" {
		t.Errorf("Classify changed an already classified error: %v", got)
	}
}

func TestClassifyNamesTheHost(t *testing.T) {
	err := &errcode.ErrorResponse{
		Method:     http.MethodGet,
		URL:        &url.URL{Scheme: "https", Host: "registry.example:5000", Path: "/v2/"},
		StatusCode: http.StatusForbidden,
	}

	requireMessage(t, Classify(err, "copy catalog"), "copy catalog (registry.example:5000): access denied")
	requireMessage(t, Classify(err, "copy to registry.example:5000/x"), "copy to registry.example:5000/x: access denied")
	requireMessage(t, Classify(auth.ErrBasicCredentialNotFound, "copy"), "copy: the registry requires credentials")
}

// TestClassifyORASMismatch pins the untyped ORAS messages that
// contradictsDescriptor relies on; TestFetchTo and TestFetchManifestByDigest
// produce them from real ORAS responses.
func TestClassifyORASMismatch(t *testing.T) {
	for _, msg := range []string{
		`GET "https://r/v2/x/manifests/sha256:a": invalid response; digest mismatch in Docker-Content-Digest: received "sha256:b" when expecting "sha256:a"`,
		`GET "https://r/v2/x/blobs/sha256:a": mismatch Content-Length`,
	} {
		requireKind(t, Classify(errors.New(msg), "fetch"), fault.Integrity)
	}

	requireKind(t, Classify(errors.New("mismatch content length 3: expect 4"), "push"), fault.Registry)
}

func notFoundAt(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}

	return fmt.Errorf("GET %q: %w", "https://registry.example/v2/org/schemas/manifests/v1", &errcode.ErrorResponse{Method: http.MethodGet, URL: u, StatusCode: http.StatusNotFound})
}

func TestIsNotFound(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("x: %w", errdef.ErrNotFound), true},
		{Classify(fmt.Errorf("x: %w", errdef.ErrNotFound), "resolve"), true},
		{notFoundAt("https://registry.example/v2/org/schemas/tags/list"), true},
		{notFoundAt("https://registry.example/v2/org/schemas/manifests/v1"), true},
		{notFoundAt("https://registry.example/v2/org/schemas/blobs/sha256:x"), true},
		{notFoundAt("https://registry.example/token?scope=repository:org/schemas:pull"), false},
		{notFoundAt("https://registry.example/v2/token"), false},
		{notFoundAt("https://registry.example/v2/"), false},
		{&errcode.ErrorResponse{StatusCode: http.StatusNotFound}, false},
		{&errcode.ErrorResponse{URL: &url.URL{Path: "/v2/a/tags/list"}, StatusCode: http.StatusForbidden}, false},
		{errors.New("not found"), false},
		{nil, false},
	}

	for _, tc := range cases {
		if got := IsNotFound(tc.err); got != tc.want {
			t.Errorf("IsNotFound(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
