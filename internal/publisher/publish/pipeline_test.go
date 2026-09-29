package publish

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/publisher/bundle"
	"github.com/ovineko/schepherd/internal/publisher/httpfetch"
	"github.com/ovineko/schepherd/internal/publisher/licensedetect"
	"github.com/ovineko/schepherd/internal/publisher/policy"
	"github.com/ovineko/schepherd/internal/publisher/prepare"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

const (
	mitURL   = "https://raw.githubusercontent.com/owner/mit/main/mit.json"
	toolsURL = "https://raw.githubusercontent.com/owner/tools/main/tools.json"
	draft7   = "http://json-schema.org/draft-07/schema#"
	// movedURL is a GitHub raw file URL, which GitHub always serves through
	// a redirect to raw.githubusercontent.com with a qualified ref.
	movedURL    = "https://github.com/owner/moved/raw/main/moved.json"
	movedTarget = "https://raw.githubusercontent.com/owner/moved/refs/heads/main/moved.json"
	movedSchema = `{"type":"boolean","title":"Moved v1"}`
)

// fakeWeb stands in for public web hosts as the transport of the fetcher
// prepare builds: it serves documents and redirects by URL and counts the
// requests per URL.
type fakeWeb struct {
	docs      map[string]string
	redirects map[string]string
	hits      map[string]int
	mu        sync.Mutex
}

func (w *fakeWeb) RoundTrip(r *http.Request) (*http.Response, error) {
	key := r.URL.String()

	w.mu.Lock()
	w.hits[key]++
	location, redirected := w.redirects[key]
	body, found := w.docs[key]
	w.mu.Unlock()

	rec := httptest.NewRecorder()

	switch {
	case redirected:
		http.Redirect(rec, r, location, http.StatusFound)
	case found:
		_, _ = rec.WriteString(body)
	default:
		http.NotFound(rec, r)
	}

	resp := rec.Result()
	resp.Request = r

	return resp, nil
}

func (w *fakeWeb) hitCount(url string) int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.hits[url]
}

// fakeGitHub answers the two GitHub API requests license detection makes
// for a repository ref, counts every request per repository and can fail
// all of them.
type fakeGitHub struct {
	srv      *httptest.Server
	licenses map[string][2]string
	hits     map[string]int
	failing  bool
	mu       sync.Mutex
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()

	g := &fakeGitHub{licenses: map[string][2]string{}, hits: map[string]int{}}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)

	return g
}

func commitOf(repo string) string {
	sum := sha1.Sum([]byte(repo))

	return hex.EncodeToString(sum[:])
}

func (g *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/owner/"), "/")

	g.mu.Lock()
	g.hits[parts[0]]++
	failing := g.failing
	license, known := g.licenses[parts[0]]
	g.mu.Unlock()

	switch {
	case failing:
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	case !known:
		http.NotFound(w, r)
	case len(parts) == 3 && parts[1] == "commits" && parts[2] == "main":
		_, _ = w.Write([]byte(commitOf(parts[0])))
	case len(parts) == 2 && parts[1] == "license" && r.URL.Query().Get("ref") == commitOf(parts[0]):
		text := license[1]
		blob := sha1.New()
		_, _ = fmt.Fprintf(blob, "blob %d\x00%s", len(text), text)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"path": "LICENSE", "sha": hex.EncodeToString(blob.Sum(nil)), "size": len(text), "encoding": "base64",
			"content": base64.StdEncoding.EncodeToString([]byte(text)), "license": map[string]string{"spdx_id": license[0]},
		})
	default:
		http.NotFound(w, r)
	}
}

func (g *fakeGitHub) set(repo, spdx, text string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.licenses[repo] = [2]string{spdx, text}
}

func (g *fakeGitHub) fail(failing bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.failing = failing
}

// calls returns the requests per repository since the last call.
func (g *fakeGitHub) calls() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()

	hits := g.hits
	g.hits = map[string]int{}

	return hits
}

func (g *fakeGitHub) detector(t *testing.T) *licensedetect.Detector {
	t.Helper()

	d, err := licensedetect.New(licensedetect.Config{
		Hosts:     []string{policy.HostGitHubAPI},
		Endpoints: map[string]string{policy.HostGitHubAPI: g.srv.URL},
		Fetch:     httpfetch.Policy{AllowHTTP: true, AllowPrivateHosts: []string{g.srv.Listener.Addr().String()}},
	})
	if err != nil {
		t.Fatal(err)
	}

	return d
}

// pipeline runs the weekly update with prepare and publish: a local source
// of two schemas whose licenses automatic detection finds on the fake
// GitHub API, two a license rule allows and one served from GitHub through
// a redirect whose license detection finds as well, the state file standing
// in for catalog/state.json in Git.
type pipeline struct {
	repo   *weeklyRepo
	github *fakeGitHub
	web    *fakeWeb
	tool   *bundle.Tool
	dir    string
	git    string
}

func pinnedBundler(t *testing.T) *bundle.Tool {
	t.Helper()

	name := "jsonschema"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}

	path, err := filepath.Abs(filepath.Join("..", "..", "..", ".tools", "bin", name))
	if err != nil {
		t.Fatal(err)
	}

	tool, err := bundle.FindTool(path, bundle.PinnedVersion)
	if err != nil {
		t.Fatalf("the pinned JSON Schema CLI is unusable (install it with 'go run ./tools/install-jsonschema'): %v", err)
	}

	return tool
}

// pipelineSource is the source file; {store}, {storeMatch} and {extra}
// vary by week.
const pipelineSource = `kind = "local"
name = "weekly"
policy = "licenses.toml"

[[entries]]
id = "mit"
name = "MIT schema"
url = "` + mitURL + `"
file = "schemas/mit.json"
file_match = ["mit.json"]

[[entries]]
id = "tools"
name = "Tools"
url = "` + toolsURL + `"
file = "schemas/tools.json"

[[entries]]
id = "store"
name = "{store}"
url = "https://www.schemastore.org/store.json"
file = "schemas/store.json"
file_match = [{storeMatch}]

[[entries]]
id = "bundled"
name = "Bundled"
url = "https://www.schemastore.org/bundled.json"
file = "schemas/bundled.json"

[[entries]]
id = "moved"
name = "Moved"
url = "` + movedURL + `"

[[documents]]
uri = "https://www.schemastore.org/common.json"
file = "schemas/common.json"
`

const pipelinePolicy = `[auto]
enabled = true
hosts = ["api.github.com"]

[[rules]]
id = "store"
decision = "allow"
hosts = ["www.schemastore.org"]
license = "Apache-2.0"
notice = "{notice}"
reason = "store"
{extra}`

const bundledSchema = `{"$schema":"` + draft7 + `","properties":{"x":{"$ref":"https://www.schemastore.org/common.json#/definitions/x"}}}`

func newPipeline(t *testing.T) *pipeline {
	t.Helper()

	p := &pipeline{
		repo: newWeeklyRepo(t), github: newFakeGitHub(t), tool: pinnedBundler(t), dir: t.TempDir(),
		git: filepath.Join(t.TempDir(), "catalog", "state.json"),
		web: &fakeWeb{
			docs: map[string]string{movedTarget: movedSchema}, redirects: map[string]string{movedURL: movedTarget}, hits: map[string]int{},
		},
	}

	p.github.set("mit", "MIT", "MIT License\n\nCopyright (c) 2026 MIT Authors\n")
	p.github.set("tools", "MIT", "MIT License\n\nCopyright (c) 2026 Tool Authors\n")
	p.github.set("moved", "MIT", "MIT License\n\nCopyright (c) 2026 Moved Authors\n")

	p.source("Store", `"store.json"`)
	p.policy("Store notice.", "")
	p.write("schemas/mit.json", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","title":"MIT v1"}`)
	p.write("schemas/tools.json", `{"type":"string","title":"Tools v1"}`)
	p.write("schemas/store.json", `{"type":"integer"}`)
	p.write("schemas/bundled.json", bundledSchema)
	p.write("schemas/common.json", `{"$schema":"`+draft7+`","definitions":{"x":{"type":"string","minLength":1}}}`)

	return p
}

func (p *pipeline) write(name, content string) {
	path := filepath.Join(p.dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		panic(err)
	}
}

func (p *pipeline) source(storeName, storeMatch string) {
	p.write("source.toml", strings.NewReplacer("{store}", storeName, "{storeMatch}", storeMatch).Replace(pipelineSource))
}

func (p *pipeline) policy(notice, extra string) {
	p.write("licenses.toml", strings.NewReplacer("{notice}", notice, "{extra}", extra).Replace(pipelinePolicy))
}

// prepare runs prepare with the state in Git and returns its result, the
// prepared directory and the detection requests it made per repository.
func (p *pipeline) prepare(t *testing.T, change func(o *prepare.Options)) (*prepare.Result, string, map[string]int) {
	t.Helper()

	previous, err := state.Load(p.git)
	if err != nil {
		t.Fatal(err)
	}

	p.github.calls()

	out := filepath.Join(t.TempDir(), "prepared")
	opts := prepare.Options{
		Tool: p.tool, SourceFile: filepath.Join(p.dir, "source.toml"), OutDir: out, State: previous,
		LicenseDetector: p.github.detector(t), Transport: p.web, Jobs: 2,
	}

	if change != nil {
		change(&opts)
	}

	res, err := prepare.Run(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}

	if err := res.Check(); err != nil {
		t.Fatalf("prepare failed on a record: %v", err)
	}

	return res, out, p.github.calls()
}

// publish publishes like the weekly job and commits the new state to Git.
func (p *pipeline) publish(t *testing.T, dir string, now time.Time) *Result {
	t.Helper()

	p.repo.target.reset()

	res, out, err := p.repo.run(t, dir, p.git, now, true)
	if err != nil {
		t.Fatal(err)
	}

	commit(t, out, p.git)

	return res
}

func (p *pipeline) record(t *testing.T, id string) state.Schema {
	t.Helper()

	rec, ok := loadState(t, p.git).Lookup(id)
	if !ok {
		t.Fatalf("the state has no %s", id)
	}

	return *rec
}

func total(calls map[string]int) int {
	n := 0
	for _, c := range calls {
		n += c
	}

	return n
}

// mustPass stops a test whose steps depend on each other when one failed.
func mustPass(t *testing.T, passed bool) {
	t.Helper()

	if !passed {
		t.FailNow()
	}
}

func holds(pairs ...string) []Hold {
	out := []Hold{}
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, Hold{ID: pairs[i], Reason: pairs[i+1]})
	}

	return out
}

// TestWeeklyPipeline runs the fully autonomous weekly update week after
// week: prepare with the state in Git, publish what changed, commit the
// state. A published schema is never dropped and never fails the run; an
// unchanged one costs no license detection request, also when it is served
// through a redirect.
func TestWeeklyPipeline(t *testing.T) {
	p := newPipeline(t)
	monday := time.Date(2026, 9, 28, 3, 0, 7, 0, time.UTC)
	week := 7 * 24 * time.Hour

	var first *Result

	mustPass(t, t.Run("the first run publishes everything with its license decisions", func(t *testing.T) {
		res, dir, calls := p.prepare(t, nil)
		if res.Totals.Entries != 5 || res.Totals.Reused != 0 || calls["mit"] == 0 || calls["tools"] == 0 || calls["moved"] == 0 {
			t.Fatalf("prepare = %+v, detection %v", res.Totals, calls)
		}

		first = p.publish(t, dir, monday)
		if first.Status != StatusPublished || len(first.Added) != 5 || first.UploadedSchemas != 5 {
			t.Fatalf("result = %+v", first)
		}

		moved := p.record(t, "moved")
		if want := []state.Redirect{{URL: movedURL, Target: movedTarget, Digest: digest.FromBytes([]byte(movedSchema))}}; !reflect.DeepEqual(moved.License.Redirects, want) ||
			len(moved.License.Detections) != 2 || moved.License.Detections[0].URL != movedURL || moved.License.Detections[1].URL != movedTarget {
			t.Errorf("moved record = %+v", moved)
		}

		if mit := p.record(t, "mit"); len(mit.License.Detections) != 1 || mit.License.Detections[0].Source != "github:owner/mit@"+commitOf("mit") ||
			len(mit.License.Rules) != 0 || mit.Entry.Provenance.License != "MIT" {
			t.Errorf("mit record = %+v", mit)
		}

		if bundled := p.record(t, "bundled"); !slices.Equal(bundled.License.Rules, []string{"store"}) || len(bundled.License.Detections) != 0 ||
			len(bundled.Entry.Provenance.Dependencies) != 1 {
			t.Errorf("bundled record = %+v", bundled)
		}
	}))

	mustPass(t, t.Run("an unchanged week asks detection nothing and publishes nothing", func(t *testing.T) {
		recorded := readText(t, p.git)
		targetHits := p.web.hitCount(movedTarget)

		res, dir, calls := p.prepare(t, nil)
		if total(calls) != 0 {
			t.Fatalf("an unchanged week made detection requests: %v", calls)
		}

		set := loadSet(t, dir)
		if res.Totals.Reused != 5 || len(set.Schemas)+len(set.Notices) != 0 {
			t.Fatalf("prepare = %+v with %d schema files; every schema must reuse its artifact", res.Totals, len(set.Schemas))
		}

		if p.web.hitCount(movedTarget) != targetHits+1 {
			t.Error("the recorded redirect target was not fetched to compare its digest")
		}

		noop := p.publish(t, dir, monday.Add(week))
		if noop.Status != StatusNoop || len(p.repo.target.manifests)+len(p.repo.target.tags)+p.repo.target.blobs != 0 {
			t.Fatalf("result = %+v; registry writes: %v %v", noop, p.repo.target.manifests, p.repo.target.tags)
		}

		if readText(t, p.git) != recorded {
			t.Error("the state changed")
		}
	}))

	var metadataWeek *Result

	mustPass(t, t.Run("a metadata-only change updates the entry and keeps the artifact", func(t *testing.T) {
		before := p.record(t, "store")
		p.source("Store v2", `"store.json", "store.yaml"`)

		res, dir, calls := p.prepare(t, nil)
		if total(calls) != 0 || res.Totals.Reused != 5 {
			t.Fatalf("prepare = %+v, detection %v", res.Totals, calls)
		}

		metadataWeek = p.publish(t, dir, monday.Add(2*week))
		if !slices.Equal(metadataWeek.MetadataChanged, []string{"store"}) || len(metadataWeek.Changed)+len(metadataWeek.Added) != 0 ||
			metadataWeek.UploadedSchemas != 0 || len(p.repo.target.manifests) != 2 {
			t.Fatalf("result = %+v, pushed %v", metadataWeek, p.repo.target.manifests)
		}

		rec := p.record(t, "store")
		if rec.Entry.Name != "Store v2" || rec.Entry.Artifact != before.Entry.Artifact || rec.ArtifactRevision != first.Revision ||
			rec.LastChangedRevision != metadataWeek.Revision {
			t.Errorf("store record = %+v", rec)
		}
	}))

	var heldWeek *Result

	mustPass(t, t.Run("a transient detection failure holds the changed schema and the run succeeds", func(t *testing.T) {
		oldMIT := p.record(t, "mit")

		p.write("schemas/mit.json", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","title":"MIT v2"}`)
		p.write("schemas/store.json", `{"type":"integer","minimum":0}`)
		p.github.fail(true)

		res, dir, _ := p.prepare(t, nil)
		if want := []prepare.Hold{{ID: "mit", Reason: state.HeldLicenseDetectionFailed}}; !reflect.DeepEqual(res.Held, want) ||
			len(res.Regressions) != 0 {
			t.Fatalf("prepare held %v, regressions %v", res.Held, res.Regressions)
		}

		heldWeek = p.publish(t, dir, monday.Add(3*week))
		if heldWeek.Status != StatusPublished || !slices.Equal(heldWeek.Changed, []string{"store"}) ||
			!reflect.DeepEqual(heldWeek.Held, holds("mit", state.HeldLicenseDetectionFailed)) {
			t.Fatalf("result = %+v", heldWeek)
		}

		if got := p.repo.catalog(t, LatestTag)["mit"]; got != oldMIT.Entry.Artifact.Digest {
			t.Errorf("mit in the catalog = %s, want its last artifact %s", got, oldMIT.Entry.Artifact.Digest)
		}

		if rec := p.record(t, "mit"); rec.HeldSinceRevision != heldWeek.Revision || rec.HeldReason != state.HeldLicenseDetectionFailed ||
			rec.ContentDigest != oldMIT.ContentDigest {
			t.Errorf("mit record = %+v", rec)
		}
	}))

	mustPass(t, t.Run("the next successful run refreshes the held schema and ends the hold", func(t *testing.T) {
		p.github.fail(false)

		res, dir, calls := p.prepare(t, nil)
		if len(res.Held) != 0 || calls["mit"] == 0 || calls["tools"]+calls["moved"] != 0 || res.Totals.Reused != 4 {
			t.Fatalf("prepare = %+v held %v, detection %v", res.Totals, res.Held, calls)
		}

		res2 := p.publish(t, dir, monday.Add(4*week))
		if !slices.Equal(res2.Changed, []string{"mit"}) || len(res2.Held) != 0 || res2.UploadedSchemas != 1 {
			t.Fatalf("result = %+v", res2)
		}

		if rec := p.record(t, "mit"); rec.Held() || rec.LastChangedRevision != res2.Revision {
			t.Errorf("mit record = %+v", rec)
		}
	}))

	mustPass(t, t.Run("a license that is no longer permissive and a bundling failure hold their schemas", func(t *testing.T) {
		oldTools, oldBundled := p.record(t, "tools"), p.record(t, "bundled")

		p.github.set("tools", "GPL-3.0-only", "GNU GENERAL PUBLIC LICENSE\nVersion 3\n")
		p.write("schemas/tools.json", `{"type":"string","title":"Tools v2"}`)
		p.write("schemas/bundled.json", `{"$schema":"`+draft7+`","properties":{"x":{"$ref":"file:///etc/passwd"}}}`)
		p.source("Store v3", `"store.json", "store.yaml"`)

		res, dir, _ := p.prepare(t, nil)
		if want := []prepare.Hold{{ID: "bundled", Reason: state.HeldPrepareFailed}, {ID: "tools", Reason: state.HeldLicenseRefused}}; !reflect.DeepEqual(res.Held, want) {
			t.Fatalf("prepare held %v", res.Held)
		}

		out := p.publish(t, dir, monday.Add(5*week))
		if !slices.Equal(out.MetadataChanged, []string{"store"}) || out.UploadedSchemas != 0 ||
			!reflect.DeepEqual(out.Held, holds("bundled", state.HeldPrepareFailed, "tools", state.HeldLicenseRefused)) {
			t.Fatalf("result = %+v", out)
		}

		catalog := p.repo.catalog(t, LatestTag)
		if catalog["tools"] != oldTools.Entry.Artifact.Digest || catalog["bundled"] != oldBundled.Entry.Artifact.Digest {
			t.Errorf("held schemas in the catalog: %v", catalog)
		}
	}))

	mustPass(t, t.Run("an exclude rule drops a published schema and nothing else does", func(t *testing.T) {
		p.policy("Store notice.", `
[[rules]]
id = "takedown"
decision = "exclude"
urls = ["`+toolsURL+`"]
reason = "takedown request"
`)

		res, dir, calls := p.prepare(t, nil)
		if total(calls) != 0 || !reflect.DeepEqual(res.Excluded, []prepare.Exclusion{{ID: "tools", Rule: "takedown"}}) ||
			!reflect.DeepEqual(res.Held, []prepare.Hold{{ID: "bundled", Reason: state.HeldPrepareFailed}}) {
			t.Fatalf("prepare excluded %v, held %v, detection %v", res.Excluded, res.Held, calls)
		}

		out := p.publish(t, dir, monday.Add(6*week))
		if !slices.Equal(out.Excluded, []string{"tools"}) || len(out.RemovedUpstream) != 0 ||
			!reflect.DeepEqual(out.Held, holds("bundled", state.HeldPrepareFailed)) || out.Unchanged != 4 {
			t.Fatalf("result = %+v", out)
		}

		if _, listed := p.repo.catalog(t, LatestTag)["tools"]; listed {
			t.Error("the excluded tools is still in the catalog")
		}

		if rec := p.record(t, "tools"); rec.ExcludedRevision != out.Revision || rec.Held() {
			t.Errorf("tools record = %+v; its ID must stay reserved", rec)
		}
	}))

	mustPass(t, t.Run("a policy notice change reaches a reused schema only through --refresh", func(t *testing.T) {
		p.policy("Store notice v2.", `
[[rules]]
id = "takedown"
decision = "exclude"
urls = ["`+toolsURL+`"]
reason = "takedown request"
`)
		p.write("schemas/bundled.json", bundledSchema)

		res, dir, calls := p.prepare(t, nil)
		if total(calls) != 0 || len(res.Held) != 0 || res.Totals.Reused != 4 {
			t.Fatalf("prepare = %+v held %v, detection %v", res.Totals, res.Held, calls)
		}

		if quiet := p.publish(t, dir, monday.Add(7*week)); quiet.Status != StatusNoop {
			t.Fatalf("without --refresh = %+v", quiet)
		}

		before := p.record(t, "store")

		res, dir, calls = p.prepare(t, func(o *prepare.Options) { o.Refresh = []string{"store"} })
		if total(calls) != 0 || res.Totals.Reused != 3 {
			t.Fatalf("prepare = %+v, detection %v", res.Totals, calls)
		}

		out := p.publish(t, dir, monday.Add(7*week+time.Hour))
		if !slices.Equal(out.Changed, []string{"store"}) || out.UploadedSchemas != 1 {
			t.Fatalf("with --refresh store = %+v", out)
		}

		if rec := p.record(t, "store"); rec.NoticeDigest == before.NoticeDigest || rec.Entry.Artifact == before.Entry.Artifact ||
			rec.ContentDigest != before.ContentDigest {
			t.Errorf("store record = %+v", rec)
		}

		if rec := p.record(t, "bundled"); rec.Held() {
			t.Errorf("bundled is still held after its source recovered: %+v", rec)
		}
	}))

	mustPass(t, t.Run("--refresh-all prepares every published schema anew", func(t *testing.T) {
		before, moved := p.record(t, "mit"), p.record(t, "moved")

		res, dir, calls := p.prepare(t, func(o *prepare.Options) { o.RefreshAll = true })
		if res.Totals.Reused != 0 || calls["mit"] == 0 || calls["moved"] == 0 {
			t.Fatalf("prepare = %+v, detection %v", res.Totals, calls)
		}

		out := p.publish(t, dir, monday.Add(8*week))
		if !slices.Equal(out.Changed, []string{"bundled"}) || out.UploadedSchemas != 1 || out.Unchanged != 3 {
			t.Fatalf("result = %+v", out)
		}

		if rec := p.record(t, "mit"); rec.Entry.Artifact != before.Entry.Artifact || !rec.License.Equal(&before.License) {
			t.Errorf("mit record = %+v, want it unchanged", rec)
		}

		if rec := p.record(t, "moved"); rec.Entry.Artifact != moved.Entry.Artifact || !rec.License.Equal(&moved.License) {
			t.Errorf("moved record = %+v, want it unchanged", rec)
		}
	}))
}
