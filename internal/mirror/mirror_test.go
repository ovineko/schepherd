package mirror

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"testing/iotest"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

type countingStorage struct {
	fetches atomic.Int32
}

func (s *countingStorage) Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error) {
	s.fetches.Add(1)

	return io.NopCloser(bytes.NewReader([]byte("remote"))), nil
}

func (s *countingStorage) Exists(context.Context, ocispec.Descriptor) (bool, error) {
	return true, nil
}

func TestPrefetchedServesKnownBytesOnly(t *testing.T) {
	base := &countingStorage{}
	known := []byte("known")
	kd := godigest.FromBytes(known)
	p := prefetched{ReadOnlyStorage: base, data: map[godigest.Digest][]byte{kd: known}}

	read := func(desc ocispec.Descriptor) string {
		rc, err := p.Fetch(context.Background(), desc)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rc.Close() }()

		data, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}

		return string(data)
	}

	if got := read(ocispec.Descriptor{Digest: kd, Size: int64(len(known))}); got != "known" || base.fetches.Load() != 0 {
		t.Errorf("known descriptor = %q with %d remote fetches", got, base.fetches.Load())
	}

	mismatched, err := p.Fetch(context.Background(), ocispec.Descriptor{Digest: kd, Size: 99})
	if err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(mismatched)
	_ = mismatched.Close()

	if base.fetches.Load() != 1 {
		t.Errorf("a size mismatch must go to the source, got %q with %d remote fetches", got, base.fetches.Load())
	}

	if fault.KindOf(err) != fault.Integrity || !errors.Is(err, content.ErrInvalidDescriptorSize) {
		t.Errorf("source body that contradicts the descriptor = %q, %v; want an integrity error", got, err)
	}

	if got := read(ocispec.Descriptor{Digest: godigest.FromString("remote"), Size: 6}); got != "remote" {
		t.Errorf("unknown descriptor = %q", got)
	}

	if _, err := io.ReadAll(verified(io.NopCloser(bytes.NewReader([]byte("remote"))), ocispec.Descriptor{Digest: godigest.FromString("other"), Size: 6})); !errors.Is(err, content.ErrMismatchedDigest) {
		t.Errorf("a body with the declared size but another digest = %v, want a digest mismatch", err)
	}
}

// verifiedReader must not hand out the last byte of a body that fails
// verification, so an upload fed by it always stays short.
func TestVerifiedReaderWithholdsTheLastByteOfABadBody(t *testing.T) {
	good := []byte("0123456789abcdef")
	desc := ocispec.Descriptor{Digest: godigest.FromBytes(good), Size: int64(len(good))}

	cases := map[string]struct {
		body  []byte
		cause error
	}{
		"intact":    {body: good},
		"flipped":   {body: []byte("0123456789abcdeF"), cause: content.ErrMismatchedDigest},
		"truncated": {body: good[:10], cause: content.ErrInvalidDescriptorSize},
		"trailing":  {body: append(bytes.Clone(good), 'x'), cause: content.ErrTrailingData},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			for _, chunk := range []int{1, 3, len(good), 64} {
				v := verified(io.NopCloser(iotest.OneByteReader(bytes.NewReader(tc.body))), desc)

				var out bytes.Buffer

				buf := make([]byte, chunk)

				var err error

				for err == nil {
					var n int

					n, err = v.Read(buf)
					out.Write(buf[:n])
				}

				if tc.cause == nil {
					if !errors.Is(err, io.EOF) || !bytes.Equal(out.Bytes(), good) {
						t.Fatalf("chunk %d: got %q, %v", chunk, out.Bytes(), err)
					}

					continue
				}

				if !errors.Is(err, tc.cause) || fault.KindOf(err) != fault.Integrity {
					t.Fatalf("chunk %d: error = %v, want %v", chunk, err, tc.cause)
				}

				if int64(out.Len()) >= desc.Size {
					t.Fatalf("chunk %d: %d bytes handed out before the failure, want fewer than %d", chunk, out.Len(), desc.Size)
				}

				if n, again := v.Read(buf); n != 0 || !errors.Is(again, tc.cause) {
					t.Fatalf("chunk %d: read after the failure = %d, %v", chunk, n, again)
				}
			}
		})
	}
}

// ORAS only hands over descriptors that the artifact and catalog parsers
// accepted, but a malformed one must still fail as an integrity error: an
// unknown digest algorithm makes go-digest panic and a negative size would
// slice out of range.
func TestVerifiedReaderRefusesMalformedDescriptors(t *testing.T) {
	body := []byte("content")

	cases := map[string]struct {
		cause error
		desc  ocispec.Descriptor
	}{
		"negative size":     {cause: content.ErrInvalidDescriptorSize, desc: ocispec.Descriptor{Digest: godigest.FromBytes(body), Size: -1}},
		"unknown algorithm": {cause: digest.ErrUnsupportedAlgorithm, desc: ocispec.Descriptor{Digest: "md5:9a0364b9e99bb480dd25e1f0284c8555", Size: int64(len(body))}},
		"empty digest":      {cause: digest.ErrInvalid, desc: ocispec.Descriptor{Size: int64(len(body))}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := io.ReadAll(verified(io.NopCloser(bytes.NewReader(body)), tc.desc))
			if len(got) != 0 || fault.KindOf(err) != fault.Integrity || !errors.Is(err, tc.cause) {
				t.Fatalf("read of %+v = %q, %v; want an integrity error caused by %v and no bytes", tc.desc, got, err, tc.cause)
			}
		})
	}
}
