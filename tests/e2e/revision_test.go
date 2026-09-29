//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/tests/e2e/regproxy"
)

// revHold is a held schema in the diff and publish results.
type revHold struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// revPublish is the --json result of schepherd-publisher publish
// (docs/publishing.md, "Publishing").
type revPublish struct {
	Status          string    `json:"status"`
	Revision        string    `json:"revision"`
	CatalogDigest   string    `json:"catalogDigest"`
	Added           []string  `json:"added"`
	Changed         []string  `json:"changed"`
	MetadataChanged []string  `json:"metadataChanged"`
	RemovedUpstream []string  `json:"removedUpstream"`
	Held            []revHold `json:"held"`
	Excluded        []string  `json:"excluded"`
	Tags            struct {
		Created  []string `json:"created"`
		Existing []string `json:"existing"`
	} `json:"tags"`
	CatalogSize     int64 `json:"catalogSize"`
	Unchanged       int   `json:"unchanged"`
	UploadedSchemas int   `json:"uploadedSchemas"`
	ReusedSchemas   int   `json:"reusedSchemas"`
}

// revDiff is the --json result of schepherd-publisher diff.
type revDiff struct {
	Added           []string  `json:"added"`
	Changed         []string  `json:"changed"`
	MetadataChanged []string  `json:"metadataChanged"`
	Held            []revHold `json:"held"`
	Excluded        []string  `json:"excluded"`
	Unchanged       int       `json:"unchanged"`
	HasChanges      bool      `json:"hasChanges"`
}

// revState is the part of catalog/state.json these scenarios read.
type revState struct {
	Catalog struct {
		Revision string `json:"revision"`
		Digest   string `json:"digest"`
		Size     int64  `json:"size"`
	} `json:"catalog"`
	Schemas []revSchema `json:"schemas"`
}

// revSchema is the part of a state record these scenarios read.
type revSchema struct {
	ID                  string          `json:"id"`
	FirstRevision       string          `json:"firstRevision"`
	ArtifactRevision    string          `json:"artifactRevision"`
	LastChangedRevision string          `json:"lastChangedRevision"`
	HeldSinceRevision   string          `json:"heldSinceRevision"`
	HeldReason          string          `json:"heldReason"`
	ExcludedRevision    string          `json:"excludedRevision"`
	Entry               json.RawMessage `json:"entry"`
}

// revPublisher publishes prepared sets into one repository the way the
// weekly job does: --now fixes the UTC minute of the revision.
type revPublisher struct {
	repo string
}

func (p revPublisher) run(t *testing.T, prepared, now string, extra ...string) result {
	t.Helper()

	args := append([]string{
		"publish", "--prepared", prepared, "--repository", p.repo, "--registry-config", publisherRegistryConfig(t),
		"--now", now, "--json",
	}, extra...)

	return publisher(t, runOpts{}, args...)
}

func (p revPublisher) ok(t *testing.T, prepared, now string, extra ...string) revPublish {
	t.Helper()

	return decodeJSON[revPublish](t, p.run(t, prepared, now, extra...).ok(t).Stdout)
}

// revStep stops a scenario whose steps depend on each other when a step
// failed.
func revStep(t *testing.T, passed bool) {
	t.Helper()

	if !passed {
		t.FailNow()
	}
}

// revCatalogAt reads the catalog document a tag points to directly from the
// registry and returns it with the catalog index digest.
func revCatalogAt(t *testing.T, reg *registry, path, ref string) (catalogDoc, string) {
	t.Helper()

	c := reg.Catalog(t, path, ref)

	return c.Doc, c.Index.Digest
}

func revTagPointsTo(t *testing.T, reg *registry, path, tag, dgst string) {
	t.Helper()

	if got := reg.Manifest(t, path, tag).Digest; got != dgst {
		t.Fatalf("%s points to %s, want %s", tag, got, dgst)
	}
}

// revRevisionTags returns the catalog-<revision> tags of a repository in
// byte order, which for YYYYMMDD.HHMM is the chronological order.
func revRevisionTags(t *testing.T, reg *registry, path string) []string {
	t.Helper()

	var out []string

	for _, tag := range reg.Tags(t, path) {
		if strings.HasPrefix(tag, "catalog-") && tag != "catalog-latest" {
			out = append(out, tag)
		}
	}

	slices.Sort(out)

	return out
}

// revManifestPuts returns the reference of every manifest PUT the proxy saw.
func revManifestPuts(t *testing.T, px *proxy) []string {
	t.Helper()

	var refs []string

	for _, r := range px.Records(t) {
		if r.Class == regproxy.ClassManifestPut {
			refs = append(refs, r.Path[strings.LastIndex(r.Path, "/manifests/")+len("/manifests/"):])
		}
	}

	return refs
}

func revFaultApplied(t *testing.T, px *proxy, name string) {
	t.Helper()

	if !slices.ContainsFunc(px.Records(t), func(r regproxy.Record) bool { return r.Fault == name }) {
		t.Fatalf("fault %s never applied; records: %+v", name, px.Records(t))
	}
}

// revForeignCatalog pushes a catalog of one schema that no fixture set has,
// with revision in its document, tags it catalog-<revision> and returns its
// digest. The revision is not validated, so it may be an invalid one.
func revForeignCatalog(t *testing.T, reg *registry, path, revision string) string {
	t.Helper()

	content := []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","title":"foreign","type":"string"}`)
	schema := pushRaw(t, reg, path, rawArtifact{Layers: []rawLayer{schemaLayer(mediaTypeSchemaJSON, content, content)}})

	doc, err := json.Marshal(catalogDoc{FormatVersion: catalogFormatVersion, Revision: revision, Schemas: []catalogEntry{
		{ID: "foreign", Name: "Foreign", FileMatch: []string{"foreign.json"}, Artifact: descriptorOf(schema)},
	}})
	if err != nil {
		t.Fatal(err)
	}

	return pushRawCatalog(t, reg, path, doc, "catalog-"+revision).Digest.String()
}

func revReadState(t *testing.T, path string) revState {
	t.Helper()

	return decodeJSON[revState](t, readFile(t, path))
}

func revNoFile(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s was written (%v)", path, err)
	}
}

// TestE35_CatalogRevisions checks how the publisher allocates catalog
// revisions (docs/publishing.md, "Publishing"; docs/versioning.md) through
// the real binaries: a revision is the UTC minute of its publication, an
// unchanged set is a noop that does not contact the registry, a publication
// interrupted before its state was written resumes its revision at any later
// time, a minute that another catalog took is refused without moving its
// tag, a clock behind the newest revision is refused, and revision tags sort
// by time as text.
func TestE35_CatalogRevisions(t *testing.T) {
	reg := suite.source
	px := newProxy(t, reg)
	path := repoPath(t)
	pub := revPublisher{repo: px.Repo(path)}
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")

	basic := prepareSet(t, newSet(t, "set-basic"), "--state", stateFile)
	v2 := prepareSet(t, newSet(t, "set-basic-v2"), "--state", stateFile)

	var first revPublish

	revStep(t, t.Run("the first publication takes its UTC minute and writes the state", func(t *testing.T) {
		px.Reset(t)

		first = pub.ok(t, basic, "20260101.0900", "--state", stateFile, "--state-out", stateFile, "--update-latest")
		if first.Status != "published" || first.Revision != "20260101.0900" || first.UploadedSchemas != 6 || len(first.Added) != 6 {
			t.Fatalf("result = %+v", first)
		}

		doc, dgst := revCatalogAt(t, reg, path, "catalog-20260101.0900")
		if doc.Revision != "20260101.0900" || dgst != first.CatalogDigest || len(doc.Schemas) != 6 {
			t.Fatalf("catalog-20260101.0900 holds revision %q (%s) with %d schemas", doc.Revision, dgst, len(doc.Schemas))
		}

		revTagPointsTo(t, reg, path, "catalog-latest", first.CatalogDigest)

		if tags := reg.Tags(t, path); !slices.Equal(tags, []string{"catalog-20260101.0900", "catalog-latest"}) {
			t.Errorf("tags %v, want only the revision tag and catalog-latest", tags)
		}

		st := revReadState(t, stateFile)
		if st.Catalog.Revision != first.Revision || st.Catalog.Digest != first.CatalogDigest || st.Catalog.Size != first.CatalogSize ||
			len(st.Schemas) != 6 {
			t.Errorf("state = %+v", st)
		}
	}))

	revStep(t, t.Run("an unchanged set is a noop without registry requests", func(t *testing.T) {
		px.Reset(t)

		recorded := readFile(t, stateFile)
		out := filepath.Join(dir, "noop.json")

		d := decodeJSON[revDiff](t, publisher(t, runOpts{}, "diff", "--prepared", basic, "--state", stateFile, "--json").ok(t).Stdout)
		if d.HasChanges || d.Unchanged != 6 || len(d.Added)+len(d.Changed)+len(d.MetadataChanged)+len(d.Held)+len(d.Excluded) != 0 {
			t.Errorf("diff = %+v", d)
		}

		res := pub.ok(t, basic, "20260108.0900", "--state", stateFile, "--state-out", out)
		if res.Status != "noop" || res.Revision != first.Revision || res.CatalogDigest != first.CatalogDigest {
			t.Errorf("result = %+v", res)
		}

		if stats := px.Stats(t); stats.Total != 0 {
			t.Errorf("the noop sent %d registry requests: %v", stats.Total, stats.ByClass)
		}

		if !bytes.Equal(readFile(t, out), recorded) {
			t.Error("the noop wrote a different state")
		}
	}))

	interrupted := filepath.Join(dir, "locked", "interrupted.json")

	var resumed revPublish

	revStep(t, t.Run("a publication interrupted before its state was written resumes a day later", func(t *testing.T) {
		px.Reset(t)

		// A read-only directory for the state makes the run fail after the
		// catalog and its revision tag exist, as a crash would. The suite
		// runs unprivileged, so the directory permissions hold.
		locked := filepath.Dir(interrupted)
		if err := os.Mkdir(locked, 0o500); err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

		res := pub.run(t, v2, "20260115.0900", "--state", stateFile, "--state-out", interrupted, "--update-latest").wantCode(t, 2)
		if len(res.Stdout) != 0 || !strings.Contains(string(res.Stderr), "its state was not written") {
			t.Errorf("the failed publication wrote a result or another error:\n%s", res)
		}

		revNoFile(t, interrupted)

		if err := os.Chmod(locked, 0o700); err != nil {
			t.Fatal(err)
		}

		revTagPointsTo(t, reg, path, "catalog-latest", first.CatalogDigest)

		taken, dgst := revCatalogAt(t, reg, path, "catalog-20260115.0900")
		if taken.Revision != "20260115.0900" {
			t.Fatalf("catalog-20260115.0900 holds revision %q", taken.Revision)
		}

		px.ClearFaults()
		px.Reset(t)

		resumed = pub.ok(t, v2, "20260116.1000", "--state", stateFile, "--state-out", interrupted, "--update-latest")
		if resumed.Status != "resumed" || resumed.Revision != "20260115.0900" || resumed.CatalogDigest != dgst ||
			!slices.Equal(resumed.Changed, []string{"beta"}) || resumed.UploadedSchemas != 0 || resumed.ReusedSchemas != 5 {
			t.Fatalf("result = %+v", resumed)
		}

		if stats := px.Stats(t); stats.ByClass[regproxy.ClassBlobUpload] != 0 {
			t.Errorf("the resumed publication uploaded blobs: %+v", stats)
		}

		puts := revManifestPuts(t, px)
		slices.Sort(puts)

		if want := []string{"catalog-latest"}; !slices.Equal(puts, want) {
			t.Errorf("manifest PUTs %v, want %v", puts, want)
		}

		st := revReadState(t, interrupted)
		if st.Catalog.Revision != "20260115.0900" || st.Catalog.Digest != dgst {
			t.Errorf("state catalog = %+v", st.Catalog)
		}

		for _, rec := range st.Schemas {
			if want := map[bool]string{true: "20260115.0900", false: "20260101.0900"}[rec.ID == "beta"]; rec.LastChangedRevision != want ||
				rec.FirstRevision != "20260101.0900" {
				t.Errorf("%s: first %s, last changed %s", rec.ID, rec.FirstRevision, rec.LastChangedRevision)
			}
		}

		revTagPointsTo(t, reg, path, "catalog-latest", dgst)
	}))

	revStep(t, t.Run("the same minute with other content is refused", func(t *testing.T) {
		out := filepath.Join(dir, "refused.json")

		for _, extra := range [][]string{{"--state", interrupted, "--state-out", out}, nil} {
			px.Reset(t)

			res := pub.run(t, basic, "20260115.0900", extra...).wantCode(t, 2)
			if len(res.Stdout) != 0 || !strings.Contains(string(res.Stderr), "catalog revision 20260115.0900 already exists") {
				t.Errorf("with %v:\n%s", extra, res)
			}

			if puts := revManifestPuts(t, px); len(puts) != 0 {
				t.Errorf("with %v the refused publication wrote manifests %v", extra, puts)
			}
		}

		revNoFile(t, out)
		revTagPointsTo(t, reg, path, "catalog-20260115.0900", resumed.CatalogDigest)
		revTagPointsTo(t, reg, path, "catalog-latest", resumed.CatalogDigest)
	}))

	t.Run("another publication that took the minute after the tag listing is refused", func(t *testing.T) {
		px.Reset(t)

		tagsBefore := reg.Tags(t, path)

		// A stale tag list (the repository seems empty) is what a publisher
		// sees when another one takes the minute after the listing.
		px.AddFault(t, regproxy.Fault{Name: "stale-tag-list", Class: regproxy.ClassTags, Status: http.StatusNotFound, Times: 1})
		defer px.ClearFaults()

		res := pub.run(t, basic, "20260115.0900").wantCode(t, 2)
		if !strings.Contains(string(res.Stderr), "20260115.0900 already exists") {
			t.Errorf("the error does not name the taken revision:\n%s", res)
		}

		revFaultApplied(t, px, "stale-tag-list")

		if puts := revManifestPuts(t, px); slices.ContainsFunc(puts, func(ref string) bool { return strings.HasPrefix(ref, "catalog-") }) {
			t.Errorf("the refused publication wrote catalog tags %v", puts)
		}

		if after := reg.Tags(t, path); !slices.Equal(after, tagsBefore) {
			t.Errorf("tags changed from %v to %v", tagsBefore, after)
		}

		revTagPointsTo(t, reg, path, "catalog-20260115.0900", resumed.CatalogDigest)
	})

	t.Run("a clock behind the newest revision is refused", func(t *testing.T) {
		px.Reset(t)

		res := pub.run(t, basic, "20260110.1200", "--state", interrupted).wantCode(t, 2)
		if !strings.Contains(string(res.Stderr), "revision 20260110.1200 of the publication time is older than revision 20260115.0900") {
			t.Errorf("stderr:\n%s", res)
		}

		if puts := revManifestPuts(t, px); len(puts) != 0 {
			t.Errorf("manifest PUTs %v", puts)
		}
	})

	t.Run("the next minute publishes and revision tags sort by time", func(t *testing.T) {
		res := pub.ok(t, basic, "20260115.0901", "--state", interrupted, "--update-latest")
		if res.Status != "published" || res.Revision != "20260115.0901" || res.UploadedSchemas != 0 || !slices.Equal(res.Changed, []string{"beta"}) {
			t.Fatalf("result = %+v", res)
		}

		revTagPointsTo(t, reg, path, "catalog-latest", res.CatalogDigest)
		revTagPointsTo(t, reg, path, "catalog-20260101.0900", first.CatalogDigest)

		want := []string{"catalog-20260101.0900", "catalog-20260115.0900", "catalog-20260115.0901"}
		if got := revRevisionTags(t, reg, path); !slices.Equal(got, want) {
			t.Errorf("revision tags %v, want %v", got, want)
		}
	})
}

// TestE35_RevisionGrammar checks the revision grammar YYYYMMDD.HHMM
// (docs/versioning.md) through the real binaries: the publisher refuses
// impossible dates and times and the retired YYYYMMDD.N form, and the client
// refuses catalogs whose revision is not a real UTC minute while accepting
// valid ones.
func TestE35_RevisionGrammar(t *testing.T) {
	t.Parallel()

	t.Run("the publisher refuses invalid revision times", func(t *testing.T) {
		t.Parallel()

		prepared := prepareSet(t, newSet(t, "set-basic"))
		path := repoPath(t)
		pub := revPublisher{repo: suite.source.Repo(path)}

		for _, now := range []string{"20260230.1200", "20250229.0000", "20260101.2400", "20260101.1260", "20260101.1", "2026-01-01.1200", "19991231.2359"} {
			res := pub.run(t, prepared, now).wantCode(t, 2)
			if len(res.Stdout) != 0 || !strings.Contains(string(res.Stderr), now) {
				t.Errorf("--now %s:\n%s", now, res)
			}
		}

		if tags := suite.source.Tags(t, path); tags != nil {
			t.Errorf("refused publications created tags %v", tags)
		}

		res := pub.ok(t, prepared, "20280229.2359")
		if res.Status != "published" || res.Revision != "20280229.2359" {
			t.Errorf("a leap day = %+v", res)
		}
	})

	t.Run("the client refuses catalogs whose revision is invalid", func(t *testing.T) {
		t.Parallel()

		for _, revision := range []string{"20260230.1200", "20260101.1", "20260101.2400", "20260101.0960"} {
			path := repoPath(t, "r"+strings.ReplaceAll(revision, ".", "-"))
			dgst := revForeignCatalog(t, suite.source, path, revision)
			cfg := writeConfig(t, t.TempDir(), clientConfig{Repository: suite.source.Repo(path), Catalog: dgst}.TOML())

			for _, args := range [][]string{{"catalog", "--json"}, {"list"}} {
				res := cli(t, runOpts{}, append([]string{"--config", cfg}, args...)...).wantCode(t, 5)
				if len(res.Stdout) != 0 || !strings.Contains(string(res.Stderr), revision) {
					t.Errorf("a catalog with revision %s must be refused as invalid (exit 5):\n%s", revision, res)
				}
			}
		}

		path := repoPath(t, "valid")
		dgst := revForeignCatalog(t, suite.source, path, "20261231.2359")
		cfg := writeConfig(t, t.TempDir(), clientConfig{Repository: suite.source.Repo(path), Catalog: dgst}.TOML())

		res := cli(t, runOpts{}, "--config", cfg, "catalog", "--json").ok(t)
		if !strings.Contains(string(res.Stdout), "20261231.2359") {
			t.Errorf("catalog --json of revision 20261231.2359:\n%s", res)
		}
	})
}

// revRecord returns the state record of id.
func revRecord(t *testing.T, path, id string) revSchema {
	t.Helper()

	for _, rec := range revReadState(t, path).Schemas {
		if rec.ID == id {
			return rec
		}
	}

	t.Fatalf("the state has no record %s", id)

	return revSchema{}
}

// revPrepare runs prepare with the state and returns the prepared directory
// and the decoded result.
func revPrepare[T any](t *testing.T, source, stateFile string) (string, T) {
	t.Helper()

	prepared := filepath.Join(t.TempDir(), "prepared")
	res := publisher(t, runOpts{}, "prepare", "--source", source, "--out", prepared, "--jsonschema", suite.jsonschema,
		"--state", stateFile, "--json").ok(t)

	return prepared, decodeJSON[T](t, res.Stdout)
}

// revEdit replaces old with replacement in a file of a fixture copy.
func revEdit(t *testing.T, path, old, replacement string) {
	t.Helper()

	data := string(readFile(t, path))
	if !strings.Contains(data, old) {
		t.Fatalf("%s does not contain %q", path, old)
	}

	writeFile(t, path, []byte(strings.Replace(data, old, replacement, 1)))
}

// revPrepareResult is the part of the prepare result these scenarios read.
type revPrepareResult struct {
	Held     []revHold `json:"held"`
	Excluded []struct {
		ID   string `json:"id"`
		Rule string `json:"rule"`
	} `json:"excluded"`
	Regressions []any `json:"regressions"`
	Totals      struct {
		Entries int `json:"entries"`
		Reused  int `json:"reused"`
	} `json:"totals"`
}

// TestE35_RemovedUpstream checks the weekly state flow for a schema that
// upstream no longer lists (docs/publishing.md) through the real binaries:
// prepare holds it without failing and reuses the unchanged schemas, diff
// and publish report the hold, which alone changes no catalog and publishes
// nothing, and the next publication keeps its entry and published artifact,
// uploads nothing for it and records the hold in the state. A later
// takedown rule removes the held schema: it is excluded, never reported as
// removed upstream.
func TestE35_RemovedUpstream(t *testing.T) {
	t.Parallel()

	path := repoPath(t)
	pub := revPublisher{repo: suite.source.Repo(path)}
	stateFile := filepath.Join(t.TempDir(), "state.json")
	set := newSet(t, "set-basic")
	source := filepath.Join(set, "source.toml")

	first := pub.ok(t, prepareSet(t, set, "--state", stateFile), "20260201.0300", "--state", stateFile, "--state-out", stateFile)
	if first.Status != "published" {
		t.Fatalf("first = %+v", first)
	}

	before, _ := revCatalogAt(t, suite.source, path, "catalog-"+first.Revision)
	recorded := readFile(t, stateFile)

	data := string(readFile(t, source))
	start := strings.Index(data, "[[entries]]\nid = \"delta\"")
	end := strings.Index(data[start+1:], "[[entries]]")

	if start < 0 || end < 0 {
		t.Fatalf("set-basic has no delta entry followed by another:\n%s", data)
	}

	writeFile(t, source, []byte(data[:start]+data[start+1+end:]))

	held := []revHold{{ID: "delta", Reason: "removed-upstream"}}

	prepared, report := revPrepare[revPrepareResult](t, source, stateFile)
	if !reflect.DeepEqual(report.Held, held) || len(report.Regressions)+len(report.Excluded) != 0 || report.Totals.Reused != 5 {
		t.Fatalf("prepare result: %+v", report)
	}

	d := decodeJSON[revDiff](t, publisher(t, runOpts{}, "diff", "--prepared", prepared, "--state", stateFile, "--json").ok(t).Stdout)
	if d.HasChanges || !reflect.DeepEqual(d.Held, held) || len(d.Added)+len(d.Changed)+len(d.MetadataChanged)+len(d.Excluded) != 0 ||
		d.Unchanged != 6 {
		t.Errorf("diff = %+v", d)
	}

	alone := pub.ok(t, prepared, "20260208.0300", "--state", stateFile, "--state-out", stateFile)
	if alone.Status != "noop" || !reflect.DeepEqual(alone.Held, held) || !slices.Equal(alone.RemovedUpstream, []string{"delta"}) {
		t.Errorf("a hold alone = %+v", alone)
	}

	if !bytes.Equal(readFile(t, stateFile), recorded) {
		t.Error("a hold alone changed the state")
	}

	revEdit(t, source, `description = "Alpha fixture:`, `description = "Alpha fixture, renamed:`)

	prepared, _ = revPrepare[revPrepareResult](t, source, stateFile)

	second := pub.ok(t, prepared, "20260215.0300", "--state", stateFile, "--state-out", stateFile)
	if second.Status != "published" || !reflect.DeepEqual(second.Held, held) || !slices.Equal(second.MetadataChanged, []string{"alpha"}) ||
		len(second.Changed)+len(second.Added)+len(second.Excluded) != 0 || second.UploadedSchemas != 0 || second.ReusedSchemas != 6 {
		t.Fatalf("second = %+v", second)
	}

	after, _ := revCatalogAt(t, suite.source, path, "catalog-20260215.0300")
	if after.entry(t, "delta").Artifact != before.entry(t, "delta").Artifact || after.entry(t, "alpha").Artifact != before.entry(t, "alpha").Artifact {
		t.Errorf("artifacts changed: delta %+v -> %+v", before.entry(t, "delta").Artifact, after.entry(t, "delta").Artifact)
	}

	if rec := revRecord(t, stateFile, "delta"); rec.HeldSinceRevision != "20260215.0300" || rec.HeldReason != "removed-upstream" ||
		rec.LastChangedRevision != first.Revision {
		t.Errorf("delta record = %+v", rec)
	}

	if rec := revRecord(t, stateFile, "alpha"); rec.ArtifactRevision != first.Revision || rec.LastChangedRevision != "20260215.0300" {
		t.Errorf("alpha record = %+v", rec)
	}

	licenses := filepath.Join(set, "licenses.toml")
	writeFile(t, licenses, append(readFile(t, licenses), []byte(`
[[rules]]
id = "takedown"
decision = "exclude"
urls = ["https://schemas.example.com/e2e/delta.json"]
reason = "takedown request"
`)...))

	prepared, report = revPrepare[revPrepareResult](t, source, stateFile)
	if len(report.Held) != 0 || len(report.Excluded) != 1 || report.Excluded[0].ID != "delta" || report.Excluded[0].Rule != "takedown" {
		t.Fatalf("prepare result after the takedown: %+v", report)
	}

	third := pub.ok(t, prepared, "20260222.0300", "--state", stateFile, "--state-out", stateFile)
	if third.Status != "published" || !slices.Equal(third.Excluded, []string{"delta"}) ||
		len(third.Held)+len(third.RemovedUpstream)+third.UploadedSchemas != 0 {
		t.Fatalf("third = %+v", third)
	}

	if doc, _ := revCatalogAt(t, suite.source, path, "catalog-20260222.0300"); slices.ContainsFunc(doc.Schemas,
		func(e catalogEntry) bool { return e.ID == "delta" }) {
		t.Error("the catalog still lists delta after the takedown")
	}

	if rec := revRecord(t, stateFile, "delta"); rec.ExcludedRevision != "20260222.0300" || rec.HeldSinceRevision != "" || rec.HeldReason != "" {
		t.Errorf("delta record after the takedown = %+v", rec)
	}
}

// TestE35_ExcludedByRule checks that an explicit exclude rule is the one
// way a published schema leaves the catalog: prepare and diff report it as
// excluded, never as removed upstream, the next revision drops it, the state
// keeps its record with excludedRevision so its ID stays reserved, and when
// the rule goes away it returns as added with its published artifact.
func TestE35_ExcludedByRule(t *testing.T) {
	t.Parallel()

	path := repoPath(t)
	pub := revPublisher{repo: suite.source.Repo(path)}
	stateFile := filepath.Join(t.TempDir(), "state.json")
	set := newSet(t, "set-basic")
	source, licenses := filepath.Join(set, "source.toml"), filepath.Join(set, "licenses.toml")
	policy := readFile(t, licenses)

	first := pub.ok(t, prepareSet(t, set, "--state", stateFile), "20260301.0300", "--state", stateFile, "--state-out", stateFile)
	before, _ := revCatalogAt(t, suite.source, path, "catalog-"+first.Revision)

	writeFile(t, licenses, append(append([]byte{}, policy...), []byte(`
[[rules]]
id = "takedown"
decision = "exclude"
urls = ["https://schemas.example.com/e2e/gamma.json"]
reason = "takedown request"
`)...))

	prepared, report := revPrepare[revPrepareResult](t, source, stateFile)
	if len(report.Excluded) != 1 || report.Excluded[0].ID != "gamma" || report.Excluded[0].Rule != "takedown" ||
		len(report.Held)+len(report.Regressions) != 0 {
		t.Fatalf("prepare result: %+v", report)
	}

	d := decodeJSON[revDiff](t, publisher(t, runOpts{}, "diff", "--prepared", prepared, "--state", stateFile, "--json").ok(t).Stdout)
	if !d.HasChanges || !slices.Equal(d.Excluded, []string{"gamma"}) || len(d.Held) != 0 || d.Unchanged != 5 {
		t.Errorf("diff = %+v", d)
	}

	second := pub.ok(t, prepared, "20260308.0300", "--state", stateFile, "--state-out", stateFile)
	if second.Status != "published" || !slices.Equal(second.Excluded, []string{"gamma"}) || len(second.RemovedUpstream)+len(second.Held) != 0 ||
		second.UploadedSchemas != 0 {
		t.Fatalf("second = %+v", second)
	}

	if doc, _ := revCatalogAt(t, suite.source, path, "catalog-20260308.0300"); len(doc.Schemas) != 5 ||
		slices.ContainsFunc(doc.Schemas, func(e catalogEntry) bool { return e.ID == "gamma" }) {
		t.Errorf("the catalog still lists gamma: %d entries", len(doc.Schemas))
	}

	if rec := revRecord(t, stateFile, "gamma"); rec.ExcludedRevision != "20260308.0300" || rec.HeldSinceRevision != "" {
		t.Errorf("gamma record = %+v", rec)
	}

	writeFile(t, licenses, policy)

	prepared, _ = revPrepare[revPrepareResult](t, source, stateFile)

	back := pub.ok(t, prepared, "20260315.0300", "--state", stateFile, "--state-out", stateFile)
	if back.Status != "published" || !slices.Equal(back.Added, []string{"gamma"}) || back.UploadedSchemas != 0 {
		t.Fatalf("back = %+v", back)
	}

	after, _ := revCatalogAt(t, suite.source, path, "catalog-20260315.0300")
	if after.entry(t, "gamma").Artifact != before.entry(t, "gamma").Artifact {
		t.Errorf("gamma artifact changed from %+v to %+v", before.entry(t, "gamma").Artifact, after.entry(t, "gamma").Artifact)
	}

	if rec := revRecord(t, stateFile, "gamma"); rec.ExcludedRevision != "" || rec.FirstRevision != "20260315.0300" {
		t.Errorf("gamma record after its return = %+v", rec)
	}
}

// revLatest is the --json result of schepherd-publisher latest.
type revLatest struct {
	Repository    string `json:"repository"`
	Revision      string `json:"revision"`
	CatalogDigest string `json:"catalogDigest"`
	Latest        string `json:"latest"`
	CatalogSize   int64  `json:"catalogSize"`
}

// TestE35_FinishFromState checks how the weekly job finishes a revision
// whose state was recorded but whose catalog-latest step did not run
// (docs/publishing.md, "Moving catalog-latest") through the real binaries:
// latest --check verifies the recorded catalog without writing, latest moves
// catalog-latest with its only manifest PUT, a second run writes nothing,
// and a state that lists other entries than the registry's catalog is
// refused (exit 5) without a write.
func TestE35_FinishFromState(t *testing.T) {
	reg := suite.source
	px := newProxy(t, reg)
	path := repoPath(t)
	pub := revPublisher{repo: px.Repo(path)}
	stateFile := filepath.Join(t.TempDir(), "state.json")

	first := pub.ok(t, prepareSet(t, newSet(t, "set-basic"), "--state", stateFile), "20260301.0300",
		"--state", stateFile, "--state-out", stateFile)
	if first.Status != "published" || slices.Contains(reg.Tags(t, path), "catalog-latest") {
		t.Fatalf("first = %+v, tags %v", first, reg.Tags(t, path))
	}

	latest := func(t *testing.T, state string, extra ...string) result {
		t.Helper()

		return publisher(t, runOpts{}, append([]string{
			"latest", "--repository", px.Repo(path), "--registry-config", publisherRegistryConfig(t), "--state", state, "--json",
		}, extra...)...)
	}

	revStep(t, t.Run("check verifies without writing", func(t *testing.T) {
		px.Reset(t)

		res := decodeJSON[revLatest](t, latest(t, stateFile, "--check").ok(t).Stdout)
		if res.Latest != "checked" || res.Revision != first.Revision || res.CatalogDigest != first.CatalogDigest ||
			res.CatalogSize != first.CatalogSize {
			t.Errorf("result = %+v", res)
		}

		if puts := revManifestPuts(t, px); len(puts) != 0 || slices.Contains(reg.Tags(t, path), "catalog-latest") {
			t.Errorf("the check wrote manifests %v", puts)
		}
	}))

	revStep(t, t.Run("latest moves catalog-latest once", func(t *testing.T) {
		px.Reset(t)

		res := decodeJSON[revLatest](t, latest(t, stateFile).ok(t).Stdout)
		if res.Latest != "moved" {
			t.Errorf("result = %+v", res)
		}

		if puts := revManifestPuts(t, px); !slices.Equal(puts, []string{"catalog-latest"}) {
			t.Errorf("manifest PUTs %v, want only catalog-latest", puts)
		}

		if stats := px.Stats(t); stats.ByClass[regproxy.ClassBlobUpload] != 0 {
			t.Errorf("latest uploaded blobs: %+v", stats)
		}

		revTagPointsTo(t, reg, path, "catalog-latest", first.CatalogDigest)

		px.Reset(t)

		if again := decodeJSON[revLatest](t, latest(t, stateFile).ok(t).Stdout); again.Latest != "current" {
			t.Errorf("second run = %+v", again)
		}

		if puts := revManifestPuts(t, px); len(puts) != 0 {
			t.Errorf("the second run wrote manifests %v", puts)
		}
	}))

	t.Run("a state that differs from the registry is refused", func(t *testing.T) {
		recorded := string(readFile(t, stateFile))
		if !strings.Contains(recorded, `"description": "Alpha fixture:`) {
			t.Fatalf("the state has no alpha description:\n%s", recorded)
		}

		edited := filepath.Join(t.TempDir(), "state.json")
		writeFile(t, edited, []byte(strings.Replace(recorded, `"description": "Alpha fixture:`, `"description": "Edited alpha fixture:`, 1)))

		px.Reset(t)

		for _, extra := range [][]string{{"--check"}, nil} {
			res := latest(t, edited, extra...).wantCode(t, 5)
			if len(res.Stdout) != 0 || !strings.Contains(string(res.Stderr), "does not list the entries the state records") {
				t.Errorf("with %v:\n%s", extra, res)
			}
		}

		if puts := revManifestPuts(t, px); len(puts) != 0 {
			t.Errorf("the refused runs wrote manifests %v", puts)
		}

		revTagPointsTo(t, reg, path, "catalog-latest", first.CatalogDigest)
	})
}
