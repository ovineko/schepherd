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
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

func (s *Store) open() (Remote, error) {
	if s.opts.Offline {
		return nil, fault.New(fault.Offline, "network access is disabled (--offline)")
	}

	s.openMu.Lock()
	defer s.openMu.Unlock()

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
		return nil, fault.Wrap(fault.Integrity, err, "cached catalog index %s", catalogDigest)
	}

	metadata, err := s.readDescribed(ci.Metadata, "catalog index")
	if err != nil {
		return nil, err
	}

	cm, err := artifact.ParseCatalogMetadata(metadata, s.opts.ArtifactLimits)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "cached catalog metadata manifest %s", ci.Metadata.Digest)
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

	return &LoadedCatalog{Catalog: c, Digest: catalogDigest, Raw: raw, Stats: CatalogStats{CachedManifests: 2, CachedBlobs: 1}}, nil
}

// readDescribed reads the cached blob desc describes; parent names the
// verified content that declares it.
func (s *Store) readDescribed(desc ocispec.Descriptor, parent string) ([]byte, error) {
	dgst := desc.Digest.String()

	data, err := s.opts.Cache.ReadBlob(dgst, s.blobLimit(desc.MediaType))
	if err := readLocal(dgst, err); err != nil {
		return nil, err
	}

	if desc.Size >= 0 && int64(len(data)) != desc.Size {
		return nil, fault.Wrap(fault.Integrity, artifact.ErrInvalid, "cached blob %s has %d bytes, %s declares %d", dgst, len(data), parent, desc.Size)
	}

	return data, nil
}

func (s *Store) blobLimit(mediaType string) int64 {
	switch mediaType {
	case artifact.CatalogMediaType:
		return s.opts.ArtifactLimits.MaxCatalogBytes
	case artifact.SchemaMediaType, artifact.SchemaGzipMediaType:
		return s.opts.ArtifactLimits.MaxPayloadBytes
	case artifact.NoticeMediaType:
		return s.opts.ArtifactLimits.MaxNoticeBytes
	default:
		return s.opts.ArtifactLimits.MaxManifestBytes
	}
}

func (s *Store) fetchCatalog(ctx context.Context, catalogDigest string) (*LoadedCatalog, error) {
	index, indexOutcome, releaseIndex, err := s.catalogBlob(ctx, ocispec.Descriptor{
		MediaType: artifact.IndexMediaType, Digest: godigest.Digest(catalogDigest), Size: -1,
	}, true)
	if err != nil {
		return nil, err
	}
	if releaseIndex != nil {
		defer releaseIndex()
	}
	ci, err := artifact.ParseCatalogIndex(index.Data, s.opts.ArtifactLimits)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "catalog %s", catalogDigest)
	}

	metadata, metadataOutcome, releaseMetadata, err := s.catalogBlob(ctx, ci.Metadata, false)
	if err != nil {
		return nil, err
	}
	if releaseMetadata != nil {
		defer releaseMetadata()
	}
	cm, err := artifact.ParseCatalogMetadata(metadata.Data, s.opts.ArtifactLimits)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "catalog %s: metadata manifest %s", catalogDigest, ci.Metadata.Digest)
	}

	payload, payloadOutcome, releasePayload, err := s.catalogBlob(ctx, cm.Payload, false)
	if err != nil {
		return nil, err
	}
	if releasePayload != nil {
		defer releasePayload()
	}
	c, err := catalog.Parse(payload.Data, s.opts.CatalogLimits)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "catalog %s", catalogDigest)
	}
	if err := ci.CheckSchemas(c.Artifacts()); err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "catalog %s", catalogDigest)
	}

	stats := CatalogStats{}
	blobs := [3]artifact.Blob{index, metadata, payload}
	for i, outcome := range [3]EnsureOutcome{indexOutcome, metadataOutcome, payloadOutcome} {
		b := &blobs[i]
		if outcome.Fetched {
			if err := checkContext(ctx); err != nil {
				return nil, err
			}
			if err := s.opts.Cache.WriteBlob(b.Descriptor.Digest.String(), b.Descriptor.Size, s.blobLimit(b.Descriptor.MediaType), bytes.NewReader(b.Data)); err != nil {
				return nil, contextError(ctx, cacheWriteError(err))
			}
			stats.FetchedBytes += outcome.FetchedBytes
		}
		switch {
		case outcome.Fetched && i < 2:
			stats.FetchedManifests++
		case outcome.Fetched:
			stats.FetchedBlobs++
		case i < 2:
			stats.CachedManifests++
		default:
			stats.CachedBlobs++
		}
		if outcome.Repaired {
			stats.RepairedEntries++
		}
	}

	return &LoadedCatalog{Catalog: c, Digest: catalogDigest, Raw: payload.Data, Stats: stats}, nil
}

// A fetched blob stays locked until fetchCatalog validates and stores the
// whole graph: an index must not be cached before CheckSchemas succeeds.
func (s *Store) catalogBlob(ctx context.Context, desc ocispec.Descriptor, index bool) (artifact.Blob, EnsureOutcome, func(), error) {
	if err := checkContext(ctx); err != nil {
		return artifact.Blob{}, EnsureOutcome{}, nil, err
	}
	data, err := s.readDescribed(desc, "catalog manifest")
	if err == nil {
		desc.Size = int64(len(data))
		return artifact.Blob{Descriptor: desc, Data: data}, EnsureOutcome{}, nil, checkContext(ctx)
	}
	what := "catalog artifact " + desc.Digest.String()
	if err := s.recoverableForComplete(err, what); err != nil {
		return artifact.Blob{}, EnsureOutcome{}, nil, err
	}

	release, err := s.opts.Cache.Lock(ctx, digest.Hex(desc.Digest.String()))
	if err != nil {
		return artifact.Blob{}, EnsureOutcome{}, nil, lockError(err)
	}
	handedOff := false
	defer func() {
		if !handedOff {
			release()
		}
	}()

	data, err = s.readDescribed(desc, "catalog manifest")
	if err == nil {
		desc.Size = int64(len(data))
		return artifact.Blob{Descriptor: desc, Data: data}, EnsureOutcome{}, nil, checkContext(ctx)
	}
	if err := s.recoverableForComplete(err, what); err != nil {
		return artifact.Blob{}, EnsureOutcome{}, nil, err
	}
	_, repaired := errors.AsType[*corruptError](err)
	remote, err := s.open()
	if err != nil {
		return artifact.Blob{}, EnsureOutcome{}, nil, err
	}
	s.logf("fetching %s from %s", what, s.opts.Repository)
	if index {
		data, err = remote.FetchManifestByDigest(ctx, desc.Digest.String(), desc.MediaType, s.opts.ArtifactLimits.MaxManifestBytes)
	} else {
		var buf bytes.Buffer
		err = remote.FetchTo(ctx, desc, &buf)
		data = buf.Bytes()
	}
	if err != nil {
		return artifact.Blob{}, EnsureOutcome{}, nil, contextError(ctx, fault.Wrap(fault.Registry, err, "fetch catalog artifact %s", desc.Digest))
	}
	if err := checkContext(ctx); err != nil {
		return artifact.Blob{}, EnsureOutcome{}, nil, err
	}
	if digest.FromBytes(data) != desc.Digest.String() {
		return artifact.Blob{}, EnsureOutcome{}, nil, fault.Wrap(fault.Integrity, cache.ErrDigestMismatch, "catalog artifact %s has a different digest", desc.Digest)
	}
	if desc.Size >= 0 && int64(len(data)) != desc.Size {
		return artifact.Blob{}, EnsureOutcome{}, nil, fault.Wrap(fault.Integrity, artifact.ErrInvalid, "catalog artifact %s has %d bytes, expected %d", desc.Digest, len(data), desc.Size)
	}
	if index {
		desc.Size = int64(len(data))
	}
	handedOff = true
	outcome := EnsureOutcome{Fetched: true, Repaired: repaired, FetchedBytes: int64(len(data))}
	return artifact.Blob{Descriptor: desc, Data: data}, outcome, release, nil
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
	if err == nil {
		return nil
	}
	var what string
	if entry.ID == "" {
		what = "materialized schema " + entry.Artifact.Digest
	} else {
		what = fmt.Sprintf("materialized schema %q", entry.ID)
	}

	switch {
	case errors.Is(err, cache.ErrNotFound):
		return fmt.Errorf("%s: %w", what, err)
	case errors.Is(err, cache.ErrCorrupt):
		return &corruptError{digest: entry.Artifact.Digest, err: fmt.Errorf("%s: %w", what, err)}
	default:
		return fault.Wrap(fault.Internal, err, "verify %s", what)
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
	data, err := s.readDescribed(desc, "catalog")
	if err != nil {
		return nil, err
	}

	sm, err := artifact.ParseSchemaManifest(data, s.opts.ArtifactLimits)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "cached schema manifest %s", desc.Digest)
	}

	return sm, nil
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
