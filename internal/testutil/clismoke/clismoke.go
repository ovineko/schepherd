// Package clismoke is a black-box smoke scenario for the schepherd client,
// for tests only. It publishes a small schema set to an in-memory registry
// with the project's own packing and push code, then drives a client through
// every command that reads it: first against the registry, then from the warm
// cache after the registry is gone. Local schemas of the workspace, named by
// paths relative to the configuration, are used next to the catalog and,
// with no catalog configured at all, without registry and cache. The same
// scenario runs against the compiled binary and in process. It uses no shell, no environment-specific
// paths and no platform-specific behaviour, so it runs unchanged on every
// platform the client supports.
package clismoke

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/calver"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/internal/registry"
	"github.com/ovineko/schepherd/internal/testutil/ociregistry"
)

const (
	consumerPkg = "github.com/ovineko/schepherd/internal/testutil/smokeconsumer"
	repoPath    = "smoke/schemas"
	mirrorPath  = "smoke/mirror"
	revision    = "20260924.1200"
	catalogTag  = "catalog-latest"
	// failStatus is deliberately none of schepherd's own exit codes, so the
	// test can tell the consumer's status apart from schepherd's.
	failStatus = 17
	failMarker = "INVALID"
	localID    = "local-config"
	// localRel is the local schema relative to the workspace, with "/" as
	// {schema-ref} spells it.
	localRel = "schemas/local.schema.json"
)

var (
	alphaNotice = []byte("Alpha notice\n")
	localSchema = []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$ref":"defs.schema.json"}`)
	localDefs   = []byte(`{"type":"object","required":["kind"]}`)
)

// Result is the outcome of one client invocation.
type Result struct {
	Stdout string
	Stderr string
	Code   int
}

// Client runs schepherd with args and dir as the working directory. It must
// use the current environment of the test process, which the scenario sets
// with t.Setenv.
type Client func(t *testing.T, dir string, args ...string) Result

// Build compiles the main package pkg into a temporary directory and returns
// the absolute path of the executable, with the platform's suffix.
func Build(t *testing.T, name, pkg string) string {
	t.Helper()

	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go toolchain not found: %v", err)
	}

	if runtime.GOOS == "windows" {
		name += ".exe"
	}

	bin := filepath.Join(t.TempDir(), name)

	// bearer:disable go_gosec_injection_subproc_injection
	//nolint:gosec // builds a package of this module with the go toolchain from PATH
	out, err := exec.CommandContext(t.Context(), goBin, "build", "-buildvcs=false", "-o", bin, pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, out)
	}

	return bin
}

type published struct {
	schemas       map[string][]byte
	manifests     map[string]string
	catalogDigest string
	catalogRaw    []byte
}

type scenario struct {
	t       *testing.T
	client  Client
	reg     *ociregistry.Registry
	set     *published
	root    string
	ws      string
	config  string
	records string
	out     string
	repo    string
	// local extends config with a [schemas] table; localOnly declares the
	// same local schema and the runner but no catalog.
	local     string
	localOnly string
}

// Run executes the scenario with client.
func Run(t *testing.T, client Client) {
	t.Helper()

	consumer := Build(t, "smokeconsumer", consumerPkg)

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	s := &scenario{t: t, client: client, root: root, reg: ociregistry.New(t)}
	s.repo = s.reg.Host() + "/" + repoPath
	s.set = publish(t, s.reg.Host())
	s.layout(consumer)

	s.discovery()
	s.materialize()
	s.export()
	s.runConsumer()
	s.localSchemas()
	s.mirror()
	s.warmCache()
	s.localWithoutRegistry()
}

func publish(t *testing.T, host string) *published {
	t.Helper()

	set := &published{
		schemas: map[string][]byte{
			"alpha": []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["name"]}`),
			"gamma": largeSchema(),
		},
		manifests: map[string]string{},
	}

	alpha, err := artifact.PackSchema(set.schemas["alpha"], alphaNotice)
	if err != nil {
		t.Fatal(err)
	}

	gamma, err := artifact.PackSchema(set.schemas["gamma"], nil)
	if err != nil {
		t.Fatal(err)
	}

	if !slices.ContainsFunc(gamma.Blobs, func(b artifact.Blob) bool { return b.Descriptor.MediaType == artifact.SchemaGzipMediaType }) {
		t.Fatal("the large schema must be packed with a gzip payload so decompression is covered")
	}

	entries := make([]catalog.Entry, 0, 2)

	for id, packed := range map[string]*artifact.Packed{"alpha": alpha, "gamma": gamma} {
		set.manifests[id] = packed.Manifest.Descriptor.Digest.String()
		entry := catalog.Entry{
			ID:       id,
			Name:     strings.ToUpper(id[:1]) + id[1:],
			Artifact: catalog.Descriptor{MediaType: packed.Manifest.Descriptor.MediaType, Digest: set.manifests[id], Size: packed.Manifest.Descriptor.Size},
		}

		if id == "alpha" {
			entry.Description = "Smoke test schema"
			entry.FileMatch = []string{"config/alpha.json"}
		}

		entries = append(entries, entry)
	}

	raw, err := catalog.Marshal(&catalog.Catalog{FormatVersion: catalog.FormatVersion, Revision: revision, Schemas: entries})
	if err != nil {
		t.Fatal(err)
	}

	cat, err := artifact.PackCatalog(raw, []ocispec.Descriptor{alpha.Manifest.Descriptor, gamma.Manifest.Descriptor})
	if err != nil {
		t.Fatal(err)
	}

	set.catalogRaw = raw
	set.catalogDigest = cat.Index.Descriptor.Digest.String()

	name, err := registry.ParseRepository(host + "/" + repoPath)
	if err != nil {
		t.Fatal(err)
	}

	repo, err := registry.NewClient(registry.Options{Hosts: map[string]registry.HostConfig{host: {PlainHTTP: true}}}).Open(name)
	if err != nil {
		t.Fatal(err)
	}

	for _, packed := range []*artifact.Packed{alpha, gamma, cat.Metadata} {
		for _, b := range packed.Blobs {
			if _, err := repo.PushBlob(t.Context(), b.Descriptor, b.Data); err != nil {
				t.Fatal(err)
			}
		}

		if _, err := repo.PushManifest(t.Context(), packed.Manifest.Descriptor, packed.Manifest.Data); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := repo.PushManifest(t.Context(), cat.Index.Descriptor, cat.Index.Data); err != nil {
		t.Fatal(err)
	}

	if err := repo.Tag(t.Context(), cat.Index.Descriptor, catalogTag); err != nil {
		t.Fatal(err)
	}

	return set
}

// largeSchema is compact, compressible JSON above artifact.GzipThreshold.
func largeSchema() []byte {
	var b strings.Builder

	b.WriteString(`{"type":"object","properties":{`)

	for i := range 200 {
		if i > 0 {
			b.WriteByte(',')
		}

		b.WriteString(`"p` + strconv.Itoa(i) + `":{"type":"string","description":"repeated text that compresses well"}`)
	}

	b.WriteString(`}}`)

	return []byte(b.String())
}

// layout writes the workspace and the configurations. Every absolute path
// reaches a configuration through ${NAME} interpolation, so no platform path
// is ever written into TOML; the local schemas are named by relative paths
// with "/" separators, which mean the same on every platform.
func (s *scenario) layout(consumer string) {
	s.t.Helper()

	s.ws = filepath.Join(s.root, "workspace")
	s.records = filepath.Join(s.root, "records")
	s.out = filepath.Join(s.root, "out")
	s.config = filepath.Join(s.root, "conf", "schepherd.toml")
	s.local = filepath.Join(s.root, "conf", "local.schepherd.toml")
	s.localOnly = filepath.Join(s.root, "conf", "local-only.schepherd.toml")

	runner := fmt.Sprintf(`[runner]
mode = "batch"
command = "${SMOKE_CONSUMER}"
args = ["-record-dir", "${SMOKE_RECORDS}", "-schema-id", "{schema-id}", "-schema-ref", "{schema-ref}", "-fail-if-contains", %q, "-fail-status", "%d", "{schema}", "{files...}"]
timeout = "2m"
`, failMarker, failStatus)

	schemas := `[schemas.` + localID + `]
path = "../workspace/` + localRel + `"
file_match = ["local/*.json"]
`

	s.write(filepath.Join(s.ws, "config", "alpha.json"), `{"name":"ok"}`)
	s.write(filepath.Join(s.ws, "bad.json"), `{"name":"`+failMarker+`"}`)
	s.write(filepath.Join(s.ws, "notes", "unmatched.json"), `{}`)
	s.write(filepath.Join(s.ws, "local", "doc.json"), `{"kind":"ok"}`)
	s.write(filepath.Join(s.ws, filepath.FromSlash(localRel)), string(localSchema))
	s.write(filepath.Join(s.ws, "schemas", "defs.schema.json"), string(localDefs))
	s.write(s.config, fmt.Sprintf(`config_version = 1

[catalog]
repository = "${SMOKE_REPOSITORY}"
digest = "${SMOKE_CATALOG}"

[registries.%s]
plain_http = true

`, strconv.Quote(s.reg.Host()))+runner)
	s.write(s.local, "config_version = 1\nextends = [\"schepherd.toml\"]\n\n"+schemas)
	s.write(s.localOnly, "config_version = 1\n\n"+schemas+"\n"+runner)

	for _, dir := range []string{s.records, s.out} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			s.t.Fatal(err)
		}
	}

	for _, key := range []string{env.KeyConfig, env.KeyRepository, env.KeyCatalog, env.KeyOffline, env.KeyWorkspace, env.KeyTimeout} {
		s.t.Setenv(key, "")
	}

	s.t.Setenv(env.KeyCacheDir, filepath.Join(s.root, "cache"))
	s.t.Setenv("SMOKE_REPOSITORY", s.repo)
	s.t.Setenv("SMOKE_CATALOG", s.set.catalogDigest)
	s.t.Setenv("SMOKE_CONSUMER", consumer)
	s.t.Setenv("SMOKE_RECORDS", s.records)
}

func (s *scenario) discovery() {
	var version struct {
		Version               string `json:"version"`
		CatalogFormatVersions []int  `json:"catalogFormatVersions"`
	}

	s.decode(s.ok("version", "--json"), &version)

	if version.Version == "" || !slices.Contains(version.CatalogFormatVersions, catalog.FormatVersion) {
		s.t.Errorf("version --json = %+v", version)
	}

	if res := s.ok("--config", s.config, "config", "check"); res.Stdout != "ok (1 file(s))\n" {
		s.t.Errorf("config check stdout %q", res.Stdout)
	}

	s.checkPin(s.repo, catalogTag)

	if res := s.ok("--config", s.config, "catalog", "--json"); res.Stdout != string(s.set.catalogRaw) {
		s.t.Errorf("catalog --json is not the published catalog byte for byte:\n%s", res.Stdout)
	}

	summary := s.ok("--config", s.config, "catalog").Stdout
	for _, line := range []string{"repository\t" + s.repo, "catalog\t" + s.set.catalogDigest, "revision\t" + revision, "schemas\t2"} {
		if !strings.Contains(summary, line+"\n") {
			s.t.Errorf("catalog summary lacks %q:\n%s", line, summary)
		}
	}

	var list []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Digest      string `json:"digest"`
	}

	s.decode(s.ok("--config", s.config, "list", "--json"), &list)

	if len(list) != 2 || list[0].ID != "alpha" || list[0].Name != "Alpha" || list[0].Description != "Smoke test schema" ||
		list[0].Digest != s.set.manifests["alpha"] || list[1].ID != "gamma" || list[1].Digest != s.set.manifests["gamma"] {
		s.t.Errorf("list --json = %+v", list)
	}

	s.expectStdout("alpha\tAlpha\ngamma\tGamma\n", "--config", s.config, "list")
	s.expectStdout("alpha\tconfig/alpha.json\n", "--config", s.config, "patterns")
	s.expectStdout("alpha\n", "--config", s.config, "resolve", "--file", filepath.Join("config", "alpha.json"))

	var resolved struct {
		File     string `json:"file"`
		Path     string `json:"path"`
		Schema   string `json:"schema"`
		Origin   string `json:"origin"`
		Artifact struct {
			Digest string `json:"digest"`
		} `json:"artifact"`
	}

	s.decode(s.ok("--config", s.config, "resolve", "--file", filepath.Join("config", "alpha.json"), "--json"), &resolved)

	if resolved.Path != "config/alpha.json" || resolved.Schema != "alpha" || resolved.Origin != "catalog" || resolved.Artifact.Digest != s.set.manifests["alpha"] {
		s.t.Errorf("resolve --json = %+v", resolved)
	}

	s.sameFile(resolved.File, filepath.Join(s.ws, "config", "alpha.json"))
	s.fails(3, "--config", s.config, "resolve", "--file", filepath.Join("notes", "unmatched.json"))
}

func (s *scenario) checkPin(repo, tag string) {
	s.t.Helper()

	var pinned struct {
		Repository string `json:"repository"`
		Tag        string `json:"tag"`
		Digest     string `json:"digest"`
		Revision   string `json:"revision"`
		Schemas    int    `json:"schemas"`
	}

	s.decode(s.ok("--config", s.config, "pin", repo+":"+tag, "--json"), &pinned)

	if pinned.Repository != repo || pinned.Tag != tag || pinned.Digest != s.set.catalogDigest || pinned.Revision != revision || pinned.Schemas != 2 {
		s.t.Errorf("pin %s:%s --json = %+v, want digest %s", repo, tag, pinned, s.set.catalogDigest)
	}
}

func (s *scenario) materialize() {
	res := s.ok("--config", s.config, "path", "alpha")

	path, terminated := strings.CutSuffix(res.Stdout, "\n")
	if !terminated || strings.ContainsAny(path, "\n\x00") {
		s.t.Fatalf("path stdout %q is not one newline-terminated path", res.Stdout)
	}

	s.checkSchemaFile(path, "alpha")

	res = s.ok("--config", s.config, "path", "--null", "gamma")

	path, terminated = strings.CutSuffix(res.Stdout, "\x00")
	if !terminated || strings.ContainsAny(path, "\n\x00") {
		s.t.Fatalf("path --null stdout %q is not one NUL-terminated path", res.Stdout)
	}

	s.checkSchemaFile(path, "gamma")

	s.expectStdout(string(s.set.schemas["gamma"]), "--config", s.config, "cat", "gamma")
	s.expectStdout(string(s.set.schemas["alpha"]), "--config", s.config, "cat", "alpha")
	s.fails(3, "--config", s.config, "path", "nope")
	s.fails(3, "--config", s.config, "cat", "nope")
}

func (s *scenario) checkSchemaFile(path, id string) {
	s.t.Helper()

	if !filepath.IsAbs(path) || filepath.Base(path) != "schema.json" {
		s.t.Errorf("path %s: %q is not an absolute path to a schema.json", id, path)
	}

	s.fileEquals(path, s.set.schemas[id])
}

// export writes alpha with its notice and gamma, which has none, over it:
// --force also removes alpha's notice, which does not belong to gamma.
func (s *scenario) export() {
	dest := filepath.Join(s.out, "export.json")
	notice := dest + ".NOTICE"

	s.expectStdout("", "--config", s.config, "export", "alpha", dest)
	s.fileEquals(dest, s.set.schemas["alpha"])
	s.fileEquals(notice, alphaNotice)

	s.fails(2, "--config", s.config, "export", "gamma", dest)
	s.fileEquals(dest, s.set.schemas["alpha"])
	s.fileEquals(notice, alphaNotice)

	s.expectStdout("", "--config", s.config, "export", "--force", "gamma", dest)
	s.fileEquals(dest, s.set.schemas["gamma"])

	if _, err := os.Lstat(notice); err == nil {
		s.t.Errorf("export --force of gamma, which has no notice, kept alpha's notice %s", notice)
	}

	var res struct {
		ID     string `json:"id"`
		Origin string `json:"origin"`
		Path   string `json:"path"`
		Notice string `json:"notice"`
	}

	s.decode(s.ok("--config", s.config, "export", "--force", "--json", "alpha", dest), &res)

	if res.ID != "alpha" || res.Origin != "catalog" || res.Path != dest || res.Notice != notice {
		s.t.Errorf("export --json = %+v, want alpha at %s with the notice %s", res, dest, notice)
	}

	s.fileEquals(dest, s.set.schemas["alpha"])
	s.fileEquals(notice, alphaNotice)

	s.ok("--config", s.config, "export", "alpha", "relative-export.json")
	s.fileEquals(filepath.Join(s.ws, "relative-export.json"), s.set.schemas["alpha"])
	s.fileEquals(filepath.Join(s.ws, "relative-export.json.NOTICE"), alphaNotice)
}

type consumerFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type consumerRecord struct {
	SchemaID  string         `json:"schemaId"`
	SchemaRef string         `json:"schemaRef"`
	Cwd       string         `json:"cwd"`
	Schema    consumerFile   `json:"schema"`
	Files     []consumerFile `json:"files"`
}

type runReport struct {
	Tasks []struct {
		SchemaID  string   `json:"schemaId"`
		SchemaRef string   `json:"schemaRef"`
		Origin    string   `json:"origin"`
		Status    string   `json:"status"`
		Files     []string `json:"files"`
		ExitCode  int      `json:"exitCode"`
	} `json:"tasks"`
	Skipped []struct {
		File   string `json:"file"`
		Reason string `json:"reason"`
	} `json:"skipped"`
	ExitCode int `json:"exitCode"`
}

func (s *scenario) runConsumer() {
	alphaInput := filepath.Join("config", "alpha.json")
	unmatched := filepath.Join("notes", "unmatched.json")
	reportFile := filepath.Join(s.out, "report.json")

	s.resetRecords()
	s.expectStdout("smokeconsumer: alpha 1 file(s)\n", "--config", s.config, "run", "--report", reportFile, "--", alphaInput)
	s.checkRecord("alpha", filepath.Join(s.ws, alphaInput))

	var report runReport

	s.readJSON(reportFile, &report)

	if report.ExitCode != 0 || len(report.Tasks) != 1 || report.Tasks[0].SchemaID != "alpha" || report.Tasks[0].Status != "ok" ||
		report.Tasks[0].SchemaRef != s.repo+"@"+s.set.manifests["alpha"] || len(report.Tasks[0].Files) != 1 {
		s.t.Errorf("run report = %+v", report)
	} else {
		s.sameFile(report.Tasks[0].Files[0], filepath.Join(s.ws, alphaInput))
	}

	s.resetRecords()

	res := s.exec("--config", s.config, "run", "--schema", "gamma", "--", "bad.json")
	if res.Code != failStatus || !strings.Contains(res.Stderr, "consumer exited with status "+strconv.Itoa(failStatus)) {
		s.t.Errorf("failing consumer: exit code %d, want %d; stderr %q", res.Code, failStatus, res.Stderr)
	}

	s.checkRecord("gamma", filepath.Join(s.ws, "bad.json"))

	s.resetRecords()
	s.fails(3, "--config", s.config, "run", "--", unmatched, alphaInput)

	if entries, _ := os.ReadDir(s.records); len(entries) != 0 {
		s.t.Errorf("a consumer started although a file matched no schema: %v", entries)
	}

	s.resetRecords()
	s.ok("--config", s.config, "run", "--ignore-unmatched", "--report", reportFile, "--", unmatched, alphaInput)
	s.checkRecord("alpha", filepath.Join(s.ws, alphaInput))

	report = runReport{}
	s.readJSON(reportFile, &report)

	if report.ExitCode != 0 || len(report.Skipped) != 1 || report.Skipped[0].Reason != "unmatched" {
		s.t.Errorf("run --ignore-unmatched report = %+v", report)
	} else {
		s.sameFile(report.Skipped[0].File, filepath.Join(s.ws, unmatched))
	}
}

func (s *scenario) checkRecord(id, input string) {
	s.t.Helper()

	var rec consumerRecord

	s.readJSON(filepath.Join(s.records, id+".json"), &rec)

	if rec.SchemaID != id || rec.Schema.SHA256 != sha256Hex(s.set.schemas[id]) || filepath.Base(rec.Schema.Path) != "schema.json" {
		s.t.Errorf("consumer for %s got schema %+v", id, rec.Schema)
	}

	if want := s.repo + "@" + s.set.manifests[id]; rec.SchemaRef != want {
		s.t.Errorf("consumer for %s got {schema-ref} %q, want %q", id, rec.SchemaRef, want)
	}

	if len(rec.Files) != 1 {
		s.t.Fatalf("consumer for %s got files %+v, want only %s", id, rec.Files, input)
	}

	data, err := os.ReadFile(input) //nolint:gosec // a file of the test workspace
	if err != nil {
		s.t.Fatal(err)
	}

	if rec.Files[0].SHA256 != sha256Hex(data) {
		s.t.Errorf("consumer for %s read different bytes from %s", id, rec.Files[0].Path)
	}

	s.sameFile(rec.Files[0].Path, input)
	s.sameFile(rec.Cwd, s.ws)
}

// localSchemas uses a schema file of the workspace next to the catalog:
// path prints the file itself rather than a copy in the cache, cat prints
// its bytes, and run hands the consumer that file, whose relative $ref to a
// sibling therefore keeps resolving, in the same run as a catalog schema.
func (s *scenario) localSchemas() {
	localFile := filepath.Join(s.ws, filepath.FromSlash(localRel))
	doc := filepath.Join("local", "doc.json")

	res := s.ok("--config", s.local, "path", localID)
	if res.Stdout != localFile+"\n" {
		s.t.Errorf("path %s = %q, want the workspace file %s itself", localID, res.Stdout, localFile)
	}

	s.expectStdout(string(localSchema), "--config", s.local, "cat", localID)
	s.expectStdout(localID+"\n", "--config", s.local, "resolve", "--file", doc)

	reportFile := filepath.Join(s.out, "local-report.json")

	s.resetRecords()
	s.ok("--config", s.local, "run", "--report", reportFile, "--", doc, filepath.Join("config", "alpha.json"))
	s.checkLocalRecord(filepath.Join(s.ws, doc))
	s.checkRecord("alpha", filepath.Join(s.ws, "config", "alpha.json"))

	var report runReport

	s.readJSON(reportFile, &report)

	if len(report.Tasks) != 2 || report.Tasks[0].SchemaID != localID || report.Tasks[0].Origin != "local" || report.Tasks[0].SchemaRef != "local:"+localRel ||
		report.Tasks[1].SchemaID != "alpha" || report.Tasks[1].Origin != "catalog" || report.Tasks[1].Status != "ok" {
		s.t.Errorf("run report with a local and a catalog schema = %+v", report)
	}
}

func (s *scenario) checkLocalRecord(input string) {
	s.t.Helper()

	var rec consumerRecord

	s.readJSON(filepath.Join(s.records, localID+".json"), &rec)

	if rec.SchemaID != localID || rec.SchemaRef != "local:"+localRel || rec.Schema.SHA256 != sha256Hex(localSchema) {
		s.t.Errorf("consumer for %s got %+v", localID, rec)
	}

	s.sameFile(rec.Schema.Path, filepath.Join(s.ws, filepath.FromSlash(localRel)))
	s.fileEquals(filepath.Join(filepath.Dir(rec.Schema.Path), "defs.schema.json"), localDefs)

	if len(rec.Files) != 1 {
		s.t.Fatalf("consumer for %s got files %+v, want only %s", localID, rec.Files, input)
	}

	s.sameFile(rec.Files[0].Path, input)
}

func (s *scenario) mirror() {
	dest := s.reg.Host() + "/" + mirrorPath

	var result struct {
		Destination   string `json:"destination"`
		CatalogDigest string `json:"catalogDigest"`
		Schemas       int    `json:"schemas"`
	}

	s.decode(s.ok("--config", s.config, "mirror", s.repo+"@"+s.set.catalogDigest, dest, "--json"), &result)

	if result.Destination != dest || result.CatalogDigest != s.set.catalogDigest || result.Schemas != 2 {
		s.t.Errorf("mirror --json = %+v", result)
	}

	v, err := calver.ParseRevision(revision)
	if err != nil {
		s.t.Fatal(err)
	}

	s.checkPin(dest, v.Tag())

	fresh := []string{"--config", s.config, "--cache-dir", filepath.Join(s.root, "mirror-cache"), "--repository", dest}
	s.expectStdout(string(s.set.catalogRaw), append(fresh, "catalog", "--json")...)
	s.expectStdout(string(s.set.schemas["gamma"]), append(fresh, "cat", "gamma")...)
}

// warmCache checks that a populated cache serves every command without the
// registry: first with the registry still up (it must see no request), then
// with it shut down, online and offline.
func (s *scenario) warmCache() {
	before := s.reg.Requests()

	s.ok("--config", s.config, "path", "alpha")
	s.ok("--config", s.config, "cat", "gamma")
	s.ok("--config", s.config, "list")

	if after := s.reg.Requests(); after != before {
		s.t.Errorf("commands served from a warm cache sent %d registry request(s)", after-before)
	}

	s.reg.Close()

	offline := []string{"--config", s.config, "--offline"}

	s.checkSchemaFile(strings.TrimSuffix(s.ok(append(offline, "path", "alpha")...).Stdout, "\n"), "alpha")
	s.expectStdout(string(s.set.schemas["gamma"]), append(offline, "cat", "gamma")...)
	s.expectStdout("alpha\tAlpha\ngamma\tGamma\n", append(offline, "list")...)

	dest := filepath.Join(s.out, "offline-export.json")
	s.ok(append(offline, "export", "gamma", dest)...)
	s.fileEquals(dest, s.set.schemas["gamma"])

	s.ok(append(offline, "export", "--force", "alpha", dest)...)
	s.fileEquals(dest, s.set.schemas["alpha"])
	s.fileEquals(dest+".NOTICE", alphaNotice)

	s.resetRecords()
	s.ok(append(offline, "run", "--", filepath.Join("config", "alpha.json"))...)
	s.checkRecord("alpha", filepath.Join(s.ws, "config", "alpha.json"))

	s.expectStdout(string(s.set.schemas["alpha"]), "--config", s.config, "cat", "alpha")

	s.t.Setenv(env.KeyOffline, "true")

	res := s.fails(6, "--config", s.config, "--cache-dir", filepath.Join(s.root, "empty-cache"), "path", "alpha")
	if !strings.Contains(res.Stderr, s.set.catalogDigest) {
		s.t.Errorf("offline cache miss does not name the catalog digest: %q", res.Stderr)
	}
}

// localWithoutRegistry runs after the registry is gone: the local schema
// serves path, cat and run offline next to a warm catalog, and with no
// catalog configured at all it needs neither the registry nor a cache, which
// is never created.
func (s *scenario) localWithoutRegistry() {
	localFile := filepath.Join(s.ws, filepath.FromSlash(localRel))
	doc := filepath.Join("local", "doc.json")

	s.expectStdout(localFile+"\n", "--config", s.local, "--offline", "path", localID)

	s.resetRecords()
	s.ok("--config", s.local, "--offline", "run", "--", doc, filepath.Join("config", "alpha.json"))
	s.checkLocalRecord(filepath.Join(s.ws, doc))

	cache := filepath.Join(s.root, "never-created-cache")

	for _, offline := range []string{"--offline=false", "--offline=true"} {
		only := []string{"--config", s.localOnly, "--cache-dir", cache, offline}

		s.expectStdout(localFile+"\n", append(only, "path", localID)...)
		s.expectStdout(string(localSchema), append(only, "cat", localID)...)

		s.resetRecords()
		s.expectStdout("smokeconsumer: "+localID+" 1 file(s)\n", append(only, "run", "--", doc)...)
		s.checkLocalRecord(filepath.Join(s.ws, doc))

		s.resetRecords()
		s.ok(append(only, "run", "--schema", localID, "--", filepath.Join("config", "alpha.json"))...)
		s.checkLocalRecord(filepath.Join(s.ws, "config", "alpha.json"))

		s.fails(2, append(only, "path", "alpha")...)
	}

	if _, err := os.Lstat(cache); !os.IsNotExist(err) {
		s.t.Errorf("commands on local schemas created the cache directory %s (%v)", cache, err)
	}
}

func (s *scenario) exec(args ...string) Result {
	s.t.Helper()

	return s.client(s.t, s.ws, args...)
}

func (s *scenario) ok(args ...string) Result {
	s.t.Helper()

	res := s.exec(args...)
	if res.Code != 0 {
		s.t.Fatalf("schepherd %s: exit code %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), res.Code, res.Stdout, res.Stderr)
	}

	return res
}

// fails expects exit code and an empty stdout, as for every schepherd error.
func (s *scenario) fails(code int, args ...string) Result {
	s.t.Helper()

	res := s.exec(args...)
	if res.Code != code || res.Stdout != "" || !strings.HasPrefix(res.Stderr, "schepherd: ") {
		s.t.Errorf("schepherd %s: exit code %d (want %d), stdout %q, stderr %q", strings.Join(args, " "), res.Code, code, res.Stdout, res.Stderr)
	}

	return res
}

func (s *scenario) expectStdout(want string, args ...string) {
	s.t.Helper()

	if res := s.ok(args...); res.Stdout != want {
		s.t.Errorf("schepherd %s: stdout %q, want %q", strings.Join(args, " "), res.Stdout, want)
	}
}

func (s *scenario) decode(res Result, v any) {
	s.t.Helper()

	if err := json.Unmarshal([]byte(res.Stdout), v); err != nil {
		s.t.Fatalf("stdout is not the expected JSON: %v\n%s", err, res.Stdout)
	}
}

func (s *scenario) readJSON(path string, v any) {
	s.t.Helper()

	// bearer:disable go_gosec_filesystem_filereadtaint
	// The scenario created this file itself.
	data, err := os.ReadFile(path) //nolint:gosec // a file the scenario created
	if err != nil {
		s.t.Fatal(err)
	}

	if err := json.Unmarshal(data, v); err != nil {
		s.t.Fatalf("%s: %v\n%s", path, err, data)
	}
}

func (s *scenario) write(path, content string) {
	s.t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		s.t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

func (s *scenario) fileEquals(path string, want []byte) {
	s.t.Helper()

	// bearer:disable go_gosec_filesystem_filereadtaint
	// Reading the path the client under test printed is what this check verifies.
	got, err := os.ReadFile(path) //nolint:gosec // a path printed by the client under test
	if err != nil {
		s.t.Errorf("read %s: %v", path, err)

		return
	}

	if string(got) != string(want) {
		s.t.Errorf("%s holds %d bytes that differ from the expected %d", path, len(got), len(want))
	}
}

// sameFile compares files rather than strings, so short names, symlinked
// temporary directories and letter case never cause false failures.
func (s *scenario) sameFile(got, want string) {
	s.t.Helper()

	gotInfo, err := os.Stat(got)
	if err != nil {
		s.t.Errorf("stat %s: %v", got, err)

		return
	}

	wantInfo, err := os.Stat(want)
	if err != nil {
		s.t.Fatal(err)
	}

	if !os.SameFile(gotInfo, wantInfo) {
		s.t.Errorf("%s is not %s", got, want)
	}
}

func (s *scenario) resetRecords() {
	s.t.Helper()

	entries, err := os.ReadDir(s.records)
	if err != nil {
		s.t.Fatal(err)
	}

	for _, e := range entries {
		if err := os.Remove(filepath.Join(s.records, e.Name())); err != nil {
			s.t.Fatal(err)
		}
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}
