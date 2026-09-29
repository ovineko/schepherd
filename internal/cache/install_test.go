package cache

import (
	"path/filepath"
	"testing"
)

// placeVerifiedCopy does what a concurrent writer of the same entry does:
// it puts a complete, sealed copy in place of the materialized schema.json.
// The old copy is removed first because Windows cannot rename over a
// read-only file; either way the entry becomes a different file.
func placeVerifiedCopy(t *testing.T, c *Cache, m materialized) {
	t.Helper()

	tmp, err := c.stage("schema", int64(len(testSchema)))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := tmp.w.Write(testSchema); err != nil {
		t.Fatal(err)
	}

	if err := tmp.seal(); err != nil {
		t.Fatal(err)
	}

	if err := c.root.MkdirAll(m.dir, dirPerm); err != nil {
		t.Fatal(err)
	}

	c.removeFile(m.file)

	if err := c.root.Rename(tmp.name, m.file); err != nil {
		t.Fatal(err)
	}
}

// Between deciding that the materialized directory is missing and looking at
// it, another writer can create the directory and install a verified copy;
// a third writer replacing that copy then makes verification fail once
// ("replaced while being opened"). The directory must not be quarantined for
// that: it holds a verified entry. This reproduces the interleaving behind
// the rare failures of TestWriteMaterializedConcurrentWriters.
func TestInstallKeepsADirectoryAnotherWriterCreated(t *testing.T) {
	c := openTestCache(t)
	size := int64(len(testSchema))

	m, err := materializedEntry(testManifest, testContent, size)
	if err != nil {
		t.Fatal(err)
	}

	tmp, err := c.stage("schema", size)
	if err != nil {
		t.Fatal(err)
	}

	defer tmp.discard()

	if _, err := tmp.w.Write(testSchema); err != nil {
		t.Fatal(err)
	}

	if err := tmp.seal(); err != nil {
		t.Fatal(err)
	}

	var decided, verified int

	err = tmp.install(slot{
		label:  m.label,
		parent: m.dir,
		name:   m.file,
		suspect: func() (string, string) {
			name, prefix := c.materializedSuspect(m)

			decided++
			if decided == 1 {
				placeVerifiedCopy(t, c, m)
			}

			return name, prefix
		},
		verify: func() error {
			verified++
			if verified == 1 {
				placeVerifiedCopy(t, c, m)

				return corruptf(m.label, c.path(m.file), "replaced while being opened")
			}

			_, err := c.checkMaterialized(m, size, false)

			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	assertEmptyDir(t, filepath.Join(c.Dir(), "v1", "quarantine"))

	if err := c.VerifyMaterialized(testManifest, testContent, size); err != nil {
		t.Fatal(err)
	}
}
