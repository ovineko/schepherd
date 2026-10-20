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

	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/store"
)

// DefaultConcurrency bounds how many schemas are processed at once.
const DefaultConcurrency = 4

// Options configures a materialize run.
type Options struct {
	Log         func(format string, args ...any)
	Concurrency int
}

// MissingEntry names one absent or corrupt cache entry of the completeness
// definition. IDs lists the catalog entries that reference it (a manifest or
// notice can be shared by many).
type MissingEntry struct {
	Kind   string   `json:"kind"`   // catalogIndex | catalogMetadata | catalogPayload | manifest | payload | schemaFile | notice
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

type schemaGroup struct {
	entry catalog.Entry
	ids   []string
}

type counters struct {
	fetchedManifests   atomic.Int64
	cachedManifests    atomic.Int64
	fetchedBlobs       atomic.Int64
	cachedBlobs        atomic.Int64
	writtenSchemaFiles atomic.Int64
	cachedSchemaFiles  atomic.Int64
	repairedEntries    atomic.Int64
	fetchedBytes       atomic.Int64
}

// Run makes the cache behind st complete for the loaded catalog lc. In
// offline mode it only verifies and collects missing entries; the returned
// Result carries Complete and Missing either way.
func Run(ctx context.Context, st *store.Store, lc *store.LoadedCatalog, opts Options) (*Result, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}

	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}

	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}

	logMu := &sync.Mutex{}
	innerLog := opts.Log
	opts.Log = func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()

		innerLog(format, args...)
	}

	groups := groupSchemas(lc)

	res := &Result{
		Schemas:       len(groups),
		CatalogDigest: lc.Digest,
	}
	if lc.Catalog != nil {
		res.Entries = len(lc.Catalog.Schemas)
		res.Revision = lc.Catalog.Revision
	}

	cnt := &counters{}
	countCatalog(cnt, lc.Stats)

	blobs := &blobTracker{}
	var missing missingList
	for _, problem := range lc.CatalogProblems {
		if !st.Offline() || !offlineProblem(problem.Err) {
			return nil, fmt.Errorf("materialize %s %s: %w", problem.Kind, problem.Digest, problem.Err)
		}
		missing.add(problem.Kind, problem.Digest, nil, problem.Err)
	}

	progress := newProgress(len(groups), opts.Log)

	group, gctx := errgroup.WithContext(ctx)
	group.SetLimit(opts.Concurrency)

	for i := range groups {
		if gctx.Err() != nil {
			break
		}

		g := groups[i]

		group.Go(func() error {
			if err := completeSchema(gctx, st, g, cnt, blobs, &missing); err != nil {
				return err
			}

			progress.done()

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, fmt.Errorf("materialize schemas: %w", err)
	}

	if err := contextError(ctx); err != nil {
		return nil, err
	}

	res.FetchedManifests = int(cnt.fetchedManifests.Load())
	res.CachedManifests = int(cnt.cachedManifests.Load())
	res.FetchedBlobs = int(cnt.fetchedBlobs.Load())
	res.CachedBlobs = int(cnt.cachedBlobs.Load())
	res.WrittenSchemaFiles = int(cnt.writtenSchemaFiles.Load())
	res.CachedSchemaFiles = int(cnt.cachedSchemaFiles.Load())
	res.RepairedEntries = int(cnt.repairedEntries.Load())
	res.FetchedBytes = cnt.fetchedBytes.Load()
	res.Notices = blobs.noticeCount()
	res.Offline = st.Offline()
	res.Missing = missing.entries()
	res.Complete = lc.Catalog != nil && len(res.Missing) == 0

	return res, nil
}

func groupSchemas(lc *store.LoadedCatalog) []schemaGroup {
	var groups []schemaGroup

	byDigest := map[string]int{}
	if lc.Catalog == nil {
		for _, desc := range lc.SchemaDescriptors {
			dgst := desc.Digest.String()
			if _, ok := byDigest[dgst]; ok {
				continue
			}
			byDigest[dgst] = len(groups)
			groups = append(groups, schemaGroup{entry: catalog.Entry{
				Artifact: catalog.Descriptor{MediaType: desc.MediaType, Digest: dgst, Size: desc.Size},
			}})
		}
		return groups
	}

	for _, e := range lc.Catalog.Schemas {
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

func countCatalog(cnt *counters, stats store.CatalogStats) {
	cnt.fetchedManifests.Add(int64(stats.FetchedManifests))
	cnt.cachedManifests.Add(int64(stats.CachedManifests))
	cnt.fetchedBlobs.Add(int64(stats.FetchedBlobs))
	cnt.cachedBlobs.Add(int64(stats.CachedBlobs))
	cnt.repairedEntries.Add(int64(stats.RepairedEntries))
	cnt.fetchedBytes.Add(stats.FetchedBytes)
}

func completeSchema(ctx context.Context, st *store.Store, g schemaGroup, cnt *counters, blobs *blobTracker, missing *missingList) error {
	if err := contextError(ctx); err != nil {
		return err
	}

	entry := &g.entry
	desc := ocispec.Descriptor{MediaType: entry.Artifact.MediaType, Digest: godigest.Digest(entry.Artifact.Digest), Size: entry.Artifact.Size}
	what := "schema manifest " + entry.Artifact.Digest
	if len(g.ids) > 0 {
		what = fmt.Sprintf("schema %q (manifest %s)", g.ids[0], entry.Artifact.Digest)
	}

	handle := func(kind, dgst string, err error) error {
		err = fmt.Errorf("%s: %w", what, err)
		if st.Offline() && offlineProblem(err) {
			missing.add(kind, dgst, g.ids, err)

			return nil
		}

		return err
	}

	sm, outcome, err := st.EnsureManifest(ctx, entry, desc)
	if err != nil {
		return handle("manifest", entry.Artifact.Digest, err)
	}

	countManifest(cnt, outcome)

	if err := contextError(ctx); err != nil {
		return err
	}

	err = blobs.ensure(ctx, what, sm.Payload, false, cnt, func() (store.EnsureOutcome, error) {
		return st.EnsurePayload(ctx, what, sm)
	})
	if err != nil {
		if err := handle("payload", sm.Payload.Digest.String(), err); err != nil {
			return err
		}
	}

	if err := contextError(ctx); err != nil {
		return err
	}

	outcome, err = st.EnsureSchemaFile(ctx, entry, sm)
	if err != nil {
		if err := handle("schemaFile", entry.Artifact.Digest, err); err != nil {
			return err
		}
	} else {
		if outcome.Written {
			cnt.writtenSchemaFiles.Add(1)
		} else {
			cnt.cachedSchemaFiles.Add(1)
		}

		countTransfer(cnt, outcome)
	}

	if err := contextError(ctx); err != nil {
		return err
	}

	if sm.Notice != nil {
		err := blobs.ensure(ctx, what, *sm.Notice, true, cnt, func() (store.EnsureOutcome, error) {
			return st.EnsureNotice(ctx, what, *sm.Notice)
		})
		if err != nil {
			return handle("notice", sm.Notice.Digest.String(), err)
		}
	}

	return contextError(ctx)
}

func countManifest(cnt *counters, outcome store.EnsureOutcome) {
	if outcome.Fetched {
		cnt.fetchedManifests.Add(1)
	} else if !outcome.Written {
		cnt.cachedManifests.Add(1)
	}

	countTransfer(cnt, outcome)
}

func countTransfer(cnt *counters, outcome store.EnsureOutcome) {
	if outcome.Repaired {
		cnt.repairedEntries.Add(1)
	}

	cnt.fetchedBytes.Add(outcome.FetchedBytes)
}

func offlineProblem(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var f *fault.Error

	return errors.As(err, &f) && f.Kind == fault.Offline &&
		(errors.Is(err, cache.ErrNotFound) || errors.Is(err, cache.ErrCorrupt))
}

type missingKey struct {
	kind   string
	digest string
}

type missingList struct {
	mu   sync.Mutex
	list map[missingKey]MissingEntry
}

func (l *missingList) add(kind, dgst string, ids []string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.list == nil {
		l.list = map[missingKey]MissingEntry{}
	}

	key := missingKey{kind: kind, digest: dgst}
	entry, ok := l.list[key]
	if !ok {
		entry = MissingEntry{Kind: kind, Digest: dgst, State: "absent"}
	}

	if errors.Is(err, cache.ErrCorrupt) {
		entry.State = "corrupt"
	}

	entry.IDs = append(entry.IDs, ids...)
	l.list[key] = entry
}

func (l *missingList) entries() []MissingEntry {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]MissingEntry, 0, len(l.list))
	for _, entry := range l.list {
		entry.IDs = append([]string{}, entry.IDs...)
		slices.Sort(entry.IDs)
		entry.IDs = slices.Compact(entry.IDs)
		out = append(out, entry)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}

		return out[i].Digest < out[j].Digest
	})

	return out
}

type blobResult struct {
	done   chan struct{}
	err    error
	size   int64
	notice bool
}

type blobTracker struct {
	mu   sync.Mutex
	seen map[string]*blobResult
}

func (t *blobTracker) ensure(ctx context.Context, what string, desc ocispec.Descriptor, notice bool, cnt *counters, ensure func() (store.EnsureOutcome, error)) error {
	if err := contextError(ctx); err != nil {
		return err
	}

	t.mu.Lock()
	if t.seen == nil {
		t.seen = map[string]*blobResult{}
	}

	result, claimed := t.seen[desc.Digest.String()]
	if claimed {
		if result.size != desc.Size {
			t.mu.Unlock()

			return fault.New(fault.Integrity, "%s: shared blob %s declares size %d, previously %d", what, desc.Digest, desc.Size, result.size)
		}

		result.notice = result.notice || notice
		t.mu.Unlock()

		select {
		case <-ctx.Done():
			return contextError(ctx)
		case <-result.done:
			if err := contextError(ctx); err != nil {
				return err
			}

			return result.err
		}
	}

	result = &blobResult{done: make(chan struct{}), size: desc.Size, notice: notice}
	t.seen[desc.Digest.String()] = result
	t.mu.Unlock()

	outcome, err := ensure()
	if err == nil {
		if outcome.Fetched {
			cnt.fetchedBlobs.Add(1)
		} else if !outcome.Written {
			cnt.cachedBlobs.Add(1)
		}

		countTransfer(cnt, outcome)
	}

	t.mu.Lock()
	result.err = err
	close(result.done)
	t.mu.Unlock()

	return err
}

func (t *blobTracker) noticeCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	count := 0
	for _, result := range t.seen {
		if result.notice && result.err == nil {
			count++
		}
	}

	return count
}

func contextError(ctx context.Context) error {
	err := ctx.Err()
	if err == nil {
		return nil
	}

	kind := fault.Registry
	if errors.Is(err, context.Canceled) {
		kind = fault.Canceled
	}

	return fault.Wrap(kind, err, "materialize canceled")
}

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
