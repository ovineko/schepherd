package cache

import (
	"bytes"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

const installAttempts = 4

var (
	errOverflow     = errors.New("more data than allowed")
	errEntryChanged = errors.New("entry changed while being replaced")
)

func (c *Cache) openRegular(label, name string) (*os.File, fs.FileInfo, error) {
	path := c.path(name)

	info, err := c.root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, notFound(label, path)
	}

	if err != nil {
		return nil, nil, fault.Wrap(fault.Internal, err, "inspect %s", label)
	}

	if !info.Mode().IsRegular() {
		return nil, nil, corruptf(label, path, "%s, not a regular file", describeType(info))
	}

	var f *os.File

	err = retryTransient(func() error {
		var openErr error
		f, openErr = c.root.Open(name)
		if openErr != nil {
			return fmt.Errorf("%w", openErr)
		}

		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, notFound(label, path)
	}

	if err != nil {
		return nil, nil, fault.Wrap(fault.Internal, err, "open %s", label)
	}

	opened, err := f.Stat()
	if err != nil {
		_ = f.Close()

		return nil, nil, fault.Wrap(fault.Internal, err, "inspect %s", label)
	}

	if !os.SameFile(info, opened) {
		_ = f.Close()

		return nil, nil, corruptf(label, path, "replaced while being opened")
	}

	return f, info, nil
}

func (c *Cache) verifyFile(label, name, want string, size, maxSize int64, keep bool) ([]byte, error) {
	f, info, err := c.openRegular(label, name)
	if err != nil {
		return nil, err
	}

	defer func() { _ = f.Close() }()

	path := c.path(name)
	actual := info.Size()

	if size >= 0 && actual != size {
		return nil, corruptf(label, path, "size %d, expected %d", actual, size)
	}

	if actual > maxSize {
		return nil, corruptf(label, path, "size %d exceeds the limit of %d bytes", actual, maxSize)
	}

	h := digest.NewHash()

	var buf *bytes.Buffer

	w := io.Writer(h)
	if keep {
		buf = bytes.NewBuffer(make([]byte, 0, actual))
		w = io.MultiWriter(h, buf)
	}

	n, err := io.Copy(w, io.LimitReader(f, plusOne(actual)))
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "read %s", label)
	}

	if n != actual {
		return nil, corruptf(label, path, "changed size while being read")
	}

	if got := digest.FromHash(h); got != want {
		return nil, corruptf(label, path, "content digest is %s", got)
	}

	if !keep {
		return nil, nil
	}

	return buf.Bytes(), nil
}

func plusOne(n int64) int64 {
	if n == math.MaxInt64 {
		return n
	}

	return n + 1
}

// stagingWriter writes to a temporary file while hashing, and refuses data
// beyond limit so a hostile source cannot fill the disk.
type stagingWriter struct {
	f        *os.File
	h        hash.Hash
	writeErr error
	n        int64
	limit    int64
	overflow bool
}

func (w *stagingWriter) Write(p []byte) (int, error) {
	if w.overflow {
		return 0, errOverflow
	}

	if w.writeErr != nil {
		return 0, w.writeErr
	}

	if int64(len(p)) > w.limit-w.n {
		w.overflow = true

		return 0, errOverflow
	}

	n, err := w.f.Write(p)
	w.h.Write(p[:n])
	w.n += int64(n)

	if err != nil {
		w.writeErr = fmt.Errorf("write %s: %w", w.f.Name(), err)

		return n, w.writeErr
	}

	return n, nil
}

type staged struct {
	c         *Cache
	w         *stagingWriter
	name      string
	closed    bool
	installed bool
}

func (c *Cache) stage(kind string, limit int64) (*staged, error) {
	var lastErr error

	for range 2 {
		name := filepath.Join(tmpDir, kind+"-"+randomToken())

		f, err := c.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, tmpPerm)
		if err == nil {
			return &staged{c: c, name: name, w: &stagingWriter{f: f, h: digest.NewHash(), limit: limit}}, nil
		}

		lastErr = err

		if errors.Is(err, fs.ErrNotExist) {
			if err := c.root.MkdirAll(tmpDir, dirPerm); err != nil {
				return nil, fault.Wrap(fault.Internal, err, "create %s", c.path(tmpDir))
			}

			continue
		}

		if !errors.Is(err, fs.ErrExist) {
			break
		}
	}

	return nil, fault.Wrap(fault.Internal, lastErr, "create temporary file in %s", c.path(tmpDir))
}

// seal makes the complete temporary file read-only and durable. The mode is
// set through the open descriptor so no symlink swap can redirect it.
func (s *staged) seal() error {
	f := s.w.f

	if err := f.Chmod(finalPerm); err != nil {
		return fault.Wrap(fault.Internal, err, "set mode of %s", s.c.path(s.name))
	}

	if err := f.Sync(); err != nil {
		return fault.Wrap(fault.Internal, err, "sync %s", s.c.path(s.name))
	}

	s.closed = true

	if err := f.Close(); err != nil {
		return fault.Wrap(fault.Internal, err, "close %s", s.c.path(s.name))
	}

	return nil
}

func (s *staged) discard() {
	if s.installed {
		return
	}

	if !s.closed {
		s.closed = true
		_ = s.w.f.Close()
	}

	s.c.removeFile(s.name)
}

type slot struct {
	verify  func() error
	suspect func() (name, prefix string)
	label   string
	parent  string
	name    string
}

// install renames the sealed file into place. An existing entry that verifies
// is kept as is. A corrupt one is quarantined first, but only while it is
// still the object that failed verification, because a concurrent writer may
// have replaced it with a verified copy in the meantime; losing such a race,
// or a rename into a directory another writer just moved aside, starts over.
// On Windows a rename onto an existing read-only file fails, so a failed
// rename counts as success when a concurrent writer has meanwhile installed a
// verified copy.
func (s *staged) install(dst slot) error {
	var err error

	for range installAttempts {
		var retry bool
		if retry, err = s.tryInstall(dst); !retry {
			return err
		}
	}

	return err
}

func (s *staged) tryInstall(dst slot) (retry bool, err error) {
	c := s.c

	suspect, prefix := dst.suspect()
	seen, seenErr := c.root.Lstat(suspect)

	// The suspect is chosen from what exists, such as the materialized
	// directory while it is missing. When another writer changes that before
	// seen is taken, seen may describe a healthy directory that holds its
	// verified copy, and quarantining it would move that copy away.
	if again, _ := dst.suspect(); again != suspect {
		return true, fault.Wrap(fault.Internal, errEntryChanged, "store %s at %s", dst.label, c.path(dst.name))
	}

	switch err := dst.verify(); {
	case err == nil:
		return false, nil
	case errors.Is(err, ErrNotFound):
	case errors.Is(err, ErrCorrupt):
		if seenErr != nil {
			return true, fault.Wrap(fault.Internal, errEntryChanged, "store %s at %s", dst.label, c.path(dst.name))
		}

		if err := c.quarantine(suspect, dst.label, prefix, seen); err != nil {
			return errors.Is(err, errEntryChanged), err
		}
	default:
		return false, err
	}

	if err := c.root.MkdirAll(dst.parent, dirPerm); err != nil {
		return true, fault.Wrap(fault.Internal, err, "store %s: create %s", dst.label, c.path(dst.parent))
	}

	// A rename onto an entry another writer has meanwhile installed and
	// verified is not needed; otherwise a transient failure is retried.
	err = retryTransient(func() error {
		renameErr := c.root.Rename(s.name, dst.name)
		if renameErr == nil || dst.verify() == nil {
			return nil
		}

		return fmt.Errorf("%w", renameErr)
	})
	if err != nil {
		return errors.Is(err, fs.ErrNotExist), fault.Wrap(fault.Internal, err, "store %s at %s", dst.label, c.path(dst.name))
	}

	if _, statErr := c.root.Lstat(s.name); statErr == nil {
		return false, nil
	}

	s.installed = true

	return false, nil
}
