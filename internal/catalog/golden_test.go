package catalog_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/jsonutil"
)

var update = flag.Bool("update", false, "regenerate testdata/catalogs from testdata/schemas")

const regenerate = "go test ./internal/catalog -run TestGolden -update"

type fixture struct {
	packed       *artifact.Packed
	artifact     catalog.Descriptor
	sourceDigest string
}

// packFixture packs a schema from testdata/schemas the way the publisher
// does: compact JSON, then the schepherd-pack/1 recipe.
func packFixture(tb testing.TB, name, notice string) fixture {
	tb.Helper()

	raw, err := os.ReadFile(filepath.Join(schemasDir, name))
	if err != nil {
		tb.Fatalf("read fixture schema: %v", err)
	}

	compact, err := jsonutil.Compact(raw, 0)
	if err != nil {
		tb.Fatalf("compact %s: %v", name, err)
	}

	var noticeBytes []byte
	if notice != "" {
		if noticeBytes, err = os.ReadFile(filepath.Join(schemasDir, notice)); err != nil {
			tb.Fatalf("read fixture notice: %v", err)
		}
	}

	packed, err := artifact.PackSchema(compact, noticeBytes)
	if err != nil {
		tb.Fatalf("PackSchema(%s) error = %v", name, err)
	}

	manifest := packed.Manifest.Descriptor

	return fixture{
		packed:       packed,
		artifact:     catalog.Descriptor{MediaType: manifest.MediaType, Digest: manifest.Digest.String(), Size: manifest.Size},
		sourceDigest: digest.FromBytes(raw),
	}
}

type fixtures struct {
	any, tool, service, large fixture
}

func loadFixtures(tb testing.TB) fixtures {
	tb.Helper()

	return fixtures{
		any:     packFixture(tb, "boolean-true.json", ""),
		tool:    packFixture(tb, "draft-07-object.json", ""),
		service: packFixture(tb, "draft-2019-09-refs.json", "draft-2019-09-refs.notice.txt"),
		large:   packFixture(tb, "draft-2020-12-large.json", ""),
	}
}

// goldenCatalogs returns every file of testdata/catalogs as the catalog it
// must decode to. The artifact descriptors are the real manifests of the
// packed fixture schemas.
func goldenCatalogs(fx fixtures) map[string]*catalog.Catalog {
	const example = "https://schemas.example.com/"

	return map[string]*catalog.Catalog{
		"minimal.json": {
			FormatVersion: 2,
			Revision:      testRevision,
			Schemas:       []catalog.Entry{{ID: "any", Name: "Any JSON value", Artifact: fx.any.artifact}},
		},
		"full.json": {
			FormatVersion: 2,
			Revision:      testRevision,
			Schemas: []catalog.Entry{
				{
					ID:          "any",
					Name:        "Any JSON value",
					Description: "Accepts every JSON document. Reachable by ID only.",
					Artifact:    fx.any.artifact,
				},
				{
					ID:        "example-service",
					Name:      "Example service definition",
					Dialect:   "https://json-schema.org/draft/2019-09/schema",
					FileMatch: []string{"service.json", "/deploy/*.service.json"},
					Artifact:  fx.service.artifact,
					Provenance: &catalog.Provenance{
						Source:       example + "service.json",
						SourceDigest: fx.service.sourceDigest,
						License:      "CC0-1.0",
					},
				},
				{
					ID:          "example-tool",
					Name:        "Example tool configuration",
					Description: "Configuration file of a fictional command-line tool.",
					Dialect:     "http://json-schema.org/draft-07/schema#",
					FileMatch:   []string{"example-tool.json", ".example-toolrc.json"},
					Artifact:    fx.tool.artifact,
					Provenance: &catalog.Provenance{
						Source:       example + "tool.json",
						SourceDigest: fx.tool.sourceDigest,
						License:      "MIT",
					},
				},
				{
					ID:        "example-tool-legacy",
					Name:      "Example tool configuration (legacy file name)",
					FileMatch: []string{"example-tool.legacy.json"},
					Artifact:  fx.tool.artifact,
				},
				{
					ID:          "large-config",
					Name:        "Example large configuration",
					Description: "A schema above the gzip threshold.\nIts payload is stored compressed.",
					Dialect:     "https://json-schema.org/draft/2020-12/schema",
					FileMatch:   []string{"large-config.json", "config/large-*.json", "!**/node_modules/**"},
					Artifact:    fx.large.artifact,
					Provenance: &catalog.Provenance{
						Source:       example + "large-config.json",
						SourceDigest: fx.large.sourceDigest,
						License:      "Apache-2.0",
						Dependencies: []catalog.Dependency{
							{Source: example + "service.json", Digest: fx.service.sourceDigest},
							{Source: example + "tool.json", Digest: fx.tool.sourceDigest},
						},
					},
				},
			},
		},
	}
}

func TestGoldenFixtureEncodings(t *testing.T) {
	t.Parallel()

	fx := loadFixtures(t)

	for name, tc := range map[string]struct {
		fx        fixture
		mediaType string
		layers    int
	}{
		"boolean-true.json":        {fx: fx.any, mediaType: artifact.SchemaMediaType, layers: 1},
		"draft-07-object.json":     {fx: fx.tool, mediaType: artifact.SchemaMediaType, layers: 1},
		"draft-2019-09-refs.json":  {fx: fx.service, mediaType: artifact.SchemaMediaType, layers: 2},
		"draft-2020-12-large.json": {fx: fx.large, mediaType: artifact.SchemaGzipMediaType, layers: 1},
	} {
		sm, err := artifact.ParseSchemaManifest(tc.fx.packed.Manifest.Data, artifact.DefaultLimits())
		if err != nil {
			t.Fatalf("%s: ParseSchemaManifest() error = %v", name, err)
		}

		layers := 1
		if sm.Notice != nil {
			layers = 2
		}

		if sm.Payload.MediaType != tc.mediaType || layers != tc.layers {
			t.Fatalf("%s: payload %s with %d layers, want %s with %d", name, sm.Payload.MediaType, layers, tc.mediaType, tc.layers)
		}
	}

	if fx.large.packed.ContentSize <= artifact.GzipThreshold {
		t.Fatalf("draft-2020-12-large.json is %d bytes compact, want more than %d", fx.large.packed.ContentSize, artifact.GzipThreshold)
	}
}

func TestGolden(t *testing.T) {
	want := goldenCatalogs(loadFixtures(t))

	if *update {
		writeGoldens(t, want)
	}

	files, err := filepath.Glob(filepath.Join(catalogsDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, filepath.Base(f))
	}

	wantNames := make([]string, 0, len(want))
	for name := range want {
		wantNames = append(wantNames, name)
	}

	slices.Sort(wantNames)

	if !slices.Equal(names, wantNames) {
		t.Fatalf("testdata/catalogs holds %v, the generator produces %v; run %s", names, wantNames, regenerate)
	}

	for _, name := range wantNames {
		c := want[name]
		t.Run(name, func(t *testing.T) {
			canonical := mustMarshal(t, c)

			data, err := os.ReadFile(filepath.Join(catalogsDir, name))
			if err != nil {
				t.Fatal(err)
			}

			var compact bytes.Buffer
			if err := json.Compact(&compact, data); err != nil {
				t.Fatalf("golden file is not JSON: %v", err)
			}

			if !bytes.Equal(compact.Bytes(), canonical) {
				t.Fatalf("golden file drifted from PackSchema/Marshal output; run %s\ngot  %s\nwant %s", regenerate, compact.Bytes(), canonical)
			}

			parsed, err := catalog.Parse(data, catalog.DefaultLimits())
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}

			if !reflect.DeepEqual(parsed, c) {
				t.Fatalf("Parse() =\n%#v\nwant\n%#v", parsed, c)
			}
		})
	}
}

// writeGoldens stores each catalog indented with two spaces and sorted
// members, the layout the repository's JSON formatters converge on, so
// formatting the files never changes them.
func writeGoldens(tb testing.TB, want map[string]*catalog.Catalog) {
	tb.Helper()

	if err := os.MkdirAll(catalogsDir, 0o755); err != nil {
		tb.Fatal(err)
	}

	stale, err := filepath.Glob(filepath.Join(catalogsDir, "*.json"))
	if err != nil {
		tb.Fatal(err)
	}

	for _, path := range stale {
		if _, keep := want[filepath.Base(path)]; !keep {
			if err := os.Remove(path); err != nil {
				tb.Fatal(err)
			}
		}
	}

	for name, c := range want {
		var indented bytes.Buffer
		if err := json.Indent(&indented, mustMarshal(tb, c), "", "  "); err != nil {
			tb.Fatal(err)
		}

		indented.WriteByte('\n')

		if err := os.WriteFile(filepath.Join(catalogsDir, name), indented.Bytes(), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
}

func TestGoldenRules(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join(catalogsDir, "full.json"))
	if err != nil {
		t.Fatal(err)
	}

	c, err := catalog.Parse(data, catalog.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	items := c.Patterns()

	got := make([]string, 0, len(items))
	for _, item := range items {
		got = append(got, item.ID+"\t"+strings.Join(item.FileMatch, " "))
	}

	want := []string{
		"example-service\tservice.json /deploy/*.service.json",
		"example-tool\texample-tool.json .example-toolrc.json",
		"example-tool-legacy\texample-tool.legacy.json",
		"large-config\tlarge-config.json config/large-*.json !**/node_modules/**",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Patterns() = %q, want %q", got, want)
	}

	tool, _ := c.Lookup("example-tool")
	legacy, _ := c.Lookup("example-tool-legacy")

	if tool == nil || legacy == nil || tool.Artifact != legacy.Artifact {
		t.Fatalf("aliases must share one artifact: %+v, %+v", tool, legacy)
	}
}
