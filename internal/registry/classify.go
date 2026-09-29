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
	"slices"
	"strings"

	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/ovineko/schepherd/internal/fault"
)

// IsNotFound reports whether err means the registry answered 404 for the
// requested manifest, blob, tag or repository. A 404 from anywhere else, such
// as the bearer token service during an authentication challenge, is an
// authentication failure and not "not found".
func IsNotFound(err error) bool {
	if errors.Is(err, errdef.ErrNotFound) {
		return true
	}

	resp, ok := errors.AsType[*errcode.ErrorResponse](err)

	return ok && resp.StatusCode == http.StatusNotFound && isRepositoryEndpoint(resp.URL)
}

// isRepositoryEndpoint reports whether u addresses a manifest, a blob or the
// tag list of some repository in the distribution API.
func isRepositoryEndpoint(u *url.URL) bool {
	if u == nil {
		return false
	}

	rest, ok := strings.CutPrefix(u.Path, "/v2/")
	if !ok {
		return false
	}

	for _, endpoint := range []string{"/manifests/", "/blobs/"} {
		if strings.Index(rest, endpoint) > 0 {
			return true
		}
	}

	return len(rest) > len("/tags/list") && strings.HasSuffix(rest, "/tags/list")
}

// isRegistryAPI reports whether u is one of the distribution API's own paths.
// An error response from any other URL comes from the bearer token service or
// from a storage backend the registry redirected to. Token realms such as
// /v2/token or /v2/auth share the /v2/ prefix but are not API paths.
func isRegistryAPI(u *url.URL) bool {
	return u.Path == "/v2/" || u.Path == "/v2/_catalog" || isRepositoryEndpoint(u)
}

// Classify turns an error from ORAS or the network into a fault error whose
// message starts with op. An error that is already a *fault.Error keeps its
// message, and a classified cause deeper in the chain keeps its kind. The
// registry host is taken from the error when op does not name it. URLs in the
// message never show user information, a query or a fragment, which may carry
// the signature of a storage URL the registry redirected to.
func Classify(err error, op string) error {
	host := hostFromError(err)
	if host != "" && !strings.Contains(op, host) {
		op = op + " (" + host + ")"
	}

	return classify(err, op, host)
}

func classify(err error, op, host string) error {
	if err == nil {
		return nil
	}

	cause := err

	clean := redacted(err)
	if clean != nil {
		cause = clean
	}

	if classified, ok := errors.AsType[*fault.Error](err); ok {
		//nolint:errorlint // identity, not equality: only an outermost *fault.Error passes through; a nested one still gets op
		if error(classified) != err {
			return fault.Wrap(classified.Kind, cause, "%s", op)
		}

		if clean == nil {
			return err
		}

		return &fault.Error{Kind: classified.Kind, Err: clean}
	}

	kind, msg := describe(err, host)
	if msg == "" {
		return fault.Wrap(kind, cause, "%s", op)
	}

	return fault.Wrap(kind, cause, "%s: %s", op, msg)
}

func describe(err error, host string) (fault.Kind, string) {
	if kind, msg, ok := describeSentinel(err, host); ok {
		return kind, msg
	}

	if resp, ok := errors.AsType[*errcode.ErrorResponse](err); ok {
		return fault.Registry, describeResponse(resp)
	}

	if contradictsDescriptor(err) {
		return fault.Integrity, "the registry response does not match the requested descriptor"
	}

	return describeTransport(err, host)
}

// ORAS v2.6.2 reports a response whose Docker-Content-Digest or
// Content-Length contradicts the requested descriptor as an untyped error,
// so the message is the only signal. TestClassifyORASMismatch pins it.
var descriptorMismatchMessages = []string{
	"invalid response; digest mismatch",
	": mismatch Content-Length",
}

func contradictsDescriptor(err error) bool {
	msg := err.Error()

	return slices.ContainsFunc(descriptorMismatchMessages, func(fragment string) bool {
		return strings.Contains(msg, fragment)
	})
}

func describeSentinel(err error, host string) (fault.Kind, string, bool) {
	switch {
	case errors.Is(err, context.Canceled):
		return fault.Canceled, "canceled", true
	case errors.Is(err, context.DeadlineExceeded):
		return fault.Registry, "timed out", true
	case errors.Is(err, content.ErrMismatchedDigest),
		errors.Is(err, content.ErrTrailingData),
		errors.Is(err, content.ErrInvalidDescriptorSize):
		return fault.Integrity, "content does not match its descriptor", true
	case errors.Is(err, errdef.ErrSizeExceedsLimit):
		return fault.Integrity, "content exceeds the size limit", true
	case errors.Is(err, auth.ErrBasicCredentialNotFound):
		return fault.Registry, registryName(host) + " requires credentials but none are configured for it", true
	case errors.Is(err, errdef.ErrNotFound):
		return fault.Registry, "not found (404)", true
	case errors.Is(err, errdef.ErrInvalidReference), errors.Is(err, errdef.ErrInvalidDigest):
		return fault.Usage, "invalid reference", true
	case errors.Is(err, errdef.ErrTooManyPages):
		return fault.Registry, "the registry returned too many result pages", true
	case errors.Is(err, errdef.ErrUnsupported):
		return fault.Registry, "the registry does not support this operation", true
	default:
		return fault.Internal, "", false
	}
}

func describeResponse(resp *errcode.ErrorResponse) string {
	status := describeStatus(resp.StatusCode)
	if resp.URL == nil || isRegistryAPI(resp.URL) {
		return status
	}

	return "token service or storage backend " + resp.URL.Host + " answered: " + status
}

func describeStatus(code int) string {
	switch {
	case code == http.StatusUnauthorized:
		return "authentication required or credentials rejected (401)"
	case code == http.StatusForbidden:
		return "access denied (403)"
	case code == http.StatusNotFound:
		return "not found (404)"
	case code == http.StatusTooManyRequests:
		return "rate limited (429)"
	case code >= http.StatusInternalServerError:
		return fmt.Sprintf("registry server error (%d)", code)
	default:
		return fmt.Sprintf("registry rejected the request (%d)", code)
	}
}

func describeTransport(err error, host string) (fault.Kind, string) {
	if isCertificateError(err) {
		return fault.Registry, "TLS verification of " + registryName(host) +
			" failed; if it uses a private CA, trust it with ca_file for this host"
	}

	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return fault.Registry, "timed out"
	}

	if errors.Is(err, io.ErrUnexpectedEOF) {
		return fault.Registry, "the connection ended before the content was complete"
	}

	if _, ok := errors.AsType[net.Error](err); ok {
		return fault.Registry, "network error"
	}

	return fault.Registry, ""
}

func isCertificateError(err error) bool {
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return true
	}

	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
		return true
	}

	if _, ok := errors.AsType[x509.HostnameError](err); ok {
		return true
	}

	_, ok := errors.AsType[x509.CertificateInvalidError](err)

	return ok
}

func hostFromError(err error) string {
	if resp, ok := errors.AsType[*errcode.ErrorResponse](err); ok && resp.URL != nil && isRegistryAPI(resp.URL) {
		return resp.URL.Host
	}

	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		if u, parseErr := url.Parse(urlErr.URL); parseErr == nil {
			return u.Host
		}
	}

	return ""
}

func registryName(host string) string {
	if host == "" {
		return "the registry"
	}

	return "registry " + host
}
