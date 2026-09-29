// Package githubtest is an in-memory stand-in for the parts of the GitHub
// API the catalog bot uses: the GraphQL queries and the createCommitOnBranch
// mutation of package github, tag refs and releases. It keeps a small commit
// graph with file contents so tests can check what was committed, tagged and
// released. Only tests import it.
package githubtest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ovineko/schepherd/tools/catalogbot/internal/github"
)

// Commit is one commit of the fake repository.
type Commit struct {
	Files    map[string][]byte
	OID      string
	Parent   string
	Headline string
	Body     string
}

// Release is a release of the fake repository.
type Release struct {
	Tag        string
	Name       string
	Body       string
	MakeLatest string
	ID         int64
	Draft      bool
	Prerelease bool
}

type tagRef struct {
	target    string
	annotated bool
}

// Server is the fake API. All methods are safe for concurrent use.
type Server struct {
	commits  map[string]*Commit
	branches map[string]string
	tags     map[string]tagRef
	releases map[string]*Release
	failures map[string][]int
	// BeforeMutation runs, without the lock, before a commit mutation is
	// applied; tests use it to move the branch concurrently.
	BeforeMutation func()
	srv            *httptest.Server
	owner          string
	name           string
	token          string
	requests       []string
	loseResponses  int
	mu             sync.Mutex
}

var operationPattern = regexp.MustCompile(`^\s*(?:query|mutation)\s+([A-Za-z]+)`)

// New starts a server for repository owner/name that requires token.
func New(tb testing.TB, repository, token string) *Server {
	tb.Helper()

	owner, name, _ := strings.Cut(repository, "/")
	s := &Server{
		commits: map[string]*Commit{}, branches: map[string]string{}, tags: map[string]tagRef{},
		releases: map[string]*Release{}, failures: map[string][]int{}, owner: owner, name: name, token: token,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", s.graphql)
	mux.HandleFunc("GET /repos/{owner}/{name}/git/ref/tags/{tag}", s.scoped(s.getTag))
	mux.HandleFunc("GET /repos/{owner}/{name}/git/tags/{sha}", s.scoped(s.getTagObject))
	mux.HandleFunc("POST /repos/{owner}/{name}/git/refs", s.scoped(s.createRef))
	mux.HandleFunc("GET /repos/{owner}/{name}/releases/tags/{tag}", s.scoped(s.getRelease))
	mux.HandleFunc("POST /repos/{owner}/{name}/releases", s.scoped(s.createRelease))

	s.srv = httptest.NewServer(s.authorize(mux))
	tb.Cleanup(s.srv.Close)

	return s
}

// URL is the API base URL; GraphQL is served at URL + "/graphql".
func (s *Server) URL() string {
	return s.srv.URL
}

// Push adds a commit to branch with files changed or added on top of the
// current head, creating the branch when it is missing.
func (s *Server) Push(branch, headline string, files map[string][]byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.pushLocked(branch, headline, "", files)
}

// Head returns the commit branch points to.
func (s *Server) Head(branch string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.branches[branch]
}

// CommitByID returns a copy of a commit.
func (s *Server) CommitByID(oid string) (Commit, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.commits[oid]
	if !ok {
		return Commit{}, false
	}

	copied := *c
	copied.Files = maps.Clone(c.Files)

	return copied, true
}

// SetTag points tag at target; annotated tags go through a tag object.
func (s *Server) SetTag(tag, target string, annotated bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.tags[tag] = tagRef{target: target, annotated: annotated}
}

// TagTarget returns the commit tag points to.
func (s *Server) TagTarget(tag string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ref, ok := s.tags[tag]

	return ref.target, ok
}

// AddRelease publishes a release of tag.
func (s *Server) AddRelease(tag, name, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.releases[tag] = &Release{Tag: tag, Name: name, Body: body, ID: int64(len(s.releases) + 1), MakeLatest: "true"}
}

// ReleaseOf returns a copy of the release of tag.
func (s *Server) ReleaseOf(tag string) (Release, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.releases[tag]
	if !ok {
		return Release{}, false
	}

	return *r, true
}

// Fail makes the next calls of operation answer with the given HTTP
// statuses, one per call. Operations are GraphQL operation names and
// "getTag", "getTagObject", "createRef", "getRelease", "createRelease".
func (s *Server) Fail(operation string, statuses ...int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.failures[operation] = append(s.failures[operation], statuses...)
}

// LoseMutationResponses applies the next n commit mutations but answers them
// with HTTP 502, as when a response is lost on the way back.
func (s *Server) LoseMutationResponses(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.loseResponses = n
}

// Requests returns the operations served so far, in order.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.requests)
}

func (s *Server) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" && r.Header.Get("Authorization") != "Bearer "+s.token {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})

			return
		}

		if r.Header.Get("X-Github-Api-Version") != github.APIVersion || r.Header.Get("Accept") != "application/vnd.github+json" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "missing API version or Accept header"})

			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) scoped(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("owner") != s.owner || r.PathValue("name") != s.name {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})

			return
		}

		next(w, r)
	}
}

// served records operation and reports an injected failure status, or 0.
func (s *Server) served(operation string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.requests = append(s.requests, operation)

	if queue := s.failures[operation]; len(queue) > 0 {
		s.failures[operation] = queue[1:]

		return queue[0]
	}

	return 0
}

type graphQLRequest struct {
	Variables map[string]json.RawMessage `json:"variables"`
	Query     string                     `json:"query"`
}

func (s *Server) graphql(w http.ResponseWriter, r *http.Request) {
	var req graphQLRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})

		return
	}

	m := operationPattern.FindStringSubmatch(req.Query)
	if m == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "unnamed operation"})

		return
	}

	operation := m[1]
	if status := s.served(operation); status != 0 {
		writeJSON(w, status, map[string]string{"message": "injected failure"})

		return
	}

	if operation == "CreateCommit" {
		s.createCommit(w, req)

		return
	}

	var owner, name string
	_ = json.Unmarshal(req.Variables["owner"], &owner)
	_ = json.Unmarshal(req.Variables["name"], &name)

	if owner != s.owner || name != s.name {
		writeGraphQLError(w, "NOT_FOUND", "Could not resolve to a Repository with the name '"+owner+"/"+name+"'.")

		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch operation {
	case "BranchHead":
		var ref string
		_ = json.Unmarshal(req.Variables["ref"], &ref)

		var target any
		if head, ok := s.branches[strings.TrimPrefix(ref, "refs/heads/")]; ok && strings.HasPrefix(ref, "refs/heads/") {
			target = map[string]any{"target": map[string]string{"oid": head}}
		}

		writeData(w, map[string]any{"repository": map[string]any{"ref": target}})
	case "BlobAt":
		var expression string
		_ = json.Unmarshal(req.Variables["expression"], &expression)
		writeData(w, map[string]any{"repository": map[string]any{"object": s.objectLocked(expression)}})
	case "PathHistory":
		var oid, path string
		_ = json.Unmarshal(req.Variables["oid"], &oid)
		_ = json.Unmarshal(req.Variables["path"], &path)

		var object any
		if _, ok := s.commits[oid]; ok {
			nodes := []map[string]string{}
			if last := s.lastTouchingLocked(oid, path); last != "" {
				nodes = append(nodes, map[string]string{"oid": last})
			}

			object = map[string]any{"history": map[string]any{"nodes": nodes}}
		}

		writeData(w, map[string]any{"repository": map[string]any{"object": object}})
	default:
		writeGraphQLError(w, "UNKNOWN", "unknown operation "+operation)
	}
}

func (s *Server) objectLocked(expression string) any {
	oid, path, ok := strings.Cut(expression, ":")
	commit, known := s.commits[oid]

	if !ok || !known {
		return nil
	}

	if contents, ok := commit.Files[path]; ok {
		return map[string]string{"__typename": "Blob", "oid": github.BlobID(contents)}
	}

	for p := range commit.Files {
		if strings.HasPrefix(p, path+"/") {
			return map[string]string{"__typename": "Tree"}
		}
	}

	return nil
}

func (s *Server) lastTouchingLocked(oid, path string) string {
	for c := s.commits[oid]; c != nil; c = s.commits[c.Parent] {
		contents, ok := c.Files[path]

		var parentContents []byte

		parentOK := false
		if parent := s.commits[c.Parent]; parent != nil {
			parentContents, parentOK = parent.Files[path]
		}

		if ok != parentOK || string(contents) != string(parentContents) {
			return c.OID
		}
	}

	return ""
}

type commitInput struct {
	Branch struct {
		RepositoryNameWithOwner string `json:"repositoryNameWithOwner"`
		BranchName              string `json:"branchName"`
	} `json:"branch"`
	Message struct {
		Headline string `json:"headline"`
		Body     string `json:"body"`
	} `json:"message"`
	ExpectedHeadOID string `json:"expectedHeadOid"`
	FileChanges     struct {
		Additions []struct {
			Path     string `json:"path"`
			Contents string `json:"contents"`
		} `json:"additions"`
	} `json:"fileChanges"`
}

func (s *Server) createCommit(w http.ResponseWriter, req graphQLRequest) {
	var in commitInput
	if err := json.Unmarshal(req.Variables["input"], &in); err != nil {
		writeGraphQLError(w, "INVALID", err.Error())

		return
	}

	if s.BeforeMutation != nil {
		s.BeforeMutation()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	head, ok := s.branches[in.Branch.BranchName]

	switch {
	case in.Branch.RepositoryNameWithOwner != s.owner+"/"+s.name:
		writeGraphQLError(w, "NOT_FOUND", "unknown repository "+in.Branch.RepositoryNameWithOwner)

		return
	case !ok:
		writeGraphQLError(w, "NOT_FOUND", "unknown branch "+in.Branch.BranchName)

		return
	case in.ExpectedHeadOID != head:
		writeGraphQLError(w, "STALE_DATA", fmt.Sprintf("Expected branch to point to %q but it did not. Pull and try again.", in.ExpectedHeadOID))

		return
	case in.Message.Headline == "" || len(in.FileChanges.Additions) == 0:
		writeGraphQLError(w, "INVALID", "a headline and at least one file are required")

		return
	}

	files := map[string][]byte{}

	for _, a := range in.FileChanges.Additions {
		contents, err := base64.StdEncoding.DecodeString(a.Contents)
		if err != nil || a.Path == "" || strings.HasPrefix(a.Path, "/") {
			writeGraphQLError(w, "INVALID", "invalid file "+a.Path)

			return
		}

		files[a.Path] = contents
	}

	oid := s.pushLocked(in.Branch.BranchName, in.Message.Headline, in.Message.Body, files)

	if s.loseResponses > 0 {
		s.loseResponses--
		writeJSON(w, http.StatusBadGateway, map[string]string{"message": "Bad Gateway"})

		return
	}

	writeData(w, map[string]any{"createCommitOnBranch": map[string]any{"commit": map[string]string{"oid": oid}}})
}

func (s *Server) pushLocked(branch, headline, body string, files map[string][]byte) string {
	parent := s.branches[branch]

	merged := map[string][]byte{}
	if p := s.commits[parent]; p != nil {
		maps.Copy(merged, p.Files)
	}

	maps.Copy(merged, files)

	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%d\x00", parent, headline, body, len(s.commits))

	for _, path := range slices.Sorted(maps.Keys(merged)) {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00", path, github.BlobID(merged[path]))
	}

	oid := hex.EncodeToString(h.Sum(nil))[:40]
	s.commits[oid] = &Commit{OID: oid, Parent: parent, Files: merged, Headline: headline, Body: body}
	s.branches[branch] = oid

	return oid
}

func (s *Server) getTag(w http.ResponseWriter, r *http.Request) {
	if status := s.served("getTag"); status != 0 {
		writeJSON(w, status, map[string]string{"message": "injected failure"})

		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	ref, ok := s.tags[r.PathValue("tag")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})

		return
	}

	object := map[string]string{"type": "commit", "sha": ref.target}
	if ref.annotated {
		object = map[string]string{"type": "tag", "sha": tagObjectID(ref.target)}
	}

	writeJSON(w, http.StatusOK, map[string]any{"ref": "refs/tags/" + r.PathValue("tag"), "object": object})
}

func (s *Server) getTagObject(w http.ResponseWriter, r *http.Request) {
	if status := s.served("getTagObject"); status != 0 {
		writeJSON(w, status, map[string]string{"message": "injected failure"})

		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, ref := range s.tags {
		if ref.annotated && tagObjectID(ref.target) == r.PathValue("sha") {
			writeJSON(w, http.StatusOK, map[string]any{"object": map[string]string{"type": "commit", "sha": ref.target}})

			return
		}
	}

	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

func (s *Server) createRef(w http.ResponseWriter, r *http.Request) {
	if status := s.served("createRef"); status != 0 {
		writeJSON(w, status, map[string]string{"message": "injected failure"})

		return
	}

	var in struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}

	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})

		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tag, ok := strings.CutPrefix(in.Ref, "refs/tags/")

	switch {
	case !ok:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "only tags are supported here"})
	case s.commits[in.SHA] == nil:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Object does not exist"})
	case s.tags[tag] != tagRef{}:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Reference already exists"})
	default:
		s.tags[tag] = tagRef{target: in.SHA}
		writeJSON(w, http.StatusCreated, map[string]any{"ref": in.Ref, "object": map[string]string{"type": "commit", "sha": in.SHA}})
	}
}

func (s *Server) getRelease(w http.ResponseWriter, r *http.Request) {
	if status := s.served("getRelease"); status != 0 {
		writeJSON(w, status, map[string]string{"message": "injected failure"})

		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rel, ok := s.releases[r.PathValue("tag")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})

		return
	}

	writeJSON(w, http.StatusOK, s.releaseJSON(rel))
}

func (s *Server) createRelease(w http.ResponseWriter, r *http.Request) {
	if status := s.served("createRelease"); status != 0 {
		writeJSON(w, status, map[string]string{"message": "injected failure"})

		return
	}

	var in struct {
		MakeLatest *string `json:"make_latest"`
		Draft      *bool   `json:"draft"`
		Prerelease *bool   `json:"prerelease"`
		TagName    string  `json:"tag_name"`
		Name       string  `json:"name"`
		Body       string  `json:"body"`
	}

	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})

		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case s.releases[in.TagName] != nil:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"message": "Validation Failed",
			"errors":  []map[string]string{{"resource": "Release", "code": "already_exists", "field": "tag_name"}},
		})
	case s.tags[in.TagName] == tagRef{}:
		// GitHub would silently tag the default branch; the bot must never
		// rely on that, so the fake refuses.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "the tag does not exist"})
	case in.MakeLatest == nil || in.Draft == nil || in.Prerelease == nil:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "make_latest, draft and prerelease must be explicit"})
	default:
		rel := &Release{
			Tag: in.TagName, Name: in.Name, Body: in.Body, MakeLatest: *in.MakeLatest, Draft: *in.Draft,
			Prerelease: *in.Prerelease, ID: int64(len(s.releases) + 1),
		}
		s.releases[in.TagName] = rel
		writeJSON(w, http.StatusCreated, s.releaseJSON(rel))
	}
}

func (s *Server) releaseJSON(rel *Release) map[string]any {
	return map[string]any{
		"id": rel.ID, "tag_name": rel.Tag, "name": rel.Name, "draft": rel.Draft, "prerelease": rel.Prerelease,
		"html_url": fmt.Sprintf("https://github.example/%s/%s/releases/tag/%s", s.owner, s.name, rel.Tag),
	}
}

func tagObjectID(target string) string {
	sum := sha256.Sum256([]byte("tag object " + target))

	return hex.EncodeToString(sum[:])[:40]
}

func writeData(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

func writeGraphQLError(w http.ResponseWriter, kind, message string) {
	writeJSON(w, http.StatusOK, map[string]any{"data": nil, "errors": []map[string]string{{"type": kind, "message": message}}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		// bearer:disable go_lang_information_leakage
		// A test double: the only client is the test, which should see why the fake could not encode its reply.
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
