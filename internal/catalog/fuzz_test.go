package catalog_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/fault"
)

const (
	maxFuzzInput = 64 << 10
	// maxFuzzMessage leaves room for the longest text an error quotes in
	// full: a fileMatch pattern of up to 1024 bytes that match rejects.
	maxFuzzMessage = 4 << 10
)

// FuzzParse checks that Parse never panics, classifies every failure with a
// bounded, terminal-safe message, and that every accepted catalog has a
// canonical encoding which parses back to the same value and which
// api/catalog.schema.json also accepts.
func FuzzParse(f *testing.F) {
	files, err := filepath.Glob(filepath.Join(catalogsDir, "*.json"))
	if err != nil {
		f.Fatal(err)
	}

	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			f.Fatal(err)
		}

		f.Add(data)
	}

	f.Add([]byte(validCanonical()))
	f.Add(encode(f, validDoc()))
	f.Add([]byte(`{"formatVersion":2,"revision":"20260923.1200","schemas":[]}`))
	f.Add([]byte(`{"formatVersion":3}`))
	f.Add([]byte(`{"formatVersion":"2","revision":"20260923.1200","schemas":[]}`))

	for _, tc := range invalidCases(f) {
		if len(tc.doc) <= 4<<10 {
			f.Add(tc.doc)
		}
	}

	schema := compileCatalogSchema(f)
	limits := catalog.Limits{MaxEntries: 64, MaxManifestBytes: 1 << 20}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxFuzzInput {
			return
		}

		c, err := catalog.Parse(data, limits)
		if err != nil {
			if kind := fault.KindOf(err); kind != fault.Integrity && kind != fault.Usage {
				t.Fatalf("Parse() error has kind %s: %v", kind, err)
			}

			if c != nil {
				t.Fatal("Parse() returned a catalog with an error")
			}

			if len(err.Error()) > maxFuzzMessage {
				t.Fatalf("Parse() error is %d bytes long", len(err.Error()))
			}

			assertSafeMessage(t, err)

			return
		}

		canonical, err := catalog.Marshal(c)
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}

		again, err := catalog.Parse(canonical, limits)
		if err != nil {
			t.Fatalf("Parse(Marshal()) error = %v\n%s", err, canonical)
		}

		if !reflect.DeepEqual(again, c) {
			t.Fatalf("Parse(Marshal()) =\n%#v\nwant\n%#v", again, c)
		}

		if second, err := catalog.Marshal(again); err != nil || !bytes.Equal(second, canonical) {
			t.Fatalf("Marshal() is not stable: %v\n%s\n%s", err, canonical, second)
		}

		if err := validateAgainstSchema(schema, canonical); err != nil {
			t.Fatalf("api/catalog.schema.json rejects a catalog Parse accepts: %v\n%s", err, canonical)
		}
	})
}
