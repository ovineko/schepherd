// Package regproxy is a counting, fault-injecting reverse proxy that the
// end-to-end suite places in front of a real OCI registry. It records every
// request with its distribution API class, so tests can assert exactly which
// traffic a command caused (for example none at all on a warm cache), and it
// can fail, corrupt or truncate chosen responses to exercise error and
// integrity handling. It is test-only code and never shipped.
package regproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Action selects how a Fault changes a matching exchange.
type Action int

const (
	// ActionStatus answers with Fault.Status without contacting the upstream.
	ActionStatus Action = iota
	// ActionCorruptBody inverts the byte in the middle of the upstream body
	// and keeps its length and every header, including Docker-Content-Digest.
	ActionCorruptBody
	// ActionTruncateBody sends the upstream headers, including the full
	// Content-Length, then half of the body, and aborts the connection.
	ActionTruncateBody
)

// Fault describes an injected failure. Empty Method, Class and PathContains
// match every request. Faults are tried in the order they were added; the
// first one that matches and is not used up applies. A body fault is used up
// only when the response arrives, so when concurrent exchanges chose the same
// one, those that find it used up fall through to the next matching body
// fault; they reached the upstream already and skip status faults.
type Fault struct {
	// Name identifies the fault in Record.Fault.
	Name string
	// Method matches the request method case-insensitively.
	Method string
	// Class matches Record.Class, one of the Class* constants.
	Class string
	// PathContains matches a substring of the request path.
	PathContains string
	Action       Action
	// Status is the response status of ActionStatus; zero means 500.
	Status int
	// Times is how often the fault may apply; zero means without limit. Body
	// actions apply, and count, only on 2xx responses that carry a body, so
	// an authentication challenge does not use them up.
	Times int
}

// Record describes one request that reached the proxy.
type Record struct {
	Method string
	Path   string
	Query  string
	// Status is the status sent to the client, or zero when the connection
	// was aborted before a response header was written.
	Status int
	Class  string
	// ReqBytes counts request body bytes forwarded to the upstream.
	ReqBytes int64
	// RespBytes counts response body bytes written to the client.
	RespBytes        int64
	HasAuthorization bool
	// Fault names the fault that changed this exchange, if any.
	Fault string
	// Time is when the request arrived.
	Time time.Time
}

// Stats aggregates the current records.
type Stats struct {
	ByClass map[string]int
	Total   int
	// AuthChallenges counts 401 responses.
	AuthChallenges    int
	WithAuthorization int
	// BlobBytesOut sums the body bytes of successful blob-get responses
	// written to clients; error bodies are not blob content.
	BlobBytesOut int64
}

// Option customizes a Proxy.
type Option func(*Proxy)

// WithTransport sets the round tripper that reaches the upstream, for
// example one that trusts a test CA. The default ignores proxy environment
// variables and disables transparent compression, so bodies pass through
// byte for byte.
func WithTransport(rt http.RoundTripper) Option {
	return func(p *Proxy) { p.transport = rt }
}

// WithErrorLog sets where the listening server reports handler panics and
// connection errors; the default is the standard logger. The aborts that
// ActionTruncateBody causes are never reported.
func WithErrorLog(l *log.Logger) Option {
	return func(p *Proxy) { p.errorLog = l }
}

// Proxy forwards every request to one upstream registry and preserves the
// incoming Host header, so a registry that builds absolute upload locations
// from it keeps pointing clients back at the proxy. All methods are safe for
// concurrent use.
type Proxy struct {
	transport http.RoundTripper
	errorLog  *log.Logger
	handler   *httputil.ReverseProxy
	target    *url.URL

	mu       sync.Mutex
	records  []Record
	faults   []*installedFault
	down     bool
	srv      *http.Server
	host     string
	closed   bool
	inflight int
	idle     chan struct{}
}

// New returns a proxy for the registry at target. It does not listen until
// Start is called.
func New(target *url.URL, opts ...Option) *Proxy {
	upstream := *target
	idle := make(chan struct{})
	close(idle)

	p := &Proxy{target: &upstream, idle: idle}

	for _, opt := range opts {
		opt(p)
	}

	if p.transport == nil {
		p.transport = &http.Transport{
			DialContext:        (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:       100,
			IdleConnTimeout:    90 * time.Second,
			DisableCompression: true,
		}
	}

	p.handler = &httputil.ReverseProxy{
		Rewrite:        p.rewrite,
		Transport:      p.transport,
		FlushInterval:  -1,
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   upstreamFailed,
		// Deliberate truncation makes ReverseProxy log copy errors; the
		// records already tell a test everything that happened. The server
		// keeps its own log, since net/http reports panics other than
		// http.ErrAbortHandler there.
		ErrorLog: log.New(io.Discard, "", 0),
	}

	return p
}

// Start listens on an ephemeral loopback port and returns the base URL, for
// example http://127.0.0.1:41234. A proxy starts at most once.
func (p *Proxy) Start() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.srv != nil || p.closed {
		return "", errors.New("regproxy: Start called twice or after Close")
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("regproxy: listen: %w", err)
	}

	srv := &http.Server{
		Handler:           http.HandlerFunc(p.serve),
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          p.errorLog,
	}

	go func() { _ = srv.Serve(ln) }()

	p.srv = srv
	p.host = ln.Addr().String()

	return "http://" + p.host, nil
}

// Close stops listening, drops open client connections and releases idle
// upstream connections. It is safe to call more than once.
func (p *Proxy) Close() {
	p.mu.Lock()
	srv := p.srv
	p.srv = nil
	p.closed = true
	p.mu.Unlock()

	if srv != nil {
		_ = srv.Close()
	}

	if idle, ok := p.transport.(interface{ CloseIdleConnections() }); ok {
		idle.CloseIdleConnections()
	}
}

// URLHost returns the listening address as host:port, or "" before Start.
func (p *Proxy) URLHost() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.host
}

// WaitIdle blocks until no request is being handled. A client can read the
// last byte of a response slightly before the proxy records the exchange, so
// call it before inspecting Records or Stats.
func (p *Proxy) WaitIdle(ctx context.Context) error {
	p.mu.Lock()
	idle := p.idle
	p.mu.Unlock()

	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("regproxy: wait for idle: %w", ctx.Err())
	}
}

// Records returns a copy of the records of finished exchanges in arrival
// order.
func (p *Proxy) Records() []Record {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := slices.Clone(p.records)
	slices.SortStableFunc(out, func(a, b Record) int { return a.Time.Compare(b.Time) })

	return out
}

// Reset forgets all records. Faults and the down state are kept.
func (p *Proxy) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.records = nil
}

// Stats aggregates the records collected since the last Reset.
func (p *Proxy) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()

	s := Stats{ByClass: map[string]int{}}

	for _, rec := range p.records {
		s.Total++
		s.ByClass[rec.Class]++

		if rec.Status == http.StatusUnauthorized {
			s.AuthChallenges++
		}

		if rec.HasAuthorization {
			s.WithAuthorization++
		}

		if rec.Class == ClassBlobGet && rec.Status >= http.StatusOK && rec.Status < http.StatusMultipleChoices {
			s.BlobBytesOut += rec.RespBytes
		}
	}

	return s
}

// AddFault installs f after the faults already installed. It panics on an
// unknown Action, a negative Times or a Status that cannot be sent as a
// final response, since those are mistakes in the calling test.
func (p *Proxy) AddFault(f Fault) {
	if f.Action < ActionStatus || f.Action > ActionTruncateBody {
		panic("regproxy: unknown fault action " + strconv.Itoa(int(f.Action)))
	}

	if f.Times < 0 {
		panic("regproxy: negative fault Times")
	}

	if f.Status != 0 && (f.Status < 200 || f.Status > 599) {
		panic("regproxy: fault status " + strconv.Itoa(f.Status) + " is not a final HTTP status")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.faults = append(p.faults, &installedFault{spec: f})
}

// ClearFaults removes every fault, including ones already chosen for an
// exchange that is still waiting for the upstream response.
func (p *Proxy) ClearFaults() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, f := range p.faults {
		f.removed = true
	}

	p.faults = nil
}

// SetDown makes every request fail with 502 without contacting the upstream
// while down is true.
func (p *Proxy) SetDown(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.down = down
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	p.enter()

	rec := Record{
		Method:           r.Method,
		Path:             r.URL.Path,
		Query:            r.URL.RawQuery,
		Class:            classify(r.Method, r.URL.Path),
		HasAuthorization: r.Header.Get("Authorization") != "",
		Time:             time.Now(),
	}
	x := &exchange{w: w, method: rec.Method, class: rec.Class, path: rec.Path}
	body := &countingBody{rc: r.Body}

	// Deferred so an exchange aborted with http.ErrAbortHandler is recorded
	// too.
	defer func() {
		rec.Status = x.status
		rec.RespBytes = x.written
		rec.ReqBytes = body.n.Load()
		rec.Fault = x.applied
		p.finish(rec)
	}()

	down, fault := p.admit(rec.Method, rec.Class, rec.Path)

	switch {
	case down:
		writeError(x, r.Method, http.StatusBadGateway, "regproxy: upstream is down")
	case fault != nil && fault.spec.Action == ActionStatus:
		x.applied = fault.spec.Name
		writeError(x, r.Method, fault.spec.status(), "regproxy: injected fault "+fault.spec.Name)
	default:
		x.candidate = fault
		out := r.WithContext(context.WithValue(r.Context(), exchangeKey{}, x))
		out.Body = body
		p.handler.ServeHTTP(x, out)
	}
}

func (p *Proxy) rewrite(r *httputil.ProxyRequest) {
	r.SetURL(p.target)
	r.Out.Host = r.In.Host
}

func (p *Proxy) modifyResponse(resp *http.Response) error {
	if resp.Request == nil {
		return nil
	}

	x, _ := resp.Request.Context().Value(exchangeKey{}).(*exchange)
	if x == nil || x.candidate == nil || !carriesBody(resp) {
		return nil
	}

	fault := p.claimBodyFault(x)
	if fault == nil {
		return nil
	}

	x.applied = fault.Name

	if resp.ContentLength < 0 {
		if err := buffer(resp); err != nil {
			return err
		}
	}

	if fault.Action == ActionTruncateBody {
		resp.Body = &truncatingBody{rc: resp.Body, remaining: resp.ContentLength / 2}
		x.flushHeader = true
	} else {
		resp.Body = &corruptingBody{rc: resp.Body, at: resp.ContentLength / 2}
	}

	return nil
}

// admit decides before contacting the upstream. A status fault is used up
// here; a body fault is only a candidate until the response shows whether
// it carries a body.
func (p *Proxy) admit(method, class, path string) (bool, *installedFault) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.down {
		return true, nil
	}

	for _, f := range p.faults {
		if !f.available() || !f.spec.matches(method, class, path) {
			continue
		}

		if f.spec.Action == ActionStatus {
			f.used++
		}

		return false, f
	}

	return false, nil
}

// claimBodyFault uses up the exchange's candidate or, when concurrent
// exchanges used it up after admission, the first body fault that still
// matches and is available. A candidate removed by ClearFaults ends the
// search, so faults added after the clear only reach later requests.
func (p *Proxy) claimBodyFault(x *exchange) *Fault {
	p.mu.Lock()
	defer p.mu.Unlock()

	if x.candidate.removed {
		return nil
	}

	if x.candidate.available() {
		x.candidate.used++

		return &x.candidate.spec
	}

	for _, f := range p.faults {
		if f.spec.Action != ActionStatus && f.available() && f.spec.matches(x.method, x.class, x.path) {
			f.used++

			return &f.spec
		}
	}

	return nil
}

func (p *Proxy) enter() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.inflight == 0 {
		p.idle = make(chan struct{})
	}

	p.inflight++
}

func (p *Proxy) finish(rec Record) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.records = append(p.records, rec)

	p.inflight--
	if p.inflight == 0 {
		close(p.idle)
	}
}

type installedFault struct {
	spec    Fault
	used    int
	removed bool
}

func (f *installedFault) available() bool {
	return !f.removed && (f.spec.Times == 0 || f.used < f.spec.Times)
}

func (f *Fault) matches(method, class, path string) bool {
	return (f.Method == "" || strings.EqualFold(f.Method, method)) &&
		(f.Class == "" || f.Class == class) &&
		(f.PathContains == "" || strings.Contains(path, f.PathContains))
}

func (f *Fault) status() int {
	if f.Status == 0 {
		return http.StatusInternalServerError
	}

	return f.Status
}

type exchangeKey struct{}

// exchange observes what reaches the client and carries the per-request
// fault decision from serve to modifyResponse.
type exchange struct {
	w           http.ResponseWriter
	method      string
	class       string
	path        string
	status      int
	written     int64
	candidate   *installedFault
	applied     string
	flushHeader bool
}

func (x *exchange) Header() http.Header {
	return x.w.Header()
}

// WriteHeader also receives 1xx responses, from the transport's goroutine,
// so it touches the exchange's fields only for final statuses.
func (x *exchange) WriteHeader(code int) {
	if code >= http.StatusOK && x.status == 0 {
		x.status = code
	}

	x.w.WriteHeader(code)

	// Headers must reach the client before a truncated body is aborted, or
	// the client would see a failed request instead of a short body.
	if code >= http.StatusOK && x.flushHeader {
		_ = http.NewResponseController(x.w).Flush()
	}
}

func (x *exchange) Write(b []byte) (int, error) {
	if x.status == 0 {
		x.status = http.StatusOK
	}

	n, err := x.w.Write(b)
	x.written += int64(n)

	if err != nil {
		return n, fmt.Errorf("regproxy: write response: %w", err)
	}

	return n, nil
}

// Unwrap lets http.ResponseController reach the connection's Flusher.
func (x *exchange) Unwrap() http.ResponseWriter {
	return x.w
}

func carriesBody(resp *http.Response) bool {
	return resp.Request.Method != http.MethodHead &&
		resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices &&
		resp.StatusCode != http.StatusNoContent &&
		resp.ContentLength != 0
}

// buffer reads a body of unknown length so its middle can be located. It
// only runs for responses a body fault applies to.
func buffer(resp *http.Response) error {
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if err != nil {
		return fmt.Errorf("regproxy: read upstream body: %w", err)
	}

	resp.Body = io.NopCloser(bytes.NewReader(data))
	resp.ContentLength = int64(len(data))
	resp.Header.Set("Content-Length", strconv.Itoa(len(data)))

	return nil
}

type registryError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorBody struct {
	Errors []registryError `json:"errors"`
}

// writeError answers the way a distribution registry reports errors.
func writeError(w http.ResponseWriter, method string, status int, message string) {
	if method == http.MethodHead || status == http.StatusNoContent || status == http.StatusNotModified {
		w.WriteHeader(status)

		return
	}

	data, err := json.Marshal(errorBody{Errors: []registryError{{Code: "UNKNOWN", Message: message}}})
	if err != nil {
		w.WriteHeader(status)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func upstreamFailed(w http.ResponseWriter, r *http.Request, err error) {
	writeError(w, r.Method, http.StatusBadGateway, "regproxy: upstream: "+err.Error())
}
