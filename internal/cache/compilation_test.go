package cache

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

func compilationMetadataPath(t *testing.T, c *Cache, key string) string {
	t.Helper()
	e, err := newCompilationEntry(key)
	if err != nil {
		t.Fatal(err)
	}
	return c.path(e.name)
}

func replaceCompilationFile(t *testing.T, name string, data []byte) {
	t.Helper()
	makeWritable(t, name)
	if err := os.WriteFile(name, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCompilationRecords(t *testing.T) {
	for _, tt := range []struct {
		name     string
		reason   string
		artifact []byte
	}{
		{name: "compiled", artifact: []byte("module.exports = function validate() { return true; };\n")},
		{name: "empty artifact", artifact: []byte{}},
		{name: "compatible probe"},
		{name: "incompatible", reason: "unsupported dialect"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := openTestCache(t)
			key := digest.FromBytes([]byte(tt.name))
			_, err := c.LoadCompilation(key)
			assertIs(t, err, ErrNotFound)
			assertUnclassified(t, err)
			saved, err := c.SaveCompilation(key, tt.reason, tt.artifact)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := c.LoadCompilation(key)
			if err != nil || loaded != saved || loaded.Reason != tt.reason {
				t.Fatalf("load=%+v, save=%+v, err=%v", loaded, saved, err)
			}
			metadataPath := compilationMetadataPath(t, c, key)
			assertReadOnly(t, metadataPath)
			metadata, err := os.ReadFile(metadataPath)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(metadata, []byte(c.Dir())) || bytes.Contains(metadata, []byte("path")) {
				t.Fatalf("metadata contains a machine path: %s", metadata)
			}
			if tt.artifact != nil {
				if loaded.Path != blobPath(c, digest.FromBytes(tt.artifact)) {
					t.Fatalf("unexpected artifact path %q", loaded.Path)
				}
				data, err := os.ReadFile(loaded.Path)
				if err != nil || !bytes.Equal(data, tt.artifact) {
					t.Fatalf("artifact=%q, err=%v", data, err)
				}
				assertReadOnly(t, loaded.Path)
			} else if loaded.Path != "" {
				t.Fatalf("probe has an artifact path: %q", loaded.Path)
			}
			before := readOnlySnapshot(t, c.Dir())
			if _, err := c.SaveCompilation(key, tt.reason, tt.artifact); err != nil {
				t.Fatal(err)
			}
			if after := readOnlySnapshot(t, c.Dir()); !reflect.DeepEqual(before, after) {
				t.Fatal("repeated save changed cache")
			}
			assertNoTmp(t, c)
		})
	}
}

func TestCompilationCorruptMetadata(t *testing.T) {
	key := digest.FromBytes([]byte("recipe"))
	valid := fmt.Sprintf(`{"key":%q,"reason":""}`, key)
	for _, tt := range []struct {
		name string
		data string
	}{
		{name: "truncated", data: valid[:len(valid)-1]},
		{name: "trailing value", data: valid + "{}"},
		{name: "duplicate key", data: fmt.Sprintf(`{"key":%q,"key":%q,"reason":""}`, key, key)},
		{name: "unknown field", data: fmt.Sprintf(`{"key":%q,"reason":"","path":"relative.js"}`, key)},
		{name: "wrong recipe", data: fmt.Sprintf(`{"key":%q,"reason":""}`, digest.FromBytes([]byte("other")))},
		{name: "invalid recipe", data: `{"key":"../../outside","reason":""}`},
		{name: "missing recipe", data: `{"reason":""}`},
		{name: "missing reason", data: fmt.Sprintf(`{"key":%q}`, key)},
		{name: "null reason", data: fmt.Sprintf(`{"key":%q,"reason":null}`, key)},
		{name: "null record", data: "null"},
		{name: "wrong reason type", data: fmt.Sprintf(`{"key":%q,"reason":true}`, key)},
		{name: "invalid UTF8", data: fmt.Sprintf("{\"key\":%q,\"reason\":\"\xff\"}", key)},
		{name: "oversized", data: strings.Repeat(" ", maxCompilationMetadata+1)},
		{name: "invalid artifact digest", data: fmt.Sprintf(`{"key":%q,"reason":"","artifact":{"digest":"../../outside","size":1}}`, key)},
		{name: "negative artifact size", data: fmt.Sprintf(`{"key":%q,"reason":"","artifact":{"digest":%q,"size":-1}}`, key, key)},
		{name: "missing artifact size", data: fmt.Sprintf(`{"key":%q,"reason":"","artifact":{"digest":%q}}`, key, key)},
		{name: "oversized artifact", data: fmt.Sprintf(`{"key":%q,"reason":"","artifact":{"digest":%q,"size":%d}}`, key, key, MaxCompiledBytes+1)},
		{name: "unknown artifact field", data: fmt.Sprintf(`{"key":%q,"reason":"","artifact":{"digest":%q,"size":0,"path":"template.js"}}`, key, key)},
		{name: "incompatible artifact", data: fmt.Sprintf(`{"key":%q,"reason":"unsupported","artifact":{"digest":%q,"size":0}}`, key, key)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := openTestCache(t)
			if _, err := c.SaveCompilation(key, "", nil); err != nil {
				t.Fatal(err)
			}
			replaceCompilationFile(t, compilationMetadataPath(t, c, key), []byte(tt.data))
			before := readOnlySnapshot(t, c.Dir())
			result, err := c.LoadCompilation(key)
			assertIs(t, err, ErrCorrupt)
			assertUnclassified(t, err)
			if result != (Compilation{}) {
				t.Fatalf("corrupt record returned a usable result: %+v", result)
			}
			if after := readOnlySnapshot(t, c.Dir()); !reflect.DeepEqual(before, after) {
				t.Fatal("load changed corrupt metadata")
			}
			result, err = c.SaveCompilation(key, "rejected", nil)
			if err != nil || result != (Compilation{Reason: "rejected"}) {
				t.Fatalf("repair=%+v, err=%v", result, err)
			}
			assertNoTmp(t, c)
		})
	}
}

func TestCompilationArtifactRefusalAndRepair(t *testing.T) {
	artifact := []byte("trusted compiler output")
	key := digest.FromBytes([]byte("recipe"))
	for _, mode := range []string{"missing", "digest mismatch", "truncated", "size mismatch", "directory"} {
		t.Run(mode, func(t *testing.T) {
			c := openTestCache(t)
			saved, err := c.SaveCompilation(key, "", artifact)
			if err != nil {
				t.Fatal(err)
			}
			want := ErrCorrupt
			switch mode {
			case "missing", "directory":
				makeWritable(t, saved.Path)
				if err := os.Remove(saved.Path); err != nil {
					t.Fatal(err)
				}
				if mode == "missing" {
					want = ErrNotFound
				} else if err := os.Mkdir(saved.Path, 0o755); err != nil {
					t.Fatal(err)
				}
			case "digest mismatch":
				replaceCompilationFile(t, saved.Path, bytes.Repeat([]byte("x"), len(artifact)))
			case "truncated":
				replaceCompilationFile(t, saved.Path, artifact[:len(artifact)-1])
			case "size mismatch":
				replaceCompilationFile(t, compilationMetadataPath(t, c, key), []byte(fmt.Sprintf(`{"key":%q,"reason":"","artifact":{"digest":%q,"size":0}}`, key, digest.FromBytes(artifact))))
			}
			before := readOnlySnapshot(t, c.Dir())
			result, err := c.LoadCompilation(key)
			assertIs(t, err, want)
			assertUnclassified(t, err)
			if result != (Compilation{}) {
				t.Fatalf("unverified artifact returned: %+v", result)
			}
			if after := readOnlySnapshot(t, c.Dir()); !reflect.DeepEqual(before, after) {
				t.Fatal("load changed corrupt artifact")
			}
			repaired, err := c.SaveCompilation(key, "", artifact)
			if err != nil || repaired != saved {
				t.Fatalf("repair=%+v, want=%+v, err=%v", repaired, saved, err)
			}
			assertNoTmp(t, c)
		})
	}
}

func TestCompilationConcurrentPublication(t *testing.T) {
	c := openTestCache(t)
	key := digest.FromBytes([]byte("concurrent recipe"))
	artifact := bytes.Repeat([]byte("compiler output\n"), 4096)
	const writers = 12
	start := make(chan struct{})
	results := make(chan error, writers)
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			<-start
			_, err := c.SaveCompilation(key, "", artifact)
			results <- err
		})
	}
	close(start)
	for {
		_, err := c.LoadCompilation(key)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("reader observed partial publication: %v", err)
		}
		runtime.Gosched()
		if len(results) == writers {
			break
		}
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := c.LoadCompilation(key)
	if err != nil || loaded.Path != blobPath(c, digest.FromBytes(artifact)) {
		t.Fatalf("load=%+v, err=%v", loaded, err)
	}
	assertNoTmp(t, c)
	assertEmptyDir(t, filepath.Join(c.Dir(), quarantineDir))
}

func TestCompilationFailedPublication(t *testing.T) {
	c := openTestCache(t)
	key := digest.FromBytes([]byte("recipe"))
	if err := os.Remove(c.path(tmpDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.path(tmpDir), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := c.SaveCompilation(key, "", []byte("compiler output"))
	assertKind(t, err, fault.Internal)
	_, err = c.LoadCompilation(key)
	assertIs(t, err, ErrNotFound)
	assertAbsent(t, compilationMetadataPath(t, c, key))
}

func TestCompilationRelocatedReadOnly(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	c := openCacheAt(t, source)
	artifact := []byte("portable compiler output")
	key := digest.FromBytes([]byte("compiled"))
	negativeKey := digest.FromBytes([]byte("incompatible"))
	probeKey := digest.FromBytes([]byte("probe"))
	for _, entry := range []struct {
		key      string
		reason   string
		artifact []byte
	}{
		{key: key, artifact: artifact},
		{key: negativeKey, reason: "not supported"},
		{key: probeKey},
	} {
		if _, err := c.SaveCompilation(entry.key, entry.reason, entry.artifact); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "relocated")
	if err := os.CopyFS(destination, os.DirFS(source)); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{locksDir, tmpDir, quarantineDir} {
		if err := os.Remove(filepath.Join(destination, rel)); err != nil {
			t.Fatal(err)
		}
	}
	before := readOnlySnapshot(t, destination)
	ro := openReadOnlyAt(t, destination)
	loaded, err := ro.LoadCompilation(key)
	if err != nil || loaded.Path != blobPath(ro, digest.FromBytes(artifact)) || strings.HasPrefix(loaded.Path, source) {
		t.Fatalf("relocated load=%+v, err=%v", loaded, err)
	}
	negative, err := ro.LoadCompilation(negativeKey)
	if err != nil || negative != (Compilation{Reason: "not supported"}) {
		t.Fatalf("negative load=%+v, err=%v", negative, err)
	}
	probe, err := ro.LoadCompilation(probeKey)
	if err != nil || probe != (Compilation{}) {
		t.Fatalf("probe load=%+v, err=%v", probe, err)
	}
	for _, data := range [][]byte{nil, artifact} {
		result, err := ro.SaveCompilation(key, "", data)
		assertReadOnlyError(t, err)
		if result != (Compilation{}) {
			t.Fatalf("read-only save returned %+v", result)
		}
	}
	if after := readOnlySnapshot(t, destination); !reflect.DeepEqual(before, after) {
		t.Fatal("read-only access mutated relocated cache")
	}
}

func TestCompilationAbsentReadOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	c := openReadOnlyAt(t, dir)
	key := digest.FromBytes([]byte("recipe"))
	_, err := c.LoadCompilation(key)
	assertIs(t, err, ErrNotFound)
	_, err = c.SaveCompilation(key, "", nil)
	assertReadOnlyError(t, err)
	assertAbsent(t, dir)
}

func TestCompilationInvalidRequests(t *testing.T) {
	c := openTestCache(t)
	before := readOnlySnapshot(t, c.Dir())
	_, err := c.LoadCompilation("../../outside")
	assertKind(t, err, fault.Internal)
	_, err = c.SaveCompilation("../../outside", "", nil)
	assertKind(t, err, fault.Internal)
	_, err = c.SaveCompilation(digest.FromBytes([]byte("recipe")), "unsupported", []byte{})
	assertKind(t, err, fault.Internal)
	_, err = c.SaveCompilation(digest.FromBytes([]byte("recipe")), strings.Repeat("x", maxCompilationMetadata), nil)
	assertIs(t, err, ErrTooLarge)
	assertKind(t, err, fault.Integrity)
	_, err = c.SaveCompilation(digest.FromBytes([]byte("recipe")), "", make([]byte, MaxCompiledBytes+1))
	assertIs(t, err, ErrTooLarge)
	assertKind(t, err, fault.Integrity)
	if after := readOnlySnapshot(t, c.Dir()); !reflect.DeepEqual(before, after) {
		t.Fatal("invalid request mutated cache")
	}
}

func TestCompilationSymlinkContainment(t *testing.T) {
	if runtime.GOOS == "windows" {
		return // Windows symlinks require privileges unavailable to the test suite.
	}
	artifact := []byte("compiler output")
	key := digest.FromBytes([]byte("recipe"))
	for _, target := range []string{"compiled", "algorithm", "recipe", "metadata", "blobs", "artifact"} {
		for _, outside := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/outside=%t", target, outside), func(t *testing.T) {
				c := openTestCache(t)
				loaded, err := c.SaveCompilation(key, "", artifact)
				if err != nil {
					t.Fatal(err)
				}
				metadata := compilationMetadataPath(t, c, key)
				name := metadata
				switch target {
				case "compiled":
					name = c.path(filepath.Dir(compiledDir))
				case "algorithm":
					name = c.path(compiledDir)
				case "recipe":
					name = filepath.Dir(metadata)
				case "blobs":
					name = c.path(blobsDir)
				case "artifact":
					name = loaded.Path
				}
				victimRoot := filepath.Join(c.Dir(), "victim")
				if outside {
					victimRoot = t.TempDir()
				} else if err := os.Mkdir(victimRoot, 0o755); err != nil {
					t.Fatal(err)
				}
				victim := filepath.Join(victimRoot, "original")
				if err := os.Rename(name, victim); err != nil {
					t.Fatal(err)
				}
				linkTarget, err := filepath.Rel(filepath.Dir(name), victim)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(linkTarget, name); err != nil {
					t.Fatal(err)
				}
				before := readOnlySnapshot(t, victimRoot)
				result, err := c.LoadCompilation(key)
				assertIs(t, err, ErrCorrupt)
				assertUnclassified(t, err)
				if result != (Compilation{}) {
					t.Fatalf("linked entry returned %+v", result)
				}
				_, saveErr := c.SaveCompilation(key, "", artifact)
				if target == "compiled" || target == "algorithm" || target == "blobs" {
					assertIs(t, saveErr, ErrCorrupt)
				} else if saveErr != nil {
					t.Fatal(saveErr)
				}
				if after := readOnlySnapshot(t, victimRoot); !reflect.DeepEqual(before, after) {
					t.Fatal("symlink redirected a compilation cache mutation")
				}
				assertNoTmp(t, c)
			})
		}
	}
}
