package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/cache"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

// Offline reports whether the store never opens the registry.
func (s *Store) Offline() bool { return s.opts.Offline }

// log emits one store message; concurrent workers serialize through logMu so
// lines never interleave mid-write.
func (s *Store) logf(format string, args ...any) {
	s.logMu.Lock()
	defer s.logMu.Unlock()

	s.opts.Log(format, args...)
}

// EnsureManifest returns the verified schema manifest of a catalog entry,
// fetching it when missing. The bool reports whether it was fetched this
// call (false: verified from the cache).
func (s *Store) EnsureManifest(ctx context.Context, entry *catalog.Entry, desc ocispec.Descriptor) (*artifact.SchemaManifest, bool, error) {
	sm, err := s.localManifest(desc)
	if err == nil {
		return sm, false, nil
	}

	if err := s.recoverableForComplete(err, "schema manifest "+desc.Digest.String()); err != nil {
		return nil, false, err
	}

	release, err := s.opts.Cache.Lock(ctx, digestHex(desc.Digest.String()))
	if err != nil {
		return nil, false, lockError(err)
	}
	defer release()

	sm, err = s.fetchManifest(ctx, entry, desc)
	if err != nil {
		return nil, false, err
	}

	return sm, true, nil
}

// EnsurePayload verifies the payload blob of a schema manifest and downloads
// it when absent or corrupt — even when schema.json is already whole. This
// is the completeness definition of "materialize"; Store.Materialize keeps
// its own, lazier contract for the other commands.
func (s *Store) EnsurePayload(ctx context.Context, what string, sm *artifact.SchemaManifest) error {
	dgst := sm.Payload.Digest.String()

	err := readLocal(dgst, s.opts.Cache.VerifyBlob(dgst, sm.Payload.Size))
	if err == nil {
		return nil
	}

	if err := s.recoverableForComplete(err, "payload "+dgst+" of "+what); err != nil {
		return err
	}

	remote, err := s.open()
	if err != nil {
		return err
	}

	s.logf("fetching payload %s of %s from %s", dgst, what, s.opts.Repository)

	for attempt := 1; ; attempt++ {
		err = s.downloadPayload(ctx, remote, sm)
		if err == nil {
			return nil
		}

		if attempt == payloadAttempts || !interrupted(err) || ctx.Err() != nil {
			return err
		}

		s.logf("download of payload %s of %s failed (%v); retrying", dgst, what, err)

		select {
		case <-ctx.Done():
			return fault.Wrap(fault.Registry, ctx.Err(), "download payload %s", dgst)
		case <-time.After(time.Duration(attempt) * payloadRetryDelay):
		}
	}
}

// EnsureSchemaFile verifies the materialized schema.json of a catalog entry
// and rebuilds it from the (already ensured) payload when missing. The bool
// reports whether it was written this call.
func (s *Store) EnsureSchemaFile(entry *catalog.Entry, sm *artifact.SchemaManifest) (bool, error) {
	err := s.verifyMaterialized(entry, sm)
	if err == nil {
		return false, nil
	}

	if !errors.Is(err, cache.ErrNotFound) {
		if _, corrupted := errors.AsType[*corruptError](err); !corrupted {
			return false, err
		}

		if s.opts.Offline {
			return false, fault.Wrap(fault.Offline, err, "schema %q is corrupt in the local cache and --offline forbids fetching it again", entry.ID)
		}

		s.logf("schema %q is corrupt in the local cache (%v); rebuilding it", entry.ID, err)

		if qerr := s.opts.Cache.QuarantineMaterialized(entry.Artifact.Digest); qerr != nil && !errors.Is(qerr, cache.ErrNotFound) {
			return false, fault.Wrap(fault.Internal, qerr, "quarantine schema %q", entry.ID)
		}
	}

	_, werr := s.opts.Cache.WriteMaterialized(entry.Artifact.Digest, sm.ContentDigest, sm.ContentSize, func(w io.Writer) error {
		payload, perr := s.opts.Cache.OpenBlob(sm.Payload.Digest.String())
		if perr != nil {
			return fmt.Errorf("open payload: %w", perr)
		}
		defer func() { _ = payload.Close() }()

		return artifact.DecodeSchema(w, payload, sm)
	})
	if werr != nil {
		if errors.Is(werr, artifact.ErrInvalid) {
			return false, fault.Wrap(fault.Integrity, werr, "schema %q", entry.ID)
		}

		return false, cacheWriteError(werr)
	}

	return true, nil
}

// EnsureNotice verifies the notice blob of a schema and fetches it when
// missing. The bool reports whether it was fetched this call.
func (s *Store) EnsureNotice(ctx context.Context, what string, desc ocispec.Descriptor) (bool, error) {
	rerr := readLocal(desc.Digest.String(), s.opts.Cache.VerifyBlob(desc.Digest.String(), desc.Size))
	if rerr == nil {
		return false, nil
	}

	if err := s.recoverableForComplete(rerr, "notice "+desc.Digest.String()+" of "+what); err != nil {
		return false, err
	}

	remote, err := s.open()
	if err != nil {
		return false, err
	}

	s.logf("fetching notice %s of %s from %s", desc.Digest, what, s.opts.Repository)

	var buf bytes.Buffer
	if err := remote.FetchTo(ctx, desc, &buf); err != nil {
		return false, fault.Wrap(fault.Registry, err, "fetch notice %s", desc.Digest)
	}

	if err := s.opts.Cache.WriteBlob(desc.Digest.String(), desc.Size, desc.Size, bytes.NewReader(buf.Bytes())); err != nil {
		return false, cacheWriteError(err)
	}

	return true, nil
}

// recoverableForComplete classifies a failed local read for the completeness
// flow: offline, an absent or corrupt entry is the offline refusal (the run
// collects it); online a corrupt entry is recovered by fetching again.
func (s *Store) recoverableForComplete(err error, what string) error {
	if err == nil {
		return nil
	}

	if s.opts.Offline {
		return fault.Wrap(fault.Offline, err, "%s is not in the local cache and --offline forbids fetching it", what)
	}

	if _, corrupted := errors.AsType[*corruptError](err); corrupted {
		s.logf("%s is corrupt in the local cache (%v); fetching it again", what, err)

		return nil
	}

	if errors.Is(err, cache.ErrNotFound) {
		return nil
	}

	return err
}

func digestHex(dgst string) string { return digest.Hex(dgst) }
