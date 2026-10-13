package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

// EnsureOutcome counts only successful, verified work. A successful outcome
// with neither Fetched nor Written reused the cache; Repaired excludes misses.
// FetchedBytes excludes failed transfers and counts verified artifact bytes.
type EnsureOutcome struct {
	Fetched      bool
	Written      bool
	Repaired     bool
	FetchedBytes int64
}

// Offline reports whether the store never opens the registry.
func (s *Store) Offline() bool { return s.opts.Offline }

func (s *Store) logf(format string, args ...any) {
	s.opts.Log(format, args...)
}

// EnsureManifest fetches only after rechecking the cache under its manifest lock.
func (s *Store) EnsureManifest(ctx context.Context, entry *catalog.Entry, desc ocispec.Descriptor) (*artifact.SchemaManifest, EnsureOutcome, error) {
	if err := checkContext(ctx); err != nil {
		return nil, EnsureOutcome{}, err
	}

	sm, err := s.localManifest(desc)
	if err == nil {
		return sm, EnsureOutcome{}, checkContext(ctx)
	}

	if err := s.recoverableForComplete(err, "schema manifest "+desc.Digest.String()); err != nil {
		return nil, EnsureOutcome{}, err
	}

	release, err := s.opts.Cache.Lock(ctx, digest.Hex(desc.Digest.String()))
	if err != nil {
		return nil, EnsureOutcome{}, lockError(err)
	}
	defer release()

	sm, err = s.localManifest(desc)
	if err == nil {
		return sm, EnsureOutcome{}, checkContext(ctx)
	}
	if err := s.recoverableForComplete(err, "schema manifest "+desc.Digest.String()); err != nil {
		return nil, EnsureOutcome{}, err
	}
	_, repaired := errors.AsType[*corruptError](err)

	sm, err = s.fetchManifest(ctx, entry, desc)
	if err != nil {
		return nil, EnsureOutcome{}, contextError(ctx, err)
	}

	return sm, EnsureOutcome{Fetched: true, Written: true, Repaired: repaired, FetchedBytes: desc.Size}, checkContext(ctx)
}

// EnsurePayload requires the blob even when schema.json is already whole;
// Materialize keeps its lazier contract for path, cat and run.
func (s *Store) EnsurePayload(ctx context.Context, what string, sm *artifact.SchemaManifest) (EnsureOutcome, error) {
	dgst := sm.Payload.Digest.String()
	verify := func() error { return s.verifyDescribed(sm.Payload, what) }
	return s.ensureBlob(ctx, "payload "+dgst+" of "+what, sm.Payload, verify, func(remote Remote) error {
		for attempt := 1; ; attempt++ {
			err := s.downloadPayload(ctx, remote, sm)
			if err == nil {
				return nil
			}
			if attempt == payloadAttempts || !interrupted(err) || ctx.Err() != nil {
				return contextError(ctx, err)
			}
			s.logf("download of payload %s of %s failed (%v); retrying", dgst, what, err)
			select {
			case <-ctx.Done():
				return checkContext(ctx)
			case <-time.After(time.Duration(attempt) * payloadRetryDelay):
			}
		}
	})
}

// EnsureSchemaFile verifies without writing offline. Online reconstruction
// rechecks under the manifest lock and relies on guarded cache replacement.
func (s *Store) EnsureSchemaFile(ctx context.Context, entry *catalog.Entry, sm *artifact.SchemaManifest) (EnsureOutcome, error) {
	if err := checkContext(ctx); err != nil {
		return EnsureOutcome{}, err
	}
	var what string
	if entry.ID == "" {
		what = "schema " + entry.Artifact.Digest
	} else {
		what = fmt.Sprintf("schema %q", entry.ID)
	}
	err := s.verifyMaterialized(entry, sm)
	if err == nil {
		return EnsureOutcome{}, checkContext(ctx)
	}
	if err := s.recoverableForComplete(err, what); err != nil {
		return EnsureOutcome{}, err
	}

	release, err := s.opts.Cache.Lock(ctx, digest.Hex(entry.Artifact.Digest))
	if err != nil {
		return EnsureOutcome{}, lockError(err)
	}
	defer release()

	err = s.verifyMaterialized(entry, sm)
	if err == nil {
		return EnsureOutcome{}, checkContext(ctx)
	}
	if err := s.recoverableForComplete(err, what); err != nil {
		return EnsureOutcome{}, err
	}
	_, repaired := errors.AsType[*corruptError](err)

	_, werr := s.opts.Cache.WriteMaterialized(entry.Artifact.Digest, sm.ContentDigest, sm.ContentSize, func(w io.Writer) error {
		if err := checkContext(ctx); err != nil {
			return err
		}
		payload, perr := s.opts.Cache.OpenBlob(sm.Payload.Digest.String())
		if perr != nil {
			return fmt.Errorf("open payload: %w", perr)
		}
		defer func() { _ = payload.Close() }()

		return artifact.DecodeSchema(w, payload, sm)
	})
	if werr != nil {
		if errors.Is(werr, artifact.ErrInvalid) {
			return EnsureOutcome{}, fault.Wrap(fault.Integrity, werr, "%s", what)
		}
		return EnsureOutcome{}, contextError(ctx, cacheWriteError(werr))
	}

	return EnsureOutcome{Written: true, Repaired: repaired}, checkContext(ctx)
}

// EnsureNotice verifies the descriptor size as well as the notice digest.
func (s *Store) EnsureNotice(ctx context.Context, what string, desc ocispec.Descriptor) (EnsureOutcome, error) {
	dgst := desc.Digest.String()
	verify := func() error { return s.verifyDescribed(desc, what) }
	return s.ensureBlob(ctx, "notice "+dgst+" of "+what, desc, verify, func(remote Remote) error {
		var buf bytes.Buffer
		if err := remote.FetchTo(ctx, desc, &buf); err != nil {
			return contextError(ctx, fault.Wrap(fault.Registry, err, "fetch notice %s", desc.Digest))
		}
		if err := s.opts.Cache.WriteBlob(dgst, desc.Size, desc.Size, bytes.NewReader(buf.Bytes())); err != nil {
			return cacheWriteError(err)
		}
		return nil
	})
}

func (s *Store) verifyDescribed(desc ocispec.Descriptor, parent string) error {
	dgst := desc.Digest.String()
	reader, err := s.opts.Cache.OpenBlob(dgst)
	if err != nil {
		return readLocal(dgst, err)
	}
	defer func() { _ = reader.Close() }()

	maxSize := s.blobLimit(desc.MediaType)
	limit := maxSize
	if limit < math.MaxInt64 {
		limit++
	}
	size, err := io.Copy(io.Discard, io.LimitReader(reader, limit))
	if err := readLocal(dgst, err); err != nil {
		return err
	}
	if size > maxSize {
		return corrupt(dgst, fmt.Errorf("%w: size exceeds the limit of %d bytes", cache.ErrCorrupt, maxSize))
	}
	if size != desc.Size {
		return fault.Wrap(fault.Integrity, artifact.ErrInvalid, "cached blob %s has %d bytes, %s declares %d", dgst, size, parent, desc.Size)
	}
	return nil
}

func (s *Store) ensureBlob(ctx context.Context, what string, desc ocispec.Descriptor, verify func() error, fetch func(Remote) error) (EnsureOutcome, error) {
	if err := checkContext(ctx); err != nil {
		return EnsureOutcome{}, err
	}
	err := verify()
	if err == nil {
		return EnsureOutcome{}, checkContext(ctx)
	}
	if err := s.recoverableForComplete(err, what); err != nil {
		return EnsureOutcome{}, err
	}

	release, err := s.opts.Cache.Lock(ctx, digest.Hex(desc.Digest.String()))
	if err != nil {
		return EnsureOutcome{}, lockError(err)
	}
	defer release()

	err = verify()
	if err == nil {
		return EnsureOutcome{}, checkContext(ctx)
	}
	if err := s.recoverableForComplete(err, what); err != nil {
		return EnsureOutcome{}, err
	}
	_, repaired := errors.AsType[*corruptError](err)
	remote, err := s.open()
	if err != nil {
		return EnsureOutcome{}, err
	}
	s.logf("fetching %s from %s", what, s.opts.Repository)
	if err := fetch(remote); err != nil {
		return EnsureOutcome{}, contextError(ctx, err)
	}
	return EnsureOutcome{Fetched: true, Written: true, Repaired: repaired, FetchedBytes: desc.Size}, checkContext(ctx)
}

func (s *Store) recoverableForComplete(err error, what string) error {
	if err == nil {
		return nil
	}
	_, corrupted := errors.AsType[*corruptError](err)
	if !corrupted && !errors.Is(err, cache.ErrNotFound) {
		return err
	}
	if s.opts.Offline {
		if corrupted {
			return fault.Wrap(fault.Offline, err, "%s is corrupt in the local cache and --offline forbids fetching it again", what)
		}
		return fault.Wrap(fault.Offline, err, "%s is not in the local cache and --offline forbids fetching it", what)
	}
	if corrupted {
		s.logf("%s is corrupt in the local cache (%v); fetching it again", what, err)
	}
	return nil
}

func checkContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		kind := fault.Registry
		if errors.Is(err, context.Canceled) {
			kind = fault.Canceled
		}
		return fault.Wrap(kind, err, "cache operation interrupted")
	}
	return nil
}

func contextError(ctx context.Context, err error) error {
	if interrupted := checkContext(ctx); interrupted != nil {
		return interrupted
	}
	return err
}
