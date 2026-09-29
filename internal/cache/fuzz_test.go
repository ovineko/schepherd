package cache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

const fuzzMaxData = 64 << 10

func fuzzSize(n int, selector uint8) int64 {
	switch selector % 4 {
	case 0:
		return -1
	case 1:
		return int64(n)
	case 2:
		return int64(n) + 1
	default:
		return max(int64(n)-1, 0)
	}
}

func openFuzzCache(f *testing.F) *Cache {
	f.Helper()

	c, err := Open(filepath.Join(f.TempDir(), "cache"))
	if err != nil {
		f.Fatal(err)
	}

	f.Cleanup(func() { _ = c.Close() })

	return c
}

func FuzzWriteBlob(f *testing.F) {
	f.Add([]byte(`{"type":"object"}`), uint8(1), false, uint16(100))
	f.Add([]byte{}, uint8(0), false, uint16(0))
	f.Add([]byte("abc"), uint8(2), true, uint16(2))
	f.Add(bytes.Repeat([]byte{0xff}, 300), uint8(3), false, uint16(299))

	c := openFuzzCache(f)

	f.Fuzz(func(t *testing.T, data []byte, sizeSelector uint8, wrongDigest bool, limit uint16) {
		if len(data) > fuzzMaxData {
			return
		}

		claimed := digest.FromBytes(data)
		if wrongDigest {
			claimed = digest.FromBytes(append([]byte("wrong:"), data...))
		}

		size := fuzzSize(len(data), sizeSelector)
		lim := int64(limit)

		err := c.WriteBlob(claimed, size, lim, bytes.NewReader(data))

		accept := !wrongDigest && (size == -1 || size == int64(len(data))) && int64(len(data)) <= lim && size <= lim
		if accept {
			if err != nil {
				t.Fatalf("valid write refused: %v", err)
			}

			got, err := c.ReadBlob(claimed, lim)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("round trip: %v", err)
			}
		} else {
			assertKind(t, err, fault.Integrity)

			if !errors.Is(err, ErrDigestMismatch) && !errors.Is(err, ErrTooLarge) {
				t.Fatalf("refusal without a sentinel: %v", err)
			}

			if wrongDigest {
				assertAbsent(t, blobPath(c, claimed))
			}
		}

		assertNoTmp(t, c)
	})
}

func FuzzWriteMaterialized(f *testing.F) {
	f.Add([]byte(`{"type":"object"}`), uint8(1), false, uint8(3))
	f.Add([]byte{}, uint8(1), false, uint8(0))
	f.Add([]byte("abcdef"), uint8(2), true, uint8(1))
	f.Add([]byte("abcdef"), uint8(3), false, uint8(7))

	c := openFuzzCache(f)

	f.Fuzz(func(t *testing.T, data []byte, sizeSelector uint8, wrongDigest bool, chunk uint8) {
		if len(data) > fuzzMaxData {
			return
		}

		size := fuzzSize(len(data), sizeSelector)
		if size < 0 {
			size = int64(len(data))
		}

		content := digest.FromBytes(data)
		if wrongDigest {
			content = digest.FromBytes(append([]byte("wrong:"), data...))
		}

		valid := !wrongDigest && size == int64(len(data))

		// Failing cases use their own manifest so the absence check below
		// is not confused by a successful earlier input with the same data.
		prefix := "manifest-fail:"
		if valid {
			prefix = "manifest:"
		}

		manifest := digest.FromBytes(append([]byte(prefix), data...))
		step := int(chunk) + 1

		path, err := c.WriteMaterialized(manifest, content, size, func(w io.Writer) error {
			for start := 0; start < len(data); start += step {
				if _, err := w.Write(data[start:min(start+step, len(data))]); err != nil {
					return err
				}
			}

			return nil
		})

		if valid {
			if err != nil {
				t.Fatalf("valid write refused: %v", err)
			}

			got, err := c.ReadMaterialized(manifest, content, size)
			if err != nil || !bytes.Equal(got, data) || path != c.SchemaPath(manifest) {
				t.Fatalf("round trip: %v", err)
			}
		} else {
			assertIs(t, err, ErrDigestMismatch)
			assertKind(t, err, fault.Integrity)
			assertAbsent(t, materializedDirPath(c, manifest))
		}

		assertNoTmp(t, c)
	})
}

func FuzzLockKey(f *testing.F) {
	for _, seed := range []string{"a", "0123abcd", "a-b", "../x", "a/b", "A", "", "a.lock", strings.Repeat("a", 101)} {
		f.Add(seed)
	}

	c := openFuzzCache(f)

	f.Fuzz(func(t *testing.T, key string) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		release, err := c.Lock(ctx, key)
		if err != nil {
			assertKind(t, err, fault.Internal)

			if lockKeyPattern.MatchString(key) {
				t.Fatalf("valid key %q refused: %v", key, err)
			}

			return
		}

		release()

		name := key + ".lock"
		if !filepath.IsLocal(name) || filepath.Base(name) != name || strings.ContainsAny(key, `/\.:`) || len(key) > 100 {
			t.Fatalf("key %q was accepted but does not name a single file", key)
		}
	})
}
