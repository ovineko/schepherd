// Package httpfetch is the publisher's HTTP(S) client for untrusted upstream
// URLs. It is hardened against server-side request forgery: the address
// policy is enforced on the IP that is actually dialed, so a hostname that
// resolves (or re-resolves, as in DNS rebinding) to an internal address is
// refused; every redirect target is re-validated; downgrades from HTTPS to
// HTTP, credentials in URLs, cookies and proxies from the environment are not
// supported; and response bodies are bounded.
//
// The package is used only by the maintainer-side publisher.
package httpfetch

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ovineko/schepherd/internal/buildinfo"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

const (
	// DefaultMaxBytes bounds a response body when Policy.MaxBytes is not set.
	DefaultMaxBytes = 16 << 20
	// DefaultMaxRedirects is used when Policy.MaxRedirects is zero.
	DefaultMaxRedirects = 5
	// DefaultTimeout bounds one Get, redirects and body included, when
	// Policy.Timeout is not set.
	DefaultTimeout = 2 * time.Minute

	dialTimeout           = 30 * time.Second
	keepAlive             = 30 * time.Second
	tlsHandshakeTimeout   = 15 * time.Second
	idleConnTimeout       = 90 * time.Second
	maxIdleConns          = 16
	maxResponseHeaderSize = 1 << 20
)

var (
	// ErrInvalidURL reports a URL that is malformed, relative or opaque.
	ErrInvalidURL = errors.New("invalid URL")
	// ErrScheme reports a scheme other than https (or http when allowed).
	ErrScheme = errors.New("URL scheme is not allowed")
	// ErrUserinfo reports credentials embedded in a URL.
	ErrUserinfo = errors.New("URL must not contain credentials")
	// ErrBlockedAddress reports a destination that is not a public unicast
	// address and is not explicitly allowlisted.
	ErrBlockedAddress = errors.New("destination address is not public")
	// ErrTooManyRedirects reports a redirect chain longer than allowed.
	ErrTooManyRedirects = errors.New("too many redirects")
	// ErrRedirectRefused reports a redirect target rejected by
	// Policy.AllowRedirect.
	ErrRedirectRefused = errors.New("redirect refused")
	// ErrDowngrade reports a redirect from https to http.
	ErrDowngrade = errors.New("redirect from https to http is refused")
	// ErrTooLarge reports a response body above the size limit.
	ErrTooLarge = errors.New("response exceeds the size limit")
)

// StatusError reports a final response with a non-2xx status code. Header
// holds the response headers, from which callers read rate-limit hints.
type StatusError struct {
	Header http.Header
	URL    string
	Code   int
}

// Error implements the error interface.
func (e *StatusError) Error() string {
	return fmt.Sprintf("GET %s: unexpected HTTP status %d", e.URL, e.Code)
}

// Policy configures a Fetcher. Zero values select the documented defaults;
// limits cannot be disabled.
type Policy struct {
	// UserAgent defaults to "schepherd-publisher/<version>".
	UserAgent string
	// AllowPrivateHosts lists exact "host:port" pairs that may be dialed even
	// though they are loopback, private or otherwise internal. A pair matches
	// the host and port written in the URL (default ports included), not the
	// resolved address. Intended for tests and local fixture servers.
	AllowPrivateHosts []string
	// MaxBytes bounds each response body after content decoding; bodies
	// above it are an error, never silently truncated.
	MaxBytes int64
	// MaxRedirects bounds followed redirects. Zero means DefaultMaxRedirects;
	// a negative value refuses every redirect.
	MaxRedirects int
	// Timeout bounds one Get including redirects and the body.
	Timeout time.Duration
	// AllowHTTP permits plain-http URLs. Redirects from https to http stay
	// refused regardless.
	AllowHTTP bool
	// AllowRedirect, when set, vets every redirect target after the built-in
	// checks and before the target is contacted. Its error stops the fetch
	// and is wrapped with ErrRedirectRefused.
	AllowRedirect func(target *url.URL) error
	// Transport, when set, carries every request in place of the network.
	// The dialer's address checks do not apply to it; the checks of URLs,
	// redirects and sizes still do. Tests serve public host names from
	// memory with it; no source or policy file can set it.
	Transport http.RoundTripper
}

// Result is a successful response.
type Result struct {
	// URL is the final URL after redirects, without fragment.
	URL string
	// Digest is the sha256 digest of Body.
	Digest string
	Body   []byte
}

// Fetcher performs policy-checked GET requests. It is safe for concurrent use.
type Fetcher struct {
	client  *http.Client
	allowed map[string]struct{}
	policy  Policy
}

// New returns a Fetcher enforcing p.
func New(p Policy) *Fetcher {
	if p.MaxBytes <= 0 {
		p.MaxBytes = DefaultMaxBytes
	}

	if p.MaxRedirects == 0 {
		p.MaxRedirects = DefaultMaxRedirects
	}

	if p.Timeout <= 0 {
		p.Timeout = DefaultTimeout
	}

	if p.UserAgent == "" {
		p.UserAgent = "schepherd-publisher/" + buildinfo.Get().Version
	}

	f := &Fetcher{policy: p, allowed: make(map[string]struct{}, len(p.AllowPrivateHosts))}

	for _, hostPort := range p.AllowPrivateHosts {
		if key, ok := canonicalHostPort(hostPort); ok {
			f.allowed[key] = struct{}{}
		}
	}

	restricted := &net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlive, Control: controlPublic}
	unrestricted := &net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlive}

	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if f.allowlisted(addr) {
				return unrestricted.DialContext(ctx, network, addr)
			}

			return restricted.DialContext(ctx, network, addr)
		},
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    tlsHandshakeTimeout,
		IdleConnTimeout:        idleConnTimeout,
		MaxIdleConns:           maxIdleConns,
		MaxResponseHeaderBytes: maxResponseHeaderSize,
	}

	f.client = &http.Client{
		Transport:     transport,
		CheckRedirect: f.checkRedirect,
		Timeout:       p.Timeout,
	}

	if p.Transport != nil {
		f.client.Transport = p.Transport
	}

	return f
}

// Get downloads rawURL. Policy violations and oversized bodies are classified
// as fault.Integrity, network failures and non-2xx statuses as fault.Registry
// and cancellation of ctx as fault.Canceled. Error messages never contain a
// password from rawURL.
func (f *Fetcher) Get(ctx context.Context, rawURL string) (Result, error) {
	return f.GetWithHeader(ctx, rawURL, nil)
}

// GetWithHeader is Get with additional request headers, which replace the
// default Accept and User-Agent when they name them. Credentials such as an
// Authorization header are only forwarded along redirects to the same host
// (or its subdomains); net/http drops them on any other redirect.
func (f *Fetcher) GetWithHeader(ctx context.Context, rawURL string, header http.Header) (Result, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		// *url.Error quotes the whole input, credentials included.
		if urlErr, ok := errors.AsType[*url.Error](err); ok {
			err = urlErr.Err
		}

		return Result{}, fault.Wrap(fault.Integrity, fmt.Errorf("%w: %w", ErrInvalidURL, err), "fetch")
	}

	u.Fragment, u.RawFragment = "", ""
	target := u.Redacted()

	if err := f.checkURL(u); err != nil {
		return Result{}, fault.Wrap(fault.Integrity, err, "fetch %s", target)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Result{}, fault.Wrap(fault.Integrity, fmt.Errorf("%w: %w", ErrInvalidURL, err), "fetch %s", target)
	}

	req.Header.Set("User-Agent", f.policy.UserAgent)
	req.Header.Set("Accept", "*/*")

	for name, values := range header {
		req.Header[http.CanonicalHeaderKey(name)] = slices.Clone(values)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return Result{}, classify(ctx, err, target)
	}

	defer func() { _ = resp.Body.Close() }()

	final := resp.Request.URL.String()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return Result{}, fault.Wrap(fault.Registry, &StatusError{URL: final, Code: resp.StatusCode, Header: resp.Header.Clone()}, "fetch %s", target)
	}

	if resp.ContentLength > f.policy.MaxBytes {
		return Result{}, fault.Wrap(fault.Integrity,
			fmt.Errorf("%w: declared %d bytes, limit is %d", ErrTooLarge, resp.ContentLength, f.policy.MaxBytes),
			"fetch %s", final)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, f.policy.MaxBytes+1))
	if err != nil {
		return Result{}, classify(ctx, err, final)
	}

	if int64(len(body)) > f.policy.MaxBytes {
		return Result{}, fault.Wrap(fault.Integrity,
			fmt.Errorf("%w: more than %d bytes", ErrTooLarge, f.policy.MaxBytes), "fetch %s", final)
	}

	return Result{URL: final, Body: body, Digest: digest.FromBytes(body)}, nil
}

func (f *Fetcher) checkURL(u *url.URL) error {
	switch {
	case !u.IsAbs() || u.Opaque != "" || u.Host == "" || u.Hostname() == "":
		return fmt.Errorf("%w %q: expected an absolute URL with a host", ErrInvalidURL, u.Redacted())
	case u.User != nil:
		return fmt.Errorf("%w (%s)", ErrUserinfo, u.Redacted())
	case u.Scheme == "https":
	case u.Scheme == "http" && f.policy.AllowHTTP:
	default:
		return fmt.Errorf("%w: %q", ErrScheme, u.Scheme)
	}

	port := u.Port()
	if port == "" {
		port = defaultPort(u.Scheme)
	}

	if f.allowlisted(net.JoinHostPort(u.Hostname(), port)) {
		return nil
	}

	if addr, ok := parseIP(u.Hostname()); ok && !isPublic(addr) {
		return fmt.Errorf("%w: %s", ErrBlockedAddress, addr)
	}

	return nil
}

func (f *Fetcher) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > f.policy.MaxRedirects {
		return fmt.Errorf("%w (limit %d)", ErrTooManyRedirects, max(f.policy.MaxRedirects, 0))
	}

	if err := f.checkURL(req.URL); err != nil {
		return err
	}

	if req.URL.Scheme == "http" {
		for _, prev := range via {
			if prev.URL.Scheme == "https" {
				return fmt.Errorf("%w: %s", ErrDowngrade, req.URL.Redacted())
			}
		}
	}

	if f.policy.AllowRedirect != nil {
		if err := f.policy.AllowRedirect(req.URL); err != nil {
			return fmt.Errorf("%w to %s: %w", ErrRedirectRefused, req.URL.Redacted(), err)
		}
	}

	return nil
}

func (f *Fetcher) allowlisted(hostPort string) bool {
	if len(f.allowed) == 0 {
		return false
	}

	key, ok := canonicalHostPort(hostPort)
	if !ok {
		return false
	}

	_, ok = f.allowed[key]

	return ok
}

func controlPublic(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparsable dial address %q", ErrBlockedAddress, address)
	}

	addr, ok := parseIP(host)
	if !ok {
		return fmt.Errorf("%w: unparsable dial address %q", ErrBlockedAddress, address)
	}

	if !isPublic(addr) {
		return fmt.Errorf("%w: %s", ErrBlockedAddress, addr)
	}

	return nil
}

func canonicalHostPort(hostPort string) (string, bool) {
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil || host == "" || port == "" {
		return "", false
	}

	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if addr, ok := parseIP(host); ok {
		host = addr.String()
	}

	return net.JoinHostPort(host, port), true
}

func defaultPort(scheme string) string {
	if scheme == "http" {
		return "80"
	}

	return "443"
}

func classify(ctx context.Context, err error, target string) error {
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		err = urlErr.Err
	}

	switch {
	case errors.Is(err, ErrBlockedAddress), errors.Is(err, ErrScheme), errors.Is(err, ErrUserinfo),
		errors.Is(err, ErrInvalidURL), errors.Is(err, ErrTooManyRedirects), errors.Is(err, ErrDowngrade), errors.Is(err, ErrRedirectRefused):
		return fault.Wrap(fault.Integrity, err, "fetch %s", target)
	case errors.Is(ctx.Err(), context.Canceled):
		return fault.Wrap(fault.Canceled, err, "fetch %s", target)
	default:
		return fault.Wrap(fault.Registry, err, "fetch %s", target)
	}
}
