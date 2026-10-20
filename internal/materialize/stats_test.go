package materialize

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/store"
)

type runStats struct {
	FetchedManifests   int
	CachedManifests    int
	FetchedBlobs       int
	CachedBlobs        int
	WrittenSchemaFiles int
	CachedSchemaFiles  int
	RepairedEntries    int
	FetchedBytes       int64
}

func statsOf(res *Result) runStats {
	return runStats{
		FetchedManifests:   res.FetchedManifests,
		CachedManifests:    res.CachedManifests,
		FetchedBlobs:       res.FetchedBlobs,
		CachedBlobs:        res.CachedBlobs,
		WrittenSchemaFiles: res.WrittenSchemaFiles,
		CachedSchemaFiles:  res.CachedSchemaFiles,
		RepairedEntries:    res.RepairedEntries,
		FetchedBytes:       res.FetchedBytes,
	}
}

type fixtureGraph struct {
	manifests   map[string]struct{}
	blobs       map[string]struct{}
	schemas     map[string]*artifact.SchemaManifest
	catalogJSON string
	bytes       int64
}

func graphOf(t *testing.T, f *fixture, lc *store.LoadedCatalog) fixtureGraph {
	t.Helper()

	index, err := artifact.ParseCatalogIndex(f.rem.blobs[f.digest], artifact.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	metadata, err := artifact.ParseCatalogMetadata(f.rem.blobs[index.Metadata.Digest.String()], artifact.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	graph := fixtureGraph{
		manifests:   map[string]struct{}{f.digest: {}, index.Metadata.Digest.String(): {}},
		blobs:       map[string]struct{}{metadata.Payload.Digest.String(): {}},
		schemas:     map[string]*artifact.SchemaManifest{},
		catalogJSON: metadata.Payload.Digest.String(),
	}
	for _, entry := range lc.Catalog.Schemas {
		if _, ok := graph.schemas[entry.Artifact.Digest]; ok {
			continue
		}

		sm, err := artifact.ParseSchemaManifest(f.rem.blobs[entry.Artifact.Digest], artifact.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}

		graph.schemas[entry.Artifact.Digest] = sm
		graph.manifests[entry.Artifact.Digest] = struct{}{}
		graph.blobs[sm.Payload.Digest.String()] = struct{}{}
		if sm.Notice != nil {
			graph.blobs[sm.Notice.Digest.String()] = struct{}{}
		}
	}

	for dgst := range graph.manifests {
		graph.bytes += int64(len(f.rem.blobs[dgst]))
	}
	for dgst := range graph.blobs {
		graph.bytes += int64(len(f.rem.blobs[dgst]))
	}

	return graph
}

func requireStats(t *testing.T, res *Result, want runStats) {
	t.Helper()

	if !res.Complete || len(res.Missing) != 0 {
		t.Fatalf("successful statistics run is incomplete: %+v", res)
	}

	if got := statsOf(res); !reflect.DeepEqual(got, want) {
		t.Fatalf("statistics = %+v, want %+v", got, want)
	}

	if res.Entries != 4 || res.Schemas != 3 || res.Notices != 1 {
		t.Fatalf("shared artifact counts = entries %d, schemas %d, notices %d; want 4, 3, 1", res.Entries, res.Schemas, res.Notices)
	}
}

func TestDistinctArtifactColdWarmAndRepairStats(t *testing.T) {
	for _, concurrency := range []int{1, 4} {
		t.Run(strconv.Itoa(concurrency), func(t *testing.T) {
			f := fixtureFromItems(t, sharedItems(t))
			dir := t.TempDir()
			opts := Options{Concurrency: concurrency}
			lc, cold := loadAndRun(t, f, dir, false, opts)
			graph := graphOf(t, f, lc)
			wantCold := runStats{
				FetchedManifests:   len(graph.manifests),
				FetchedBlobs:       len(graph.blobs),
				WrittenSchemaFiles: len(graph.schemas),
				FetchedBytes:       graph.bytes,
			}
			requireStats(t, cold, wantCold)

			requests := f.rem.requests.Load()
			_, warm := loadAndRun(t, f, dir, false, opts)
			wantWarm := runStats{
				CachedManifests:   len(graph.manifests),
				CachedBlobs:       len(graph.blobs),
				CachedSchemaFiles: len(graph.schemas),
			}
			requireStats(t, warm, wantWarm)
			_, offline := loadAndRun(t, f, dir, true, opts)
			requireStats(t, offline, wantWarm)
			if f.rem.requests.Load() != requests {
				t.Fatal("warm runs contacted the registry")
			}

			c := testCache(t, dir)
			for dgst := range graph.manifests {
				corruptFile(t, blobPath(dir, dgst))
			}
			for dgst := range graph.blobs {
				corruptFile(t, blobPath(dir, dgst))
			}
			for dgst := range graph.schemas {
				corruptFile(t, c.SchemaPath(dgst))
			}

			_, repaired := loadAndRun(t, f, dir, false, opts)
			wantRepair := wantCold
			wantRepair.RepairedEntries = len(graph.manifests) + len(graph.blobs) + len(graph.schemas)
			requireStats(t, repaired, wantRepair)
		})
	}
}

func TestPartialRepairStatsKeepVerifiedCatalogArtifactsCached(t *testing.T) {
	for _, concurrency := range []int{1, 4} {
		t.Run(strconv.Itoa(concurrency), func(t *testing.T) {
			f := fixtureFromItems(t, sharedItems(t))
			dir := t.TempDir()
			opts := Options{Concurrency: concurrency}
			lc, _ := loadAndRun(t, f, dir, false, opts)
			graph := graphOf(t, f, lc)
			entry := lc.Catalog.Schemas[0]
			sm := graph.schemas[entry.Artifact.Digest]
			c := testCache(t, dir)
			corruptFile(t, blobPath(dir, graph.catalogJSON))
			corruptFile(t, blobPath(dir, sm.Payload.Digest.String()))
			corruptFile(t, blobPath(dir, sm.Notice.Digest.String()))
			corruptFile(t, c.SchemaPath(entry.Artifact.Digest))

			_, res := loadAndRun(t, f, dir, false, opts)
			requireStats(t, res, runStats{
				CachedManifests:    len(graph.manifests),
				FetchedBlobs:       3,
				CachedBlobs:        len(graph.blobs) - 3,
				WrittenSchemaFiles: 1,
				CachedSchemaFiles:  len(graph.schemas) - 1,
				RepairedEntries:    4,
				FetchedBytes: int64(len(f.rem.blobs[graph.catalogJSON]) +
					len(f.rem.blobs[sm.Payload.Digest.String()]) + len(f.rem.blobs[sm.Notice.Digest.String()])),
			})
		})
	}
}
