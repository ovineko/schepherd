// Package cache is the local content-addressed store of verified registry
// content.
//
// Layout below the cache root:
//
//	v1/blobs/sha256/<hex>                          raw manifest and layer bytes (0444)
//	v1/materialized/sha256/<manifest-hex>/schema.json  decompressed schema (0444)
//	v1/locks/<key>.lock                            inter-process lock files
//	v1/tmp/                                        partial files, renamed into place when complete
//	v1/quarantine/                                 corrupt entries moved aside, deleted after a day
//
// Every file name is derived from a validated sha256 digest or a validated
// lock key, never from registry annotations, and every access below the root
// goes through an os.Root, so neither remote data nor planted symlinks can
// make the cache write outside its directory. Final files only ever appear by
// renaming a fully written, verified temporary file, so readers never observe
// partial content. There is no automatic garbage collection of verified
// entries; only leftovers in v1/tmp and v1/quarantine expire.
//
// Error classes: Open reports unusable directories as fault.Usage, digest
// and size mismatches of incoming data are fault.Integrity, and local I/O
// failures are fault.Internal. ErrNotFound and ErrCorrupt are deliberately
// left unclassified because their exit class depends on whether the caller
// may refetch. Errors returned by caller-supplied readers and fill functions
// are passed through with context but without adding a class.
//
// A Cache is safe for concurrent use by multiple goroutines and processes.
// Writers of the same entry need not hold its Lock: all of them succeed and
// leave one verified entry in place. Only while writers without the Lock race
// to repair a corrupt entry can a reader briefly find it missing, and on
// Windows a verified copy can end up in quarantine next to the corrupt one.
package cache

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ovineko/schepherd/internal/fault"
)

var (
	// ErrNotFound reports that an entry is absent from the cache.
	ErrNotFound = errors.New("not in cache")
	// ErrCorrupt reports a cache entry that exists but does not verify: wrong
	// digest or size, not a regular file, or reached through a symlink.
	ErrCorrupt = errors.New("corrupt cache entry")
	// ErrDigestMismatch reports incoming data whose digest or size differs
	// from the expected one. Nothing is stored in that case.
	ErrDigestMismatch = errors.New("digest mismatch")
	// ErrTooLarge reports incoming data that exceeds the caller's size limit.
	ErrTooLarge = errors.New("size limit exceeded")
)

const (
	dirPerm       fs.FileMode = 0o755
	finalPerm     fs.FileMode = 0o444
	tmpPerm       fs.FileMode = 0o600
	staleTmpAge               = 24 * time.Hour
	quarantineAge             = 24 * time.Hour
	appDirName                = "schepherd"
	schemaFile                = "schema.json"
)

var (
	blobsDir        = filepath.Join("v1", "blobs", "sha256")
	materializedDir = filepath.Join("v1", "materialized", "sha256")
	locksDir        = filepath.Join("v1", "locks")
	tmpDir          = filepath.Join("v1", "tmp")
	quarantineDir   = filepath.Join("v1", "quarantine")

	layoutDirs = []string{
		"v1",
		filepath.Dir(blobsDir),
		blobsDir,
		filepath.Dir(materializedDir),
		materializedDir,
		locksDir,
		tmpDir,
		quarantineDir,
	}
)

// Cache is an open cache directory.
type Cache struct {
	root *os.Root
	// lockBusy is set by tests to learn that a Lock found its key held.
	lockBusy func()
	dir      string
}

// DefaultDir returns the per-user default cache root: the OS user cache
// directory joined with "schepherd".
func DefaultDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fault.Wrap(fault.Usage, err, "no default cache directory; set --cache-dir or SCHEPHERD_CACHE_DIR")
	}

	return filepath.Join(base, appDirName), nil
}

// Open creates the cache layout below dir if needed and opens it. dir is made
// absolute without resolving symlinks. Temporary files older than 24 hours,
// left behind by crashed processes, and entries quarantined more than 24
// hours ago are removed.
func Open(dir string) (*Cache, error) {
	if dir == "" {
		return nil, fault.New(fault.Usage, "cache directory is empty")
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "cache directory %q", dir)
	}

	if err := os.MkdirAll(abs, dirPerm); err != nil {
		return nil, fault.Wrap(fault.Usage, err, "create cache directory %s", abs)
	}

	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "open cache directory %s", abs)
	}

	c := &Cache{root: root, dir: abs}

	for _, name := range layoutDirs {
		if err := c.ensureDir(name); err != nil {
			_ = root.Close()

			return nil, fault.Wrap(fault.Usage, err, "unusable cache directory %s", abs)
		}
	}

	now := time.Now()
	c.removeStaleTmp(now)
	c.removeOldQuarantine(now)

	return c, nil
}

// Dir returns the absolute cache root, the value of the {cache} placeholder.
func (c *Cache) Dir() string {
	return c.dir
}

// Close releases the directory handle. Locks already acquired stay valid
// until released.
func (c *Cache) Close() error {
	if err := c.root.Close(); err != nil {
		return fault.Wrap(fault.Internal, err, "close cache %s", c.dir)
	}

	return nil
}

func (c *Cache) path(name string) string {
	return filepath.Join(c.dir, name)
}

func (c *Cache) ensureDir(name string) error {
	info, err := c.root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if err := c.root.Mkdir(name, dirPerm); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create %s: %w", c.path(name), err)
		}

		info, err = c.root.Lstat(name)
	}

	if err != nil {
		return fmt.Errorf("inspect %s: %w", c.path(name), err)
	}

	if !info.IsDir() {
		return fmt.Errorf("%s is %s, not a directory; remove it", c.path(name), describeType(info))
	}

	return nil
}

func (c *Cache) removeStaleTmp(now time.Time) {
	entries, err := fs.ReadDir(c.root.FS(), filepath.ToSlash(tmpDir))
	if err != nil {
		return
	}

	for _, entry := range entries {
		name := filepath.Join(tmpDir, entry.Name())

		info, err := c.root.Lstat(name)
		if err != nil || now.Sub(info.ModTime()) < staleTmpAge {
			continue
		}

		if info.IsDir() {
			_ = c.root.RemoveAll(name)
		} else {
			c.removeFile(name)
		}
	}
}

// removeFile is best effort: a leftover file is harmless because final names
// are only ever produced by rename.
func (c *Cache) removeFile(name string) {
	err := c.root.Remove(name)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return
	}

	if c.root.Chmod(name, tmpPerm) == nil {
		_ = c.root.Remove(name)
	}
}

func (c *Cache) quarantine(name, label, prefix string, expect fs.FileInfo) error {
	current, err := c.root.Lstat(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return fault.Wrap(fault.Internal, err, "quarantine %s (%s)", label, c.path(name))
	}

	if expect != nil && !os.SameFile(expect, current) {
		return fault.Wrap(fault.Internal, errEntryChanged, "quarantine %s (%s)", label, c.path(name))
	}

	if err := c.root.MkdirAll(quarantineDir, dirPerm); err != nil {
		return fault.Wrap(fault.Internal, err, "create %s", c.path(quarantineDir))
	}

	dest := filepath.Join(quarantineDir, quarantineName(prefix, time.Now()))
	if err := c.root.Rename(name, dest); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return fault.Wrap(fault.Internal, err, "quarantine %s (%s)", label, c.path(name))
	}

	if expect == nil {
		return nil
	}

	// A concurrent writer can install a new entry between the check above
	// and the rename; such an entry is not the corrupt one and goes back.
	if moved, err := c.root.Lstat(dest); err == nil && !os.SameFile(expect, moved) {
		_ = c.root.Rename(dest, name)

		return fault.Wrap(fault.Internal, errEntryChanged, "quarantine %s (%s)", label, c.path(name))
	}

	return nil
}

// quarantineName records when an entry was moved aside. Its modification
// time cannot tell: a blob keeps the time it was first written, so an entry
// quarantined a moment ago could look expired to a concurrent Open while
// quarantine may still move it back.
func quarantineName(prefix string, now time.Time) string {
	return prefix + ".q" + strconv.FormatInt(now.Unix(), 10) + "." + randomToken()
}

// quarantinedAt returns the time recorded by quarantineName, or false for a
// name without one.
func quarantinedAt(name string) (time.Time, bool) {
	rest, _, ok := cutLast(name, ".")
	if !ok {
		return time.Time{}, false
	}

	_, stamp, ok := cutLast(rest, ".q")
	if !ok {
		return time.Time{}, false
	}

	sec, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || sec < 0 {
		return time.Time{}, false
	}

	return time.Unix(sec, 0), true
}

func cutLast(s, sep string) (before, after string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}

	return s[:i], s[i+len(sep):], true
}

// removeOldQuarantine deletes entries quarantined more than quarantineAge
// ago, and entries without a recorded time that were last modified before
// that. Nothing reads quarantined entries; they are kept for a day only so
// that the damage can be inspected. Failures are ignored and retried by the
// next Open.
func (c *Cache) removeOldQuarantine(now time.Time) {
	entries, err := fs.ReadDir(c.root.FS(), filepath.ToSlash(quarantineDir))
	if err != nil {
		return
	}

	for _, entry := range entries {
		name := filepath.Join(quarantineDir, entry.Name())

		at, ok := quarantinedAt(entry.Name())
		if !ok {
			info, err := c.root.Lstat(name)
			if err != nil {
				continue
			}

			at = info.ModTime()
		}

		if now.Sub(at) >= quarantineAge {
			c.removeTree(name)
		}
	}
}

// removeTree removes a file or directory below the root, best effort. Cache
// files are read-only, which Windows refuses to delete, so a failed removal
// makes the regular files and directories inside writable and tries again.
// Links are removed, never followed.
func (c *Cache) removeTree(name string) {
	if c.root.RemoveAll(name) == nil {
		return
	}

	_ = fs.WalkDir(c.root.FS(), filepath.ToSlash(name), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best effort: the next Open retries
		}

		switch {
		case d.IsDir():
			_ = c.root.Chmod(filepath.FromSlash(p), dirPerm)
		case d.Type().IsRegular():
			_ = c.root.Chmod(filepath.FromSlash(p), tmpPerm)
		}

		return nil
	})

	_ = c.root.RemoveAll(name)
}

func randomToken() string {
	return strings.ToLower(rand.Text())
}

func describeType(info fs.FileInfo) string {
	switch mode := info.Mode(); {
	case mode&fs.ModeSymlink != 0:
		return "a symlink"
	case mode.IsDir():
		return "a directory"
	case mode.IsRegular():
		return "a regular file"
	default:
		return "a special file (" + mode.Type().String() + ")"
	}
}

func notFound(label, path string) error {
	return fmt.Errorf("%s (%s): %w", label, path, ErrNotFound)
}

func corruptf(label, path, format string, args ...any) error {
	return fmt.Errorf("%s (%s): %s: %w", label, path, fmt.Sprintf(format, args...), ErrCorrupt)
}

func mismatchf(label, format string, args ...any) error {
	return fault.Wrap(fault.Integrity, ErrDigestMismatch, "%s: %s", label, fmt.Sprintf(format, args...))
}
