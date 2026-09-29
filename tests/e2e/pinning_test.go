//go:build e2e

package e2e

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/tests/e2e/regproxy"
)

// pinningMentionsPin matches the hint that points from a refused tag to the
// "schepherd pin" command.
var pinningMentionsPin = regexp.MustCompile(`\bschepherd pin\b`)

// TestE07_OneSchemaChanged publishes set-basic and then set-basic-v2, which
// differs only in beta, as a second snapshot of the same repository. Both
// snapshots stay usable side by side: each pin gets its own beta bytes while
// the unchanged schemas keep their manifests, and the second publication
// writes nothing but the new beta artifact and the new catalog.
func TestE07_OneSchemaChanged(t *testing.T) {
	px := newProxy(t, suite.source)
	reg := suite.source
	path := repoPath(t)
	repo := px.Repo(path)
	state := filepath.Join(t.TempDir(), "state.json")

	v1 := publishSet(t, repo, newSet(t, "set-basic"), "--now", "20260101.0000", "--state", state, "--state-out", state)
	prepared2 := prepareSet(t, newSet(t, "set-basic-v2"))
	tagsBefore := reg.Tags(t, path)

	px.Reset(t)

	v2 := publishPrepared(t, repo, prepared2, "--now", "20260201.0000", "--state", state)
	records, stats := px.Records(t), px.Stats(t)

	cat1, _, _ := pinningCatalog(t, reg, path, v1.CatalogDigest)
	cat2, _, cat2Metadata := pinningCatalog(t, reg, path, v2.CatalogDigest)
	old, cur := pinningArtifacts(cat1), pinningArtifacts(cat2)

	ids := slices.Sorted(maps.Keys(old))
	beta1, beta2 := preparedSchema(t, v1.Prepared, "beta"), preparedSchema(t, prepared2, "beta")
	betaManifest := reg.Manifest(t, path, cur["beta"].Digest)

	t.Run("publish result", func(t *testing.T) {
		if v2.Status != "published" || v2.Revision != "20260201.0000" {
			t.Fatalf("status/revision = %q/%q, want published/20260201.0000\n%s", v2.Status, v2.Revision, v2.Raw)
		}

		if !digestPattern.MatchString(v2.CatalogDigest) || v2.CatalogDigest == v1.CatalogDigest {
			t.Fatalf("catalogDigest %q (first snapshot %s)", v2.CatalogDigest, v1.CatalogDigest)
		}

		if !slices.Equal(v2.Changed, []string{"beta"}) || len(v2.Added)+len(v2.RemovedUpstream) != 0 || v2.Unchanged != len(ids)-1 {
			t.Fatalf("added %v, changed %v, removed upstream %v, %d unchanged; want only beta changed and %d unchanged\n%s",
				v2.Added, v2.Changed, v2.RemovedUpstream, v2.Unchanged, len(ids)-1, v2.Raw)
		}

		if v2.UploadedSchemas != 1 || v2.ReusedSchemas != len(ids)-1 {
			t.Fatalf("uploaded %d and reused %d schemas, want 1 uploaded and %d reused\n%s", v2.UploadedSchemas, v2.ReusedSchemas, len(ids)-1, v2.Raw)
		}
	})

	t.Run("only beta's manifest digest changed", func(t *testing.T) {
		if len(ids) != 6 || !slices.Contains(ids, "beta") {
			t.Fatalf("first catalog lists %v, want the six set-basic schemas", ids)
		}

		if got := slices.Sorted(maps.Keys(cur)); !slices.Equal(got, ids) {
			t.Fatalf("second catalog lists %v, first %v", got, ids)
		}

		for _, id := range ids {
			switch {
			case id == "beta" && cur[id].Digest == old[id].Digest:
				t.Errorf("beta kept manifest %s although its schema changed", old[id].Digest)
			case id != "beta" && cur[id] != old[id]:
				t.Errorf("%s: artifact %+v became %+v although its schema did not change", id, old[id], cur[id])
			}

			same := bytes.Equal(preparedSchema(t, v1.Prepared, id), preparedSchema(t, prepared2, id))
			if same != (id != "beta") {
				t.Errorf("%s: prepared bytes identical in both sets = %v", id, same)
			}
		}

		checkEnvelope(t, betaManifest, artifactTypeSchema)
		checkSchemaLayers(t, reg, path, "beta", betaManifest, beta2, true)
	})

	t.Run("exactly one new schema manifest was pushed", func(t *testing.T) {
		if stats.ByClass[regproxy.ClassManifestDelete] != 0 {
			t.Fatalf("the publisher deleted manifests: %+v", stats)
		}

		written := pinningManifestWrites(t, records, map[string]string{"catalog-" + v2.Revision: v2.CatalogDigest})
		want := []string{cur["beta"].Digest, cat2Metadata.Digest, v2.CatalogDigest}
		slices.Sort(want)

		if !slices.Equal(written, want) {
			t.Fatalf("manifests written (by digest or tag) %v, want only the new beta %s, the new catalog metadata %s and index %s",
				written, cur["beta"].Digest, cat2Metadata.Digest, v2.CatalogDigest)
		}

		uploaded := pinningBlobUploads(t, records)
		wantBlobs := []string{betaManifest.Manifest.Layers[0].Digest.String(), cat2Metadata.Manifest.Layers[0].Digest.String()}
		slices.Sort(wantBlobs)

		if !slices.Equal(uploaded, wantBlobs) {
			t.Fatalf("blobs uploaded %v, want one upload each of the new beta payload and the new catalog document %v", uploaded, wantBlobs)
		}
	})

	t.Run("tags", func(t *testing.T) {
		added := []string{"catalog-" + v2.Revision}

		if created := slices.Sorted(slices.Values(v2.Tags.Created)); !slices.Equal(created, added) {
			t.Errorf("publish result lists created tags %v, want %v", created, added)
		}

		want := slices.Sorted(slices.Values(append(slices.Clone(tagsBefore), added...)))
		if got := reg.Tags(t, path); !slices.Equal(got, want) {
			t.Errorf("tags after the second snapshot %v, want %v", got, want)
		}

		if got := reg.Manifest(t, path, "catalog-"+v1.Revision).Digest; got != v1.CatalogDigest {
			t.Errorf("catalog-%s moved to %s", v1.Revision, got)
		}

		pinningCheckTagTargets(t, reg, path, map[string]string{v1.Revision: v1.CatalogDigest, v2.Revision: v2.CatalogDigest})
	})

	t.Run("old pin gets old beta, new pin gets new beta", func(t *testing.T) {
		if bytes.Equal(beta1, beta2) {
			t.Fatal("the fixture sets do not differ in beta")
		}

		oldCfg := pinningClientConfig(t, repo, v1.CatalogDigest, "")
		newCfg := pinningClientConfig(t, repo, v2.CatalogDigest, "")

		for i, step := range []struct {
			cfg  string
			want []byte
		}{{oldCfg, beta1}, {newCfg, beta2}, {oldCfg, beta1}} {
			res := cli(t, runOpts{}, "--config", step.cfg, "cat", "beta").ok(t)
			if !bytes.Equal(res.Stdout, step.want) {
				t.Fatalf("step %d: cat beta printed\n%s\nwant\n%s", i, clip(res.Stdout), clip(step.want))
			}
		}

		oldBeta := pinningPath(t, oldCfg, "beta")
		newBeta := pinningPath(t, newCfg, "beta")

		if oldBeta == newBeta {
			t.Fatalf("both pins materialize beta at %s", oldBeta)
		}

		if !bytes.Equal(readFile(t, oldBeta), beta1) || !bytes.Equal(readFile(t, newBeta), beta2) {
			t.Fatalf("materialized beta files do not hold the bytes of their snapshots:\n%s\n%s", oldBeta, newBeta)
		}

		if a, b := pinningPath(t, oldCfg, "alpha"), pinningPath(t, newCfg, "alpha"); a != b {
			t.Fatalf("unchanged alpha has different paths under the two pins: %s and %s", a, b)
		}
	})
}

// TestE08_Noop prepares and publishes set-basic again on top of the state
// of its first publication: nothing changed, so the publisher reports the
// recorded catalog without contacting the registry, and with
// --update-latest it only reads the revision tag and catalog-latest, which
// already point at that catalog.
func TestE08_Noop(t *testing.T) {
	px := newProxy(t, suite.source)
	reg := suite.source
	path := repoPath(t)
	repo := px.Repo(path)
	state := filepath.Join(t.TempDir(), "state.json")

	first := publishSet(t, repo, newSet(t, "set-basic"), "--now", "20260101.0000", "--update-latest", "--state", state, "--state-out", state)
	if first.Status != "published" {
		t.Fatalf("first publication: status %q\n%s", first.Status, first.Raw)
	}

	tagsBefore := reg.Tags(t, path)
	if !slices.Contains(tagsBefore, "catalog-latest") {
		t.Fatalf("first publication did not create catalog-latest: %v", tagsBefore)
	}

	for _, tc := range []struct {
		name   string
		extra  []string
		latest bool
	}{
		{"a week later with --update-latest", []string{"--now", "20260108.0000", "--state", state, "--update-latest"}, true},
		{"the same minute", []string{"--now", "20260101.0000", "--state", state}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepared := prepareSet(t, newSet(t, "set-basic"))

			px.Reset(t)
			res := publishPrepared(t, repo, prepared, tc.extra...)
			records := px.Records(t)

			if res.Status != "noop" {
				t.Fatalf("status %q, want noop\n%s", res.Status, res.Raw)
			}

			if res.CatalogDigest != first.CatalogDigest || res.Revision != first.Revision {
				t.Errorf("catalogDigest/revision = %q/%q, want the recorded snapshot %s (revision %s)\n%s",
					res.CatalogDigest, res.Revision, first.CatalogDigest, first.Revision, res.Raw)
			}

			if len(res.Tags.Created) != 0 || res.UploadedSchemas != 0 {
				t.Errorf("a noop reports created tags %v and %d uploaded schemas\n%s", res.Tags.Created, res.UploadedSchemas, res.Raw)
			}

			if len(res.Added)+len(res.Changed)+len(res.RemovedUpstream) != 0 || res.Unchanged != len(first.Added) {
				t.Errorf("added %v, changed %v, removed upstream %v, %d unchanged; want all %d unchanged",
					res.Added, res.Changed, res.RemovedUpstream, res.Unchanged, len(first.Added))
			}

			var read []string

			for _, rec := range records {
				switch rec.Class {
				case regproxy.ClassManifestGet, regproxy.ClassManifestHead:
					read = append(read, rec.Path[strings.LastIndexByte(rec.Path, '/')+1:])
				default:
					t.Errorf("a noop sent %s %s (%s)", rec.Method, rec.Path, rec.Class)
				}
			}

			var want []string
			if tc.latest {
				want = []string{"catalog-" + first.Revision, "catalog-latest"}

				if !slices.Equal(res.Tags.Existing, []string{"catalog-latest"}) {
					t.Errorf("existing tags %v, want [catalog-latest]\n%s", res.Tags.Existing, res.Raw)
				}
			}

			if read = slices.Compact(slices.Sorted(slices.Values(read))); !slices.Equal(read, want) {
				t.Errorf("a noop read the manifests %v, want %v", read, want)
			}

			if got := reg.Tags(t, path); !slices.Equal(got, tagsBefore) {
				t.Errorf("tags changed from %v to %v", tagsBefore, got)
			}

			if got := reg.Manifest(t, path, "catalog-latest").Digest; got != first.CatalogDigest {
				t.Errorf("catalog-latest moved to %s", got)
			}
		})
	}
}

// TestE09_MutableTag moves catalog-latest to a second snapshot. Only the
// explicit pin command follows the tag; a client pinned by digest keeps
// getting the old snapshot, and every runtime command refuses a tag in
// place of the catalog digest before it touches the network.
func TestE09_MutableTag(t *testing.T) {
	px := newProxy(t, suite.source)
	reg := suite.source
	path := repoPath(t)
	repo := px.Repo(path)
	state := filepath.Join(t.TempDir(), "state.json")

	v1 := publishSet(t, repo, newSet(t, "set-basic"), "--now", "20260101.0000", "--state", state, "--state-out", state, "--update-latest")
	beta1 := preparedSchema(t, v1.Prepared, "beta")

	pinCfg := writeConfig(t, t.TempDir(), clientConfig{}.TOML())
	oldCfg := pinningClientConfig(t, repo, v1.CatalogDigest, "")

	t.Run("pin resolves the first snapshot", func(t *testing.T) {
		pinningPinLatest(t, pinCfg, repo, v1.CatalogDigest, v1.Revision)
	})

	warm := newSandbox(t)
	if res := cli(t, runOpts{Sandbox: warm}, "--config", oldCfg, "cat", "beta").ok(t); !bytes.Equal(res.Stdout, beta1) {
		t.Fatalf("cat beta before the second snapshot printed\n%s", clip(res.Stdout))
	}

	v2 := publishSet(t, repo, newSet(t, "set-basic-v2"), "--now", "20260102.0000", "--state", state, "--update-latest")
	beta2 := preparedSchema(t, v2.Prepared, "beta")

	t.Run("second snapshot moved catalog-latest", func(t *testing.T) {
		if v2.Status != "published" || !slices.Equal(v2.Changed, []string{"beta"}) {
			t.Fatalf("second publication: %s", v2.Raw)
		}

		if !slices.Contains(v2.Tags.Created, "catalog-latest") {
			t.Errorf("publish result does not list catalog-latest as created (moved): %v", v2.Tags.Created)
		}

		if got := reg.Manifest(t, path, "catalog-latest").Digest; got != v2.CatalogDigest {
			t.Fatalf("catalog-latest points to %s, want %s", got, v2.CatalogDigest)
		}

		if got := reg.Manifest(t, path, "catalog-"+v1.Revision).Digest; got != v1.CatalogDigest {
			t.Fatalf("catalog-%s moved to %s", v1.Revision, got)
		}
	})

	t.Run("pin resolves the new digest and changes nothing on disk", func(t *testing.T) {
		sb := newSandbox(t)
		before := fileTree(t, sb.Dir)

		pinningPinLatest(t, pinCfg, repo, v2.CatalogDigest, v2.Revision, sb)

		if after := fileTree(t, sb.Dir); !slices.Equal(after, before) {
			t.Fatalf("pin changed the sandbox:\nbefore %v\nafter  %v", before, after)
		}
	})

	t.Run("a client pinned to the old digest still gets old bytes", func(t *testing.T) {
		for _, c := range []struct {
			name string
			sb   *sandbox
			cold bool
		}{{"warm cache", warm, false}, {"cold cache", newSandbox(t), true}} {
			px.Reset(t)

			res := cli(t, runOpts{Sandbox: c.sb}, "--config", oldCfg, "cat", "beta").ok(t)
			if !bytes.Equal(res.Stdout, beta1) {
				t.Errorf("%s: cat beta printed\n%s\nwant the first snapshot's\n%s", c.name, clip(res.Stdout), clip(beta1))
			}

			res = cli(t, runOpts{Sandbox: c.sb}, "--config", oldCfg, "catalog", "--json").ok(t)
			if got := decodeJSON[catalogDoc](t, res.Stdout).Revision; got != v1.Revision {
				t.Errorf("%s: catalog revision %q, want %q", c.name, got, v1.Revision)
			}

			if byDigest := pinningNoTagLookups(t, px.Records(t)); c.cold && byDigest == 0 {
				t.Errorf("%s: no manifest was fetched by digest, so the absence of tag lookups proves nothing", c.name)
			}
		}

		px.Reset(t)

		newCfg := pinningClientConfig(t, repo, v2.CatalogDigest, "")
		if res := cli(t, runOpts{}, "--config", newCfg, "cat", "beta").ok(t); !bytes.Equal(res.Stdout, beta2) {
			t.Errorf("the new pin printed\n%s\nwant\n%s", clip(res.Stdout), clip(beta2))
		}

		pinningNoTagLookups(t, px.Records(t))
	})

	t.Run("runtime commands refuse catalog-latest", func(t *testing.T) {
		pinningRefuseTag(t, px, repo, v2.CatalogDigest, beta2)
	})
}

// pinningNoTagLookups fails the test if a digest-pinned client asked the
// registry for a tag (a manifest by tag or a tag list) and returns how many
// manifests it fetched or probed by digest.
func pinningNoTagLookups(t *testing.T, records []regproxy.Record) int {
	t.Helper()

	byDigest := 0

	for _, r := range records {
		switch r.Class {
		case regproxy.ClassTags:
			t.Errorf("a digest-pinned client listed tags: %s %s", r.Method, r.Path)
		case regproxy.ClassManifestGet, regproxy.ClassManifestHead:
			if _, ref, _ := strings.Cut(r.Path, "/manifests/"); !digestPattern.MatchString(ref) {
				t.Errorf("a digest-pinned client looked up a tag: %s %s", r.Method, r.Path)
			} else {
				byDigest++
			}
		}
	}

	return byDigest
}

// pinningRefuseTag runs every runtime command with --catalog catalog-latest
// (exit 2, a hint to pin, no request, no side effect) and, as a control, with
// the digest (exit 0), so the refusal can only come from the tag. The same
// holds for the tag given through SCHEPHERD_CATALOG, catalog.digest and a
// mirror source. beta is the schema the digest's catalog serves as beta.
func pinningRefuseTag(t *testing.T, px *proxy, repo, dgst string, beta []byte) {
	t.Helper()

	logDir := t.TempDir()
	runner := "[runner]\nmode = \"batch\"\ncommand = \"testconsumer\"\nargs = [\"{schema}\", \"{files...}\"]\n\n" +
		"[runner.env]\nTC_LOG_DIR = " + tomlString(logDir) + "\n"
	cfg := pinningClientConfig(t, repo, "", runner)
	exportDir := t.TempDir()

	commands := []struct {
		name string
		args []string
	}{
		{"path", []string{"path", "beta"}},
		{"cat", []string{"cat", "beta"}},
		{"export", []string{"export", "beta", filepath.Join(exportDir, "beta.json")}},
		{"catalog", []string{"catalog", "--json"}},
		{"list", []string{"list", "--json"}},
		{"patterns", []string{"patterns", "--json"}},
		{"resolve", []string{"resolve", "--file", "beta.yaml", "--json"}},
		{"run", []string{"run", "--", "beta.yaml"}},
	}

	for _, c := range commands {
		t.Run(c.name, func(t *testing.T) {
			writeFile(t, filepath.Join(sandboxOf(t).Workspace, "beta.yaml"), readFile(t, instance("beta", "valid.yaml")))

			exports, consumers := fileTree(t, exportDir), fileTree(t, logDir)

			pinningRefused(t, px, runOpts{}, append([]string{"--config", cfg, "--catalog", "catalog-latest"}, c.args...), pinningTagRefusal...)

			if after := fileTree(t, exportDir); !slices.Equal(after, exports) {
				t.Errorf("the refused command wrote %v", after)
			}

			if after := fileTree(t, logDir); !slices.Equal(after, consumers) {
				t.Errorf("a consumer ran although the catalog was refused: %v", after)
			}

			cli(t, runOpts{}, append([]string{"--config", cfg, "--catalog", dgst}, c.args...)...).ok(t)
		})
	}

	t.Run("SCHEPHERD_CATALOG", func(t *testing.T) {
		pinningRefused(t, px, runOpts{Env: []string{"SCHEPHERD_CATALOG=catalog-latest"}}, []string{"--config", cfg, "path", "beta"},
			append([]string{"SCHEPHERD_CATALOG"}, pinningTagRefusal...)...)

		res := cli(t, runOpts{Env: []string{"SCHEPHERD_CATALOG=" + dgst}}, "--config", cfg, "cat", "beta").ok(t)
		if !bytes.Equal(res.Stdout, beta) {
			t.Errorf("SCHEPHERD_CATALOG=%s: cat beta printed\n%s\nwant\n%s", dgst, clip(res.Stdout), clip(beta))
		}
	})

	t.Run("catalog.digest in the configuration", func(t *testing.T) {
		tagCfg := pinningClientConfig(t, repo, "catalog-latest", "")
		want := append([]string{"catalog.digest"}, pinningTagRefusal...)

		pinningRefused(t, px, runOpts{}, []string{"--config", tagCfg, "path", "beta"}, want...)
		pinningRefused(t, px, runOpts{}, []string{"--config", tagCfg, "config", "check"}, want...)

		digestCfg := pinningClientConfig(t, repo, dgst, "")

		cli(t, runOpts{}, "--config", digestCfg, "config", "check").ok(t)

		if res := cli(t, runOpts{}, "--config", digestCfg, "cat", "beta").ok(t); !bytes.Equal(res.Stdout, beta) {
			t.Errorf("catalog.digest = %s: cat beta printed\n%s\nwant\n%s", dgst, clip(res.Stdout), clip(beta))
		}
	})

	t.Run("mirror source by tag", func(t *testing.T) {
		dest := repoPath(t, "mirror")

		pinningRefused(t, px, runOpts{}, []string{"--config", cfg, "mirror", repo + ":catalog-latest", px.Repo(dest)}, "pinned by digest")

		if tags := px.Registry().Tags(t, dest); tags != nil {
			t.Errorf("the refused mirror created tags %v", tags)
		}

		cli(t, runOpts{}, "--config", cfg, "mirror", repo+"@"+dgst, px.Repo(dest)).ok(t)

		if got := px.Registry().Manifest(t, dest, dgst).Digest; got != dgst {
			t.Errorf("mirroring %s@%s stored catalog %s", repo, dgst, got)
		}
	})
}

// pinningTagRefusal is what stderr must contain, besides the pin hint, when
// catalog-latest is refused as a catalog digest. The generic "no catalog
// digest configured" error names no value, so a product that ignored the
// tag's source altogether cannot pass for one that refused the tag.
var pinningTagRefusal = []string{`"catalog-latest"`, "looks like a tag"}

// pinningRefused runs schepherd with args and asserts a usage refusal: exit
// code 2, empty stdout, stderr pointing to "schepherd pin" and containing
// every string of want, no registry request and no file created or removed
// in the sandbox.
func pinningRefused(t *testing.T, px *proxy, o runOpts, args []string, want ...string) {
	t.Helper()

	sb := o.Sandbox
	if sb == nil {
		sb = sandboxOf(t)
	}

	before := fileTree(t, sb.Dir)

	px.Reset(t)

	res := cli(t, o, args...).wantCode(t, 2)

	if len(res.Stdout) != 0 {
		t.Errorf("stdout is not empty:\n%s", res)
	}

	if !pinningMentionsPin.Match(res.Stderr) {
		t.Errorf("stderr does not point to the pin command:\n%s", res)
	}

	for _, s := range want {
		if !bytes.Contains(res.Stderr, []byte(s)) {
			t.Errorf("stderr does not contain %q:\n%s", s, res)
		}
	}

	if records := px.Records(t); len(records) != 0 {
		t.Errorf("a refused tag still caused %d registry requests, first %s %s", len(records), records[0].Method, records[0].Path)
	}

	if after := fileTree(t, sb.Dir); !slices.Equal(after, before) {
		t.Errorf("the refused command changed the sandbox:\nbefore %v\nafter  %v", before, after)
	}
}

// TestE10_TimeIndependence builds the same set three times, as if on three
// machines: different publication dates, HOSTNAME, time zones, working
// directories and repositories, the third one inside a container with its
// own host name and registry address. The prepared output (except the
// timestamp of report.json) and the schema manifests must be identical, and
// the catalogs may differ only in their revision. Because the three builds
// run within seconds of each other, equality alone would miss a dependency
// on the wall clock's day; so every published document is also checked to
// hold no undocumented member and no date around today.
func TestE10_TimeIndependence(t *testing.T) {
	hostA := pinningHostBuild(t, "a", "20260101", []string{"HOSTNAME=e2e-builder-alpha", "TZ=UTC"})
	hostB := pinningHostBuild(t, "b", "20271231", []string{"HOSTNAME=e2e-builder-omega.example", "TZ=Pacific/Kiritimati"})
	boxC := pinningContainerBuild(t, "c", "20280229", []string{"HOSTNAME=e2e-builder-container", "TZ=America/Los_Angeles"})

	builds := []pinningBuild{hostA, hostB, boxC}
	reg := suite.source
	clock := pinningClockNeedles([]string{hostA.date, hostB.date, boxC.date})
	leaks := make([]string, 0, 5+2*len(builds)+len(clock))
	leaks = append(leaks, "e2e-builder", "127.0.0.1", "registry-source", "/work/", suite.root)
	leaks = append(leaks, clock...)

	for _, b := range builds {
		leaks = append(leaks, b.set, b.sandbox)
	}

	t.Run("prepared output", func(t *testing.T) {
		tree := fileTree(t, hostA.pub.Prepared)
		for _, name := range []string{"prepared.json", "report.json", "schemas/", "notices/"} {
			if !slices.Contains(tree, name) {
				t.Fatalf("prepared output of a has no %s: %v", name, tree)
			}
		}

		if n := len(readPrepared(t, hostA.pub.Prepared).Entries); n != 6 {
			t.Fatalf("prepared.json of a lists %d entries, want the six set-basic schemas", n)
		}

		for _, b := range builds[1:] {
			if got := fileTree(t, b.pub.Prepared); !slices.Equal(got, tree) {
				t.Errorf("%s: prepared files %v, a has %v", b.name, got, tree)
			}
		}

		for _, name := range tree {
			if strings.HasSuffix(name, "/") {
				continue
			}

			want := pinningPreparedFile(t, hostA.pub.Prepared, name)
			pinningNoLeak(t, "prepared "+name, want, leaks)

			for _, b := range builds[1:] {
				if got := pinningPreparedFile(t, b.pub.Prepared, name); !bytes.Equal(got, want) {
					t.Errorf("%s: prepared %s differs from a:\n%s\n%s", b.name, name, clip(got), clip(want))
				}
			}
		}
	})

	want, wantRaw, _ := pinningCatalog(t, reg, hostA.path, hostA.pub.CatalogDigest)
	wantArtifacts := pinningArtifacts(want)
	ids := slices.Sorted(maps.Keys(wantArtifacts))

	if len(ids) != 6 {
		t.Fatalf("catalog of a lists %v, want the six set-basic schemas", ids)
	}

	for _, b := range builds {
		t.Run(b.name, func(t *testing.T) {
			if b.pub.Status != "published" || b.pub.Revision != b.date+".0000" {
				t.Fatalf("status %q, revision %q, want published %s.0000\n%s", b.pub.Status, b.pub.Revision, b.date, b.pub.Raw)
			}

			got, raw, _ := pinningCatalog(t, reg, b.path, b.pub.CatalogDigest)
			if a := pinningArtifacts(got); !maps.Equal(a, wantArtifacts) {
				t.Errorf("schema manifests differ from a:\n%v\n%v", a, wantArtifacts)
			}

			if normalized := bytes.ReplaceAll(raw, []byte(b.pub.Revision), []byte(hostA.pub.Revision)); !bytes.Equal(normalized, wantRaw) {
				t.Errorf("catalog documents differ in more than the revision:\n%s\n%s", raw, wantRaw)
			}

			pinningDecodeStrict[pinningStrictCatalog](t, "catalog", raw)
			pinningNoLeak(t, "catalog", raw, leaks)

			entries := readPrepared(t, b.pub.Prepared).Entries

			for _, id := range ids {
				m := reg.Manifest(t, b.path, wantArtifacts[id].Digest)
				checkEnvelope(t, m, artifactTypeSchema)

				if !bytes.Equal(m.Body, reg.Manifest(t, hostA.path, wantArtifacts[id].Digest).Body) {
					t.Errorf("%s: manifest bytes differ from a", id)
				}

				i := slices.IndexFunc(entries, func(e preparedEntry) bool { return e.ID == id })
				if i < 0 {
					t.Fatalf("prepared.json of %s has no entry %s", b.name, id)
				}

				pinningCheckPure(t, reg, b.path, m, b.pub.Prepared, entries[i])
				pinningNoLeak(t, id+" manifest", m.Body, leaks)
			}

			children := reg.Catalog(t, b.path, b.pub.CatalogDigest).schemaChildren()
			if wantChildren := reg.Catalog(t, hostA.path, hostA.pub.CatalogDigest).schemaChildren(); !slices.EqualFunc(children, wantChildren,
				func(x, y ocispec.Descriptor) bool { return descriptorOf(x) == descriptorOf(y) }) {
				t.Errorf("index schema children %v, a has %v", children, wantChildren)
			}
		})
	}
}

// pinningPreparedFile reads one file of a prepared directory. report.json is
// the one file allowed to carry a timestamp, so its generatedAt member is
// required and then removed.
func pinningPreparedFile(t *testing.T, prepared, name string) []byte {
	t.Helper()

	data := readFile(t, filepath.Join(prepared, filepath.FromSlash(name)))
	if name != "report.json" {
		return data
	}

	report := decodeJSON[map[string]json.RawMessage](t, data)
	if _, ok := report["generatedAt"]; !ok {
		t.Fatalf("%s/report.json has no generatedAt:\n%s", prepared, clip(data))
	}

	delete(report, "generatedAt")

	out, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode report.json: %v", err)
	}

	return out
}

// pinningClockNeedles returns the UTC dates of yesterday, today and tomorrow
// (which covers today in every time zone) as YYYYMMDD and YYYY-MM-DD, except
// the explicit publication dates, which may legitimately appear.
func pinningClockNeedles(dates []string) []string {
	now := time.Now().UTC()

	var out []string

	for _, offset := range []int{-1, 0, 1} {
		day := now.AddDate(0, 0, offset)
		if !slices.Contains(dates, day.Format("20060102")) {
			out = append(out, day.Format("20060102"), day.Format("2006-01-02"))
		}
	}

	return out
}

// pinningStrictManifest has exactly the members docs/oci-format.md allows
// in a schema manifest, for decoding with unknown members rejected.
type pinningStrictManifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	MediaType     string `json:"mediaType"`
	ArtifactType  string `json:"artifactType"`
	Config        struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
		Data      string `json:"data"`
	} `json:"config"`
	Layers []struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Size        int64             `json:"size"`
		Annotations map[string]string `json:"annotations"`
	} `json:"layers"`
}

// pinningStrictCatalog has exactly the members of the catalog document
// (api/catalog.schema.json), for decoding with unknown members rejected.
type pinningStrictCatalog struct {
	FormatVersion int    `json:"formatVersion"`
	Revision      string `json:"revision"`
	Schemas       []struct {
		ID          string        `json:"id"`
		Name        string        `json:"name"`
		Description string        `json:"description"`
		Dialect     string        `json:"dialect"`
		FileMatch   []string      `json:"fileMatch"`
		Artifact    descriptorDoc `json:"artifact"`
		Provenance  *struct {
			Source       string `json:"source"`
			SourceDigest string `json:"sourceDigest"`
			License      string `json:"license"`
			Dependencies []struct {
				Source string `json:"source"`
				Digest string `json:"digest"`
			} `json:"dependencies"`
		} `json:"provenance"`
	} `json:"schemas"`
}

func pinningDecodeStrict[T any](t *testing.T, what string, data []byte) T {
	t.Helper()

	var v T

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&v); err != nil {
		t.Errorf("%s holds a member its format does not define: %v\n%s", what, err, clip(data))
	}

	return v
}

// pinningCheckPure asserts that a schema manifest holds nothing the
// schepherd-pack/1 recipe does not derive from the prepared schema and
// notice: no undocumented member or layer annotation, a payload that is the
// schema itself or a gzip stream of it without modification time, name or
// comment, and a notice layer with the prepared notice bytes. Then no member
// is left where the build time could hide.
func pinningCheckPure(t *testing.T, reg *registry, path string, m fetchedManifest, prepared string, entry preparedEntry) {
	t.Helper()

	id := entry.ID
	schema := readFile(t, filepath.Join(prepared, filepath.FromSlash(entry.Schema)))
	checkSchemaLayers(t, reg, path, id, m, schema, entry.Notice != "")

	man := pinningDecodeStrict[pinningStrictManifest](t, id+" manifest", m.Body)
	if len(man.Layers) == 0 {
		t.Errorf("%s: manifest has no layers:\n%s", id, m.Body)

		return
	}

	if keys := slices.Sorted(maps.Keys(man.Layers[0].Annotations)); !slices.Equal(keys, []string{annotationContentDigest, annotationContentSize, annotationTitle}) {
		t.Errorf("%s: payload annotations %v, want exactly the content digest, content size and title", id, man.Layers[0].Annotations)
	}

	blob := reg.Blob(t, path, man.Layers[0].Digest)
	if man.Layers[0].MediaType == mediaTypeSchemaGzip {
		zr, err := gzip.NewReader(bytes.NewReader(blob))
		if err != nil {
			t.Fatalf("%s: gzip payload: %v", id, err)
		}

		if !zr.ModTime.IsZero() || zr.Name != "" || zr.Comment != "" || zr.Extra != nil {
			t.Errorf("%s: gzip header carries mtime %v, name %q, comment %q, extra %q", id, zr.ModTime, zr.Name, zr.Comment, zr.Extra)
		}

		content, err := io.ReadAll(zr)
		if err != nil || !bytes.Equal(content, schema) {
			t.Errorf("%s: gzip payload does not decompress to the prepared schema (%v)", id, err)
		}
	}

	if entry.Notice == "" || len(man.Layers) != 2 {
		return
	}

	for k := range man.Layers[1].Annotations {
		if k != annotationTitle {
			t.Errorf("%s: notice layer annotation %s is not documented", id, k)
		}
	}

	if got := reg.Blob(t, path, man.Layers[1].Digest); !bytes.Equal(got, readFile(t, filepath.Join(prepared, filepath.FromSlash(entry.Notice)))) {
		t.Errorf("%s: notice layer differs from the prepared notice %s", id, entry.Notice)
	}
}

// pinningBuild is one independent prepare+publish run of set-basic.
type pinningBuild struct {
	name    string
	date    string
	path    string
	set     string
	sandbox string
	pub     publishResult
}

// pinningHostBuild prepares and publishes a fresh copy of set-basic on the
// host in its own sandbox (HOME, TMPDIR, working directory) with env on top.
func pinningHostBuild(t *testing.T, name, date string, env []string) pinningBuild {
	t.Helper()

	sb := newSandbox(t)
	o := runOpts{Sandbox: sb, Env: env}
	b := pinningBuild{name: name, date: date, path: repoPath(t, name), set: newSet(t, "set-basic"), sandbox: sb.Dir}
	prepared := filepath.Join(sb.Tmp, "prepared")

	publisher(t, o, "prepare", "--source", filepath.Join(b.set, "source.toml"), "--out", prepared,
		"--jsonschema", suite.jsonschema, "--json").ok(t)

	res := publisher(t, o, "publish", "--prepared", prepared, "--repository", suite.source.Repo(b.path),
		"--registry-config", publisherRegistryConfig(t), "--now", date+".0000", "--json").ok(t)

	b.pub = decodeJSON[publishResult](t, res.Stdout)
	b.pub.Prepared, b.pub.Raw = prepared, res.Stdout

	return b
}

// pinningContainerBuild prepares set-basic inside the validators image
// without network (a real different host name, the image's own Sourcemeta
// binary) and publishes it from another container through the compose
// network, where the registry is registry-source:5000.
func pinningContainerBuild(t *testing.T, name, date string, env []string) pinningBuild {
	t.Helper()

	work := t.TempDir()
	b := pinningBuild{name: name, date: date, path: repoPath(t, name), set: newSet(t, "set-basic"), sandbox: work}
	image := validatorsImage(t)
	mounts := []mount{binMount(), {Host: b.set, Container: "/set"}, {Host: work, Container: "/work", Writable: true}}
	publisherBin := "/e2e/bin/schepherd-publisher"

	writeFile(t, filepath.Join(work, "registries.toml"), []byte("[registries.\"registry-source:5000\"]\nplain_http = true\n"))

	dockerRun(t, image, dockerOpts{Mounts: mounts, Env: env, Workdir: "/work"}, publisherBin, "prepare",
		"--source", "/set/source.toml", "--out", "/work/prepared", "--jsonschema", "/usr/local/bin/jsonschema", "--json").ok(t)

	res := dockerRun(t, image, dockerOpts{Mounts: mounts, Env: env, Workdir: "/work", Network: composeNetwork()}, publisherBin, "publish",
		"--prepared", "/work/prepared", "--repository", "registry-source:5000/"+b.path,
		"--registry-config", "/work/registries.toml", "--now", date+".0000", "--json").ok(t)

	b.pub = decodeJSON[publishResult](t, res.Stdout)
	b.pub.Prepared, b.pub.Raw = filepath.Join(work, "prepared"), res.Stdout

	return b
}

func pinningNoLeak(t *testing.T, what string, data []byte, needles []string) {
	t.Helper()

	for _, s := range needles {
		if s != "" && bytes.Contains(data, []byte(s)) {
			t.Errorf("%s contains %q:\n%s", what, s, clip(data))
		}
	}
}

// pinningCatalog fetches a catalog directly from the registry and returns
// the decoded document, its exact bytes and the metadata manifest.
func pinningCatalog(t *testing.T, reg *registry, path, dgst string) (catalogDoc, []byte, fetchedManifest) {
	t.Helper()

	c := reg.Catalog(t, path, dgst)
	checkEnvelope(t, c.Metadata, artifactTypeMetadata)
	checkIndex(t, c)

	return c.Doc, c.Blob, c.Metadata
}

func pinningArtifacts(c catalogDoc) map[string]descriptorDoc {
	out := make(map[string]descriptorDoc, len(c.Schemas))
	for _, e := range c.Schemas {
		out[e.ID] = e.Artifact
	}

	return out
}

// pinningManifestWrites maps every manifest PUT (by digest or by tag) to the
// manifest digest it stores and returns the distinct digests, sorted.
// Retention tags name their digest; other tags need an entry in tagDigests.
func pinningManifestWrites(t *testing.T, records []regproxy.Record, tagDigests map[string]string) []string {
	t.Helper()

	var out []string

	for _, r := range records {
		if r.Class != regproxy.ClassManifestPut {
			continue
		}

		_, ref, _ := strings.Cut(r.Path, "/manifests/")

		var dgst string

		switch {
		case digestPattern.MatchString(ref):
			dgst = ref
		case tagDigests[ref] != "":
			dgst = tagDigests[ref]
		default:
			t.Errorf("unexpected manifest write %s %s", r.Method, r.Path)

			continue
		}

		out = append(out, dgst)
	}

	slices.Sort(out)

	return slices.Compact(out)
}

// pinningBlobUploads returns the digest of every completed blob upload (the
// digest query parameter of the request that finishes it), sorted and not
// deduplicated. A cross-repository mount, a failed upload request or an
// upload session that was opened but never finished fails the test, because
// each of them writes or reserves a blob that the digest list would not show.
func pinningBlobUploads(t *testing.T, records []regproxy.Record) []string {
	t.Helper()

	var (
		out    []string
		opened int
	)

	for _, r := range records {
		if r.Class != regproxy.ClassBlobUpload {
			continue
		}

		q, err := url.ParseQuery(r.Query)
		if err != nil {
			t.Fatalf("upload query %q: %v", r.Query, err)
		}

		switch {
		case q.Has("mount") || q.Has("from"):
			t.Errorf("cross-repository blob mount %s %s?%s", r.Method, r.Path, r.Query)
		case r.Status < 200 || r.Status > 299:
			t.Errorf("blob upload request %s %s?%s answered %d", r.Method, r.Path, r.Query, r.Status)
		case q.Get("digest") != "":
			out = append(out, q.Get("digest"))

			if r.Method == http.MethodPut {
				opened--
			}
		case r.Method == http.MethodPost:
			opened++
		}
	}

	if opened != 0 {
		t.Errorf("%d blob upload sessions were opened but not finished (or finished without being opened)", opened)
	}

	slices.Sort(out)

	return out
}

// pinningCheckTagTargets asserts that every tag of path is catalog-<revision>
// and resolves to revisions[revision]. Any other tag fails the test.
func pinningCheckTagTargets(t *testing.T, reg *registry, path string, revisions map[string]string) {
	t.Helper()

	for _, tag := range reg.Tags(t, path) {
		var want string

		rev, isCatalog := strings.CutPrefix(tag, "catalog-")

		switch {
		case isCatalog && revisions[rev] != "":
			want = revisions[rev]
		default:
			t.Errorf("unexpected tag %s", tag)

			continue
		}

		if got := reg.Manifest(t, path, tag).Digest; got != want {
			t.Errorf("tag %s resolves to %s, want %s", tag, got, want)
		}
	}
}

// pinningClientConfig writes a client configuration into a fresh directory;
// empty dgst leaves catalog.digest out.
func pinningClientConfig(t *testing.T, repo, dgst, extra string) string {
	t.Helper()

	return writeConfig(t, t.TempDir(), clientConfig{Repository: repo, Catalog: dgst, Extra: extra}.TOML())
}

func pinningPath(t *testing.T, cfg, id string) string {
	t.Helper()

	res := cli(t, runOpts{}, "--config", cfg, "path", id).ok(t)

	path, ok := strings.CutSuffix(string(res.Stdout), "\n")
	if !ok || !filepath.IsAbs(path) || strings.Contains(path, "\n") {
		t.Fatalf("path %s printed %q", id, res.Stdout)
	}

	return path
}

// pinningPinLatest runs "pin <repo>:catalog-latest --json" and checks the
// resolved digest and revision.
func pinningPinLatest(t *testing.T, cfg, repo, wantDigest, wantRevision string, sb ...*sandbox) {
	t.Helper()

	o := runOpts{}
	if len(sb) > 0 {
		o.Sandbox = sb[0]
	}

	res := cli(t, o, "--config", cfg, "pin", repo+":catalog-latest", "--json").ok(t)

	var got struct {
		Repository string `json:"repository"`
		Tag        string `json:"tag"`
		Digest     string `json:"digest"`
		Revision   string `json:"revision"`
	}

	if err := json.Unmarshal(res.Stdout, &got); err != nil {
		t.Fatalf("pin --json output: %v\n%s", err, res)
	}

	if got.Repository != repo || got.Tag != "catalog-latest" || got.Digest != wantDigest || got.Revision != wantRevision {
		t.Fatalf("pin resolved %+v, want repository %s, tag catalog-latest, digest %s, revision %s",
			got, repo, wantDigest, wantRevision)
	}
}
