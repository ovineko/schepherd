package catalog_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ovineko/schepherd/internal/catalog"
)

func TestCatalogSchemaDocument(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		ID     string `json:"$id"`
		Schema string `json:"$schema"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}

	if doc.Schema != "https://json-schema.org/draft/2020-12/schema" || doc.ID != catalogSchemaURL {
		t.Fatalf("$schema = %q, $id = %q", doc.Schema, doc.ID)
	}

	compileCatalogSchema(t)
}

func TestCatalogSchemaAcceptsFixtures(t *testing.T) {
	t.Parallel()

	schema := compileCatalogSchema(t)

	files, err := filepath.Glob(filepath.Join(catalogsDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}

	if len(files) == 0 {
		t.Fatal("testdata/catalogs holds no fixtures")
	}

	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			t.Parallel()

			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}

			if err := validateAgainstSchema(schema, data); err != nil {
				t.Fatalf("api/catalog.schema.json rejects the fixture: %v", err)
			}

			c, err := catalog.Parse(data, catalog.DefaultLimits())
			if err != nil {
				t.Fatalf("Parse() rejects the fixture: %v", err)
			}

			if err := validateAgainstSchema(schema, mustMarshal(t, c)); err != nil {
				t.Fatalf("api/catalog.schema.json rejects the canonical encoding: %v", err)
			}
		})
	}
}

// TestCatalogSchemaAgreesOnRejections runs every invalid document that JSON
// Schema can express through both validators.
func TestCatalogSchemaAgreesOnRejections(t *testing.T) {
	t.Parallel()

	schema := compileCatalogSchema(t)
	checked := 0

	for _, tc := range invalidCases(t) {
		if !tc.schema {
			continue
		}

		checked++

		if _, err := catalog.Parse(tc.doc, tc.limits); err == nil {
			t.Errorf("%s: Parse() accepts the document", tc.name)
		}

		if err := validateAgainstSchema(schema, tc.doc); err == nil {
			t.Errorf("%s: api/catalog.schema.json accepts the document", tc.name)
		}
	}

	if checked < 50 {
		t.Fatalf("only %d invalid documents are checked against the schema", checked)
	}
}

// TestCatalogSchemaNeverStricterThanParse places interesting code points into
// every free-text and URI member: whatever Parse accepts, the published
// schema must accept too, or third-party tooling would reject real catalogs.
func TestCatalogSchemaNeverStricterThanParse(t *testing.T) {
	t.Parallel()

	schema := compileCatalogSchema(t)

	extra := []rune{0x1680, 0x3000, 0xfeff, 0xfffd, 0x1f600, 0x10ffff}

	runes := make([]rune, 0, 0x300+0x70+len(extra))
	for r := range rune(0x300) {
		runes = append(runes, r)
	}

	for r := range rune(0x70) {
		runes = append(runes, 0x2000+r)
	}

	runes = append(runes, extra...)

	fields := map[string]func(d map[string]any, s string){
		"name":        func(d map[string]any, s string) { entryOf(d, 0)["name"] = s },
		"blank name":  func(d map[string]any, s string) { entryOf(d, 0)["name"] = s + s },
		"description": func(d map[string]any, s string) { entryOf(d, 1)["description"] = "d" + s },
		"license":     func(d map[string]any, s string) { provenanceOf(d)["license"] = "L" + s },
		"fileMatch":   func(d map[string]any, s string) { entryOf(d, 1)["fileMatch"] = []any{"a" + s + ".json"} },
		"dialect":     func(d map[string]any, s string) { entryOf(d, 1)["dialect"] = "urn:x:" + s },
		"source":      func(d map[string]any, s string) { provenanceOf(d)["source"] = "https://h" + s + "/x" },
	}

	accepted := 0

	for field, edit := range fields {
		for _, r := range runes {
			doc := mutated(t, func(d map[string]any) { edit(d, string(r)) })

			if _, err := catalog.Parse(doc, catalog.Limits{}); err != nil {
				continue
			}

			accepted++

			if err := validateAgainstSchema(schema, doc); err != nil {
				t.Errorf("%s with %U: Parse accepts, api/catalog.schema.json rejects: %v", field, r, err)
			}
		}
	}

	if accepted == 0 {
		t.Fatal("no variant was accepted; the test exercises nothing")
	}
}
