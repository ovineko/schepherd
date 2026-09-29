package artifact

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/fault"
)

// isUnsupported reports an error that refuses another wire format version:
// exit code 2, and never mistaken for a corrupt artifact.
func isUnsupported(err error) bool {
	return errors.Is(err, ErrUnsupported) && !errors.Is(err, ErrInvalid) && fault.KindOf(err) == fault.Usage
}

func mutateManifest(t *testing.T, data []byte, edit func(m map[string]any)) []byte {
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

func layerAt(m map[string]any, i int) map[string]any {
	return m["layers"].([]any)[i].(map[string]any)
}

func TestNewerWireFormatIsUnsupported(t *testing.T) {
	schema, err := PackSchema(bigSchema(), []byte("NOTICE"))
	if err != nil {
		t.Fatal(err)
	}

	catalog, err := PackCatalog([]byte(`{"formatVersion":2}`), []ocispec.Descriptor{schema.Manifest.Descriptor})
	if err != nil {
		t.Fatal(err)
	}

	set := func(key, value string) func(m map[string]any) {
		return func(m map[string]any) { m[key] = value }
	}
	setLayer := func(i int, value string) func(m map[string]any) {
		return func(m map[string]any) { layerAt(m, i)["mediaType"] = value }
	}
	setChild := func(i int, value string) func(m map[string]any) {
		return func(m map[string]any) { m["manifests"].([]any)[i].(map[string]any)["artifactType"] = value }
	}

	const (
		asSchema = iota
		asMetadata
		asIndex
	)

	cases := []struct {
		edit        func(m map[string]any)
		name        string
		mediaType   string
		kind        int
		unsupported bool
	}{
		{name: "schema artifactType v3", edit: set("artifactType", "application/vnd.ovineko.schepherd.schema.v3"), unsupported: true, mediaType: "schema.v3"},
		{name: "schema artifactType v10", edit: set("artifactType", "application/vnd.ovineko.schepherd.schema.v10"), unsupported: true, mediaType: "schema.v10"},
		{name: "gzip payload v3", edit: setLayer(0, "application/vnd.ovineko.schepherd.schema.v3+gzip"), unsupported: true, mediaType: "schema.v3+gzip"},
		{name: "payload v3 with a new suffix", edit: setLayer(0, "application/vnd.ovineko.schepherd.schema.v3+zstd"), unsupported: true, mediaType: "schema.v3+zstd"},
		{name: "notice v3", edit: setLayer(1, "application/vnd.ovineko.schepherd.notice.v3+text"), unsupported: true, mediaType: "notice.v3+text"},
		{
			name: "v3 artifact with a v3 subject and config",
			edit: func(m map[string]any) {
				m["artifactType"] = "application/vnd.ovineko.schepherd.schema.v3"
				m["subject"] = m["config"]
				m["config"].(map[string]any)["size"] = 3
			},
			unsupported: true, mediaType: "schema.v3",
		},
		{name: "metadata artifactType v3", kind: asMetadata, edit: set("artifactType", "application/vnd.ovineko.schepherd.catalog-metadata.v3"), unsupported: true, mediaType: "catalog-metadata.v3"},
		{name: "catalog payload v3", kind: asMetadata, edit: setLayer(0, "application/vnd.ovineko.schepherd.catalog.v3+json"), unsupported: true, mediaType: "catalog.v3+json"},
		{name: "index artifactType v3", kind: asIndex, edit: set("artifactType", "application/vnd.ovineko.schepherd.catalog.v3"), unsupported: true, mediaType: "catalog.v3"},
		{name: "index metadata child v3", kind: asIndex, edit: setChild(0, "application/vnd.ovineko.schepherd.catalog-metadata.v3"), unsupported: true, mediaType: "catalog-metadata.v3"},
		{name: "index schema child typed v3", kind: asIndex, edit: setChild(1, "application/vnd.ovineko.schepherd.schema.v3"), unsupported: true, mediaType: "schema.v3"},

		{name: "catalog index artifactType where a schema is expected", edit: set("artifactType", "application/vnd.ovineko.schepherd.catalog.v3")},
		{name: "catalog artifactType where catalog metadata is expected", kind: asMetadata, edit: set("artifactType", "application/vnd.ovineko.schepherd.catalog.v3")},
		{name: "schema artifactType where a catalog is expected", kind: asMetadata, edit: set("artifactType", "application/vnd.ovineko.schepherd.schema.v3")},
		{name: "metadata artifactType where an index is expected", kind: asIndex, edit: set("artifactType", "application/vnd.ovineko.schepherd.catalog-metadata.v3")},
		{name: "older schema version", edit: set("artifactType", "application/vnd.ovineko.schepherd.schema.v1")},
		{name: "older payload version", edit: setLayer(0, "application/vnd.ovineko.schepherd.schema.v1+gzip")},
		{name: "older catalog payload", kind: asMetadata, edit: setLayer(0, "application/vnd.ovineko.schepherd.catalog.v1+json")},
		{name: "older index", kind: asIndex, edit: set("artifactType", "application/vnd.ovineko.schepherd.catalog.v1")},
		{name: "version zero", edit: set("artifactType", "application/vnd.ovineko.schepherd.schema.v0")},
		{name: "version with a leading zero", edit: set("artifactType", "application/vnd.ovineko.schepherd.schema.v03")},
		{name: "unknown kind", edit: set("artifactType", "application/vnd.ovineko.schepherd.bundle.v3")},
		{name: "letter case", edit: set("artifactType", "application/vnd.ovineko.schepherd.Schema.v3")},
		{name: "foreign artifactType", edit: set("artifactType", "application/vnd.example.schema.v3")},
		{name: "foreign layer", edit: setLayer(0, "application/vnd.oci.image.layer.v1.tar")},
		{name: "unknown v2 payload suffix", edit: setLayer(0, "application/vnd.ovineko.schepherd.schema.v2+zstd")},
		{name: "foreign catalog payload", kind: asMetadata, edit: setLayer(0, "application/vnd.example.catalog.v3+json")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error

			switch tc.kind {
			case asMetadata:
				_, err = ParseCatalogMetadata(mutateManifest(t, catalog.Metadata.Manifest.Data, tc.edit), DefaultLimits())
			case asIndex:
				_, err = ParseCatalogIndex(mutateManifest(t, catalog.Index.Data, tc.edit), DefaultLimits())
			default:
				_, err = ParseSchemaManifest(mutateManifest(t, schema.Manifest.Data, tc.edit), DefaultLimits())
			}

			if !tc.unsupported {
				if !errors.Is(err, ErrInvalid) || errors.Is(err, ErrUnsupported) {
					t.Fatalf("error = %v, want ErrInvalid only", err)
				}

				if _, classified := errors.AsType[*fault.Error](err); classified {
					t.Fatalf("an invalid artifact is classified by the caller, got %v", err)
				}

				return
			}

			if !isUnsupported(err) {
				t.Fatalf("error = %v (kind %s), want an unsupported format with kind usage", err, fault.KindOf(err))
			}

			for _, fragment := range []string{tc.mediaType, "client is too old", "upgrade schepherd"} {
				if !strings.Contains(err.Error(), fragment) {
					t.Errorf("message %q does not contain %q", err, fragment)
				}
			}

			if got := fault.KindOf(fault.Wrap(fault.Integrity, err, "schema %q", "x")); got != fault.Usage {
				t.Errorf("a caller's integrity classification turned the refusal into %s", got)
			}
		})
	}
}
