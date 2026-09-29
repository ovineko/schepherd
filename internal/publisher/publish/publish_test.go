package publish

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/calver"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/prepare"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

const repository = "registry.example/org/schemas"

// week1 is the first publication in these tests; later runs add minutes,
// hours or days to it.
var week1 = time.Date(2026, 9, 23, 3, 0, 42, 0, time.UTC)

func digestOf(data []byte) godigest.Digest {
	return godigest.Digest(digest.FromBytes(data))
}

func gammaSchema(extra string) string {
	var b strings.Builder

	b.WriteString(`{"type":"object","properties":{`)

	for i := range 80 {
		if i > 0 {
			b.WriteString(",")
		}

		fmt.Fprintf(&b, `"setting%02d":{"type":"string","description":"Setting %d of the gamma component%s."}`, i, i, extra)
	}

	b.WriteString(`}}`)

	return b.String()
}

func baseSchemas() map[string]string {
	return map[string]string{
		"alpha": `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object"}`,
		"beta":  `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"string"}`,
		"gamma": gammaSchema(""),
		"delta": `{"type":"array"}`,
	}
}

// setOptions varies a prepared set beyond its schemas.
type setOptions struct {
	// names overrides the display name of an ID.
	names map[string]string
	// held and excluded list published schemas by ID, with the hold reason
	// and the exclude rule.
	held     map[string]string
	excluded map[string]string
	// reuse makes the listed IDs reused entries of the records in state.
	state *state.State
	reuse []string
	// notice is the notice of alpha; empty means the default.
	notice string
}

// writeSet writes a prepared set with one entry per schema; alpha carries a
// notice and a fileMatch pattern.
func writeSet(t *testing.T, schemas map[string]string) string {
	t.Helper()

	return writeSetWith(t, schemas, setOptions{})
}

func writeSetWith(t *testing.T, schemas map[string]string, opts setOptions) string {
	t.Helper()

	notice := []byte("Fixture notice.\n")
	if opts.notice != "" {
		notice = []byte(opts.notice)
	}

	set := &prepare.Set{
		Document: prepare.Document{
			FormatVersion: prepare.FormatVersion, Recipe: prepare.Recipe,
			Source: prepare.Source{Kind: prepare.KindLocal, Name: "fixtures"},
		},
		Schemas: map[string][]byte{},
		Notices: map[string][]byte{},
	}

	for _, id := range slices.Sorted(maps.Keys(schemas)) {
		body := []byte(schemas[id])

		name := strings.ToUpper(id[:1]) + id[1:]
		if override, ok := opts.names[id]; ok {
			name = override
		}

		entry := prepare.Entry{
			ID: id, Name: name, FileMatch: []string{}, Schema: prepare.SchemaPath(id),
			ContentDigest: digest.FromBytes(body),
			Provenance: catalog.Provenance{
				Source: "https://schemas.example/" + id + ".json", SourceDigest: digest.FromBytes(body), License: "MIT",
			},
		}

		if id == "alpha" {
			entry.FileMatch = []string{"alpha.json"}
			entry.Notice = prepare.NoticePathFor(notice)
			set.Notices[entry.Notice] = notice
		}

		if slices.Contains(opts.reuse, id) {
			entry = reusedEntry(t, opts.state, id, name)
		} else {
			set.Schemas[id] = body
		}

		set.Document.Entries = append(set.Document.Entries, entry)
	}

	for _, id := range slices.Sorted(maps.Keys(opts.held)) {
		set.Document.Held = append(set.Document.Held, prepare.Hold{ID: id, Reason: opts.held[id]})
	}

	for _, id := range slices.Sorted(maps.Keys(opts.excluded)) {
		set.Document.Excluded = append(set.Document.Excluded, prepare.Exclusion{ID: id, Rule: opts.excluded[id]})
	}

	dir := filepath.Join(t.TempDir(), "prepared")
	if err := prepare.WriteSet(dir, set, nil); err != nil {
		t.Fatal(err)
	}

	return dir
}

// reusedEntry is the reused entry that keeps the recorded artifact of id,
// as prepare writes it for an unchanged source, under the given name.
func reusedEntry(t *testing.T, st *state.State, id, name string) prepare.Entry {
	t.Helper()

	rec, ok := st.Lookup(id)
	if !ok {
		t.Fatalf("the state has no %s to reuse", id)
	}

	artifact := rec.Entry.Artifact

	return prepare.Entry{
		ID: id, Name: name, Description: rec.Entry.Description, Dialect: rec.Entry.Dialect,
		FileMatch: append([]string{}, rec.Entry.FileMatch...), ContentDigest: rec.ContentDigest, NoticeDigest: rec.NoticeDigest,
		Reused: &artifact, Provenance: *rec.Entry.Provenance, License: rec.License,
	}
}

func publish(t *testing.T, repo *fakeRepo, dir string, opts Options) *Result {
	t.Helper()

	res, err := try(t, repo, dir, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	return res
}

func try(t *testing.T, repo *fakeRepo, dir string, opts Options) (*Result, error) {
	t.Helper()

	opts.PreparedDir = dir
	opts.Repository = repository

	if opts.Now.IsZero() {
		opts.Now = week1
	}

	return Run(t.Context(), repo, opts)
}

func catalogAt(t *testing.T, repo *fakeRepo, tag string) *catalog.Catalog {
	t.Helper()

	desc, err := repo.Resolve(t.Context(), tag)
	if err != nil {
		t.Fatal(err)
	}

	c, err := fetchCatalog(t.Context(), repo, desc.Digest.String())
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func encodeState(t *testing.T, s *state.State) []byte {
	t.Helper()

	data, err := state.Encode(s)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

func loadState(t *testing.T, path string) *state.State {
	t.Helper()

	s, err := state.Load(path)
	if err != nil || s == nil {
		t.Fatalf("load %s: %v, %v", path, s, err)
	}

	return s
}

// storedManifests returns the manifest digests pushed since the last
// resetCalls, in push order.
func storedManifests(repo *fakeRepo) []string {
	var manifests []string

	for _, s := range repo.stored {
		if kind, dgst, _ := strings.Cut(s, " "); kind == "manifest" {
			manifests = append(manifests, dgst)
		}
	}

	return manifests
}

// catalogPushes returns the manifests a publication of the catalog index
// dgst pushes last: its metadata manifest, then the index.
func catalogPushes(t *testing.T, repo *fakeRepo, dgst string) []string {
	t.Helper()

	var ix ocispec.Index
	if err := json.Unmarshal(repo.manifests[dgst], &ix); err != nil || len(ix.Manifests) == 0 {
		t.Fatalf("catalog index %s: %v", dgst, err)
	}

	return []string{ix.Manifests[0].Digest.String(), dgst}
}

func TestFirstPublicationWritesTheState(t *testing.T) {
	repo := newFakeRepo()
	dir := writeSet(t, baseSchemas())
	out := filepath.Join(t.TempDir(), "catalog", "state.json")

	res := publish(t, repo, dir, Options{StateOut: out, UpdateLatest: true})

	if res.Status != StatusPublished || res.Revision != "20260923.0300" || res.UploadedSchemas != 4 || res.ReusedSchemas != 0 ||
		!slices.Equal(res.Added, []string{"alpha", "beta", "delta", "gamma"}) || len(res.Changed)+len(res.RemovedUpstream) != 0 ||
		res.Unchanged != 0 || len(res.KeptArtifacts) != 0 {
		t.Fatalf("result = %+v", res)
	}

	wantTags := map[string]string{LatestTag: res.CatalogDigest, "catalog-20260923.0300": res.CatalogDigest}
	if !maps.Equal(repo.tags, wantTags) {
		t.Errorf("tags = %v, want only the catalog tags %v", repo.tags, wantTags)
	}

	if int64(len(repo.manifests[res.CatalogDigest])) != res.CatalogSize || repo.mediaTypes[res.CatalogDigest] != artifact.IndexMediaType {
		t.Errorf("catalogSize %d, index has %d bytes and media type %s", res.CatalogSize, len(repo.manifests[res.CatalogDigest]), repo.mediaTypes[res.CatalogDigest])
	}

	if !slices.Equal(res.Tags.Created, []string{"catalog-20260923.0300", LatestTag}) || len(res.Tags.Existing) != 0 {
		t.Errorf("tags = %+v", res.Tags)
	}

	published := catalogAt(t, repo, LatestTag)
	checkCatalog(t, repo, published)

	written := loadState(t, out)
	if !slices.Equal(encodeState(t, written), encodeState(t, res.State)) {
		t.Error("the written state differs from the result's state")
	}

	want := state.Catalog{Revision: "20260923.0300", Digest: res.CatalogDigest, Size: res.CatalogSize}
	if written.Catalog != want || written.Source != (state.Source{Kind: state.KindLocal, Name: "fixtures"}) ||
		written.Recipe != prepare.Recipe || len(written.Schemas) != 4 {
		t.Fatalf("state = %+v", written)
	}

	for _, rec := range written.Schemas {
		entry, _ := published.Lookup(rec.ID)

		same, err := sameEntry(&rec.Entry, entry)
		if err != nil || !same || rec.FirstRevision != "20260923.0300" || rec.LastChangedRevision != "20260923.0300" ||
			rec.ArtifactRevision != "" || rec.Held() || rec.Excluded() || rec.ContentDigest != digest.FromBytes([]byte(baseSchemas()[rec.ID])) {
			t.Errorf("state record %+v does not match catalog entry %+v", rec, entry)
		}
	}
}

// checkCatalog verifies that every entry resolves to a valid schema
// artifact whose content matches what was prepared.
func checkCatalog(t *testing.T, repo *fakeRepo, c *catalog.Catalog) {
	t.Helper()

	for _, e := range c.Schemas {
		manifest, err := repo.FetchManifestByDigest(t.Context(), e.Artifact.Digest, artifact.ManifestMediaType, 1<<20)
		if err != nil {
			t.Fatal(err)
		}

		sm, err := artifact.ParseSchemaManifest(manifest, artifact.DefaultLimits())
		if err != nil {
			t.Fatalf("%s: %v", e.ID, err)
		}

		var buf bytes.Buffer
		if err := artifact.DecodeSchema(&buf, bytes.NewReader(repo.blobs[sm.Payload.Digest.String()]), sm); err != nil {
			t.Fatalf("%s: %v", e.ID, err)
		}

		if want := baseSchemas()[e.ID]; e.ID != "beta" && buf.String() != want {
			t.Errorf("%s content = %s", e.ID, buf.String())
		}

		if (sm.Notice != nil) != (e.ID == "alpha") {
			t.Errorf("%s notice = %v", e.ID, sm.Notice)
		}
	}

	for tag := range repo.tags {
		if _, err := calver.ParseRevisionTag(tag); err != nil && tag != LatestTag {
			t.Errorf("tag %s is neither a revision tag nor %s", tag, LatestTag)
		}
	}

	gamma, ok := c.Lookup("gamma")
	if !ok {
		t.Fatal("no gamma")
	}

	sm, err := artifact.ParseSchemaManifest(repo.manifests[gamma.Artifact.Digest], artifact.DefaultLimits())
	if err != nil || sm.Payload.MediaType != artifact.SchemaGzipMediaType {
		t.Errorf("gamma payload = %+v, %v", sm, err)
	}

	alpha, _ := c.Lookup("alpha")
	if !slices.Equal(alpha.FileMatch, []string{"alpha.json"}) || alpha.Provenance.License != "MIT" {
		t.Errorf("alpha = %+v", alpha)
	}

	if delta, _ := c.Lookup("delta"); delta.FileMatch != nil {
		t.Errorf("delta fileMatch = %#v", delta.FileMatch)
	}
}

// An unchanged prepared set compared with the state is decided without the
// registry: no request at all, and the state comes out byte for byte.
func TestNoopNeedsNoRegistry(t *testing.T) {
	repo := newFakeRepo()
	dir := writeSet(t, baseSchemas())
	first := publish(t, repo, dir, Options{UpdateLatest: true})
	before := encodeState(t, first.State)

	repo.resetCalls()

	out := filepath.Join(t.TempDir(), "state.json")
	res := publish(t, repo, writeSet(t, baseSchemas()), Options{State: first.State, StateOut: out, Now: week1.Add(7 * 24 * time.Hour)})

	if res.Status != StatusNoop || res.Revision != first.Revision || res.CatalogDigest != first.CatalogDigest ||
		res.CatalogSize != first.CatalogSize || res.Unchanged != 4 || res.ReusedSchemas != 4 || res.UploadedSchemas != 0 ||
		len(res.Added)+len(res.Changed)+len(res.RemovedUpstream) != 0 || len(res.Tags.Created)+len(res.Tags.Existing) != 0 {
		t.Errorf("result = %+v", res)
	}

	if n := len(repo.calls); n != 0 {
		t.Errorf("a noop made registry calls: %v", repo.calls)
	}

	if written, err := os.ReadFile(out); err != nil || !bytes.Equal(written, before) {
		t.Errorf("the noop state differs from the input:\n%s\nwant\n%s", written, before)
	}

	for _, key := range []string{
		`"added":[]`, `"changed":[]`, `"metadataChanged":[]`, `"removedUpstream":[]`, `"held":[]`, `"excluded":[]`, `"keptArtifacts":[]`,
	} {
		if data, _ := json.Marshal(res); !bytes.Contains(data, []byte(key)) {
			t.Errorf("noop result JSON lacks %s: %s", key, data)
		}
	}
}

// catalog-latest moves in a noop run with UpdateLatest when a publication's
// last step failed, and only after the state's revision tag is checked.
func TestNoopFinishesCatalogLatest(t *testing.T) {
	repo := newFakeRepo()
	dir := writeSet(t, baseSchemas())
	first := publish(t, repo, dir, Options{})

	repo.resetCalls()

	res := publish(t, repo, dir, Options{State: first.State, UpdateLatest: true})
	if res.Status != StatusNoop || repo.tags[LatestTag] != first.CatalogDigest || !slices.Equal(res.Tags.Created, []string{LatestTag}) {
		t.Fatalf("result = %+v, latest %s", res, repo.tags[LatestTag])
	}

	if repo.count("PushBlob", "PushManifest", "EnsureTag") != 0 || repo.count("Tag") != 1 {
		t.Errorf("calls = %v", repo.calls)
	}

	repo.resetCalls()

	again := publish(t, repo, dir, Options{State: first.State, UpdateLatest: true})
	if again.Status != StatusNoop || repo.writes() != 0 || !slices.Equal(again.Tags.Existing, []string{LatestTag}) {
		t.Errorf("second noop = %+v, calls %v", again, repo.calls)
	}

	wrong := *first.State
	wrong.Catalog.Digest = digest.FromBytes([]byte("another catalog"))

	_, err := try(t, repo, dir, Options{State: &wrong, UpdateLatest: true})
	if fault.KindOf(err) != fault.Integrity || !strings.Contains(err.Error(), "catalog-20260923.0300 points to") {
		t.Errorf("a state that does not match the repository = %v", err)
	}

	missing := *first.State
	missing.Catalog.Revision = "20260923.0400"

	_, err = try(t, newFakeRepo(), dir, Options{State: &missing, UpdateLatest: true})
	if fault.KindOf(err) != fault.Integrity || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("a state whose revision the repository lacks = %v", err)
	}
}

// Without a state there is nothing to compare with: identical content
// resumes the newest revision instead of publishing it twice.
func TestWithoutStateIdenticalContentResumes(t *testing.T) {
	repo := newFakeRepo()
	dir := writeSet(t, baseSchemas())
	first := publish(t, repo, dir, Options{})

	repo.resetCalls()

	res := publish(t, repo, dir, Options{Now: week1.Add(3 * time.Hour)})
	if res.Status != StatusResumed || res.Revision != first.Revision || res.CatalogDigest != first.CatalogDigest ||
		res.UploadedSchemas != 0 || repo.count("PushManifest", "PushBlob") != 0 {
		t.Errorf("result = %+v, calls %v", res, repo.calls)
	}
}

func TestOneSchemaChanged(t *testing.T) {
	repo := newFakeRepo()
	first := publish(t, repo, writeSet(t, baseSchemas()), Options{UpdateLatest: true})
	before := catalogAt(t, repo, LatestTag)

	changed := baseSchemas()
	changed["beta"] = `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"string","minLength":1}`

	repo.resetCalls()

	later := week1.Add(7*24*time.Hour + 5*time.Minute)
	res := publish(t, repo, writeSet(t, changed), Options{State: first.State, Now: later, UpdateLatest: true})

	if res.Status != StatusPublished || res.Revision != "20260930.0305" || res.UploadedSchemas != 1 || res.ReusedSchemas != 3 ||
		!slices.Equal(res.Changed, []string{"beta"}) || res.Unchanged != 3 || len(res.Added)+len(res.RemovedUpstream) != 0 {
		t.Fatalf("result = %+v", res)
	}

	after := catalogAt(t, repo, LatestTag)

	for _, e := range after.Schemas {
		old, _ := before.Lookup(e.ID)
		if (old.Artifact.Digest != e.Artifact.Digest) != (e.ID == "beta") {
			t.Errorf("%s digest changed = %v", e.ID, old.Artifact.Digest != e.Artifact.Digest)
		}
	}

	beta, _ := after.Lookup("beta")
	if got := storedManifests(repo); !slices.Equal(got, append([]string{beta.Artifact.Digest}, catalogPushes(t, repo, res.CatalogDigest)...)) || len(repo.stored) != 5 {
		t.Errorf("stored = %v, want the beta artifact (payload, manifest) and then the catalog (catalog.json, metadata manifest, index)", repo.stored)
	}

	if repo.tags["catalog-20260923.0300"] != first.CatalogDigest {
		t.Error("an older revision tag moved")
	}

	next := res.State
	for _, rec := range next.Schemas {
		old, _ := first.State.Lookup(rec.ID)

		switch {
		case rec.ID == "beta":
			if rec.FirstRevision != "20260923.0300" || rec.LastChangedRevision != "20260930.0305" ||
				rec.ContentDigest != digest.FromBytes([]byte(changed["beta"])) || rec.Entry.Artifact.Digest != beta.Artifact.Digest {
				t.Errorf("beta record = %+v", rec)
			}
		case !slices.Equal(encodeRecord(t, rec), encodeRecord(t, *old)):
			t.Errorf("%s record changed: %+v", rec.ID, rec)
		}
	}
}

func encodeRecord(t *testing.T, rec state.Schema) []byte {
	t.Helper()

	entry, err := catalog.MarshalEntry(&rec.Entry)
	if err != nil {
		t.Fatal(err)
	}

	return fmt.Appendf(nil, "%s %s %s %s %s %s %s %s %s %v %s", rec.ID, rec.ContentDigest, rec.NoticeDigest, rec.FirstRevision,
		rec.ArtifactRevision, rec.LastChangedRevision, rec.HeldSinceRevision, rec.HeldReason, rec.ExcludedRevision, rec.License, entry)
}

// Metadata that changes without the content keeps the artifact.
func TestMetadataChangeReusesTheArtifact(t *testing.T) {
	repo := newFakeRepo()
	first := publish(t, repo, writeSet(t, baseSchemas()), Options{})

	repo.resetCalls()

	res := publish(t, repo, writeSetWith(t, baseSchemas(), setOptions{names: map[string]string{"delta": "Delta lists"}}),
		Options{State: first.State, Now: week1.Add(time.Hour)})

	if res.Status != StatusPublished || !slices.Equal(res.MetadataChanged, []string{"delta"}) || len(res.Changed) != 0 ||
		res.UploadedSchemas != 0 || res.ReusedSchemas != 4 {
		t.Fatalf("result = %+v", res)
	}

	if got := storedManifests(repo); !slices.Equal(got, catalogPushes(t, repo, res.CatalogDigest)) {
		t.Errorf("stored manifests = %v, want only the catalog", got)
	}

	rec, _ := res.State.Lookup("delta")
	old, _ := first.State.Lookup("delta")

	if rec.Entry.Name != "Delta lists" || rec.Entry.Artifact != old.Entry.Artifact || rec.LastChangedRevision != "20260923.0400" ||
		rec.ArtifactRevision != "20260923.0300" || rec.ArtifactSince() != old.LastChangedRevision {
		t.Errorf("delta record = %+v", rec)
	}

	content := baseSchemas()
	content["delta"] = `{"type":"array","minItems":1}`

	later := publish(t, repo, writeSetWith(t, content, setOptions{names: map[string]string{"delta": "Delta lists"}}),
		Options{State: res.State, Now: week1.Add(2 * time.Hour)})

	if rec, _ := later.State.Lookup("delta"); !slices.Equal(later.Changed, []string{"delta"}) || rec.ArtifactRevision != "" ||
		rec.ArtifactSince() != later.Revision {
		t.Errorf("new content after a metadata change: result %+v, record %+v", later, rec)
	}
}

// A reused entry keeps the artifact the state records without the prepared
// set carrying its bytes: new metadata changes the entry, nothing is packed
// or uploaded for it, and the recorded license decision stays.
func TestReusedEntryKeepsTheRecordedArtifact(t *testing.T) {
	repo := newFakeRepo()
	first := publish(t, repo, writeSet(t, baseSchemas()), Options{})
	recorded := withRecord(first.State, "alpha", func(rec *state.Schema) { rec.License = state.LicenseDecision{Rules: []string{"fixtures"}} })

	dir := writeSetWith(t, baseSchemas(), setOptions{
		names: map[string]string{"alpha": "Alpha config"}, state: recorded, reuse: []string{"alpha", "gamma"},
	})
	if _, err := os.Stat(filepath.Join(dir, "schemas", "alpha.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the prepared set carries the bytes of a reused entry: %v", err)
	}

	repo.resetCalls()

	res := publish(t, repo, dir, Options{State: recorded, Now: week1.Add(time.Hour)})
	if res.Status != StatusPublished || !slices.Equal(res.MetadataChanged, []string{"alpha"}) || res.UploadedSchemas != 0 ||
		res.ReusedSchemas != 4 || len(res.KeptArtifacts) != 0 {
		t.Fatalf("result = %+v", res)
	}

	before, _ := recorded.Lookup("alpha")
	rec, _ := res.State.Lookup("alpha")

	if rec.Entry.Name != "Alpha config" || rec.Entry.Artifact != before.Entry.Artifact || rec.NoticeDigest != before.NoticeDigest ||
		!rec.License.Equal(&before.License) {
		t.Errorf("alpha record = %+v", rec)
	}

	if got := storedManifests(repo); !slices.Equal(got, catalogPushes(t, repo, res.CatalogDigest)) {
		t.Errorf("stored manifests = %v, want only the catalog", got)
	}

	gone := newFakeRepo()
	if _, err := try(t, gone, dir, Options{State: recorded, Now: week1.Add(time.Hour)}); fault.KindOf(err) != fault.Integrity ||
		!strings.Contains(err.Error(), "cannot rebuild it") {
		t.Errorf("a reused artifact missing from the repository: %v", err)
	}
}

// Unchanged content keeps the artifact the state records even when packing
// it today gives other bytes. The state here records an alpha artifact that
// an earlier packing produced; the current packing gives another one.
func TestUnchangedContentKeepsItsArtifact(t *testing.T) {
	repo := newFakeRepo()
	first := publish(t, repo, writeSet(t, baseSchemas()), Options{})

	earlier := packedSchemaOf(t, writeSetWith(t, baseSchemas(), setOptions{notice: "An earlier packing.\n"}), "alpha")
	if err := (&run{target: repo}).pushArtifact(t.Context(), earlier); err != nil {
		t.Fatal(err)
	}

	recorded := *first.State
	recorded.Schemas = slices.Clone(first.State.Schemas)
	alphaBefore, _ := recorded.Lookup("alpha")
	current := alphaBefore.Entry.Artifact
	alphaBefore.Entry.Artifact = catalog.Descriptor{
		MediaType: earlier.Manifest.Descriptor.MediaType, Digest: earlier.Manifest.Descriptor.Digest.String(), Size: earlier.Manifest.Descriptor.Size,
	}

	if alphaBefore.Entry.Artifact.Digest == current.Digest {
		t.Fatal("the fixture does not pack alpha differently")
	}

	changed := baseSchemas()
	changed["beta"] = `{"type":"string","maxLength":3}`

	var (
		logs []string
		mu   sync.Mutex
	)

	repo.resetCalls()

	res := publish(t, repo, writeSet(t, changed), Options{State: &recorded, Now: week1.Add(time.Hour), Log: func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()

		logs = append(logs, fmt.Sprintf(format, args...))
	}})

	if res.Status != StatusPublished || !slices.Equal(res.Changed, []string{"beta"}) || !slices.Equal(res.KeptArtifacts, []string{"alpha"}) ||
		res.UploadedSchemas != 1 {
		t.Fatalf("result = %+v", res)
	}

	alpha, _ := catalogAt(t, repo, "catalog-"+res.Revision).Lookup("alpha")
	if alpha.Artifact != alphaBefore.Entry.Artifact {
		t.Errorf("alpha artifact = %+v, want the recorded %+v", alpha.Artifact, alphaBefore.Entry.Artifact)
	}

	if rec, _ := res.State.Lookup("alpha"); rec.Entry.Artifact != alphaBefore.Entry.Artifact || rec.LastChangedRevision != alphaBefore.LastChangedRevision {
		t.Errorf("alpha record = %+v", rec)
	}

	if !slices.ContainsFunc(logs, func(l string) bool {
		return strings.Contains(l, "warning: schema alpha is unchanged") && strings.Contains(l, alphaBefore.Entry.Artifact.Digest)
	}) {
		t.Errorf("no warning about alpha in %q", logs)
	}

	if text := res.String(); !strings.Contains(text, "kept artifacts that pack differently today: alpha") {
		t.Errorf("String() = %q", text)
	}
}

// A new notice for unchanged schema bytes is new content: the artifact must
// carry the notice that goes with the entry, so it is packed and uploaded
// again, and the state records the notice digest.
func TestNoticeChangeRepublishesTheSchema(t *testing.T) {
	repo := newFakeRepo()
	first := publish(t, repo, writeSet(t, baseSchemas()), Options{})
	alphaBefore, _ := first.State.Lookup("alpha")

	if alphaBefore.NoticeDigest != digest.FromBytes([]byte("Fixture notice.\n")) {
		t.Fatalf("alpha notice digest = %q", alphaBefore.NoticeDigest)
	}

	if beta, _ := first.State.Lookup("beta"); beta.NoticeDigest != "" {
		t.Errorf("beta has no notice but records %q", beta.NoticeDigest)
	}

	dir := writeSetWith(t, baseSchemas(), setOptions{notice: "A newer notice.\n"})
	repacked := packedSchemaOf(t, dir, "alpha")

	repo.resetCalls()

	res := publish(t, repo, dir, Options{State: first.State, Now: week1.Add(time.Hour)})
	if res.Status != StatusPublished || !slices.Equal(res.Changed, []string{"alpha"}) || res.UploadedSchemas != 1 ||
		len(res.KeptArtifacts) != 0 || res.ReusedSchemas != 3 {
		t.Fatalf("result = %+v", res)
	}

	alpha, _ := catalogAt(t, repo, "catalog-"+res.Revision).Lookup("alpha")
	if alpha.Artifact.Digest != repacked.Manifest.Descriptor.Digest.String() || alpha.Artifact == alphaBefore.Entry.Artifact {
		t.Errorf("alpha artifact = %+v, want the repacked %s", alpha.Artifact, repacked.Manifest.Descriptor.Digest)
	}

	if _, pushed := repo.manifests[repacked.Manifest.Descriptor.Digest.String()]; !pushed {
		t.Error("the artifact with the new notice was not uploaded")
	}

	rec, _ := res.State.Lookup("alpha")
	if rec.NoticeDigest != digest.FromBytes([]byte("A newer notice.\n")) || rec.LastChangedRevision != res.Revision ||
		rec.ContentDigest != alphaBefore.ContentDigest {
		t.Errorf("alpha record = %+v", rec)
	}

	again := publish(t, repo, dir, Options{State: res.State, Now: week1.Add(2 * time.Hour)})
	if again.Status != StatusNoop {
		t.Errorf("republishing the same notice = %+v", again)
	}
}

// A held schema keeps its entry and artifact. A hold alone publishes
// nothing; the state records it, with the revision it began in, once a
// revision is published, and forgets it once the schema is refreshed.
func TestHeldSchemaStaysInTheCatalog(t *testing.T) {
	repo := newFakeRepo()
	first := publish(t, repo, writeSet(t, baseSchemas()), Options{UpdateLatest: true})
	gammaBefore, _ := first.State.Lookup("gamma")
	week := 7 * 24 * time.Hour

	repo.resetCalls()

	alone := publish(t, repo, writeSetWith(t, without("gamma"), setOptions{held: map[string]string{"gamma": state.HeldRemovedUpstream}}),
		Options{State: first.State, Now: week1.Add(week), UpdateLatest: true})
	if alone.Status != StatusNoop || alone.State != first.State || !slices.Equal(alone.RemovedUpstream, []string{"gamma"}) ||
		!reflect.DeepEqual(alone.Held, []Hold{{ID: "gamma", Reason: state.HeldRemovedUpstream}}) || alone.Unchanged != 4 {
		t.Fatalf("a hold alone = %+v", alone)
	}

	changed := without("gamma")
	changed["beta"] = `{"type":"string","maxLength":9}`
	heldAt := week1.Add(2 * week)

	res := publish(t, repo, writeSetWith(t, changed, setOptions{held: map[string]string{"gamma": state.HeldRemovedUpstream}}),
		Options{State: first.State, Now: heldAt, UpdateLatest: true})

	if res.Status != StatusPublished || res.Revision != "20261007.0300" || !slices.Equal(res.RemovedUpstream, []string{"gamma"}) ||
		!slices.Equal(res.Changed, []string{"beta"}) || len(res.Added)+len(res.MetadataChanged)+len(res.Excluded) != 0 ||
		res.Unchanged != 3 || res.UploadedSchemas != 1 {
		t.Fatalf("result = %+v", res)
	}

	gamma, ok := catalogAt(t, repo, LatestTag).Lookup("gamma")
	if !ok || gamma.Artifact != gammaBefore.Entry.Artifact {
		t.Fatalf("gamma in the new catalog = %+v, %v", gamma, ok)
	}

	rec, _ := res.State.Lookup("gamma")
	if rec.HeldSinceRevision != res.Revision || rec.HeldReason != state.HeldRemovedUpstream || rec.LastChangedRevision != "20260923.0300" ||
		rec.ContentDigest != gammaBefore.ContentDigest || rec.Excluded() {
		t.Errorf("gamma record = %+v", rec)
	}

	changed["beta"] = `{"type":"string","maxLength":10}`

	other := publish(t, repo, writeSetWith(t, changed, setOptions{held: map[string]string{"gamma": state.HeldPrepareFailed}}),
		Options{State: res.State, Now: heldAt.Add(week)})
	if rec, _ := other.State.Lookup("gamma"); other.Status != StatusPublished || len(other.RemovedUpstream) != 0 ||
		rec.HeldSinceRevision != res.Revision || rec.HeldReason != state.HeldPrepareFailed {
		t.Errorf("held for another reason: result %+v, record %+v", other, rec)
	}

	back := baseSchemas()
	back["beta"] = changed["beta"]

	quiet := publish(t, repo, writeSet(t, back), Options{State: other.State, Now: heldAt.Add(2 * week)})
	if quiet.Status != StatusNoop || len(quiet.Held) != 0 {
		t.Errorf("a refresh that changes no entry = %+v", quiet)
	}

	cleared := publish(t, repo, writeSetWith(t, back, setOptions{names: map[string]string{"delta": "Delta v2"}}),
		Options{State: other.State, Now: heldAt.Add(3 * week)})
	if rec, _ := cleared.State.Lookup("gamma"); cleared.Status != StatusPublished || rec.Held() || rec.LastChangedRevision != "20260923.0300" ||
		rec.Entry.Artifact != gammaBefore.Entry.Artifact || !slices.Equal(cleared.MetadataChanged, []string{"delta"}) {
		t.Errorf("gamma after its refresh: result %+v, record %+v", cleared, rec)
	}
}

// Only an exclusion removes a published schema from the catalog. Its record
// stays in the state, so its ID stays reserved; when the rule goes away it
// returns as added, with its recorded artifact when the content is the same.
func TestExclusionDropsTheSchema(t *testing.T) {
	repo := newFakeRepo()
	first := publish(t, repo, writeSet(t, baseSchemas()), Options{UpdateLatest: true})
	gammaBefore, _ := first.State.Lookup("gamma")
	week := 7 * 24 * time.Hour

	repo.resetCalls()

	res := publish(t, repo, writeSetWith(t, without("gamma"), setOptions{excluded: map[string]string{"gamma": "takedown"}}),
		Options{State: first.State, Now: week1.Add(week), UpdateLatest: true})
	if res.Status != StatusPublished || !slices.Equal(res.Excluded, []string{"gamma"}) || len(res.RemovedUpstream)+len(res.Held) != 0 ||
		res.Unchanged != 3 || res.UploadedSchemas != 0 {
		t.Fatalf("result = %+v", res)
	}

	if _, ok := catalogAt(t, repo, LatestTag).Lookup("gamma"); ok {
		t.Error("the excluded gamma is still in the catalog")
	}

	rec, ok := res.State.Lookup("gamma")
	if !ok || rec.ExcludedRevision != res.Revision || rec.Held() || rec.Entry.Artifact != gammaBefore.Entry.Artifact ||
		len(res.State.Entries()) != 3 {
		t.Fatalf("gamma record = %+v, %v", rec, ok)
	}

	still := publish(t, repo, writeSet(t, without("gamma")), Options{State: res.State, Now: week1.Add(2 * week)})
	if still.Status != StatusNoop || len(still.Excluded) != 0 {
		t.Errorf("a week later, still excluded = %+v", still)
	}

	back := publish(t, repo, writeSet(t, baseSchemas()), Options{State: res.State, Now: week1.Add(3 * week)})
	if back.Status != StatusPublished || !slices.Equal(back.Added, []string{"gamma"}) || back.UploadedSchemas != 0 {
		t.Fatalf("gamma back = %+v", back)
	}

	rec, _ = back.State.Lookup("gamma")
	if rec.Excluded() || rec.FirstRevision != back.Revision || rec.LastChangedRevision != back.Revision ||
		rec.Entry.Artifact != gammaBefore.Entry.Artifact {
		t.Errorf("gamma record after its return = %+v", rec)
	}
}

// The weekly test job republishes the prepared set against the state its
// first publication wrote and requires a noop that writes the same state:
// the holds and exclusions the set lists are then already recorded.
func TestRepublishingAgainstTheWrittenStateIsANoop(t *testing.T) {
	repo := newFakeRepo()
	first := publish(t, repo, writeSet(t, baseSchemas()), Options{UpdateLatest: true})

	changed := without("gamma")
	delete(changed, "delta")
	changed["beta"] = `{"type":"string","maxLength":9}`
	dir := writeSetWith(t, changed, setOptions{
		held: map[string]string{"gamma": state.HeldLicenseDetectionFailed}, excluded: map[string]string{"delta": "takedown"},
	})

	second := publish(t, repo, dir, Options{State: first.State, Now: week1.Add(7 * 24 * time.Hour)})
	if second.Status != StatusPublished || !slices.Equal(second.Excluded, []string{"delta"}) || len(second.Held) != 1 {
		t.Fatalf("second = %+v", second)
	}

	set, err := prepare.Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := NewPlan(set, second.State)
	if err != nil {
		t.Fatalf("the set does not fit the state it wrote: %v", err)
	}

	if d := plan.Diff(); d.HasChanges || len(d.Excluded) != 0 || !reflect.DeepEqual(d.Held, []Hold{{ID: "gamma", Reason: state.HeldLicenseDetectionFailed}}) {
		t.Errorf("diff against the written state = %+v", d)
	}

	again := publish(t, repo, dir, Options{State: second.State, Now: week1.Add(8 * 24 * time.Hour)})
	if again.Status != StatusNoop || again.Revision != second.Revision || len(again.Excluded) != 0 {
		t.Errorf("republication = %+v", again)
	}

	before, err := state.Encode(second.State)
	if err != nil {
		t.Fatal(err)
	}

	after, err := state.Encode(again.State)
	if err != nil || !bytes.Equal(before, after) {
		t.Errorf("the noop wrote another state: %v\n%s\nwant\n%s", err, after, before)
	}
}

// A publication that crashed after its catalog and tags existed but before
// the state was written is resumed by the next run, at any later time.
func TestResumeAfterACrashBeforeTheStateWasWritten(t *testing.T) {
	repo := newFakeRepo()
	first := publish(t, repo, writeSet(t, baseSchemas()), Options{UpdateLatest: true})

	changed := baseSchemas()
	changed["delta"] = `{"type":"array","items":{"type":"string"}}`
	dir := writeSet(t, changed)

	unwritable := t.TempDir()
	crashAt := time.Date(2026, 9, 30, 23, 59, 10, 0, time.UTC)

	_, err := try(t, repo, dir, Options{State: first.State, StateOut: unwritable, Now: crashAt, UpdateLatest: true})
	if err == nil || !strings.Contains(err.Error(), "its state was not written") {
		t.Fatalf("err = %v", err)
	}

	interrupted := repo.tags["catalog-20260930.2359"]
	if interrupted == "" || repo.tags[LatestTag] != first.CatalogDigest {
		t.Fatalf("tags after the crash = %v", repo.tags)
	}

	repo.resetCalls()

	out := filepath.Join(t.TempDir(), "state.json")
	res := publish(t, repo, dir, Options{State: first.State, StateOut: out, Now: crashAt.Add(26 * time.Hour), UpdateLatest: true})

	if res.Status != StatusResumed || res.Revision != "20260930.2359" || res.CatalogDigest != interrupted ||
		!slices.Equal(res.Changed, []string{"delta"}) || res.UploadedSchemas != 0 || !slices.Equal(res.Tags.Created, []string{LatestTag}) {
		t.Fatalf("result = %+v", res)
	}

	if repo.count("PushManifest", "PushBlob") != 0 || len(catalogTags(repo)) != 3 {
		t.Errorf("the resumed run pushed or tagged a new revision: calls %v, tags %v", repo.calls, catalogTags(repo))
	}

	written := loadState(t, out)
	if written.Catalog.Revision != "20260930.2359" || written.Catalog.Digest != interrupted {
		t.Errorf("state catalog = %+v", written.Catalog)
	}

	if rec, _ := written.Lookup("delta"); rec.LastChangedRevision != "20260930.2359" {
		t.Errorf("delta record = %+v", rec)
	}
}

// A publication whose catalog-latest step failed has written its state;
// the next run with the old state resumes, the next with the new one is a
// noop that moves catalog-latest.
func TestLatestFailsAfterTheStateWasWritten(t *testing.T) {
	repo := newFakeRepo()
	dir := writeSet(t, baseSchemas())
	out := filepath.Join(t.TempDir(), "state.json")
	repo.failTag[LatestTag] = true

	_, err := try(t, repo, dir, Options{StateOut: out, UpdateLatest: true})
	if !errors.Is(err, errInjected) || fault.KindOf(err) != fault.Registry {
		t.Fatalf("err = %v", err)
	}

	written := loadState(t, out)
	if written.Catalog.Digest != repo.tags["catalog-20260923.0300"] {
		t.Fatalf("state catalog %+v, tags %v", written.Catalog, repo.tags)
	}

	if _, ok := repo.tags[LatestTag]; ok {
		t.Fatal("catalog-latest was set by the failed run")
	}

	res := publish(t, repo, dir, Options{State: written, UpdateLatest: true, Now: week1.Add(time.Hour)})
	if res.Status != StatusNoop || repo.tags[LatestTag] != written.Catalog.Digest {
		t.Errorf("result = %+v", res)
	}
}

// Schema artifacts come before the catalog, so a failure among them leaves
// no catalog index, no catalog tag and no state.
func TestSchemaFailureLeavesNoCatalog(t *testing.T) {
	changed := baseSchemas()
	changed["gamma"] = gammaSchema(" (revised)")
	dir := writeSet(t, changed)

	gamma := packedSchemaOf(t, dir, "gamma")
	manifest := gamma.Manifest.Descriptor.Digest.String()
	payload := payloadOf(t, gamma)

	cases := map[string]func(*fakeRepo){
		"schema blob push":     func(f *fakeRepo) { f.failPush[payload] = true },
		"schema manifest push": func(f *fakeRepo) { f.failPush[manifest] = true },
	}

	for name, inject := range cases {
		t.Run(name, func(t *testing.T) {
			repo := newFakeRepo()
			previous := publish(t, repo, writeSet(t, baseSchemas()), Options{UpdateLatest: true})
			tagsBefore := catalogTags(repo)
			out := filepath.Join(t.TempDir(), "state.json")

			inject(repo)

			_, err := try(t, repo, dir, Options{State: previous.State, StateOut: out, Now: week1.Add(time.Minute), UpdateLatest: true})
			if !errors.Is(err, errInjected) || fault.KindOf(err) != fault.Registry {
				t.Fatalf("err = %v", err)
			}

			if got := catalogTags(repo); !maps.Equal(got, tagsBefore) {
				t.Errorf("catalog tags = %v, want %v", got, tagsBefore)
			}

			if got := catalogManifests(t, repo); !slices.Equal(got, []string{previous.CatalogDigest}) {
				t.Errorf("catalog manifests = %v, want only the previous %s", got, previous.CatalogDigest)
			}

			if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the failed run wrote a state: %v", err)
			}

			res := publish(t, repo, dir, Options{State: previous.State, Now: week1.Add(2 * time.Minute), UpdateLatest: true})
			if res.Status != StatusPublished || res.Revision != "20260923.0302" || repo.tags[LatestTag] != res.CatalogDigest {
				t.Errorf("re-run = %+v", res)
			}
		})
	}
}

func packedSchemaOf(t *testing.T, dir, id string) *artifact.Packed {
	t.Helper()

	set, err := prepare.Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range set.Document.Entries {
		if e.ID != id {
			continue
		}

		var notice []byte
		if e.Notice != "" {
			notice = set.Notices[e.Notice]
		}

		packed, err := artifact.PackSchema(set.Schemas[id], notice)
		if err != nil {
			t.Fatal(err)
		}

		return packed
	}

	t.Fatalf("no schema %s", id)

	return nil
}

func payloadOf(t *testing.T, p *artifact.Packed) string {
	t.Helper()

	for _, b := range p.Blobs {
		if b.Descriptor.MediaType == artifact.SchemaMediaType || b.Descriptor.MediaType == artifact.SchemaGzipMediaType {
			return b.Descriptor.Digest.String()
		}
	}

	t.Fatal("no schema payload")

	return ""
}

// catalogTags returns catalog-<revision> and catalog-latest with their
// targets.
func catalogTags(repo *fakeRepo) map[string]string {
	tags := map[string]string{}

	for tag, target := range repo.tags {
		if strings.HasPrefix(tag, "catalog-") {
			tags[tag] = target
		}
	}

	return tags
}

// catalogManifests returns the digests of every stored catalog index, valid
// or not.
func catalogManifests(t *testing.T, repo *fakeRepo) []string {
	t.Helper()

	var digests []string

	for dgst, data := range repo.manifests {
		var m ocispec.Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}

		if m.ArtifactType == artifact.CatalogArtifactType {
			digests = append(digests, dgst)
		}
	}

	slices.Sort(digests)

	return digests
}

// Content of an older revision that a different catalog followed is a new
// revision; resuming the old one would move catalog-latest backwards.
func TestOlderContentIsANewRevision(t *testing.T) {
	repo := newFakeRepo()
	a := writeSet(t, baseSchemas())

	changed := baseSchemas()
	changed["delta"] = `{"type":"array","items":{"type":"string"}}`
	b := writeSet(t, changed)

	first := publish(t, repo, a, Options{UpdateLatest: true})
	second := publish(t, repo, b, Options{State: first.State, Now: week1.Add(time.Minute), UpdateLatest: true})

	res := publish(t, repo, a, Options{State: second.State, Now: week1.Add(2 * time.Minute), UpdateLatest: true})

	if res.Status != StatusPublished || res.Revision != "20260923.0302" || res.CatalogDigest == first.CatalogDigest ||
		res.UploadedSchemas != 0 || !slices.Equal(res.Changed, []string{"delta"}) || res.ReusedSchemas != 3 {
		t.Fatalf("result = %+v", res)
	}

	if repo.tags[LatestTag] != res.CatalogDigest || repo.tags["catalog-20260923.0300"] != first.CatalogDigest {
		t.Errorf("tags = %v", repo.tags)
	}

	if c := catalogAt(t, repo, LatestTag); c.Revision != "20260923.0302" {
		t.Errorf("catalog-latest points to revision %s", c.Revision)
	}

	// Without a state, the same content resumes the newest revision, which
	// now has it, and still never the first.
	again := publish(t, repo, a, Options{Now: week1.Add(3 * time.Minute)})
	if again.Status != StatusResumed || again.Revision != "20260923.0302" {
		t.Errorf("without a state = %+v", again)
	}
}

func TestSameMinuteCollisionIsRefused(t *testing.T) {
	repo := newFakeRepo()
	first := publish(t, repo, writeSet(t, baseSchemas()), Options{UpdateLatest: true})

	changed := baseSchemas()
	changed["delta"] = `{"type":"array","items":{"type":"string"}}`
	dir := writeSet(t, changed)
	sameMinute := week1.Add(15 * time.Second)

	for name, previous := range map[string]*state.State{"with state": first.State, "without state": nil} {
		t.Run(name, func(t *testing.T) {
			repo.resetCalls()

			out := filepath.Join(t.TempDir(), "state.json")

			_, err := try(t, repo, dir, Options{State: previous, StateOut: out, Now: sameMinute, UpdateLatest: true})
			if fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), "catalog revision 20260923.0300 already exists") ||
				!strings.Contains(err.Error(), "later minute") {
				t.Fatalf("err = %v", err)
			}

			if repo.writes() != 0 {
				t.Errorf("a refused publication wrote: %v", repo.calls)
			}

			if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused publication wrote a state: %v", err)
			}
		})
	}

	earlier := week1.Add(-time.Hour)

	_, err := try(t, repo, dir, Options{State: first.State, Now: earlier})
	if fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), "revision 20260923.0200 of the publication time is older than revision 20260923.0300") {
		t.Errorf("a clock behind the newest revision: %v", err)
	}

	if repo.tags[LatestTag] != first.CatalogDigest {
		t.Error("catalog-latest moved")
	}

	res := publish(t, repo, dir, Options{State: first.State, Now: week1.Add(time.Minute), UpdateLatest: true})
	if res.Status != StatusPublished || res.Revision != "20260923.0301" {
		t.Errorf("the next minute = %+v", res)
	}
}

// Another publication can take the minute between listing the tags and
// tagging the catalog: the tag must not move, the refused catalog must not
// be pushed or retained, and the error is the same collision.
func TestRevisionTakenAfterListing(t *testing.T) {
	repo := newFakeRepo()
	dir := writeSet(t, baseSchemas())

	other := publish(t, newFakeRepo(), dir, Options{})

	stray := []byte(`{"not":"a catalog"}`)
	repo.manifests[digest.FromBytes(stray)] = stray
	repo.tags["catalog-20260923.0300"] = digest.FromBytes(stray)
	repo.hidden["catalog-20260923.0300"] = true

	_, err := try(t, repo, dir, Options{UpdateLatest: true})
	if fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v", err)
	}

	if repo.tags["catalog-20260923.0300"] != digest.FromBytes(stray) {
		t.Error("the colliding tag moved")
	}

	for tag, target := range repo.tags {
		if target == other.CatalogDigest {
			t.Errorf("the refused catalog was tagged %s", tag)
		}
	}

	if _, pushed := repo.manifests[other.CatalogDigest]; pushed {
		t.Error("the refused catalog index was pushed")
	}

	if _, ok := repo.tags[LatestTag]; ok {
		t.Error("catalog-latest was set although publishing failed")
	}
}

func TestTimeIndependentArtifacts(t *testing.T) {
	dir := writeSet(t, baseSchemas())
	a, b := newFakeRepo(), newFakeRepo()

	ra := publish(t, a, dir, Options{})

	rb, err := Run(t.Context(), b, Options{PreparedDir: dir, Repository: "mirror.example:5000/other/set", Now: week1.AddDate(0, 1, 5)})
	if err != nil {
		t.Fatal(err)
	}

	ca, cb := catalogAt(t, a, "catalog-"+ra.Revision), catalogAt(t, b, "catalog-"+rb.Revision)
	if ra.Revision == rb.Revision || !sameContent(ca, cb) {
		t.Fatalf("revisions %s and %s, same content %v", ra.Revision, rb.Revision, sameContent(ca, cb))
	}

	if !slices.EqualFunc(ca.Artifacts(), cb.Artifacts(), func(x, y ocispec.Descriptor) bool { return x.Digest == y.Digest }) || len(ca.Artifacts()) != 4 {
		t.Error("schema manifests depend on the publication time or the repository")
	}
}

// A state can outlive the repository it describes. Artifacts the prepared
// set rebuilds byte for byte are uploaded again; one it cannot rebuild stops
// the run before any catalog is written.
func TestStateArtifactsMissingFromTheRepository(t *testing.T) {
	smaller := baseSchemas()
	delete(smaller, "gamma")

	first := publish(t, newFakeRepo(), writeSet(t, baseSchemas()), Options{})

	changed := baseSchemas()
	changed["beta"] = `{"type":"string","pattern":"^b"}`

	fresh := newFakeRepo()

	res := publish(t, fresh, writeSet(t, changed), Options{State: first.State, Now: week1.Add(time.Hour)})
	if res.Status != StatusPublished || res.UploadedSchemas != 4 || res.ReusedSchemas != 3 {
		t.Errorf("rebuilding reused artifacts = %+v", res)
	}

	checkCatalog(t, fresh, catalogAt(t, fresh, "catalog-"+res.Revision))

	empty := newFakeRepo()

	smaller["beta"] = changed["beta"]

	_, err := try(t, empty, writeSetWith(t, smaller, setOptions{held: map[string]string{"gamma": state.HeldFetchFailed}}),
		Options{State: first.State, Now: week1.Add(time.Hour)})
	if fault.KindOf(err) != fault.Integrity || !strings.Contains(err.Error(), "schema gamma: its published artifact") {
		t.Fatalf("err = %v", err)
	}

	if len(catalogTags(empty)) != 0 || len(catalogManifests(t, empty)) != 0 {
		t.Errorf("the refused run left catalogs behind: %v", empty.tags)
	}
}

func TestPublicationTimeAndInputErrors(t *testing.T) {
	repo := newFakeRepo()

	cases := map[string]struct {
		opts Options
		kind fault.Kind
		msg  string
	}{
		"missing prepared": {Options{PreparedDir: filepath.Join(t.TempDir(), "missing")}, fault.Usage, "load prepared set"},
		"year out of range": {
			Options{Now: time.Date(1999, 12, 31, 23, 59, 0, 0, time.UTC)}, fault.Usage, "publication time 1999-12-31T23:59:00Z",
		},
		"zero time": {Options{Now: time.Time{}.Add(time.Hour)}, fault.Usage, "publication time"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			opts := tc.opts
			if opts.PreparedDir == "" {
				opts.PreparedDir = writeSet(t, map[string]string{"alpha": `{}`})
			}

			opts.Repository = repository

			_, err := Run(t.Context(), repo, opts)
			if fault.KindOf(err) != tc.kind || !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("err = %v (kind %s), want %s with %q", err, fault.KindOf(err), tc.kind, tc.msg)
			}
		})
	}

	if repo.writes() != 0 {
		t.Errorf("refused runs wrote: %v", repo.calls)
	}
}

func TestResultJSON(t *testing.T) {
	res := publish(t, newFakeRepo(), writeSet(t, map[string]string{"alpha": `{}`}), Options{})

	data, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}

	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{
		"status", "revision", "catalogDigest", "catalogSize", "added", "changed", "metadataChanged", "removedUpstream", "held",
		"excluded", "unchanged", "uploadedSchemas", "reusedSchemas", "repository", "keptArtifacts", "tags",
	} {
		if _, ok := generic[key]; !ok {
			t.Errorf("result JSON lacks %s: %s", key, data)
		}
	}

	if _, ok := generic["State"]; ok || strings.Contains(string(data), `"formatVersion"`) {
		t.Errorf("the result JSON embeds the state: %s", data)
	}

	for _, empty := range []string{"metadataChanged", "removedUpstream", "held", "excluded", "existing"} {
		if !strings.Contains(string(data), `"`+empty+`":[]`) {
			t.Errorf("empty list %s must be an array: %s", empty, data)
		}
	}

	if _, ok := generic["changed_ids"]; ok {
		t.Errorf("the result JSON carries the retired changed_ids: %s", data)
	}

	if text := res.String(); !strings.Contains(text, "status: published") || !strings.Contains(text, "revision: 20260923.0300") ||
		!strings.Contains(text, "1 uploaded") {
		t.Errorf("String() = %q", text)
	}
}

// storeForeignIndex tags an empty image index of the given artifact type
// as catalog revision rev, as a publisher of another wire format could
// have left it.
func storeForeignIndex(t *testing.T, repo *fakeRepo, rev, artifactType string) string {
	t.Helper()

	data, err := json.Marshal(ocispec.Index{SchemaVersion: 2, MediaType: artifact.IndexMediaType, ArtifactType: artifactType, Manifests: []ocispec.Descriptor{}})
	if err != nil {
		t.Fatal(err)
	}

	dgst := digestOf(data).String()
	repo.manifests[dgst] = data
	repo.mediaTypes[dgst] = artifact.IndexMediaType
	repo.tags["catalog-"+rev] = dgst

	return dgst
}

// storeForeignCatalog tags a manifest of the given artifact and layer types
// as catalog revision rev, as another publisher could have left it.
func storeForeignCatalog(t *testing.T, repo *fakeRepo, rev, artifactType, layerType string) string {
	t.Helper()

	payload := []byte(`{"formatVersion":2}`)
	layer := ocispec.Descriptor{MediaType: layerType, Digest: digestOf(payload), Size: int64(len(payload))}
	m := ocispec.Manifest{
		MediaType: artifact.ManifestMediaType, ArtifactType: artifactType,
		Config: ocispec.DescriptorEmptyJSON, Layers: []ocispec.Descriptor{layer},

		SchemaVersion: 2,
	}

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	dgst := digestOf(data).String()
	repo.blobs[layer.Digest.String()] = payload
	repo.manifests[dgst] = data
	repo.tags["catalog-"+rev] = dgst

	return dgst
}

// The newest revision decides whether a run resumes. One that holds no
// valid catalog is not resumed but still owns its minute; one in a newer
// wire format stops the run before anything is written, because this build
// cannot tell whether it already has the candidate's content.
func TestNewestRevisionThatIsNotACatalogOfThisBuild(t *testing.T) {
	changed := baseSchemas()
	changed["delta"] = `{"type":"array","minItems":1}`

	t.Run("invalid", func(t *testing.T) {
		repo := newFakeRepo()
		first := publish(t, repo, writeSet(t, baseSchemas()), Options{UpdateLatest: true})
		storeForeignCatalog(t, repo, "20260923.0310", artifact.SchemaArtifactType, artifact.SchemaMediaType)

		_, err := try(t, repo, writeSet(t, changed), Options{State: first.State, Now: week1.Add(10 * time.Minute)})
		if fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), "20260923.0310 already exists") {
			t.Errorf("the minute of an invalid catalog = %v", err)
		}

		res := publish(t, repo, writeSet(t, changed), Options{State: first.State, Now: week1.Add(11 * time.Minute), UpdateLatest: true})
		if res.Status != StatusPublished || res.Revision != "20260923.0311" {
			t.Errorf("result = %+v", res)
		}
	})

	t.Run("newer wire format", func(t *testing.T) {
		repo := newFakeRepo()
		first := publish(t, repo, writeSet(t, baseSchemas()), Options{UpdateLatest: true})
		storeForeignIndex(t, repo, "20260923.0310", "application/vnd.ovineko.schepherd.catalog.v3")

		tagsBefore := maps.Clone(repo.tags)
		repo.resetCalls()

		_, err := try(t, repo, writeSet(t, changed), Options{State: first.State, Now: week1.Add(time.Hour), UpdateLatest: true})
		if fault.KindOf(err) != fault.Usage || !errors.Is(err, artifact.ErrUnsupported) {
			t.Fatalf("err = %v, want an unsupported-format usage error", err)
		}

		if !maps.Equal(repo.tags, tagsBefore) || repo.writes() != 0 {
			t.Errorf("a refused run wrote to the repository: tags %v, calls %v", repo.tags, repo.calls)
		}
	})
}

func TestIndexOverTheManifestLimitIsRefused(t *testing.T) {
	c := &catalog.Catalog{FormatVersion: catalog.FormatVersion, Revision: "20260923.0300"}

	for i := range 3 {
		packed, err := artifact.PackSchema(fmt.Appendf(nil, `{"title":"s%d"}`, i), nil)
		if err != nil {
			t.Fatal(err)
		}

		d := packed.Manifest.Descriptor
		c.Schemas = append(c.Schemas, catalog.Entry{
			ID: fmt.Sprintf("s%d", i), Name: "S",
			Artifact: catalog.Descriptor{MediaType: d.MediaType, Digest: d.Digest.String(), Size: d.Size},
		})
	}

	packed, err := packCatalog(c)
	if err != nil {
		t.Fatal(err)
	}

	size := packed.Index.Descriptor.Size
	if err := checkIndexSize(packed, 3, size); err != nil {
		t.Errorf("an index at the limit: %v", err)
	}

	err = checkIndexSize(packed, 3, size-1)
	if fault.KindOf(err) != fault.Integrity || !strings.Contains(err.Error(), "references 3 schema artifacts") ||
		!strings.Contains(err.Error(), fmt.Sprintf("at most %d bytes", size-1)) {
		t.Errorf("an index over the limit: %v", err)
	}
}
