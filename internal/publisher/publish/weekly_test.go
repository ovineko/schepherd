package publish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/state"
	"github.com/ovineko/schepherd/internal/registry"
	"github.com/ovineko/schepherd/internal/testutil/ociregistry"
)

// countingTarget records the writes that reach a real registry client.
type countingTarget struct {
	Target

	manifests []string
	tags      []string
	blobs     int
	mu        sync.Mutex
}

func (c *countingTarget) PushBlob(ctx context.Context, desc ocispec.Descriptor, data []byte) (bool, error) {
	c.mu.Lock()
	c.blobs++
	c.mu.Unlock()

	return c.Target.PushBlob(ctx, desc, data)
}

func (c *countingTarget) PushManifest(ctx context.Context, desc ocispec.Descriptor, data []byte) (bool, error) {
	pushed, err := c.Target.PushManifest(ctx, desc, data)
	if pushed {
		c.mu.Lock()
		c.manifests = append(c.manifests, desc.Digest.String())
		c.mu.Unlock()
	}

	return pushed, err
}

func (c *countingTarget) EnsureTag(ctx context.Context, desc ocispec.Descriptor, tag string) (bool, error) {
	created, err := c.Target.EnsureTag(ctx, desc, tag)
	if created {
		c.record(tag)
	}

	return created, err
}

func (c *countingTarget) Tag(ctx context.Context, desc ocispec.Descriptor, tag string) error {
	err := c.Target.Tag(ctx, desc, tag)
	if err == nil {
		c.record(tag)
	}

	return err
}

func (c *countingTarget) record(tag string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.tags = append(c.tags, tag)
}

func (c *countingTarget) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.manifests, c.tags, c.blobs = nil, nil, 0
}

// weeklyRepo is one repository of the in-process registry, opened with the
// publisher's registry client.
type weeklyRepo struct {
	reg    *ociregistry.Registry
	repo   *registry.Repo
	target *countingTarget
}

func newWeeklyRepo(t *testing.T) *weeklyRepo {
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

	return &weeklyRepo{reg: reg, repo: repo, target: &countingTarget{Target: repo}}
}

// run publishes like the weekly job: the state in Git is --state, the new
// state goes to a separate file first.
func (w *weeklyRepo) run(t *testing.T, dir, stateFile string, now time.Time, update bool) (*Result, string, error) {
	t.Helper()

	previous, err := state.Load(stateFile)
	if err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "state.json")

	res, err := Run(t.Context(), w.target, Options{
		Now: now, State: previous, StateOut: out, PreparedDir: dir, Repository: w.repo.Name().String(), UpdateLatest: update,
	})

	return res, out, err
}

func (w *weeklyRepo) tags(t *testing.T) []string {
	t.Helper()

	tags, err := w.repo.Tags(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	slices.Sort(tags)

	return tags
}

func (w *weeklyRepo) resolve(t *testing.T, tag string) string {
	t.Helper()

	desc, err := w.repo.Resolve(t.Context(), tag)
	if err != nil {
		t.Fatalf("resolve %s: %v", tag, err)
	}

	return desc.Digest.String()
}

func (w *weeklyRepo) catalog(t *testing.T, tag string) map[string]string {
	t.Helper()

	c, err := fetchCatalog(t.Context(), w.repo, w.resolve(t, tag))
	if err != nil {
		t.Fatal(err)
	}

	artifacts := map[string]string{"": c.Revision}
	for _, e := range c.Schemas {
		artifacts[e.ID] = e.Artifact.Digest
	}

	return artifacts
}

func commit(t *testing.T, from, to string) {
	t.Helper()

	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(to, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readText(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

// changedLines returns the numbers of the lines that differ between two
// texts with the same number of lines.
func changedLines(t *testing.T, before, after string) []int {
	t.Helper()

	a, b := strings.Split(before, "\n"), strings.Split(after, "\n")
	if len(a) != len(b) {
		t.Fatalf("the state grew from %d to %d lines:\n%s", len(a), len(b), after)
	}

	var lines []int

	for i := range a {
		if a[i] != b[i] {
			lines = append(lines, i)
		}
	}

	return lines
}

// block returns the half-open line range of the JSON object that starts at
// the first line containing start.
func block(t *testing.T, text, start string) (int, int) {
	t.Helper()

	lines := strings.Split(text, "\n")

	for i, line := range lines {
		if !strings.Contains(line, start) {
			continue
		}

		indent := len(line) - len(strings.TrimLeft(line, " "))

		for j := i + 1; j < len(lines); j++ {
			if trimmed := strings.TrimLeft(lines[j], " "); len(lines[j])-len(trimmed) == indent && strings.HasPrefix(trimmed, "}") {
				return i, j + 1
			}
		}
	}

	t.Fatalf("no block %q", start)

	return 0, 0
}

// TestWeeklyFlow runs the weekly update against the in-process registry
// through the publisher's registry client, week after week, with the state
// file standing in for catalog/state.json in Git.
func TestWeeklyFlow(t *testing.T) {
	w := newWeeklyRepo(t)
	git := filepath.Join(t.TempDir(), "catalog", "state.json")
	monday := time.Date(2026, 9, 28, 3, 0, 7, 0, time.UTC)
	week := 7 * 24 * time.Hour

	var first *Result

	t.Run("the first run publishes everything and writes the state", func(t *testing.T) {
		res, out, err := w.run(t, writeSet(t, baseSchemas()), git, monday, true)
		if err != nil {
			t.Fatal(err)
		}

		if res.Status != StatusPublished || res.Revision != "20260928.0300" || res.UploadedSchemas != 4 || len(res.Added) != 4 {
			t.Fatalf("result = %+v", res)
		}

		if len(w.target.manifests) != 6 || w.resolve(t, LatestTag) != res.CatalogDigest ||
			w.resolve(t, "catalog-20260928.0300") != res.CatalogDigest {
			t.Errorf("manifests %v, tags %v", w.target.manifests, w.tags(t))
		}

		written := loadState(t, out)
		if written.Catalog.Revision != res.Revision || written.Catalog.Digest != res.CatalogDigest || len(written.Schemas) != 4 {
			t.Errorf("state = %+v", written)
		}

		commit(t, out, git)

		first = res
	})

	if first == nil {
		t.FailNow()
	}

	t.Run("an unchanged run is a noop", func(t *testing.T) {
		before := readText(t, git)
		requests := w.reg.Requests()
		tags := w.tags(t)

		w.target.reset()

		res, out, err := w.run(t, writeSet(t, baseSchemas()), git, monday.Add(week), false)
		if err != nil {
			t.Fatal(err)
		}

		if res.Status != StatusNoop || res.Revision != first.Revision || res.CatalogDigest != first.CatalogDigest {
			t.Errorf("result = %+v", res)
		}

		if got := w.reg.Requests(); got != requests+1 || len(w.target.manifests)+len(w.target.tags)+w.target.blobs != 0 {
			t.Errorf("the noop sent %d requests (the tag listing below is one)", got-requests-1)
		}

		if after := w.tags(t); !slices.Equal(after, tags) {
			t.Errorf("tags changed from %v to %v", tags, after)
		}

		if readText(t, out) != before {
			t.Error("the noop changed the state bytes")
		}
	})

	var second *Result

	t.Run("one changed schema uploads one artifact and the catalog", func(t *testing.T) {
		before := readText(t, git)
		oldCatalog := w.catalog(t, LatestTag)

		changed := baseSchemas()
		changed["beta"] = `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"string","maxLength":64}`

		w.target.reset()

		res, out, err := w.run(t, writeSet(t, changed), git, monday.Add(2*week), true)
		if err != nil {
			t.Fatal(err)
		}

		if res.Status != StatusPublished || res.Revision != "20261012.0300" || res.UploadedSchemas != 1 ||
			!slices.Equal(res.Changed, []string{"beta"}) || res.Unchanged != 3 {
			t.Fatalf("result = %+v", res)
		}

		newCatalog := w.catalog(t, LatestTag)
		if len(w.target.manifests) != 3 || w.target.manifests[0] != newCatalog["beta"] || w.target.manifests[2] != res.CatalogDigest {
			t.Errorf("pushed manifests %v, want the beta artifact, the catalog metadata and then the catalog index", w.target.manifests)
		}

		for _, id := range []string{"alpha", "delta", "gamma"} {
			if newCatalog[id] != oldCatalog[id] {
				t.Errorf("%s artifact changed from %s to %s", id, oldCatalog[id], newCatalog[id])
			}
		}

		after := readText(t, out)
		catalogFrom, catalogTo := block(t, before, `"catalog": {`)
		betaFrom, betaTo := block(t, before, `"id": "beta"`)

		lines := changedLines(t, before, after)
		for _, line := range lines {
			if (line < catalogFrom || line >= catalogTo) && (line < betaFrom-1 || line >= betaTo) {
				t.Errorf("state line %d changed outside the catalog block and the beta record: %q", line+1, strings.Split(after, "\n")[line])
			}
		}

		if len(lines) < 4 {
			t.Errorf("only lines %v changed", lines)
		}

		commit(t, out, git)

		second = res
	})

	if second == nil {
		t.FailNow()
	}

	gammaHeld := setOptions{held: map[string]string{"gamma": state.HeldRemovedUpstream}, names: map[string]string{"alpha": "Alpha weekly"}}

	t.Run("a schema removed upstream stays with its artifact and is held", func(t *testing.T) {
		oldCatalog := w.catalog(t, LatestTag)
		recorded := readText(t, git)

		smaller := baseSchemas()
		smaller["beta"] = `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"string","maxLength":64}`
		delete(smaller, "gamma")

		w.target.reset()

		alone, out, err := w.run(t, writeSetWith(t, smaller, setOptions{held: gammaHeld.held}), git, monday.Add(3*week), true)
		if err != nil || alone.Status != StatusNoop || !slices.Equal(alone.RemovedUpstream, []string{"gamma"}) ||
			len(w.target.manifests)+len(w.target.tags) != 0 || readText(t, out) != recorded {
			t.Fatalf("the hold alone = %+v, %v; it must neither publish nor change the state", alone, err)
		}

		res, out, err := w.run(t, writeSetWith(t, smaller, gammaHeld), git, monday.Add(3*week+time.Hour), true)
		if err != nil {
			t.Fatal(err)
		}

		if res.Status != StatusPublished || !slices.Equal(res.RemovedUpstream, []string{"gamma"}) || res.UploadedSchemas != 0 ||
			!slices.Equal(res.MetadataChanged, []string{"alpha"}) {
			t.Fatalf("result = %+v", res)
		}

		if newCatalog := w.catalog(t, LatestTag); newCatalog["gamma"] != oldCatalog["gamma"] || newCatalog[""] != res.Revision {
			t.Errorf("gamma in the new catalog = %q, want %q", newCatalog["gamma"], oldCatalog["gamma"])
		}

		rec, ok := loadState(t, out).Lookup("gamma")
		if !ok || rec.HeldSinceRevision != res.Revision || rec.HeldReason != state.HeldRemovedUpstream || rec.LastChangedRevision != first.Revision {
			t.Errorf("gamma record = %+v", rec)
		}

		commit(t, out, git)
	})

	var crashed string

	t.Run("a crash between the catalog and the state resumes the revision", func(t *testing.T) {
		recorded := readText(t, git)

		changed := baseSchemas()
		changed["beta"] = `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"string","maxLength":64}`
		changed["delta"] = `{"type":"array","uniqueItems":true}`
		delete(changed, "gamma")

		dir := writeSetWith(t, changed, gammaHeld)
		previous := loadState(t, git)

		_, err := Run(t.Context(), w.target, Options{
			Now: monday.Add(4 * week), State: previous, StateOut: t.TempDir(), PreparedDir: dir,
			Repository: w.repo.Name().String(), UpdateLatest: true,
		})
		if err == nil {
			t.Fatal("the run with an unwritable state succeeded")
		}

		crashed = w.resolve(t, "catalog-20261026.0300")
		if w.resolve(t, LatestTag) == crashed || readText(t, git) != recorded {
			t.Fatal("catalog-latest moved or the state changed before the state was written")
		}

		w.target.reset()

		res, out, err := w.run(t, dir, git, monday.Add(4*week+30*time.Hour), true)
		if err != nil {
			t.Fatal(err)
		}

		if res.Status != StatusResumed || res.Revision != "20261026.0300" || res.CatalogDigest != crashed ||
			len(w.target.manifests) != 0 || !slices.Equal(w.target.tags, []string{LatestTag}) {
			t.Fatalf("result = %+v, pushed %v, tagged %v", res, w.target.manifests, w.target.tags)
		}

		if written := loadState(t, out); written.Catalog.Revision != "20261026.0300" || written.Catalog.Digest != crashed {
			t.Errorf("state catalog = %+v", written.Catalog)
		}

		commit(t, out, git)
	})

	t.Run("a second run in the same minute with other content is refused", func(t *testing.T) {
		now := monday.Add(5 * week)

		changed := baseSchemas()
		changed["alpha"] = `{"type":"object","title":"Alpha 5"}`

		res, out, err := w.run(t, writeSet(t, changed), git, now, true)
		if err != nil || res.Status != StatusPublished || res.Revision != "20261102.0300" {
			t.Fatalf("first run of the minute = %+v, %v", res, err)
		}

		commit(t, out, git)

		recorded := readText(t, git)
		tags := w.tags(t)

		changed["alpha"] = `{"type":"object","title":"Alpha 5b"}`

		w.target.reset()

		_, out, err = w.run(t, writeSet(t, changed), git, now.Add(40*time.Second), true)
		if fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), "catalog revision 20261102.0300 already exists") {
			t.Fatalf("err = %v", err)
		}

		if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
			t.Error("the refused run wrote a state")
		}

		if after := w.tags(t); !slices.Equal(after, tags) || len(w.target.manifests)+len(w.target.tags)+w.target.blobs != 0 {
			t.Errorf("the refused run wrote: tags %v, manifests %v", after, w.target.manifests)
		}

		if readText(t, git) != recorded {
			t.Error("the state changed")
		}
	})
}
