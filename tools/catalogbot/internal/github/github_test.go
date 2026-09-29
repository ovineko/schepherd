package github_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/tools/catalogbot/internal/github"
	"github.com/ovineko/schepherd/tools/catalogbot/internal/github/githubtest"
)

const (
	repo  = "owner/repo"
	token = "ghs_secret-token-value"
)

func newClient(t *testing.T, srv *githubtest.Server, tok string) *github.Client {
	t.Helper()

	c, err := github.New(github.Options{
		APIURL: srv.URL(), Token: tok, Repository: repo, Backoff: func(int) time.Duration { return time.Millisecond },
	})
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func TestBlobIDIsTheGitObjectID(t *testing.T) {
	// Values from `git hash-object --stdin`.
	cases := map[string]string{
		"":        "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391",
		"hello":   "b6fc4c620b67d95f953a5c1c1230aaab5db5a1b0",
		"hello\n": "ce013625030ba8dba906f756967f9e9ca394464a",
	}

	for contents, want := range cases {
		if got := github.BlobID([]byte(contents)); got != want {
			t.Errorf("BlobID(%q) = %s, want %s", contents, got, want)
		}
	}
}

func TestNewValidatesEndpointsAndRepository(t *testing.T) {
	bad := []github.Options{
		{Repository: "owner"},
		{Repository: "owner/repo/extra"},
		{Repository: "../repo"},
		{Repository: repo, APIURL: "http://api.example.com"},
		{Repository: repo, APIURL: "https://user:pass@api.example.com"},
		{Repository: repo, APIURL: "ftp://api.example.com"},
		{Repository: repo, GraphQLURL: "http://api.example.com/graphql"},
	}

	for _, opts := range bad {
		if _, err := github.New(opts); fault.KindOf(err) != fault.Usage {
			t.Errorf("New(%+v) = %v, want a usage error", opts, err)
		}
	}

	for _, opts := range []github.Options{
		{Repository: repo},
		{Repository: repo, APIURL: "http://127.0.0.1:8080"},
		{Repository: repo, APIURL: "https://github.example/api/v3", GraphQLURL: "https://github.example/api/graphql"},
	} {
		if _, err := github.New(opts); err != nil {
			t.Errorf("New(%+v) = %v", opts, err)
		}
	}
}

func TestBranchBlobAndHistoryQueries(t *testing.T) {
	srv := githubtest.New(t, repo, token)
	first := srv.Push("main", "first", map[string][]byte{"catalog/state.json": []byte("one"), "README.md": []byte("r")})
	second := srv.Push("main", "second", map[string][]byte{"README.md": []byte("r2")})
	c := newClient(t, srv, token)

	head, err := c.BranchHead(t.Context(), "main")
	if err != nil || head != second {
		t.Fatalf("BranchHead = %s, %v; want %s", head, err, second)
	}

	if _, err := c.BranchHead(t.Context(), "missing"); !errors.Is(err, github.ErrNotFound) || fault.KindOf(err) != fault.NotFound {
		t.Errorf("BranchHead(missing) = %v, want a not-found error", err)
	}

	oid, ok, err := c.BlobID(t.Context(), second, "catalog/state.json")
	if err != nil || !ok || oid != github.BlobID([]byte("one")) {
		t.Errorf("BlobID = %s, %v, %v", oid, ok, err)
	}

	if _, ok, err := c.BlobID(t.Context(), second, "absent.json"); ok || err != nil {
		t.Errorf("BlobID(absent) = %v, %v; want not found without error", ok, err)
	}

	if _, _, err := c.BlobID(t.Context(), second, "catalog"); err == nil || !strings.Contains(err.Error(), "not a file") {
		t.Errorf("BlobID(directory) = %v, want an error", err)
	}

	last, err := c.LastCommitTouching(t.Context(), second, "catalog/state.json")
	if err != nil || last != first {
		t.Errorf("LastCommitTouching = %s, %v; want %s", last, err, first)
	}

	if _, err := c.LastCommitTouching(t.Context(), second, "absent.json"); !errors.Is(err, github.ErrNotFound) {
		t.Errorf("LastCommitTouching(absent) = %v, want ErrNotFound", err)
	}
}

func TestCreateCommitOnBranch(t *testing.T) {
	srv := githubtest.New(t, repo, token)
	base := srv.Push("main", "base", map[string][]byte{"a.txt": []byte("a")})
	c := newClient(t, srv, token)

	binary := []byte{0, 1, 2, 0xff, '\n'}

	oid, err := c.CreateCommitOnBranch(t.Context(), github.CommitInput{
		Branch: "main", ExpectedHeadOID: base, Headline: "chore: one", Body: "details",
		Files: []github.FileAddition{{Path: "dir/b.bin", Contents: binary}},
	})
	if err != nil {
		t.Fatal(err)
	}

	commit, ok := srv.CommitByID(oid)
	if !ok || commit.Parent != base || commit.Headline != "chore: one" || commit.Body != "details" ||
		string(commit.Files["dir/b.bin"]) != string(binary) || string(commit.Files["a.txt"]) != "a" {
		t.Errorf("commit = %+v", commit)
	}

	_, err = c.CreateCommitOnBranch(t.Context(), github.CommitInput{
		Branch: "main", ExpectedHeadOID: base, Headline: "stale", Files: []github.FileAddition{{Path: "c", Contents: []byte("c")}},
	})
	if err == nil || !strings.Contains(err.Error(), "STALE_DATA") || fault.KindOf(err) != fault.Registry {
		t.Errorf("a stale expected head = %v, want a registry error naming STALE_DATA", err)
	}
}

func TestMutationIsNeverRetried(t *testing.T) {
	srv := githubtest.New(t, repo, token)
	base := srv.Push("main", "base", map[string][]byte{"a.txt": []byte("a")})
	c := newClient(t, srv, token)
	srv.Fail("CreateCommit", http.StatusBadGateway)

	_, err := c.CreateCommitOnBranch(t.Context(), github.CommitInput{
		Branch: "main", ExpectedHeadOID: base, Headline: "h", Files: []github.FileAddition{{Path: "b", Contents: []byte("b")}},
	})
	if err == nil {
		t.Fatal("a failed mutation succeeded")
	}

	if n := countOf(srv.Requests(), "CreateCommit"); n != 1 {
		t.Errorf("the mutation was sent %d times, want once", n)
	}
}

func TestReadsAreRetriedOnTransientFailures(t *testing.T) {
	srv := githubtest.New(t, repo, token)
	head := srv.Push("main", "base", map[string][]byte{"a.txt": []byte("a")})
	c := newClient(t, srv, token)

	srv.Fail("BranchHead", http.StatusBadGateway, http.StatusServiceUnavailable)

	if got, err := c.BranchHead(t.Context(), "main"); err != nil || got != head {
		t.Fatalf("BranchHead after two transient failures = %s, %v", got, err)
	}

	srv.Fail("getRelease", http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway)

	if _, err := c.ReleaseByTag(t.Context(), "catalog-20260924.0905"); err == nil || fault.KindOf(err) != fault.Registry {
		t.Errorf("ReleaseByTag after three failures = %v, want a registry error", err)
	}

	srv.Fail("getTag", http.StatusForbidden)

	if _, err := c.TagCommit(t.Context(), "catalog-20260924.0905"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("TagCommit after a 403 = %v", err)
	}

	if n := countOf(srv.Requests(), "getTag"); n != 1 {
		t.Errorf("a 403 was retried: %d requests", n)
	}
}

func TestTagsAndReleases(t *testing.T) {
	srv := githubtest.New(t, repo, token)
	commit := srv.Push("main", "base", map[string][]byte{"a.txt": []byte("a")})
	c := newClient(t, srv, token)

	const tag = "catalog-20260924.0905"

	if _, err := c.TagCommit(t.Context(), tag); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("TagCommit(missing) = %v", err)
	}

	if err := c.CreateTag(t.Context(), tag, commit); err != nil {
		t.Fatal(err)
	}

	if err := c.CreateTag(t.Context(), tag, commit); !errors.Is(err, github.ErrAlreadyExists) {
		t.Errorf("creating an existing tag = %v, want ErrAlreadyExists", err)
	}

	if got, err := c.TagCommit(t.Context(), tag); err != nil || got != commit {
		t.Errorf("TagCommit = %s, %v", got, err)
	}

	srv.SetTag("annotated", commit, true)

	if got, err := c.TagCommit(t.Context(), "annotated"); err != nil || got != commit {
		t.Errorf("TagCommit(annotated) = %s, %v", got, err)
	}

	rel, err := c.CreateRelease(t.Context(), github.NewRelease{Tag: tag, Name: "Schemas 20260924.0905", Body: "notes"})
	if err != nil || !strings.HasSuffix(rel.HTMLURL, "/releases/tag/"+tag) {
		t.Fatalf("CreateRelease = %+v, %v", rel, err)
	}

	stored, _ := srv.ReleaseOf(tag)
	if stored.MakeLatest != "false" || stored.Draft || stored.Prerelease || stored.Body != "notes" {
		t.Errorf("release = %+v, want a published release that is not marked latest", stored)
	}

	if _, err := c.CreateRelease(t.Context(), github.NewRelease{Tag: tag, Name: "x"}); !errors.Is(err, github.ErrAlreadyExists) {
		t.Errorf("a second release = %v, want ErrAlreadyExists", err)
	}

	if got, err := c.ReleaseByTag(t.Context(), tag); err != nil || got.TagName != tag {
		t.Errorf("ReleaseByTag = %+v, %v", got, err)
	}
}

func TestEveryRequestCarriesTheTokenAndNoErrorQuotesIt(t *testing.T) {
	var sawToken atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+token {
			sawToken.Store(true)
		}

		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	t.Cleanup(srv.Close)

	c, err := github.New(github.Options{APIURL: srv.URL, Token: token, Repository: repo})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.BranchHead(t.Context(), "main")
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), token) {
		t.Errorf("error = %v", err)
	}

	if !sawToken.Load() {
		t.Error("the token was not sent")
	}

	wrong := githubtest.New(t, repo, token)
	wrong.Push("main", "base", map[string][]byte{"a": []byte("a")})

	if _, err := newClient(t, wrong, "other").BranchHead(t.Context(), "main"); err == nil || strings.Contains(err.Error(), "other") {
		t.Errorf("a wrong token = %v", err)
	}
}

func countOf(requests []string, operation string) int {
	return len(slices.DeleteFunc(slices.Clone(requests), func(r string) bool { return r != operation }))
}
