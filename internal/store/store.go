// Package store retrieves catalogs and schemas for the client: it serves them
// from the local cache when every byte verifies, and otherwise fetches exactly
// the missing artifacts from the configured repository. In offline mode the
// registry is never opened.
package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

// Remote is the subset of a registry repository the store needs.
type Remote interface {
	FetchManifestByDigest(ctx context.Context, dgst, mediaType string, limit int64) ([]byte, error)
	FetchTo(ctx context.Context, desc ocispec.Descriptor, w io.Writer) error
}

// Options configures a Store.
type Options struct {
	Cache *cache.Cache
	// Open connects to the repository. It is called at most once and never
	// in offline mode.
	Open           func() (Remote, error)
	Log            func(format string, args ...any)
	Repository     string
	ArtifactLimits artifact.Limits
	CatalogLimits  catalog.Limits
	Offline        bool
}

// Store resolves catalogs and schemas.
type Store struct {
	remote    Remote
	remoteErr error
	opts      Options
	opened    bool
}

// New returns a store.
func New(opts Options) *Store {
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}

	return &Store{opts: opts}
}

// LoadedCatalog is a verified catalog.
type LoadedCatalog struct {
	Catalog *catalog.Catalog
	Digest  string
	Raw     []byte
}

// Schema is a materialized, verified schema. Notice describes the notice
// layer of its artifact, nil when it has none; Store.Notice retrieves it.
type Schema struct {
	Notice         *ocispec.Descriptor
	ID             string
	Path           string
	ManifestDigest string
	ContentDigest  string
	Ref            string
	Size           int64
}

// corruptError marks a cache entry that exists but failed verification.
type corruptError struct {
	err    error
	digest string
}

func (e *corruptError) Error() string { return e.err.Error() }

func (e *corruptError) Unwrap() error { return e.err }

func corrupt(dgst string, err error) error {
	return &corruptError{digest: dgst, err: fmt.Errorf("cache entry %s: %w", dgst, err)}
}

// readLocal classifies a cache read: not found and corrupt entries are
// recoverable, anything else is final.
func readLocal(dgst string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, cache.ErrNotFound):
		return err
	case errors.Is(err, cache.ErrCorrupt):
		return corrupt(dgst, err)
	default:
		return fault.Wrap(fault.Internal, err, "read cache entry %s", dgst)
	}
}

// Catalog returns the verified catalog whose index has the given digest.
func (s *Store) Catalog(ctx context.Context, catalogDigest string) (*LoadedCatalog, error) {
	if err := digest.Validate(catalogDigest); err != nil {
		return nil, fault.Wrap(fault.Usage, err, "invalid catalog digest")
	}

	what := "catalog " + catalogDigest

	loaded, err := s.localCatalog(catalogDigest)
	if err == nil {
		return loaded, nil
	}

	if err := s.recoverable(err, what, false); err != nil {
		return nil, err
	}

	release, err := s.opts.Cache.Lock(ctx, digest.Hex(catalogDigest))
	if err != nil {
		return nil, lockError(err)
	}
	defer release()

	for range 3 {
		loaded, err = s.localCatalog(catalogDigest)
		if err == nil {
			return loaded, nil
		}

		if err := s.recoverable(err, what, true); err != nil {
			return nil, err
		}

		if _, corrupted := errors.AsType[*corruptError](err); !corrupted {
			break
		}
	}

	return s.fetchCatalog(ctx, catalogDigest)
}

// Materialize returns the verified schema.json of a catalog entry, fetching
// only what is missing. Once the manifest and schema.json verify, the payload
// blob is not needed and is not looked at: a missing or damaged one is
// repaired only when schema.json has to be rebuilt. Without network access a
// missing schema.json is rebuilt from verified cached blobs, but corrupt
// entries are never repaired.
func (s *Store) Materialize(ctx context.Context, entry *catalog.Entry) (*Schema, error) {
	desc := ocispec.Descriptor{MediaType: entry.Artifact.MediaType, Digest: godigest.Digest(entry.Artifact.Digest), Size: entry.Artifact.Size}

	sm, err := s.localManifest(desc)
	if err == nil {
		err = s.verifyMaterialized(entry, sm)
		if err == nil {
			return s.schema(entry, sm), nil
		}
	}

	if _, corrupted := errors.AsType[*corruptError](err); corrupted && s.opts.Offline {
		return nil, fault.Wrap(fault.Offline, err, "schema %q is corrupt in the local cache and --offline forbids fetching it again", entry.ID)
	}

	if !errors.Is(err, cache.ErrNotFound) {
		if _, corrupted := errors.AsType[*corruptError](err); !corrupted {
			return nil, err
		}
	}

	release, err := s.opts.Cache.Lock(ctx, digest.Hex(entry.Artifact.Digest))
	if err != nil {
		return nil, lockError(err)
	}
	defer release()

	return s.materializeLocked(ctx, entry, desc)
}

// ReadSchema returns the verified bytes of a materialized schema.
func (s *Store) ReadSchema(sch *Schema) ([]byte, error) {
	data, err := s.opts.Cache.ReadMaterialized(sch.ManifestDigest, sch.ContentDigest, sch.Size)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "read schema %q", sch.ID)
	}

	return data, nil
}

// Notice returns the verified notice text of a schema's artifact, or nil
// when it has none. The notice is not needed to materialize the schema, so
// it is read from the cache or fetched only on request; offline, a missing
// or corrupt notice is an error.
func (s *Store) Notice(ctx context.Context, sch *Schema) ([]byte, error) {
	if sch.Notice == nil {
		return nil, nil
	}

	desc := *sch.Notice
	what := fmt.Sprintf("notice %s of schema %q", desc.Digest, sch.ID)

	data, err := s.readDescribed(desc, "schema manifest "+sch.ManifestDigest)
	if err == nil {
		return data, nil
	}

	// A corrupt copy is replaced by WriteBlob below, which moves it aside
	// only while it is still the damaged file.
	if err := s.recoverable(err, what, false); err != nil {
		return nil, err
	}

	remote, err := s.open()
	if err != nil {
		return nil, err
	}

	s.opts.Log("fetching %s from %s", what, s.opts.Repository)

	var buf bytes.Buffer
	if err := remote.FetchTo(ctx, desc, &buf); err != nil {
		return nil, fault.Wrap(fault.Registry, err, "fetch %s", what)
	}

	if err := s.opts.Cache.WriteBlob(desc.Digest.String(), desc.Size, desc.Size, bytes.NewReader(buf.Bytes())); err != nil {
		return nil, cacheWriteError(err)
	}

	return buf.Bytes(), nil
}

func lockError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return fault.Wrap(fault.Canceled, err, "waiting for the cache lock")
	case errors.Is(err, context.DeadlineExceeded):
		return fault.Wrap(fault.Registry, err, "timed out waiting for the cache lock")
	default:
		return fault.Wrap(fault.Internal, err, "cache lock")
	}
}

func cacheWriteError(err error) error {
	if errors.Is(err, cache.ErrDigestMismatch) || errors.Is(err, cache.ErrCorrupt) {
		return fault.Wrap(fault.Integrity, err, "cache write")
	}

	return fault.Wrap(fault.Internal, err, "cache write")
}
