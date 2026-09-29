// Package github is the small GitHub API client of the catalog bot: branch
// heads, blobs and file history through GraphQL, signed commits through the
// createCommitOnBranch mutation, and tag refs and releases through REST.
//
// Every failure is classified with internal/fault: transport, authentication
// and unexpected API responses are fault.Registry, invalid arguments are
// fault.Usage. The token is only ever written into the Authorization header.
package github

import (
	"bytes"
	"context"

	// bearer:disable go_gosec_blocklist_sha1
	// Git object IDs are SHA-1, see BlobID.
	"crypto/sha1" //nolint:gosec // Git object IDs, see BlobID
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ovineko/schepherd/internal/fault"
)

// DefaultAPIURL is the REST base URL of github.com.
const DefaultAPIURL = "https://api.github.com"

// APIVersion is the REST API version every request asks for.
const APIVersion = "2022-11-28"

const (
	userAgent       = "schepherd-catalogbot"
	maxResponseSize = 16 << 20
	requestTimeout  = 2 * time.Minute
	getAttempts     = 3
)

var (
	// ErrNotFound reports a missing branch, ref, release or commit.
	ErrNotFound = errors.New("not found")
	// ErrAlreadyExists reports a ref or release that another request created.
	ErrAlreadyExists = errors.New("already exists")

	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	objectIDPattern   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Options configures a Client.
type Options struct {
	// HTTPClient defaults to a client with a two-minute timeout.
	HTTPClient *http.Client
	// Backoff returns the pause before retry attempt n (1-based) of a
	// read; nil selects one second times n.
	Backoff func(n int) time.Duration
	// APIURL is the REST base URL; empty selects DefaultAPIURL.
	APIURL string
	// GraphQLURL is the GraphQL endpoint; empty selects APIURL + "/graphql".
	GraphQLURL string
	// Token authenticates every request; empty sends none.
	Token string
	// Repository is owner/name.
	Repository string
}

// Client talks to one repository.
type Client struct {
	http    *http.Client
	backoff func(n int) time.Duration
	restURL *url.URL
	graphql *url.URL
	token   string
	owner   string
	name    string
}

// New validates opts. Endpoints must use https unless they are on a loopback
// address, so the token never travels in clear text to another host.
func New(opts Options) (*Client, error) {
	if !repositoryPattern.MatchString(opts.Repository) || strings.Contains(opts.Repository, "..") {
		return nil, fault.New(fault.Usage, "repository %q must be owner/name", opts.Repository)
	}

	apiURL := strings.TrimSuffix(opts.APIURL, "/")
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}

	rest, err := endpoint(apiURL)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "GitHub API URL")
	}

	graphqlURL := opts.GraphQLURL
	if graphqlURL == "" {
		graphqlURL = apiURL + "/graphql"
	}

	graphql, err := endpoint(graphqlURL)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "GitHub GraphQL URL")
	}

	c := &Client{http: opts.HTTPClient, backoff: opts.Backoff, restURL: rest, graphql: graphql, token: opts.Token}
	c.owner, c.name, _ = strings.Cut(opts.Repository, "/")

	if c.http == nil {
		c.http = &http.Client{Timeout: requestTimeout}
	}

	if c.backoff == nil {
		c.backoff = func(n int) time.Duration { return time.Duration(n) * time.Second }
	}

	return c, nil
}

// Repository returns owner/name.
func (c *Client) Repository() string {
	return c.owner + "/" + c.name
}

// ValidObjectID reports whether s is a full lowercase SHA-1 object ID.
func ValidObjectID(s string) bool {
	return objectIDPattern.MatchString(s)
}

// BranchHead returns the commit a branch points to, or ErrNotFound.
func (c *Client) BranchHead(ctx context.Context, branch string) (string, error) {
	const query = `query BranchHead($owner: String!, $name: String!, $ref: String!) {
  repository(owner: $owner, name: $name) {
    ref(qualifiedName: $ref) { target { oid } }
  }
}`

	var data struct {
		Repository struct {
			Ref *struct {
				Target struct {
					OID string `json:"oid"`
				} `json:"target"`
			} `json:"ref"`
		} `json:"repository"`
	}

	if err := c.query(ctx, query, c.vars("ref", "refs/heads/"+branch), &data); err != nil {
		return "", fault.Wrap(fault.Registry, err, "read the head of %s", branch)
	}

	if data.Repository.Ref == nil {
		return "", fault.Wrap(fault.NotFound, ErrNotFound, "branch %s", branch)
	}

	return checkedOID(data.Repository.Ref.Target.OID, "head of "+branch)
}

// BlobID returns the blob ID of path at commit; ok is false when the commit
// has no file there.
func (c *Client) BlobID(ctx context.Context, commit, path string) (oid string, ok bool, err error) {
	const query = `query BlobAt($owner: String!, $name: String!, $expression: String!) {
  repository(owner: $owner, name: $name) {
    object(expression: $expression) { __typename ... on Blob { oid } }
  }
}`

	var data struct {
		Repository struct {
			Object *struct {
				Type string `json:"__typename"`
				OID  string `json:"oid"`
			} `json:"object"`
		} `json:"repository"`
	}

	if err := c.query(ctx, query, c.vars("expression", commit+":"+path), &data); err != nil {
		return "", false, fault.Wrap(fault.Registry, err, "read %s at %s", path, commit)
	}

	object := data.Repository.Object

	switch {
	case object == nil:
		return "", false, nil
	case object.Type != "Blob":
		return "", false, fault.New(fault.Registry, "%s at %s is a %s, not a file", path, commit, object.Type)
	}

	oid, err = checkedOID(object.OID, path+" at "+commit)

	return oid, err == nil, err
}

// LastCommitTouching returns the newest commit reachable from commit that
// changed path.
func (c *Client) LastCommitTouching(ctx context.Context, commit, path string) (string, error) {
	const query = `query PathHistory($owner: String!, $name: String!, $oid: GitObjectID!, $path: String!) {
  repository(owner: $owner, name: $name) {
    object(oid: $oid) { ... on Commit { history(first: 1, path: $path) { nodes { oid } } } }
  }
}`

	var data struct {
		Repository struct {
			Object *struct {
				History struct {
					Nodes []struct {
						OID string `json:"oid"`
					} `json:"nodes"`
				} `json:"history"`
			} `json:"object"`
		} `json:"repository"`
	}

	if err := c.query(ctx, query, c.vars("oid", commit, "path", path), &data); err != nil {
		return "", fault.Wrap(fault.Registry, err, "read the history of %s", path)
	}

	if data.Repository.Object == nil || len(data.Repository.Object.History.Nodes) == 0 {
		return "", fault.Wrap(fault.NotFound, ErrNotFound, "no commit at or before %s changed %s", commit, path)
	}

	return checkedOID(data.Repository.Object.History.Nodes[0].OID, "history of "+path)
}

// FileAddition is one file a commit writes.
type FileAddition struct {
	Path     string
	Contents []byte
}

// CommitInput describes a commit created by CreateCommitOnBranch.
type CommitInput struct {
	Branch          string
	ExpectedHeadOID string
	Headline        string
	Body            string
	Files           []FileAddition
}

// CreateCommitOnBranch creates one commit through the GraphQL mutation of
// the same name, which GitHub signs, and returns its ID. It is never retried
// here: a lost response is indistinguishable from a failure, so the caller
// must look at the branch before trying again.
func (c *Client) CreateCommitOnBranch(ctx context.Context, in CommitInput) (string, error) {
	const mutation = `mutation CreateCommit($input: CreateCommitOnBranchInput!) {
  createCommitOnBranch(input: $input) { commit { oid } }
}`

	type addition struct {
		Path     string `json:"path"`
		Contents string `json:"contents"`
	}

	additions := make([]addition, 0, len(in.Files))
	for _, f := range in.Files {
		additions = append(additions, addition{Path: f.Path, Contents: base64.StdEncoding.EncodeToString(f.Contents)})
	}

	message := map[string]string{"headline": in.Headline}
	if in.Body != "" {
		message["body"] = in.Body
	}

	input := map[string]any{
		"branch":          map[string]string{"repositoryNameWithOwner": c.Repository(), "branchName": in.Branch},
		"expectedHeadOid": in.ExpectedHeadOID,
		"message":         message,
		"fileChanges":     map[string]any{"additions": additions},
	}

	var data struct {
		CreateCommitOnBranch *struct {
			Commit struct {
				OID string `json:"oid"`
			} `json:"commit"`
		} `json:"createCommitOnBranch"`
	}

	if err := c.graphqlOnce(ctx, mutation, map[string]any{"input": input}, &data); err != nil {
		return "", fault.Wrap(fault.Registry, err, "create a commit on %s", in.Branch)
	}

	if data.CreateCommitOnBranch == nil {
		return "", fault.New(fault.Registry, "create a commit on %s: the response has no commit", in.Branch)
	}

	return checkedOID(data.CreateCommitOnBranch.Commit.OID, "new commit")
}

// TagCommit returns the commit a tag points to, following annotated tag
// objects, or ErrNotFound.
func (c *Client) TagCommit(ctx context.Context, tag string) (string, error) {
	var ref struct {
		Object gitObject `json:"object"`
	}

	if err := c.rest(ctx, http.MethodGet, nil, &ref, "git", "ref", "tags", tag); err != nil {
		return "", fault.Wrap(fault.Registry, err, "read tag %s", tag)
	}

	object := ref.Object

	for range 8 {
		switch object.Type {
		case "commit":
			return checkedOID(object.SHA, "tag "+tag)
		case "tag":
			if !ValidObjectID(object.SHA) {
				return "", fault.New(fault.Registry, "tag %s: invalid tag object %q", tag, object.SHA)
			}

			var annotated struct {
				Object gitObject `json:"object"`
			}

			if err := c.rest(ctx, http.MethodGet, nil, &annotated, "git", "tags", object.SHA); err != nil {
				return "", fault.Wrap(fault.Registry, err, "read tag object %s", object.SHA)
			}

			object = annotated.Object
		default:
			return "", fault.New(fault.Registry, "tag %s points to a %s, not a commit", tag, object.Type)
		}
	}

	return "", fault.New(fault.Registry, "tag %s: too many nested tag objects", tag)
}

// CreateTag creates the lightweight tag ref refs/tags/<tag> on commit. A ref
// that already exists is ErrAlreadyExists, wherever it points.
func (c *Client) CreateTag(ctx context.Context, tag, commit string) error {
	body := map[string]string{"ref": "refs/tags/" + tag, "sha": commit}

	if err := c.rest(ctx, http.MethodPost, body, nil, "git", "refs"); err != nil {
		return fault.Wrap(fault.Registry, err, "create tag %s", tag)
	}

	return nil
}

// Release is the part of a GitHub release the bot reads.
type Release struct {
	HTMLURL    string `json:"html_url"`
	TagName    string `json:"tag_name"`
	Name       string `json:"name"`
	ID         int64  `json:"id"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// ReleaseByTag returns the published release of tag, or ErrNotFound.
func (c *Client) ReleaseByTag(ctx context.Context, tag string) (*Release, error) {
	var r Release

	if err := c.rest(ctx, http.MethodGet, nil, &r, "releases", "tags", tag); err != nil {
		return nil, fault.Wrap(fault.Registry, err, "read the release of %s", tag)
	}

	return &r, nil
}

// NewRelease describes a release to create.
type NewRelease struct {
	Tag  string
	Name string
	Body string
}

// CreateRelease publishes a release of an existing tag that is never marked
// as the repository's latest release. A release that already exists for
// the tag is ErrAlreadyExists.
func (c *Client) CreateRelease(ctx context.Context, in NewRelease) (*Release, error) {
	body := map[string]any{
		"tag_name":    in.Tag,
		"name":        in.Name,
		"body":        in.Body,
		"draft":       false,
		"prerelease":  false,
		"make_latest": "false",
	}

	var r Release

	if err := c.rest(ctx, http.MethodPost, body, &r, "releases"); err != nil {
		return nil, fault.Wrap(fault.Registry, err, "create the release of %s", in.Tag)
	}

	return &r, nil
}

type gitObject struct {
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

func (c *Client) vars(pairs ...string) map[string]any {
	v := map[string]any{"owner": c.owner, "name": c.name}
	for i := 0; i+1 < len(pairs); i += 2 {
		v[pairs[i]] = pairs[i+1]
	}

	return v
}

// query runs a read-only GraphQL query, retrying transient failures.
func (c *Client) query(ctx context.Context, query string, variables map[string]any, out any) error {
	return c.retry(ctx, func() error { return c.graphqlOnce(ctx, query, variables, out) })
}

func (c *Client) graphqlOnce(ctx context.Context, query string, variables map[string]any, out any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return fmt.Errorf("encode the GraphQL request: %w", err)
	}

	status, body, err := c.send(ctx, http.MethodPost, c.graphql.String(), payload)
	if err != nil {
		return err
	}

	if status != http.StatusOK {
		return apiError(status, body)
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode the GraphQL response: %w", err)
	}

	if len(envelope.Errors) > 0 {
		messages := make([]string, 0, len(envelope.Errors))
		for _, e := range envelope.Errors {
			messages = append(messages, strings.TrimSpace(e.Type+" "+e.Message))
		}

		return fmt.Errorf("GraphQL: %s", strings.Join(messages, "; "))
	}

	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return errors.New("GraphQL: the response has no data")
	}

	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("decode the GraphQL data: %w", err)
	}

	return nil
}

// rest calls a repository endpoint. GET requests are retried; a POST is
// safe to send once more because both creations it is used for report an
// existing object as ErrAlreadyExists.
func (c *Client) rest(ctx context.Context, method string, in, out any, segments ...string) error {
	for _, s := range segments {
		if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/?#%\\") {
			return fault.New(fault.Usage, "invalid path segment %q", s)
		}
	}

	target := c.restURL.JoinPath(append([]string{"repos", c.owner, c.name}, segments...)...)
	path := strings.Join(segments, "/")

	var payload []byte

	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return fmt.Errorf("encode the request: %w", err)
		}
	}

	return c.retry(ctx, func() error {
		status, body, err := c.send(ctx, method, target.String(), payload)
		if err != nil {
			return err
		}

		switch {
		case status == http.StatusNotFound:
			return permanent{ErrNotFound}
		case status == http.StatusUnprocessableEntity && strings.Contains(string(body), "already_exists"),
			status == http.StatusUnprocessableEntity && strings.Contains(string(body), "Reference already exists"):
			return permanent{ErrAlreadyExists}
		case status < 200 || status > 299:
			return apiError(status, body)
		case out == nil:
			return nil
		}

		if err := json.Unmarshal(body, out); err != nil {
			return permanent{fmt.Errorf("decode the response of %s %s: %w", method, path, err)}
		}

		return nil
	})
}

func (c *Client) send(ctx context.Context, method, target string, payload []byte) (int, []byte, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return 0, nil, permanent{fmt.Errorf("build the request: %w", err)}
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Github-Api-Version", APIVersion)

	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, transient{fmt.Errorf("%s %s: %w", method, redactURL(target), stripURL(err))}
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		return 0, nil, transient{fmt.Errorf("read the response of %s %s: %w", method, redactURL(target), err)}
	}

	if len(body) > maxResponseSize {
		return 0, nil, permanent{fmt.Errorf("the response of %s %s is larger than %d bytes", method, redactURL(target), maxResponseSize)}
	}

	return resp.StatusCode, body, nil
}

// retry runs attempt up to getAttempts times while it fails transiently.
func (c *Client) retry(ctx context.Context, attempt func() error) error {
	var err error

	for n := 1; ; n++ {
		err = attempt()

		if _, stop := errors.AsType[permanent](err); err == nil || stop || !isTransient(err) || n == getAttempts {
			break
		}

		timer := time.NewTimer(c.backoff(n))
		select {
		case <-ctx.Done():
			timer.Stop()

			return fmt.Errorf("%w (after: %w)", ctx.Err(), err)
		case <-timer.C:
		}
	}

	if p, ok := errors.AsType[permanent](err); ok {
		return p.err
	}

	if t, ok := errors.AsType[transient](err); ok {
		return t.err
	}

	return err
}

func isTransient(err error) bool {
	if _, ok := errors.AsType[transient](err); ok {
		return true
	}

	s, ok := errors.AsType[*statusError](err)

	return ok && (s.status == http.StatusTooManyRequests || s.status >= 500)
}

type permanent struct{ err error }

func (p permanent) Error() string { return p.err.Error() }
func (p permanent) Unwrap() error { return p.err }

type transient struct{ err error }

func (t transient) Error() string { return t.err.Error() }
func (t transient) Unwrap() error { return t.err }

type statusError struct {
	message string
	status  int
}

func (e *statusError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("HTTP %d", e.status)
	}

	return fmt.Sprintf("HTTP %d: %s", e.status, e.message)
}

func apiError(status int, body []byte) error {
	var parsed struct {
		Message string `json:"message"`
	}

	message := ""
	if json.Unmarshal(body, &parsed) == nil {
		message = parsed.Message
	}

	return &statusError{status: status, message: printable(message)}
}

// printable keeps error messages from the API on one short line.
func printable(s string) string {
	const maxLen = 300

	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}

		return r
	}, s)

	if len(s) > maxLen {
		s = s[:maxLen] + "..."
	}

	return s
}

func checkedOID(oid, what string) (string, error) {
	if !ValidObjectID(oid) {
		return "", fault.New(fault.Registry, "%s: the API returned an invalid object ID %q", what, printable(oid))
	}

	return oid, nil
}

func endpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", raw, err)
	}

	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" {
		return nil, fmt.Errorf("%q must be a plain http(s) URL without credentials, query or fragment", raw)
	}

	switch u.Scheme {
	case "https":
		return u, nil
	case "http":
		if ip := net.ParseIP(u.Hostname()); (ip != nil && ip.IsLoopback()) || u.Hostname() == "localhost" {
			return u, nil
		}

		return nil, fmt.Errorf("%q must use https unless it is a loopback address", raw)
	default:
		return nil, fmt.Errorf("%q must be an http(s) URL", raw)
	}
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<url>"
	}

	u.User = nil
	u.RawQuery = ""

	return u.String()
}

// stripURL drops the *url.Error wrapper, whose message repeats the URL.
func stripURL(err error) error {
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		return urlErr.Err
	}

	return err
}

// BlobID returns the Git blob object ID of contents, which is what the API
// reports for a file, so contents can be compared without downloading them.
func BlobID(contents []byte) string {
	// bearer:disable go_gosec_crypto_weak_crypto
	// Git object IDs are SHA-1 by definition; this is an identity, not a security check.
	h := sha1.New() //nolint:gosec // Git object IDs are SHA-1 by definition; this is an identity, not a security check
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(contents))
	_, _ = h.Write(contents)

	// bearer:disable go_lang_weak_hash_sha1
	// The Git blob object ID, compared with the ID GitHub reports; not a security check.
	return hex.EncodeToString(h.Sum(nil))
}
