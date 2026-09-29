package httpfetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

type counter struct {
	n atomic.Int64
}

func (c *counter) wrap(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.n.Add(1)
		h(w, r)
	}
}

func hostPort(t *testing.T, srv *httptest.Server) string {
	t.Helper()

	return srv.Listener.Addr().String()
}

func port(t *testing.T, srv *httptest.Server) string {
	t.Helper()

	_, p, err := net.SplitHostPort(hostPort(t, srv))
	if err != nil {
		t.Fatal(err)
	}

	return p
}

func newServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return srv
}

func newTLSServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()

	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)

	return srv
}

func trust(t *testing.T, f *Fetcher, srv *httptest.Server) {
	t.Helper()

	transport, ok := f.client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("unexpected transport type")
	}

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	transport.TLSClientConfig.RootCAs = pool
}

func expectErr(t *testing.T, err, want error, kind fault.Kind) {
	t.Helper()

	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}

	if got := fault.KindOf(err); got != kind {
		t.Fatalf("kind = %v, want %v (%v)", got, kind, err)
	}
}

func okHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}
}

func TestLoopbackRefusedByDefault(t *testing.T) {
	var hits counter

	srv := newServer(t, hits.wrap(okHandler("{}")))
	f := New(Policy{AllowHTTP: true})

	_, err := f.Get(t.Context(), srv.URL)
	expectErr(t, err, ErrBlockedAddress, fault.Integrity)

	_, err = f.Get(t.Context(), "http://localhost:"+port(t, srv)+"/")
	expectErr(t, err, ErrBlockedAddress, fault.Integrity)

	if hits.n.Load() != 0 {
		t.Fatalf("server received %d requests", hits.n.Load())
	}
}

func TestDialedAddressIsChecked(t *testing.T) {
	var hits counter

	srv := newServer(t, hits.wrap(okHandler("{}")))
	f := New(Policy{AllowHTTP: true})

	_, err := f.Get(t.Context(), "http://localhost:"+port(t, srv)+"/")
	expectErr(t, err, ErrBlockedAddress, fault.Integrity)

	if _, ok := errors.AsType[*net.OpError](err); !ok {
		t.Fatalf("expected the refusal to come from the dialer, got %v", err)
	}

	if hits.n.Load() != 0 {
		t.Fatalf("server received %d requests", hits.n.Load())
	}
}

func TestAllowlistedLoopback(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "test-agent/1" {
			t.Errorf("User-Agent = %q", got)
		}

		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	f := New(Policy{AllowHTTP: true, AllowPrivateHosts: []string{hostPort(t, srv)}, UserAgent: "test-agent/1"})

	res, err := f.Get(t.Context(), srv.URL+"/schema.json#/definitions/x")
	if err != nil {
		t.Fatal(err)
	}

	if string(res.Body) != `{"ok":true}` {
		t.Fatalf("body = %q", res.Body)
	}

	if res.Digest != digest.FromBytes(res.Body) {
		t.Fatalf("digest = %s", res.Digest)
	}

	if res.URL != srv.URL+"/schema.json" {
		t.Fatalf("URL = %s", res.URL)
	}
}

func TestAllowlistByHostName(t *testing.T) {
	srv := newServer(t, okHandler("x"))
	p := port(t, srv)

	f := New(Policy{AllowHTTP: true, AllowPrivateHosts: []string{"LOCALHOST:" + p}})

	res, err := f.Get(t.Context(), "http://localhost:"+p+"/")
	if err != nil {
		t.Fatal(err)
	}

	if string(res.Body) != "x" {
		t.Fatalf("body = %q", res.Body)
	}

	_, err = f.Get(t.Context(), srv.URL)
	expectErr(t, err, ErrBlockedAddress, fault.Integrity)
}

func TestDefaultUserAgent(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Header.Get("User-Agent")))
	})

	f := New(Policy{AllowHTTP: true, AllowPrivateHosts: []string{hostPort(t, srv)}})

	res, err := f.Get(t.Context(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(string(res.Body), "schepherd-publisher/") {
		t.Fatalf("User-Agent = %q", res.Body)
	}
}

func TestRedirectToPrivateAddressRefused(t *testing.T) {
	var privateHits counter

	private := newServer(t, privateHits.wrap(okHandler("secret")))

	targets := map[string]string{
		"/literal":   private.URL + "/",
		"/name":      "http://localhost:" + port(t, private) + "/",
		"/rfc1918":   "http://10.0.0.1/",
		"/mapped":    "http://[::ffff:127.0.0.1]:" + port(t, private) + "/",
		"/metadata":  "http://169.254.169.254/latest/meta-data/",
		"/cgnat":     "http://100.64.0.1/",
		"/ula":       "http://[fd00::1]/",
		"/zero":      "http://0.0.0.0:" + port(t, private) + "/",
		"/userinfo":  "http://user:pass@" + hostPort(t, private) + "/",
		"/ftp":       "ftp://example.com/x",
		"/multicast": "http://224.0.0.1/",
	}

	public := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targets[r.URL.Path], http.StatusFound)
	})

	f := New(Policy{AllowHTTP: true, AllowPrivateHosts: []string{hostPort(t, public)}})

	want := map[string]error{
		"/literal":   ErrBlockedAddress,
		"/name":      ErrBlockedAddress,
		"/rfc1918":   ErrBlockedAddress,
		"/mapped":    ErrBlockedAddress,
		"/metadata":  ErrBlockedAddress,
		"/cgnat":     ErrBlockedAddress,
		"/ula":       ErrBlockedAddress,
		"/zero":      ErrBlockedAddress,
		"/userinfo":  ErrUserinfo,
		"/ftp":       ErrScheme,
		"/multicast": ErrBlockedAddress,
	}

	for path, wantErr := range want {
		t.Run(strings.TrimPrefix(path, "/"), func(t *testing.T) {
			_, err := f.Get(t.Context(), public.URL+path)
			expectErr(t, err, wantErr, fault.Integrity)

			if strings.Contains(err.Error(), ":pass@") {
				t.Fatalf("error reveals the redirect target's password: %v", err)
			}
		})
	}

	if privateHits.n.Load() != 0 {
		t.Fatalf("private server received %d requests", privateHits.n.Load())
	}
}

func TestRedirectFollowedWithinPolicy(t *testing.T) {
	target := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusMovedPermanently)

			return
		}

		_, _ = w.Write([]byte("done"))
	})

	f := New(Policy{AllowHTTP: true, AllowPrivateHosts: []string{hostPort(t, target)}})

	res, err := f.Get(t.Context(), target.URL+"/start")
	if err != nil {
		t.Fatal(err)
	}

	if res.URL != target.URL+"/final" || string(res.Body) != "done" {
		t.Fatalf("result = %+v", res)
	}
}

func TestTooManyRedirects(t *testing.T) {
	var hits counter

	srv := newServer(t, hits.wrap(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		http.Redirect(w, r, "/"+strconv.Itoa(n+1), http.StatusFound)
	}))

	tests := []struct {
		name     string
		max      int
		wantHits int64
	}{
		{name: "limit two", max: 2, wantHits: 3},
		{name: "default", max: 0, wantHits: DefaultMaxRedirects + 1},
		{name: "none", max: -1, wantHits: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hits.n.Store(0)

			f := New(Policy{AllowHTTP: true, AllowPrivateHosts: []string{hostPort(t, srv)}, MaxRedirects: tt.max})

			_, err := f.Get(t.Context(), srv.URL+"/0")
			expectErr(t, err, ErrTooManyRedirects, fault.Integrity)

			if got := hits.n.Load(); got != tt.wantHits {
				t.Fatalf("requests = %d, want %d", got, tt.wantHits)
			}
		})
	}
}

func TestHTTPSToHTTPDowngradeRefused(t *testing.T) {
	var plainHits counter

	plain := newServer(t, plainHits.wrap(okHandler("plain")))
	secure := newTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/", http.StatusFound)
	})

	f := New(Policy{AllowHTTP: true, AllowPrivateHosts: []string{hostPort(t, plain), hostPort(t, secure)}})
	trust(t, f, secure)

	_, err := f.Get(t.Context(), secure.URL+"/")
	expectErr(t, err, ErrDowngrade, fault.Integrity)

	strict := New(Policy{AllowPrivateHosts: []string{hostPort(t, plain), hostPort(t, secure)}})
	trust(t, strict, secure)

	_, err = strict.Get(t.Context(), secure.URL+"/")
	expectErr(t, err, ErrScheme, fault.Integrity)

	if plainHits.n.Load() != 0 {
		t.Fatalf("plain server received %d requests", plainHits.n.Load())
	}
}

func TestHTTPSAllowed(t *testing.T) {
	srv := newTLSServer(t, okHandler("secure"))
	f := New(Policy{AllowPrivateHosts: []string{hostPort(t, srv)}})
	trust(t, f, srv)

	res, err := f.Get(t.Context(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	if string(res.Body) != "secure" {
		t.Fatalf("body = %q", res.Body)
	}
}

func TestURLValidation(t *testing.T) {
	f := New(Policy{})

	tests := []struct {
		want error
		url  string
	}{
		{url: "http://example.com/x", want: ErrScheme},
		{url: "ftp://example.com/x", want: ErrScheme},
		{url: "file:///etc/passwd", want: ErrInvalidURL},
		{url: "https://user:pw@example.com/", want: ErrUserinfo},
		{url: "https://user@example.com/", want: ErrUserinfo},
		{url: "/relative/path", want: ErrInvalidURL},
		{url: "https:opaque", want: ErrInvalidURL},
		{url: "https://%zz/", want: ErrInvalidURL},
		{url: "https://127.0.0.1/", want: ErrBlockedAddress},
		{url: "https://[::1]/", want: ErrBlockedAddress},
		{url: "https://[fe80::1%25eth0]/", want: ErrBlockedAddress},
		{url: "https://192.168.1.1:8443/", want: ErrBlockedAddress},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			_, err := f.Get(t.Context(), tt.url)
			expectErr(t, err, tt.want, fault.Integrity)
		})
	}
}

func TestErrorsRedactPasswords(t *testing.T) {
	f := New(Policy{})

	for _, raw := range []string{
		"https://alice:s3cr3t@example.com/x.json",
		"https://alice:s3cr3t@exa mple.com/x.json",
		"https://alice:s3cr3t@example.com:port/x.json",
		"https://alice:s3cr3t@example.com/%zz",
		"http://alice:s3cr3t@example.com/x.json",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := f.Get(t.Context(), raw)
			if err == nil || fault.KindOf(err) != fault.Integrity {
				t.Fatalf("error = %v", err)
			}

			if strings.Contains(err.Error(), "s3cr3t") {
				t.Fatalf("error reveals the password: %v", err)
			}
		})
	}
}

func TestResponseSizeLimit(t *testing.T) {
	const limit = 64

	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/exact":
			_, _ = w.Write(bytes.Repeat([]byte("a"), limit))
		case "/declared":
			_, _ = w.Write(bytes.Repeat([]byte("a"), limit+1))
		case "/chunked":
			flusher, _ := w.(http.Flusher)
			for range limit + 1 {
				_, _ = w.Write([]byte("a"))
				flusher.Flush()
			}
		case "/gzip":
			var buf bytes.Buffer

			zw := gzip.NewWriter(&buf)
			_, _ = zw.Write(make([]byte, 1<<20))
			_ = zw.Close()

			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(buf.Bytes())
		}
	})

	f := New(Policy{AllowHTTP: true, AllowPrivateHosts: []string{hostPort(t, srv)}, MaxBytes: limit})

	res, err := f.Get(t.Context(), srv.URL+"/exact")
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Body) != limit {
		t.Fatalf("body length = %d", len(res.Body))
	}

	for _, path := range []string{"/declared", "/chunked", "/gzip"} {
		t.Run(path, func(t *testing.T) {
			_, err := f.Get(t.Context(), srv.URL+path)
			expectErr(t, err, ErrTooLarge, fault.Integrity)
		})
	}
}

func TestNon2xxStatus(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	})

	f := New(Policy{AllowHTTP: true, AllowPrivateHosts: []string{hostPort(t, srv)}})

	_, err := f.Get(t.Context(), srv.URL+"/missing")

	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Code != http.StatusNotFound {
		t.Fatalf("error = %v", err)
	}

	if fault.KindOf(err) != fault.Registry {
		t.Fatalf("kind = %v", fault.KindOf(err))
	}
}

func TestNoCookies(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/set" {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "secret", Path: "/"})
			http.Redirect(w, r, "/check", http.StatusFound)

			return
		}

		_, _ = w.Write([]byte(r.Header.Get("Cookie")))
	})

	f := New(Policy{AllowHTTP: true, AllowPrivateHosts: []string{hostPort(t, srv)}})

	for range 2 {
		res, err := f.Get(t.Context(), srv.URL+"/set")
		if err != nil {
			t.Fatal(err)
		}

		if len(res.Body) != 0 {
			t.Fatalf("cookie sent: %q", res.Body)
		}
	}
}

func TestNoEnvironmentProxy(t *testing.T) {
	f := New(Policy{})

	transport, ok := f.client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("unexpected transport type")
	}

	if transport.Proxy != nil {
		t.Fatal("transport consults a proxy")
	}

	if f.client.Jar != nil {
		t.Fatal("client keeps cookies")
	}
}

func TestTimeoutAndCancel(t *testing.T) {
	release := make(chan struct{})

	srv := newServer(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	t.Cleanup(func() { close(release) })

	allowed := []string{hostPort(t, srv)}

	f := New(Policy{AllowHTTP: true, AllowPrivateHosts: allowed, Timeout: 100 * time.Millisecond})

	_, err := f.Get(t.Context(), srv.URL)
	if err == nil || fault.KindOf(err) != fault.Registry {
		t.Fatalf("timeout error = %v (kind %v)", err, fault.KindOf(err))
	}

	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)

	slow := New(Policy{AllowHTTP: true, AllowPrivateHosts: allowed})

	_, err = slow.Get(ctx, srv.URL)
	if err == nil || fault.KindOf(err) != fault.Canceled {
		t.Fatalf("cancel error = %v (kind %v)", err, fault.KindOf(err))
	}
}

func TestIsPublic(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{addr: "93.184.215.14", want: true},
		{addr: "140.82.112.3", want: true},
		{addr: "2606:4700::1111", want: true},
		{addr: "::ffff:93.184.215.14", want: true},
		{addr: "64:ff9b::5db8:d70e", want: true},
		{addr: "2002:5db8:d70e::1", want: true},
		{addr: "127.0.0.1", want: false},
		{addr: "127.1.2.3", want: false},
		{addr: "10.1.2.3", want: false},
		{addr: "172.16.0.1", want: false},
		{addr: "172.31.255.255", want: false},
		{addr: "192.168.0.1", want: false},
		{addr: "169.254.169.254", want: false},
		{addr: "100.64.0.1", want: false},
		{addr: "100.127.255.255", want: false},
		{addr: "0.0.0.0", want: false},
		{addr: "0.1.2.3", want: false},
		{addr: "224.0.0.251", want: false},
		{addr: "239.255.255.250", want: false},
		{addr: "255.255.255.255", want: false},
		{addr: "240.0.0.1", want: false},
		{addr: "192.0.2.1", want: false},
		{addr: "198.18.0.1", want: false},
		{addr: "::", want: false},
		{addr: "::1", want: false},
		{addr: "::127.0.0.1", want: false},
		{addr: "::ffff:127.0.0.1", want: false},
		{addr: "::ffff:10.0.0.1", want: false},
		{addr: "::ffff:100.64.0.1", want: false},
		{addr: "::ffff:169.254.169.254", want: false},
		{addr: "fc00::1", want: false},
		{addr: "fd12:3456::1", want: false},
		{addr: "fe80::1", want: false},
		{addr: "fec0::1", want: false},
		{addr: "ff02::1", want: false},
		{addr: "64:ff9b::7f00:1", want: false},
		{addr: "64:ff9b::a00:1", want: false},
		{addr: "64:ff9b:1::1", want: false},
		{addr: "2002:7f00:1::1", want: false},
		{addr: "2002:a00:1::1", want: false},
		{addr: "2001::1", want: false},
		{addr: "2001:db8::1", want: false},
		{addr: "100::1", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			if got := isPublic(netip.MustParseAddr(tt.addr)); got != tt.want {
				t.Fatalf("isPublic(%s) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

func TestCanonicalHostPort(t *testing.T) {
	tests := map[string]string{
		"LocalHost:80":         "localhost:80",
		"example.com.:443":     "example.com:443",
		"[::FFFF:7F00:1]:8080": "[::ffff:127.0.0.1]:8080",
		"127.0.0.1:1":          "127.0.0.1:1",
	}

	for in, want := range tests {
		got, ok := canonicalHostPort(in)
		if !ok || got != want {
			t.Fatalf("canonicalHostPort(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}

	for _, bad := range []string{"", "host", ":80", "host:"} {
		if _, ok := canonicalHostPort(bad); ok {
			t.Fatalf("canonicalHostPort(%q) accepted", bad)
		}
	}
}

func FuzzIsPublicMapped(f *testing.F) {
	f.Add([]byte{127, 0, 0, 1})
	f.Add([]byte{10, 0, 0, 1})
	f.Add([]byte{93, 184, 215, 14})

	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) < 4 {
			return
		}

		v4 := netip.AddrFrom4([4]byte(raw[:4]))
		mapped := netip.AddrFrom16(v4.As16())
		nat := netip.MustParseAddr("64:ff9b::").As16()
		copy(nat[12:], raw[:4])

		want := isPublic(v4)
		if isPublic(mapped) != want || isPublic(netip.AddrFrom16(nat)) != want {
			t.Fatalf("IPv6 spellings of %s disagree", v4)
		}
	})
}

func FuzzCheckURL(f *testing.F) {
	f.Add("https://example.com/a.json")
	f.Add("http://[::1]:80/")
	f.Add("https://u:p@h/")
	f.Add("https://[fe80::1%25en0]/")
	f.Add("http://[::ffff:10.0.0.1]/")

	fetcher := New(Policy{AllowHTTP: true})

	f.Fuzz(func(t *testing.T, raw string) {
		u, err := url.Parse(raw)
		if err != nil || fetcher.checkURL(u) != nil {
			return
		}

		if u.Scheme != "http" && u.Scheme != "https" {
			t.Fatalf("accepted scheme %q", u.Scheme)
		}

		if u.User != nil || u.Hostname() == "" {
			t.Fatalf("accepted %q", raw)
		}

		if addr, ok := parseIP(u.Hostname()); ok && !isPublic(addr) {
			t.Fatalf("accepted internal address %q", raw)
		}
	})
}

func TestAllowRedirectVetsTargetsBeforeContactingThem(t *testing.T) {
	var targetHits atomic.Int32

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(target.Close)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/moved.json", http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	refused := errors.New("not reviewed")
	policy := Policy{
		AllowHTTP:         true,
		AllowPrivateHosts: []string{hostPort(t, origin), hostPort(t, target)},
		AllowRedirect: func(u *url.URL) error {
			if u.Path == "/moved.json" {
				return refused
			}

			return nil
		},
	}

	_, err := New(policy).Get(context.Background(), origin.URL+"/dep.json")
	if !errors.Is(err, ErrRedirectRefused) || !errors.Is(err, refused) {
		t.Fatalf("Get = %v, want ErrRedirectRefused wrapping the hook error", err)
	}

	if hits := targetHits.Load(); hits != 0 {
		t.Errorf("refused redirect target was contacted %d times", hits)
	}

	policy.AllowRedirect = func(*url.URL) error { return nil }

	res, err := New(policy).Get(context.Background(), origin.URL+"/dep.json")
	if err != nil || res.URL != target.URL+"/moved.json" || targetHits.Load() != 1 {
		t.Fatalf("allowed redirect: %v, final URL %q, hits %d", err, res.URL, targetHits.Load())
	}
}

func TestGetWithHeader(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Header.Get("Accept") + "|" + r.Header.Get("X-Api-Version") + "|" + r.Header.Get("User-Agent")))
	})

	f := New(Policy{AllowHTTP: true, AllowPrivateHosts: []string{hostPort(t, srv)}, UserAgent: "test-agent"})

	res, err := f.GetWithHeader(t.Context(), srv.URL, http.Header{"Accept": {"application/json"}, "x-api-version": {"7"}})
	if err != nil {
		t.Fatal(err)
	}

	if got := string(res.Body); got != "application/json|7|test-agent" {
		t.Fatalf("headers seen by the server = %q", got)
	}

	res, err = f.Get(t.Context(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	if got := string(res.Body); got != "*/*||test-agent" {
		t.Fatalf("default headers seen by the server = %q", got)
	}
}

func TestAuthorizationNotForwardedToAnotherHost(t *testing.T) {
	var seen atomic.Value

	other := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte("ok"))
	})

	// 127.0.0.1 and localhost are different hosts to net/http's redirect
	// rules, just like two public domains.
	target := "http://localhost:" + port(t, other) + "/"
	origin := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	})

	f := New(Policy{AllowHTTP: true, AllowPrivateHosts: []string{hostPort(t, origin), "localhost:" + port(t, other)}})

	if _, err := f.GetWithHeader(t.Context(), origin.URL, http.Header{"Authorization": {"Bearer secret"}}); err != nil {
		t.Fatal(err)
	}

	if got, _ := seen.Load().(string); got != "" {
		t.Fatalf("the redirect target received Authorization %q", got)
	}
}

func TestStatusErrorCarriesHeaders(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Ratelimit-Remaining", "0")
		w.Header().Set("Retry-After", "30")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	})

	f := New(Policy{AllowHTTP: true, AllowPrivateHosts: []string{hostPort(t, srv)}})

	_, err := f.Get(t.Context(), srv.URL)

	statusErr, ok := errors.AsType[*StatusError](err)
	if !ok || statusErr.Code != http.StatusTooManyRequests {
		t.Fatalf("error = %v", err)
	}

	if statusErr.Header.Get("X-Ratelimit-Remaining") != "0" || statusErr.Header.Get("Retry-After") != "30" {
		t.Fatalf("headers = %v", statusErr.Header)
	}
}

// roundTripFunc answers requests without a network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// A Policy.Transport carries requests for public host names in place of the
// network; the URL, redirect and size checks still apply.
func TestTransportReplacesTheNetwork(t *testing.T) {
	var requested []string

	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requested = append(requested, r.URL.String())

		rec := httptest.NewRecorder()

		switch r.URL.Path {
		case "/raw/main/a.json":
			http.Redirect(rec, r, "https://raw.example.org/main/a.json", http.StatusFound)
		case "/downgrade.json":
			http.Redirect(rec, r, "http://raw.example.org/main/a.json", http.StatusFound)
		case "/refused.json":
			http.Redirect(rec, r, "https://refused.example.org/a.json", http.StatusFound)
		default:
			_, _ = rec.WriteString(`{"served":"` + r.URL.Host + `"}`)
		}

		resp := rec.Result()
		resp.Request = r

		return resp, nil
	})

	refused := errors.New("not reviewed")
	f := New(Policy{
		Transport: transport, MaxBytes: 64,
		AllowRedirect: func(u *url.URL) error {
			if u.Host == "refused.example.org" {
				return refused
			}

			return nil
		},
	})

	res, err := f.Get(t.Context(), "https://github.example.org/raw/main/a.json")
	if err != nil || res.URL != "https://raw.example.org/main/a.json" || string(res.Body) != `{"served":"raw.example.org"}` {
		t.Fatalf("Get = %+v, %v", res, err)
	}

	_, err = f.Get(t.Context(), "https://github.example.org/downgrade.json")
	expectErr(t, err, ErrScheme, fault.Integrity)

	_, err = f.Get(t.Context(), "https://github.example.org/refused.json")
	expectErr(t, err, refused, fault.Integrity)

	_, err = f.Get(t.Context(), "https://127.0.0.1/a.json")
	expectErr(t, err, ErrBlockedAddress, fault.Integrity)

	_, err = f.Get(t.Context(), "https://"+strings.Repeat("x", 100)+".example.org/a.json")
	expectErr(t, err, ErrTooLarge, fault.Integrity)

	for _, u := range requested {
		if strings.Contains(u, "refused.example.org") || strings.HasPrefix(u, "http:") || strings.Contains(u, "127.0.0.1") {
			t.Errorf("the transport received a request the checks refuse: %s", u)
		}
	}
}
