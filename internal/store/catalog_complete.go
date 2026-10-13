package store

import (
	"context"
	"errors"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

// CatalogForCompleteness retains the verified index's schema descriptors when
// offline catalog artifacts are absent or corrupt, without repairing the cache.
// Online loads keep Catalog's complete, fail-fast contract.
func (s *Store) CatalogForCompleteness(ctx context.Context, catalogDigest string) (*LoadedCatalog, error) {
	if !s.opts.Offline {
		return s.Catalog(ctx, catalogDigest)
	}
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if err := digest.Validate(catalogDigest); err != nil {
		return nil, fault.Wrap(fault.Usage, err, "invalid catalog digest")
	}

	loaded := &LoadedCatalog{Digest: catalogDigest}
	index, err := s.opts.Cache.ReadBlob(catalogDigest, s.opts.ArtifactLimits.MaxManifestBytes)
	if err := readLocal(catalogDigest, err); err != nil {
		return s.incompleteCatalog(ctx, loaded, "catalogIndex", catalogDigest, err)
	}
	ci, err := artifact.ParseCatalogIndex(index, s.opts.ArtifactLimits)
	if err != nil {
		return nil, contextError(ctx, fault.Wrap(fault.Integrity, err, "cached catalog index %s", catalogDigest))
	}
	loaded.SchemaDescriptors = ci.Schemas
	loaded.Stats.CachedManifests++
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	metadataDigest := ci.Metadata.Digest.String()
	metadata, err := s.readDescribed(ci.Metadata, "catalog index")
	if err != nil {
		return s.incompleteCatalog(ctx, loaded, "catalogMetadata", metadataDigest, err)
	}
	cm, err := artifact.ParseCatalogMetadata(metadata, s.opts.ArtifactLimits)
	if err != nil {
		return nil, contextError(ctx, fault.Wrap(fault.Integrity, err, "cached catalog metadata manifest %s", metadataDigest))
	}
	loaded.Stats.CachedManifests++
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	payloadDigest := cm.Payload.Digest.String()
	raw, err := s.readDescribed(cm.Payload, "catalog metadata manifest")
	if err != nil {
		return s.incompleteCatalog(ctx, loaded, "catalogPayload", payloadDigest, err)
	}
	c, err := catalog.Parse(raw, s.opts.CatalogLimits)
	if err != nil {
		return nil, contextError(ctx, fault.Wrap(fault.Integrity, err, "cached catalog %s", catalogDigest))
	}
	if err := ci.CheckSchemas(c.Artifacts()); err != nil {
		return nil, contextError(ctx, fault.Wrap(fault.Integrity, err, "cached catalog %s", catalogDigest))
	}
	loaded.Catalog = c
	loaded.Raw = raw
	loaded.Stats.CachedBlobs++
	return loaded, checkContext(ctx)
}

func (s *Store) incompleteCatalog(ctx context.Context, loaded *LoadedCatalog, kind, dgst string, err error) (*LoadedCatalog, error) {
	if interrupted := checkContext(ctx); interrupted != nil {
		return nil, interrupted
	}
	err = s.recoverableForComplete(err, kind+" "+dgst)
	if fault.KindOf(err) != fault.Offline || (!errors.Is(err, cache.ErrNotFound) && !errors.Is(err, cache.ErrCorrupt)) {
		return nil, err
	}
	loaded.CatalogProblems = append(loaded.CatalogProblems, CatalogProblem{Kind: kind, Digest: dgst, Err: err})
	return loaded, checkContext(ctx)
}
