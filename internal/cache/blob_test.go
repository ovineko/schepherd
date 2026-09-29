package cache

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

func TestBlobRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		unknown bool
	}{
		{name: "empty", data: []byte{}},
		{name: "small", data: []byte(`{"type":"object"}`)},
		{name: "large", data: bytes.Repeat([]byte("0123456789abcdef"), 16*1024)},
		{name: "unknown size", data: []byte("payload"), unknown: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := openTestCache(t)
			dgst := digest.FromBytes(tt.data)

			size := int64(len(tt.data))
			if tt.unknown {
				size = -1
			}

			if err := c.WriteBlob(dgst, size, int64(len(tt.data)), bytes.NewReader(tt.data)); err != nil {
				t.Fatal(err)
			}

			got, err := c.ReadBlob(dgst, int64(len(tt.data)))
			if err != nil {
				t.Fatal(err)
			}

			if !bytes.Equal(got, tt.data) {
				t.Fatal("ReadBlob returned different bytes")
			}

			for _, verifySize := range []int64{int64(len(tt.data)), -1} {
				if err := c.VerifyBlob(dgst, verifySize); err != nil {
					t.Fatalf("VerifyBlob(size %d): %v", verifySize, err)
				}
			}

			rc, err := c.OpenBlob(dgst)
			if err != nil {
				t.Fatal(err)
			}

			streamed, err := io.ReadAll(rc)
			if err != nil {
				t.Fatal(err)
			}

			if err := rc.Close(); err != nil {
				t.Fatal(err)
			}

			if !bytes.Equal(streamed, tt.data) {
				t.Fatal("OpenBlob streamed different bytes")
			}

			assertNoTmp(t, c)
		})
	}
}

func TestBlobMissing(t *testing.T) {
	c := openTestCache(t)
	dgst := digest.FromBytes([]byte("absent"))

	_, err := c.ReadBlob(dgst, 10)
	assertIs(t, err, ErrNotFound)
	assertUnclassified(t, err)

	if !strings.Contains(err.Error(), dgst) || !strings.Contains(err.Error(), blobPath(c, dgst)) {
		t.Fatalf("error must name digest and path: %v", err)
	}

	assertIs(t, c.VerifyBlob(dgst, -1), ErrNotFound)

	_, err = c.OpenBlob(dgst)
	assertIs(t, err, ErrNotFound)

	if err := c.QuarantineBlob(dgst); err != nil {
		t.Fatalf("QuarantineBlob of a missing blob: %v", err)
	}
}

func TestBlobCorruptionDetected(t *testing.T) {
	data := []byte(`{"stored":"content"}`)
	dgst := digest.FromBytes(data)

	tests := []struct {
		tamper func(t *testing.T, path string)
		name   string
	}{
		{name: "same size other bytes", tamper: func(t *testing.T, path string) {
			t.Helper()
			writeOver(t, path, bytes.ToUpper(data))
		}},
		{name: "truncated", tamper: func(t *testing.T, path string) {
			t.Helper()
			writeOver(t, path, data[:5])
		}},
		{name: "extended", tamper: func(t *testing.T, path string) {
			t.Helper()
			writeOver(t, path, append(append([]byte{}, data...), '\n'))
		}},
		{name: "empty", tamper: func(t *testing.T, path string) {
			t.Helper()
			writeOver(t, path, nil)
		}},
		{name: "directory", tamper: func(t *testing.T, path string) {
			t.Helper()
			removePath(t, path)

			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := openTestCache(t)
			if err := c.WriteBlob(dgst, int64(len(data)), 1024, bytes.NewReader(data)); err != nil {
				t.Fatal(err)
			}

			tt.tamper(t, blobPath(c, dgst))

			_, err := c.ReadBlob(dgst, 1024)
			assertIs(t, err, ErrCorrupt)
			assertUnclassified(t, err)

			if !strings.Contains(err.Error(), dgst) || !strings.Contains(err.Error(), blobPath(c, dgst)) {
				t.Fatalf("error must name digest and path: %v", err)
			}

			assertIs(t, c.VerifyBlob(dgst, -1), ErrCorrupt)
			assertIs(t, c.VerifyBlob(dgst, int64(len(data))), ErrCorrupt)

			if rc, err := c.OpenBlob(dgst); err != nil {
				assertIs(t, err, ErrCorrupt)
			} else {
				_, err := io.ReadAll(rc)
				_ = rc.Close()
				assertIs(t, err, ErrCorrupt)
			}
		})
	}
}

func TestReadBlobLimit(t *testing.T) {
	c := openTestCache(t)
	data := []byte("0123456789")
	dgst := digest.FromBytes(data)

	if err := c.WriteBlob(dgst, 10, 10, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	_, err := c.ReadBlob(dgst, 9)
	assertIs(t, err, ErrCorrupt)

	if _, err := c.ReadBlob(dgst, 10); err != nil {
		t.Fatal(err)
	}

	assertIs(t, c.VerifyBlob(dgst, 11), ErrCorrupt)
}

func TestWriteBlobKeepsVerifiedCopy(t *testing.T) {
	c := openTestCache(t)
	data := []byte("kept")
	dgst := digest.FromBytes(data)

	if err := c.WriteBlob(dgst, 4, 4, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	first, err := os.Lstat(blobPath(c, dgst))
	if err != nil {
		t.Fatal(err)
	}

	if err := c.WriteBlob(dgst, -1, 4, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	second, err := os.Lstat(blobPath(c, dgst))
	if err != nil {
		t.Fatal(err)
	}

	if !os.SameFile(first, second) {
		t.Fatal("a verified blob must not be replaced")
	}

	assertEmptyDir(t, filepath.Join(c.Dir(), "v1", "quarantine"))
	assertNoTmp(t, c)
}

func TestWriteBlobQuarantinesCorruptCopy(t *testing.T) {
	c := openTestCache(t)
	data := []byte("genuine content")
	dgst := digest.FromBytes(data)

	if err := c.WriteBlob(dgst, -1, 100, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	writeOver(t, blobPath(c, dgst), []byte("tampered"))

	if err := c.WriteBlob(dgst, -1, 100, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	got, err := c.ReadBlob(dgst, 100)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("after rewrite: %q, %v", got, err)
	}

	quarantine := filepath.Join(c.Dir(), "v1", "quarantine")

	names := listDir(t, quarantine)
	if len(names) != 1 || !strings.HasPrefix(names[0], digest.Hex(dgst)+".") {
		t.Fatalf("quarantine holds %v", names)
	}

	kept, err := os.ReadFile(filepath.Join(quarantine, names[0]))
	if err != nil || string(kept) != "tampered" {
		t.Fatalf("quarantined content %q, %v", kept, err)
	}

	assertNoTmp(t, c)
}

func TestQuarantineBlob(t *testing.T) {
	c := openTestCache(t)
	data := []byte("to quarantine")
	dgst := digest.FromBytes(data)

	if err := c.WriteBlob(dgst, -1, 100, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	if err := c.QuarantineBlob(dgst); err != nil {
		t.Fatal(err)
	}

	assertAbsent(t, blobPath(c, dgst))

	if names := listDir(t, filepath.Join(c.Dir(), "v1", "quarantine")); len(names) != 1 {
		t.Fatalf("quarantine holds %v", names)
	}

	_, err := c.ReadBlob(dgst, 100)
	assertIs(t, err, ErrNotFound)
}

func TestWriteBlobFailuresLeaveNothing(t *testing.T) {
	data := []byte("the real content")
	dgst := digest.FromBytes(data)
	errSource := errors.New("connection reset")

	tests := []struct {
		src      io.Reader
		wantErr  error
		name     string
		dgst     string
		size     int64
		limit    int64
		wantKind fault.Kind
	}{
		{name: "digest mismatch", dgst: digest.FromBytes([]byte("other")), size: -1, limit: 100, src: bytes.NewReader(data), wantErr: ErrDigestMismatch, wantKind: fault.Integrity},
		{name: "shorter than declared", dgst: dgst, size: int64(len(data)) + 5, limit: 100, src: bytes.NewReader(data), wantErr: ErrDigestMismatch, wantKind: fault.Integrity},
		{name: "longer than declared", dgst: dgst, size: 4, limit: 100, src: bytes.NewReader(data), wantErr: ErrDigestMismatch, wantKind: fault.Integrity},
		{name: "over limit unknown size", dgst: dgst, size: -1, limit: 4, src: bytes.NewReader(data), wantErr: ErrTooLarge, wantKind: fault.Integrity},
		{name: "declared size over limit", dgst: dgst, size: int64(len(data)), limit: 4, src: bytes.NewReader(data), wantErr: ErrTooLarge, wantKind: fault.Integrity},
		{name: "source fails midway", dgst: dgst, size: -1, limit: 100, src: io.MultiReader(bytes.NewReader(data[:6]), iotest.ErrReader(errSource)), wantErr: errSource},
		{name: "source fails at once", dgst: dgst, size: int64(len(data)), limit: 100, src: iotest.ErrReader(errSource), wantErr: errSource},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := openTestCache(t)

			err := c.WriteBlob(tt.dgst, tt.size, tt.limit, tt.src)
			assertIs(t, err, tt.wantErr)

			if errors.Is(tt.wantErr, errSource) {
				assertUnclassified(t, err)
			} else {
				assertKind(t, err, tt.wantKind)
			}

			assertAbsent(t, blobPath(c, tt.dgst))
			assertNoTmp(t, c)
		})
	}
}

func TestWriteBlobReadsAtMostLimitPlusOne(t *testing.T) {
	c := openTestCache(t)
	src := &countingReader{}

	err := c.WriteBlob(digest.FromBytes(nil), -1, 1000, src)
	assertIs(t, err, ErrTooLarge)

	if src.n > 1001 {
		t.Fatalf("read %d bytes from an endless source with limit 1000", src.n)
	}

	assertNoTmp(t, c)
}

func TestInvalidArguments(t *testing.T) {
	c := openTestCache(t)
	valid := digest.FromBytes([]byte("x"))
	badDigests := []string{"", "sha256:ABC", "sha512:" + strings.Repeat("a", 128), "sha256:../../etc", digest.Hex(valid)}

	for _, bad := range badDigests {
		checks := map[string]error{
			"ReadBlob":   second(c.ReadBlob(bad, 1)),
			"VerifyBlob": c.VerifyBlob(bad, -1),
			"OpenBlob":   second(c.OpenBlob(bad)),
			"WriteBlob":  c.WriteBlob(bad, -1, 1, strings.NewReader("x")),
			"Quarantine": c.QuarantineBlob(bad),
			"VerifyMat":  c.VerifyMaterialized(bad, valid, 1),
			"VerifyMat2": c.VerifyMaterialized(valid, bad, 1),
			"ReadMat":    second(c.ReadMaterialized(bad, valid, 1)),
			"WriteMat":   second(c.WriteMaterialized(bad, valid, 1, func(io.Writer) error { return nil })),
			"WriteMat2":  second(c.WriteMaterialized(valid, bad, 1, func(io.Writer) error { return nil })),
			"QuarMat":    c.QuarantineMaterialized(bad),
		}

		for name, err := range checks {
			assertKind(t, err, fault.Internal)

			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrCorrupt) {
				t.Fatalf("%s(%q): invalid input reported as cache state: %v", name, bad, err)
			}
		}

		if got := c.SchemaPath(bad); got != "" {
			t.Fatalf("SchemaPath(%q) = %q, want empty", bad, got)
		}
	}

	sizeChecks := map[string]error{
		"negative read limit":   second(c.ReadBlob(valid, -1)),
		"verify size below -1":  c.VerifyBlob(valid, -2),
		"negative write limit":  c.WriteBlob(valid, -1, -1, strings.NewReader("x")),
		"write size below -1":   c.WriteBlob(valid, -2, 1, strings.NewReader("x")),
		"negative content size": c.VerifyMaterialized(valid, valid, -1),
	}

	for name, err := range sizeChecks {
		if err == nil {
			t.Fatalf("%s: want error", name)
		}

		assertKind(t, err, fault.Internal)
	}

	assertNoTmp(t, c)
}

func second[T any](_ T, err error) error {
	return err
}

type countingReader struct {
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}

	r.n += int64(len(p))

	return len(p), nil
}

func writeOver(t *testing.T, path string, data []byte) {
	t.Helper()
	makeWritable(t, path)

	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func removePath(t *testing.T, path string) {
	t.Helper()
	makeWritable(t, path)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}
