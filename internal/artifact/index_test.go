package artifact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/digest"
)

func schemaDescriptors(tb testing.TB, schemas ...string) []ocispec.Descriptor {
	tb.Helper()

	descs := make([]ocispec.Descriptor, 0, len(schemas))

	for _, s := range schemas {
		packed, err := PackSchema([]byte(s), nil)
		if err != nil {
			tb.Fatal(err)
		}

		descs = append(descs, packed.Manifest.Descriptor)
	}

	return descs
}

func packTestCatalog(tb testing.TB, schemas []ocispec.Descriptor) *PackedCatalog {
	tb.Helper()

	packed, err := PackCatalog([]byte(`{"formatVersion":2}`), schemas)
	if err != nil {
		tb.Fatal(err)
	}

	return packed
}

func TestPackCatalogIndex(t *testing.T) {
	schemas := schemaDescriptors(t, `{"type":"object"}`, `true`, `{"type":"string"}`)
	packed := packTestCatalog(t, schemas)

	reversed := make([]ocispec.Descriptor, 0, len(schemas)+1)
	reversed = append(reversed, schemas[2], schemas[1], schemas[0])
	if again := packTestCatalog(t, reversed); !bytes.Equal(again.Index.Data, packed.Index.Data) {
		t.Fatal("the index depends on the order of the schema descriptors")
	}

	var ix ocispec.Index
	if err := json.Unmarshal(packed.Index.Data, &ix); err != nil {
		t.Fatal(err)
	}

	if ix.MediaType != IndexMediaType || ix.ArtifactType != CatalogArtifactType || ix.SchemaVersion != 2 || ix.Annotations != nil || ix.Subject != nil {
		t.Fatalf("index envelope = %s", packed.Index.Data)
	}

	if len(ix.Manifests) != 4 {
		t.Fatalf("index has %d children, want the metadata manifest and 3 schemas", len(ix.Manifests))
	}

	metadata := packed.Metadata.Manifest.Descriptor
	if first := ix.Manifests[0]; first.Digest != metadata.Digest || first.Size != metadata.Size ||
		first.MediaType != ManifestMediaType || first.ArtifactType != CatalogMetadataArtifactType {
		t.Fatalf("first child = %+v, want the metadata manifest", first)
	}

	for i, child := range ix.Manifests[1:] {
		if i > 0 && ix.Manifests[i].Digest >= child.Digest {
			t.Fatalf("schema children are not in ascending digest order: %s", packed.Index.Data)
		}

		if child.ArtifactType != "" || child.Platform != nil || child.Annotations != nil {
			t.Fatalf("schema child %d carries more than mediaType, digest and size: %+v", i, child)
		}
	}

	if want := `{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + ix.Manifests[1].Digest.String() +
		`","size":` + strconv.FormatInt(ix.Manifests[1].Size, 10) + `}`; !strings.Contains(string(packed.Index.Data), want) {
		t.Fatalf("index does not encode a schema child as %s:\n%s", want, packed.Index.Data)
	}

	if packed.Index.Descriptor.MediaType != IndexMediaType || packed.Index.Descriptor.Digest.String() != digest.FromBytes(packed.Index.Data) {
		t.Fatalf("index descriptor = %+v", packed.Index.Descriptor)
	}

	ci, err := ParseCatalogIndex(packed.Index.Data, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	if ci.Metadata.Digest != metadata.Digest || len(ci.Schemas) != 3 {
		t.Fatalf("parsed index = %+v", ci)
	}

	if err := ci.CheckSchemas(append(reversed, schemas[0])); err != nil {
		t.Fatalf("CheckSchemas with the same set: %v", err)
	}
}

// TestPackCatalogIndexGolden pins the bytes of an index: the envelope, the
// member order of the descriptors and the child order.
func TestPackCatalogIndexGolden(t *testing.T) {
	packed := packTestCatalog(t, schemaDescriptors(t, `{"type":"object"}`))
	metadata := packed.Metadata.Manifest.Descriptor
	schema := schemaDescriptors(t, `{"type":"object"}`)[0]

	want := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","artifactType":"application/vnd.ovineko.schepherd.catalog.v2",` +
		`"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + metadata.Digest.String() + `","size":` + strconv.FormatInt(metadata.Size, 10) +
		`,"artifactType":"application/vnd.ovineko.schepherd.catalog-metadata.v2"},` +
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + schema.Digest.String() + `","size":` + strconv.FormatInt(schema.Size, 10) + `}]}`

	if got := string(packed.Index.Data); got != want {
		t.Errorf("index =\n%s\nwant\n%s", got, want)
	}

	wantMetadata := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","artifactType":"application/vnd.ovineko.schepherd.catalog-metadata.v2",` +
		`"config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2,"data":"e30="},` +
		`"layers":[{"mediaType":"application/vnd.ovineko.schepherd.catalog.v2+json","digest":"` + digest.FromBytes([]byte(`{"formatVersion":2}`)) +
		`","size":19,"annotations":{"org.opencontainers.image.title":"catalog.json"}}]}`

	if got := string(packed.Metadata.Manifest.Data); got != wantMetadata {
		t.Errorf("metadata manifest =\n%s\nwant\n%s", got, wantMetadata)
	}
}

func TestPackCatalogRefusesDuplicatesAndForeignChildren(t *testing.T) {
	schemas := schemaDescriptors(t, `true`)

	if _, err := PackCatalog([]byte(`{}`), append(schemas, schemas[0])); !errors.Is(err, ErrInvalid) {
		t.Errorf("duplicate schema descriptor: %v", err)
	}

	foreign := schemas[0]
	foreign.MediaType = IndexMediaType

	if _, err := PackCatalog([]byte(`{}`), []ocispec.Descriptor{foreign}); !errors.Is(err, ErrInvalid) {
		t.Errorf("index as a schema child: %v", err)
	}
}

// TestCatalogIndexCapacity checks that an index over as many distinct
// schema artifacts as the default catalog entry limit allows stays within
// the default manifest limit, which is also what registries accept.
func TestCatalogIndexCapacity(t *testing.T) {
	const entries = 20000

	schemas := make([]ocispec.Descriptor, 0, entries)
	for i := range entries {
		schemas = append(schemas, ocispec.Descriptor{
			MediaType: ManifestMediaType,
			Digest:    godigest.Digest(digest.FromBytes(fmt.Appendf(nil, "schema %d", i))),
			Size:      DefaultLimits().MaxManifestBytes,
		})
	}

	packed := packTestCatalog(t, schemas)
	if limit := DefaultLimits().MaxManifestBytes; packed.Index.Descriptor.Size > limit {
		t.Fatalf("an index over %d schemas has %d bytes, more than the default manifest limit %d", entries, packed.Index.Descriptor.Size, limit)
	}

	ci, err := ParseCatalogIndex(packed.Index.Data, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	if len(ci.Schemas) != entries {
		t.Fatalf("parsed %d schema children", len(ci.Schemas))
	}

	limits := DefaultLimits()
	limits.MaxManifestBytes = packed.Index.Descriptor.Size - 1

	if _, err := ParseCatalogIndex(packed.Index.Data, limits); !errors.Is(err, ErrInvalid) {
		t.Errorf("index above the manifest limit: %v", err)
	}
}

func mutateIndex(t *testing.T, data []byte, edit func(m map[string]any)) []byte {
	t.Helper()

	return mutateManifest(t, data, edit)
}

func childAt(m map[string]any, i int) map[string]any {
	return m["manifests"].([]any)[i].(map[string]any)
}

func TestParseCatalogIndexRejects(t *testing.T) {
	packed := packTestCatalog(t, schemaDescriptors(t, `true`, `{"type":"object"}`))
	valid := packed.Index.Data

	cases := map[string]func(m map[string]any){
		"schemaVersion":             func(m map[string]any) { m["schemaVersion"] = 3 },
		"image manifest media type": func(m map[string]any) { m["mediaType"] = ManifestMediaType },
		"no artifactType":           func(m map[string]any) { delete(m, "artifactType") },
		"schema artifactType":       func(m map[string]any) { m["artifactType"] = SchemaArtifactType },
		"older catalog version":     func(m map[string]any) { m["artifactType"] = "application/vnd.ovineko.schepherd.catalog.v1" },
		"subject":                   func(m map[string]any) { m["subject"] = childAt(m, 1) },
		"unknown member":            func(m map[string]any) { m["extra"] = true },
		"no metadata child":         func(m map[string]any) { m["manifests"] = m["manifests"].([]any)[1:] },
		"two metadata children": func(m map[string]any) {
			childAt(m, 1)["artifactType"] = CatalogMetadataArtifactType
		},
		"duplicate schema child": func(m map[string]any) {
			m["manifests"] = append(m["manifests"].([]any), childAt(m, 1))
		},
		"typed schema child": func(m map[string]any) { childAt(m, 1)["artifactType"] = SchemaArtifactType },
		"foreign child type": func(m map[string]any) { childAt(m, 1)["artifactType"] = "application/vnd.example.thing" },
		"platform": func(m map[string]any) {
			childAt(m, 1)["platform"] = map[string]any{"os": "linux", "architecture": "amd64"}
		},
		"child annotations":      func(m map[string]any) { childAt(m, 1)["annotations"] = map[string]any{"a": "b"} },
		"child urls":             func(m map[string]any) { childAt(m, 1)["urls"] = []any{"https://example.com/x"} },
		"child data":             func(m map[string]any) { childAt(m, 1)["data"] = "e30=" },
		"nested index child":     func(m map[string]any) { childAt(m, 1)["mediaType"] = IndexMediaType },
		"metadata as index":      func(m map[string]any) { childAt(m, 0)["mediaType"] = IndexMediaType },
		"child size zero":        func(m map[string]any) { childAt(m, 1)["size"] = 0 },
		"child size over limit":  func(m map[string]any) { childAt(m, 2)["size"] = DefaultLimits().MaxManifestBytes + 1 },
		"child digest algorithm": func(m map[string]any) { childAt(m, 1)["digest"] = "sha512:" + strings.Repeat("a", 128) },
		"child digest case": func(m map[string]any) {
			childAt(m, 1)["digest"] = strings.ToUpper(childAt(m, 1)["digest"].(string))
		},
	}

	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseCatalogIndex(mutateIndex(t, valid, edit), DefaultLimits())
			if !errors.Is(err, ErrInvalid) || errors.Is(err, ErrUnsupported) {
				t.Fatalf("error = %v, want ErrInvalid", err)
			}
		})
	}

	for name, data := range map[string][]byte{
		"duplicate key": bytes.Replace(valid, []byte(`"schemaVersion":2,`), []byte(`"schemaVersion":2,"schemaVersion":2,`), 1),
		"not json":      []byte(`{`),
		"a manifest":    packed.Metadata.Manifest.Data,
	} {
		if _, err := ParseCatalogIndex(data, DefaultLimits()); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: error = %v, want ErrInvalid", name, err)
		}
	}
}

func TestCheckSchemas(t *testing.T) {
	schemas := schemaDescriptors(t, `true`, `{"type":"object"}`, `{"type":"string"}`)
	packed := packTestCatalog(t, schemas[:2])

	ci, err := ParseCatalogIndex(packed.Index.Data, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	resized := schemas[0]
	resized.Size++

	retyped := schemas[0]
	retyped.MediaType = IndexMediaType

	cases := map[string]struct {
		listed []ocispec.Descriptor
		msg    string
	}{
		"schema missing from the index": {listed: schemas, msg: "which the catalog index does not reference"},
		"schema missing from catalog":   {listed: schemas[:1], msg: "which catalog.json does not list"},
		"other size":                    {listed: []ocispec.Descriptor{resized, schemas[1]}, msg: "bytes in catalog.json"},
		"other media type":              {listed: []ocispec.Descriptor{retyped, schemas[1]}, msg: "bytes in catalog.json"},
		"metadata listed as a schema":   {listed: append([]ocispec.Descriptor{packed.Metadata.Manifest.Descriptor}, schemas[:2]...), msg: "does not reference"},
		"empty catalog":                 {listed: nil, msg: "which catalog.json does not list"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := ci.CheckSchemas(tc.listed)
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("error = %v, want ErrInvalid containing %q", err, tc.msg)
			}
		})
	}

	empty := packTestCatalog(t, nil)

	eci, err := ParseCatalogIndex(empty.Index.Data, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	if err := eci.CheckSchemas(nil); err != nil {
		t.Errorf("empty catalog and index: %v", err)
	}
}

func FuzzParseCatalogIndex(f *testing.F) {
	packed, err := PackCatalog([]byte(`{"formatVersion":2}`), schemaDescriptors(f, `true`, `{"type":"object"}`))
	if err != nil {
		f.Fatal(err)
	}

	f.Add(packed.Index.Data)
	f.Add(packed.Metadata.Manifest.Data)
	f.Add(bytes.Replace(packed.Index.Data, []byte(".catalog.v2\""), []byte(".catalog.v3\""), 1))
	f.Add(bytes.Replace(packed.Index.Data, []byte(".catalog-metadata.v2\""), []byte(".catalog-metadata.v3\""), 1))
	f.Add([]byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`))

	limits := DefaultLimits()

	f.Fuzz(func(t *testing.T, data []byte) {
		ci, err := ParseCatalogIndex(data, limits)
		if err != nil {
			if !errors.Is(err, ErrInvalid) && !isUnsupported(err) {
				t.Fatalf("error is neither ErrInvalid nor an unsupported format: %v", err)
			}

			return
		}

		if ci.Metadata.MediaType != ManifestMediaType || ci.Metadata.ArtifactType != CatalogMetadataArtifactType {
			t.Fatalf("accepted metadata child %+v", ci.Metadata)
		}

		seen := map[godigest.Digest]bool{ci.Metadata.Digest: true}

		for _, d := range ci.Schemas {
			if d.MediaType != ManifestMediaType || d.ArtifactType != "" || d.Size <= 0 || d.Size > limits.MaxManifestBytes || seen[d.Digest] {
				t.Fatalf("accepted schema child %+v", d)
			}

			seen[d.Digest] = true
		}

		if err := ci.CheckSchemas(ci.Schemas); err != nil {
			t.Fatalf("an index does not agree with its own schema children: %v", err)
		}
	})
}
