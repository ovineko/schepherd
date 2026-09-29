package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

// Repo is an opened repository. Every error it returns is classified with
// Classify, so callers can map it to an exit code directly.
type Repo struct {
	target           *remote.Repository
	name             Repository
	maxManifestBytes int64
}

// Name returns the repository this Repo was opened for.
func (r *Repo) Name() Repository {
	return r.name
}

// Target exposes the underlying ORAS repository for graph copies. Content
// fetched through it directly is not verified by this package.
func (r *Repo) Target() *remote.Repository {
	return r.target
}

// FetchManifestByDigest fetches the manifest (an OCI image manifest or
// index, per mediaType) and returns its bytes only after checking them
// against dgst. A limit that is not positive means the client's
// MaxManifestBytes. Oversized bodies, digest mismatches and other media
// types are fault.Integrity errors.
func (r *Repo) FetchManifestByDigest(ctx context.Context, dgst, mediaType string, limit int64) ([]byte, error) {
	op := fmt.Sprintf("fetch manifest %s from %s", dgst, r.name)

	if err := digest.Validate(dgst); err != nil {
		return nil, fault.Wrap(fault.Usage, err, "%s", op)
	}

	if limit <= 0 {
		limit = r.maxManifestBytes
	}

	desc, rc, err := r.target.FetchReference(ctx, dgst)
	if err != nil {
		return nil, r.classify(err, op)
	}
	defer func() { _ = rc.Close() }()

	if desc.MediaType != mediaType {
		return nil, fault.New(fault.Integrity, "%s: media type is %q, want %q", op, desc.MediaType, mediaType)
	}

	if desc.Size > limit {
		return nil, fault.New(fault.Integrity, "%s: manifest size %d exceeds the limit of %d bytes", op, desc.Size, limit)
	}

	body, err := io.ReadAll(io.LimitReader(rc, min(limit, math.MaxInt64-1)+1))
	if err != nil {
		return nil, r.classify(err, op)
	}

	if int64(len(body)) > limit {
		return nil, fault.New(fault.Integrity, "%s: manifest exceeds the limit of %d bytes", op, limit)
	}

	if got := digest.FromBytes(body); got != dgst {
		return nil, fault.New(fault.Integrity, "%s: received content has digest %s", op, got)
	}

	return body, nil
}

// FetchTo streams the content described by desc into w and verifies its size
// and digest. A mismatch is a fault.Integrity error, a truncated or failed
// transfer a fault.Registry error. w may have received unverified bytes when
// an error is returned, so callers write to a temporary location.
func (r *Repo) FetchTo(ctx context.Context, desc ocispec.Descriptor, w io.Writer) error {
	op := fmt.Sprintf("fetch %s from %s", desc.Digest, r.name)

	if err := validateDescriptor(desc); err != nil {
		return fault.Wrap(fault.Integrity, err, "%s", op)
	}

	rc, err := r.target.Fetch(ctx, desc)
	if err != nil {
		return r.classify(err, op)
	}
	defer func() { _ = rc.Close() }()

	verifier := content.NewVerifyReader(rc, desc)
	sink := &errorRecordingWriter{w: w}

	if _, err := io.Copy(sink, verifier); err != nil {
		if sink.err != nil {
			return fault.Wrap(fault.Internal, sink.err, "%s: write", op)
		}

		return r.classify(err, op)
	}

	if err := verifier.Verify(); err != nil {
		return r.classify(err, op)
	}

	return nil
}

// Exists reports whether the manifest or blob described by desc is present.
func (r *Repo) Exists(ctx context.Context, desc ocispec.Descriptor) (bool, error) {
	ok, err := r.target.Exists(ctx, desc)
	if err != nil {
		return false, r.classify(err, fmt.Sprintf("check %s in %s", desc.Digest, r.name))
	}

	return ok, nil
}

// Resolve returns the manifest descriptor tag points to. When the tag does
// not exist, IsNotFound reports true for the returned error. The descriptor
// comes from registry headers; fetch it with FetchManifestByDigest to get
// verified bytes.
func (r *Repo) Resolve(ctx context.Context, tag string) (ocispec.Descriptor, error) {
	op := fmt.Sprintf("resolve tag %q in %s", tag, r.name)

	if err := validateTag(r.name, tag); err != nil {
		return ocispec.Descriptor{}, fault.Wrap(fault.Usage, err, "%s", op)
	}

	desc, err := r.target.Resolve(ctx, tag)
	if err != nil {
		return ocispec.Descriptor{}, r.classify(err, op)
	}

	if err := digest.Validate(desc.Digest.String()); err != nil {
		return ocispec.Descriptor{}, fault.Wrap(fault.Integrity, err, "%s: registry returned an unusable digest", op)
	}

	return desc, nil
}

// PushBlob uploads data as the blob desc unless the registry already has it.
// pushed is false when nothing was uploaded.
func (r *Repo) PushBlob(ctx context.Context, desc ocispec.Descriptor, data []byte) (bool, error) {
	op := fmt.Sprintf("push blob %s to %s", desc.Digest, r.name)

	return r.push(ctx, r.target.Blobs(), desc, data, op)
}

// PushManifest uploads data as the manifest desc (by digest, without a tag)
// unless the registry already has it. pushed is false when nothing was
// uploaded.
func (r *Repo) PushManifest(ctx context.Context, desc ocispec.Descriptor, data []byte) (bool, error) {
	op := fmt.Sprintf("push manifest %s to %s", desc.Digest, r.name)

	return r.push(ctx, r.target.Manifests(), desc, data, op)
}

// Tag points tag at the manifest desc, moving it if it already exists.
func (r *Repo) Tag(ctx context.Context, desc ocispec.Descriptor, tag string) error {
	op := fmt.Sprintf("tag %s as %q in %s", desc.Digest, tag, r.name)

	if err := validateTag(r.name, tag); err != nil {
		return fault.Wrap(fault.Usage, err, "%s", op)
	}

	if err := r.target.Tag(ctx, desc, tag); err != nil {
		return r.classify(err, op)
	}

	return nil
}

// Tags lists all tags in lexical order. A repository that does not exist yet
// has no tags.
func (r *Repo) Tags(ctx context.Context) ([]string, error) {
	tags := []string{}
	listed := false

	err := r.target.Tags(ctx, "", func(page []string) error {
		tags = append(tags, page...)
		listed = true

		return nil
	})
	if err != nil {
		if !listed && r.isMissingRepository(err) {
			return []string{}, nil
		}

		return nil, r.classify(err, "list tags of "+r.name.String())
	}

	slices.Sort(tags)

	return tags, nil
}

// EnsureTag makes tag point to desc without ever moving an existing tag.
// created is true when the tag was added; an existing tag with another
// digest is a fault.Integrity error.
func (r *Repo) EnsureTag(ctx context.Context, desc ocispec.Descriptor, tag string) (bool, error) {
	current, err := r.Resolve(ctx, tag)

	switch {
	case err == nil:
		if current.Digest == desc.Digest {
			return false, nil
		}

		return false, fault.New(fault.Integrity, "%s: tag %s already points to %s; refusing to move it to %s",
			r.name, tag, current.Digest, desc.Digest)
	case IsNotFound(err):
		if err := r.Tag(ctx, desc, tag); err != nil {
			return false, err
		}

		return true, nil
	default:
		return false, err
	}
}

type pushTarget interface {
	Exists(ctx context.Context, target ocispec.Descriptor) (bool, error)
	Push(ctx context.Context, expected ocispec.Descriptor, content io.Reader) error
}

func (r *Repo) push(ctx context.Context, store pushTarget, desc ocispec.Descriptor, data []byte, op string) (bool, error) {
	if err := validateDescriptor(desc); err != nil {
		return false, fault.Wrap(fault.Integrity, err, "%s", op)
	}

	if int64(len(data)) != desc.Size || digest.FromBytes(data) != desc.Digest.String() {
		return false, fault.New(fault.Integrity, "%s: data does not match the descriptor", op)
	}

	exists, err := store.Exists(ctx, desc)
	if err != nil {
		return false, r.classify(err, op)
	}

	if exists {
		return false, nil
	}

	if err := store.Push(ctx, desc, bytes.NewReader(data)); err != nil {
		if errors.Is(err, errdef.ErrAlreadyExists) {
			return false, nil
		}

		return false, r.classify(err, op)
	}

	return true, nil
}

func (r *Repo) classify(err error, op string) error {
	return classify(err, op, r.name.Host)
}

func validateDescriptor(desc ocispec.Descriptor) error {
	if err := digest.Validate(desc.Digest.String()); err != nil {
		return fmt.Errorf("descriptor: %w", err)
	}

	if desc.Size < 0 {
		return fmt.Errorf("descriptor: negative size %d", desc.Size)
	}

	return nil
}

// isMissingRepository reports whether err is the registry saying that this
// repository does not exist. Only an answer for the repository's own tag list
// counts; the same status from the bearer token service or any other URL says
// nothing about the repository.
func (r *Repo) isMissingRepository(err error) bool {
	resp, ok := errors.AsType[*errcode.ErrorResponse](err)
	if !ok || resp.URL == nil {
		return false
	}

	ref := r.target.Reference
	if resp.URL.Host != ref.Host() || resp.URL.Path != "/v2/"+ref.Repository+"/tags/list" {
		return false
	}

	return resp.StatusCode == http.StatusNotFound || slices.ContainsFunc(resp.Errors, func(e errcode.Error) bool {
		return e.Code == errcode.ErrorCodeNameUnknown
	})
}

// errorRecordingWriter tells writer failures apart from read failures inside
// io.Copy: the former are local problems, not registry ones.
type errorRecordingWriter struct {
	w   io.Writer
	err error
}

func (e *errorRecordingWriter) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}

	if err != nil {
		e.err = err
	}

	return n, err
}
