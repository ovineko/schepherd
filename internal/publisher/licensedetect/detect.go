// Package licensedetect finds the license of a schema source that no license
// rule covers, for the publisher's automatic license decisions. It knows two
// kinds of sources that can be pinned:
//
//   - a file of a GitHub repository at a ref
//     (raw.githubusercontent.com/<owner>/<repo>/<ref>/<path>,
//     github.com/<owner>/<repo>/raw|blob/<ref>/<path>): the ref is resolved
//     to a commit and the license, and for Apache-2.0 the NOTICE file, are
//     read through the GitHub REST API at that commit;
//   - a file of an npm package at an exact version on a CDN
//     (unpkg.com/<package>@<version>/<path>,
//     cdn.jsdelivr.net/npm/<package>@<version>/<path>): the license comes
//     from the npm registry metadata of that version, the license and NOTICE
//     files from the same CDN at that version.
//
// Detection does not decide: it reports what it found as a policy.Finding,
// which the license policy judges. Every problem, a rate limit included, is
// a finding that holds the source for review; only cancellation is an error.
// Requests go through the SSRF-hardened httpfetch client to the services the
// policy's [auto] hosts allow, are made once per URL, and at most Jobs of
// them run at a time. A rate limit that resets within Config.RateLimitWait
// is waited out; any other stops requests to that service for the run.
package licensedetect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/httpfetch"
	"github.com/ovineko/schepherd/internal/publisher/policy"
)

// DefaultJobs bounds concurrent requests when Config.Jobs is not positive.
const DefaultJobs = 4

const (
	maxResponseBytes = 2 << 20
	maxRedirects     = 3
	defaultTimeout   = time.Minute
	// maxPauses bounds how often one service's rate limit is waited out in
	// a run, so that a service that keeps refusing cannot stall it for long.
	maxPauses = 4
	// resetMargin is added to a reset time, which has whole seconds only.
	resetMargin = time.Second
)

// Config configures a Detector.
type Config struct {
	// Endpoints replaces the base URL ("https://<host>") of a service, keyed
	// by its policy.Host* name. Tests point services at local servers.
	Endpoints map[string]string
	// Log receives a line when a service starts refusing requests because
	// of a rate limit; nil discards it.
	Log func(format string, args ...any)
	// Token is sent to the GitHub API only, as a bearer token.
	Token string
	// Hosts are the services detection may contact (policy.Auto.Hosts).
	Hosts []string
	// Fetch configures the HTTP client. The detector sets its own body
	// limit and redirect rules: redirects stay on the services of Hosts.
	Fetch httpfetch.Policy
	// Jobs bounds concurrent requests; DefaultJobs when not positive.
	Jobs int
	// RateLimitWait is the longest the detector pauses a service whose rate
	// limit names when it resets (X-RateLimit-Reset or Retry-After) before
	// it retries; a service is paused at most four times per run. A limit
	// that resets later, or names no time, stops all requests to the
	// service for the run. Zero never waits.
	RateLimitWait time.Duration
}

// Detector detects licenses. It is safe for concurrent use and remembers
// every result for its lifetime, which is meant to be one publisher run.
type Detector struct {
	fetcher *httpfetch.Fetcher
	log     func(format string, args ...any)
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error
	base    map[string]string
	limited map[string]string
	paused  map[string]time.Time
	pauses  map[string]int
	results flight[policy.Finding]
	gets    flight[response]
	slots   chan struct{}
	token   string
	hosts   []string
	wait    time.Duration
	mu      sync.Mutex
}

// New returns a Detector for cfg. Endpoints must be absolute http(s) URLs
// without a path; other values are a fault.Usage error.
func New(cfg Config) (*Detector, error) {
	d := &Detector{
		log: cfg.Log, token: cfg.Token, hosts: cfg.Hosts, wait: max(cfg.RateLimitWait, 0), now: time.Now, sleep: sleep,
		base: map[string]string{}, limited: map[string]string{}, paused: map[string]time.Time{}, pauses: map[string]int{},
		results: flight[policy.Finding]{calls: map[string]*call[policy.Finding]{}},
		gets:    flight[response]{calls: map[string]*call[response]{}},
	}

	if d.log == nil {
		d.log = func(string, ...any) {}
	}

	jobs := cfg.Jobs
	if jobs <= 0 {
		jobs = DefaultJobs
	}

	d.slots = make(chan struct{}, jobs)

	endpointHosts := map[string]bool{}

	for _, service := range []string{policy.HostGitHubAPI, policy.HostNPMRegistry, policy.HostUnpkg, policy.HostJSDelivr} {
		base := "https://" + service
		if override, ok := cfg.Endpoints[service]; ok {
			base = override
		}

		u, err := url.Parse(base)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || strings.Trim(u.Path, "/") != "" ||
			u.RawQuery != "" || u.User != nil {
			return nil, fault.New(fault.Usage, "license detection endpoint %q for %s must be an http(s) URL without path", base, service)
		}

		d.base[service] = u.Scheme + "://" + u.Host

		if d.allows(service) {
			endpointHosts[strings.ToLower(u.Host)] = true
		}
	}

	fetch := cfg.Fetch
	fetch.MaxBytes = maxResponseBytes
	fetch.MaxRedirects = maxRedirects

	if fetch.Timeout <= 0 {
		fetch.Timeout = defaultTimeout
	}

	fetch.AllowRedirect = func(target *url.URL) error {
		if !endpointHosts[strings.ToLower(target.Host)] {
			return fmt.Errorf("%s is not a license detection service", target.Host)
		}

		return nil
	}

	d.fetcher = httpfetch.New(fetch)

	return d, nil
}

// Detect returns the finding for the document at rawURL. Findings are
// shared by every URL of the same repository ref or package version on the
// same host, and computed once. The error is non-nil only when ctx ends
// (fault.Canceled).
func (d *Detector) Detect(ctx context.Context, rawURL string) (policy.Finding, error) {
	src, unsupported := d.recognize(rawURL)
	if unsupported != "" {
		return policy.Finding{Failure: policy.RefusedUnsupportedHost, Detail: unsupported}, nil
	}

	return d.results.do(ctx, src.key(), func() (policy.Finding, error) {
		return src.detect(ctx, d)
	})
}

// Cached returns the finding for rawURL without contacting any service: the
// result of an earlier Detect for the same source, or the refusal of a URL
// detection does not support. False means Detect has not finished for the
// URL's source yet.
func (d *Detector) Cached(rawURL string) (policy.Finding, bool) {
	src, unsupported := d.recognize(rawURL)
	if unsupported != "" {
		return policy.Finding{Failure: policy.RefusedUnsupportedHost, Detail: unsupported}, true
	}

	return d.results.done(src.key())
}

// source is a recognized, pinnable origin of a document.
type source interface {
	// key identifies the detection result the source shares with others.
	key() string
	detect(ctx context.Context, d *Detector) (policy.Finding, error)
}

func (d *Detector) recognize(rawURL string) (source, string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "not a URL"
	}

	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")

	switch {
	case u.Scheme != "https" && u.Scheme != "http", u.Opaque != "", host == "":
		return nil, "not an absolute http(s) URL"
	case u.User != nil:
		return nil, "the URL contains credentials"
	case u.RawQuery != "" || u.ForceQuery:
		return nil, "the URL has a query, so it does not name a file of a repository or package"
	case u.Port() != "" && u.Port() != "443" && u.Port() != "80":
		return nil, "the URL names a port"
	}

	segments, err := pathSegments(u)
	if err != nil {
		return nil, err.Error()
	}

	var (
		src     source
		needs   []string
		problem string
	)

	switch host {
	case "raw.githubusercontent.com":
		src, problem = githubSource(segments, false)
		needs = []string{policy.HostGitHubAPI}
	case "github.com":
		src, problem = githubSource(segments, true)
		needs = []string{policy.HostGitHubAPI}
	case policy.HostUnpkg:
		src, problem = npmSource(host, segments)
		needs = []string{policy.HostNPMRegistry, host}
	case policy.HostJSDelivr:
		if len(segments) == 0 || segments[0] != "npm" {
			return nil, "only npm packages (cdn.jsdelivr.net/npm/...) are supported on " + host
		}

		src, problem = npmSource(host, segments[1:])
		needs = []string{policy.HostNPMRegistry, host}
	default:
		return nil, host + " is neither a GitHub nor a supported npm CDN host"
	}

	if problem != "" {
		return nil, problem
	}

	for _, service := range needs {
		if !d.allows(service) {
			return nil, "detection for this source needs " + service + ", which the [auto] hosts do not list"
		}
	}

	return src, ""
}

func (d *Detector) allows(service string) bool {
	return slices.Contains(d.hosts, service)
}

// pathSegments splits the path into unescaped segments. Encoded slashes and
// dot segments are refused: a server could resolve them to another
// repository or package than the one the path appears to name.
func pathSegments(u *url.URL) ([]string, error) {
	escaped := strings.TrimPrefix(u.EscapedPath(), "/")
	if escaped == "" {
		return nil, nil
	}

	raw := strings.Split(escaped, "/")
	out := make([]string, 0, len(raw))

	for _, segment := range raw {
		lower := strings.ToLower(segment)
		if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
			return nil, errors.New("the URL path contains an encoded separator")
		}

		value, err := url.PathUnescape(segment)
		if err != nil {
			return nil, errors.New("the URL path is not validly escaped")
		}

		if value == "." || value == ".." || strings.Contains(value, "\\") {
			return nil, errors.New("the URL path contains dot segments")
		}

		out = append(out, value)
	}

	return out, nil
}

// response is a completed request: status is 200 or 404; a missing file is
// an answer, not a failure.
type response struct {
	body   []byte
	status int
}

type request struct {
	query   url.Values
	service string
	path    string
	accept  string
}

// get performs one request, at most once per URL and accept header for the
// detector's lifetime. path is escaped already.
func (d *Detector) get(ctx context.Context, r request) (response, error) {
	target := d.base[r.service] + r.path
	if len(r.query) > 0 {
		target += "?" + r.query.Encode()
	}

	return d.gets.do(ctx, r.accept+" "+target, func() (response, error) {
		for {
			if err := d.await(ctx, r.service); err != nil {
				return response{}, err
			}

			res, retry, err := d.send(ctx, r, target)
			if !retry {
				return res, err
			}
		}
	})
}

// send performs one attempt of a request; retry means that the service's
// rate limit paused it and the request is to be sent again afterwards.
func (d *Detector) send(ctx context.Context, r request, target string) (response, bool, error) {
	select {
	case d.slots <- struct{}{}:
	case <-ctx.Done():
		return response{}, false, fault.Wrap(fault.Canceled, ctx.Err(), "license detection")
	}

	defer func() { <-d.slots }()

	header := http.Header{"Accept": {r.accept}}
	if r.service == policy.HostGitHubAPI {
		header.Set("X-Github-Api-Version", "2022-11-28")

		if d.token != "" {
			header.Set("Authorization", "Bearer "+d.token)
		}
	}

	res, err := d.fetcher.GetWithHeader(ctx, target, header)
	if err == nil {
		return response{body: res.Body, status: http.StatusOK}, false, nil
	}

	if status, ok := errors.AsType[*httpfetch.StatusError](err); ok {
		if status.Code == http.StatusNotFound || status.Code == http.StatusGone {
			return response{status: http.StatusNotFound}, false, nil
		}

		if r.service == policy.HostGitHubAPI && d.token != "" && status.Code == http.StatusUnauthorized {
			return response{}, false, fault.Wrap(fault.Usage, errRejectedToken, "license detection")
		}

		if message, retry, limited := d.noteRateLimit(r.service, status); limited {
			if retry {
				return response{}, true, nil
			}

			return response{}, false, errors.New(message)
		}
	}

	return response{}, false, err //nolint:wrapcheck // classified by package httpfetch
}

// await returns once the service may be contacted: at once, or after the
// pause of its rate limit. It fails when the service gets no further
// requests in this run.
func (d *Detector) await(ctx context.Context, service string) error {
	for {
		d.mu.Lock()
		limited, until := d.limited[service], d.paused[service]
		d.mu.Unlock()

		if limited != "" {
			return errors.New(limited)
		}

		wait := until.Sub(d.now())
		if wait <= 0 {
			return nil
		}

		if err := d.sleep(ctx, wait); err != nil {
			return fault.Wrap(fault.Canceled, err, "license detection")
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("rate limit pause: %w", ctx.Err())
	}
}

// noteRateLimit recognizes a rate-limit refusal. When the limit resets
// within the detector's wait, the service is paused until then and the
// request retried (retry). Otherwise the detector sends the service nothing
// more in this run: every later request would be refused as well and only
// extend the penalty.
func (d *Detector) noteRateLimit(service string, status *httpfetch.StatusError) (message string, retry, limited bool) {
	if status.Code != http.StatusForbidden && status.Code != http.StatusTooManyRequests {
		return "", false, false
	}

	h := status.Header
	now := d.now()

	var until time.Time

	switch {
	case h.Get("X-Ratelimit-Remaining") == "0":
		message = service + " rate limit exceeded"

		if reset, err := strconv.ParseInt(h.Get("X-Ratelimit-Reset"), 10, 64); err == nil {
			until = time.Unix(reset, 0)
			message += "; it resets at " + until.UTC().Format(time.RFC3339)
		}
	case h.Get("Retry-After") != "":
		message = service + " rate limit exceeded; it asks to retry after " + strconv.Quote(h.Get("Retry-After"))
		until = retryAfter(h.Get("Retry-After"), now)
	case status.Code == http.StatusTooManyRequests:
		message = service + " rate limit exceeded (HTTP 429)"
	default:
		return "", false, false
	}

	if service == policy.HostGitHubAPI && d.token == "" {
		message += "; set GITHUB_TOKEN for a higher limit"
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if stopped := d.limited[service]; stopped != "" {
		return stopped, false, true
	}

	if !until.IsZero() {
		until = later(until, now).Add(resetMargin)

		switch {
		case until.Sub(now) > d.wait:
		case d.paused[service].After(now):
			d.paused[service] = later(d.paused[service], until)

			return message, true, true
		case d.pauses[service] < maxPauses:
			d.paused[service] = until
			d.pauses[service]++
			d.log("warning: %s; license detection waits until %s before it sends more", message, until.UTC().Format(time.RFC3339))

			return message, true, true
		}
	}

	d.limited[service] = message
	d.log("warning: %s; license detection sends it no further requests in this run", message)

	return message, false, true
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}

	return b
}

// retryAfter reads a Retry-After value: delay seconds or an HTTP date. The
// zero time means it names no usable time.
func retryAfter(value string, now time.Time) time.Time {
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil && seconds >= 0 {
		return now.Add(time.Duration(seconds) * time.Second)
	}

	if at, err := http.ParseTime(value); err == nil {
		return at
	}

	return time.Time{}
}

// failed turns a request error into a fetch-failed finding, or into the
// cancellation error when ctx ended.
// errRejectedToken stops the run instead of holding records: with a broken
// token every source that needs detection would quietly stay out of the
// catalog week after week.
var errRejectedToken = errors.New("GitHub rejected the token in GITHUB_TOKEN (HTTP 401); fix or unset it")

func failed(ctx context.Context, src, what string, err error) (policy.Finding, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return policy.Finding{}, fault.Wrap(fault.Canceled, ctxErr, "license detection")
	}

	if errors.Is(err, errRejectedToken) {
		return policy.Finding{}, err
	}

	return policy.Finding{Source: src, Failure: policy.RefusedFetchFailed, Detail: what + ": " + err.Error()}, nil
}

type call[T any] struct {
	done     chan struct{}
	val      T
	err      error
	finished bool
}

// flight runs a function once per key and shares its result. A call that
// ended because its context was canceled is forgotten, so a caller whose
// context is still alive runs it again.
type flight[T any] struct {
	calls map[string]*call[T]
	mu    sync.Mutex
}

func (f *flight[T]) do(ctx context.Context, key string, fn func() (T, error)) (T, error) {
	for {
		f.mu.Lock()

		c, ok := f.calls[key]
		if !ok {
			c = &call[T]{done: make(chan struct{})}
			f.calls[key] = c
			f.mu.Unlock()

			c.val, c.err = fn()

			f.mu.Lock()
			if c.err != nil && ctx.Err() != nil {
				delete(f.calls, key)
			} else {
				c.finished = true
			}
			f.mu.Unlock()
			close(c.done)

			return c.val, c.err
		}

		f.mu.Unlock()

		select {
		case <-c.done:
		case <-ctx.Done():
			var zero T

			return zero, fault.Wrap(fault.Canceled, ctx.Err(), "license detection")
		}

		f.mu.Lock()
		finished := c.finished
		f.mu.Unlock()

		if finished {
			return c.val, c.err
		}
	}
}

func (f *flight[T]) done(key string) (T, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if c, ok := f.calls[key]; ok && c.finished && c.err == nil {
		return c.val, true
	}

	var zero T

	return zero, false
}
