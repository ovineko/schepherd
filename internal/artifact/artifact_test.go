package artifact

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/digest"
)

func bigSchema() []byte {
	var b strings.Builder

	b.WriteString(`{"$schema":"http://json-schema.org/draft-07/schema#","properties":{`)

	for i := range 200 {
		if i > 0 {
			b.WriteByte(',')
		}

		b.WriteString(`"field` + strconv.Itoa(i) + `":{"type":"string","description":"a repeated description"}`)
	}

	b.WriteString(`}}`)

	return []byte(b.String())
}

func TestPackSchemaIsDeterministic(t *testing.T) {
	for _, schema := range [][]byte{[]byte(`{"type":"object"}`), []byte(`true`), bigSchema()} {
		first, err := PackSchema(schema, nil)
		if err != nil {
			t.Fatal(err)
		}

		second, err := PackSchema(append([]byte(nil), schema...), nil)
		if err != nil {
			t.Fatal(err)
		}

		if first.Manifest.Descriptor.Digest != second.Manifest.Descriptor.Digest || !bytes.Equal(first.Manifest.Data, second.Manifest.Data) {
			t.Fatalf("packing %d bytes twice produced different manifests", len(schema))
		}
	}
}

func TestPackSchemaGolden(t *testing.T) {
	packed, err := PackSchema([]byte(`{"type":"object"}`), nil)
	if err != nil {
		t.Fatal(err)
	}

	wantManifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","artifactType":"application/vnd.ovineko.schepherd.schema.v2","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2,"data":"e30="},"layers":[{"mediaType":"application/schema+json","digest":"sha256:fe7ec9d5d6ac8a1cbf6d7d7b3a1fc1cdd0e1e1b0e4bd8d1c4e7e5b6b40b0a4c6","size":17,"annotations":{"com.ovineko.schepherd.content.digest":"sha256:fe7ec9d5d6ac8a1cbf6d7d7b3a1fc1cdd0e1e1b0e4bd8d1c4e7e5b6b40b0a4c6","com.ovineko.schepherd.content.size":"17","org.opencontainers.image.title":"schema.json"}}]}`
	wantDigest := digest.FromBytes([]byte(`{"type":"object"}`))
	wantManifest = strings.ReplaceAll(wantManifest, "sha256:fe7ec9d5d6ac8a1cbf6d7d7b3a1fc1cdd0e1e1b0e4bd8d1c4e7e5b6b40b0a4c6", wantDigest)

	if got := string(packed.Manifest.Data); got != wantManifest {
		t.Errorf("manifest =\n%s\nwant\n%s", got, wantManifest)
	}

	if packed.ContentDigest != wantDigest || packed.ContentSize != 17 {
		t.Errorf("content = %s/%d", packed.ContentDigest, packed.ContentSize)
	}
}

func TestGzipDecisionAndRoundTrip(t *testing.T) {
	schema := bigSchema()

	packed, err := PackSchema(schema, []byte("NOTICE text"))
	if err != nil {
		t.Fatal(err)
	}

	sm, err := ParseSchemaManifest(packed.Manifest.Data, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	if sm.Payload.MediaType != SchemaGzipMediaType {
		t.Fatalf("payload media type = %s, want gzip for %d bytes", sm.Payload.MediaType, len(schema))
	}

	if sm.Notice == nil || sm.Notice.MediaType != NoticeMediaType {
		t.Fatalf("notice layer missing: %+v", sm.Notice)
	}

	payload := findBlob(t, packed, sm.Payload.Digest.String())

	if payload[0] != 0x1f || payload[1] != 0x8b || !bytes.Equal(payload[4:8], []byte{0, 0, 0, 0}) {
		t.Errorf("gzip header must have zero mtime: % x", payload[:10])
	}

	var out bytes.Buffer
	if err := DecodeSchema(&out, bytes.NewReader(payload), sm); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(out.Bytes(), schema) {
		t.Error("decoded schema differs from input")
	}
}

func TestSmallSchemaStaysUncompressed(t *testing.T) {
	packed, err := PackSchema([]byte(`{"type":"string"}`), nil)
	if err != nil {
		t.Fatal(err)
	}

	sm, err := ParseSchemaManifest(packed.Manifest.Data, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	if sm.Payload.MediaType != SchemaMediaType || sm.ContentDigest != sm.Payload.Digest.String() {
		t.Errorf("small schema payload = %+v", sm.Payload)
	}
}

func TestDecodeRejectsGzipBomb(t *testing.T) {
	huge := bytes.Repeat([]byte(" "), 10<<20)

	bomb, err := Gzip(huge)
	if err != nil {
		t.Fatal(err)
	}

	sm := &SchemaManifest{
		Payload:       ocispec.Descriptor{MediaType: SchemaGzipMediaType, Size: int64(len(bomb))},
		ContentDigest: digest.FromBytes([]byte("{}")),
		ContentSize:   2,
	}

	var out bytes.Buffer

	err = DecodeSchema(&out, bytes.NewReader(bomb), sm)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("DecodeSchema(bomb) = %v, want ErrInvalid", err)
	}

	if out.Len() > int(sm.ContentSize)+1 {
		t.Errorf("DecodeSchema(bomb) decompressed %d bytes, want at most the declared %d plus one", out.Len(), sm.ContentSize)
	}
}

func TestDecodeRejectsTrailingGzipMember(t *testing.T) {
	content := []byte(`{"a":1}`)
	first, _ := Gzip(content)
	second, _ := Gzip([]byte("junk"))

	sm := &SchemaManifest{
		Payload:       ocispec.Descriptor{MediaType: SchemaGzipMediaType},
		ContentDigest: digest.FromBytes(content),
		ContentSize:   int64(len(content)),
	}

	if err := DecodeSchema(&bytes.Buffer{}, bytes.NewReader(append(first, second...)), sm); !errors.Is(err, ErrInvalid) {
		t.Fatalf("trailing member accepted: %v", err)
	}

	if err := DecodeSchema(&bytes.Buffer{}, bytes.NewReader(first), sm); err != nil {
		t.Fatalf("clean stream rejected: %v", err)
	}
}

func TestDecodeRejectsWrongContentDigest(t *testing.T) {
	content := []byte(`{"a":1}`)
	sm := &SchemaManifest{
		Payload:       ocispec.Descriptor{MediaType: SchemaMediaType},
		ContentDigest: digest.FromBytes([]byte(`{"a":2}`)),
		ContentSize:   int64(len(content)),
	}

	if err := DecodeSchema(&bytes.Buffer{}, bytes.NewReader(content), sm); !errors.Is(err, ErrInvalid) {
		t.Fatalf("digest mismatch accepted: %v", err)
	}
}

func TestParseSchemaManifestRejects(t *testing.T) {
	valid, err := PackSchema([]byte(`{"type":"object"}`), nil)
	if err != nil {
		t.Fatal(err)
	}

	mutate := func(f func(m map[string]any)) []byte {
		var m map[string]any
		if err := json.Unmarshal(valid.Manifest.Data, &m); err != nil {
			t.Fatal(err)
		}

		f(m)

		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}

		return out
	}

	layer := func(m map[string]any) map[string]any {
		return m["layers"].([]any)[0].(map[string]any)
	}

	cases := map[string][]byte{
		"catalog artifact type": mutate(func(m map[string]any) { m["artifactType"] = CatalogArtifactType }),
		"wrong layer type":      mutate(func(m map[string]any) { layer(m)["mediaType"] = "application/vnd.oci.image.layer.v1.tar" }),
		"two payloads": mutate(func(m map[string]any) {
			m["layers"] = append(m["layers"].([]any), m["layers"].([]any)[0])
		}),
		"no layers":           mutate(func(m map[string]any) { m["layers"] = []any{} }),
		"config not empty":    mutate(func(m map[string]any) { m["config"].(map[string]any)["size"] = 3 }),
		"external urls":       mutate(func(m map[string]any) { layer(m)["urls"] = []any{"https://example.invalid/x"} }),
		"unknown field":       mutate(func(m map[string]any) { m["extra"] = true }),
		"subject":             mutate(func(m map[string]any) { m["subject"] = m["config"] }),
		"bad content size":    mutate(func(m map[string]any) { layer(m)["annotations"].(map[string]any)[AnnotationContentSize] = "017" }),
		"size mismatch":       mutate(func(m map[string]any) { layer(m)["annotations"].(map[string]any)[AnnotationContentSize] = "18" }),
		"missing digest":      mutate(func(m map[string]any) { delete(layer(m)["annotations"].(map[string]any), AnnotationContentDigest) }),
		"schemaVersion":       mutate(func(m map[string]any) { m["schemaVersion"] = 1 }),
		"duplicate key":       []byte(`{"schemaVersion":2,"schemaVersion":2}`),
		"negative layer size": mutate(func(m map[string]any) { layer(m)["size"] = -1 }),
	}

	for name, data := range cases {
		if _, err := ParseSchemaManifest(data, DefaultLimits()); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: ParseSchemaManifest = %v, want ErrInvalid", name, err)
		}
	}

	limits := DefaultLimits()
	limits.MaxManifestBytes = 10

	if _, err := ParseSchemaManifest(valid.Manifest.Data, limits); !errors.Is(err, ErrInvalid) {
		t.Errorf("oversized manifest accepted: %v", err)
	}
}

func TestParseCatalogMetadata(t *testing.T) {
	packed, err := PackCatalog([]byte(`{"formatVersion":2}`), nil)
	if err != nil {
		t.Fatal(err)
	}

	metadata := packed.Metadata.Manifest.Data

	cm, err := ParseCatalogMetadata(metadata, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	if cm.Payload.MediaType != CatalogMediaType {
		t.Errorf("payload = %+v", cm.Payload)
	}

	if _, err := ParseSchemaManifest(metadata, DefaultLimits()); !errors.Is(err, ErrInvalid) {
		t.Error("catalog metadata manifest accepted as schema manifest")
	}

	limits := DefaultLimits()
	limits.MaxCatalogBytes = 5

	if _, err := ParseCatalogMetadata(metadata, limits); !errors.Is(err, ErrInvalid) {
		t.Error("oversized catalog accepted")
	}
}

func FuzzDecodeSchema(f *testing.F) {
	gz, _ := Gzip([]byte(`{"a":1}`))
	f.Add(gz)
	f.Add([]byte(`{"a":1}`))

	f.Fuzz(func(t *testing.T, payload []byte) {
		sm := &SchemaManifest{
			Payload:       ocispec.Descriptor{MediaType: SchemaGzipMediaType},
			ContentDigest: digest.FromBytes([]byte(`{"a":1}`)),
			ContentSize:   7,
		}

		var out bytes.Buffer
		if err := DecodeSchema(&out, bytes.NewReader(payload), sm); err == nil && out.String() != `{"a":1}` {
			t.Fatalf("accepted wrong content %q", out.String())
		}

		if out.Len() > 8 {
			t.Fatalf("wrote %d bytes past the declared size", out.Len())
		}
	})
}

func findBlob(t *testing.T, packed *Packed, d string) []byte {
	t.Helper()

	for _, b := range packed.Blobs {
		if b.Descriptor.Digest.String() == d {
			return b.Data
		}
	}

	t.Fatalf("blob %s not found", d)

	return nil
}

// TestGzipRecipeGolden pins the bytes of a gzip-packed schema with a notice
// layer. A Go toolchain whose compress/flate output differs, or any change to
// the packing recipe, fails here and must be an explicit, reviewed update.
func TestGzipRecipeGolden(t *testing.T) {
	packed, err := PackSchema(bigSchema(), []byte("NOTICE text\n"))
	if err != nil {
		t.Fatal(err)
	}

	const (
		wantManifest = "sha256:5e2b19912ee2a399a4cab69af7569430e80fb7a274124493cfe057947af41ebb"
		wantPayload  = "sha256:e6329060f8abb807d260b1af81c36b966200d8b1f816cace27f1681c0c414166"
		wantSize     = 685
	)

	if got := packed.Manifest.Descriptor.Digest.String(); got != wantManifest {
		t.Errorf("manifest digest = %s, want %s (packing recipe or compress/flate output changed)", got, wantManifest)
	}

	payload := packed.Blobs[1].Descriptor
	if payload.MediaType != SchemaGzipMediaType || payload.Digest.String() != wantPayload || payload.Size != wantSize {
		t.Errorf("payload = %s %s %d, want gzip %s %d", payload.MediaType, payload.Digest, payload.Size, wantPayload, wantSize)
	}
}

func FuzzParseSchemaManifest(f *testing.F) {
	for _, schema := range [][]byte{[]byte(`{"type":"object"}`), bigSchema()} {
		packed, err := PackSchema(schema, []byte("NOTICE"))
		if err != nil {
			f.Fatal(err)
		}

		f.Add(packed.Manifest.Data)
	}

	catalog, err := PackCatalog([]byte(`{"formatVersion":2}`), nil)
	if err != nil {
		f.Fatal(err)
	}

	f.Add(catalog.Metadata.Manifest.Data)
	f.Add([]byte(`{"schemaVersion":2}`))
	f.Add(bytes.Replace(catalog.Metadata.Manifest.Data, []byte(".catalog-metadata.v2\""), []byte(".schema.v3\""), 1))

	limits := DefaultLimits()

	f.Fuzz(func(t *testing.T, data []byte) {
		sm, err := ParseSchemaManifest(data, limits)
		if err != nil {
			if !errors.Is(err, ErrInvalid) && !isUnsupported(err) {
				t.Fatalf("error is neither ErrInvalid nor an unsupported format: %v", err)
			}

			return
		}

		if sm.ContentSize <= 0 || sm.ContentSize > limits.MaxSchemaBytes || sm.Payload.Size <= 0 || sm.Payload.Size > limits.MaxPayloadBytes {
			t.Fatalf("accepted out-of-range sizes: %+v", sm)
		}

		if sm.Payload.MediaType != SchemaMediaType && sm.Payload.MediaType != SchemaGzipMediaType {
			t.Fatalf("accepted payload media type %q", sm.Payload.MediaType)
		}
	})
}

func FuzzParseCatalogMetadata(f *testing.F) {
	packed, err := PackCatalog([]byte(`{"formatVersion":2}`), nil)
	if err != nil {
		f.Fatal(err)
	}

	f.Add(packed.Metadata.Manifest.Data)

	schema, err := PackSchema([]byte(`true`), nil)
	if err != nil {
		f.Fatal(err)
	}

	f.Add(schema.Manifest.Data)
	f.Add(bytes.Replace(packed.Metadata.Manifest.Data, []byte(".catalog.v2+json"), []byte(".catalog.v3+json"), 1))

	limits := DefaultLimits()

	f.Fuzz(func(t *testing.T, data []byte) {
		cm, err := ParseCatalogMetadata(data, limits)
		if err != nil {
			if !errors.Is(err, ErrInvalid) && !isUnsupported(err) {
				t.Fatalf("error is neither ErrInvalid nor an unsupported format: %v", err)
			}

			return
		}

		if cm.Payload.MediaType != CatalogMediaType || cm.Payload.Size <= 0 || cm.Payload.Size > limits.MaxCatalogBytes {
			t.Fatalf("accepted catalog payload %+v", cm.Payload)
		}
	})
}
