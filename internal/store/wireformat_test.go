package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

// newerManifest rewrites a published manifest as a later wire format would
// type it and adds it to the remote under its new digest.
func newerManifest(t *testing.T, f *fixture, manifestDigest string, edit func(m map[string]any)) (string, []byte) {
	t.Helper()

	var m map[string]any
	if err := json.Unmarshal(f.remote.blobs[manifestDigest], &m); err != nil {
		t.Fatal(err)
	}

	edit(m)

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	dgst := digest.FromBytes(data)
	f.remote.blobs[dgst] = data

	return dgst, data
}

func setArtifactType(value string) func(m map[string]any) {
	return func(m map[string]any) { m["artifactType"] = value }
}

func setPayloadType(value string) func(m map[string]any) {
	return func(m map[string]any) { m["layers"].([]any)[0].(map[string]any)["mediaType"] = value }
}

func setChild(i int, key string, value any) func(m map[string]any) {
	return func(m map[string]any) { m["manifests"].([]any)[i].(map[string]any)[key] = value }
}

func entryFor(id, dgst string, size int) *catalog.Entry {
	return &catalog.Entry{ID: id, Name: id, Artifact: catalog.Descriptor{MediaType: artifact.ManifestMediaType, Digest: dgst, Size: int64(size)}}
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

func TestNewerWireFormatFromRegistryIsUnsupported(t *testing.T) {
	ctx := context.Background()

	t.Run("catalog index artifactType", func(t *testing.T) {
		f := newFixture(t)
		dgst, _ := newerManifest(t, f, f.catalog, setArtifactType("application/vnd.ovineko.schepherd.catalog.v3"))
		dir := t.TempDir()

		s, _ := f.store(t, dir, false)

		_, err := s.Catalog(ctx, dgst)
		assertUnsupported(t, err)

		c, cerr := cache.Open(dir)
		if cerr != nil {
			t.Fatal(cerr)
		}
		defer func() { _ = c.Close() }()

		if _, err := c.ReadBlob(dgst, 1<<20); !errors.Is(err, cache.ErrNotFound) {
			t.Errorf("the unsupported manifest was cached: %v", err)
		}
	})

	t.Run("catalog metadata child", func(t *testing.T) {
		f := newFixture(t)
		dgst, _ := newerManifest(t, f, f.catalog, setChild(0, "artifactType", "application/vnd.ovineko.schepherd.catalog-metadata.v3"))

		s, _ := f.store(t, t.TempDir(), false)

		_, err := s.Catalog(ctx, dgst)
		assertUnsupported(t, err)
	})

	t.Run("catalog payload", func(t *testing.T) {
		f := newFixture(t)
		metadata, data := newerManifest(t, f, f.metadata, setPayloadType("application/vnd.ovineko.schepherd.catalog.v3+json"))
		dgst, _ := newerManifest(t, f, f.catalog, func(m map[string]any) {
			setChild(0, "digest", metadata)(m)
			setChild(0, "size", len(data))(m)
		})

		s, _ := f.store(t, t.TempDir(), false)

		_, err := s.Catalog(ctx, dgst)
		assertUnsupported(t, err)
	})

	for name, edit := range map[string]func(m map[string]any){
		"schema artifactType": setArtifactType("application/vnd.ovineko.schepherd.schema.v3"),
		"schema payload":      setPayloadType("application/vnd.ovineko.schepherd.schema.v3+gzip"),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			dgst, data := newerManifest(t, f, f.entries["gamma"].Artifact.Digest, edit)

			s, _ := f.store(t, t.TempDir(), false)

			_, err := s.Materialize(ctx, entryFor("gamma", dgst, len(data)))
			assertUnsupported(t, err)
		})
	}
}

// A newer client sharing the cache directory stores manifests of its own
// wire format there. An older client must refuse them as unsupported, not
// report them as corrupt, quarantine them or fetch them again.
func TestCachedNewerWireFormatIsUnsupportedNotCorrupt(t *testing.T) {
	ctx := context.Background()

	f := newFixture(t)
	catalogDigest, catalogData := newerManifest(t, f, f.catalog, setArtifactType("application/vnd.ovineko.schepherd.catalog.v3"))
	schemaDigest, schemaData := newerManifest(t, f, f.entries["alpha"].Artifact.Digest, setArtifactType("application/vnd.ovineko.schepherd.schema.v3"))
	entry := entryFor("alpha", schemaDigest, len(schemaData))

	dir := t.TempDir()

	c, err := cache.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	for dgst, data := range map[string][]byte{catalogDigest: catalogData, schemaDigest: schemaData} {
		if err := c.WriteBlob(dgst, int64(len(data)), int64(len(data)), bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}

	_ = c.Close()

	for _, offline := range []bool{true, false} {
		s, opened := f.store(t, dir, offline)

		_, err := s.Catalog(ctx, catalogDigest)
		assertUnsupported(t, err)

		_, err = s.Materialize(ctx, entry)
		assertUnsupported(t, err)

		if *opened != 0 {
			t.Errorf("offline=%v: the registry was opened %d times for content the cache holds", offline, *opened)
		}
	}

	if names := quarantined(t, dir); len(names) != 0 {
		t.Errorf("quarantine holds %v, want nothing", names)
	}

	c, err = cache.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	for dgst, data := range map[string][]byte{catalogDigest: catalogData, schemaDigest: schemaData} {
		if err := c.VerifyBlob(dgst, int64(len(data))); err != nil {
			t.Errorf("cached %s was changed: %v", dgst, err)
		}
	}
}

// A catalog whose index and catalog.json disagree on the schema manifests
// is refused as invalid, both from the registry and from the cache, and is
// never cached or quarantined: fetching it again returns the same bytes.
func TestIndexAndCatalogMustAgree(t *testing.T) {
	ctx := context.Background()

	f := newFixture(t)
	raw := f.remote.blobs[mustPayload(t, f).Digest.String()]
	alpha := f.entries["alpha"].Artifact
	gamma := f.entries["gamma"].Artifact
	other, err := artifact.PackSchema([]byte(`{"type":"string"}`), nil)
	if err != nil {
		t.Fatal(err)
	}

	f.add(other)

	descriptor := func(a catalog.Descriptor) ocispec.Descriptor {
		return ocispec.Descriptor{MediaType: a.MediaType, Digest: godigest.Digest(a.Digest), Size: a.Size}
	}

	resized := descriptor(gamma)
	resized.Size++

	cases := map[string]struct {
		schemas []ocispec.Descriptor
		msg     string
	}{
		"schema missing from the index": {schemas: []ocispec.Descriptor{descriptor(alpha)}, msg: "which the catalog index does not reference"},
		"extra schema in the index":     {schemas: []ocispec.Descriptor{descriptor(alpha), descriptor(gamma), other.Manifest.Descriptor}, msg: "which catalog.json does not list"},
		"other size in the index":       {schemas: []ocispec.Descriptor{descriptor(alpha), resized}, msg: "bytes in catalog.json"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			index, _ := f.addCatalog(t, raw, tc.schemas)

			dir := t.TempDir()
			s, _ := f.store(t, dir, false)

			_, err := s.Catalog(ctx, index)
			if fault.KindOf(err) != fault.Integrity || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("error = %v (kind %s), want an integrity error containing %q", err, fault.KindOf(err), tc.msg)
			}

			c, cerr := cache.Open(dir)
			if cerr != nil {
				t.Fatal(cerr)
			}

			if _, err := c.ReadBlob(index, 1<<20); !errors.Is(err, cache.ErrNotFound) {
				t.Errorf("the inconsistent index was cached: %v", err)
			}

			packed, perr := artifact.PackCatalog(raw, tc.schemas)
			if perr != nil {
				t.Fatal(perr)
			}

			for _, b := range append(packed.Metadata.Blobs, packed.Metadata.Manifest, packed.Index) {
				if err := c.WriteBlob(b.Descriptor.Digest.String(), b.Descriptor.Size, b.Descriptor.Size, bytes.NewReader(b.Data)); err != nil {
					t.Fatal(err)
				}
			}

			_ = c.Close()

			offline, opened := f.store(t, dir, true)
			if _, err := offline.Catalog(ctx, index); fault.KindOf(err) != fault.Integrity || !strings.Contains(err.Error(), tc.msg) || *opened != 0 {
				t.Fatalf("cached: error = %v (kind %s, opened %d), want an integrity error containing %q", err, fault.KindOf(err), *opened, tc.msg)
			}

			if names := quarantined(t, dir); len(names) != 0 {
				t.Errorf("quarantine holds %v, want nothing", names)
			}
		})
	}
}

func mustPayload(t *testing.T, f *fixture) ocispec.Descriptor {
	t.Helper()

	cm, err := artifact.ParseCatalogMetadata(f.remote.blobs[f.metadata], artifact.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	return cm.Payload
}
