//go:build e2e

package e2e

import (
	"bytes"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/tests/e2e/regproxy"
)

var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// TestE01_RoundTrip publishes set-basic with the real publisher through the
// source proxy, reads the catalog back with the client and inspects every
// artifact directly through the registry HTTP API.
func TestE01_RoundTrip(t *testing.T) {
	path := repoPath(t)
	repo := suite.sourceProxy.Repo(path)

	pub := publishSet(t, repo, newSet(t, "set-basic"), "--now", "20260101.0000")
	prepared := pub.Prepared

	wantIDs := []string{"alpha", "beta", "delta", "dup1", "dup2", "gamma"}

	t.Run("publish result", func(t *testing.T) {
		if pub.Status != "published" || pub.Revision != "20260101.0000" || pub.Repository != repo {
			t.Fatalf("status/revision/repository = %q/%q/%q, want published/20260101.0000/%s\n%s",
				pub.Status, pub.Revision, pub.Repository, repo, pub.Raw)
		}

		if !digestPattern.MatchString(pub.CatalogDigest) || pub.CatalogSize <= 0 {
			t.Fatalf("catalogDigest %q, catalogSize %d", pub.CatalogDigest, pub.CatalogSize)
		}

		if pub.UploadedSchemas != len(wantIDs) || pub.ReusedSchemas != 0 || len(pub.KeptArtifacts) != 0 {
			t.Fatalf("uploaded %d, reused %d and kept %v schemas, want %d uploaded\n%s",
				pub.UploadedSchemas, pub.ReusedSchemas, pub.KeptArtifacts, len(wantIDs), pub.Raw)
		}

		added := slices.Sorted(slices.Values(pub.Added))
		if !slices.Equal(added, wantIDs) || len(pub.Changed)+len(pub.RemovedUpstream)+pub.Unchanged != 0 {
			t.Fatalf("added %v, changed %v, removed upstream %v, %d unchanged; want all of %v added",
				pub.Added, pub.Changed, pub.RemovedUpstream, pub.Unchanged, wantIDs)
		}
	})

	res := cli(t, runOpts{}, "--config", writeConfig(t, sandboxOf(t).Workspace,
		clientConfig{Repository: repo, Catalog: pub.CatalogDigest}.TOML()), "catalog", "--json").ok(t)

	reg := suite.source
	fetched := reg.Catalog(t, path, pub.CatalogDigest)
	checkEnvelope(t, fetched.Metadata, artifactTypeMetadata)
	checkIndex(t, fetched)

	if pub.CatalogSize != int64(len(fetched.Index.Body)) {
		t.Errorf("catalogSize %d, index has %d bytes", pub.CatalogSize, len(fetched.Index.Body))
	}

	catalogLayer := fetched.Metadata.Manifest.Layers[0]
	catalogBytes := fetched.Blob

	t.Run("catalog --json prints the catalog blob byte for byte", func(t *testing.T) {
		if catalogLayer.Size != int64(len(catalogBytes)) {
			t.Fatalf("catalog layer declares %d bytes, blob has %d", catalogLayer.Size, len(catalogBytes))
		}

		if !bytes.Equal(res.Stdout, catalogBytes) {
			t.Fatalf("catalog --json output differs from the catalog blob\n--- stdout ---\n%s\n--- blob ---\n%s", clip(res.Stdout), clip(catalogBytes))
		}
	})

	catalog := decodeJSON[catalogDoc](t, catalogBytes)

	t.Run("catalog document", func(t *testing.T) {
		if catalog.FormatVersion != catalogFormatVersion || catalog.Revision != pub.Revision {
			t.Fatalf("formatVersion %d, revision %q", catalog.FormatVersion, catalog.Revision)
		}

		ids := make([]string, 0, len(catalog.Schemas))
		for _, e := range catalog.Schemas {
			ids = append(ids, e.ID)
		}

		if !slices.Equal(ids, wantIDs) {
			t.Fatalf("catalog IDs %v, want %v in this order", ids, wantIDs)
		}

		wantFileMatch := map[string][]string{
			"alpha": {"alpha.json", "**/.github/workflows/*.yml"},
			"beta":  {"config/**/*.toml", "beta.yaml"},
			"gamma": {"gamma.json"},
			"dup1":  {"compose.yml"},
			"dup2":  {"compose.yml"},
		}
		wantDialect := map[string]string{
			"alpha": "http://json-schema.org/draft-07/schema#",
			"beta":  "https://json-schema.org/draft/2020-12/schema",
			"delta": "https://json-schema.org/draft/2019-09/schema",
		}

		for _, e := range catalog.Schemas {
			if !slices.Equal(e.FileMatch, wantFileMatch[e.ID]) {
				t.Errorf("%s: fileMatch %q, want %q", e.ID, e.FileMatch, wantFileMatch[e.ID])
			}

			if want, ok := wantDialect[e.ID]; ok && e.Dialect != want {
				t.Errorf("%s: dialect %q, want %q", e.ID, e.Dialect, want)
			}

			if e.Artifact.MediaType != mediaTypeManifest || !digestPattern.MatchString(e.Artifact.Digest) {
				t.Errorf("%s: artifact %+v", e.ID, e.Artifact)
			}
		}
	})

	t.Run("schema artifacts", func(t *testing.T) {
		for _, e := range catalog.Schemas {
			m := reg.Manifest(t, path, e.Artifact.Digest)
			if int64(len(m.Body)) != e.Artifact.Size {
				t.Errorf("%s: manifest has %d bytes, catalog says %d", e.ID, len(m.Body), e.Artifact.Size)
			}

			checkEnvelope(t, m, artifactTypeSchema)
			// The fixture policy carries a notice, so every artifact has
			// the notice layer.
			checkSchemaLayers(t, reg, path, e.ID, m, preparedSchema(t, prepared, e.ID), true)
		}
	})

	t.Run("gamma is gzip-packed, alpha is not", func(t *testing.T) {
		gamma := reg.Manifest(t, path, catalog.entry(t, "gamma").Artifact.Digest).Manifest.Layers[0]
		alpha := reg.Manifest(t, path, catalog.entry(t, "alpha").Artifact.Digest).Manifest.Layers[0]

		if gamma.MediaType != mediaTypeSchemaGzip || alpha.MediaType != mediaTypeSchemaJSON {
			t.Fatalf("payload media types gamma=%q alpha=%q", gamma.MediaType, alpha.MediaType)
		}
	})

	t.Run("the index references every schema artifact", func(t *testing.T) {
		want := map[string]descriptorDoc{}
		for _, e := range catalog.Schemas {
			want[e.Artifact.Digest] = e.Artifact
		}

		got := map[string]descriptorDoc{}
		for _, d := range fetched.schemaChildren() {
			got[d.Digest.String()] = descriptorOf(d)
		}

		if !maps.Equal(got, want) || len(got) == 0 {
			t.Fatalf("index schema children %v, want the distinct catalog artifacts %v", got, want)
		}
	})

	t.Run("tags", func(t *testing.T) {
		tags := reg.Tags(t, path)
		want := []string{"catalog-" + pub.Revision}

		if !slices.Equal(tags, want) {
			t.Fatalf("tags %v, want %v", tags, want)
		}

		for _, tag := range want {
			if !slices.Contains(pub.Tags.Created, tag) {
				t.Errorf("publish result does not list created tag %s: %v", tag, pub.Tags.Created)
			}
		}

		if got := reg.Manifest(t, path, "catalog-"+pub.Revision).Digest; got != pub.CatalogDigest {
			t.Fatalf("catalog-%s points to %s, want %s", pub.Revision, got, pub.CatalogDigest)
		}
	})

	t.Run("the client read through the proxy", func(t *testing.T) {
		suite.sourceProxy.Reset(t)
		cli(t, runOpts{}, "--config", writeConfig(t, sandboxOf(t).Workspace,
			clientConfig{Repository: repo, Catalog: pub.CatalogDigest}.TOML()), "list", "--json").ok(t)

		stats := suite.sourceProxy.Stats(t)
		if stats.Total == 0 || stats.ByClass[regproxy.ClassManifestPut] != 0 || stats.ByClass[regproxy.ClassBlobUpload] != 0 {
			t.Fatalf("proxy stats %+v", stats)
		}
	})
}

// checkIndex asserts the envelope of a catalog index (docs/oci-format.md):
// no annotations or subject, the metadata manifest first, then distinct,
// untyped schema manifest descriptors in ascending digest order.
func checkIndex(t *testing.T, c fetchedCatalog) {
	t.Helper()

	ix := c.Index.Index
	if ix.Subject != nil || len(ix.Annotations) != 0 {
		t.Errorf("catalog index carries a subject or annotations:\n%s", c.Index.Body)
	}

	metadata := ix.Manifests[0]
	if metadata.Digest.String() != c.Metadata.Digest || metadata.Size != int64(len(c.Metadata.Body)) {
		t.Errorf("metadata child %+v does not describe the metadata manifest %s (%d bytes)", metadata, c.Metadata.Digest, len(c.Metadata.Body))
	}

	for i, d := range c.schemaChildren() {
		if d.MediaType != mediaTypeManifest || d.ArtifactType != "" || d.Platform != nil || len(d.Annotations) != 0 || len(d.URLs) != 0 {
			t.Errorf("schema child %d is not a plain manifest descriptor: %+v", i, d)
		}

		if i > 0 && c.schemaChildren()[i-1].Digest >= d.Digest {
			t.Errorf("schema children are not distinct and sorted by digest:\n%s", c.Index.Body)
		}
	}
}

// checkEnvelope asserts the manifest envelope shared by schema and catalog
// metadata artifacts (docs/oci-format.md).
func checkEnvelope(t *testing.T, m fetchedManifest, artifactType string) {
	t.Helper()

	man := m.Manifest

	switch {
	case m.ContentType != mediaTypeManifest:
		t.Errorf("Content-Type %q, want %q", m.ContentType, mediaTypeManifest)
	case man.SchemaVersion != 2 || man.MediaType != mediaTypeManifest:
		t.Errorf("schemaVersion %d, mediaType %q", man.SchemaVersion, man.MediaType)
	case man.ArtifactType != artifactType:
		t.Errorf("artifactType %q, want %q", man.ArtifactType, artifactType)
	case man.Config.MediaType != mediaTypeEmpty || man.Config.Digest.String() != emptyConfigDigest ||
		man.Config.Size != 2 || string(man.Config.Data) != "{}":
		t.Errorf("config is not the OCI empty descriptor: %+v", man.Config)
	case man.Subject != nil || len(man.Annotations) != 0:
		t.Errorf("unexpected subject or manifest annotations:\n%s", m.Body)
	case bytes.Contains(m.Body, []byte("org.opencontainers.image.created")):
		t.Errorf("manifest carries a creation time:\n%s", m.Body)
	}
}

// checkSchemaLayers asserts the payload layer (and the optional notice
// layer) of a schema artifact against the prepared schema bytes.
func checkSchemaLayers(t *testing.T, reg *registry, path, id string, m fetchedManifest, prepared []byte, withNotice bool) {
	t.Helper()

	layers := m.Manifest.Layers

	want := 1
	if withNotice {
		want = 2
	}

	if len(layers) != want {
		t.Errorf("%s: %d layers, want %d:\n%s", id, len(layers), want, m.Body)

		return
	}

	payload := layers[0]
	wantAnnotations := map[string]string{
		annotationContentDigest: digestOf(prepared),
		annotationContentSize:   strconv.Itoa(len(prepared)),
		annotationTitle:         payloadTitle(payload.MediaType),
	}

	for k, want := range wantAnnotations {
		if got := payload.Annotations[k]; got != want {
			t.Errorf("%s: payload annotation %s = %q, want %q", id, k, got, want)
		}
	}

	if len(payload.URLs) != 0 || payload.Data != nil {
		t.Errorf("%s: payload descriptor declares urls or data: %+v", id, payload)
	}

	blob := reg.Blob(t, path, payload.Digest.String())

	switch payload.MediaType {
	case mediaTypeSchemaJSON:
		if !bytes.Equal(blob, prepared) {
			t.Errorf("%s: uncompressed payload differs from the prepared schema", id)
		}
	case mediaTypeSchemaGzip:
		if len(blob) >= len(prepared) {
			t.Errorf("%s: gzip payload (%d bytes) is not smaller than the schema (%d bytes)", id, len(blob), len(prepared))
		}
	default:
		t.Errorf("%s: payload media type %q", id, payload.MediaType)
	}

	if len(layers) == 2 {
		checkNoticeLayer(t, reg, path, id, layers[1])
	}
}

func checkNoticeLayer(t *testing.T, reg *registry, path, id string, notice ocispec.Descriptor) {
	t.Helper()

	if notice.MediaType != mediaTypeNotice {
		t.Errorf("%s: second layer media type %q, want %q", id, notice.MediaType, mediaTypeNotice)

		return
	}

	if text := reg.Blob(t, path, notice.Digest.String()); !bytes.Contains(text, []byte("MIT")) {
		t.Errorf("%s: notice does not mention the fixture license:\n%s", id, text)
	}
}
