package cache

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"path/filepath"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

type materialized struct {
	label   string
	dir     string
	file    string
	content string
	hex     string
}

func materializedEntry(manifestDigest, contentDigest string, size int64) (materialized, error) {
	if err := digest.Validate(manifestDigest); err != nil {
		return materialized{}, fault.Wrap(fault.Internal, err, "cached schema: manifest digest")
	}

	label := "cached schema of manifest " + manifestDigest

	if err := digest.Validate(contentDigest); err != nil {
		return materialized{}, fault.Wrap(fault.Internal, err, "%s: content digest", label)
	}

	if size < 0 {
		return materialized{}, fault.New(fault.Internal, "%s: invalid content size %d", label, size)
	}

	hex := digest.Hex(manifestDigest)
	dir := filepath.Join(materializedDir, hex)

	return materialized{
		label:   label,
		dir:     dir,
		file:    filepath.Join(dir, schemaFile),
		content: contentDigest,
		hex:     hex,
	}, nil
}

// SchemaPath returns the absolute path of the materialized schema.json of a
// schema manifest. It depends only on the manifest digest and the cache root,
// so it is stable across runs and mirrors. It returns "" for an invalid
// digest.
func (c *Cache) SchemaPath(manifestDigest string) string {
	if digest.Validate(manifestDigest) != nil {
		return ""
	}

	return c.path(filepath.Join(materializedDir, digest.Hex(manifestDigest), schemaFile))
}

// VerifyMaterialized checks that the schema.json of a manifest exists as a
// regular file, directly inside a real directory, with the given size and
// content digest. It returns ErrNotFound when it is absent and ErrCorrupt for
// any other deviation, including symlinks.
func (c *Cache) VerifyMaterialized(manifestDigest, contentDigest string, size int64) error {
	m, err := materializedEntry(manifestDigest, contentDigest, size)
	if err != nil {
		return err
	}

	_, err = c.checkMaterialized(m, size, false)

	return err
}

// ReadMaterialized returns the verified bytes of a materialized schema.json,
// with the same checks as VerifyMaterialized.
func (c *Cache) ReadMaterialized(manifestDigest, contentDigest string, size int64) ([]byte, error) {
	m, err := materializedEntry(manifestDigest, contentDigest, size)
	if err != nil {
		return nil, err
	}

	return c.checkMaterialized(m, size, true)
}

func (c *Cache) checkMaterialized(m materialized, size int64, keep bool) ([]byte, error) {
	info, err := c.root.Lstat(m.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, notFound(m.label, c.path(m.file))
	}

	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "inspect %s", m.label)
	}

	if !info.IsDir() {
		return nil, corruptf(m.label, c.path(m.dir), "%s, not a directory", describeType(info))
	}

	return c.verifyFile(m.label, m.file, m.content, size, math.MaxInt64, keep)
}

// WriteMaterialized stores the schema.json of a manifest and returns its
// path. fill must write exactly size bytes hashing to contentDigest; writes
// beyond size fail and the result is refused with ErrDigestMismatch. Only a
// complete, verified, read-only file is ever renamed into place. An existing
// verified schema.json is kept; a corrupt one, or a symlink or non-directory
// in place of its directory, is quarantined and replaced. Errors returned by
// fill are passed through wrapped but unclassified.
func (c *Cache) WriteMaterialized(manifestDigest, contentDigest string, size int64, fill func(io.Writer) error) (string, error) {
	m, err := materializedEntry(manifestDigest, contentDigest, size)
	if err != nil {
		return "", err
	}

	tmp, err := c.stage("schema", size)
	if err != nil {
		return "", err
	}

	defer tmp.discard()

	w := tmp.w
	fillErr := fill(w)

	switch {
	case w.overflow:
		return "", mismatchf(m.label, "content is longer than the expected %d bytes", size)
	case w.writeErr != nil:
		return "", fault.Wrap(fault.Internal, w.writeErr, "store %s", m.label)
	case fillErr != nil:
		return "", fmt.Errorf("materialize %s: %w", m.label, fillErr)
	case w.n != size:
		return "", mismatchf(m.label, "content has %d bytes, expected %d", w.n, size)
	}

	if got := digest.FromHash(w.h); got != contentDigest {
		return "", mismatchf(m.label, "content digest is %s, expected %s", got, contentDigest)
	}

	if err := tmp.seal(); err != nil {
		return "", err
	}

	err = tmp.install(slot{
		label:   m.label,
		parent:  m.dir,
		name:    m.file,
		suspect: func() (string, string) { return c.materializedSuspect(m) },
		verify: func() error {
			_, err := c.checkMaterialized(m, size, false)

			return err
		},
	})
	if err != nil {
		return "", err
	}

	return c.path(m.file), nil
}

// QuarantineMaterialized moves the whole materialized directory of a manifest
// (or whatever occupies its name, such as a symlink) to v1/quarantine. A
// missing entry is not an error.
func (c *Cache) QuarantineMaterialized(manifestDigest string) error {
	if err := digest.Validate(manifestDigest); err != nil {
		return fault.Wrap(fault.Internal, err, "cached schema: manifest digest")
	}

	hex := digest.Hex(manifestDigest)

	return c.quarantine(filepath.Join(materializedDir, hex), "cached schema of manifest "+manifestDigest, hex+".materialized", nil)
}

// materializedSuspect names what a repair moves aside: only schema.json while
// the directory is intact, so that the directory other writers install into
// is never moved. The prefixes keep quarantined entries apart from the
// quarantined manifest blob, which has the same hex.
func (c *Cache) materializedSuspect(m materialized) (name, prefix string) {
	if info, err := c.root.Lstat(m.dir); err == nil && info.IsDir() {
		return m.file, m.hex + ".schema"
	}

	return m.dir, m.hex + ".materialized"
}
