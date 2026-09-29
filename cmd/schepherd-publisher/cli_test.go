package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/buildinfo"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/bundle"
	"github.com/ovineko/schepherd/internal/publisher/prepare"
	"github.com/ovineko/schepherd/internal/publisher/publish"
	"github.com/ovineko/schepherd/internal/publisher/state"
	"github.com/ovineko/schepherd/internal/registry"
)

var localBasic = filepath.Join("..", "..", "testdata", "publisher", "local-basic")

type cliResult struct {
	stdout string
	stderr string
	code   int
}

func runCLI(t *testing.T, args ...string) cliResult {
	t.Helper()

	var stdout, stderr bytes.Buffer

	code := run(t.Context(), args, &stdout, &stderr)

	return cliResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func (r cliResult) want(t *testing.T, code int) cliResult {
	t.Helper()

	if r.code != code {
		t.Fatalf("exit code %d, want %d\nstdout:\n%s\nstderr:\n%s", r.code, code, r.stdout, r.stderr)
	}

	return r
}

func decode[T any](t *testing.T, r cliResult) T {
	t.Helper()

	var v T
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatalf("decode %q: %v", r.stdout, err)
	}

	return v
}

// bundler returns the pinned bundler installed in the module; preparation
// cannot be tested without it.
func bundler(t *testing.T) string {
	t.Helper()

	name := "jsonschema"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}

	path, err := filepath.Abs(filepath.Join("..", "..", ".tools", "bin", name))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := bundle.FindTool(path, bundle.PinnedVersion); err != nil {
		t.Fatalf("the pinned JSON Schema CLI is unusable (install it with 'go run ./tools/install-jsonschema' from the module root): %v", err)
	}

	return path
}

func prepareDir(t *testing.T, source string, extra ...string) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "prepared")
	args := append([]string{"prepare", "--quiet", "--source", source, "--out", out, "--jsonschema", bundler(t), "--json"}, extra...)
	runCLI(t, args...).want(t, 0)

	return out
}

func writeRegistryConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "registries.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

func plainHTTPConfig(t *testing.T, reg *testRegistry) string {
	t.Helper()

	return writeRegistryConfig(t, "[registries.\""+reg.host()+"\"]\nplain_http = true\n")
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()

	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPrepareLocalSource(t *testing.T) {
	out := filepath.Join(t.TempDir(), "prepared")
	res := runCLI(t, "prepare", "--source", filepath.Join(localBasic, "source.toml"), "--out", out, "--jsonschema", bundler(t), "--json").want(t, 0)

	got := decode[prepare.Result](t, res)
	if got.Totals.Entries != 4 || got.Totals.Bundled != 1 || got.Source.Kind != prepare.KindLocal || len(got.Regressions) != 0 {
		t.Errorf("result = %+v", got)
	}

	if !strings.Contains(res.stderr, "schepherd-publisher: prepared 4 entries") {
		t.Errorf("stderr = %q", res.stderr)
	}

	if _, err := prepare.Load(out); err != nil {
		t.Errorf("prepared set: %v", err)
	}

	text := filepath.Join(t.TempDir(), "text")
	res = runCLI(t, "prepare", "--quiet", "--source", filepath.Join(localBasic, "source.toml"), "--out", text, "--jsonschema", bundler(t)).want(t, 0)

	if !strings.HasPrefix(res.stdout, "prepared 4 entries (0 reusing their recorded artifact) from 4 records into ") || res.stderr != "" ||
		!strings.Contains(res.stdout, "entries: 1 bundled (1 behaviour-compared, 0 structural-only), 3 compacted only\n") {
		t.Errorf("text output = %q, stderr = %q", res.stdout, res.stderr)
	}
}

func TestPrepareRegressionFailsAfterWriting(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"source.toml": `kind = "local"
name = "regression"

[[entries]]
name = "Unlicensed"
url = "https://schemas.example/unlicensed.json"
file = "unlicensed.json"
`,
		"unlicensed.json": `{"type":"string"}`,
	}

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out := filepath.Join(dir, "out")
	res := runCLI(t, "prepare", "--source", filepath.Join(dir, "source.toml"), "--out", out, "--jsonschema", bundler(t), "--json").want(t, 5)

	got := decode[prepare.Result](t, res)
	if len(got.Regressions) != 1 || got.Regressions[0].Status != prepare.StatusPendingReview {
		t.Errorf("regressions = %+v", got.Regressions)
	}

	if !strings.Contains(res.stderr, "integrity error: prepare ") || !strings.Contains(res.stderr, "1 entries of the local source were not prepared") {
		t.Errorf("stderr = %q", res.stderr)
	}

	if _, err := os.Stat(filepath.Join(out, prepare.ReportFile)); err != nil {
		t.Errorf("the report was not written: %v", err)
	}
}

// changedGamma copies local-basic with a changed gamma schema and returns
// its source file.
func changedGamma(t *testing.T) string {
	t.Helper()

	set := filepath.Join(t.TempDir(), "set")
	copyTree(t, localBasic, set)

	gamma := filepath.Join(set, "schemas", "gamma.json")

	data, err := os.ReadFile(gamma)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(gamma, bytes.Replace(data, []byte(`"type"`), []byte(`"title":"Changed","type"`), 1), 0o644); err != nil {
		t.Fatal(err)
	}

	return filepath.Join(set, "source.toml")
}

// TestWeeklyRoundTrip runs prepare, diff and publish the way the weekly
// update does, with the state file standing in for catalog/state.json.
func TestWeeklyRoundTrip(t *testing.T) {
	reg := newTestRegistry(t)
	config := plainHTTPConfig(t, reg)
	repo := reg.host() + "/org/schemas"
	stateFile := filepath.Join(t.TempDir(), "catalog", "state.json")

	publishArgs := func(dir, now string, extra ...string) []string {
		return append([]string{
			"publish", "--prepared", dir, "--repository", repo, "--registry-config", config, "--now", now,
			"--state", stateFile, "--state-out", stateFile, "--json",
		}, extra...)
	}

	prepared := prepareDir(t, filepath.Join(localBasic, "source.toml"), "--state", stateFile)

	d := decode[publish.Diff](t, runCLI(t, "diff", "--prepared", prepared, "--state", stateFile, "--json").want(t, 0))
	if !d.HasChanges || len(d.Added) != 4 || len(d.Changed)+len(d.MetadataChanged)+len(d.Held)+len(d.Excluded) != 0 {
		t.Errorf("first diff = %+v", d)
	}

	first := decode[publish.Result](t, runCLI(t, publishArgs(prepared, "20260923.0300", "--update-latest")...).want(t, 0))
	if first.Status != publish.StatusPublished || first.Revision != "20260923.0300" || first.Repository != repo ||
		first.UploadedSchemas != 4 || reg.tag("org/schemas", publish.LatestTag) != first.CatalogDigest {
		t.Fatalf("first publication = %+v", first)
	}

	recorded, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}

	written, err := state.Parse(recorded)
	if err != nil || written.Catalog.Digest != first.CatalogDigest || written.Catalog.Revision != first.Revision {
		t.Fatalf("state = %+v, %v", written, err)
	}

	again := prepareDir(t, filepath.Join(localBasic, "source.toml"), "--state", stateFile)

	if set, err := prepare.Load(again); err != nil || len(set.Schemas) != 0 || len(set.Document.Entries) != 4 {
		t.Errorf("unchanged sources are not reused: %v, %d prepared schema files", err, len(set.Schemas))
	}

	text := runCLI(t, "diff", "--prepared", again, "--state", stateFile).want(t, 0)
	if text.stdout != "catalog changes: false\nadded: 0\nchanged: 0\nmetadata changed: 0\nheld: 0\nexcluded: 0\nunchanged: 4\n" {
		t.Errorf("unchanged diff = %q", text.stdout)
	}

	writes, requests := reg.writeCount(), reg.requestCount()

	noop := decode[publish.Result](t, runCLI(t, publishArgs(again, "20260930.0300", "--update-latest")...).want(t, 0))
	if noop.Status != publish.StatusNoop || noop.CatalogDigest != first.CatalogDigest || reg.writeCount() != writes {
		t.Errorf("unchanged publication = %+v, %d writes", noop, reg.writeCount()-writes)
	}

	// catalog-latest already points at the state's catalog; resolving it is
	// the only request.
	if n := reg.requestCount() - requests; n > 3 {
		t.Errorf("the noop sent %d registry requests", n)
	}

	if after, _ := os.ReadFile(stateFile); !bytes.Equal(after, recorded) {
		t.Error("the noop changed the state file")
	}

	changed := prepareDir(t, changedGamma(t), "--state", stateFile)

	d = decode[publish.Diff](t, runCLI(t, "diff", "--prepared", changed, "--state", stateFile, "--json").want(t, 0))
	if !d.HasChanges || !slices.Equal(d.Changed, []string{"gamma"}) || d.Unchanged != 3 {
		t.Errorf("diff after the change = %+v", d)
	}

	second := decode[publish.Result](t, runCLI(t, publishArgs(changed, "20261007.0300", "--update-latest")...).want(t, 0))
	if second.Status != publish.StatusPublished || second.Revision != "20261007.0300" || second.UploadedSchemas != 1 ||
		!slices.Equal(second.Changed, []string{"gamma"}) || second.Unchanged != 3 || second.ReusedSchemas != 3 {
		t.Errorf("second publication = %+v", second)
	}

	next, err := state.Load(stateFile)
	if err != nil || next.Catalog.Revision != "20261007.0300" || !slices.Equal(ids(next), ids(written)) {
		t.Errorf("state after the change = %+v, %v", next, err)
	}

	plain := runCLI(t, "publish", "--prepared", changed, "--repository", repo, "--registry-config", config, "--state", stateFile).want(t, 0)
	if !strings.HasPrefix(plain.stdout, "status: noop\n") {
		t.Errorf("text output = %q", plain.stdout)
	}
}

func ids(s *state.State) []string {
	out := make([]string, 0, len(s.Schemas))
	for _, rec := range s.Schemas {
		out = append(out, rec.ID)
	}

	return out
}

// A schema upstream no longer lists is held: prepare reports it, a hold
// alone publishes nothing, and the next publication keeps its entry and
// records the hold in the state.
func TestRemovedUpstreamThroughTheCLI(t *testing.T) {
	reg := newTestRegistry(t)
	config := plainHTTPConfig(t, reg)
	repo := reg.host() + "/org/schemas"
	stateFile := filepath.Join(t.TempDir(), "state.json")

	prepared := prepareDir(t, filepath.Join(localBasic, "source.toml"), "--state", stateFile)
	runCLI(t, "publish", "--prepared", prepared, "--repository", repo, "--registry-config", config, "--now", "20260923.0300",
		"--state", stateFile, "--state-out", stateFile, "--quiet").want(t, 0)

	recorded, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}

	source := changedGamma(t)

	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}

	delta := "[[entries]]\nid = \"delta-schema\"\nname = \"Delta\"\nurl = \"https://schemas.example/delta.json\"\n" +
		"file = \"schemas/delta.json\"\n\n"
	if !strings.Contains(string(data), delta) {
		t.Fatalf("local-basic has no delta entry:\n%s", data)
	}

	if err := os.WriteFile(source, []byte(strings.Replace(string(data), delta, "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}

	gamma := filepath.Join(filepath.Dir(source), "schemas", "gamma.json")

	changedBytes, err := os.ReadFile(gamma)
	if err != nil {
		t.Fatal(err)
	}

	original, err := os.ReadFile(filepath.Join(localBasic, "schemas", "gamma.json"))
	if err != nil {
		t.Fatal(err)
	}

	writeGamma := func(content []byte) {
		t.Helper()

		if err := os.WriteFile(gamma, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeGamma(original)

	res := decode[prepare.Result](t, runCLI(t, "prepare", "--quiet", "--source", source, "--out", filepath.Join(t.TempDir(), "out"),
		"--jsonschema", bundler(t), "--state", stateFile, "--json").want(t, 0))

	held := []prepare.Hold{{ID: "delta-schema", Reason: state.HeldRemovedUpstream}}
	if !reflect.DeepEqual(res.Held, held) || len(res.Excluded)+len(res.Regressions) != 0 || res.Totals.Reused != 3 {
		t.Fatalf("prepare result = %+v", res)
	}

	stateless := prepareDir(t, source)
	if other := runCLI(t, "diff", "--prepared", stateless, "--state", stateFile).want(t, 2); !strings.Contains(other.stderr,
		"not prepared with this publisher state: delta-schema of the recorded catalog is neither an entry nor held nor excluded") {
		t.Errorf("a set prepared without the state: stderr = %q", other.stderr)
	}

	publishJSON := func(dir, now string) publish.Result {
		t.Helper()

		return decode[publish.Result](t, runCLI(t, "publish", "--prepared", dir, "--repository", repo, "--registry-config", config,
			"--now", now, "--state", stateFile, "--state-out", stateFile, "--json").want(t, 0))
	}

	alone := publishJSON(res.OutDir, "20260930.0300")
	if alone.Status != publish.StatusNoop || !slices.Equal(alone.RemovedUpstream, []string{"delta-schema"}) ||
		!reflect.DeepEqual(alone.Held, []publish.Hold{{ID: "delta-schema", Reason: state.HeldRemovedUpstream}}) {
		t.Errorf("a hold alone = %+v", alone)
	}

	if after, _ := os.ReadFile(stateFile); !bytes.Equal(after, recorded) {
		t.Error("a hold alone changed the state")
	}

	writeGamma(changedBytes)

	res = decode[prepare.Result](t, runCLI(t, "prepare", "--quiet", "--source", source, "--out", filepath.Join(t.TempDir(), "out"),
		"--jsonschema", bundler(t), "--state", stateFile, "--json").want(t, 0))

	pub := publishJSON(res.OutDir, "20261007.0300")
	if pub.Status != publish.StatusPublished || !slices.Equal(pub.RemovedUpstream, []string{"delta-schema"}) || pub.UploadedSchemas != 1 ||
		!slices.Equal(pub.Changed, []string{"gamma"}) {
		t.Errorf("publication = %+v", pub)
	}

	next, err := state.Load(stateFile)
	if err != nil {
		t.Fatal(err)
	}

	if rec, ok := next.Lookup("delta-schema"); !ok || rec.HeldSinceRevision != "20261007.0300" || rec.HeldReason != state.HeldRemovedUpstream {
		t.Errorf("delta record = %+v", rec)
	}
}

// A takedown works for a schema upstream no longer lists: the exclude rule
// removes it from the catalog instead of leaving it held as removed
// upstream, and nothing calls the exclusion a removal upstream.
func TestTakedownOfARemovedSchemaThroughTheCLI(t *testing.T) {
	reg := newTestRegistry(t)
	config := plainHTTPConfig(t, reg)
	repo := reg.host() + "/org/schemas"
	stateFile := filepath.Join(t.TempDir(), "state.json")
	set := filepath.Join(t.TempDir(), "set")
	copyTree(t, localBasic, set)

	source, licenses := filepath.Join(set, "source.toml"), filepath.Join(set, "licenses.toml")
	publishJSON := func(dir, now string) publish.Result {
		t.Helper()

		return decode[publish.Result](t, runCLI(t, "publish", "--prepared", dir, "--repository", repo, "--registry-config", config,
			"--now", now, "--state", stateFile, "--state-out", stateFile, "--json").want(t, 0))
	}

	publishJSON(prepareDir(t, source, "--state", stateFile), "20260923.0300")

	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}

	delta := "[[entries]]\nid = \"delta-schema\"\nname = \"Delta\"\nurl = \"https://schemas.example/delta.json\"\n" +
		"file = \"schemas/delta.json\"\n\n"
	if !strings.Contains(string(data), delta) {
		t.Fatalf("local-basic has no delta entry:\n%s", data)
	}

	policy, err := os.ReadFile(licenses)
	if err != nil {
		t.Fatal(err)
	}

	takedown := "\n[[rules]]\nid = \"takedown\"\ndecision = \"exclude\"\nurls = [\"https://schemas.example/delta.json\"]\nreason = \"takedown\"\n"
	if err := os.WriteFile(source, []byte(strings.Replace(string(data), delta, "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(licenses, append(policy, takedown...), 0o644); err != nil {
		t.Fatal(err)
	}

	res := decode[prepare.Result](t, runCLI(t, "prepare", "--quiet", "--source", source, "--out", filepath.Join(t.TempDir(), "out"),
		"--jsonschema", bundler(t), "--state", stateFile, "--json").want(t, 0))
	if !reflect.DeepEqual(res.Excluded, []prepare.Exclusion{{ID: "delta-schema", Rule: "takedown"}}) || len(res.Held)+len(res.Regressions) != 0 {
		t.Fatalf("prepare result = %+v", res)
	}

	d := decode[publish.Diff](t, runCLI(t, "diff", "--prepared", res.OutDir, "--state", stateFile, "--json").want(t, 0))
	if !d.HasChanges || !slices.Equal(d.Excluded, []string{"delta-schema"}) || len(d.Held) != 0 || d.Unchanged != 3 {
		t.Errorf("diff = %+v", d)
	}

	pub := publishJSON(res.OutDir, "20260930.0300")
	if pub.Status != publish.StatusPublished || !slices.Equal(pub.Excluded, []string{"delta-schema"}) ||
		len(pub.Held)+len(pub.RemovedUpstream)+pub.UploadedSchemas != 0 {
		t.Errorf("publication = %+v", pub)
	}

	next, err := state.Load(stateFile)
	if err != nil {
		t.Fatal(err)
	}

	if rec, ok := next.Lookup("delta-schema"); !ok || rec.ExcludedRevision != "20260930.0300" || rec.Held() {
		t.Errorf("delta record = %+v", rec)
	}
}

// The test job of the weekly workflow (.github/workflows/update-schemas.yml)
// publishes the prepared set against the recorded state, publishes it again
// against the state that wrote (a noop with the same state), diffs it with
// that state (no changes), resumes the first publication and points
// catalog-latest at the result. A week with a change, a hold and an
// exclusion must pass every step.
func TestWeeklyTestJobSequenceThroughTheCLI(t *testing.T) {
	reg := newTestRegistry(t)
	config := plainHTTPConfig(t, reg)
	repo := reg.host() + "/org/schemas"
	dir := t.TempDir()
	base := filepath.Join(dir, "base-state.json")

	publishArgs := func(prepared, now, stateIn, stateOut string, extra ...string) []string {
		return append([]string{
			"publish", "--prepared", prepared, "--repository", repo, "--registry-config", config, "--now", now,
			"--state", stateIn, "--state-out", stateOut, "--json",
		}, extra...)
	}

	runCLI(t, publishArgs(prepareDir(t, filepath.Join(localBasic, "source.toml"), "--state", base), "20260923.0300", base, base)...).want(t, 0)

	source := changedGamma(t)
	set := filepath.Dir(source)

	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}

	delta := "[[entries]]\nid = \"delta-schema\"\nname = \"Delta\"\nurl = \"https://schemas.example/delta.json\"\n" +
		"file = \"schemas/delta.json\"\n\n"
	if !strings.Contains(string(data), delta) {
		t.Fatalf("local-basic has no delta entry:\n%s", data)
	}

	if err := os.WriteFile(source, []byte(strings.Replace(string(data), delta, "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}

	policy, err := os.ReadFile(filepath.Join(set, "licenses.toml"))
	if err != nil {
		t.Fatal(err)
	}

	takedown := "\n[[rules]]\nid = \"takedown\"\ndecision = \"exclude\"\nurls = [\"https://schemas.example/beta.json\"]\nreason = \"takedown\"\n"
	if err := os.WriteFile(filepath.Join(set, "licenses.toml"), append(policy, takedown...), 0o644); err != nil {
		t.Fatal(err)
	}

	prepared := prepareDir(t, source, "--state", base)
	first := filepath.Join(dir, "state-1.json")

	published := decode[publish.Result](t, runCLI(t, publishArgs(prepared, "20260930.0300", base, first, "--update-latest")...).want(t, 0))
	if published.Status != publish.StatusPublished || !slices.Equal(published.Changed, []string{"gamma"}) ||
		!slices.Equal(published.Excluded, []string{"beta"}) || !slices.Equal(published.RemovedUpstream, []string{"delta-schema"}) {
		t.Fatalf("first = %+v", published)
	}

	second := filepath.Join(dir, "state-2.json")

	noop := decode[publish.Result](t, runCLI(t, publishArgs(prepared, "20261001.0300", first, second)...).want(t, 0))
	if noop.Status != publish.StatusNoop || noop.Revision != published.Revision {
		t.Errorf("second = %+v", noop)
	}

	if a, b := mustRead(t, first), mustRead(t, second); !bytes.Equal(a, b) {
		t.Errorf("the noop wrote another state:\n%s\nwant\n%s", b, a)
	}

	if d := decode[publish.Diff](t, runCLI(t, "diff", "--prepared", prepared, "--state", first, "--json").want(t, 0)); d.HasChanges {
		t.Errorf("diff against the written state = %+v", d)
	}

	third := filepath.Join(dir, "state-3.json")

	resumed := decode[publish.Result](t, runCLI(t, publishArgs(prepared, "20261002.0300", base, third, "--update-latest")...).want(t, 0))
	if resumed.Status != publish.StatusResumed || resumed.Revision != published.Revision {
		t.Errorf("third = %+v", resumed)
	}

	if a, b := mustRead(t, first), mustRead(t, third); !bytes.Equal(a, b) {
		t.Errorf("the resumed publication wrote another state:\n%s\nwant\n%s", b, a)
	}

	latest := decode[publish.LatestResult](t, runCLI(t, "latest", "--repository", repo, "--registry-config", config, "--state", third,
		"--json").want(t, 0))
	if latest.Latest != publish.LatestCurrent || latest.CatalogDigest != published.CatalogDigest {
		t.Errorf("latest = %+v", latest)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

func TestSameMinuteThroughTheCLI(t *testing.T) {
	reg := newTestRegistry(t)
	config := plainHTTPConfig(t, reg)
	repo := reg.host() + "/org/schemas"

	runCLI(t, "publish", "--quiet", "--prepared", prepareDir(t, filepath.Join(localBasic, "source.toml")), "--repository", repo,
		"--registry-config", config, "--now", "20260923.0300").want(t, 0)

	writes := reg.writeCount()

	res := runCLI(t, "publish", "--prepared", prepareDir(t, changedGamma(t)), "--repository", repo, "--registry-config", config,
		"--now", "20260923.0300", "--json").want(t, 2)
	if res.stdout != "" || !strings.Contains(res.stderr, "catalog revision 20260923.0300 already exists") {
		t.Errorf("stdout = %q, stderr = %q", res.stdout, res.stderr)
	}

	if reg.writeCount() != writes {
		t.Errorf("the refused publication made %d writes", reg.writeCount()-writes)
	}
}

// latest points catalog-latest at the catalog of a recorded state without a
// prepared set, after checking that the registry holds exactly that catalog.
func TestLatestThroughTheCLI(t *testing.T) {
	reg := newTestRegistry(t)
	config := plainHTTPConfig(t, reg)
	repo := reg.host() + "/org/schemas"
	stateFile := filepath.Join(t.TempDir(), "state.json")

	first := decode[publish.Result](t, runCLI(t, "publish", "--prepared", prepareDir(t, filepath.Join(localBasic, "source.toml")),
		"--repository", repo, "--registry-config", config, "--now", "20260923.0300", "--state", stateFile, "--state-out", stateFile,
		"--json").want(t, 0))
	if reg.tag("org/schemas", publish.LatestTag) != "" {
		t.Fatal("publish without --update-latest moved catalog-latest")
	}

	latestArgs := []string{"latest", "--repository", repo, "--registry-config", config, "--state", stateFile}
	writes := reg.writeCount()

	checked := decode[publish.LatestResult](t, runCLI(t, append(latestArgs, "--check", "--json")...).want(t, 0))
	if checked.Latest != publish.LatestChecked || checked.Revision != first.Revision || checked.CatalogDigest != first.CatalogDigest ||
		reg.writeCount() != writes {
		t.Errorf("check = %+v, %d writes", checked, reg.writeCount()-writes)
	}

	moved := decode[publish.LatestResult](t, runCLI(t, append(latestArgs, "--json")...).want(t, 0))
	if moved.Latest != publish.LatestMoved || reg.tag("org/schemas", publish.LatestTag) != first.CatalogDigest {
		t.Errorf("move = %+v, catalog-latest %s", moved, reg.tag("org/schemas", publish.LatestTag))
	}

	text := runCLI(t, latestArgs...).want(t, 0)
	if !strings.HasPrefix(text.stdout, "catalog-latest: current\n") || !strings.Contains(text.stdout, "revision: 20260923.0300\n") {
		t.Errorf("text output = %q", text.stdout)
	}

	edited, err := state.Load(stateFile)
	if err != nil {
		t.Fatal(err)
	}

	edited.Schemas[0].Entry.Name = "Edited by hand"

	editedFile := filepath.Join(t.TempDir(), "edited.json")
	if err := state.Save(editedFile, edited); err != nil {
		t.Fatal(err)
	}

	writes = reg.writeCount()

	res := runCLI(t, "latest", "--repository", repo, "--registry-config", config, "--state", editedFile).want(t, 5)
	if !strings.Contains(res.stderr, "does not list the entries the state records") || reg.writeCount() != writes {
		t.Errorf("an edited state: stderr %q, %d writes", res.stderr, reg.writeCount()-writes)
	}

	missing := runCLI(t, "latest", "--repository", repo, "--registry-config", config,
		"--state", filepath.Join(t.TempDir(), "none.json")).want(t, 2)
	if !strings.Contains(missing.stderr, "does not exist") {
		t.Errorf("a missing state: %q", missing.stderr)
	}
}

func TestPlainHTTPIsOptIn(t *testing.T) {
	reg := newTestRegistry(t)
	prepared := prepareDir(t, filepath.Join(localBasic, "source.toml"))

	res := runCLI(t, "publish", "--prepared", prepared, "--repository", reg.host()+"/org/schemas").want(t, 4)
	if reg.writeCount() != 0 || !strings.Contains(res.stderr, "registry error") {
		t.Errorf("stderr = %q, %d writes", res.stderr, reg.writeCount())
	}
}

func TestUsageErrors(t *testing.T) {
	prepared := t.TempDir()
	source := filepath.Join(localBasic, "source.toml")
	badKey := writeRegistryConfig(t, "[registries.\"example.com\"]\nplain_https = true\n")
	badHost := writeRegistryConfig(t, "[registries.\"https://example.com\"]\nplain_http = true\n")
	missingCA := writeRegistryConfig(t, "[registries.\"example.com\"]\nca_file = \"missing.pem\"\n")

	badState := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(badState, []byte(`{"formatVersion":2}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := map[string][]string{
		"no command arguments":    {"prepare", "extra"},
		"prepare without source":  {"prepare", "--out", t.TempDir()},
		"prepare without out":     {"prepare", "--source", source},
		"bad jobs":                {"prepare", "--source", source, "--out", filepath.Join(t.TempDir(), "o"), "--jobs", "0"},
		"missing bundler":         {"prepare", "--source", source, "--out", filepath.Join(t.TempDir(), "o"), "--jsonschema", filepath.Join(t.TempDir(), "nope")},
		"prepare bad state":       {"prepare", "--source", source, "--out", filepath.Join(t.TempDir(), "o"), "--state", badState},
		"prepare state is a dir":  {"prepare", "--source", source, "--out", filepath.Join(t.TempDir(), "o"), "--state", t.TempDir()},
		"removed previous flag":   {"prepare", "--source", source, "--out", filepath.Join(t.TempDir(), "o"), "--previous-catalog", "c.json"},
		"export is gone":          {"export-previous", "--repository", "example.com/x", "--tag", "catalog-latest", "--out", "x.json"},
		"unknown flag":            {"publish", "--nope"},
		"publish without repo":    {"publish", "--prepared", prepared},
		"removed date flag":       {"publish", "--prepared", prepared, "--repository", "example.com/x", "--date", "20260923"},
		"removed previous tag":    {"publish", "--prepared", prepared, "--repository", "example.com/x", "--previous-tag", "catalog-latest"},
		"removed dry run":         {"publish", "--prepared", prepared, "--repository", "example.com/x", "--dry-run"},
		"old revision grammar":    {"publish", "--prepared", prepared, "--repository", "example.com/x", "--now", "20260923.1"},
		"impossible date":         {"publish", "--prepared", prepared, "--repository", "example.com/x", "--now", "20260230.1200"},
		"hour 24":                 {"publish", "--prepared", prepared, "--repository", "example.com/x", "--now", "20260923.2400"},
		"state-out without state": {"publish", "--prepared", prepared, "--repository", "example.com/x", "--state-out", filepath.Join(t.TempDir(), "s.json")},
		"state-out is a dir": {
			"publish", "--prepared", prepared, "--repository", "example.com/x", "--state", badState, "--state-out", t.TempDir(),
		},
		"publish bad state":    {"publish", "--prepared", prepared, "--repository", "example.com/x", "--state", badState},
		"bad repository":       {"publish", "--prepared", prepared, "--repository", "Example.com/x"},
		"unknown config key":   {"publish", "--prepared", prepared, "--repository", "example.com/x", "--registry-config", badKey},
		"bad config host":      {"publish", "--prepared", prepared, "--repository", "example.com/x", "--registry-config", badHost},
		"missing ca file":      {"publish", "--prepared", prepared, "--repository", "example.com/x", "--registry-config", missingCA},
		"missing config":       {"publish", "--prepared", prepared, "--repository", "example.com/x", "--registry-config", filepath.Join(t.TempDir(), "nope.toml")},
		"diff without state":   {"diff", "--prepared", prepared},
		"diff without set":     {"diff", "--state", badState},
		"diff bad state":       {"diff", "--prepared", prepared, "--state", badState},
		"diff empty set":       {"diff", "--prepared", prepared, "--state", filepath.Join(t.TempDir(), "missing.json")},
		"latest without state": {"latest", "--repository", "example.com/x"},
		"latest without repo":  {"latest", "--state", badState},
		"latest bad state":     {"latest", "--repository", "example.com/x", "--state", badState},
		"latest missing state": {"latest", "--repository", "example.com/x", "--state", filepath.Join(t.TempDir(), "missing.json")},
		"latest arguments":     {"latest", "--repository", "example.com/x", "--state", badState, "extra"},
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			res := runCLI(t, args...).want(t, 2)
			if !strings.Contains(res.stderr, "usage error") || !strings.Contains(res.stderr, "--help") || res.stdout != "" {
				t.Errorf("stdout = %q, stderr = %q", res.stdout, res.stderr)
			}
		})
	}
}

func TestLoadRegistries(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{"ca.pem", "docker.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	path := filepath.Join(dir, "registries.toml")
	content := `[registries."localhost:5000"]
plain_http = true

[registries."registry.example"]
ca_file = "ca.pem"
credentials_file = "` + filepath.ToSlash(filepath.Join(dir, "docker.json")) + `"

[registries."registry.example/team/schemas"]
plain_http = true
`

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	hosts, err := loadRegistries(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(hosts) != 3 || !hosts["localhost:5000"].PlainHTTP || hosts["registry.example"].PlainHTTP ||
		hosts["registry.example"].CAFile != filepath.Join(dir, "ca.pem") ||
		hosts["registry.example"].CredentialsFile != filepath.Join(dir, "docker.json") ||
		hosts["registry.example/team/schemas"] != (registry.HostConfig{PlainHTTP: true}) {
		t.Errorf("hosts = %+v", hosts)
	}

	if hosts, err := loadRegistries(""); err != nil || len(hosts) != 0 {
		t.Errorf("no file: %v, %v", hosts, err)
	}

	if err := os.WriteFile(path, []byte("[registries.\"registry.example\"]\nca_file = \".\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := loadRegistries(path); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("directory as ca_file: %v", err)
	}

	for _, key := range []string{
		"https://registry.example", "registry.example/", "registry.example//team", "Registry.example",
		"registry.example/Team", "registry.example/team:tag", "registry.example:0", "",
	} {
		if err := os.WriteFile(path, []byte("[registries."+strconv.Quote(key)+"]\nplain_http = true\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := loadRegistries(path)
		if fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), "is not a registry host[:port] or host[:port]/path") {
			t.Errorf("key %q: %v", key, err)
		}
	}
}

func TestVersion(t *testing.T) {
	if res := runCLI(t, "version").want(t, 0); res.stdout != buildinfo.Get().Version+"\n" {
		t.Errorf("version = %q", res.stdout)
	}

	info := decode[buildinfo.Info](t, runCLI(t, "version", "--json").want(t, 0))
	if info.Version != buildinfo.Get().Version || info.GoVersion == "" {
		t.Errorf("version --json = %+v", info)
	}
}
