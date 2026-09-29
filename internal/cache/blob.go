package cache

import (
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

func blobLabel(dgst string) string {
	return "cached blob " + dgst
}

func blobName(dgst string) (string, error) {
	if err := digest.Validate(dgst); err != nil {
		return "", fault.Wrap(fault.Internal, err, "cache blob")
	}

	return filepath.Join(blobsDir, digest.Hex(dgst)), nil
}

// ReadBlob returns the verified bytes of a blob. A blob that is not a regular
// file, is larger than maxSize or does not hash to dgst is reported as
// ErrCorrupt; it is not quarantined automatically.
func (c *Cache) ReadBlob(dgst string, maxSize int64) ([]byte, error) {
	name, err := blobName(dgst)
	if err != nil {
		return nil, err
	}

	if maxSize < 0 {
		return nil, fault.New(fault.Internal, "read %s: negative size limit %d", blobLabel(dgst), maxSize)
	}

	return c.verifyFile(blobLabel(dgst), name, dgst, -1, maxSize, true)
}

// VerifyBlob hashes a stored blob without keeping its content. size -1 means
// the size is not known.
func (c *Cache) VerifyBlob(dgst string, size int64) error {
	name, err := blobName(dgst)
	if err != nil {
		return err
	}

	if size < -1 {
		return fault.New(fault.Internal, "verify %s: invalid size %d", blobLabel(dgst), size)
	}

	_, err = c.verifyFile(blobLabel(dgst), name, dgst, size, math.MaxInt64, false)

	return err
}

// OpenBlob opens a stored blob for streaming. The reader hashes what it
// delivers and, instead of io.EOF, returns an error wrapping ErrCorrupt when
// the content does not match dgst, so callers never act on unverified bytes
// without being told.
func (c *Cache) OpenBlob(dgst string) (io.ReadCloser, error) {
	name, err := blobName(dgst)
	if err != nil {
		return nil, err
	}

	label := blobLabel(dgst)

	f, info, err := c.openRegular(label, name)
	if err != nil {
		return nil, err
	}

	return &verifyingReader{f: f, h: digest.NewHash(), label: label, path: c.path(name), want: dgst, size: info.Size()}, nil
}

// WriteBlob stores the content read from src under dgst. It reads at most
// limit+1 bytes; size is the expected length or -1 when unknown. Content
// whose size or digest differs is refused with ErrDigestMismatch, content
// beyond limit with ErrTooLarge, and in both cases nothing is stored. An
// existing verified copy is kept; a corrupt one is quarantined and replaced.
// Errors from src are returned wrapped but unclassified.
func (c *Cache) WriteBlob(dgst string, size, limit int64, src io.Reader) error {
	name, err := blobName(dgst)
	if err != nil {
		return err
	}

	label := blobLabel(dgst)

	if limit < 0 || size < -1 {
		return fault.New(fault.Internal, "store %s: invalid size %d or limit %d", label, size, limit)
	}

	if size > limit {
		return fault.Wrap(fault.Integrity, ErrTooLarge, "%s: declared size %d exceeds the limit of %d bytes", label, size, limit)
	}

	accept := limit
	if size >= 0 {
		accept = size
	}

	tmp, err := c.stage("blob", accept)
	if err != nil {
		return err
	}

	defer tmp.discard()

	w := tmp.w
	if _, err := io.Copy(w, io.LimitReader(src, plusOne(limit))); err != nil {
		switch {
		case w.overflow:
			return blobOverflow(label, size, limit)
		case w.writeErr != nil:
			return fault.Wrap(fault.Internal, w.writeErr, "store %s", label)
		default:
			return fmt.Errorf("receive %s: %w", label, err)
		}
	}

	if size >= 0 && w.n != size {
		return mismatchf(label, "received %d bytes, expected %d", w.n, size)
	}

	if got := digest.FromHash(w.h); got != dgst {
		return mismatchf(label, "received content with digest %s", got)
	}

	if err := tmp.seal(); err != nil {
		return err
	}

	return tmp.install(slot{
		label:   label,
		parent:  blobsDir,
		name:    name,
		suspect: func() (string, string) { return name, digest.Hex(dgst) },
		verify: func() error {
			_, err := c.verifyFile(label, name, dgst, size, math.MaxInt64, false)

			return err
		},
	})
}

func blobOverflow(label string, size, limit int64) error {
	if size >= 0 {
		return mismatchf(label, "received more than the expected %d bytes", size)
	}

	return fault.Wrap(fault.Integrity, ErrTooLarge, "%s: content exceeds the limit of %d bytes", label, limit)
}

// QuarantineBlob moves a stored blob to v1/quarantine so it is refetched on
// next use. A missing blob is not an error.
func (c *Cache) QuarantineBlob(dgst string) error {
	name, err := blobName(dgst)
	if err != nil {
		return err
	}

	return c.quarantine(name, blobLabel(dgst), digest.Hex(dgst), nil)
}

type verifyingReader struct {
	f     *os.File
	h     hash.Hash
	label string
	path  string
	want  string
	n     int64
	size  int64
}

func (r *verifyingReader) Read(p []byte) (int, error) {
	n, err := r.f.Read(p)
	r.h.Write(p[:n])
	r.n += int64(n)

	if r.n > r.size {
		return n, corruptf(r.label, r.path, "changed size while being read")
	}

	if errors.Is(err, io.EOF) {
		if r.n != r.size {
			return n, corruptf(r.label, r.path, "changed size while being read")
		}

		if got := digest.FromHash(r.h); got != r.want {
			return n, corruptf(r.label, r.path, "content digest is %s", got)
		}

		return n, io.EOF
	}

	if err != nil {
		return n, fault.Wrap(fault.Internal, err, "read %s", r.label)
	}

	return n, nil
}

func (r *verifyingReader) Close() error {
	if err := r.f.Close(); err != nil {
		return fault.Wrap(fault.Internal, err, "close %s", r.label)
	}

	return nil
}
