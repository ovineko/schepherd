package catalog_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
)

const (
	testRevision     = "20260923.1200"
	catalogSchemaURL = "https://raw.githubusercontent.com/ovineko/schepherd/main/api/catalog.schema.json"
)

var (
	digestA = digest.FromBytes([]byte("manifest a"))
	digestB = digest.FromBytes([]byte("manifest b"))
	digestC = digest.FromBytes([]byte("upstream beta"))
	digestD = digest.FromBytes([]byte("dependency a"))
	digestE = digest.FromBytes([]byte("dependency b"))
)

var (
	repoRoot    = filepath.Join("..", "..")
	schemasDir  = filepath.Join(repoRoot, "testdata", "schemas")
	catalogsDir = filepath.Join(repoRoot, "testdata", "catalogs")
	schemaFile  = filepath.Join(repoRoot, "api", "catalog.schema.json")
)

func artifactOf(d string, size int) map[string]any {
	return map[string]any{"mediaType": catalog.ManifestMediaType, "digest": d, "size": size}
}

// validDoc returns a document that exercises every member of the format.
func validDoc() map[string]any {
	return map[string]any{
		"formatVersion": 2,
		"revision":      testRevision,
		"schemas": []any{
			map[string]any{
				"id":       "alpha",
				"name":     "Alpha",
				"artifact": artifactOf(digestA, 512),
			},
			map[string]any{
				"id":          "beta",
				"name":        "Beta schema",
				"description": "Line one.\nLine two with\ttab.",
				"dialect":     "https://json-schema.org/draft/2020-12/schema",
				"fileMatch":   []any{"beta.json", "**/.beta/*.json", "!**/node_modules/**"},
				"artifact":    artifactOf(digestB, 700),
				"provenance": map[string]any{
					"source":       "https://schemas.example.com/beta.json",
					"sourceDigest": digestC,
					"license":      "MIT",
					"dependencies": []any{
						map[string]any{"source": "https://schemas.example.com/a.json", "digest": digestD},
						map[string]any{"source": "https://schemas.example.com/b.json", "digest": digestE},
					},
				},
			},
		},
	}
}

// validCatalog is validDoc as Parse must return it.
func validCatalog() *catalog.Catalog {
	return &catalog.Catalog{
		FormatVersion: 2,
		Revision:      testRevision,
		Schemas: []catalog.Entry{
			{
				ID:       "alpha",
				Name:     "Alpha",
				Artifact: catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: digestA, Size: 512},
			},
			{
				ID:          "beta",
				Name:        "Beta schema",
				Description: "Line one.\nLine two with\ttab.",
				Dialect:     "https://json-schema.org/draft/2020-12/schema",
				FileMatch:   []string{"beta.json", "**/.beta/*.json", "!**/node_modules/**"},
				Artifact:    catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: digestB, Size: 700},
				Provenance: &catalog.Provenance{
					Source:       "https://schemas.example.com/beta.json",
					SourceDigest: digestC,
					License:      "MIT",
					Dependencies: []catalog.Dependency{
						{Source: "https://schemas.example.com/a.json", Digest: digestD},
						{Source: "https://schemas.example.com/b.json", Digest: digestE},
					},
				},
			},
		},
	}
}

// validCanonical is the exact canonical encoding of validDoc.
func validCanonical() string {
	return `{"formatVersion":2,"revision":"20260923.1200","schemas":[` +
		`{"artifact":{"digest":"` + digestA + `","mediaType":"application/vnd.oci.image.manifest.v1+json","size":512},"id":"alpha","name":"Alpha"},` +
		`{"artifact":{"digest":"` + digestB + `","mediaType":"application/vnd.oci.image.manifest.v1+json","size":700},` +
		`"description":"Line one.\nLine two with\ttab.","dialect":"https://json-schema.org/draft/2020-12/schema",` +
		`"fileMatch":["beta.json","**/.beta/*.json","!**/node_modules/**"],"id":"beta","name":"Beta schema",` +
		`"provenance":{"dependencies":[{"digest":"` + digestD + `","source":"https://schemas.example.com/a.json"},` +
		`{"digest":"` + digestE + `","source":"https://schemas.example.com/b.json"}],` +
		`"license":"MIT","source":"https://schemas.example.com/beta.json","sourceDigest":"` + digestC + `"}}]}`
}

func encode(tb testing.TB, doc any) []byte {
	tb.Helper()

	data, err := json.Marshal(doc)
	if err != nil {
		tb.Fatalf("encode test document: %v", err)
	}

	return data
}

func mutated(tb testing.TB, edit func(doc map[string]any)) []byte {
	tb.Helper()

	doc := validDoc()
	edit(doc)

	return encode(tb, doc)
}

func schemasOf(doc map[string]any) []any {
	schemas, _ := doc["schemas"].([]any)

	return schemas
}

func entryOf(doc map[string]any, i int) map[string]any {
	entry, _ := schemasOf(doc)[i].(map[string]any)

	return entry
}

func artifactIn(doc map[string]any, i int) map[string]any {
	a, _ := entryOf(doc, i)["artifact"].(map[string]any)

	return a
}

func provenanceOf(doc map[string]any) map[string]any {
	p, _ := entryOf(doc, 1)["provenance"].(map[string]any)

	return p
}

func dependencyOf(doc map[string]any, i int) map[string]any {
	deps, _ := provenanceOf(doc)["dependencies"].([]any)
	dep, _ := deps[i].(map[string]any)

	return dep
}

func entryWithID(id string) map[string]any {
	return map[string]any{"id": id, "name": "Entry " + id, "artifact": artifactOf(digestA, 512)}
}

func manyEntries(n int) []byte {
	var b bytes.Buffer

	b.WriteString(`{"formatVersion":2,"revision":"` + testRevision + `","schemas":[`)

	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}

		fmt.Fprintf(&b, `{"id":"e%06d","name":"N","artifact":{"mediaType":%q,"digest":%q,"size":512}}`, i, catalog.ManifestMediaType, digestA)
	}

	b.WriteString(`]}`)

	return b.Bytes()
}

type noLoader struct{}

var errNoLoad = errors.New("the catalog schema must not load external resources")

func (noLoader) Load(string) (any, error) {
	return nil, errNoLoad
}

func compileCatalogSchema(tb testing.TB) *jsonschema.Schema {
	tb.Helper()

	data, err := os.ReadFile(schemaFile)
	if err != nil {
		tb.Fatalf("read catalog schema: %v", err)
	}

	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		tb.Fatalf("decode catalog schema: %v", err)
	}

	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(noLoader{})

	if err := compiler.AddResource(catalogSchemaURL, doc); err != nil {
		tb.Fatalf("add catalog schema: %v", err)
	}

	schema, err := compiler.Compile(catalogSchemaURL)
	if err != nil {
		tb.Fatalf("compile catalog schema: %v", err)
	}

	return schema
}

func validateAgainstSchema(schema *jsonschema.Schema, data []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return err
	}

	return schema.Validate(inst)
}
