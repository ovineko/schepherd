package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/publisher/prepare"
	"github.com/ovineko/schepherd/internal/publisher/publish"
	"github.com/ovineko/schepherd/internal/publisher/state"
	"github.com/ovineko/schepherd/internal/registry"
	"github.com/ovineko/schepherd/internal/testutil/ociregistry"
	"github.com/ovineko/schepherd/tools/catalogbot/internal/github/githubtest"
)

// The tests in this file run the publish job of update-schemas.yml step by
// step: the real publisher against an in-process registry, and the bot
// against the fake GitHub API, with the state file passed between them
// exactly as in the workflow.

const (
	stateFile     = "catalog/state.json"
	sourceFile    = "sources/schemastore.toml"
	firstUpstream = "1111111111111111111111111111111111111111"
)

type pipeline struct {
	gh     *githubtest.Server
	repo   *registry.Repo
	lookup func(string) (string, bool)
	dir    string
}

func newPipeline(t *testing.T) *pipeline {
	t.Helper()

	reg := ociregistry.New(t)

	name, err := registry.ParseRepository(reg.Host() + "/ovineko/schepherd-schemas")
	if err != nil {
		t.Fatal(err)
	}

	repo, err := registry.NewClient(registry.Options{Hosts: map[string]registry.HostConfig{reg.Host(): {PlainHTTP: true}}}).Open(name)
	if err != nil {
		t.Fatal(err)
	}

	gh := githubtest.New(t, testRepo, testToken)
	gh.Push("main", "init", map[string][]byte{sourceFile: sourceWith(firstUpstream), "README.md": []byte("readme\n")})

	return &pipeline{gh: gh, repo: repo, lookup: apiEnv(gh), dir: t.TempDir()}
}

// sourceWith is a SchemaStore source description that records commit.
func sourceWith(commit string) []byte {
	return []byte("# SchemaStore upstream\nkind = \"upstream\"\ncommit = \"" + commit + "\"\n" +
		"tarball_base_url = \"https://codeload.github.com/SchemaStore/schemastore/tar.gz\"\nmax_tarball_bytes = 67108864\n\n" +
		"[dependencies]\nmax_depth = 8\nmax_per_schema = 64\nmax_document_bytes = 16777216\nmax_total_bytes = 268435456\n")
}

// week is what prepare produced from one upstream commit: the prepared
// schemas (with names other than "Schema <id>" in names), the published
// schemas it holds with their reason and those an exclude rule removes. An
// entry whose content base records keeps the recorded artifact without a
// file, as prepare does for a source whose digests did not change.
type week struct {
	base     *state.State
	schemas  map[string]string
	names    map[string]string
	held     map[string]string
	commit   string
	excluded []string
}

// preparedSet writes the prepared set of w.
func (p *pipeline) preparedSet(t *testing.T, w week) string {
	t.Helper()

	set := &prepare.Set{
		Document: prepare.Document{
			FormatVersion: prepare.FormatVersion, Recipe: prepare.Recipe,
			Source: prepare.Source{Kind: prepare.KindSchemaStore, Commit: w.commit, TarballDigest: digest.FromBytes([]byte(w.commit))},
		},
		Schemas: map[string][]byte{},
		Notices: map[string][]byte{},
	}

	for _, id := range slices.Sorted(maps.Keys(w.schemas)) {
		body := []byte(w.schemas[id])
		name := "Schema " + id

		if n, ok := w.names[id]; ok {
			name = n
		}

		e := prepare.Entry{
			ID: id, Name: name, FileMatch: []string{}, Schema: prepare.SchemaPath(id), ContentDigest: digest.FromBytes(body),
			Provenance: catalog.Provenance{Source: "https://json.schemastore.org/" + id + ".json", SourceDigest: digest.FromBytes(body), License: "Apache-2.0"},
		}

		if rec, ok := lookup(w.base, id); ok && !rec.Excluded() && rec.ContentDigest == e.ContentDigest {
			artifact := rec.Entry.Artifact
			e.Schema, e.Reused, e.NoticeDigest = "", &artifact, rec.NoticeDigest
		} else {
			set.Schemas[id] = body
		}

		set.Document.Entries = append(set.Document.Entries, e)
	}

	for _, id := range slices.Sorted(maps.Keys(w.held)) {
		set.Document.Held = append(set.Document.Held, prepare.Hold{ID: id, Reason: w.held[id]})
	}

	for _, id := range slices.Sorted(slices.Values(w.excluded)) {
		set.Document.Excluded = append(set.Document.Excluded, prepare.Exclusion{ID: id, Rule: "takedown-" + id})
	}

	dir := filepath.Join(t.TempDir(), "prepared")
	if err := prepare.WriteSet(dir, set, nil); err != nil {
		t.Fatal(err)
	}

	return dir
}

func lookup(st *state.State, id string) (*state.Schema, bool) {
	if st == nil {
		return nil, false
	}

	return st.Lookup(id)
}

func (p *pipeline) loadState(t *testing.T, path string) *state.State {
	t.Helper()

	st, err := state.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	return st
}

// stateAt writes the state recorded in commit to a file and returns its
// path, which does not exist when the commit has no state yet.
func (p *pipeline) stateAt(t *testing.T, commit string) string {
	t.Helper()

	path := filepath.Join(p.dir, "state-"+commit+".json")

	c, ok := p.gh.CommitByID(commit)
	if !ok {
		t.Fatalf("no commit %s", commit)
	}

	if data, ok := c.Files[stateFile]; ok {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return path
}

// publishStep is `schepherd-publisher publish --json` with --state and
// --state-out; it returns the path of the JSON result.
func (p *pipeline) publishStep(t *testing.T, prepared, base, out string, at time.Time, latest bool) (string, *publish.Result) {
	t.Helper()

	previous, err := state.Load(base)
	if err != nil {
		t.Fatal(err)
	}

	res, err := publish.Run(t.Context(), p.repo, publish.Options{
		Now: at, State: previous, StateOut: out, PreparedDir: prepared, Repository: p.repo.Name().String(), UpdateLatest: latest,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	resultPath := out + ".result.json"
	if err := os.WriteFile(resultPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	return resultPath, res
}

func (p *pipeline) bot(t *testing.T, args ...string) string {
	t.Helper()

	code, stdout, stderr := invoke(t, p.lookup, args...)
	if code != 0 {
		t.Fatalf("catalogbot %s exited %d: %s", args[0], code, stderr)
	}

	return strings.TrimSpace(stdout)
}

func (p *pipeline) latest(t *testing.T) string {
	t.Helper()

	desc, err := p.repo.Resolve(t.Context(), publish.LatestTag)
	if err != nil {
		return ""
	}

	return desc.Digest.String()
}

// jobRun is what one publish job works with: the prepared set and the
// base state from the prepare job's artifact, the upstream commit the
// prepare job reported and the commit the run checked out.
type jobRun struct {
	prepared  string
	base      string
	upstream  string
	expected  string
	failAfter string
}

// checkout writes the file path of the commit the run checked out to dir
// and returns its local path, which does not exist when the commit has no
// such file.
func (p *pipeline) checkout(t *testing.T, commit, path, dir string) string {
	t.Helper()

	c, ok := p.gh.CommitByID(commit)
	if !ok {
		t.Fatalf("no commit %s", commit)
	}

	local := filepath.Join(dir, filepath.Base(path))
	if data, ok := c.Files[path]; ok {
		if err := os.WriteFile(local, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return local
}

// checkPrepared is the first step of the publish job: the prepare job, which
// processed untrusted upstream data, must have compared with exactly the
// state file of the checkout (cmp), or with none when the checkout has none.
func checkPrepared(base, checkout string) error {
	want, wantErr := os.ReadFile(checkout)
	got, gotErr := os.ReadFile(base)

	switch {
	case errors.Is(wantErr, fs.ErrNotExist) && errors.Is(gotErr, fs.ErrNotExist):
		return nil
	case wantErr != nil || gotErr != nil:
		return errors.Join(errors.New("the prepared update was not compared with the state of the checkout"), wantErr, gotErr)
	case !bytes.Equal(want, got):
		return errors.New("the prepared update was compared with another state than the checkout's")
	}

	return nil
}

// publishJob runs the steps of the publish job and returns the commit that
// recorded the state. failAfter stops the job after "commit" or "release".
// Only the prepared set comes from the prepare job: the state is the
// checkout's, and the source description committed with the new state is
// the checkout's with the upstream commit that state records.
func (p *pipeline) publishJob(t *testing.T, run jobRun, at time.Time) (string, string) {
	t.Helper()

	work := t.TempDir()
	out := filepath.Join(work, "state.json")
	checkedOut := p.checkout(t, run.expected, stateFile, t.TempDir())

	if err := checkPrepared(run.base, checkedOut); err != nil {
		t.Fatal(err)
	}

	result, res := p.publishStep(t, run.prepared, checkedOut, out, at, false)
	if res.Status != publish.StatusPublished && res.Status != publish.StatusResumed {
		t.Fatalf("publish status %s", res.Status)
	}

	notesFile := filepath.Join(work, "notes.md")
	p.bot(t, "notes", "--result", result, "--state", out, "--repository", "ghcr.io/ovineko/schepherd-schemas", "--out", notesFile)

	message := writeFile(t, work, "message.txt", "chore(schemas): record catalog "+res.Revision+"\n")
	source := filepath.Join(work, "schemastore.toml")
	p.bot(t, "set-commit", "--source", p.checkout(t, run.expected, sourceFile, t.TempDir()), "--state", out,
		"--commit", run.upstream, "--out", source)

	commit := p.bot(t, "commit", "--repo", testRepo, "--branch", "main", "--expected-head", run.expected,
		"--message-file", message, "--file", stateFile+"="+out, "--file", sourceFile+"="+source)

	if run.failAfter == "commit" {
		return commit, res.Revision
	}

	p.bot(t, "release", "--repo", testRepo, "--revision", res.Revision, "--commit", commit, "--notes", notesFile)

	if run.failAfter == "release" {
		return commit, res.Revision
	}

	latestOut := filepath.Join(work, "state-latest.json")

	_, again := p.publishStep(t, run.prepared, checkedOut, latestOut, at.Add(7*time.Minute), true)
	if again.Status != publish.StatusResumed || again.Revision != res.Revision {
		t.Fatalf("the catalog-latest step published %s as %s", again.Revision, again.Status)
	}

	if readText(t, out) != readText(t, latestOut) {
		t.Fatal("the catalog-latest step wrote another state")
	}

	return commit, res.Revision
}

func readText(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

func weekSchemas(n int) map[string]string {
	schemas := map[string]string{
		"alpha": `{"type":"object"}`,
		"beta":  `{"type":"string"}`,
		"gamma": `{"type":"array"}`,
	}

	if n >= 2 {
		schemas["beta"] = `{"type":"string","minLength":1}`
		schemas["delta"] = `{"type":"number"}`
		delete(schemas, "gamma")
	}

	return schemas
}

// secondWeek is weekSchemas(2) as prepare reports it against base: gamma
// is held because upstream removed it, alpha is reused.
func secondWeek(base *state.State, commit string) week {
	return week{base: base, commit: commit, schemas: weekSchemas(2), held: map[string]string{"gamma": state.HeldRemovedUpstream}}
}

func TestPublishJobRecordsTagsAndReleasesEachRevision(t *testing.T) {
	p := newPipeline(t)
	week1 := time.Date(2026, 9, 28, 3, 0, 12, 0, time.UTC)

	head := p.gh.Head("main")
	commit1, rev1 := p.publishJob(t, jobRun{
		prepared: p.preparedSet(t, week{commit: firstUpstream, schemas: weekSchemas(1)}), base: p.stateAt(t, head),
		upstream: firstUpstream, expected: head,
	}, week1)

	if rev1 != "20260928.0300" || p.gh.Head("main") != commit1 {
		t.Fatalf("revision %s, head %s, commit %s", rev1, p.gh.Head("main"), commit1)
	}

	recorded := p.stateAt(t, commit1)

	st, err := state.Load(recorded)
	if err != nil || st == nil || st.Catalog.Revision != rev1 || p.latest(t) != st.Catalog.Digest {
		t.Fatalf("recorded state %+v, %v; catalog-latest %s", st, err, p.latest(t))
	}

	if target, _ := p.gh.TagTarget("catalog-" + rev1); target != commit1 {
		t.Errorf("tag catalog-%s -> %s, want %s", rev1, target, commit1)
	}

	rel, _ := p.gh.ReleaseOf("catalog-" + rev1)
	if rel.MakeLatest != "false" || !strings.Contains(rel.Body, "## Added (3)") || !strings.Contains(rel.Body, "`beta` `Schema beta`") {
		t.Errorf("release of week 1:\n%+v", rel)
	}

	// Week 2: beta changed, delta is new, alpha is reused and upstream
	// dropped gamma, which the catalog keeps.
	const secondUpstream = "2222222222222222222222222222222222222222"

	head = p.gh.Head("main")
	base := p.stateAt(t, head)
	commit2, rev2 := p.publishJob(t, jobRun{
		prepared: p.preparedSet(t, secondWeek(p.loadState(t, base), secondUpstream)), base: base,
		upstream: secondUpstream, expected: head,
	}, week1.Add(7*24*time.Hour))

	next, err := state.Load(p.stateAt(t, commit2))
	if err != nil {
		t.Fatal(err)
	}

	gamma, ok := next.Lookup("gamma")
	if !ok || gamma.HeldSinceRevision != rev2 || gamma.HeldReason != state.HeldRemovedUpstream || next.Source.Commit != secondUpstream {
		t.Errorf("week 2 state: gamma %+v, source %+v", gamma, next.Source)
	}

	c2, _ := p.gh.CommitByID(commit2)
	if string(c2.Files[sourceFile]) != string(sourceWith(secondUpstream)) || c2.Parent != commit1 {
		t.Errorf("week 2 commit %+v", c2)
	}

	rel, _ = p.gh.ReleaseOf("catalog-" + rev2)
	for _, want := range []string{
		"1 added, 1 changed, 0 metadata updated, 0 excluded, 2 unchanged (1 of them held).",
		"## Added (1)\n\n- `delta`", "## Changed (1)\n\n- `beta`", "## Held (1)",
		"- `gamma` `Schema gamma`: upstream removed it (since `" + rev2 + "`)\n",
	} {
		if !strings.Contains(rel.Body, want) {
			t.Errorf("release of week 2 lacks %q:\n%s", want, rel.Body)
		}
	}
}

func TestPublishJobReRunAfterAFailedReleaseCompletesTheWork(t *testing.T) {
	p := newPipeline(t)
	at := time.Date(2026, 9, 28, 3, 0, 0, 0, time.UTC)
	head := p.gh.Head("main")
	run := jobRun{
		prepared: p.preparedSet(t, week{commit: firstUpstream, schemas: weekSchemas(1)}), base: p.stateAt(t, head),
		upstream: firstUpstream, expected: head, failAfter: "commit",
	}

	commit, rev := p.publishJob(t, run, at)

	if _, ok := p.gh.ReleaseOf("catalog-" + rev); ok || p.latest(t) != "" {
		t.Fatal("the interrupted run released or moved catalog-latest")
	}

	// GitHub re-runs the failed job with the same inputs, minutes later.
	run.failAfter = ""
	again, againRev := p.publishJob(t, run, at.Add(25*time.Minute))

	if again != commit || againRev != rev {
		t.Errorf("the re-run recorded %s as %s, want %s as %s", againRev, again, rev, commit)
	}

	if p.gh.Head("main") != commit {
		t.Error("the re-run committed again")
	}

	if _, ok := p.gh.ReleaseOf("catalog-" + rev); !ok || p.latest(t) == "" {
		t.Error("the re-run did not release or move catalog-latest")
	}
}

// The prepare job runs the bundler on untrusted upstream data, so the
// publish job, whose App token may write to main, trusts nothing it hands
// over beyond the prepared set: the preparation must have compared with the
// state of the checkout, and the source description it commits is the
// checkout's own with only the upstream commit the new state records.
func TestPublishJobChecksWhatThePrepareJobHandsOver(t *testing.T) {
	p := newPipeline(t)
	at := time.Date(2026, 9, 28, 3, 0, 0, 0, time.UTC)

	// A maintainer raised a limit on main before the first publication.
	raised := strings.Replace(string(sourceWith(firstUpstream)), "max_tarball_bytes = 67108864", "max_tarball_bytes = 134217728", 1)
	head := p.gh.Push("main", "limits", map[string][]byte{sourceFile: []byte(raised)})

	noState := filepath.Join(t.TempDir(), "none.json")
	if err := checkPrepared(noState, p.checkout(t, head, stateFile, t.TempDir())); err != nil {
		t.Fatalf("a first publication: %v", err)
	}

	smuggled := writeFile(t, t.TempDir(), "base-state.json", "{}\n")
	if err := checkPrepared(smuggled, p.checkout(t, head, stateFile, t.TempDir())); err == nil {
		t.Error("a prepared update that carries a state was accepted for a checkout without one")
	}

	commit1, _ := p.publishJob(t, jobRun{
		prepared: p.preparedSet(t, week{commit: firstUpstream, schemas: weekSchemas(1)}), base: noState, upstream: firstUpstream, expected: head,
	}, at)

	c1, _ := p.gh.CommitByID(commit1)
	if string(c1.Files[sourceFile]) != raised {
		t.Errorf("week 1 committed the source description\n%s\nwant the checkout's\n%s", c1.Files[sourceFile], raised)
	}

	recorded := p.stateAt(t, commit1)
	week2 := p.preparedSet(t, secondWeek(p.loadState(t, recorded), secondCommit))

	edited := p.loadState(t, recorded)
	edited.Schemas[0].Entry.Name = "Edited in the prepare job"

	other := filepath.Join(t.TempDir(), "state.json")
	if err := state.Save(other, edited); err != nil {
		t.Fatal(err)
	}

	for name, base := range map[string]string{"another state": other, "no state": noState} {
		if err := checkPrepared(base, p.checkout(t, commit1, stateFile, t.TempDir())); err == nil {
			t.Errorf("a prepared update compared with %s was accepted", name)
		}
	}

	commit2, _ := p.publishJob(t, jobRun{prepared: week2, base: recorded, upstream: secondCommit, expected: commit1}, at.Add(7*24*time.Hour))

	c2, _ := p.gh.CommitByID(commit2)
	if want := strings.Replace(raised, firstUpstream, secondCommit, 1); string(c2.Files[sourceFile]) != want {
		t.Errorf("week 2 committed the source description\n%s\nwant\n%s", c2.Files[sourceFile], want)
	}

	// The upstream commit the prepare job reported must be the one the
	// prepared set, and so the new state, records.
	work := t.TempDir()
	out := filepath.Join(work, "state.json")
	p.publishStep(t, week2, recorded, out, at.Add(7*24*time.Hour), false)

	code, _, stderr := invoke(t, p.lookup, "set-commit", "--source", p.checkout(t, commit1, sourceFile, work), "--state", out,
		"--commit", firstUpstream, "--out", filepath.Join(work, "schemastore.toml"))
	if code != 5 {
		t.Errorf("set-commit with another upstream commit than the state's exited %d: %s", code, stderr)
	}
}

// finishJob runs the publish job in finish mode: the state on main already
// records the revision, so nothing is prepared or published. The job checks
// that the registry holds exactly the recorded catalog, renders the notes
// from the state, finds the recording commit, tags and releases it and moves
// catalog-latest. checkout is the state file of the commit the run started
// from.
func (p *pipeline) finishJob(t *testing.T, checkout, expected string) (string, error) {
	t.Helper()

	recorded, err := state.Load(checkout)
	if err != nil || recorded == nil {
		t.Fatalf("the checkout has no state: %v", err)
	}

	checked, err := publish.Latest(t.Context(), p.repo, recorded, publish.LatestOptions{CheckOnly: true})
	if err != nil {
		return "", err
	}

	if checked.Latest != publish.LatestChecked {
		t.Fatalf("the check reported %+v", checked)
	}

	work := t.TempDir()
	notesFile := filepath.Join(work, "notes.md")
	p.bot(t, "notes", "--state", checkout, "--repository", "ghcr.io/ovineko/schepherd-schemas", "--out", notesFile)

	message := writeFile(t, work, "message.txt", "chore(schemas): record catalog "+recorded.Catalog.Revision+"\n")
	commit := p.bot(t, "commit", "--repo", testRepo, "--branch", "main", "--expected-head", expected,
		"--message-file", message, "--file", stateFile+"="+checkout)

	p.bot(t, "release", "--repo", testRepo, "--revision", recorded.Catalog.Revision, "--commit", commit, "--notes", notesFile)

	moved, err := publish.Latest(t.Context(), p.repo, recorded, publish.LatestOptions{})
	if err != nil {
		t.Fatalf("latest: %v", err)
	}

	if moved.Latest == publish.LatestChecked {
		t.Fatalf("latest did not move: %+v", moved)
	}

	return commit, nil
}

// TestFinishModeCompletesAnInterruptedRevision follows the prepare job's
// finish mode: the recorded revision lacks its tag, release and
// catalog-latest, so the next run completes it from the recorded state
// alone. Nothing is prepared again, so it does not matter that upstream has
// changed in the meantime, and the notes equal those of the interrupted run.
func TestFinishModeCompletesAnInterruptedRevision(t *testing.T) {
	p := newPipeline(t)
	at := time.Date(2026, 9, 28, 3, 0, 0, 0, time.UTC)
	head := p.gh.Head("main")

	commit, rev := p.publishJob(t, jobRun{
		prepared: p.preparedSet(t, week{commit: firstUpstream, schemas: weekSchemas(1)}), base: p.stateAt(t, head),
		upstream: firstUpstream, expected: head, failAfter: "commit",
	}, at)

	// A maintainer changes the source file after the recording commit.
	later := p.gh.Push("main", "limits", map[string][]byte{sourceFile: append(sourceWith(firstUpstream), "# raised limits\n"...)})
	recorded := p.stateAt(t, later)

	code, stdout, stderr := invoke(t, p.lookup, "status", "--repo", testRepo, "--state", recorded, "--json")
	if code != 0 || !strings.Contains(stdout, `"pending": true`) {
		t.Fatalf("status exited %d: %s %s", code, stdout, stderr)
	}

	finished, err := p.finishJob(t, recorded, later)
	if err != nil {
		t.Fatal(err)
	}

	if finished != commit || p.gh.Head("main") != later {
		t.Errorf("finish recorded %s (head %s), want %s without a new commit", finished, p.gh.Head("main"), commit)
	}

	if target, _ := p.gh.TagTarget("catalog-" + rev); target != commit {
		t.Errorf("tag -> %s, want the recording commit %s", target, commit)
	}

	rel, _ := p.gh.ReleaseOf("catalog-" + rev)
	if rel.MakeLatest != "false" || !strings.Contains(rel.Body, "## Added (3)") || !strings.Contains(rel.Body, "`gamma` `Schema gamma`") {
		t.Errorf("release:\n%+v", rel)
	}

	st, _ := state.Load(recorded)

	code, stdout, _ = invoke(t, p.lookup, "status", "--repo", testRepo, "--state", recorded, "--latest-digest", p.latest(t), "--json")
	if code != 0 || !strings.Contains(stdout, `"pending": false`) || p.latest(t) != st.Catalog.Digest {
		t.Errorf("after finishing: %s, catalog-latest %s", stdout, p.latest(t))
	}

	// The following week starts from the recorded state as usual.
	const secondUpstream = "2222222222222222222222222222222222222222"

	head = p.gh.Head("main")
	base := p.stateAt(t, head)
	commit2, rev2 := p.publishJob(t, jobRun{
		prepared: p.preparedSet(t, secondWeek(p.loadState(t, base), secondUpstream)), base: base,
		upstream: secondUpstream, expected: head,
	}, at.Add(7*24*time.Hour))

	if rev2 == rev || p.gh.Head("main") != commit2 || p.latest(t) == st.Catalog.Digest {
		t.Errorf("the next week recorded %s as %s; head %s", rev2, commit2, p.gh.Head("main"))
	}
}

// A finish run never tags or releases a revision the registry does not hold
// exactly as recorded, for example after the state was edited by hand.
func TestFinishModeRefusesAStateTheRegistryDoesNotHold(t *testing.T) {
	p := newPipeline(t)
	at := time.Date(2026, 9, 28, 3, 0, 0, 0, time.UTC)
	head := p.gh.Head("main")

	_, rev := p.publishJob(t, jobRun{
		prepared: p.preparedSet(t, week{commit: firstUpstream, schemas: weekSchemas(1)}), base: p.stateAt(t, head),
		upstream: firstUpstream, expected: head, failAfter: "commit",
	}, at)

	edited, err := state.Load(p.stateAt(t, p.gh.Head("main")))
	if err != nil {
		t.Fatal(err)
	}

	edited.Schemas[0].Entry.Name = "Edited by hand"

	checkout := filepath.Join(t.TempDir(), "state.json")
	if err := state.Save(checkout, edited); err != nil {
		t.Fatal(err)
	}

	if _, err := p.finishJob(t, checkout, p.gh.Head("main")); err == nil {
		t.Fatal("the finish run accepted an edited state")
	}

	if _, ok := p.gh.TagTarget("catalog-" + rev); ok {
		t.Error("the refused finish run created the tag")
	}

	if _, ok := p.gh.ReleaseOf("catalog-" + rev); ok || p.latest(t) != "" {
		t.Error("the refused finish run released or moved catalog-latest")
	}
}
