package bot_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/calver"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/state"
	"github.com/ovineko/schepherd/tools/catalogbot/internal/bot"
	"github.com/ovineko/schepherd/tools/catalogbot/internal/github"
	"github.com/ovineko/schepherd/tools/catalogbot/internal/github/githubtest"
)

const (
	repo      = "owner/repo"
	token     = "ghs_token"
	statePath = "catalog/state.json"
	srcPath   = "sources/schemastore.toml"
)

func setup(t *testing.T) (*githubtest.Server, *github.Client) {
	t.Helper()

	srv := githubtest.New(t, repo, token)

	c, err := github.New(github.Options{
		APIURL: srv.URL(), Token: token, Repository: repo, Backoff: func(int) time.Duration { return time.Millisecond },
	})
	if err != nil {
		t.Fatal(err)
	}

	return srv, c
}

func files(state, source string) []github.FileAddition {
	return []github.FileAddition{
		{Path: statePath, Contents: []byte(state)},
		{Path: srcPath, Contents: []byte(source)},
	}
}

func commitOpts(base string, f []github.FileAddition) bot.CommitOptions {
	return bot.CommitOptions{Branch: "main", Base: base, Headline: "chore(schemas): record catalog 20260924.0905", Body: "body", Files: f}
}

func TestCommitOnTheExpectedHead(t *testing.T) {
	srv, c := setup(t)
	base := srv.Push("main", "init", map[string][]byte{statePath: []byte("old"), srcPath: []byte("src-old"), "README.md": []byte("r")})

	res, err := bot.Commit(t.Context(), c, commitOpts(base, files("new", "src-new")))
	if err != nil {
		t.Fatal(err)
	}

	commit, _ := srv.CommitByID(res.OID)
	if !res.Created || srv.Head("main") != res.OID || commit.Parent != base {
		t.Fatalf("result %+v, head %s, parent %s", res, srv.Head("main"), commit.Parent)
	}

	if string(commit.Files[statePath]) != "new" || string(commit.Files[srcPath]) != "src-new" || string(commit.Files["README.md"]) != "r" {
		t.Errorf("committed files = %q", commit.Files)
	}

	if commit.Headline != "chore(schemas): record catalog 20260924.0905" || commit.Body != "body" {
		t.Errorf("message = %q / %q", commit.Headline, commit.Body)
	}

	if !slices.Contains(srv.Requests(), "CreateCommit") {
		t.Error("the commit was not made through createCommitOnBranch, so GitHub would not sign it")
	}
}

func TestCommitFollowsAHeadThatDidNotTouchTheFiles(t *testing.T) {
	srv, c := setup(t)
	base := srv.Push("main", "init", map[string][]byte{statePath: []byte("old"), srcPath: []byte("src")})
	other := srv.Push("main", "docs", map[string][]byte{"docs/x.md": []byte("x")})

	res, err := bot.Commit(t.Context(), c, commitOpts(base, files("new", "src")))
	if err != nil {
		t.Fatal(err)
	}

	commit, _ := srv.CommitByID(res.OID)
	if commit.Parent != other || string(commit.Files["docs/x.md"]) != "x" || string(commit.Files[statePath]) != "new" {
		t.Errorf("commit %+v does not sit on the moved head %s", commit, other)
	}
}

func TestCommitRefusesWhenTheFilesChangedSinceTheBase(t *testing.T) {
	for _, changed := range []string{statePath, srcPath} {
		srv, c := setup(t)
		base := srv.Push("main", "init", map[string][]byte{statePath: []byte("old"), srcPath: []byte("src")})
		moved := srv.Push("main", "someone else", map[string][]byte{changed: []byte("theirs")})

		_, err := bot.Commit(t.Context(), c, commitOpts(base, files("new", "src-new")))
		if fault.KindOf(err) != fault.Integrity || !strings.Contains(err.Error(), changed) {
			t.Errorf("%s changed: err = %v, want an integrity error naming it", changed, err)
		}

		if srv.Head("main") != moved || slices.Contains(srv.Requests(), "CreateCommit") {
			t.Errorf("%s changed: something was committed", changed)
		}
	}
}

func TestCommitRetriesWhenTheBranchMovesDuringTheMutation(t *testing.T) {
	srv, c := setup(t)
	base := srv.Push("main", "init", map[string][]byte{statePath: []byte("old"), srcPath: []byte("src")})

	pushed := ""
	srv.BeforeMutation = func() {
		if pushed == "" {
			pushed = srv.Push("main", "concurrent", map[string][]byte{"other.txt": []byte("o")})
		}
	}

	res, err := bot.Commit(t.Context(), c, commitOpts(base, files("new", "src")))
	if err != nil {
		t.Fatal(err)
	}

	commit, _ := srv.CommitByID(res.OID)
	if !res.Created || commit.Parent != pushed || string(commit.Files["other.txt"]) != "o" {
		t.Errorf("commit %+v does not include the concurrent change %s", commit, pushed)
	}

	if n := count(srv.Requests(), "CreateCommit"); n != 2 {
		t.Errorf("mutations = %d, want 2", n)
	}
}

func TestCommitFailsWhenAConcurrentPushChangesTheFiles(t *testing.T) {
	srv, c := setup(t)
	base := srv.Push("main", "init", map[string][]byte{statePath: []byte("old"), srcPath: []byte("src")})

	srv.BeforeMutation = func() {
		srv.BeforeMutation = nil
		srv.Push("main", "concurrent", map[string][]byte{statePath: []byte("theirs")})
	}

	_, err := bot.Commit(t.Context(), c, commitOpts(base, files("new", "src")))
	if fault.KindOf(err) != fault.Integrity {
		t.Errorf("err = %v, want an integrity error", err)
	}
}

func TestCommitIsIdempotent(t *testing.T) {
	srv, c := setup(t)
	base := srv.Push("main", "init", map[string][]byte{statePath: []byte("old"), srcPath: []byte("src")})

	first, err := bot.Commit(t.Context(), c, commitOpts(base, files("new", "src-new")))
	if err != nil {
		t.Fatal(err)
	}

	srv.Push("main", "later", map[string][]byte{"docs/y.md": []byte("y")})

	again, err := bot.Commit(t.Context(), c, commitOpts(base, files("new", "src-new")))
	if err != nil {
		t.Fatal(err)
	}

	if again.Created || again.OID != first.OID {
		t.Errorf("re-run = %+v, want the earlier commit %s without a new one", again, first.OID)
	}

	if n := count(srv.Requests(), "CreateCommit"); n != 1 {
		t.Errorf("mutations = %d, want 1", n)
	}
}

func TestCommitRecognizesItsOwnCommitAfterALostResponse(t *testing.T) {
	srv, c := setup(t)
	base := srv.Push("main", "init", map[string][]byte{statePath: []byte("old"), srcPath: []byte("src")})
	srv.LoseMutationResponses(1)

	res, err := bot.Commit(t.Context(), c, commitOpts(base, files("new", "src")))
	if err != nil {
		t.Fatal(err)
	}

	commit, _ := srv.CommitByID(res.OID)
	if res.OID != srv.Head("main") || commit.Parent != base || string(commit.Files[statePath]) != "new" {
		t.Errorf("result %+v is not the commit whose response was lost", res)
	}

	if n := count(srv.Requests(), "CreateCommit"); n != 1 {
		t.Errorf("mutations = %d, want 1: the commit already existed", n)
	}
}

func TestCommitReturnsTheHeadWhenTheFilesWereCompletedLater(t *testing.T) {
	srv, c := setup(t)
	srv.Push("main", "init", map[string][]byte{statePath: []byte("old"), srcPath: []byte("src")})
	srv.Push("main", "state", map[string][]byte{statePath: []byte("new")})
	head := srv.Push("main", "source", map[string][]byte{srcPath: []byte("src-new")})

	res, err := bot.Commit(t.Context(), c, commitOpts(head, files("new", "src-new")))
	if err != nil {
		t.Fatal(err)
	}

	if res.Created || res.OID != head {
		t.Errorf("result %+v, want the head %s, the first commit holding both files", res, head)
	}
}

func TestCommitReportsPermanentFailures(t *testing.T) {
	srv, c := setup(t)
	base := srv.Push("main", "init", map[string][]byte{statePath: []byte("old")})
	srv.Fail("CreateCommit", http.StatusForbidden)

	_, err := bot.Commit(t.Context(), c, commitOpts(base, files("new", "src")))
	if fault.KindOf(err) != fault.Registry || !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v, want the registry error of the mutation", err)
	}
}

func TestCommitGivesUpOnABranchThatKeepsMoving(t *testing.T) {
	srv, c := setup(t)
	base := srv.Push("main", "init", map[string][]byte{statePath: []byte("old")})

	n := 0
	srv.BeforeMutation = func() {
		n++
		srv.Push("main", "busy", map[string][]byte{"busy.txt": {byte(n)}})
	}

	opts := commitOpts(base, files("new", "src"))
	opts.Attempts = 3

	_, err := bot.Commit(t.Context(), c, opts)
	if err == nil || !strings.Contains(err.Error(), "kept moving") || n != 3 {
		t.Errorf("err = %v after %d mutations", err, n)
	}
}

func TestCommitValidatesItsInput(t *testing.T) {
	_, c := setup(t)
	good := strings.Repeat("a", 40)

	cases := map[string]bot.CommitOptions{
		"short base":   commitOpts("abc", files("a", "b")),
		"no files":     commitOpts(good, nil),
		"parent path":  commitOpts(good, []github.FileAddition{{Path: "../x"}}),
		"git path":     commitOpts(good, []github.FileAddition{{Path: "a/.GIT/config"}}),
		"absolute":     commitOpts(good, []github.FileAddition{{Path: "/etc/passwd"}}),
		"unclean":      commitOpts(good, []github.FileAddition{{Path: "a//b"}}),
		"duplicate":    commitOpts(good, []github.FileAddition{{Path: "a"}, {Path: "a"}}),
		"ref branch":   {Branch: "refs/heads/main", Base: good, Headline: "h", Files: files("a", "b")},
		"two headline": {Branch: "main", Base: good, Headline: "a\nb", Files: files("a", "b")},
	}

	for name, opts := range cases {
		if _, err := bot.Commit(t.Context(), c, opts); fault.KindOf(err) != fault.Usage {
			t.Errorf("%s: err = %v, want a usage error", name, err)
		}
	}
}

func revision(t *testing.T, s string) calver.Revision {
	t.Helper()

	r, err := calver.ParseRevision(s)
	if err != nil {
		t.Fatal(err)
	}

	return r
}

func TestReleaseTagsAndReleasesOnce(t *testing.T) {
	srv, c := setup(t)
	commit := srv.Push("main", "record", map[string][]byte{statePath: []byte("s")})
	rev := revision(t, "20260924.0905")

	res, err := bot.Release(t.Context(), c, bot.ReleaseOptions{Revision: rev, Commit: commit, Notes: "the notes"})
	if err != nil {
		t.Fatal(err)
	}

	if target, _ := srv.TagTarget("catalog-20260924.0905"); target != commit || !res.TagCreated || !res.ReleaseCreated {
		t.Errorf("tag -> %s, result %+v", target, res)
	}

	rel, ok := srv.ReleaseOf("catalog-20260924.0905")
	if !ok || rel.Name != "Schemas 20260924.0905" || rel.Body != "the notes" || rel.MakeLatest != "false" || rel.Draft || rel.Prerelease {
		t.Errorf("release = %+v", rel)
	}

	again, err := bot.Release(t.Context(), c, bot.ReleaseOptions{Revision: rev, Commit: commit, Notes: "other notes"})
	if err != nil {
		t.Fatal(err)
	}

	if again.TagCreated || again.ReleaseCreated || again.URL != res.URL {
		t.Errorf("re-run = %+v", again)
	}

	if rel, _ := srv.ReleaseOf("catalog-20260924.0905"); rel.Body != "the notes" {
		t.Errorf("the existing release was changed: %+v", rel)
	}
}

func TestReleaseRefusesATagOnAnotherCommit(t *testing.T) {
	srv, c := setup(t)
	old := srv.Push("main", "old", map[string][]byte{statePath: []byte("1")})
	commit := srv.Push("main", "record", map[string][]byte{statePath: []byte("2")})
	srv.SetTag("catalog-20260924.0905", old, true)

	_, err := bot.Release(t.Context(), c, bot.ReleaseOptions{Revision: revision(t, "20260924.0905"), Commit: commit, Notes: "n"})
	if fault.KindOf(err) != fault.Integrity || !strings.Contains(err.Error(), old) {
		t.Errorf("err = %v, want an integrity error naming %s", err, old)
	}

	if _, ok := srv.ReleaseOf("catalog-20260924.0905"); ok {
		t.Error("a release was created for a tag on another commit")
	}
}

func TestReleaseCompletesAnExistingTag(t *testing.T) {
	srv, c := setup(t)
	commit := srv.Push("main", "record", map[string][]byte{statePath: []byte("s")})
	srv.SetTag("catalog-20260924.0905", commit, true)

	res, err := bot.Release(t.Context(), c, bot.ReleaseOptions{Revision: revision(t, "20260924.0905"), Commit: commit, Notes: "n"})
	if err != nil || res.TagCreated || !res.ReleaseCreated {
		t.Errorf("result %+v, %v", res, err)
	}
}

func TestReleaseSurvivesRaces(t *testing.T) {
	srv, c := setup(t)
	commit := srv.Push("main", "record", map[string][]byte{statePath: []byte("s")})
	srv.SetTag("catalog-20260924.0905", commit, false)
	srv.AddRelease("catalog-20260924.0905", "Schemas 20260924.0905", "first")
	// Both lookups miss once, as if another run created the tag and the
	// release just after them.
	srv.Fail("getTag", http.StatusNotFound)
	srv.Fail("getRelease", http.StatusNotFound)

	res, err := bot.Release(t.Context(), c, bot.ReleaseOptions{Revision: revision(t, "20260924.0905"), Commit: commit, Notes: "n"})
	if err != nil || res.TagCreated || res.ReleaseCreated || res.URL == "" {
		t.Errorf("result %+v, %v", res, err)
	}
}

func TestReleaseValidatesItsInput(t *testing.T) {
	_, c := setup(t)

	if _, err := bot.Release(t.Context(), c, bot.ReleaseOptions{Commit: strings.Repeat("a", 40)}); fault.KindOf(err) != fault.Usage {
		t.Errorf("no revision: %v", err)
	}

	if _, err := bot.Release(t.Context(), c, bot.ReleaseOptions{Revision: revision(t, "20260924.0905"), Commit: "HEAD"}); fault.KindOf(err) != fault.Usage {
		t.Errorf("symbolic commit: %v", err)
	}
}

func testState(t *testing.T, rev, catalogDigest string) *state.State {
	t.Helper()

	st := &state.State{
		FormatVersion: state.FormatVersion,
		Source: state.Source{
			Kind: state.KindSchemaStore, Repository: state.SchemaStoreRepository, Commit: strings.Repeat("c", 40),
			TarballDigest: "sha256:" + strings.Repeat("d", 64),
		},
		Recipe:  "test-recipe",
		Catalog: state.Catalog{Revision: rev, Digest: catalogDigest, Size: 1234},
		Schemas: []state.Schema{{
			ID: "one", ContentDigest: "sha256:" + strings.Repeat("1", 64), FirstRevision: rev, LastChangedRevision: rev,
			Entry: catalog.Entry{
				ID: "one", Name: "One",
				Artifact:   catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: "sha256:" + strings.Repeat("2", 64), Size: 500},
				Provenance: &catalog.Provenance{Source: "https://json.schemastore.org/one.json", License: "Apache-2.0"},
			},
		}},
	}

	if err := st.Validate(); err != nil {
		t.Fatal(err)
	}

	return st
}

func TestStatus(t *testing.T) {
	catalogDigest := "sha256:" + strings.Repeat("e", 64)
	other := "sha256:" + strings.Repeat("f", 64)

	srv, c := setup(t)
	commit := srv.Push("main", "record", map[string][]byte{statePath: []byte("s")})
	st := testState(t, "20260924.0905", catalogDigest)

	res, err := bot.CheckStatus(t.Context(), c, nil, other)
	if err != nil || res.Recorded || res.Pending {
		t.Errorf("no state: %+v, %v", res, err)
	}

	res, err = bot.CheckStatus(t.Context(), c, st, "")
	if err != nil || !res.Pending || res.Tag || res.Release || res.Latest != bot.LatestUnknown ||
		!slices.Equal(res.Missing, []string{"tag catalog-20260924.0905", "release catalog-20260924.0905"}) {
		t.Errorf("nothing done: %+v, %v", res, err)
	}

	srv.SetTag("catalog-20260924.0905", commit, false)
	srv.AddRelease("catalog-20260924.0905", "Schemas 20260924.0905", "n")

	res, err = bot.CheckStatus(t.Context(), c, st, other)
	if err != nil || !res.Pending || res.Latest != bot.LatestOther || !slices.Equal(res.Missing, []string{"catalog-latest"}) {
		t.Errorf("catalog-latest behind: %+v, %v", res, err)
	}

	for _, latest := range []string{catalogDigest, ""} {
		res, err = bot.CheckStatus(t.Context(), c, st, latest)
		if err != nil || res.Pending || !res.Tag || !res.Release {
			t.Errorf("finished (latest %q): %+v, %v", latest, res, err)
		}
	}

	srv.Fail("getRelease", http.StatusForbidden)

	if _, err := bot.CheckStatus(t.Context(), c, st, ""); fault.KindOf(err) != fault.Registry {
		t.Errorf("a failed lookup must not read as missing: %v", err)
	}
}

func count(requests []string, operation string) int {
	n := 0

	for _, r := range requests {
		if r == operation {
			n++
		}
	}

	return n
}
