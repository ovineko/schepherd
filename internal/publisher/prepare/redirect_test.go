package prepare

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

// fakeWeb serves URLs of public hosts from memory as the transport of the
// fetcher prepare builds: documents and redirects by URL. It counts the
// requests per URL.
type fakeWeb struct {
	docs      map[string]string
	redirects map[string]string
	hits      map[string]int
	mu        sync.Mutex
}

func newFakeWeb() *fakeWeb {
	return &fakeWeb{docs: map[string]string{}, redirects: map[string]string{}, hits: map[string]int{}}
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
		rec.Header().Set("Content-Type", "application/json")
		_, _ = rec.WriteString(body)
	default:
		http.NotFound(rec, r)
	}

	resp := rec.Result()
	resp.Request = r

	return resp, nil
}

func (w *fakeWeb) serve(url, body string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	delete(w.redirects, url)
	w.docs[url] = body
}

func (w *fakeWeb) redirect(from, to string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.redirects[from] = to
}

func (w *fakeWeb) hitCount(url string) int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.hits[url]
}

// GitHub serves raw files of github.com/<owner>/<repo>/raw/<ref>/<path> by
// redirecting to raw.githubusercontent.com with a qualified ref: every such
// document of a schema is served through a redirect. Automatic license
// detection decides all of them.
const (
	movedURL       = "https://github.com/owner/mit/raw/main/moved.json"
	movedTarget    = "https://raw.githubusercontent.com/owner/mit/refs/heads/main/moved.json"
	movedDepURL    = "https://github.com/owner/mit/raw/main/moved-dep.json"
	movedDepTarget = "https://raw.githubusercontent.com/owner/mit/refs/heads/main/moved-dep.json"
	movedSchema    = `{"$schema":"` + draft7 + `","properties":{"x":{"$ref":"` + movedDepURL + `#/definitions/x"}}}`
	movedDep       = `{"$schema":"` + draft7 + `","definitions":{"x":{"type":"string"}}}`
	movedEntry     = `
[[entries]]
id = "moved"
name = "Moved schema"
url = "` + movedURL + `"
`
	movedSource = `kind = "local"
name = "redirects"
policy = "licenses.toml"
` + movedEntry
	movedPolicy = `[auto]
enabled = true
hosts = ["api.github.com"]
`
)

// redirectEnv publishes one schema served through redirects: its root and
// its dependency.
type redirectEnv struct {
	web      *fakeWeb
	services *licenseServices
	state    *state.State
	dir      string
}

func newRedirectEnv(t *testing.T) *redirectEnv {
	t.Helper()

	e := &redirectEnv{web: newFakeWeb(), services: newLicenseServices(t), dir: t.TempDir()}
	writeFiles(t, e.dir, map[string]string{"source.toml": movedSource, "licenses.toml": movedPolicy})

	e.web.redirect(movedURL, movedTarget)
	e.web.serve(movedTarget, movedSchema)
	e.web.redirect(movedDepURL, movedDepTarget)
	e.web.serve(movedDepTarget, movedDep)

	res, out := e.prepare(t, nil)
	if res.Totals.Entries != 1 || res.Totals.Bundled != 1 || e.services.total() == 0 {
		t.Fatalf("first preparation = %+v after %d detection requests", res.Totals, e.services.total())
	}

	e.state = stateFromSet(t, mustLoad(t, out))

	return e
}

func (e *redirectEnv) prepare(t *testing.T, previous *state.State) (*Result, string) {
	t.Helper()

	out := filepath.Join(t.TempDir(), "out")

	res, err := Run(t.Context(), Options{
		Tool: pinnedTool(t), SourceFile: filepath.Join(e.dir, "source.toml"), OutDir: out, State: previous,
		LicenseDetector: e.services.detector(t), Transport: e.web, Now: func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatal(err)
	}

	return res, out
}

// The state records the redirects that served a schema's documents, so that
// the license decision, which covers the redirect targets, can be decided
// again from the state alone.
func TestRedirectsAreRecordedInTheLicenseDecision(t *testing.T) {
	e := newRedirectEnv(t)

	rec, ok := e.state.Lookup("moved")
	if !ok {
		t.Fatal("the schema was not published")
	}

	want := []state.Redirect{
		{URL: movedDepURL, Target: movedDepTarget, Digest: digest.FromBytes([]byte(movedDep))},
		{URL: movedURL, Target: movedTarget, Digest: digest.FromBytes([]byte(movedSchema))},
	}
	if !reflect.DeepEqual(rec.License.Redirects, want) {
		t.Errorf("redirects = %+v, want %+v", rec.License.Redirects, want)
	}

	urls := make([]string, 0, len(rec.License.Detections))
	for _, det := range rec.License.Detections {
		urls = append(urls, det.URL)
	}

	if !slices.Equal(urls, []string{movedDepURL, movedURL, movedDepTarget, movedTarget}) {
		t.Errorf("detections cover %v", urls)
	}

	schema := compilePreparedSchema(t)

	for name, previous := range map[string]*state.State{"prepared": nil, "reused": e.state} {
		_, out := e.prepare(t, previous)

		data, err := os.ReadFile(filepath.Join(out, PreparedFile))
		if err != nil {
			t.Fatal(err)
		}

		if !strings.Contains(string(data), `"redirects"`) {
			t.Errorf("%s: prepared.json records no redirects:\n%s", name, data)
		}

		if err := validateAgainstSchema(schema, data); err != nil {
			t.Errorf("%s: api/prepared.schema.json rejects prepared.json: %v", name, err)
		}
	}
}

// An unchanged schema served through redirects is reused without a license
// detection request: the recorded findings allow its recorded targets,
// which are fetched to compare their digests.
func TestUnchangedRedirectServedSchemaIsReused(t *testing.T) {
	e := newRedirectEnv(t)
	before, targets := e.services.total(), e.web.hitCount(movedTarget)+e.web.hitCount(movedDepTarget)

	res, out := e.prepare(t, e.state)
	if e.services.total() != before {
		t.Errorf("an unchanged schema made %d license detection requests", e.services.total()-before)
	}

	if res.Totals.Reused != 1 || res.Totals.Bundled != 0 || len(res.Held)+len(res.Excluded)+len(res.Regressions) != 0 {
		t.Fatalf("totals %+v, held %v, excluded %v", res.Totals, res.Held, res.Excluded)
	}

	if got := e.web.hitCount(movedTarget) + e.web.hitCount(movedDepTarget); got != targets+2 {
		t.Errorf("the recorded targets were requested %d times, want 2", got-targets)
	}

	entry := entryByID(t, mustLoad(t, out), "moved")
	if rec, _ := e.state.Lookup("moved"); entry.Reused == nil || !entry.License.Equal(&rec.License) {
		t.Errorf("entry = %+v", entry)
	}
}

// A redirect that no longer leads where the state says prepares the schema
// anew: a new target is decided with fresh findings, even one the fetcher
// first refused because nothing had detected its repository yet.
func TestChangedRedirectsPrepareAnew(t *testing.T) {
	const newTarget = "https://raw.githubusercontent.com/owner/mit/refs/heads/main/moved-v2.json"

	for name, tc := range map[string]struct {
		change    func(w *fakeWeb)
		redirects []state.Redirect
	}{
		"another target": {
			change: func(w *fakeWeb) {
				w.redirect(movedURL, newTarget)
				w.serve(newTarget, movedSchema)
			},
			redirects: []state.Redirect{
				{URL: movedDepURL, Target: movedDepTarget, Digest: digest.FromBytes([]byte(movedDep))},
				{URL: movedURL, Target: newTarget, Digest: digest.FromBytes([]byte(movedSchema))},
			},
		},
		"no redirect": {
			change: func(w *fakeWeb) { w.serve(movedURL, movedSchema) },
			redirects: []state.Redirect{
				{URL: movedDepURL, Target: movedDepTarget, Digest: digest.FromBytes([]byte(movedDep))},
			},
		},
		"a dependency served elsewhere": {
			change: func(w *fakeWeb) {
				w.redirect(movedDepURL, newTarget)
				w.serve(newTarget, movedDep)
			},
			redirects: []state.Redirect{
				{URL: movedDepURL, Target: newTarget, Digest: digest.FromBytes([]byte(movedDep))},
				{URL: movedURL, Target: movedTarget, Digest: digest.FromBytes([]byte(movedSchema))},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newRedirectEnv(t)
			tc.change(e.web)

			before := e.services.total()

			res, out := e.prepare(t, e.state)
			if res.Totals.Reused != 0 || res.Totals.Entries != 1 || len(res.Held)+len(res.Excluded) != 0 {
				t.Fatalf("totals %+v, held %v, excluded %v", res.Totals, res.Held, res.Excluded)
			}

			if e.services.total() == before {
				t.Error("the schema was decided anew without asking detection")
			}

			entry := entryByID(t, mustLoad(t, out), "moved")
			if !reflect.DeepEqual(entry.License.Redirects, tc.redirects) || entry.Reused != nil {
				t.Errorf("redirects = %+v, want %+v", entry.License.Redirects, tc.redirects)
			}
		})
	}
}

// The recorded redirect targets are part of the recorded decision: an
// exclude rule that matches one removes the published schema before
// anything is contacted, also when upstream no longer lists it.
func TestExcludeRuleOnARecordedRedirectTarget(t *testing.T) {
	takedown := movedPolicy + "\n[[rules]]\nid = \"takedown\"\ndecision = \"exclude\"\nurls = [\"" + movedDepTarget + "\"]\nreason = \"takedown\"\n"

	other := "\n[[entries]]\nid = \"other\"\nname = \"Other\"\nurl = \"https://schemas.example/other.json\"\nfile = \"other.json\"\nlicense = \"MIT\"\n"

	for name, source := range map[string]string{"listed": movedSource, "removed upstream": strings.Replace(movedSource, movedEntry, other, 1)} {
		t.Run(name, func(t *testing.T) {
			e := newRedirectEnv(t)
			writeFiles(t, e.dir, map[string]string{"source.toml": source, "licenses.toml": takedown, "other.json": `{"type":"object"}`})

			before, hits := e.services.total(), e.web.hitCount(movedDepTarget)

			res, _ := e.prepare(t, e.state)
			if !reflect.DeepEqual(res.Excluded, []Exclusion{{ID: "moved", Rule: "takedown"}}) || len(res.Held) != 0 {
				t.Errorf("excluded %v, held %v", res.Excluded, res.Held)
			}

			if e.services.total() != before || e.web.hitCount(movedDepTarget) != hits {
				t.Errorf("%d detection requests, %d requests for the excluded target", e.services.total()-before, e.web.hitCount(movedDepTarget)-hits)
			}
		})
	}
}

// A recorded redirect that no document of the schema followed any more
// means the record no longer describes how the schema is served.
func TestRecordedRedirectOfNoDocumentPreparesAnew(t *testing.T) {
	e := newRedirectEnv(t)

	stale := *e.state
	stale.Schemas = slices.Clone(e.state.Schemas)
	rec := &stale.Schemas[0]
	rec.License.Redirects = append([]state.Redirect{
		{URL: "https://github.com/owner/mit/raw/main/gone.json", Target: movedTarget, Digest: digest.FromBytes([]byte(movedSchema))},
	}, rec.License.Redirects...)

	if err := stale.Validate(); err != nil {
		t.Fatal(err)
	}

	res, _ := e.prepare(t, &stale)
	if res.Totals.Reused != 0 || res.Totals.Entries != 1 {
		t.Errorf("totals %+v", res.Totals)
	}
}
