package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/fault"
)

func TestLockKeyValidation(t *testing.T) {
	valid := []string{"a", "0", "a-b", strings.Repeat("f", 64), "a" + strings.Repeat("-", 99)}
	invalid := []string{
		"", "-a", "A", "a/b", `a\b`, "..", "../x", "a.b", "a_b", "a b", "a\x00", "é",
		"a" + strings.Repeat("b", 100),
	}

	c := openTestCache(t)

	for _, key := range valid {
		release, err := c.Lock(t.Context(), key)
		if err != nil {
			t.Fatalf("Lock(%q): %v", key, err)
		}

		release()
	}

	for _, key := range invalid {
		_, err := c.Lock(t.Context(), key)
		assertKind(t, err, fault.Internal)
	}

	names := listDir(t, filepath.Join(c.Dir(), "v1", "locks"))
	if len(names) != len(valid) {
		t.Fatalf("lock files %v, want exactly one per valid key", names)
	}
}

func TestLockExcludesGoroutines(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	handles := []*Cache{openCacheAt(t, dir), openCacheAt(t, dir)}

	var (
		inside   atomic.Int32
		total    atomic.Int32
		overlaps atomic.Int32
		wg       sync.WaitGroup
	)

	errs := make(chan error, 8)

	for i := range 8 {
		c := handles[i%len(handles)]

		wg.Go(func() {
			for range 5 {
				release, err := c.Lock(t.Context(), "shared")
				if err != nil {
					errs <- err

					return
				}

				if inside.Add(1) != 1 {
					overlaps.Add(1)
				}

				time.Sleep(time.Millisecond)
				total.Add(1)
				inside.Add(-1)
				release()
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}

	if overlaps.Load() != 0 {
		t.Fatalf("%d critical sections overlapped", overlaps.Load())
	}

	if total.Load() != 40 {
		t.Fatalf("completed %d critical sections, want 40", total.Load())
	}
}

func TestLockContextEnds(t *testing.T) {
	c := openTestCache(t)

	release, err := c.Lock(t.Context(), "busy")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	deadline, cancelDeadline := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer cancelDeadline()

	start := time.Now()
	_, err = c.Lock(deadline, "busy")
	assertIs(t, err, context.DeadlineExceeded)
	assertUnclassified(t, err)

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("deadline honored only after %s", elapsed)
	}

	canceled, cancel := context.WithCancel(t.Context())
	time.AfterFunc(30*time.Millisecond, cancel)

	_, err = c.Lock(canceled, "busy")
	assertIs(t, err, context.Canceled)
	assertKind(t, err, fault.Canceled)

	other, err := c.Lock(t.Context(), "other-key")
	if err != nil {
		t.Fatalf("an unrelated key must not be blocked: %v", err)
	}

	other()
}

func TestLockReleaseIsIdempotent(t *testing.T) {
	c := openTestCache(t)

	release, err := c.Lock(t.Context(), "key")
	if err != nil {
		t.Fatal(err)
	}

	release()
	release()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	again, err := c.Lock(ctx, "key")
	if err != nil {
		t.Fatalf("relock after release: %v", err)
	}

	again()
}

func TestLockRefusesDirectoryAtLockPath(t *testing.T) {
	c := openTestCache(t)
	lockPath := filepath.Join(c.Dir(), "v1", "locks", "key.lock")

	if err := os.MkdirAll(filepath.Join(lockPath, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := c.Lock(t.Context(), "key")
	assertKind(t, err, fault.Internal)

	if _, statErr := os.Stat(filepath.Join(lockPath, "keep")); statErr != nil {
		t.Fatalf("a directory at the lock path must not be removed: %v", statErr)
	}

	if errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected cancellation: %v", err)
	}
}
