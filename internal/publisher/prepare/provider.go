package prepare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/httpfetch"
	"github.com/ovineko/schepherd/internal/publisher/policy"
	"github.com/ovineko/schepherd/internal/publisher/upstream"
)

var errNotInSnapshot = errors.New("not in the pinned SchemaStore snapshot, and SchemaStore URLs are never fetched from the network")

// fetchError is a network failure while obtaining a document.
type fetchError struct {
	err error
	uri string
}

func (e *fetchError) Error() string {
	return e.err.Error()
}

func (e *fetchError) Unwrap() error {
	return e.err
}

// redirectRefusedError is returned by the fetcher's redirect hook when the
// license policy does not allow the redirect target.
type redirectRefusedError struct {
	target   string
	decision policy.Decision
}

func (e *redirectRefusedError) Error() string {
	return "redirect target " + e.target + " is not allowed by the license policy: " + e.decision.Reason
}

// decisionFor returns the decision to report for a refused redirect.
func (e *redirectRefusedError) decisionFor() policy.Decision {
	d := e.decision
	d.Reason = "redirected to " + e.target + ": " + d.Reason

	return d
}

// limitError reports a document above the per-document size limit.
type limitError struct {
	uri   string
	limit int64
}

func (e *limitError) Error() string {
	return fmt.Sprintf("%s is larger than %d bytes", e.uri, e.limit)
}

// provider obtains documents by URI: SchemaStore URLs from the pinned
// snapshot, URIs the local source maps to files from those files, and
// everything else over the network, each network URI once per run. A
// request whose redirect the license policy refused is repeated once the
// policy allows that target: automatic license detection may have decided
// it since (see network).
type provider struct {
	snap    *upstream.Snapshot
	local   map[string]string
	fetcher *httpfetch.Fetcher
	cache   map[string]*cached
	// redirectAllowed reports whether the fetcher would now follow a
	// redirect to target; nil never repeats a request.
	redirectAllowed func(target string) bool
	limit           int64
	mu              sync.Mutex
}

type cached struct {
	err   error
	final string
	body  []byte
	once  sync.Once
}

// document is what the provider obtained for a URI.
type document struct {
	// redirect is the URL that served body when the request for the URI was
	// redirected elsewhere. The license policy must allow it too: a rule
	// for the requested URL says nothing about content hosted elsewhere.
	redirect     string
	body         []byte
	fromSnapshot bool
}

func newProvider(snap *upstream.Snapshot, local map[string]string, fetcher *httpfetch.Fetcher, limit int64) *provider {
	return &provider{snap: snap, local: local, fetcher: fetcher, limit: limit, cache: map[string]*cached{}}
}

// obtain returns the document at uri (without fragment).
func (p *provider) obtain(ctx context.Context, uri string) (document, error) {
	if p.snap != nil {
		if file, ok := p.snap.LocalPath(uri); ok {
			data, err := readLimited(file, uri, p.limit)

			return document{body: data, fromSnapshot: true}, err
		}

		if schemaStoreHosted(uri) {
			return document{}, fmt.Errorf("%s: %w", uri, errNotInSnapshot)
		}
	}

	if file, ok := p.local[normalizeURI(uri)]; ok {
		data, err := readLimited(file, uri, p.limit)

		return document{body: data}, err
	}

	return p.network(ctx, uri)
}

// network fetches uri once per run. A redirect is vetted before its
// target is contacted, on the license findings known at that moment: a
// published schema reused without detection, or a record fetched before
// detection decided its target's repository, may meet a refusal that no
// longer holds when a later record prepares the same URI in full. Such a
// refusal is not kept once the target is allowed; the request is repeated.
func (p *provider) network(ctx context.Context, uri string) (document, error) {
	c := p.fetch(ctx, uri, nil)

	if refusal, ok := errors.AsType[*redirectRefusedError](c.err); ok && p.redirectAllowed != nil && p.redirectAllowed(refusal.target) {
		c = p.fetch(ctx, uri, c)
	}

	if c.err != nil {
		if errors.Is(c.err, httpfetch.ErrTooLarge) {
			return document{}, &limitError{uri: uri, limit: p.limit}
		}

		return document{}, c.err
	}

	requested, _, _ := strings.Cut(uri, "#")

	doc := document{body: c.body}
	if normalizeURI(c.final) != normalizeURI(requested) {
		doc.redirect = c.final
	}

	return doc, nil
}

// fetch returns the result of fetching uri, fetching it on the first call
// only; stale, when not nil, is a result to replace with a new request
// unless another caller replaced it already.
func (p *provider) fetch(ctx context.Context, uri string, stale *cached) *cached {
	p.mu.Lock()

	c, ok := p.cache[uri]
	if !ok || c == stale {
		c = &cached{}
		p.cache[uri] = c
	}

	p.mu.Unlock()

	c.once.Do(func() {
		res, err := p.fetcher.Get(ctx, uri)
		if err != nil {
			c.err = &fetchError{uri: uri, err: err}

			return
		}

		c.body, c.final = res.Body, res.URL
	})

	return c
}

// schemaStoreHosted reports whether uri lives on a host whose content the
// snapshot pins: such URIs are resolved from the snapshot or not at all.
func schemaStoreHosted(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil {
		return false
	}

	switch strings.TrimSuffix(strings.ToLower(u.Hostname()), ".") {
	case "www.schemastore.org", "json.schemastore.org", "schemastore.org":
		return true
	case "raw.githubusercontent.com":
		return strings.HasPrefix(strings.ToLower(u.EscapedPath()), "/schemastore/schemastore/")
	default:
		return false
	}
}

// readLimited reads a local file of at most limit bytes. Errors name uri, not
// the local path, because they end up in reports.
func readLimited(file, uri string, limit int64) ([]byte, error) {
	f, err := os.Open(filepath.Clean(file))
	if err != nil {
		return nil, fault.Wrap(fault.Usage, withoutPath(err), "read the local file for %s", uri)
	}

	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fault.Wrap(fault.Internal, withoutPath(err), "read the local file for %s", uri)
	}

	if int64(len(data)) > limit {
		return nil, &limitError{uri: uri, limit: limit}
	}

	return data, nil
}

func withoutPath(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err
	}

	return err
}
