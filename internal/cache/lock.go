package cache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/ovineko/schepherd/internal/fault"
)

const (
	lockRetryDelay = 25 * time.Millisecond
	lockAttempts   = 5
)

var (
	lockKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)
	errLockMoved   = errors.New("lock file was replaced")
)

// Lock takes an exclusive lock on key that is honored by all goroutines and
// processes using the same cache root, waiting until it is free or ctx ends;
// in the latter case the returned error wraps ctx.Err() and is unclassified
// unless it is a cancellation. Keys match ^[a-z0-9][a-z0-9-]{0,99}$ (callers
// pass digest hex). The lock dies with the process, so a crash never leaves a
// stale lock. release is idempotent.
//
// The lock file is opened through the cache root and locked by descriptor,
// never reopened by path, so a symlink planted in v1/locks cannot make Lock
// create or lock a file outside the cache. Removing or replacing a lock file
// that is held breaks the exclusion for later callers, as with any lock file.
func (c *Cache) Lock(ctx context.Context, key string) (release func(), err error) {
	if !lockKeyPattern.MatchString(key) {
		return nil, fault.New(fault.Internal, "invalid cache lock key %q", key)
	}

	name := filepath.Join(locksDir, key+".lock")

	for range lockAttempts {
		f, err := c.openLockFile(name)
		if errors.Is(err, errLockMoved) {
			continue
		}

		if err != nil {
			return nil, err
		}

		if err := c.waitForLock(ctx, f, key, c.path(name)); err != nil {
			_ = f.Close()

			return nil, err
		}

		// The file may have been replaced while this caller waited; a lock on
		// a file that no longer has the name excludes nobody.
		if c.isLockFile(name, f) {
			var once sync.Once

			return func() {
				once.Do(func() {
					_ = unlockFile(f)
					_ = f.Close()
				})
			}, nil
		}

		_ = unlockFile(f)
		_ = f.Close()
	}

	return nil, fault.New(fault.Internal, "cache lock %s keeps being replaced", c.path(name))
}

// openLockFile creates a missing lock file with O_EXCL, which never follows a
// symlink planted after the Lstat, and reports errLockMoved when the name
// changed under it so that Lock starts over.
func (c *Cache) openLockFile(name string) (*os.File, error) {
	if err := c.root.MkdirAll(locksDir, dirPerm); err != nil {
		return nil, fault.Wrap(fault.Internal, err, "create %s", c.path(locksDir))
	}

	// Read-write because NFS emulates flock with byte-range locks, which need
	// a writable descriptor for an exclusive lock.
	flag := os.O_RDWR

	info, err := c.root.Lstat(name)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		flag |= os.O_CREATE | os.O_EXCL
	case err != nil:
		return nil, fault.Wrap(fault.Internal, err, "inspect cache lock %s", c.path(name))
	case info.Mode()&fs.ModeSymlink != 0:
		if err := c.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fault.Wrap(fault.Internal, err, "remove symlink at cache lock %s", c.path(name))
		}

		flag |= os.O_CREATE | os.O_EXCL
	case !info.Mode().IsRegular():
		return nil, fault.New(fault.Internal, "cache lock %s is %s, not a regular file; remove it", c.path(name), describeType(info))
	}

	f, err := c.root.OpenFile(name, flag, tmpPerm)

	switch {
	case errors.Is(err, fs.ErrExist), errors.Is(err, fs.ErrNotExist):
		return nil, errLockMoved
	case err != nil:
		return nil, fault.Wrap(fault.Internal, err, "open cache lock %s", c.path(name))
	case !c.isLockFile(name, f):
		_ = f.Close()

		return nil, errLockMoved
	}

	return f, nil
}

func (c *Cache) isLockFile(name string, f *os.File) bool {
	opened, err := f.Stat()
	if err != nil {
		return false
	}

	current, err := c.root.Lstat(name)

	return err == nil && current.Mode().IsRegular() && os.SameFile(opened, current)
}

func (c *Cache) waitForLock(ctx context.Context, f *os.File, key, path string) error {
	for {
		if ctx.Err() != nil {
			return lockWaitError(ctx, key)
		}

		locked, err := tryLockFile(f)
		if err != nil {
			return fault.Wrap(fault.Internal, err, "cache lock %s", path)
		}

		if locked {
			return nil
		}

		if c.lockBusy != nil {
			c.lockBusy()
		}

		select {
		case <-ctx.Done():
			return lockWaitError(ctx, key)
		case <-time.After(lockRetryDelay):
		}
	}
}

func lockWaitError(ctx context.Context, key string) error {
	err := ctx.Err()
	if errors.Is(err, context.Canceled) {
		return fault.Wrap(fault.Canceled, err, "wait for cache lock %s", key)
	}

	return fmt.Errorf("wait for cache lock %s: %w", key, err)
}
