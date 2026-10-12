// Package materialize fills a cache directory with the complete pinned
// catalog: every schema manifest, its payload blob, its unpacked schema.json
// and every notice layer, so that every catalog command afterwards runs with
// --offline — from the same directory, from a byte copy of it on another
// machine, and from a read-only copy.
//
// Run verifies as much as Store does and downloads only what is missing. A
// repeated run downloads nothing: it re-verifies every entry and reports the
// counts. With --offline it never opens the registry: it verifies
// completeness and reports what is absent or corrupt instead of failing on
// the first problem.
package materialize

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"sync/atomic"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/store"
)

// DefaultConcurrency bounds how many schemas are processed at once.
const DefaultConcurrency = 4

// Options configures a materialize run.
type Options struct {
	Log            func(format string, args ...any)
	ArtifactLimits artifact.Limits
	CatalogLimits  catalog.Limits
	Concurrency    int
}

// MissingEntry names one absent or corrupt cache entry of the completeness
// definition. IDs lists the catalog entries that reference it (a manifest or
// notice can be shared by many).
type MissingEntry struct {
	Kind   string   `json:"kind"`   // manifest | payload | schemaFile | notice
	Digest string   `json:"digest"` // the manifest digest for schemaFile
	State  string   `json:"state"`  // absent | corrupt
	IDs    []string `json:"ids"`
}

// Result summarizes a run; it is printed by "schepherd materialize --json".
type Result struct {
	Repository         string         `json:"repository"`
	CatalogDigest      string         `json:"catalogDigest"`
	Revision           string         `json:"revision"`
	CacheDir           string         `json:"cacheDir"`
	Offline            bool           `json:"offline"`
	Complete           bool           `json:"complete"`
	Entries            int            `json:"entries"`
	Schemas            int            `json:"schemas"`
	Notices            int            `json:"notices"`
	FetchedManifests   int            `json:"fetchedManifests"`
	CachedManifests    int            `json:"cachedManifests"`
	FetchedBlobs       int            `json:"fetchedBlobs"`
	CachedBlobs        int            `json:"cachedBlobs"`
	WrittenSchemaFiles int            `json:"writtenSchemaFiles"`
	CachedSchemaFiles  int            `json:"cachedSchemaFiles"`
	RepairedEntries    int            `json:"repairedEntries"`
	FetchedBytes       int64          `json:"fetchedBytes"`
	Missing            []MissingEntry `json:"missing"`
}

// schemaGroup is one distinct schema manifest and every catalog ID that
// shares it. entry holds its own copy of the catalog entry.
type schemaGroup struct {
	entry catalog.Entry
	ids   []string
}

// counters are the mutable side of a run: workers increment them in
// parallel, Run folds them into the Result before returning.
type counters struct {
	fetchedManifests   atomic.Int64
	cachedManifests    atomic.Int64
	fetchedBlobs       atomic.Int64
	cachedBlobs        atomic.Int64
	writtenSchemaFiles atomic.Int64
	cachedSchemaFiles  atomic.Int64
}

// Run makes the cache behind st complete for the loaded catalog lc. In
// offline mode it only verifies and collects missing entries; the returned
// Result carries Complete and Missing either way.
func Run(ctx context.Context, st *store.Store, lc *store.LoadedCatalog, opts Options) (*Result, error) {
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}

	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}

	// The progress lines share the caller's Log with the store's own fetch
	// lines: serialize both writers through one mutex so lines never
	// interleave when workers run in parallel.
	logMu := &sync.Mutex{}
	innerLog := opts.Log
	opts.Log = func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()

		innerLog(format, args...)
	}

	groups := groupSchemas(lc.Catalog)

	res := &Result{
		Entries:       len(lc.Catalog.Schemas),
		Schemas:       len(groups),
		CatalogDigest: lc.Digest,
		Revision:      lc.Catalog.Revision,
	}

	cnt := &counters{}

	// The catalog itself (index, metadata manifest, catalog.json) counts as
	// manifests and blobs: Catalog() has already verified or fetched them.
	countCatalog(cnt, st.Offline(), st.CatalogFetched())

	// Notice digests are shared across many schema manifests; the first
	// group that claims a digest does the work, the rest only count it.
	notices := &noticeTracker{}

	var missing missingList

	progress := newProgress(len(groups), opts.Log)

	group, gctx := errgroup.WithContext(ctx)
	group.SetLimit(opts.Concurrency)

	for i := range groups {
		g := groups[i]

		group.Go(func() error {
			if err := completeSchema(gctx, st, g, cnt, notices); err != nil {
				if st.Offline() {
					// Offline collects problems; the run itself keeps going.
					missing.add(err)
					return nil
				}

				return err
			}

			progress.done()

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, fmt.Errorf("materialize schemas: %w", err)
	}

	res.FetchedManifests = int(cnt.fetchedManifests.Load())
	res.CachedManifests = int(cnt.cachedManifests.Load())
	res.FetchedBlobs = int(cnt.fetchedBlobs.Load())
	res.CachedBlobs = int(cnt.cachedBlobs.Load())
	res.WrittenSchemaFiles = int(cnt.writtenSchemaFiles.Load())
	res.CachedSchemaFiles = int(cnt.cachedSchemaFiles.Load())
	res.FetchedBytes = 0
	res.Notices = notices.count()
	res.Offline = st.Offline()
	res.Missing = missing.entries()
	res.Complete = len(res.Missing) == 0

	return res, nil
}

// groupSchemas collapses catalog entries into distinct schema manifests.
// Entries arrive in catalog order; the first ID of a group is its
// representative and names it in messages.
func groupSchemas(c *catalog.Catalog) []schemaGroup {
	var groups []schemaGroup

	byDigest := map[string]int{}

	for _, e := range c.Schemas {
		i, ok := byDigest[e.Artifact.Digest]
		if !ok {
			byDigest[e.Artifact.Digest] = len(groups)
			groups = append(groups, schemaGroup{entry: e, ids: []string{e.ID}})

			continue
		}

		groups[i].ids = append(groups[i].ids, e.ID)
	}

	return groups
}

// countCatalog adds the catalog's own artifacts to the run counters: the
// index and the metadata manifest (two manifests) and the catalog.json blob.
func countCatalog(cnt *counters, offline, fetched bool) {
	if fetched && !offline {
		cnt.fetchedManifests.Add(2)
		cnt.cachedBlobs.Add(1)
	} else {
		cnt.cachedManifests.Add(2)
		cnt.cachedBlobs.Add(1)
	}
}

// completeSchema verifies or fetches every artifact one schema needs:
// manifest, payload (even when schema.json is already whole — the
// completeness definition differs from Store.Materialize here), schema.json
// and the notice layer.
func completeSchema(ctx context.Context, st *store.Store, g schemaGroup, cnt *counters, notices *noticeTracker) error {
	entry := &g.entry

	desc := ocispec.Descriptor{MediaType: entry.Artifact.MediaType, Digest: digestOf(entry.Artifact.Digest), Size: entry.Artifact.Size}

	what := fmt.Sprintf("schema %q (manifest %s)", g.ids[0], entry.Artifact.Digest)

	sm, fetched, err := st.EnsureManifest(ctx, entry, desc)
	if err != nil {
		err = fmt.Errorf("schema %q: %w", g.ids[0], err)
		if offlineProblem(err) {
			return &missingEntryError{kind: "manifest", digest: entry.Artifact.Digest, ids: g.ids, err: err}
		}

		return err
	}

	countManifest(cnt, fetched)

	if err := st.EnsurePayload(ctx, what, sm); err != nil {
		err = fmt.Errorf("schema %q: %w", g.ids[0], err)
		if offlineProblem(err) {
			return &missingEntryError{kind: "payload", digest: sm.Payload.Digest.String(), ids: g.ids, err: err}
		}

		return err
	}

	written, err := st.EnsureSchemaFile(&g.entry, sm)
	if err != nil {
		err = fmt.Errorf("schema %q: %w", g.ids[0], err)
		if offlineProblem(err) {
			return &missingEntryError{kind: "schemaFile", digest: entry.Artifact.Digest, ids: g.ids, err: err}
		}

		return err
	}

	if written {
		cnt.writtenSchemaFiles.Add(1)
	} else {
		cnt.cachedSchemaFiles.Add(1)
	}

	if sm.Notice != nil {
		if err := notices.ensure(ctx, st, what, *sm.Notice, cnt); err != nil {
			err = fmt.Errorf("schema %q: %w", g.ids[0], err)
			if offlineProblem(err) {
				return &missingEntryError{kind: "notice", digest: sm.Notice.Digest.String(), ids: g.ids, err: err}
			}

			return err
		}
	}

	return nil
}

func countManifest(cnt *counters, fetched bool) {
	if fetched {
		cnt.fetchedManifests.Add(1)
	} else {
		cnt.cachedManifests.Add(1)
	}
}

// offlineProblem reports whether err is the offline refusal (fault.Offline):
// in the verification mode every such problem is collected, not fatal.
func offlineProblem(err error) bool {
	var f *fault.Error

	return errors.As(err, &f) && f.Kind == fault.Offline
}

// missingEntryError is an offline completeness problem converted into a
// MissingEntry by missingList.
type missingEntryError struct {
	kind   string
	digest string
	ids    []string
	err    error
}

func (e *missingEntryError) Error() string { return e.err.Error() }

func (e *missingEntryError) Unwrap() error { return e.err }

type missingList struct {
	mu   sync.Mutex
	list []MissingEntry
}

func (l *missingList) add(err error) {
	var me *missingEntryError
	if !errors.As(err, &me) {
		return
	}

	state := "absent"
	if errors.Is(me.err, cache.ErrCorrupt) {
		state = "corrupt"
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.list = append(l.list, MissingEntry{Kind: me.kind, Digest: me.digest, State: state, IDs: me.ids})
}

func (l *missingList) entries() []MissingEntry {
	if len(l.list) == 0 {
		return []MissingEntry{}
	}

	out := slices.Clone(l.list)

	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}

		return out[i].Digest < out[j].Digest
	})

	return out
}

// noticeTracker deduplicates notice digests within the run: the first group
// that claims a digest verifies or fetches the blob; the others only count
// it as cached.
type noticeTracker struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func (t *noticeTracker) ensure(ctx context.Context, st *store.Store, what string, desc ocispec.Descriptor, cnt *counters) error {
	t.mu.Lock()
	if t.seen == nil {
		t.seen = map[string]struct{}{}
	}

	_, claimed := t.seen[desc.Digest.String()]
	if !claimed {
		t.seen[desc.Digest.String()] = struct{}{}
	}
	t.mu.Unlock()

	if claimed {
		cnt.cachedBlobs.Add(1)

		return nil
	}

	fetched, err := st.EnsureNotice(ctx, what, desc)
	if err != nil {
		err = fmt.Errorf("%s: %w", what, err)
		// Release the claim so a later group can retry (harmless online;
		// offline every group reports the same missing digest).
		t.mu.Lock()
		delete(t.seen, desc.Digest.String())
		t.mu.Unlock()

		return err
	}

	if fetched {
		cnt.fetchedBlobs.Add(1)
	} else {
		cnt.cachedBlobs.Add(1)
	}

	return nil
}

func (t *noticeTracker) count() int { return len(t.seen) }

// progress prints a line at every decile of processed schemas, in the
// spirit of mirror's one-line summaries instead of per-fetch noise.
type progress struct {
	total    int
	finished atomic.Int64
	next     atomic.Int64
	log      func(format string, args ...any)
}

func newProgress(total int, log func(string, ...any)) *progress {
	return &progress{total: total, log: log}
}

func (p *progress) done() {
	n := p.finished.Add(1)

	next := p.next.Load()
	mark := int64(float64(n) / float64(p.total) * 10)

	if mark > next && int(n) < p.total {
		if p.next.CompareAndSwap(next, mark) {
			p.log("materialize: %d/%d schemas done", n, p.total)
		}
	}
}

func digestOf(s string) godigest.Digest {
	return godigest.Digest(s)
}
