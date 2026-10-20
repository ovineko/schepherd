package cache

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"path/filepath"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/jsonutil"
)

const maxCompilationMetadata = 2 << 20

// MaxCompiledBytes bounds serialized compiler output both at capture and on disk.
const MaxCompiledBytes = 64 << 20

var compiledDir = filepath.Join("v1", "compiled", "sha256")

// Compilation describes a compiler capability decision, optionally backed by a
// verified artifact. Path is absolute for this cache root and is never persisted.
// A nonempty Reason means the schema is incompatible; two empty fields mean a
// compatible probe without a serialized artifact.
type Compilation struct {
	Path   string
	Reason string
}

type compilationArtifact struct {
	Digest string `json:"digest"`
	Size   *int64 `json:"size"`
}

type compilationRecord struct {
	Key      string               `json:"key"`
	Reason   *string              `json:"reason"`
	Artifact *compilationArtifact `json:"artifact,omitempty"`
}

type compilationEntry struct {
	label string
	dir   string
	name  string
	hex   string
}

func newCompilationEntry(key string) (compilationEntry, error) {
	if err := digest.Validate(key); err != nil {
		return compilationEntry{}, fault.Wrap(fault.Internal, err, "cached compilation: recipe digest")
	}

	hex := digest.Hex(key)
	dir := filepath.Join(compiledDir, hex)

	return compilationEntry{
		label: "cached compilation " + key,
		dir:   dir,
		name:  filepath.Join(dir, "entry.json"),
		hex:   hex,
	}, nil
}

// LoadCompilation verifies strict, bounded recipe metadata and any referenced
// artifact. Missing data returns ErrNotFound; damaged data returns ErrCorrupt.
// Loading never repairs, quarantines or writes an entry.
func (c *Cache) LoadCompilation(key string) (Compilation, error) {
	e, err := newCompilationEntry(key)
	if err != nil {
		return Compilation{}, err
	}

	if err := c.compilationDirectories(e.label, false, "v1", filepath.Dir(compiledDir), compiledDir, e.dir); err != nil {
		return Compilation{}, err
	}

	record, err := c.readCompilationRecord(key, e)
	if err != nil {
		return Compilation{}, err
	}

	result := Compilation{Reason: *record.Reason}
	if record.Artifact == nil {
		return result, nil
	}
	artifact := record.Artifact
	if result.Reason != "" || digest.Validate(artifact.Digest) != nil || artifact.Size == nil || *artifact.Size < 0 || *artifact.Size > MaxCompiledBytes {
		return Compilation{}, corruptf(e.label, c.path(e.name), "invalid artifact descriptor")
	}
	if err := c.compilationDirectories(e.label, false, "v1", filepath.Dir(blobsDir), blobsDir); err != nil {
		return Compilation{}, err
	}
	if err := c.VerifyBlob(artifact.Digest, *artifact.Size); err != nil {
		return Compilation{}, err
	}
	name, err := blobName(artifact.Digest)
	if err != nil {
		return Compilation{}, err
	}
	result.Path = c.path(name)

	return result, nil
}

func (c *Cache) readCompilationRecord(key string, e compilationEntry) (compilationRecord, error) {
	f, info, err := c.openRegular(e.label, e.name)
	if err != nil {
		return compilationRecord{}, err
	}
	defer func() { _ = f.Close() }()

	if info.Size() > maxCompilationMetadata {
		return compilationRecord{}, corruptf(e.label, c.path(e.name), "metadata exceeds %d bytes", maxCompilationMetadata)
	}

	data, err := io.ReadAll(io.LimitReader(f, maxCompilationMetadata+1))
	if err != nil {
		return compilationRecord{}, fault.Wrap(fault.Internal, err, "read %s", e.label)
	}
	if int64(len(data)) != info.Size() || len(data) > maxCompilationMetadata {
		return compilationRecord{}, corruptf(e.label, c.path(e.name), "metadata changed size while being read")
	}
	if err := jsonutil.Check(data, jsonutil.DefaultMaxDepth); err != nil {
		return compilationRecord{}, corruptf(e.label, c.path(e.name), "invalid metadata: %v", err)
	}

	var record compilationRecord
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&record); err != nil {
		return compilationRecord{}, corruptf(e.label, c.path(e.name), "invalid metadata: %v", err)
	}
	if record.Key != key || record.Reason == nil {
		return compilationRecord{}, corruptf(e.label, c.path(e.name), "metadata has a different recipe or missing reason")
	}

	return record, nil
}

// SaveCompilation atomically publishes a capability decision under the complete
// recipe digest. A nil artifact records only a probe; a non-nil artifact records
// serialized compiler output and cannot accompany an incompatibility reason.
// Existing healthy records are retained, including during concurrent writes.
func (c *Cache) SaveCompilation(key, reason string, artifact []byte) (Compilation, error) {
	e, err := newCompilationEntry(key)
	if err != nil {
		return Compilation{}, err
	}
	if reason != "" && artifact != nil {
		return Compilation{}, fault.New(fault.Internal, "%s: incompatible decision cannot have an artifact", e.label)
	}
	if len(artifact) > MaxCompiledBytes {
		return Compilation{}, fault.Wrap(fault.Integrity, ErrTooLarge, "%s: artifact exceeds %d bytes", e.label, MaxCompiledBytes)
	}
	if err := c.requireWritable(); err != nil {
		return Compilation{}, err
	}

	record := compilationRecord{Key: key, Reason: &reason}
	if artifact != nil {
		size := int64(len(artifact))
		record.Artifact = &compilationArtifact{Digest: digest.FromBytes(artifact), Size: &size}
	}
	data, err := json.Marshal(record)
	if err != nil {
		return Compilation{}, fault.Wrap(fault.Internal, err, "encode %s", e.label)
	}
	if len(data) > maxCompilationMetadata {
		return Compilation{}, fault.Wrap(fault.Integrity, ErrTooLarge, "%s: metadata exceeds %d bytes", e.label, maxCompilationMetadata)
	}
	if existing, err := c.LoadCompilation(key); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrCorrupt) {
		return Compilation{}, err
	}
	if err := c.compilationDirectories(e.label, true, "v1", filepath.Dir(compiledDir), compiledDir); err != nil {
		return Compilation{}, err
	}
	if record.Artifact != nil {
		if err := c.compilationDirectories(e.label, true, "v1", filepath.Dir(blobsDir), blobsDir); err != nil {
			return Compilation{}, err
		}
		if err := c.WriteBlob(record.Artifact.Digest, *record.Artifact.Size, *record.Artifact.Size, bytes.NewReader(artifact)); err != nil {
			return Compilation{}, err
		}
	}

	if err := c.installCompilation(key, e, data); err != nil {
		return Compilation{}, err
	}

	return c.LoadCompilation(key)
}

func (c *Cache) installCompilation(key string, e compilationEntry, data []byte) error {
	tmp, err := c.stage("compilation", int64(len(data)))
	if err != nil {
		return err
	}
	defer tmp.discard()

	if _, err := tmp.w.Write(data); err != nil {
		return fault.Wrap(fault.Internal, err, "store %s", e.label)
	}
	if err := tmp.seal(); err != nil {
		return err
	}
	return tmp.install(slot{
		label:  e.label,
		parent: e.dir,
		name:   e.name,
		suspect: func() (string, string) {
			if info, err := c.root.Lstat(e.dir); err == nil && info.IsDir() {
				return e.name, e.hex + ".compilation-entry"
			}
			return e.dir, e.hex + ".compilation"
		},
		verify: func() error {
			_, err := c.LoadCompilation(key)
			if errors.Is(err, ErrNotFound) {
				if _, statErr := c.root.Lstat(e.name); statErr == nil {
					return corruptf(e.label, c.path(e.name), "referenced content is missing")
				}
			}
			return err
		},
	})
}

func (c *Cache) compilationDirectories(label string, create bool, names ...string) error {
	for _, name := range names {
		if c.root == nil {
			return notFound(label, c.path(name))
		}
		info, err := c.root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) && create {
			if err := c.root.Mkdir(name, dirPerm); err != nil && !errors.Is(err, fs.ErrExist) {
				return fault.Wrap(fault.Internal, err, "create %s", c.path(name))
			}
			info, err = c.root.Lstat(name)
		}
		if errors.Is(err, fs.ErrNotExist) {
			return notFound(label, c.path(name))
		}
		if err != nil {
			return fault.Wrap(fault.Internal, err, "inspect %s", c.path(name))
		}
		if !info.IsDir() {
			return corruptf(label, c.path(name), "%s, not a directory", describeType(info))
		}
	}
	return nil
}
