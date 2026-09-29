package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"
	"time"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/fault"
)

func (s *Store) open() (Remote, error) {
	if s.opts.Offline {
		return nil, fault.New(fault.Offline, "network access is disabled (--offline)")
	}

	if !s.opened {
		s.opened = true
		s.remote, s.remoteErr = s.opts.Open()
	}

	return s.remote, s.remoteErr
}

// recoverable decides what to do with a failed local lookup. It returns nil
// when the caller should fetch, or the error to report.
func (s *Store) recoverable(err error, what string, quarantine bool) error {
	if errors.Is(err, cache.ErrNotFound) {
		if s.opts.Offline {
			return fault.New(fault.Offline, "%s is not in the local cache; run once without --offline to fetch it", what)
		}

		return nil
	}

	ce, ok := errors.AsType[*corruptError](err)
	if !ok {
		return err
	}

	if s.opts.Offline {
		return fault.Wrap(fault.Offline, err, "%s is corrupt in the local cache and --offline forbids fetching it again", what)
	}

	if !quarantine {
		return nil
	}

	s.opts.Log("%s is corrupt in the local cache (%v); fetching it again", what, err)

	if qerr := s.opts.Cache.QuarantineBlob(ce.digest); qerr != nil && !errors.Is(qerr, cache.ErrNotFound) {
		return fault.Wrap(fault.Internal, qerr, "quarantine cache entry %s", ce.digest)
	}

	return nil
}

func (s *Store) localCatalog(catalogDigest string) (*LoadedCatalog, error) {
	index, err := s.opts.Cache.ReadBlob(catalogDigest, s.opts.ArtifactLimits.MaxManifestBytes)
	if err := readLocal(catalogDigest, err); err != nil {
		return nil, err
	}

	ci, err := artifact.ParseCatalogIndex(index, s.opts.ArtifactLimits)
	if err != nil {
		return nil, localParseError(catalogDigest, "catalog index", err)
	}

	metadata, err := s.readDescribed(ci.Metadata, "catalog index")
	if err != nil {
		return nil, err
	}

	cm, err := artifact.ParseCatalogMetadata(metadata, s.opts.ArtifactLimits)
	if err != nil {
		return nil, localParseError(ci.Metadata.Digest.String(), "catalog metadata manifest", err)
	}

	raw, err := s.readDescribed(cm.Payload, "catalog metadata manifest")
	if err != nil {
		return nil, err
	}

	c, err := catalog.Parse(raw, s.opts.CatalogLimits)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "cached catalog %s", catalogDigest)
	}

	if err := ci.CheckSchemas(c.Artifacts()); err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "cached catalog %s", catalogDigest)
	}

	return &LoadedCatalog{Catalog: c, Digest: catalogDigest, Raw: raw}, nil
}

// readDescribed reads the cached blob desc describes; parent names the
// verified content that declares it.
func (s *Store) readDescribed(desc ocispec.Descriptor, parent string) ([]byte, error) {
	dgst := desc.Digest.String()

	data, err := s.opts.Cache.ReadBlob(dgst, desc.Size)
	if err := readLocal(dgst, err); err != nil {
		return nil, err
	}

	if int64(len(data)) != desc.Size {
		return nil, corrupt(dgst, fmt.Errorf("%w: size %d, %s declares %d", cache.ErrCorrupt, len(data), parent, desc.Size))
	}

	return data, nil
}

func (s *Store) fetchCatalog(ctx context.Context, catalogDigest string) (*LoadedCatalog, error) {
	remote, err := s.open()
	if err != nil {
		return nil, err
	}

	s.opts.Log("fetching catalog %s from %s", catalogDigest, s.opts.Repository)

	snap, err := FetchCatalog(ctx, remote, catalogDigest, s.opts.ArtifactLimits, s.opts.CatalogLimits)
	if err != nil {
		return nil, err
	}

	for _, b := range []artifact.Blob{snap.Payload, snap.Metadata, snap.Index} {
		size := b.Descriptor.Size
		if err := s.opts.Cache.WriteBlob(b.Descriptor.Digest.String(), size, size, bytes.NewReader(b.Data)); err != nil {
			return nil, cacheWriteError(err)
		}
	}

	return &LoadedCatalog{Catalog: snap.Catalog, Digest: catalogDigest, Raw: snap.Payload.Data}, nil
}

// Snapshot is a catalog fetched and verified by FetchCatalog: the index,
// its metadata manifest and the catalog.json payload, each with its
// descriptor, and the parsed catalog.
type Snapshot struct {
	Catalog  *catalog.Catalog
	Parsed   *artifact.CatalogIndex
	Index    artifact.Blob
	Metadata artifact.Blob
	Payload  artifact.Blob
}

// FetchCatalog downloads the catalog whose index has digest catalogDigest
// from remote, bypassing any cache, and verifies it as the client does:
// the index, its metadata manifest and catalog.json by digest, size and
// wire contract, and the schema manifests catalog.json lists against the
// schema children of the index. Transfer failures are fault.Registry,
// everything the pinned content gets wrong fault.Integrity, a newer wire
// or catalog format fault.Usage.
func FetchCatalog(ctx context.Context, remote Remote, catalogDigest string, al artifact.Limits, cl catalog.Limits) (*Snapshot, error) {
	index, err := remote.FetchManifestByDigest(ctx, catalogDigest, artifact.IndexMediaType, al.MaxManifestBytes)
	if err != nil {
		return nil, fault.Wrap(fault.Registry, err, "fetch catalog index %s", catalogDigest)
	}

	ci, err := artifact.ParseCatalogIndex(index, al)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "catalog %s", catalogDigest)
	}

	var metadata bytes.Buffer
	if err := remote.FetchTo(ctx, ci.Metadata, &metadata); err != nil {
		return nil, fault.Wrap(fault.Registry, err, "fetch catalog metadata manifest %s", ci.Metadata.Digest)
	}

	cm, err := artifact.ParseCatalogMetadata(metadata.Bytes(), al)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "catalog %s: metadata manifest %s", catalogDigest, ci.Metadata.Digest)
	}

	var raw bytes.Buffer
	if err := remote.FetchTo(ctx, cm.Payload, &raw); err != nil {
		return nil, fault.Wrap(fault.Registry, err, "fetch catalog payload %s", cm.Payload.Digest)
	}

	c, err := catalog.Parse(raw.Bytes(), cl)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "catalog %s", catalogDigest)
	}

	if err := ci.CheckSchemas(c.Artifacts()); err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "catalog %s", catalogDigest)
	}

	return &Snapshot{
		Catalog: c,
		Parsed:  ci,
		Index: artifact.Blob{Data: index, Descriptor: ocispec.Descriptor{
			MediaType: artifact.IndexMediaType, Digest: godigest.Digest(catalogDigest), Size: int64(len(index)),
		}},
		Metadata: artifact.Blob{Data: metadata.Bytes(), Descriptor: ci.Metadata},
		Payload:  artifact.Blob{Data: raw.Bytes(), Descriptor: cm.Payload},
	}, nil
}

func (s *Store) verifyMaterialized(entry *catalog.Entry, sm *artifact.SchemaManifest) error {
	err := s.opts.Cache.VerifyMaterialized(entry.Artifact.Digest, sm.ContentDigest, sm.ContentSize)

	switch {
	case err == nil:
		return nil
	case errors.Is(err, cache.ErrNotFound):
		return fmt.Errorf("materialized schema %q: %w", entry.ID, err)
	case errors.Is(err, cache.ErrCorrupt):
		return &corruptError{digest: entry.Artifact.Digest, err: fmt.Errorf("materialized schema %q: %w", entry.ID, err)}
	default:
		return fault.Wrap(fault.Internal, err, "verify materialized schema %q", entry.ID)
	}
}

func (s *Store) materializeLocked(ctx context.Context, entry *catalog.Entry, desc ocispec.Descriptor) (*Schema, error) {
	what := fmt.Sprintf("schema %q (manifest %s)", entry.ID, entry.Artifact.Digest)

	sm, err := s.localManifest(desc)
	if err != nil {
		if err := s.recoverable(err, what, true); err != nil {
			return nil, err
		}

		if sm, err = s.fetchManifest(ctx, entry, desc); err != nil {
			return nil, err
		}
	}

	verr := s.verifyMaterialized(entry, sm)
	if verr == nil {
		return s.schema(entry, sm), nil
	}

	if !errors.Is(verr, cache.ErrNotFound) {
		if _, corrupted := errors.AsType[*corruptError](verr); !corrupted {
			return nil, verr
		}

		if s.opts.Offline {
			return nil, fault.Wrap(fault.Offline, verr, "%s is corrupt in the local cache and --offline forbids fetching it again", what)
		}

		s.opts.Log("%s is corrupt in the local cache (%v); rebuilding it", what, verr)

		if err := s.opts.Cache.QuarantineMaterialized(entry.Artifact.Digest); err != nil && !errors.Is(err, cache.ErrNotFound) {
			return nil, fault.Wrap(fault.Internal, err, "quarantine %s", what)
		}
	}

	if err := s.ensurePayload(ctx, what, sm); err != nil {
		return nil, err
	}

	_, err = s.opts.Cache.WriteMaterialized(entry.Artifact.Digest, sm.ContentDigest, sm.ContentSize, func(w io.Writer) error {
		payload, err := s.opts.Cache.OpenBlob(sm.Payload.Digest.String())
		if err != nil {
			return fmt.Errorf("open payload: %w", err)
		}
		defer func() { _ = payload.Close() }()

		return artifact.DecodeSchema(w, payload, sm)
	})
	if err != nil {
		if errors.Is(err, artifact.ErrInvalid) {
			if qerr := s.opts.Cache.QuarantineBlob(sm.Payload.Digest.String()); qerr != nil && !errors.Is(qerr, cache.ErrNotFound) {
				s.opts.Log("quarantine payload %s: %v", sm.Payload.Digest, qerr)
			}

			return nil, fault.Wrap(fault.Integrity, err, "%s", what)
		}

		return nil, cacheWriteError(err)
	}

	return s.schema(entry, sm), nil
}

func (s *Store) localManifest(desc ocispec.Descriptor) (*artifact.SchemaManifest, error) {
	dgst := desc.Digest.String()

	data, err := s.opts.Cache.ReadBlob(dgst, desc.Size)
	if err := readLocal(dgst, err); err != nil {
		return nil, err
	}

	if int64(len(data)) != desc.Size {
		return nil, corrupt(dgst, fmt.Errorf("%w: size %d, catalog declares %d", cache.ErrCorrupt, len(data), desc.Size))
	}

	sm, err := artifact.ParseSchemaManifest(data, s.opts.ArtifactLimits)
	if err != nil {
		return nil, localParseError(dgst, "schema manifest", err)
	}

	return sm, nil
}

// localParseError classifies a cached manifest that verified against its
// digest but was refused by the parser. A manifest of a newer wire format
// (stored by a newer client sharing the cache) is not corrupt: fetching it
// again returns the same bytes, and quarantining it would damage that
// client's cache.
func localParseError(dgst, what string, err error) error {
	if errors.Is(err, artifact.ErrUnsupported) {
		return fault.Wrap(fault.Usage, err, "cached %s %s", what, dgst)
	}

	return corrupt(dgst, fmt.Errorf("%w: %w", cache.ErrCorrupt, err))
}

func (s *Store) fetchManifest(ctx context.Context, entry *catalog.Entry, desc ocispec.Descriptor) (*artifact.SchemaManifest, error) {
	remote, err := s.open()
	if err != nil {
		return nil, err
	}

	s.opts.Log("fetching schema %q (%s) from %s", entry.ID, desc.Digest, s.opts.Repository)

	var buf bytes.Buffer
	if err := remote.FetchTo(ctx, desc, &buf); err != nil {
		return nil, fault.Wrap(fault.Registry, err, "fetch schema manifest %s", desc.Digest)
	}

	sm, err := artifact.ParseSchemaManifest(buf.Bytes(), s.opts.ArtifactLimits)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "schema %q (manifest %s)", entry.ID, desc.Digest)
	}

	if err := s.opts.Cache.WriteBlob(desc.Digest.String(), desc.Size, desc.Size, bytes.NewReader(buf.Bytes())); err != nil {
		return nil, cacheWriteError(err)
	}

	return sm, nil
}

func (s *Store) ensurePayload(ctx context.Context, what string, sm *artifact.SchemaManifest) error {
	dgst := sm.Payload.Digest.String()

	err := readLocal(dgst, s.opts.Cache.VerifyBlob(dgst, sm.Payload.Size))
	if err == nil {
		return nil
	}

	if err := s.recoverable(err, "payload "+dgst+" of "+what, true); err != nil {
		return err
	}

	remote, err := s.open()
	if err != nil {
		return err
	}

	for attempt := 1; ; attempt++ {
		err = s.downloadPayload(ctx, remote, sm)
		if err == nil {
			return nil
		}

		if attempt == payloadAttempts || !interrupted(err) || ctx.Err() != nil {
			return err
		}

		s.opts.Log("download of payload %s of %s failed (%v); retrying", dgst, what, err)

		select {
		case <-ctx.Done():
			return fault.Wrap(fault.Registry, ctx.Err(), "download payload %s", dgst)
		case <-time.After(time.Duration(attempt) * payloadRetryDelay):
		}
	}
}

// payloadAttempts bounds downloads of one payload. The registry transport
// already retries failed requests; this loop covers a response body that is
// cut off mid-stream, which the transport cannot retry.
const (
	payloadAttempts   = 3
	payloadRetryDelay = 250 * time.Millisecond
)

func (s *Store) downloadPayload(ctx context.Context, remote Remote, sm *artifact.SchemaManifest) error {
	pr, pw := io.Pipe()
	done := make(chan struct{})

	go func() {
		defer close(done)

		pw.CloseWithError(remote.FetchTo(ctx, sm.Payload, pw))
	}()

	err := s.opts.Cache.WriteBlob(sm.Payload.Digest.String(), sm.Payload.Size, s.opts.ArtifactLimits.MaxPayloadBytes, pr)
	_ = pr.CloseWithError(errors.New("payload write finished"))

	<-done

	if err == nil {
		return nil
	}

	return cacheWriteError(err)
}

// interrupted reports a transfer that broke off mid-stream. Registry error
// responses (authentication, authorization, missing content) and integrity
// failures are final and never retried here.
func interrupted(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET)
}

func (s *Store) schema(entry *catalog.Entry, sm *artifact.SchemaManifest) *Schema {
	return &Schema{
		Notice:         sm.Notice,
		ID:             entry.ID,
		Path:           s.opts.Cache.SchemaPath(entry.Artifact.Digest),
		ManifestDigest: entry.Artifact.Digest,
		ContentDigest:  sm.ContentDigest,
		Size:           sm.ContentSize,
		Ref:            s.opts.Repository + "@" + entry.Artifact.Digest,
	}
}
