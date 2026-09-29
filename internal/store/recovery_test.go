package store

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

func blobFile(dir, dgst string) string {
	return filepath.Join(dir, "v1", "blobs", "sha256", digest.Hex(dgst))
}

func quarantined(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(dir, "v1", "quarantine"))
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	return names
}

// damages returns replacement contents for a cached blob: one of another
// size and one of the same size with a single flipped byte.
func damages(t *testing.T, path string) map[string][]byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	flipped := slices.Clone(data)
	flipped[len(flipped)/2] ^= 0x01

	return map[string][]byte{"garbage": []byte("garbage"), "flipped byte": flipped}
}

// assertRepairedOnlineRefusedOffline damages the cached blob dgst after a
// warm-up, then checks that offline use fails with the offline kind without
// opening the registry, and that online use fetches the blob again, moves
// the damaged copy to quarantine and leaves a cache that works offline.
func assertRepairedOnlineRefusedOffline(t *testing.T, f *fixture, dgst string, use func(t *testing.T, s *Store) error) {
	t.Helper()

	warmDir := t.TempDir()

	warm, _ := f.store(t, warmDir, false)
	if err := use(t, warm); err != nil {
		t.Fatal(err)
	}

	for name, bad := range damages(t, blobFile(warmDir, dgst)) {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()

			s, _ := f.store(t, dir, false)
			if err := use(t, s); err != nil {
				t.Fatal(err)
			}

			path := blobFile(dir, dgst)
			corruptFile(t, path, bad)

			offline, opened := f.store(t, dir, true)

			err := use(t, offline)
			if fault.KindOf(err) != fault.Offline || *opened != 0 {
				t.Fatalf("offline with a corrupt %s = %v (opened %d), want an offline error", dgst, err, *opened)
			}

			if !strings.Contains(err.Error(), "corrupt") {
				t.Errorf("offline error does not say the entry is corrupt: %v", err)
			}

			if got, _ := os.ReadFile(path); string(got) != string(bad) {
				t.Error("offline use changed the corrupt entry")
			}

			before := f.remote.count()

			online, _ := f.store(t, dir, false)
			if err := use(t, online); err != nil {
				t.Fatalf("online repair: %v", err)
			}

			if f.remote.count() == before {
				t.Error("the corrupt blob was not fetched again")
			}

			if got, err := os.ReadFile(path); err != nil || digest.FromBytes(got) != dgst {
				t.Errorf("blob not repaired: %v", err)
			}

			if names := quarantined(t, dir); len(names) != 1 || !strings.HasPrefix(names[0], digest.Hex(dgst)) {
				t.Errorf("quarantine holds %v, want the damaged %s", names, dgst)
			}

			after, opened := f.store(t, dir, true)
			if err := use(t, after); err != nil || *opened != 0 {
				t.Errorf("offline after repair: %v (opened %d)", err, *opened)
			}
		})
	}
}

func TestCorruptCatalogIndexRepairedOnlineRefusedOffline(t *testing.T) {
	f := newFixture(t)

	assertRepairedOnlineRefusedOffline(t, f, f.catalog, func(_ *testing.T, s *Store) error {
		_, err := s.Catalog(context.Background(), f.catalog)

		return err
	})
}

func TestCorruptCatalogMetadataRepairedOnlineRefusedOffline(t *testing.T) {
	f := newFixture(t)

	assertRepairedOnlineRefusedOffline(t, f, f.metadata, func(_ *testing.T, s *Store) error {
		_, err := s.Catalog(context.Background(), f.catalog)

		return err
	})
}

func TestCorruptCatalogPayloadRepairedOnlineRefusedOffline(t *testing.T) {
	f := newFixture(t)

	cm, err := artifact.ParseCatalogMetadata(f.remote.blobs[f.metadata], artifact.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	assertRepairedOnlineRefusedOffline(t, f, cm.Payload.Digest.String(), func(t *testing.T, s *Store) error {
		t.Helper()

		loaded, err := s.Catalog(context.Background(), f.catalog)
		if err == nil && len(loaded.Catalog.Schemas) != 2 {
			t.Errorf("catalog has %d schemas", len(loaded.Catalog.Schemas))
		}

		return err
	})
}

func TestCorruptSchemaManifestRepairedOnlineRefusedOffline(t *testing.T) {
	for _, id := range []string{"alpha", "gamma"} {
		t.Run(id, func(t *testing.T) {
			f := newFixture(t)

			assertRepairedOnlineRefusedOffline(t, f, f.entries[id].Artifact.Digest, func(t *testing.T, s *Store) error {
				t.Helper()

				sch, err := s.Materialize(context.Background(), f.entries[id])
				if err != nil {
					return err
				}

				if got, _ := os.ReadFile(sch.Path); string(got) != string(f.content[id]) {
					t.Errorf("%s materialized wrong content", id)
				}

				return nil
			})
		})
	}
}
