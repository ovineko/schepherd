package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/internal/publisher/policy"
	"github.com/ovineko/schepherd/internal/publisher/prepare"
	"github.com/ovineko/schepherd/internal/publisher/state"
	"github.com/ovineko/schepherd/tools/catalogbot/internal/github/githubtest"
)

const (
	testRepo  = "owner/repo"
	testToken = "ghs_do-not-print-me"
)

func lookupFrom(values map[string]string) env.LookupFunc {
	return func(name string) (string, bool) {
		v, ok := values[name]

		return v, ok
	}
}

func invoke(t *testing.T, lookup env.LookupFunc, args ...string) (code int, stdout, stderr string) {
	t.Helper()

	var out, errOut bytes.Buffer

	code = run(t.Context(), args, &out, &errOut, lookup)

	return code, out.String(), errOut.String()
}

func writeFile(t *testing.T, dir, name, contents string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func apiEnv(srv *githubtest.Server) env.LookupFunc {
	return lookupFrom(map[string]string{keyToken: testToken, keyAPIURL: srv.URL()})
}

func TestCommitAndReleaseCommands(t *testing.T) {
	srv := githubtest.New(t, testRepo, testToken)
	base := srv.Push("main", "init", map[string][]byte{"catalog/state.json": []byte("{}\n"), "sources/schemastore.toml": []byte("old\n")})
	dir := t.TempDir()

	message := writeFile(t, dir, "message.txt", "chore(schemas): record catalog 20260924.0905\n\nUpstream commit: abc\n")
	stateFile := writeFile(t, dir, "state.json", "{\"new\":true}\n")
	source := writeFile(t, dir, "schemastore.toml", "new\n")

	commitArgs := []string{
		"commit", "--repo", testRepo, "--branch", "main", "--expected-head", base, "--message-file", message,
		"--file", "catalog/state.json=" + stateFile, "--file", "sources/schemastore.toml=" + source,
	}

	code, stdout, stderr := invoke(t, apiEnv(srv), commitArgs...)
	if code != 0 {
		t.Fatalf("commit exited %d: %s", code, stderr)
	}

	oid := strings.TrimSpace(stdout)

	commit, ok := srv.CommitByID(oid)
	if !ok || commit.Parent != base || commit.Headline != "chore(schemas): record catalog 20260924.0905" || commit.Body != "Upstream commit: abc" ||
		string(commit.Files["catalog/state.json"]) != "{\"new\":true}\n" || string(commit.Files["sources/schemastore.toml"]) != "new\n" {
		t.Fatalf("commit %s = %+v", oid, commit)
	}

	code, stdout, stderr = invoke(t, apiEnv(srv), append(commitArgs, "--json")...)

	var again struct {
		Commit  string `json:"commit"`
		Created bool   `json:"created"`
	}

	if code != 0 || json.Unmarshal([]byte(stdout), &again) != nil || again.Commit != oid || again.Created {
		t.Fatalf("re-run exited %d with %q (%s)", code, stdout, stderr)
	}

	notesFile := writeFile(t, dir, "notes.md", "the notes\n")
	releaseArgs := []string{"release", "--repo", testRepo, "--revision", "20260924.0905", "--commit", oid, "--notes", notesFile}

	code, stdout, stderr = invoke(t, apiEnv(srv), releaseArgs...)
	if code != 0 || !strings.HasSuffix(strings.TrimSpace(stdout), "/releases/tag/catalog-20260924.0905") {
		t.Fatalf("release exited %d with %q (%s)", code, stdout, stderr)
	}

	if rel, _ := srv.ReleaseOf("catalog-20260924.0905"); rel.Body != "the notes\n" || rel.MakeLatest != "false" || rel.Name != "Schemas 20260924.0905" {
		t.Errorf("release = %+v", rel)
	}

	if code, _, stderr := invoke(t, apiEnv(srv), releaseArgs...); code != 0 {
		t.Errorf("a release re-run exited %d: %s", code, stderr)
	}

	moved := []string{"release", "--repo", testRepo, "--revision", "20260924.0905", "--commit", base, "--notes", notesFile}
	if code, _, stderr := invoke(t, apiEnv(srv), moved...); code != 5 || !strings.Contains(stderr, "never move") {
		t.Errorf("moving the tag exited %d: %s", code, stderr)
	}
}

func TestCommandsNeverPrintTheToken(t *testing.T) {
	srv := githubtest.New(t, testRepo, testToken)
	dir := t.TempDir()
	message := writeFile(t, dir, "message.txt", "headline\n")
	file := writeFile(t, dir, "f", "x")

	code, stdout, stderr := invoke(t, apiEnv(srv), "commit", "--repo", testToken, "--branch", "main",
		"--expected-head", strings.Repeat("a", 40), "--message-file", message, "--file", "f="+file)
	if code != 2 || strings.Contains(stdout+stderr, testToken) || !strings.Contains(stderr, "***") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	code, _, stderr = invoke(t, lookupFrom(map[string]string{keyAPIURL: srv.URL()}), "release", "--repo", testRepo,
		"--revision", "20260924.0905", "--commit", strings.Repeat("a", 40), "--notes", message)
	if code != 2 || !strings.Contains(stderr, keyToken) {
		t.Errorf("without a token: exit %d, %s", code, stderr)
	}
}

func TestUsageErrors(t *testing.T) {
	lookup := lookupFrom(nil)

	for _, args := range [][]string{
		{},
		{"unknown"},
		{"commit"},
		{"release", "--repo", testRepo, "--revision", "20260924.1", "--commit", strings.Repeat("a", 40), "--notes", "x"},
		{"status", "--repo", testRepo, "--state", "x", "--latest-digest", "sha256:abc"},
		{"notes", "--result", "x"},
		{"set-commit", "--source", "x", "--commit", "abc", "--out", "y", "extra"},
		{"summary", "--diff", "x", "--upstream", strings.Repeat("a", 40)},
		{"summary", "--checked-at", "yesterday", "--diff", "x", "--upstream", strings.Repeat("a", 40)},
		{"summary", "--checked-at", "2026-09-28T03:00:12Z"},
		{"summary", "--checked-at", "2026-09-28T03:00:12Z", "--diff", "x", "--state", "y", "--status", "z"},
		{"summary", "--checked-at", "2026-09-28T03:00:12Z", "--state", "y"},
		{"summary", "--checked-at", "2026-09-28T03:00:12Z", "--report", "r", "--upstream", strings.Repeat("a", 40)},
		{"set-commit", "--source", "x", "--out", "y"},
	} {
		if code, _, stderr := invoke(t, lookup, args...); code != 2 {
			t.Errorf("%q exited %d: %s", args, code, stderr)
		}
	}
}

func writeState(t *testing.T, dir string) (string, *state.State) {
	t.Helper()

	st := &state.State{
		FormatVersion: state.FormatVersion,
		Source: state.Source{
			Kind: state.KindSchemaStore, Repository: state.SchemaStoreRepository, Commit: strings.Repeat("c", 40),
			TarballDigest: "sha256:" + strings.Repeat("d", 64),
		},
		Recipe:  "test-recipe",
		Catalog: state.Catalog{Revision: "20260924.0905", Digest: "sha256:" + strings.Repeat("e", 64), Size: 1234},
		Schemas: []state.Schema{{
			ID: "one", ContentDigest: "sha256:" + strings.Repeat("1", 64), FirstRevision: "20260924.0905", LastChangedRevision: "20260924.0905",
			Entry: catalog.Entry{
				ID: "one", Name: "One",
				Artifact:   catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: "sha256:" + strings.Repeat("2", 64), Size: 500},
				Provenance: &catalog.Provenance{Source: "https://json.schemastore.org/one.json", License: "Apache-2.0"},
			},
		}},
	}

	path := filepath.Join(dir, "state.json")
	if err := state.Save(path, st); err != nil {
		t.Fatal(err)
	}

	return path, st
}

func TestNotesAndStatusCommands(t *testing.T) {
	srv := githubtest.New(t, testRepo, testToken)
	dir := t.TempDir()
	statePath, st := writeState(t, dir)

	result := writeFile(t, dir, "publish.json", fmt.Sprintf(
		`{"status":"published","revision":"20260924.0905","catalogDigest":%q,"catalogSize":1234,"added":["one"],"changed":[],"metadataChanged":[],"held":[],"excluded":[],"unchanged":0,"uploadedSchemas":1,"reusedSchemas":0}`,
		st.Catalog.Digest))
	out := filepath.Join(dir, "notes.md")

	code, _, stderr := invoke(t, lookupFrom(nil), "notes", "--result", result, "--state", statePath, "--repository", "ghcr.io/ovineko/schepherd-schemas", "--out", out)
	if code != 0 {
		t.Fatalf("notes exited %d: %s", code, stderr)
	}

	withResult, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(withResult), "## Added (1)\n\n- `one` `One`\n") {
		t.Errorf("notes = %q, %v", withResult, err)
	}

	code, stdout, stderr := invoke(t, lookupFrom(nil), "notes", "--state", statePath, "--repository", "ghcr.io/ovineko/schepherd-schemas")
	if code != 0 || stdout != string(withResult) {
		t.Errorf("notes from the state alone exited %d (%s) with\n%s\nwant\n%s", code, stderr, stdout, withResult)
	}

	inconsistent := writeFile(t, dir, "other.json", fmt.Sprintf(
		`{"status":"published","revision":"20260924.0905","catalogDigest":%q,"catalogSize":1234,"added":[],"changed":["one"],"metadataChanged":[],"held":[],"excluded":[],"unchanged":0,"uploadedSchemas":1,"reusedSchemas":0}`,
		st.Catalog.Digest))

	code, _, stderr = invoke(t, lookupFrom(nil), "notes", "--result", inconsistent, "--state", statePath, "--repository", "ghcr.io/ovineko/schepherd-schemas")
	if code != 5 || !strings.Contains(stderr, "other changes than the state records") {
		t.Errorf("an inconsistent result exited %d: %s", code, stderr)
	}

	code, _, stderr = invoke(t, lookupFrom(nil), "notes", "--state", filepath.Join(dir, "missing.json"), "--repository", "ghcr.io/ovineko/schepherd-schemas")
	if code != 2 || !strings.Contains(stderr, "does not exist") {
		t.Errorf("a missing state exited %d: %s", code, stderr)
	}

	status := func(args ...string) map[string]any {
		t.Helper()

		code, stdout, stderr := invoke(t, apiEnv(srv), append([]string{"status", "--repo", testRepo, "--json"}, args...)...)
		if code != 0 {
			t.Fatalf("status exited %d: %s", code, stderr)
		}

		var res map[string]any
		if err := json.Unmarshal([]byte(stdout), &res); err != nil {
			t.Fatal(err)
		}

		return res
	}

	if res := status("--state", filepath.Join(dir, "missing.json")); res["pending"] != false || res["recorded"] != false {
		t.Errorf("no state: %v", res)
	}

	if res := status("--state", statePath); res["pending"] != true || res["revision"] != "20260924.0905" {
		t.Errorf("untagged revision: %v", res)
	}

	commit := srv.Push("main", "record", map[string][]byte{"catalog/state.json": []byte("s")})
	srv.SetTag("catalog-20260924.0905", commit, false)
	srv.AddRelease("catalog-20260924.0905", "Schemas 20260924.0905", "n")

	if res := status("--state", statePath, "--latest-digest", st.Catalog.Digest); res["pending"] != false || res["latest"] != "current" {
		t.Errorf("finished revision: %v", res)
	}
}

// writeReport writes the report.json of a preparation of upstream that
// included one record, left one pending review because its host is not
// supported and failed on one.
func writeReport(t *testing.T, dir, upstream string) string {
	t.Helper()

	report := prepare.Report{
		FormatVersion: prepare.FormatVersion, GeneratedAt: "2026-09-28T03:00:00Z",
		Source: prepare.Source{Kind: prepare.KindSchemaStore, Commit: upstream, TarballDigest: "sha256:" + strings.Repeat("d", 64)},
		Totals: prepare.Totals{Records: 3, Entries: 1, Included: 1, PendingReview: 1, Failed: 1},
		Records: []prepare.RecordReport{
			{Name: "One", URL: "https://json.schemastore.org/one.json", Status: prepare.StatusIncluded, ID: "one"},
			{
				Name: "Elsewhere", URL: "https://elsewhere.example/a.json", Status: prepare.StatusPendingReview,
				LicenseDetections: []policy.Detection{{URL: "https://elsewhere.example/a.json", Verdict: policy.Review, Reason: policy.RefusedUnsupportedHost}},
			},
			{Name: "Broken", URL: "https://json.schemastore.org/broken.json", Status: prepare.StatusFailed, Reason: "invalid-json"},
		},
		Collisions: []prepare.Collision{}, Held: []prepare.Hold{}, Excluded: []prepare.Exclusion{},
	}

	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}

	return writeFile(t, dir, "report-"+upstream+".json", string(data))
}

func TestSummaryCommand(t *testing.T) {
	dir := t.TempDir()
	upstream := strings.Repeat("c", 40)
	report := writeReport(t, dir, upstream)
	diff := writeFile(t, dir, "diff.json",
		`{"hasChanges":false,"added":[],"changed":[],"metadataChanged":[],"held":[{"id":"one","reason":"license-review"}],"excluded":[],"unchanged":4}`)
	out := filepath.Join(dir, "summary.md")

	code, _, stderr := invoke(t, lookupFrom(nil), "summary", "--diff", diff, "--report", report, "--upstream", upstream,
		"--checked-at", "2026-09-28T05:00:12+02:00", "--out", out)
	if code != 0 {
		t.Fatalf("summary exited %d: %s", code, stderr)
	}

	got, err := os.ReadFile(out)
	for _, want := range []string{
		"Checked at 2026-09-28T03:00:12Z against upstream commit `" + upstream + "`: ",
		"- `one`: its license is held for review\n",
		"Upstream coverage: 3 records, 1 included (catalog entries: 1, reused: 0), 0 excluded by a rule, 1 pending review, 1 failed.",
		"### Pending review (1)", "- `unsupported-host`: 1 (",
		"### Failed (1)", "- `Broken` `https://json.schemastore.org/broken.json`: `invalid-json`\n",
	} {
		if err != nil || !strings.Contains(string(got), want) {
			t.Errorf("summary (%v) lacks %q:\n%s", err, want, got)
		}
	}

	// The coverage is part of every summary of a run that prepared upstream,
	// and it must describe the upstream commit the run prepared.
	if code, _, stderr := invoke(t, lookupFrom(nil), "summary", "--diff", diff, "--upstream", upstream, "--checked-at", "2026-09-28T03:00:12Z"); code != 2 {
		t.Errorf("a summary without --report exited %d: %s", code, stderr)
	}

	other := writeReport(t, dir, strings.Repeat("f", 40))
	if code, _, stderr := invoke(t, lookupFrom(nil), "summary", "--diff", diff, "--report", other, "--upstream", upstream,
		"--checked-at", "2026-09-28T03:00:12Z"); code != 2 || !strings.Contains(stderr, "describes upstream commit") {
		t.Errorf("a report of another upstream commit exited %d: %s", code, stderr)
	}

	changed := writeFile(t, dir, "changed.json",
		`{"hasChanges":true,"added":["two"],"changed":[],"metadataChanged":[],"held":[],"excluded":[],"unchanged":4}`)
	for flag, want := range map[string]string{
		"--publish=true": "this run publishes a new revision", "--publish=false": "publication is not enabled for this run",
	} {
		code, stdout, stderr := invoke(t, lookupFrom(nil), "summary", "--diff", changed, "--report", report, "--upstream", upstream,
			"--checked-at", "2026-09-28T03:00:12Z", flag)
		if code != 0 || !strings.Contains(stdout, want) {
			t.Errorf("summary %s exited %d (%s):\n%s", flag, code, stderr, stdout)
		}
	}

	old := writeFile(t, dir, "old.json", `{"revisionHint":null,"added":[],"changed_ids":[],"removedUpstream":[],"unchanged":0,"changed":false}`)
	if code, _, stderr := invoke(t, lookupFrom(nil), "summary", "--diff", old, "--report", report, "--upstream", upstream,
		"--checked-at", "2026-09-28T03:00:12Z"); code != 2 {
		t.Errorf("a diff in the old format exited %d: %s", code, stderr)
	}

	statePath, st := writeState(t, dir)
	status := writeFile(t, dir, "status.json", `{"revision":"`+st.Catalog.Revision+`","latest":"unknown","missing":["release catalog-`+st.Catalog.Revision+
		`"],"recorded":true,"tag":true,"release":false,"pending":true}`)

	code, stdout, stderr := invoke(t, lookupFrom(nil), "summary", "--state", statePath, "--status", status, "--checked-at", "2026-09-28T03:00:12Z")
	if code != 0 || !strings.Contains(stdout, "(missing: `release catalog-20260924.0905`)") ||
		!strings.Contains(stdout, ": 1 added, 0 changed, 0 metadata updated, 0 excluded, 0 unchanged (0 of them held).") {
		t.Errorf("finish summary exited %d (%s):\n%s", code, stderr, stdout)
	}

	for _, extra := range []string{"--publish=false", "--report=" + report} {
		if code, _, stderr := invoke(t, lookupFrom(nil), "summary", "--state", statePath, "--status", status, "--checked-at", "2026-09-28T03:00:12Z",
			extra); code != 2 {
			t.Errorf("a finish summary with %s exited %d: %s", extra, code, stderr)
		}
	}

	finished := writeFile(t, dir, "finished.json", `{"revision":"`+st.Catalog.Revision+`","latest":"current","recorded":true,"tag":true,"release":true,"pending":false}`)
	if code, _, stderr := invoke(t, lookupFrom(nil), "summary", "--state", statePath, "--status", finished, "--checked-at", "2026-09-28T03:00:12Z"); code != 2 {
		t.Errorf("a finish summary of a finished revision exited %d: %s", code, stderr)
	}
}

func TestSetCommitCommand(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "schemastore.toml")
	source := filepath.Join("..", "..", "sources", "schemastore.toml")
	commit := "0123456789abcdef0123456789abcdef01234567"

	code, _, stderr := invoke(t, lookupFrom(nil), "set-commit", "--source", source, "--commit", commit, "--out", out)
	if code != 0 {
		t.Fatalf("set-commit exited %d: %s", code, stderr)
	}

	if data, err := os.ReadFile(out); err != nil || !strings.Contains(string(data), "\ncommit = \""+commit+"\"\n") {
		t.Errorf("output = %q, %v", data, err)
	}
}

// The publish job regenerates the source description from its checkout and
// the upstream commit the published state records; it never commits the
// copy the prepare job wrote while processing untrusted upstream data.
func TestSetCommitFromTheState(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join("..", "..", "sources", "schemastore.toml")
	statePath, st := writeState(t, dir)
	recorded := st.Source.Commit

	checkout, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}

	for name, args := range map[string][]string{
		"the state alone":            {"--state", statePath},
		"the state and its commit":   {"--state", statePath, "--commit", recorded},
		"the flags in another order": {"--commit", recorded, "--state", statePath},
	} {
		out := filepath.Join(t.TempDir(), "schemastore.toml")

		code, _, stderr := invoke(t, lookupFrom(nil), append([]string{"set-commit", "--source", source, "--out", out}, args...)...)
		if code != 0 {
			t.Fatalf("%s: set-commit exited %d: %s", name, code, stderr)
		}

		want := regexp.MustCompile(`(?m)^commit = "[0-9a-f]{40}"$`).ReplaceAllString(string(checkout), `commit = "`+recorded+`"`)
		if data, err := os.ReadFile(out); err != nil || string(data) != want {
			t.Errorf("%s: output = %q, %v", name, data, err)
		}
	}

	local := &state.State{
		FormatVersion: state.FormatVersion, Source: state.Source{Kind: state.KindLocal, Name: "local"}, Recipe: st.Recipe, Catalog: st.Catalog,
		Schemas: st.Schemas,
	}

	localPath := filepath.Join(dir, "local.json")
	if err := state.Save(localPath, local); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "out.toml")

	for name, tc := range map[string]struct {
		args []string
		code int
	}{
		"another commit than the state's": {[]string{"--state", statePath, "--commit", strings.Repeat("f", 40)}, 5},
		"a local source":                  {[]string{"--state", localPath}, 2},
		"a missing state":                 {[]string{"--state", filepath.Join(dir, "missing.json")}, 2},
		"neither commit nor state":        {nil, 2},
	} {
		code, _, stderr := invoke(t, lookupFrom(nil), append([]string{"set-commit", "--source", source, "--out", out}, tc.args...)...)
		if code != tc.code {
			t.Errorf("%s: exited %d, want %d: %s", name, code, tc.code, stderr)
		}

		if _, err := os.Stat(out); err == nil {
			t.Fatalf("%s: set-commit wrote %s", name, out)
		}
	}
}
