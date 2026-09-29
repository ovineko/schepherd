//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/tests/e2e/regproxy"
)

// TestE38_SelfContainedBundle prepares the deps fixture against a local
// dependency server and publishes it: the bundle keeps the dependency's URL
// in its $ref, embeds the dependency under that URL, never makes the client
// contact the dependency server, and check-jsonschema validates with it in
// a container without any network.
func TestE38_SelfContainedBundle(t *testing.T) {
	dep := newDepServer(t)
	depURL := "http://" + dep.Host() + "/dep.json"
	served := readFile(t, fixture("deps", "served", "dep.json"))

	reg := suite.source
	path := repoPath(t)
	repo := reg.Repo(path)

	pub := publishSet(t, repo, newDepsSet(t, dep.Host()), "--now", "20260101.0000")
	if pub.Status != "published" || len(pub.Added) != 2 || pub.UploadedSchemas != 2 {
		t.Fatalf("publish result:\n%s", pub.Raw)
	}

	if n := dep.Hits("/dep.json"); n != 1 {
		t.Fatalf("prepare fetched the dependency %d times, want once", n)
	}

	bundle := preparedSchema(t, pub.Prepared, "deps-root")

	t.Run("the prepared bundle references the dependency URL and embeds it", func(t *testing.T) {
		bundlesCheckBundle(t, bundle, depURL, served)
	})

	catalog := bundlesCatalog(t, reg, path, pub.CatalogDigest)

	t.Run("catalog provenance lists the bundled dependency", func(t *testing.T) {
		for _, id := range []string{"deps-root", "deps-plain"} {
			want := "https://schemas.example.com/e2e/" + id + ".json"
			if got := bundlesProvenanceOf(t, catalog.entry(t, id)).Source; got != want {
				t.Errorf("%s provenance source %q, want the entry URL %s", id, got, want)
			}
		}

		want := []bundlesDependency{{Source: depURL, Digest: digestOf(served)}}
		if got := bundlesProvenanceOf(t, catalog.entry(t, "deps-root")).Dependencies; !reflect.DeepEqual(got, want) {
			t.Fatalf("deps-root provenance dependencies %+v, want %+v", got, want)
		}

		if got := bundlesProvenanceOf(t, catalog.entry(t, "deps-plain")).Dependencies; len(got) != 0 {
			t.Fatalf("deps-plain provenance lists dependencies %+v", got)
		}
	})

	t.Run("the published payload is the bundle", func(t *testing.T) {
		m := reg.Manifest(t, path, catalog.entry(t, "deps-root").Artifact.Digest)
		checkEnvelope(t, m, artifactTypeSchema)
		checkSchemaLayers(t, reg, path, "deps-root", m, bundle, true)
	})

	cfg := writeConfig(t, sandboxOf(t).Workspace, clientConfig{Repository: repo, Catalog: pub.CatalogDigest}.TOML())
	hits := dep.Hits("/dep.json")

	cat := cli(t, runOpts{}, "--config", cfg, "cat", "deps-root").ok(t)
	if !bytes.Equal(cat.Stdout, bundle) {
		t.Fatalf("cat deps-root differs from the prepared bundle\n--- cat ---\n%s\n--- prepared ---\n%s", clip(cat.Stdout), clip(bundle))
	}

	schemaFile := strings.TrimSuffix(string(cli(t, runOpts{}, "--config", cfg, "path", "deps-root").ok(t).Stdout), "\n")
	if !bytes.Equal(readFile(t, schemaFile), bundle) {
		t.Fatalf("materialized %s differs from the prepared bundle", schemaFile)
	}

	rel, err := filepath.Rel(sandboxOf(t).CacheDir, schemaFile)
	if err != nil || !filepath.IsLocal(rel) {
		t.Fatalf("path %s is not below the cache %s (%v)", schemaFile, sandboxOf(t).CacheDir, err)
	}

	mounts := []mount{
		{Host: sandboxOf(t).CacheDir, Container: "/cache", Writable: true},
		{Host: fixture("deps", "instances"), Container: "/instances"},
		{Host: bundlesContainerConfig(t, repo, pub.CatalogDigest), Container: "/config"},
		binMount(),
	}
	containerSchema := "/cache/" + filepath.ToSlash(rel)

	for _, tc := range []struct {
		file string
		code int
	}{
		{"valid.json", 0},
		{"invalid.json", 1},
	} {
		t.Run("check-jsonschema without network/"+tc.file, func(t *testing.T) {
			direct := dockerRunValidators(t, mounts, "check-jsonschema", "--schemafile", containerSchema, "/instances/"+tc.file)
			bundlesWantVerdict(t, direct, tc.code)

			viaRun := dockerRunValidators(t, mounts, "/e2e/bin/schepherd", "--config", "/config/schepherd.toml", "--offline",
				"run", "--schema", "deps-root", "--", "/instances/"+tc.file)
			bundlesWantVerdict(t, viaRun, tc.code)
		})
	}

	if got := dep.Hits("/dep.json"); got != hits {
		t.Fatalf("the dependency server got %d requests after publishing; clients must never contact it", got-hits)
	}
}

// TestE39_DependencyOnlyChange changes only the document the dependency
// server serves and publishes again: prepare with the state reuses the
// recorded artifact of deps-plain and prepares deps-root anew, only deps-root
// gets a new manifest, deps-plain keeps its digest and is not pushed again,
// and the publication is a new catalog revision next to the old one.
func TestE39_DependencyOnlyChange(t *testing.T) {
	dep := newDepServer(t)
	depURL := "http://" + dep.Host() + "/dep.json"
	v1 := readFile(t, fixture("deps", "served", "dep.json"))
	v2 := readFile(t, fixture("deps", "served", "dep-v2.json"))

	px := newProxy(t, suite.source)
	reg := suite.source
	path := repoPath(t)
	repo := px.Repo(path)
	set := newDepsSet(t, dep.Host())
	state := filepath.Join(t.TempDir(), "state.json")

	first := publishSet(t, repo, set, "--now", "20260101.0000", "--state", state, "--state-out", state)
	if first.Status != "published" || first.Revision != "20260101.0000" {
		t.Fatalf("first publication:\n%s", first.Raw)
	}

	hits := dep.Hits("/dep.json")

	dep.Set("/dep.json", v2)
	prepared := prepareSet(t, set)

	if got := dep.Hits("/dep.json"); got != hits+1 {
		t.Fatalf("the second prepare fetched the dependency %d times, want once", got-hits)
	}

	oldRoot, newRoot := preparedSchema(t, first.Prepared, "deps-root"), preparedSchema(t, prepared, "deps-root")

	t.Run("prepared schemas", func(t *testing.T) {
		if bytes.Equal(oldRoot, newRoot) {
			t.Fatal("deps-root did not change with its dependency")
		}

		bundlesCheckBundle(t, newRoot, depURL, v2)

		if !bytes.Equal(preparedSchema(t, first.Prepared, "deps-plain"), preparedSchema(t, prepared, "deps-plain")) {
			t.Fatal("deps-plain changed although it has no dependency")
		}
	})

	weekly := prepareSet(t, set, "--state", state)

	t.Run("the weekly prepare reuses what did not change", func(t *testing.T) {
		var doc struct {
			Entries []struct {
				Reused *struct {
					Digest string `json:"digest"`
				} `json:"reusedArtifact"`
				ID     string `json:"id"`
				Schema string `json:"schema"`
			} `json:"entries"`
		}
		if err := json.Unmarshal(readFile(t, filepath.Join(weekly, "prepared.json")), &doc); err != nil {
			t.Fatal(err)
		}

		plainDigest := bundlesCatalog(t, reg, path, first.CatalogDigest).entry(t, "deps-plain").Artifact.Digest

		for _, e := range doc.Entries {
			switch {
			case e.ID == "deps-plain" && (e.Reused == nil || e.Reused.Digest != plainDigest || e.Schema != ""):
				t.Errorf("deps-plain does not reuse its artifact %s: %+v", plainDigest, e)
			case e.ID == "deps-root" && (e.Reused != nil || !bytes.Equal(preparedSchema(t, weekly, "deps-root"), newRoot)):
				t.Errorf("deps-root was not prepared anew: %+v", e)
			}
		}
	})

	oldCatalog := bundlesCatalog(t, reg, path, first.CatalogDigest)
	existingBlobs := bundlesGraphBlobs(t, reg, path, first.CatalogDigest, oldCatalog)

	px.Reset(t)

	second := publishPrepared(t, repo, weekly, "--now", "20260101.0001", "--state", state)

	records := px.Records(t)
	newCatalog := bundlesCatalog(t, reg, path, second.CatalogDigest)

	oldRootDigest := oldCatalog.entry(t, "deps-root").Artifact.Digest
	newRootDigest := newCatalog.entry(t, "deps-root").Artifact.Digest
	plain := oldCatalog.entry(t, "deps-plain").Artifact

	t.Run("publish result", func(t *testing.T) {
		switch {
		case second.Status != "published":
			t.Errorf("status %q, want published", second.Status)
		case second.Revision != "20260101.0001":
			t.Errorf("revision %q, want the new revision 20260101.0001", second.Revision)
		case second.CatalogDigest == first.CatalogDigest || !digestPattern.MatchString(second.CatalogDigest):
			t.Errorf("catalog digest %q, first catalog %q", second.CatalogDigest, first.CatalogDigest)
		}

		if !slices.Equal(second.Changed, []string{"deps-root"}) || len(second.Added)+len(second.RemovedUpstream) != 0 || second.Unchanged != 1 {
			t.Errorf("added %v, changed %v, removed upstream %v, %d unchanged; want only deps-root changed and one unchanged",
				second.Added, second.Changed, second.RemovedUpstream, second.Unchanged)
		}

		if second.UploadedSchemas != 1 || second.ReusedSchemas != 1 {
			t.Errorf("uploaded %d and reused %d schemas, want 1 uploaded (deps-root) and 1 reused (deps-plain)",
				second.UploadedSchemas, second.ReusedSchemas)
		}

		if !slices.Equal(second.Tags.Created, []string{"catalog-20260101.0001"}) {
			t.Errorf("the result lists created tags %v, want only the new revision tag", second.Tags.Created)
		}

		if t.Failed() {
			t.Logf("publish result:\n%s", second.Raw)
		}
	})

	t.Run("catalog digests", func(t *testing.T) {
		if newRootDigest == oldRootDigest {
			t.Fatalf("deps-root manifest digest did not change: %s", newRootDigest)
		}

		if got := newCatalog.entry(t, "deps-plain").Artifact; got != plain {
			t.Fatalf("deps-plain artifact changed from %+v to %+v", plain, got)
		}

		if oldCatalog.Revision != "20260101.0000" || newCatalog.Revision != "20260101.0001" {
			t.Fatalf("catalog revisions %q and %q, want 20260101.0000 and 20260101.0001", oldCatalog.Revision, newCatalog.Revision)
		}

		for _, tc := range []struct {
			catalog catalogDoc
			served  []byte
		}{{oldCatalog, v1}, {newCatalog, v2}} {
			want := []bundlesDependency{{Source: depURL, Digest: digestOf(tc.served)}}
			if got := bundlesProvenanceOf(t, tc.catalog.entry(t, "deps-root")).Dependencies; !reflect.DeepEqual(got, want) {
				t.Errorf("revision %s: deps-root dependencies %+v, want %+v", tc.catalog.Revision, got, want)
			}
		}
	})

	t.Run("registry tags", func(t *testing.T) {
		want := []string{"catalog-20260101.0000", "catalog-20260101.0001"}

		if got := reg.Tags(t, path); !slices.Equal(got, want) {
			t.Fatalf("tags %v, want %v", got, want)
		}

		for tag, digest := range map[string]string{"catalog-20260101.0000": first.CatalogDigest, "catalog-20260101.0001": second.CatalogDigest} {
			if got := reg.Manifest(t, path, tag).Digest; got != digest {
				t.Errorf("%s points to %s, want %s", tag, got, digest)
			}
		}
	})

	t.Run("only the changed schema was pushed", func(t *testing.T) {
		secondMetadata := reg.Catalog(t, path, second.CatalogDigest).Metadata
		wantPuts := []string{newRootDigest, secondMetadata.Digest, second.CatalogDigest, "catalog-20260101.0001"}
		slices.Sort(wantPuts)

		wantUploads := map[string]int{
			reg.Manifest(t, path, newRootDigest).Manifest.Layers[0].Digest.String(): 1,
			secondMetadata.Manifest.Layers[0].Digest.String():                       1,
		}

		for d := range wantUploads {
			if slices.Contains(existingBlobs, d) {
				t.Fatalf("the new blob %s was already in the first snapshot", d)
			}
		}

		var puts []string

		uploads := map[string]int{}

		for _, rec := range records {
			switch rec.Class {
			case regproxy.ClassManifestPut:
				puts = append(puts, rec.Path[strings.LastIndexByte(rec.Path, '/')+1:])
			case regproxy.ClassBlobUpload:
				query, err := url.ParseQuery(rec.Query)
				if err != nil {
					t.Fatalf("blob upload %s?%s: %v", rec.Path, rec.Query, err)
				}

				if d := query.Get("digest"); d != "" {
					uploads[d]++
				}
			}
		}

		slices.Sort(puts)

		if !slices.Equal(puts, wantPuts) {
			t.Errorf("manifest PUTs %v, want exactly %v", puts, wantPuts)
		}

		if !maps.Equal(uploads, wantUploads) {
			t.Errorf("completed blob uploads %v, want each new blob (payload of deps-root, catalog layer) exactly once: %v; "+
				"blobs of the first snapshot: %v", uploads, wantUploads, existingBlobs)
		}
	})

	t.Run("clients pinned to either revision", func(t *testing.T) {
		ws := sandboxOf(t).Workspace
		oldCfg := writeConfig(t, filepath.Join(ws, "old"), clientConfig{Repository: repo, Catalog: first.CatalogDigest}.TOML())
		newCfg := writeConfig(t, filepath.Join(ws, "new"), clientConfig{Repository: repo, Catalog: second.CatalogDigest}.TOML())

		if got := cli(t, runOpts{}, "--config", oldCfg, "cat", "deps-root").ok(t).Stdout; !bytes.Equal(got, oldRoot) {
			t.Errorf("old pin: cat deps-root differs from the first bundle:\n%s", clip(got))
		}

		if got := cli(t, runOpts{}, "--config", newCfg, "cat", "deps-root").ok(t).Stdout; !bytes.Equal(got, newRoot) {
			t.Errorf("new pin: cat deps-root differs from the second bundle:\n%s", clip(got))
		}

		oldPath := strings.TrimSuffix(string(cli(t, runOpts{}, "--config", oldCfg, "path", "deps-plain").ok(t).Stdout), "\n")
		newPath := strings.TrimSuffix(string(cli(t, runOpts{}, "--config", newCfg, "path", "deps-plain").ok(t).Stdout), "\n")

		if oldPath == "" || oldPath != newPath {
			t.Fatalf("deps-plain resolves to %q with the old pin and %q with the new one", oldPath, newPath)
		}

		if !bytes.Equal(readFile(t, oldPath), preparedSchema(t, first.Prepared, "deps-plain")) {
			t.Errorf("materialized %s differs from the prepared deps-plain", oldPath)
		}
	})
}

type bundlesDependency struct {
	Source string `json:"source"`
	Digest string `json:"digest"`
}

type bundlesProvenance struct {
	Source       string              `json:"source"`
	Dependencies []bundlesDependency `json:"dependencies"`
}

func bundlesProvenanceOf(t *testing.T, e catalogEntry) bundlesProvenance {
	t.Helper()

	if len(e.Provenance) == 0 {
		t.Fatalf("%s has no provenance", e.ID)
	}

	return decodeJSON[bundlesProvenance](t, e.Provenance)
}

// bundlesCatalog reads the catalog document of a catalog index directly
// from the registry.
func bundlesCatalog(t *testing.T, reg *registry, path, digest string) catalogDoc {
	t.Helper()

	return reg.Catalog(t, path, digest).Doc
}

// bundlesGraphBlobs lists the blob digests of a published snapshot: the
// config and layer of the catalog metadata manifest and the config and
// layers of every schema manifest.
func bundlesGraphBlobs(t *testing.T, reg *registry, path, catalogDigest string, catalog catalogDoc) []string {
	t.Helper()

	m := reg.Catalog(t, path, catalogDigest).Metadata
	blobs := []string{m.Manifest.Config.Digest.String()}

	for _, l := range m.Manifest.Layers {
		blobs = append(blobs, l.Digest.String())
	}

	for _, e := range catalog.Schemas {
		sm := reg.Manifest(t, path, e.Artifact.Digest)
		blobs = append(blobs, sm.Manifest.Config.Digest.String())

		for _, l := range sm.Manifest.Layers {
			blobs = append(blobs, l.Digest.String())
		}
	}

	slices.Sort(blobs)

	return slices.Compact(blobs)
}

// bundlesCheckBundle asserts the shape docs/publishing.md promises for a
// bundled schema: the original $ref URL is kept, the dependency is embedded
// once as a $defs resource whose $id is that URL (inserted because the
// served document has none) with the served values, and no other reference
// leaves the document.
func bundlesCheckBundle(t *testing.T, data []byte, depURL string, served []byte) {
	t.Helper()

	doc := decodeJSON[map[string]any](t, data)

	if id := doc["$id"]; id != "https://schemas.example.com/e2e/deps-root.json" {
		t.Errorf("bundle $id %v", id)
	}

	port, _ := doc["properties"].(map[string]any)["port"].(map[string]any)
	if ref := port["$ref"]; ref != depURL {
		t.Errorf("properties.port.$ref = %v, want the dependency URL %s\n%s", ref, depURL, data)
	}

	defs, _ := doc["$defs"].(map[string]any)

	var embedded []map[string]any

	for _, v := range defs {
		if res, ok := v.(map[string]any); ok && res["$id"] == depURL {
			embedded = append(embedded, res)
		}
	}

	if len(embedded) != 1 {
		t.Fatalf("bundle embeds %d resources with $id %s, want 1:\n%s", len(embedded), depURL, data)
	}

	want := decodeJSON[map[string]any](t, served)
	want["$id"] = depURL

	if !reflect.DeepEqual(embedded[0], want) {
		got, _ := json.Marshal(embedded[0])
		t.Errorf("embedded dependency %s, want the served document %s with $id %s", got, served, depURL)
	}

	for _, ref := range bundlesRefs(doc) {
		if ref != depURL && !strings.HasPrefix(ref, "#") {
			t.Errorf("bundle keeps an external reference %q", ref)
		}
	}
}

func bundlesRefs(v any) []string {
	var refs []string

	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if s, ok := child.(string); ok && (k == "$ref" || k == "$dynamicRef") {
				refs = append(refs, s)
			}

			refs = append(refs, bundlesRefs(child)...)
		}
	case []any:
		for _, child := range x {
			refs = append(refs, bundlesRefs(child)...)
		}
	}

	return refs
}

// bundlesContainerConfig writes a configuration for use inside the
// validators container (cache at /cache, check-jsonschema as the batch
// consumer) and returns its directory.
func bundlesContainerConfig(t *testing.T, repo, catalogDigest string) string {
	t.Helper()

	dir := t.TempDir()
	host, _, _ := strings.Cut(repo, "/")

	writeConfig(t, dir, strings.Join([]string{
		"config_version = 1",
		`cache_dir = "/cache"`,
		"",
		"[catalog]",
		"repository = " + tomlString(repo),
		"digest = " + tomlString(catalogDigest),
		"",
		"[registries." + tomlString(host) + "]",
		"plain_http = true",
		"",
		"[runner]",
		`mode = "batch"`,
		`command = "/usr/local/bin/check-jsonschema"`,
		`args = ["--schemafile", "{schema}", "{files...}"]`,
		"",
	}, "\n"))

	return dir
}

// bundlesWantVerdict checks a check-jsonschema verdict: 0 for a valid
// instance, 1 for an invalid one, which must fail on the maximum that only
// the embedded dependency declares. check-jsonschema also exits 1 when it
// cannot resolve a $ref, so the verdict line on stdout is what tells a real
// validation apart from a bundle whose dependency is missing.
func bundlesWantVerdict(t *testing.T, res result, code int) {
	t.Helper()

	res.wantCode(t, code)

	verdict := "ok -- validation done"
	if code != 0 {
		verdict = "$.port: 70000 is greater than the maximum of 65535"
	}

	if !bytes.Contains(res.Stdout, []byte(verdict)) {
		t.Errorf("stdout lacks the verdict %q:\n%s", verdict, res)
	}

	if bytes.Contains(bytes.Join([][]byte{res.Stdout, res.Stderr}, nil), []byte("Failure resolving $ref")) {
		t.Errorf("check-jsonschema could not resolve a reference of the bundle:\n%s", res)
	}
}
