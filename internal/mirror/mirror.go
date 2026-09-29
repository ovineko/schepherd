// Package mirror copies a complete catalog snapshot between repositories.
//
// A catalog is an OCI image index whose children are its metadata manifest
// and every schema manifest it lists, so the whole snapshot is one OCI
// graph. Run verifies the source catalog as the client does and copies that
// graph with ORAS, children before parents, so the index and its revision
// tag appear in the destination only once everything they reference is
// there. Every schema manifest is checked against the wire contract and
// every body is verified by size and digest while it is uploaded.
package mirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/calver"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/registry"
	"github.com/ovineko/schepherd/internal/store"
)

// DefaultConcurrency bounds how many nodes of the graph are copied at once.
const DefaultConcurrency = 4

// Options configures a mirror run.
type Options struct {
	Log            func(format string, args ...any)
	ArtifactLimits artifact.Limits
	CatalogLimits  catalog.Limits
	Concurrency    int
}

// Result summarizes a mirror run. It is printed by "schepherd mirror --json".
type Result struct {
	Source           string `json:"source"`
	Destination      string `json:"destination"`
	CatalogDigest    string `json:"catalogDigest"`
	Revision         string `json:"revision"`
	Schemas          int    `json:"schemas"`
	CopiedManifests  int    `json:"copiedManifests"`
	SkippedManifests int    `json:"skippedManifests"`
	CopiedBlobs      int    `json:"copiedBlobs"`
	SkippedBlobs     int    `json:"skippedBlobs"`
	TagsCreated      int    `json:"tagsCreated"`
	TagsExisting     int    `json:"tagsExisting"`
}

type counters struct {
	copiedManifests  atomic.Int64
	skippedManifests atomic.Int64
	copiedBlobs      atomic.Int64
	skippedBlobs     atomic.Int64
}

// Run mirrors the snapshot identified by catalogDigest from src to dst.
func Run(ctx context.Context, src, dst *registry.Repo, catalogDigest string, opts Options) (*Result, error) {
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}

	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}

	if err := digest.Validate(catalogDigest); err != nil {
		return nil, fault.Wrap(fault.Usage, err, "invalid catalog digest")
	}

	snap, err := store.FetchCatalog(ctx, src, catalogDigest, opts.ArtifactLimits, opts.CatalogLimits)
	if err != nil {
		return nil, fault.Wrap(fault.Registry, err, "source catalog")
	}

	revision, err := calver.ParseRevision(snap.Catalog.Revision)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "source catalog revision")
	}

	opts.Log("mirroring catalog %s (%s) with %d schema artifacts", revision, catalogDigest, len(snap.Parsed.Schemas))

	cp := &copier{src: src, dst: dst, stats: &counters{}, opts: opts}
	if err := cp.copyGraph(ctx, snap); err != nil {
		return nil, err
	}

	created, err := dst.EnsureTag(ctx, snap.Index.Descriptor, revision.Tag())
	if err != nil {
		return nil, fault.Wrap(fault.Registry, err, "tag %s", revision.Tag())
	}

	res := &Result{
		Source:           src.Name().String(),
		Destination:      dst.Name().String(),
		CatalogDigest:    catalogDigest,
		Revision:         revision.String(),
		Schemas:          len(snap.Parsed.Schemas),
		CopiedManifests:  int(cp.stats.copiedManifests.Load()),
		SkippedManifests: int(cp.stats.skippedManifests.Load()),
		CopiedBlobs:      int(cp.stats.copiedBlobs.Load()),
		SkippedBlobs:     int(cp.stats.skippedBlobs.Load()),
	}

	if created {
		res.TagsCreated = 1
	} else {
		res.TagsExisting = 1
	}

	return res, nil
}

// prefetched serves already verified bytes of some descriptors from memory
// so content fetched for validation is not downloaded a second time. Every
// body it hands to the copy is verified against its descriptor.
type prefetched struct {
	content.ReadOnlyStorage

	data map[godigest.Digest][]byte
}

func (p prefetched) Fetch(ctx context.Context, target ocispec.Descriptor) (io.ReadCloser, error) {
	if data, ok := p.data[target.Digest]; ok && int64(len(data)) == target.Size {
		return verified(io.NopCloser(bytes.NewReader(data)), target), nil
	}

	rc, err := p.ReadOnlyStorage.Fetch(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", target.Digest, err)
	}

	return verified(rc, target), nil
}

// verifiedReader streams source content to the destination and withholds
// the final bytes until the whole body has matched the descriptor's size and
// digest. ORAS pipes the source response straight into the upload and
// checks at most the response headers, so without this the destination
// registry alone would decide whether a corrupt, truncated or overlong body
// is stored. Failing before the last byte leaves the upload short of its
// declared length, so no destination can complete it.
type verifiedReader struct {
	io.Closer

	r        io.Reader
	verifier godigest.Verifier
	err      error
	desc     ocispec.Descriptor
	read     int64
}

// verified refuses a descriptor it cannot check before touching the body:
// go-digest panics on a malformed or unknown digest, and Read cannot bound a
// negative size.
func verified(rc io.ReadCloser, desc ocispec.Descriptor) *verifiedReader {
	v := &verifiedReader{Closer: rc, r: rc, desc: desc}

	switch err := digest.Validate(desc.Digest.String()); {
	case err != nil:
		v.err = fault.Wrap(fault.Integrity, err, "source content descriptor")
	case desc.Size < 0:
		v.err = v.mismatch(content.ErrInvalidDescriptorSize, "the descriptor declares %d bytes", desc.Size)
	default:
		v.verifier = desc.Digest.Verifier()
	}

	return v
}

func (v *verifiedReader) Read(p []byte) (int, error) {
	if v.err != nil {
		return 0, v.err
	}

	if remaining := v.desc.Size - v.read; int64(len(p)) > remaining {
		p = p[:remaining]
	}

	var (
		n   int
		err error
	)

	if len(p) > 0 {
		n, err = v.r.Read(p)
		_, _ = v.verifier.Write(p[:n])
		v.read += int64(n)
	}

	switch {
	case v.read == v.desc.Size:
		if verr := v.finish(); verr != nil {
			v.err = verr

			return 0, verr
		}

		v.err = io.EOF

		return n, io.EOF
	case errors.Is(err, io.EOF):
		v.err = v.mismatch(content.ErrInvalidDescriptorSize, "the body ended after %d of %d bytes", v.read, v.desc.Size)

		return 0, v.err
	case err != nil:
		v.err = fmt.Errorf("read source %s: %w", v.desc.Digest, err)

		return n, v.err
	default:
		return n, nil
	}
}

func (v *verifiedReader) finish() error {
	var extra [1]byte

	switch _, err := io.ReadFull(v.r, extra[:]); {
	case err == nil:
		return v.mismatch(content.ErrTrailingData, "the body is longer than %d bytes", v.desc.Size)
	case !errors.Is(err, io.EOF):
		return fmt.Errorf("read source %s: %w", v.desc.Digest, err)
	}

	if !v.verifier.Verified() {
		return v.mismatch(content.ErrMismatchedDigest, "the body does not hash to its digest")
	}

	return nil
}

func (v *verifiedReader) mismatch(cause error, format string, args ...any) error {
	return fault.Wrap(fault.Integrity, cause, "source content %s: %s", v.desc.Digest, fmt.Sprintf(format, args...))
}

type copier struct {
	src   *registry.Repo
	dst   *registry.Repo
	stats *counters
	opts  Options
}

func (c *copier) count(desc ocispec.Descriptor, copied bool) {
	isManifest := desc.MediaType == artifact.ManifestMediaType || desc.MediaType == artifact.IndexMediaType

	switch {
	case isManifest && copied:
		c.stats.copiedManifests.Add(1)
	case isManifest:
		c.stats.skippedManifests.Add(1)
	case copied:
		c.stats.copiedBlobs.Add(1)
	default:
		c.stats.skippedBlobs.Add(1)
	}
}

// copyGraph copies the graph of the verified snapshot. The index, the
// metadata manifest and catalog.json are served from the verified bytes;
// every schema manifest is fetched once, checked against the wire contract
// and then copied with its blobs. A node the destination already has is
// taken to have its whole graph, as registries accept a manifest only after
// everything it references.
func (c *copier) copyGraph(ctx context.Context, snap *store.Snapshot) error {
	root := snap.Index.Descriptor
	children := append([]ocispec.Descriptor{snap.Parsed.Metadata}, snap.Parsed.Schemas...)
	metadataChildren := []ocispec.Descriptor{ocispec.DescriptorEmptyJSON, snap.Payload.Descriptor}
	known := map[godigest.Digest][]byte{
		root.Digest:                        snap.Index.Data,
		snap.Metadata.Descriptor.Digest:    snap.Metadata.Data,
		snap.Payload.Descriptor.Digest:     snap.Payload.Data,
		ocispec.DescriptorEmptyJSON.Digest: ocispec.DescriptorEmptyJSON.Data,
	}

	err := oras.CopyGraph(ctx, prefetched{ReadOnlyStorage: c.src.Target(), data: known}, c.dst.Target(), root, oras.CopyGraphOptions{
		Concurrency:      c.opts.Concurrency,
		MaxMetadataBytes: c.opts.ArtifactLimits.MaxManifestBytes,
		FindSuccessors: func(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
			switch {
			case desc.Digest == root.Digest:
				return children, nil
			case desc.Digest == snap.Parsed.Metadata.Digest:
				return metadataChildren, nil
			case desc.MediaType == artifact.ManifestMediaType:
				return c.schemaChildren(ctx, fetcher, desc)
			default:
				return nil, nil
			}
		},
		PostCopy: func(_ context.Context, desc ocispec.Descriptor) error {
			c.count(desc, true)

			return nil
		},
		OnCopySkipped: func(_ context.Context, desc ocispec.Descriptor) error {
			c.count(desc, false)

			return nil
		},
	})
	if err != nil {
		return fault.Wrap(fault.Registry, registry.Classify(err, "copy "+root.Digest.String()), "copy %s", root.Digest)
	}

	return nil
}

func (c *copier) schemaChildren(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
	data, err := content.FetchAll(ctx, fetcher, desc)
	if err != nil {
		return nil, fmt.Errorf("fetch source schema manifest %s: %w", desc.Digest, err)
	}

	sm, err := artifact.ParseSchemaManifest(data, c.opts.ArtifactLimits)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "source schema manifest %s", desc.Digest)
	}

	successors := []ocispec.Descriptor{ocispec.DescriptorEmptyJSON, sm.Payload}
	if sm.Notice != nil {
		successors = append(successors, *sm.Notice)
	}

	return successors, nil
}
