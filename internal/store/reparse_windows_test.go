//go:build windows

package store

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/ovineko/schepherd/internal/cache/linktest"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

// A junction needs no privilege on Windows, so any local user who can write
// to the cache directory can plant one in place of a cache entry. The store
// must neither read through it nor write through it: offline it refuses the
// entry, online it moves the junction aside and rebuilds the entry.
func TestWindowsJunctionInPlaceOfCacheEntry(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		entry func(dir string, f *fixture, schemaPath string) string
		name  string
	}{
		{name: "materialized directory", entry: func(_ string, _ *fixture, schemaPath string) string {
			return filepath.Dir(schemaPath)
		}},
		{name: "schema manifest blob", entry: func(dir string, f *fixture, _ string) string {
			return blobFile(dir, f.entries["alpha"].Artifact.Digest)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			dir := t.TempDir()

			s, _ := f.store(t, dir, false)

			sch, err := s.Materialize(ctx, f.entries["alpha"])
			if err != nil {
				t.Fatal(err)
			}

			outside := t.TempDir()
			bait := filepath.Join(outside, "schema.json")

			if err := os.WriteFile(bait, f.content["alpha"], 0o644); err != nil {
				t.Fatal(err)
			}

			entry := tt.entry(dir, f, sch.Path)
			if err := os.RemoveAll(entry); err != nil {
				t.Fatal(err)
			}

			if err := linktest.Junction(outside, entry); err != nil {
				t.Fatal(err)
			}

			offline, opened := f.store(t, dir, true)
			if _, err := offline.Materialize(ctx, f.entries["alpha"]); fault.KindOf(err) != fault.Offline || *opened != 0 {
				t.Fatalf("offline with a junction in the cache = %v (opened %d), want an offline error", err, *opened)
			}

			online, _ := f.store(t, dir, false)

			repaired, err := online.Materialize(ctx, f.entries["alpha"])
			if err != nil {
				t.Fatalf("online repair: %v", err)
			}

			got, err := os.ReadFile(repaired.Path)
			if err != nil || string(got) != string(f.content["alpha"]) {
				t.Fatalf("repaired schema: %q, %v", got, err)
			}

			for _, path := range []string{entry, filepath.Dir(repaired.Path)} {
				info, err := os.Lstat(path)
				if err != nil || info.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
					t.Fatalf("%s is still a link after the repair: %v, %v", path, info, err)
				}
			}

			if names := quarantined(t, dir); len(names) != 1 {
				t.Errorf("quarantine holds %v, want the junction", names)
			}

			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 1 {
				t.Fatalf("outside directory changed: %v, %v", entries, err)
			}

			info, err := os.Stat(bait)
			if err != nil || info.Mode().Perm() != 0o666 {
				t.Fatalf("outside file was modified: %v, %v", info, err)
			}

			if data, _ := os.ReadFile(blobFile(dir, f.entries["alpha"].Artifact.Digest)); digest.FromBytes(data) != f.entries["alpha"].Artifact.Digest {
				t.Error("the schema manifest blob does not verify after the repair")
			}
		})
	}
}
