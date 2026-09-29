package cache

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

var (
	testSchema   = []byte(`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object"}`)
	testManifest = digest.FromBytes([]byte("manifest bytes"))
	testContent  = digest.FromBytes(testSchema)
)

func writeAll(data []byte) func(io.Writer) error {
	return func(w io.Writer) error {
		_, err := w.Write(data)

		return err
	}
}

// writeChunks writes data in small pieces with pauses so concurrent readers
// get a chance to observe a partially written file if one were visible.
func writeChunks(data []byte, chunk int, pause time.Duration) func(io.Writer) error {
	return func(w io.Writer) error {
		for start := 0; start < len(data); start += chunk {
			end := min(start+chunk, len(data))
			if _, err := w.Write(data[start:end]); err != nil {
				return err
			}

			time.Sleep(pause)
		}

		return nil
	}
}

func TestMaterializedRoundTrip(t *testing.T) {
	c := openTestCache(t)
	size := int64(len(testSchema))

	path, err := c.WriteMaterialized(testManifest, testContent, size, writeAll(testSchema))
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(c.Dir(), "v1", "materialized", "sha256", digest.Hex(testManifest), "schema.json")
	if path != want || c.SchemaPath(testManifest) != want {
		t.Fatalf("path %q, SchemaPath %q, want %q", path, c.SchemaPath(testManifest), want)
	}

	if err := c.VerifyMaterialized(testManifest, testContent, size); err != nil {
		t.Fatal(err)
	}

	got, err := c.ReadMaterialized(testManifest, testContent, size)
	if err != nil || !bytes.Equal(got, testSchema) {
		t.Fatalf("ReadMaterialized: %q, %v", got, err)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(onDisk, testSchema) {
		t.Fatalf("file content: %q, %v", onDisk, err)
	}

	assertNoTmp(t, c)
}

func TestMaterializedEmptySchema(t *testing.T) {
	c := openTestCache(t)
	empty := digest.FromBytes(nil)

	if _, err := c.WriteMaterialized(testManifest, empty, 0, func(io.Writer) error { return nil }); err != nil {
		t.Fatal(err)
	}

	if err := c.VerifyMaterialized(testManifest, empty, 0); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaPathStable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	first := openCacheAt(t, dir)
	second := openCacheAt(t, dir)

	if first.SchemaPath(testManifest) != second.SchemaPath(testManifest) {
		t.Fatal("SchemaPath differs between handles of the same root")
	}

	other := digest.FromBytes([]byte("another manifest"))
	if first.SchemaPath(testManifest) == first.SchemaPath(other) {
		t.Fatal("different manifests share a path")
	}

	if _, err := os.Lstat(first.SchemaPath(testManifest)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("SchemaPath must not create anything: %v", err)
	}
}

func TestMaterializedMissing(t *testing.T) {
	c := openTestCache(t)
	size := int64(len(testSchema))

	err := c.VerifyMaterialized(testManifest, testContent, size)
	assertIs(t, err, ErrNotFound)
	assertUnclassified(t, err)

	_, err = c.ReadMaterialized(testManifest, testContent, size)
	assertIs(t, err, ErrNotFound)

	if err := os.MkdirAll(materializedDirPath(c, testManifest), 0o755); err != nil {
		t.Fatal(err)
	}

	assertIs(t, c.VerifyMaterialized(testManifest, testContent, size), ErrNotFound)

	if err := c.QuarantineMaterialized(digest.FromBytes([]byte("absent"))); err != nil {
		t.Fatalf("QuarantineMaterialized of a missing entry: %v", err)
	}
}

func TestMaterializedCorruptionDetected(t *testing.T) {
	size := int64(len(testSchema))

	tests := []struct {
		tamper  func(t *testing.T, c *Cache)
		name    string
		content string
		size    int64
	}{
		{name: "partially written", tamper: func(t *testing.T, c *Cache) {
			t.Helper()
			writeOver(t, c.SchemaPath(testManifest), testSchema[:len(testSchema)/2])
		}},
		{name: "same size other bytes", tamper: func(t *testing.T, c *Cache) {
			t.Helper()
			writeOver(t, c.SchemaPath(testManifest), bytes.ToUpper(testSchema))
		}},
		{name: "trailing bytes", tamper: func(t *testing.T, c *Cache) {
			t.Helper()
			writeOver(t, c.SchemaPath(testManifest), append(append([]byte{}, testSchema...), ' '))
		}},
		{name: "schema.json is a directory", tamper: func(t *testing.T, c *Cache) {
			t.Helper()
			removePath(t, c.SchemaPath(testManifest))

			if err := os.Mkdir(c.SchemaPath(testManifest), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "directory is a file", tamper: func(t *testing.T, c *Cache) {
			t.Helper()

			dir := materializedDirPath(c, testManifest)
			removePath(t, c.SchemaPath(testManifest))

			if err := os.Remove(dir); err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(dir, testSchema, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "other expected content", content: digest.FromBytes([]byte("{}"))},
		{name: "other expected size", size: size + 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := openTestCache(t)
			if _, err := c.WriteMaterialized(testManifest, testContent, size, writeAll(testSchema)); err != nil {
				t.Fatal(err)
			}

			if tt.tamper != nil {
				tt.tamper(t, c)
			}

			wantContent, wantSize := testContent, size
			if tt.content != "" {
				wantContent = tt.content
			}

			if tt.size != 0 {
				wantSize = tt.size
			}

			err := c.VerifyMaterialized(testManifest, wantContent, wantSize)
			assertIs(t, err, ErrCorrupt)
			assertUnclassified(t, err)

			if !strings.Contains(err.Error(), testManifest) {
				t.Fatalf("error must name the manifest digest: %v", err)
			}

			_, err = c.ReadMaterialized(testManifest, wantContent, wantSize)
			assertIs(t, err, ErrCorrupt)
		})
	}
}

func TestWriteMaterializedKeepsVerifiedCopy(t *testing.T) {
	c := openTestCache(t)
	size := int64(len(testSchema))

	path, err := c.WriteMaterialized(testManifest, testContent, size, writeAll(testSchema))
	if err != nil {
		t.Fatal(err)
	}

	first, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.WriteMaterialized(testManifest, testContent, size, writeAll(testSchema)); err != nil {
		t.Fatal(err)
	}

	second, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	if !os.SameFile(first, second) {
		t.Fatal("a verified schema.json must not be replaced")
	}

	assertEmptyDir(t, filepath.Join(c.Dir(), "v1", "quarantine"))
	assertNoTmp(t, c)
}

func TestWriteMaterializedQuarantinesPartialFile(t *testing.T) {
	c := openTestCache(t)
	size := int64(len(testSchema))
	partial := testSchema[:10]

	if err := os.MkdirAll(materializedDirPath(c, testManifest), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(c.SchemaPath(testManifest), partial, 0o644); err != nil {
		t.Fatal(err)
	}

	dirBefore, err := os.Lstat(materializedDirPath(c, testManifest))
	if err != nil {
		t.Fatal(err)
	}

	assertIs(t, c.VerifyMaterialized(testManifest, testContent, size), ErrCorrupt)

	if _, err := c.WriteMaterialized(testManifest, testContent, size, writeAll(testSchema)); err != nil {
		t.Fatal(err)
	}

	if err := c.VerifyMaterialized(testManifest, testContent, size); err != nil {
		t.Fatal(err)
	}

	dirAfter, err := os.Lstat(materializedDirPath(c, testManifest))
	if err != nil || !os.SameFile(dirBefore, dirAfter) {
		t.Fatalf("an intact materialized directory must stay in place: %v", err)
	}

	quarantine := filepath.Join(c.Dir(), "v1", "quarantine")

	names := listDir(t, quarantine)
	if len(names) != 1 || !strings.HasPrefix(names[0], digest.Hex(testManifest)+".schema.") {
		t.Fatalf("quarantine holds %v", names)
	}

	kept, err := os.ReadFile(filepath.Join(quarantine, names[0]))
	if err != nil || !bytes.Equal(kept, partial) {
		t.Fatalf("quarantined schema: %q, %v", kept, err)
	}

	assertNoTmp(t, c)
}

func TestQuarantineMaterialized(t *testing.T) {
	c := openTestCache(t)
	size := int64(len(testSchema))

	if _, err := c.WriteMaterialized(testManifest, testContent, size, writeAll(testSchema)); err != nil {
		t.Fatal(err)
	}

	if err := c.QuarantineMaterialized(testManifest); err != nil {
		t.Fatal(err)
	}

	assertAbsent(t, materializedDirPath(c, testManifest))
	assertIs(t, c.VerifyMaterialized(testManifest, testContent, size), ErrNotFound)
}

func TestWriteMaterializedFailuresLeaveNothing(t *testing.T) {
	size := int64(len(testSchema))
	errFill := errors.New("decompression failed")

	tests := []struct {
		fill     func(io.Writer) error
		wantErr  error
		name     string
		wantKind fault.Kind
	}{
		{
			name: "fill fails after partial write",
			fill: func(w io.Writer) error {
				if _, err := w.Write(testSchema[:5]); err != nil {
					return err
				}

				return errFill
			},
			wantErr: errFill,
		},
		{name: "too short", fill: writeAll(testSchema[:len(testSchema)-1]), wantErr: ErrDigestMismatch, wantKind: fault.Integrity},
		{name: "too long", fill: writeAll(append(append([]byte{}, testSchema...), '\n')), wantErr: ErrDigestMismatch, wantKind: fault.Integrity},
		{name: "wrong content", fill: writeAll(bytes.ToUpper(testSchema)), wantErr: ErrDigestMismatch, wantKind: fault.Integrity},
		{
			name: "fill ignores the overflow error",
			fill: func(w io.Writer) error {
				_, _ = w.Write(testSchema)
				_, _ = w.Write([]byte("extra"))

				return nil
			},
			wantErr:  ErrDigestMismatch,
			wantKind: fault.Integrity,
		},
		{
			name: "fill wraps the overflow error",
			fill: func(w io.Writer) error {
				_, err := w.Write(append(append([]byte{}, testSchema...), testSchema...))

				return fault.Wrap(fault.Internal, err, "decode")
			},
			wantErr:  ErrDigestMismatch,
			wantKind: fault.Integrity,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := openTestCache(t)

			path, err := c.WriteMaterialized(testManifest, testContent, size, tt.fill)
			if path != "" {
				t.Fatalf("path %q returned on failure", path)
			}

			assertIs(t, err, tt.wantErr)

			if errors.Is(tt.wantErr, errFill) {
				assertUnclassified(t, err)
			} else {
				assertKind(t, err, tt.wantKind)
			}

			assertAbsent(t, materializedDirPath(c, testManifest))
			assertNoTmp(t, c)
		})
	}
}

func TestWriteMaterializedConcurrentWriters(t *testing.T) {
	c := openTestCache(t)
	content := bytes.Repeat([]byte(`{"k":"0123456789"},`), 400)
	contentDigest := digest.FromBytes(content)
	size := int64(len(content))
	path := c.SchemaPath(testManifest)

	done := make(chan struct{})
	readerErrs := make(chan error, 4)

	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() { readerErrs <- watchForPartialReads(path, done, content) })
	}

	var writers sync.WaitGroup

	errs := make(chan error, 16)
	for i := range 16 {
		handle := c
		if i%2 == 1 {
			handle = openCacheAt(t, c.Dir())
		}

		writers.Go(func() {
			_, err := handle.WriteMaterialized(testManifest, contentDigest, size, writeChunks(content, 512, time.Millisecond))
			errs <- err
		})
	}

	writers.Wait()
	close(done)
	readers.Wait()
	close(errs)
	close(readerErrs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	for err := range readerErrs {
		if err != nil {
			t.Fatal(err)
		}
	}

	if err := c.VerifyMaterialized(testManifest, contentDigest, size); err != nil {
		t.Fatal(err)
	}

	assertEmptyDir(t, filepath.Join(c.Dir(), "v1", "quarantine"))
	assertNoTmp(t, c)
}

func TestConcurrentWritersRepairCorruptEntries(t *testing.T) {
	size := int64(len(testSchema))
	garbage := []byte("garbage")

	for range 50 {
		c := openTestCache(t)
		plantFile(t, c.SchemaPath(testManifest), garbage)
		plantFile(t, blobPath(c, testContent), garbage)

		start := make(chan struct{})
		errs := make(chan error, 8)

		var writers sync.WaitGroup
		for i := range 8 {
			writers.Go(func() {
				<-start

				if i%2 == 0 {
					_, err := c.WriteMaterialized(testManifest, testContent, size, writeAll(testSchema))
					errs <- err

					return
				}

				errs <- c.WriteBlob(testContent, size, size, bytes.NewReader(testSchema))
			})
		}

		close(start)
		writers.Wait()
		close(errs)

		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}

		if err := c.VerifyMaterialized(testManifest, testContent, size); err != nil {
			t.Fatal(err)
		}

		if err := c.VerifyBlob(testContent, size); err != nil {
			t.Fatal(err)
		}

		assertQuarantined(t, c, garbage, 2, testSchema)
		assertNoTmp(t, c)
	}
}

func plantFile(t *testing.T, path string, data []byte) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertQuarantined tolerates verified copies next to the corrupt ones: an
// unlocked writer that loses a repair race on Windows cannot put a copy it
// moved aside back over a read-only file.
func assertQuarantined(t *testing.T, c *Cache, corrupt []byte, times int, verified []byte) {
	t.Helper()

	quarantine := filepath.Join(c.Dir(), "v1", "quarantine")
	found := 0

	for _, name := range listDir(t, quarantine) {
		data, err := os.ReadFile(filepath.Join(quarantine, name))

		switch {
		case err != nil:
			t.Fatalf("quarantined %s is not a readable file: %v", name, err)
		case bytes.Equal(data, corrupt):
			found++
		case !bytes.Equal(data, verified):
			t.Fatalf("quarantined %s holds %q", name, data)
		}
	}

	if found != times {
		t.Fatalf("corrupt content quarantined %d times, want %d", found, times)
	}
}

func watchForPartialReads(path string, done <-chan struct{}, accepted ...[]byte) error {
	for {
		select {
		case <-done:
			return nil
		default:
		}

		data, err := readShared(path)

		switch {
		// A writer replacing the file makes Windows refuse the open for a
		// moment; a reader then sees no file, never a partial one.
		case errors.Is(err, fs.ErrNotExist), transient(err):
		case err != nil:
			return err
		case !slices.ContainsFunc(accepted, func(want []byte) bool { return bytes.Equal(data, want) }):
			return fmt.Errorf("reader observed %d bytes that are none of the accepted contents", len(data))
		}
	}
}
