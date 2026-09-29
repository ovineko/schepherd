// Package artifact defines the OCI wire contract of Schepherd artifacts and
// packs and parses them deterministically.
//
// A schema artifact is an OCI image manifest with artifactType
// application/vnd.ovineko.schepherd.schema.v2, the empty JSON config and
// exactly one payload layer holding one JSON Schema, optionally followed by a
// single notice layer. A catalog is an OCI image index with artifactType
// application/vnd.ovineko.schepherd.catalog.v2 whose children are one
// catalog metadata manifest (artifactType
// application/vnd.ovineko.schepherd.catalog-metadata.v2, exactly one
// catalog.json layer) followed by every distinct schema manifest the
// catalog lists, so generic OCI tools and garbage collectors follow the
// whole snapshot from the index. Manifests and indexes carry no timestamps
// or host-specific annotations, so equal content always produces equal
// digests.
package artifact

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/jsonutil"
)

// Media and artifact types of wire format version 2. These are project
// conventions, not IANA registrations (application/schema+json aside).
const (
	ManifestMediaType           = ocispec.MediaTypeImageManifest
	IndexMediaType              = ocispec.MediaTypeImageIndex
	SchemaArtifactType          = "application/vnd.ovineko.schepherd.schema.v2"
	CatalogArtifactType         = "application/vnd.ovineko.schepherd.catalog.v2"
	CatalogMetadataArtifactType = "application/vnd.ovineko.schepherd.catalog-metadata.v2"
	SchemaMediaType             = "application/schema+json"
	SchemaGzipMediaType         = "application/vnd.ovineko.schepherd.schema.v2+gzip"
	CatalogMediaType            = "application/vnd.ovineko.schepherd.catalog.v2+json"
	NoticeMediaType             = "application/vnd.ovineko.schepherd.notice.v2+text"
)

// Layer annotations. The content annotations describe the uncompressed schema
// so a gzip payload can be verified after decompression.
const (
	AnnotationContentDigest = "com.ovineko.schepherd.content.digest"
	AnnotationContentSize   = "com.ovineko.schepherd.content.size"
)

// Packing recipe. Changing any of these values changes artifact bytes and must
// be a deliberate, reviewed update of RecipeVersion and the golden tests.
const (
	RecipeVersion = "schepherd-pack/1"
	// GzipThreshold is the compact JSON size from which compression is tried.
	GzipThreshold = 4096
	// gzipMinSavingPercent is the minimum saving that makes gzip worth it.
	gzipMinSavingPercent = 10
	payloadTitle         = "schema.json"
	gzipPayloadTitle     = "schema.json.gz"
	catalogTitle         = "catalog.json"
	noticeTitle          = "NOTICE"
	maxManifestDepth     = 16
)

// Limits bounds artifacts accepted from registries or caches.
type Limits struct {
	MaxManifestBytes int64
	MaxCatalogBytes  int64
	MaxPayloadBytes  int64
	MaxSchemaBytes   int64
	MaxNoticeBytes   int64
}

// DefaultLimits returns the documented defaults.
func DefaultLimits() Limits {
	return Limits{
		MaxManifestBytes: 4 << 20,
		MaxCatalogBytes:  32 << 20,
		MaxPayloadBytes:  64 << 20,
		MaxSchemaBytes:   64 << 20,
		MaxNoticeBytes:   1 << 20,
	}
}

var (
	// ErrInvalid marks artifacts that violate the wire contract.
	ErrInvalid = errors.New("invalid artifact")
	// ErrUnsupported marks artifacts typed with a newer version of the
	// Schepherd wire format. Errors wrapping it are fault.Usage errors, so a
	// caller that classifies artifact errors as fault.Integrity keeps exit
	// code 2 for them, and they never match ErrInvalid: the artifact is not
	// corrupt, only newer than this client.
	ErrUnsupported = errors.New("unsupported artifact format")
)

// wireFormat is the version this build implements, the "v2" of every
// Schepherd artifact and media type.
const wireFormat = "2"

// schepherdType matches the artifact and media types of every wire format
// version: kind, version and an optional structured syntax suffix.
var schepherdType = regexp.MustCompile(`^application/vnd\.ovineko\.schepherd\.(schema|catalog|catalog-metadata|notice)\.v([1-9][0-9]*)(?:\+[0-9a-z][0-9a-z.-]*)?$`)

// newerWireFormat returns the version of a recognised Schepherd type of the
// given kind (any kind when kind is empty) whose version is newer than
// wireFormat. Older versions were never published and are merely invalid.
func newerWireFormat(mediaType, kind string) (string, bool) {
	m := schepherdType.FindStringSubmatch(mediaType)
	if m == nil || (kind != "" && m[1] != kind) {
		return "", false
	}

	if version := m[2]; len(version) > len(wireFormat) || (len(version) == len(wireFormat) && version > wireFormat) {
		return version, true
	}

	return "", false
}

func unsupported(field, mediaType, version string) error {
	return fault.Wrap(fault.Usage, fmt.Errorf("%w: %s %q is Schepherd wire format v%s, but this client is too old and reads only v%s; upgrade schepherd",
		ErrUnsupported, field, mediaType, version, wireFormat), "")
}

// Blob is content addressed by its descriptor.
type Blob struct {
	Data       []byte
	Descriptor ocispec.Descriptor
}

// Packed is a complete artifact: the manifest and every blob it references.
type Packed struct {
	Manifest      Blob
	Blobs         []Blob
	ContentDigest string
	ContentSize   int64
}

func newBlob(mediaType string, data []byte, annotations map[string]string) Blob {
	return Blob{
		Data: data,
		Descriptor: ocispec.Descriptor{
			MediaType:   mediaType,
			Digest:      godigest.FromBytes(data),
			Size:        int64(len(data)),
			Annotations: annotations,
		},
	}
}

// EncodePayload chooses the payload encoding for compact schema JSON. Inputs
// below GzipThreshold are stored as-is; larger inputs are gzip-compressed
// with fixed parameters and kept compressed only when that saves at least
// gzipMinSavingPercent.
func EncodePayload(schema []byte) (mediaType string, payload []byte, err error) {
	if len(schema) < GzipThreshold {
		return SchemaMediaType, schema, nil
	}

	compressed, err := Gzip(schema)
	if err != nil {
		return "", nil, err
	}

	if int64(len(compressed))*100 <= int64(len(schema))*(100-gzipMinSavingPercent) {
		return SchemaGzipMediaType, compressed, nil
	}

	return SchemaMediaType, schema, nil
}

// Gzip compresses data deterministically for a given Go toolchain: best
// compression, zero mtime, no file name or comment. Another toolchain may
// produce other bytes; published artifacts of unchanged content are reused
// from the publisher state, so that never changes a published digest.
func Gzip(data []byte) ([]byte, error) {
	var buf bytes.Buffer

	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}

	if _, err := zw.Write(data); err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}

	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}

	return buf.Bytes(), nil
}

// PackSchema packs one prepared schema. The schema must already be compact
// JSON; notice may be nil.
func PackSchema(schema, notice []byte) (*Packed, error) {
	if err := jsonutil.Check(schema, 0); err != nil {
		return nil, fmt.Errorf("%w: schema payload: %w", ErrInvalid, err)
	}

	mediaType, payload, err := EncodePayload(schema)
	if err != nil {
		return nil, err
	}

	title := payloadTitle
	if mediaType == SchemaGzipMediaType {
		title = gzipPayloadTitle
	}

	contentDigest := digest.FromBytes(schema)
	layers := []Blob{newBlob(mediaType, payload, map[string]string{
		AnnotationContentDigest: contentDigest,
		AnnotationContentSize:   strconv.Itoa(len(schema)),
		ocispec.AnnotationTitle: title,
	})}

	if notice != nil {
		layers = append(layers, newBlob(NoticeMediaType, notice, map[string]string{ocispec.AnnotationTitle: noticeTitle}))
	}

	packed, err := pack(SchemaArtifactType, layers)
	if err != nil {
		return nil, err
	}

	packed.ContentDigest = contentDigest
	packed.ContentSize = int64(len(schema))

	return packed, nil
}

// PackedCatalog is a complete catalog: the index users pin and the metadata
// artifact holding catalog.json. The schema artifacts the index references
// are packed separately.
type PackedCatalog struct {
	Metadata *Packed
	Index    Blob
}

// PackCatalog packs canonical catalog bytes into a metadata artifact and the
// catalog index over it and schemas, the distinct schema manifest
// descriptors catalogJSON lists. The index lists the metadata manifest
// first, then the schema manifests in ascending digest order, so the same
// catalog always gives the same index digest.
func PackCatalog(catalogJSON []byte, schemas []ocispec.Descriptor) (*PackedCatalog, error) {
	layer := newBlob(CatalogMediaType, catalogJSON, map[string]string{ocispec.AnnotationTitle: catalogTitle})

	metadata, err := pack(CatalogMetadataArtifactType, []Blob{layer})
	if err != nil {
		return nil, err
	}

	metadata.ContentDigest = layer.Descriptor.Digest.String()
	metadata.ContentSize = layer.Descriptor.Size

	children := make([]ocispec.Descriptor, 0, len(schemas)+1)
	metadataDesc := metadata.Manifest.Descriptor
	metadataDesc.ArtifactType = CatalogMetadataArtifactType
	children = append(children, metadataDesc)

	sorted := slices.Clone(schemas)
	slices.SortFunc(sorted, func(a, b ocispec.Descriptor) int { return strings.Compare(a.Digest.String(), b.Digest.String()) })

	for i, d := range sorted {
		if err := checkSchemaChild(d, math.MaxInt64); err != nil {
			return nil, fmt.Errorf("pack catalog index: %w", err)
		}

		if i > 0 && d.Digest == sorted[i-1].Digest {
			return nil, fmt.Errorf("%w: pack catalog index: schema manifest %s is listed twice", ErrInvalid, d.Digest)
		}

		children = append(children, ocispec.Descriptor{MediaType: d.MediaType, Digest: d.Digest, Size: d.Size})
	}

	data, err := json.Marshal(ocispec.Index{
		SchemaVersion: 2,
		MediaType:     IndexMediaType,
		ArtifactType:  CatalogArtifactType,
		Manifests:     children,
	})
	if err != nil {
		return nil, fmt.Errorf("encode catalog index: %w", err)
	}

	return &PackedCatalog{Metadata: metadata, Index: newBlob(IndexMediaType, data, nil)}, nil
}

func pack(artifactType string, layers []Blob) (*Packed, error) {
	config := Blob{Data: ocispec.DescriptorEmptyJSON.Data, Descriptor: ocispec.DescriptorEmptyJSON}

	manifest := ocispec.Manifest{
		SchemaVersion: 2,
		MediaType:     ManifestMediaType,
		ArtifactType:  artifactType,
		Config:        config.Descriptor,
		Layers:        make([]ocispec.Descriptor, 0, len(layers)),
	}

	for _, l := range layers {
		manifest.Layers = append(manifest.Layers, l.Descriptor)
	}

	data, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}

	blobs := append([]Blob{config}, layers...)

	return &Packed{Manifest: newBlob(ManifestMediaType, data, nil), Blobs: blobs}, nil
}

// SchemaManifest is a validated schema artifact manifest.
type SchemaManifest struct {
	Notice        *ocispec.Descriptor
	ContentDigest string
	Payload       ocispec.Descriptor
	ContentSize   int64
}

// CatalogMetadata is a validated catalog metadata manifest.
type CatalogMetadata struct {
	Payload ocispec.Descriptor
}

type wireManifest struct {
	Annotations   map[string]string    `json:"annotations,omitempty"`
	Subject       *ocispec.Descriptor  `json:"subject,omitempty"`
	MediaType     string               `json:"mediaType"`
	ArtifactType  string               `json:"artifactType"`
	Layers        []ocispec.Descriptor `json:"layers"`
	Config        ocispec.Descriptor   `json:"config"`
	SchemaVersion int                  `json:"schemaVersion"`
}

func parseManifest(data []byte, artifactType string, limits Limits) (*wireManifest, error) {
	if int64(len(data)) > limits.MaxManifestBytes {
		return nil, fmt.Errorf("%w: manifest is %d bytes, limit is %d", ErrInvalid, len(data), limits.MaxManifestBytes)
	}

	if err := jsonutil.Check(data, maxManifestDepth); err != nil {
		return nil, fmt.Errorf("%w: manifest: %w", ErrInvalid, err)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var m wireManifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: manifest: %w", ErrInvalid, err)
	}

	switch {
	case m.SchemaVersion != 2:
		return nil, fmt.Errorf("%w: manifest schemaVersion %d", ErrInvalid, m.SchemaVersion)
	case m.MediaType != ManifestMediaType:
		return nil, fmt.Errorf("%w: manifest mediaType %q", ErrInvalid, m.MediaType)
	}

	if err := checkWireFormat(&m, artifactType); err != nil {
		return nil, err
	}

	if m.Subject != nil {
		return nil, fmt.Errorf("%w: unexpected subject", ErrInvalid)
	}

	empty := ocispec.DescriptorEmptyJSON
	if m.Config.MediaType != empty.MediaType || m.Config.Digest != empty.Digest || m.Config.Size != empty.Size ||
		len(m.Config.URLs) > 0 || (m.Config.Data != nil && !bytes.Equal(m.Config.Data, empty.Data)) {
		return nil, fmt.Errorf("%w: config must be the empty JSON descriptor", ErrInvalid)
	}

	for i, layer := range m.Layers {
		if len(layer.URLs) > 0 {
			return nil, fmt.Errorf("%w: layer %d declares external URLs", ErrInvalid, i)
		}

		if err := digest.Validate(layer.Digest.String()); err != nil {
			return nil, fmt.Errorf("%w: layer %d: %w", ErrInvalid, i, err)
		}

		if layer.Size <= 0 {
			return nil, fmt.Errorf("%w: layer %d has size %d", ErrInvalid, i, layer.Size)
		}

		if layer.Data != nil {
			return nil, fmt.Errorf("%w: layer %d embeds data", ErrInvalid, i)
		}
	}

	return &m, nil
}

// checkWireFormat runs before the v1 rules for subject, config and layers,
// so an artifact of another wire format version is refused as unsupported
// whatever those contain. An artifactType of another Schepherd kind, such as
// a catalog where a schema is expected, stays invalid.
func checkWireFormat(m *wireManifest, artifactType string) error {
	if m.ArtifactType != artifactType {
		kind := schepherdType.FindStringSubmatch(artifactType)[1]
		if version, ok := newerWireFormat(m.ArtifactType, kind); ok {
			return unsupported("artifactType", m.ArtifactType, version)
		}

		return fmt.Errorf("%w: artifactType %q, want %q", ErrInvalid, m.ArtifactType, artifactType)
	}

	for i, layer := range m.Layers {
		if version, ok := newerWireFormat(layer.MediaType, ""); ok {
			return unsupported(fmt.Sprintf("layer %d mediaType", i), layer.MediaType, version)
		}
	}

	return nil
}

// ParseSchemaManifest validates a schema artifact manifest. A manifest of
// another wire format version fails with ErrUnsupported, any other violation
// with ErrInvalid.
func ParseSchemaManifest(data []byte, limits Limits) (*SchemaManifest, error) {
	m, err := parseManifest(data, SchemaArtifactType, limits)
	if err != nil {
		return nil, err
	}

	if len(m.Layers) == 0 || len(m.Layers) > 2 {
		return nil, fmt.Errorf("%w: schema artifact has %d layers, want a payload and at most one notice", ErrInvalid, len(m.Layers))
	}

	payload := m.Layers[0]
	if payload.MediaType != SchemaMediaType && payload.MediaType != SchemaGzipMediaType {
		return nil, fmt.Errorf("%w: first layer mediaType %q is not a schema payload", ErrInvalid, payload.MediaType)
	}

	if payload.Size > limits.MaxPayloadBytes {
		return nil, fmt.Errorf("%w: payload is %d bytes, limit is %d", ErrInvalid, payload.Size, limits.MaxPayloadBytes)
	}

	contentDigest := payload.Annotations[AnnotationContentDigest]
	if err := digest.Validate(contentDigest); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalid, AnnotationContentDigest, err)
	}

	contentSize, err := strconv.ParseInt(payload.Annotations[AnnotationContentSize], 10, 64)
	if err != nil || contentSize <= 0 || strconv.FormatInt(contentSize, 10) != payload.Annotations[AnnotationContentSize] {
		return nil, fmt.Errorf("%w: %s must be a positive decimal integer", ErrInvalid, AnnotationContentSize)
	}

	if contentSize > limits.MaxSchemaBytes {
		return nil, fmt.Errorf("%w: schema is %d bytes, limit is %d", ErrInvalid, contentSize, limits.MaxSchemaBytes)
	}

	if payload.MediaType == SchemaMediaType && (contentDigest != payload.Digest.String() || contentSize != payload.Size) {
		return nil, fmt.Errorf("%w: uncompressed payload annotations disagree with its descriptor", ErrInvalid)
	}

	sm := &SchemaManifest{Payload: payload, ContentDigest: contentDigest, ContentSize: contentSize}

	if len(m.Layers) == 2 {
		notice := m.Layers[1]
		if notice.MediaType != NoticeMediaType {
			return nil, fmt.Errorf("%w: unexpected second layer mediaType %q", ErrInvalid, notice.MediaType)
		}

		if notice.Size > limits.MaxNoticeBytes {
			return nil, fmt.Errorf("%w: notice is %d bytes, limit is %d", ErrInvalid, notice.Size, limits.MaxNoticeBytes)
		}

		sm.Notice = &notice
	}

	return sm, nil
}

// ParseCatalogMetadata validates a catalog metadata manifest, the child of
// a catalog index that holds catalog.json. Errors are classified as for
// ParseSchemaManifest.
func ParseCatalogMetadata(data []byte, limits Limits) (*CatalogMetadata, error) {
	m, err := parseManifest(data, CatalogMetadataArtifactType, limits)
	if err != nil {
		return nil, err
	}

	if len(m.Layers) != 1 || m.Layers[0].MediaType != CatalogMediaType {
		return nil, fmt.Errorf("%w: catalog metadata artifact must have exactly one %s layer", ErrInvalid, CatalogMediaType)
	}

	if m.Layers[0].Size > limits.MaxCatalogBytes {
		return nil, fmt.Errorf("%w: catalog is %d bytes, limit is %d", ErrInvalid, m.Layers[0].Size, limits.MaxCatalogBytes)
	}

	return &CatalogMetadata{Payload: m.Layers[0]}, nil
}

// CatalogIndex is a validated catalog index: the descriptor of its metadata
// manifest and those of the schema manifests, in index order.
type CatalogIndex struct {
	Metadata ocispec.Descriptor
	Schemas  []ocispec.Descriptor
}

type wireIndex struct {
	Annotations   map[string]string    `json:"annotations,omitempty"`
	Subject       *ocispec.Descriptor  `json:"subject,omitempty"`
	MediaType     string               `json:"mediaType"`
	ArtifactType  string               `json:"artifactType"`
	Manifests     []ocispec.Descriptor `json:"manifests"`
	SchemaVersion int                  `json:"schemaVersion"`
}

// ParseCatalogIndex validates a catalog index: an OCI image index with
// artifactType CatalogArtifactType, exactly one child typed
// CatalogMetadataArtifactType and otherwise only distinct, untyped schema
// manifest descriptors. It does not check the children against
// catalog.json; CheckSchemas does. An index of a newer wire format fails
// with ErrUnsupported, any other violation with ErrInvalid.
func ParseCatalogIndex(data []byte, limits Limits) (*CatalogIndex, error) {
	if int64(len(data)) > limits.MaxManifestBytes {
		return nil, fmt.Errorf("%w: catalog index is %d bytes, limit is %d", ErrInvalid, len(data), limits.MaxManifestBytes)
	}

	if err := jsonutil.Check(data, maxManifestDepth); err != nil {
		return nil, fmt.Errorf("%w: catalog index: %w", ErrInvalid, err)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var ix wireIndex
	if err := dec.Decode(&ix); err != nil {
		return nil, fmt.Errorf("%w: catalog index: %w", ErrInvalid, err)
	}

	switch {
	case ix.SchemaVersion != 2:
		return nil, fmt.Errorf("%w: catalog index schemaVersion %d", ErrInvalid, ix.SchemaVersion)
	case ix.MediaType != IndexMediaType:
		return nil, fmt.Errorf("%w: catalog index mediaType %q, want %q", ErrInvalid, ix.MediaType, IndexMediaType)
	}

	if err := checkIndexWireFormat(&ix); err != nil {
		return nil, err
	}

	if ix.Subject != nil {
		return nil, fmt.Errorf("%w: catalog index: unexpected subject", ErrInvalid)
	}

	ci := &CatalogIndex{Schemas: make([]ocispec.Descriptor, 0, len(ix.Manifests))}
	seen := make(map[godigest.Digest]struct{}, len(ix.Manifests))
	metadataFound := false

	for i, child := range ix.Manifests {
		if len(child.URLs) > 0 || child.Data != nil || child.Platform != nil || child.Annotations != nil {
			return nil, fmt.Errorf("%w: catalog index child %d declares urls, data, platform or annotations", ErrInvalid, i)
		}

		if _, dup := seen[child.Digest]; dup {
			return nil, fmt.Errorf("%w: catalog index lists %s twice", ErrInvalid, child.Digest)
		}

		seen[child.Digest] = struct{}{}

		switch child.ArtifactType {
		case CatalogMetadataArtifactType:
			if metadataFound {
				return nil, fmt.Errorf("%w: catalog index has more than one catalog metadata manifest", ErrInvalid)
			}

			if err := checkManifestChild(child, limits.MaxManifestBytes); err != nil {
				return nil, fmt.Errorf("catalog index child %d: %w", i, err)
			}

			ci.Metadata, metadataFound = child, true
		case "":
			if err := checkSchemaChild(child, limits.MaxManifestBytes); err != nil {
				return nil, fmt.Errorf("catalog index child %d: %w", i, err)
			}

			ci.Schemas = append(ci.Schemas, child)
		default:
			return nil, fmt.Errorf("%w: catalog index child %d has artifactType %q", ErrInvalid, i, child.ArtifactType)
		}
	}

	if !metadataFound {
		return nil, fmt.Errorf("%w: catalog index has no %s manifest", ErrInvalid, CatalogMetadataArtifactType)
	}

	return ci, nil
}

// checkIndexWireFormat refuses an index of a newer wire format, or with a
// child typed with one, before any other rule applies.
func checkIndexWireFormat(ix *wireIndex) error {
	if ix.ArtifactType != CatalogArtifactType {
		if version, ok := newerWireFormat(ix.ArtifactType, "catalog"); ok {
			return unsupported("artifactType", ix.ArtifactType, version)
		}

		return fmt.Errorf("%w: catalog index artifactType %q, want %q", ErrInvalid, ix.ArtifactType, CatalogArtifactType)
	}

	for i, child := range ix.Manifests {
		if version, ok := newerWireFormat(child.ArtifactType, ""); ok {
			return unsupported(fmt.Sprintf("catalog index child %d artifactType", i), child.ArtifactType, version)
		}
	}

	return nil
}

func checkManifestChild(d ocispec.Descriptor, maxManifestBytes int64) error {
	if err := digest.Validate(d.Digest.String()); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	switch {
	case d.MediaType != ManifestMediaType:
		return fmt.Errorf("%w: %s has mediaType %q, want %q", ErrInvalid, d.Digest, d.MediaType, ManifestMediaType)
	case d.Size <= 0 || d.Size > maxManifestBytes:
		return fmt.Errorf("%w: manifest %s has size %d, limit is %d", ErrInvalid, d.Digest, d.Size, maxManifestBytes)
	}

	return nil
}

func checkSchemaChild(d ocispec.Descriptor, maxManifestBytes int64) error {
	if d.ArtifactType != "" || len(d.URLs) > 0 || d.Data != nil || d.Platform != nil || d.Annotations != nil {
		return fmt.Errorf("%w: schema manifest descriptor %s carries more than mediaType, digest and size", ErrInvalid, d.Digest)
	}

	return checkManifestChild(d, maxManifestBytes)
}

// CheckSchemas verifies that listed, the distinct schema manifest
// descriptors of catalog.json, are exactly the schema children of the
// index, with equal media types and sizes. The index is what registries,
// garbage collectors and generic copies follow, catalog.json what the
// client resolves IDs with; a catalog in which they disagree is invalid.
func (ci *CatalogIndex) CheckSchemas(listed []ocispec.Descriptor) error {
	children := make(map[godigest.Digest]ocispec.Descriptor, len(ci.Schemas))
	for _, d := range ci.Schemas {
		children[d.Digest] = d
	}

	seen := make(map[godigest.Digest]struct{}, len(listed))

	for _, d := range listed {
		if _, dup := seen[d.Digest]; dup {
			continue
		}

		seen[d.Digest] = struct{}{}

		child, ok := children[d.Digest]

		switch {
		case !ok:
			return fmt.Errorf("%w: catalog.json lists schema manifest %s, which the catalog index does not reference", ErrInvalid, d.Digest)
		case child.MediaType != d.MediaType || child.Size != d.Size:
			return fmt.Errorf("%w: schema manifest %s is %s of %d bytes in the catalog index but %s of %d bytes in catalog.json",
				ErrInvalid, d.Digest, child.MediaType, child.Size, d.MediaType, d.Size)
		}
	}

	if len(seen) != len(children) {
		for _, d := range ci.Schemas {
			if _, ok := seen[d.Digest]; !ok {
				return fmt.Errorf("%w: the catalog index references schema manifest %s, which catalog.json does not list", ErrInvalid, d.Digest)
			}
		}
	}

	return nil
}

// DecodeSchema streams the schema JSON held by payload (already verified
// against sm.Payload) into w, decompressing when needed. It enforces the
// declared content size and verifies the content digest.
func DecodeSchema(w io.Writer, payload io.Reader, sm *SchemaManifest) error {
	src := payload
	buffered := bufio.NewReader(payload)

	if sm.Payload.MediaType == SchemaGzipMediaType {
		zr, err := gzip.NewReader(buffered)
		if err != nil {
			return fmt.Errorf("%w: gzip payload: %w", ErrInvalid, err)
		}

		zr.Multistream(false)

		defer func() { _ = zr.Close() }()

		src = zr
	}

	h := digest.NewHash()

	// bearer:disable go_gosec_filesystem_decompression_bomb
	// The LimitReader bounds decompression by the declared content size,
	// which parsing already capped at Limits.MaxSchemaBytes.
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(src, sm.ContentSize+1))
	if err != nil {
		return fmt.Errorf("%w: decode payload: %w", ErrInvalid, err)
	}

	if n != sm.ContentSize {
		return fmt.Errorf("%w: decoded schema is %s bytes, manifest declares %d", ErrInvalid, sizeDescription(n, sm.ContentSize), sm.ContentSize)
	}

	if sm.Payload.MediaType == SchemaGzipMediaType {
		if _, err := buffered.Peek(1); !errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: trailing data after gzip stream", ErrInvalid)
		}
	}

	if got := digest.FromHash(h); got != sm.ContentDigest {
		return fmt.Errorf("%w: schema content digest %s, manifest declares %s", ErrInvalid, got, sm.ContentDigest)
	}

	return nil
}

func sizeDescription(n, declared int64) string {
	if n > declared {
		return "more than " + strconv.FormatInt(declared, 10)
	}

	return strconv.FormatInt(n, 10)
}
