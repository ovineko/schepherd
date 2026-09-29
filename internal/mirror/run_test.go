package mirror

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/calver"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/registry"
	"github.com/ovineko/schepherd/internal/store"
)

// snapshot is a catalog published into a fake source registry.
type snapshot struct {
	schemas   map[string][]byte
	manifests map[string]string
	payloads  map[string]string
	notices   map[string]string
	catalog   string
	metadata  string
	revision  string
}

func (s *snapshot) catalogTags() []string {
	return []string{"catalog-" + s.revision}
}

func largeSchema(title string) []byte {
	var b strings.Builder

	b.WriteString(`{"title":"` + title + `","type":"object","properties":{`)

	for i := range 300 {
		if i > 0 {
			b.WriteByte(',')
		}

		b.WriteString(`"p` + strconv.Itoa(i) + `":{"type":"string","description":"repeated text for compression"}`)
	}

	b.WriteString(`}}`)

	return []byte(b.String())
}

func (f *fakeRegistry) addPacked(p *artifact.Packed, tags ...string) {
	for _, b := range p.Blobs {
		f.addBlob(b.Data)
	}

	f.addManifest(artifact.ManifestMediaType, p.Manifest.Data, tags...)
}

// publishCatalog publishes a catalog listing schema manifests (keyed by ID)
// that are already stored in reg, tagged as the publisher does, and returns
// the digests of its index and metadata manifest.
func publishCatalog(t *testing.T, reg *fakeRegistry, revision string, manifests map[string][]byte) (index, metadata string) {
	t.Helper()

	entries := make([]catalog.Entry, 0, len(manifests))

	for _, id := range slices.Sorted(maps.Keys(manifests)) {
		data := manifests[id]
		entries = append(entries, catalog.Entry{
			ID:       id,
			Name:     id + ".json",
			Artifact: catalog.Descriptor{MediaType: artifact.ManifestMediaType, Digest: digest.FromBytes(data), Size: int64(len(data))},
		})
	}

	c := &catalog.Catalog{FormatVersion: catalog.FormatVersion, Revision: revision, Schemas: entries}

	raw, err := catalog.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}

	return publishIndex(t, reg, raw, c.Artifacts(), "catalog-"+revision)
}

// publishIndex publishes raw as the catalog of an index over schemas,
// whether or not they agree.
func publishIndex(t *testing.T, reg *fakeRegistry, raw []byte, schemas []ocispec.Descriptor, tags ...string) (index, metadata string) {
	t.Helper()

	packed, err := artifact.PackCatalog(raw, schemas)
	if err != nil {
		t.Fatal(err)
	}

	reg.addPacked(packed.Metadata)
	reg.addManifest(artifact.IndexMediaType, packed.Index.Data, tags...)

	return packed.Index.Descriptor.Digest.String(), packed.Metadata.Manifest.Descriptor.Digest.String()
}

func publish(t *testing.T, reg *fakeRegistry, revision string, schemas map[string][]byte, notice []byte) *snapshot {
	t.Helper()

	snap := &snapshot{
		schemas:   schemas,
		manifests: map[string]string{},
		payloads:  map[string]string{},
		notices:   map[string]string{},
		revision:  revision,
	}
	manifests := map[string][]byte{}

	for id, schema := range schemas {
		packed, err := artifact.PackSchema(schema, notice)
		if err != nil {
			t.Fatal(err)
		}

		dgst := packed.Manifest.Descriptor.Digest.String()
		reg.addPacked(packed)

		sm, err := artifact.ParseSchemaManifest(packed.Manifest.Data, artifact.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}

		manifests[id] = packed.Manifest.Data
		snap.manifests[id] = dgst
		snap.payloads[id] = sm.Payload.Digest.String()

		if sm.Notice != nil {
			snap.notices[id] = sm.Notice.Digest.String()
		}
	}

	snap.catalog, snap.metadata = publishCatalog(t, reg, revision, manifests)

	return snap
}

func firstSnapshot(t *testing.T, reg *fakeRegistry) *snapshot {
	t.Helper()

	return publish(t, reg, "20260901.0300", map[string][]byte{
		"alpha": []byte(`{"type":"object"}`),
		"beta":  largeSchema("beta"),
		"delta": []byte(`{"type":"array"}`),
	}, []byte("NOTICE: test fixture"))
}

func runOptions() Options {
	return Options{ArtifactLimits: artifact.DefaultLimits(), CatalogLimits: catalog.DefaultLimits(), Concurrency: 4}
}

// resolveAll loads the snapshot's catalog by digest from repo into an empty
// cache and materializes every schema, as a client pointed at repo does.
func resolveAll(t *testing.T, repo *registry.Repo, snap *snapshot) {
	t.Helper()

	c, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = c.Close() })

	s := store.New(store.Options{
		Cache:          c,
		Repository:     repo.Name().String(),
		ArtifactLimits: artifact.DefaultLimits(),
		CatalogLimits:  catalog.DefaultLimits(),
		Open:           func() (store.Remote, error) { return repo, nil },
	})

	loaded, err := s.Catalog(t.Context(), snap.catalog)
	if err != nil {
		t.Fatalf("catalog %s (%s) does not resolve: %v", snap.catalog, snap.revision, err)
	}

	if loaded.Catalog.Revision != snap.revision || len(loaded.Catalog.Schemas) != len(snap.schemas) {
		t.Fatalf("catalog %s is revision %s with %d schemas", snap.catalog, loaded.Catalog.Revision, len(loaded.Catalog.Schemas))
	}

	for i := range loaded.Catalog.Schemas {
		entry := &loaded.Catalog.Schemas[i]

		sch, err := s.Materialize(t.Context(), entry)
		if err != nil {
			t.Fatalf("schema %s of %s does not resolve: %v", entry.ID, snap.revision, err)
		}

		got, err := os.ReadFile(sch.Path)
		if err != nil || !bytes.Equal(got, snap.schemas[entry.ID]) {
			t.Fatalf("schema %s of %s resolved to other content: %v", entry.ID, snap.revision, err)
		}
	}
}

func assertNoCatalogTag(t *testing.T, dst *fakeRegistry, snap *snapshot) {
	t.Helper()

	tags := dst.tagMap()
	for _, tag := range snap.catalogTags() {
		if d, ok := tags[tag]; ok {
			t.Errorf("destination tag %s points to %s after a failed mirror", tag, d)
		}
	}

	if dst.hasManifest(snap.catalog) {
		t.Error("the catalog index reached the destination although a schema failed")
	}
}

func assertMirrored(t *testing.T, dst *fakeRegistry, repo *registry.Repo, snap *snapshot) {
	t.Helper()

	tags := dst.tagMap()
	for _, tag := range snap.catalogTags() {
		if tags[tag] != snap.catalog {
			t.Errorf("destination tag %s = %q, want %s", tag, tags[tag], snap.catalog)
		}
	}

	for tag := range tags {
		if _, err := calver.ParseRevisionTag(tag); err != nil {
			t.Errorf("destination tag %s is not a catalog revision tag", tag)
		}
	}

	dst.assertContentIntact(t)
	resolveAll(t, repo, snap)
}

func TestRunMirrorsSnapshot(t *testing.T) {
	src, dst := newFakeRegistry(), newFakeRegistry()
	snap := firstSnapshot(t, src)
	repos := openRepos(t, src, dst)

	ctx, cancel := withTimeout(t)
	defer cancel()

	res, err := Run(ctx, repos[0], repos[1], snap.catalog, runOptions())
	if err != nil {
		t.Fatal(err)
	}

	// The index, the metadata manifest and three schema manifests; the empty
	// config, catalog.json, three payloads and the notice all three share.
	want := Result{
		Source: repos[0].Name().String(), Destination: repos[1].Name().String(), CatalogDigest: snap.catalog, Revision: snap.revision,
		Schemas: 3, CopiedManifests: 5, CopiedBlobs: 6, TagsCreated: 1,
	}
	if *res != want {
		t.Errorf("result = %+v, want %+v", *res, want)
	}

	assertMirrored(t, dst, repos[1], snap)

	again, err := Run(ctx, repos[0], repos[1], snap.catalog, runOptions())
	if err != nil {
		t.Fatal(err)
	}

	want.CopiedManifests, want.CopiedBlobs, want.TagsCreated = 0, 0, 0
	want.SkippedManifests, want.TagsExisting = 1, 1

	if *again != want {
		t.Errorf("repeated mirror = %+v, want %+v", *again, want)
	}
}

// TestRunRefusesInconsistentCatalog mirrors catalogs whose index and
// catalog.json disagree: nothing may reach the destination.
func TestRunRefusesInconsistentCatalog(t *testing.T) {
	src, dst := newFakeRegistry(), newFakeRegistry()
	snap := firstSnapshot(t, src)

	src.mu.Lock()
	metadata := src.manifests[snap.metadata].data
	src.mu.Unlock()

	cm, err := artifact.ParseCatalogMetadata(metadata, artifact.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	src.mu.Lock()
	raw := src.blobs[cm.Payload.Digest.String()]
	src.mu.Unlock()

	c, err := catalog.Parse(raw, catalog.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	index, _ := publishIndex(t, src, raw, c.Artifacts()[1:])
	repos := openRepos(t, src, dst)

	_, err = Run(t.Context(), repos[0], repos[1], index, runOptions())
	if fault.KindOf(err) != fault.Integrity || !strings.Contains(err.Error(), "does not reference") {
		t.Fatalf("mirror of an inconsistent catalog = %v (kind %s), want an integrity error", err, fault.KindOf(err))
	}

	if tags := dst.tagMap(); len(tags) != 0 || dst.hasManifest(index) {
		t.Errorf("the destination received %v and index %v", tags, dst.hasManifest(index))
	}
}

// TestRunRefusesSourceContentThatDoesNotMatch mirrors into a destination
// that stores whatever it receives, so only the client's own verification
// stands between a bad source body and a corrupt mirror.
func TestRunRefusesSourceContentThatDoesNotMatch(t *testing.T) {
	cases := []struct {
		cause  error
		target func(s *snapshot) string
		name   string
		fault  blobFault
	}{
		{name: "flipped payload", fault: serveFlipped, cause: content.ErrMismatchedDigest, target: func(s *snapshot) string { return s.payloads["beta"] }},
		{name: "truncated payload", fault: serveTruncated, cause: content.ErrInvalidDescriptorSize, target: func(s *snapshot) string { return s.payloads["beta"] }},
		{name: "overlong payload", fault: serveTrailing, cause: content.ErrTrailingData, target: func(s *snapshot) string { return s.payloads["alpha"] }},
		{name: "flipped notice", fault: serveFlipped, cause: content.ErrMismatchedDigest, target: func(s *snapshot) string { return s.notices["delta"] }},
		{name: "truncated notice", fault: serveTruncated, cause: content.ErrInvalidDescriptorSize, target: func(s *snapshot) string { return s.notices["delta"] }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, dst := newFakeRegistry(), newFakeRegistry()
			dst.lenient = true
			snap := firstSnapshot(t, src)
			target := tc.target(snap)
			src.setFault(target, tc.fault)
			repos := openRepos(t, src, dst)

			ctx, cancel := withTimeout(t)
			defer cancel()

			_, err := Run(ctx, repos[0], repos[1], snap.catalog, runOptions())
			if fault.KindOf(err) != fault.Integrity || !errors.Is(err, tc.cause) {
				t.Fatalf("mirror of a bad source body = %v (kind %s), want an integrity error caused by %v", err, fault.KindOf(err), tc.cause)
			}

			if !strings.Contains(err.Error(), target) {
				t.Errorf("error does not name the bad content %s: %v", target, err)
			}

			assertNoCatalogTag(t, dst, snap)
			dst.assertContentIntact(t)

			if dst.hasBlob(target) {
				t.Errorf("the destination holds %s after its bad body was refused", target)
			}

			src.setFault(target, serveIntact)

			if _, err := Run(ctx, repos[0], repos[1], snap.catalog, runOptions()); err != nil {
				t.Fatalf("mirror after the source recovered: %v", err)
			}

			assertMirrored(t, dst, repos[1], snap)
		})
	}
}

func TestRunKeepsOlderSnapshotResolvable(t *testing.T) {
	src, dst := newFakeRegistry(), newFakeRegistry()
	first := firstSnapshot(t, src)
	second := publish(t, src, "20260908.0300", map[string][]byte{
		"alpha":   []byte(`{"type":"object","required":["name"]}`),
		"beta":    largeSchema("beta"),
		"epsilon": largeSchema("epsilon"),
	}, []byte("NOTICE: test fixture"))

	if first.catalog == second.catalog || first.manifests["alpha"] == second.manifests["alpha"] || first.manifests["beta"] != second.manifests["beta"] {
		t.Fatal("the snapshots must differ in alpha and share beta")
	}

	repos := openRepos(t, src, dst)

	ctx, cancel := withTimeout(t)
	defer cancel()

	if _, err := Run(ctx, repos[0], repos[1], first.catalog, runOptions()); err != nil {
		t.Fatal(err)
	}

	before := dst.tagMap()

	res, err := Run(ctx, repos[0], repos[1], second.catalog, runOptions())
	if err != nil {
		t.Fatal(err)
	}

	if res.TagsExisting != 0 || res.TagsCreated != 1 || res.CopiedManifests != 4 {
		t.Errorf("second mirror = %+v; want only its revision tag and the index, metadata, alpha and epsilon manifests copied", res)
	}

	after := dst.tagMap()
	for tag, dgst := range before {
		if after[tag] != dgst {
			t.Errorf("tag %s moved from %s to %q", tag, dgst, after[tag])
		}
	}

	assertMirrored(t, dst, repos[1], first)
	assertMirrored(t, dst, repos[1], second)
}

func TestRunRefusesNewerWireFormat(t *testing.T) {
	newer := func(t *testing.T, data []byte, edit func(m map[string]any)) []byte {
		t.Helper()

		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}

		edit(m)

		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}

		return out
	}

	t.Run("catalog", func(t *testing.T) {
		src, dst := newFakeRegistry(), newFakeRegistry()
		snap := firstSnapshot(t, src)

		src.mu.Lock()
		data := src.manifests[snap.catalog].data
		src.mu.Unlock()

		v3 := newer(t, data, func(m map[string]any) { m["artifactType"] = "application/vnd.ovineko.schepherd.catalog.v3" })
		src.addManifest(artifact.IndexMediaType, v3)

		repos := openRepos(t, src, dst)

		_, err := Run(t.Context(), repos[0], repos[1], digest.FromBytes(v3), runOptions())
		assertUnsupported(t, err)

		if tags := dst.tagMap(); len(tags) != 0 {
			t.Errorf("destination got tags %v", tags)
		}
	})

	for name, edit := range map[string]func(m map[string]any){
		"schema artifactType": func(m map[string]any) { m["artifactType"] = "application/vnd.ovineko.schepherd.schema.v3" },
		"schema payload": func(m map[string]any) {
			m["layers"].([]any)[0].(map[string]any)["mediaType"] = "application/vnd.ovineko.schepherd.schema.v3+gzip"
		},
	} {
		t.Run(name, func(t *testing.T) {
			src, dst := newFakeRegistry(), newFakeRegistry()

			packed, err := artifact.PackSchema(largeSchema("future"), nil)
			if err != nil {
				t.Fatal(err)
			}

			src.addPacked(packed)
			v3 := newer(t, packed.Manifest.Data, edit)
			src.addManifest(artifact.ManifestMediaType, v3)

			current, err := artifact.PackSchema([]byte(`{"type":"object"}`), nil)
			if err != nil {
				t.Fatal(err)
			}

			src.addPacked(current)

			snap := &snapshot{revision: "20260915.0300"}
			snap.catalog, snap.metadata = publishCatalog(t, src, snap.revision, map[string][]byte{"current": current.Manifest.Data, "future": v3})

			repos := openRepos(t, src, dst)

			_, err = Run(t.Context(), repos[0], repos[1], snap.catalog, runOptions())
			assertUnsupported(t, err)
			assertNoCatalogTag(t, dst, snap)

			if dst.hasManifest(digest.FromBytes(v3)) {
				t.Error("the unsupported schema manifest was copied")
			}
		})
	}
}

func assertUnsupported(t *testing.T, err error) {
	t.Helper()

	if fault.KindOf(err) != fault.Usage || !errors.Is(err, artifact.ErrUnsupported) {
		t.Fatalf("error = %v (kind %s), want an unsupported format with kind usage", err, fault.KindOf(err))
	}

	if !strings.Contains(err.Error(), "client is too old") {
		t.Errorf("message does not say the client is too old: %v", err)
	}
}

func TestRunCanceledMidTransferResumes(t *testing.T) {
	emptyConfig := ocispec.DescriptorEmptyJSON.Digest.String()

	cases := []struct {
		target func(s *snapshot) (kind, ref string)
		name   string
	}{
		{name: "payload upload", target: func(s *snapshot) (string, string) { return "blob", s.payloads["beta"] }},
		{name: "shared config upload", target: func(*snapshot) (string, string) { return "blob", emptyConfig }},
		{name: "schema manifest push", target: func(s *snapshot) (string, string) { return "manifest", s.manifests["alpha"] }},
		{name: "catalog metadata push", target: func(s *snapshot) (string, string) { return "manifest", s.metadata }},
		{name: "catalog index push", target: func(s *snapshot) (string, string) { return "manifest", s.catalog }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, dst := newFakeRegistry(), newFakeRegistry()
			snap := firstSnapshot(t, src)
			repos := openRepos(t, src, dst)
			kind, ref := tc.target(snap)

			ctx, cancel := withTimeout(t)
			defer cancel()

			interrupted := make(chan struct{})

			dst.setOnPut(func(k, r string) bool {
				if k != kind || r != ref {
					return false
				}

				select {
				case <-interrupted:
				default:
					close(interrupted)
					cancel()
				}

				return true
			})

			_, err := Run(ctx, repos[0], repos[1], snap.catalog, runOptions())

			select {
			case <-interrupted:
			default:
				t.Fatalf("the mirror never uploaded %s %s: %v", kind, ref, err)
			}

			if fault.KindOf(err) != fault.Canceled || !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted mirror = %v (kind %s), want a canceled error", err, fault.KindOf(err))
			}

			assertNoCatalogTag(t, dst, snap)
			dst.assertContentIntact(t)

			dst.setOnPut(nil)

			resumed, cancelResumed := withTimeout(t)
			defer cancelResumed()

			if _, err := Run(resumed, repos[0], repos[1], snap.catalog, runOptions()); err != nil {
				t.Fatalf("resumed mirror: %v", err)
			}

			assertMirrored(t, dst, repos[1], snap)
		})
	}
}
