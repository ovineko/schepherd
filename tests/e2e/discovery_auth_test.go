//go:build e2e

package e2e

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// discoveryRunner is the [runner] section of the matching scenario: one
// testconsumer process per schema, which records its argv and SCHEMA_ID.
func discoveryRunner() string {
	return `
[runner]
mode = "batch"
command = ` + tomlString(binPath("testconsumer")) + `
args = ["{schema}", "{files...}"]

[runner.env]
SCHEMA_ID = "{schema-id}"
`
}

// discoveryMappings overrides the catalog locally: it resolves the
// dup1/dup2 ambiguity, takes alpha.json away from alpha, and matches a file
// whose name contains a literal backslash (escaped as \\ in the glob).
const discoveryMappings = `
[[mappings]]
file_match = ["compose.yml"]
schema = "dup2"

[[mappings]]
file_match = ["alpha.json", 'back\\slash.json']
schema = "delta"
`

// discoveryWorkspaceFiles exist in the matching workspace so that run can
// take them; resolve never reads them. Names with a backslash are single
// file names on Linux, not nested paths.
var discoveryWorkspaceFiles = []string{
	"alpha.json",
	"deep/nested/dir/alpha.json",
	"deep/beta.yaml",
	"x/y/gamma.json",
	".github/workflows/ci.yml",
	"sub/.github/workflows/ci.yml",
	"a/b/c/.github/workflows/release.yml",
	".github/workflows/ci.yaml",
	".github/workflows/nested/ci.yml",
	".github/ci.yml",
	"config/app.toml",
	"config/a/b/app.toml",
	"config/app.json",
	"sub/config/app.toml",
	"README.md",
	"Alpha.json",
	"compose.yml",
	"deep/compose.yml",
	`config\app.toml`,
	`.github\workflows\ci.yml`,
	`sub\alpha.json`,
	`deep\nested\beta.yaml`,
	`back\slash.json`,
	"back/slash.json",
}

// discoveryResolution is the documented output of resolve --json.
type discoveryResolution struct {
	File     string        `json:"file"`
	Path     string        `json:"path"`
	Schema   string        `json:"schema"`
	Origin   string        `json:"origin"`
	Artifact descriptorDoc `json:"artifact"`
}

// discoveryRecord is the part of a testconsumer record the matching
// scenario checks.
type discoveryRecord struct {
	Argv     []string          `json:"argv"`
	Env      map[string]string `json:"env"`
	ArgFiles map[string]struct {
		SHA256 string `json:"sha256"`
	} `json:"argFiles"`
}

// discoveryTask is one expected consumer process: a schema and its files
// (workspace-relative), in the order they must be passed.
type discoveryTask struct {
	schema string
	files  []string
}

// discoveryReport is the documented --report document of run.
type discoveryReport struct {
	ReportVersion int    `json:"reportVersion"`
	Mode          string `json:"mode"`
	ExitCode      int    `json:"exitCode"`
	Tasks         []struct {
		SchemaID string   `json:"schemaId"`
		Files    []string `json:"files"`
		Status   string   `json:"status"`
		ExitCode int      `json:"exitCode"`
	} `json:"tasks"`
	Skipped []discoverySkipped `json:"skipped"`
}

type discoverySkipped struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

// discoveryMatching is the shared state of TestE33_Matching: one published
// set-basic snapshot, one workspace and two configurations (catalog only,
// and catalog plus local [[mappings]]).
type discoveryMatching struct {
	sb       *sandbox
	ws       string
	prepared string
	catalog  catalogDoc
	plain    string
	mapped   string
}

// TestE33_Matching checks the matching dialect end to end: basename
// patterns at any depth, .github workflow and ** patterns, backslashes as
// ordinary name characters on Linux, no match and ambiguity (exit 3), the
// local [[mappings]] level, --schema above everything and
// --ignore-unmatched, which skips unmatched files but never ambiguous ones.
func TestE33_Matching(t *testing.T) {
	m := newDiscoveryMatching(t)

	t.Run("catalog fileMatch", func(t *testing.T) { m.catalogPatterns(t) })
	t.Run("backslash is not a separator", func(t *testing.T) { m.backslashes(t) })
	t.Run("workspace-relative paths", func(t *testing.T) { m.workspaceRelative(t) })
	t.Run("ambiguous match", func(t *testing.T) { m.ambiguous(t) })
	t.Run("local mappings", func(t *testing.T) { m.mappings(t) })
	t.Run("--schema overrides every level", func(t *testing.T) { m.schemaFlag(t) })
	t.Run("--ignore-unmatched", func(t *testing.T) { m.ignoreUnmatched(t) })
}

func newDiscoveryMatching(t *testing.T) *discoveryMatching {
	t.Helper()

	path := repoPath(t)
	repo := suite.source.Repo(path)
	pub := publishSet(t, repo, newSet(t, "set-basic"), "--now", "20260101.0000")

	sb := sandboxOf(t)

	// The binaries see the physical working directory, so every expected
	// absolute path is built from the symlink-free workspace.
	ws, err := filepath.EvalSymlinks(sb.Workspace)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}

	files := make(map[string]string, len(discoveryWorkspaceFiles))
	for _, f := range discoveryWorkspaceFiles {
		files[f] = "{}\n"
	}

	writeFiles(t, ws, files)

	runner := discoveryRunner()
	base := clientConfig{Repository: repo, Catalog: pub.CatalogDigest, Extra: runner}
	mapped := base
	mapped.Extra = runner + discoveryMappings

	return &discoveryMatching{
		sb:       sb,
		ws:       ws,
		prepared: pub.Prepared,
		catalog:  discoveryCatalog(t, suite.source, path, pub.CatalogDigest),
		plain:    writeConfig(t, t.TempDir(), base.TOML()),
		mapped:   writeConfig(t, t.TempDir(), mapped.TOML()),
	}
}

// cli runs schepherd with the scenario's sandbox in dir (default: the
// workspace) and the given configuration.
func (m *discoveryMatching) cli(t *testing.T, config, dir string, env []string, args ...string) result {
	t.Helper()

	if dir == "" {
		dir = m.ws
	}

	return cli(t, runOpts{Sandbox: m.sb, Dir: dir, Env: env}, append([]string{"--config", config}, args...)...)
}

func (m *discoveryMatching) resolve(t *testing.T, config, file string) result {
	t.Helper()

	return m.cli(t, config, "", nil, "resolve", "--file", file, "--json")
}

// run runs "schepherd run args..." in the workspace and returns the result
// and the testconsumer records in start order.
func (m *discoveryMatching) run(t *testing.T, config string, args ...string) (result, []discoveryRecord) {
	t.Helper()

	logDir := t.TempDir()
	res := m.cli(t, config, "", []string{"TC_LOG_DIR=" + logDir}, append([]string{"run"}, args...)...)

	return res, discoveryRecords(t, logDir)
}

// wantResolution checks a successful resolve --json of a workspace-relative
// file.
func (m *discoveryMatching) wantResolution(t *testing.T, res result, file, schema, origin string) {
	t.Helper()

	if res.Code != 0 {
		t.Errorf("%s: want schema %s (%s), got:\n%s", file, schema, origin, res)

		return
	}

	got := decodeJSON[discoveryResolution](t, res.Stdout)
	want := discoveryResolution{
		File:     filepath.Join(m.ws, file),
		Path:     file,
		Schema:   schema,
		Origin:   origin,
		Artifact: m.catalog.entry(t, schema).Artifact,
	}

	if got != want {
		t.Errorf("%s: resolve --json = %+v, want %+v", file, got, want)
	}
}

var discoveryAmbiguity = regexp.MustCompile(`(?i)ambiguous`)

// discoveryAmbiguityRuns is how often an ambiguity error is reproduced to
// show that its candidate order does not change between runs.
const discoveryAmbiguityRuns = 10

// discoveryWantNoMatch checks that a command failed with exit code 3 before
// printing anything to stdout, with a no-match diagnostic that names file.
func discoveryWantNoMatch(t *testing.T, what string, res result, file string) {
	t.Helper()

	if res.Code != 3 || len(res.Stdout) != 0 || !bytes.Contains(res.Stderr, []byte(file)) || discoveryAmbiguity.Match(res.Stderr) {
		t.Errorf("%s: want exit code 3, empty stdout and a no-match diagnostic naming %s, got:\n%s", what, file, res)
	}
}

// discoveryWantAmbiguous checks that a command failed with exit code 3
// before printing anything to stdout, with an ambiguity diagnostic that
// names file and lists both candidates, dup1 and dup2.
func discoveryWantAmbiguous(t *testing.T, what string, res result, file string) {
	t.Helper()

	if res.Code != 3 || len(res.Stdout) != 0 || !bytes.Contains(res.Stderr, []byte(file)) {
		t.Errorf("%s: want exit code 3, empty stdout and a diagnostic naming %s, got:\n%s", what, file, res)
	}

	for _, id := range []string{"dup1", "dup2"} {
		if !bytes.Contains(res.Stderr, []byte(id)) {
			t.Errorf("%s: the ambiguity error does not list %s:\n%s", what, id, res.Stderr)
		}
	}
}

func (m *discoveryMatching) catalogPatterns(t *testing.T) {
	t.Helper()

	cases := []struct{ file, schema string }{
		{"alpha.json", "alpha"},
		{"deep/nested/dir/alpha.json", "alpha"},
		{"deep/beta.yaml", "beta"},
		{"x/y/gamma.json", "gamma"},
		{".github/workflows/ci.yml", "alpha"},
		{"sub/.github/workflows/ci.yml", "alpha"},
		{"a/b/c/.github/workflows/release.yml", "alpha"},
		{"config/app.toml", "beta"},
		{"config/a/b/app.toml", "beta"},
		{".github/workflows/ci.yaml", ""},
		{".github/workflows/nested/ci.yml", ""},
		{".github/ci.yml", ""},
		{"config/app.json", ""},
		{"sub/config/app.toml", ""},
		{"README.md", ""},
		{"Alpha.json", ""},
		{"not-there/alpha.json", "alpha"},
	}

	for _, c := range cases {
		res := m.resolve(t, m.plain, c.file)
		if c.schema == "" {
			discoveryWantNoMatch(t, "resolve "+c.file, res, c.file)

			continue
		}

		m.wantResolution(t, res, c.file, c.schema, "catalog")
	}

	res := m.cli(t, m.plain, "", nil, "resolve", "--file", "x/y/gamma.json").ok(t)
	if string(res.Stdout) != "gamma\n" {
		t.Errorf("resolve without --json printed %q, want %q", res.Stdout, "gamma\n")
	}
}

func (m *discoveryMatching) backslashes(t *testing.T) {
	t.Helper()

	for _, file := range []string{`config\app.toml`, `.github\workflows\ci.yml`, `sub\alpha.json`, `deep\nested\beta.yaml`, `back\slash.json`} {
		discoveryWantNoMatch(t, "resolve "+file, m.resolve(t, m.plain, file), file)
	}

	m.wantResolution(t, m.resolve(t, m.mapped, `back\slash.json`), `back\slash.json`, "delta", "mapping")
	discoveryWantNoMatch(t, "resolve back/slash.json with mappings", m.resolve(t, m.mapped, "back/slash.json"), "back/slash.json")
}

func (m *discoveryMatching) workspaceRelative(t *testing.T) {
	t.Helper()

	nested := filepath.Join(m.ws, "deep", "nested")

	cases := []struct{ file, path, schema string }{
		{"dir/alpha.json", "deep/nested/dir/alpha.json", "alpha"},
		{"../../.github/workflows/ci.yml", ".github/workflows/ci.yml", "alpha"},
		{filepath.Join(m.ws, "config", "a", "b", "app.toml"), "config/a/b/app.toml", "beta"},
	}

	for _, c := range cases {
		res := m.cli(t, m.plain, nested, nil, "--workspace", m.ws, "resolve", "--file", c.file, "--json")
		if res.Code != 0 {
			t.Errorf("%s from %s: want %s, got:\n%s", c.file, nested, c.schema, res)

			continue
		}

		got := decodeJSON[discoveryResolution](t, res.Stdout)
		want := discoveryResolution{
			File:     filepath.Join(m.ws, filepath.FromSlash(c.path)),
			Path:     c.path,
			Schema:   c.schema,
			Origin:   "catalog",
			Artifact: m.catalog.entry(t, c.schema).Artifact,
		}

		if got != want {
			t.Errorf("%s from %s: resolve --json = %+v, want %+v", c.file, nested, got, want)
		}
	}

	res := m.cli(t, m.plain, nested, nil, "resolve", "--file", "dir/alpha.json", "--json").ok(t)
	if got := decodeJSON[discoveryResolution](t, res.Stdout); got.Path != "dir/alpha.json" || got.Schema != "alpha" {
		t.Errorf("with the working directory as workspace: %+v, want path dir/alpha.json and schema alpha", got)
	}

	discoveryWantNoMatch(t, "a path outside the workspace",
		m.cli(t, m.plain, nested, nil, "resolve", "--file", "../../.github/workflows/ci.yml", "--json"),
		filepath.Join(m.ws, ".github", "workflows", "ci.yml"))
}

func (m *discoveryMatching) ambiguous(t *testing.T) {
	t.Helper()

	for _, file := range []string{"compose.yml", "deep/compose.yml"} {
		first := m.resolve(t, m.plain, file)
		discoveryWantAmbiguous(t, "resolve "+file, first, file)

		for range discoveryAmbiguityRuns - 1 {
			if next := m.resolve(t, m.plain, file); !bytes.Equal(first.Stderr, next.Stderr) || next.Code != first.Code {
				t.Errorf("resolve %s: the ambiguity error is not deterministic:\n%s\n---\n%s", file, first.Stderr, next.Stderr)

				break
			}
		}
	}
}

func (m *discoveryMatching) mappings(t *testing.T) {
	t.Helper()

	cases := []struct{ file, schema, origin string }{
		{"compose.yml", "dup2", "mapping"},
		{"deep/compose.yml", "dup2", "mapping"},
		{"alpha.json", "delta", "mapping"},
		{"deep/nested/dir/alpha.json", "delta", "mapping"},
		{".github/workflows/ci.yml", "alpha", "catalog"},
		{"config/a/b/app.toml", "beta", "catalog"},
		{"x/y/gamma.json", "gamma", "catalog"},
	}

	for _, c := range cases {
		m.wantResolution(t, m.resolve(t, m.mapped, c.file), c.file, c.schema, c.origin)
	}

	discoveryWantNoMatch(t, "resolve README.md with mappings", m.resolve(t, m.mapped, "README.md"), "README.md")

	res, records := m.run(t, m.mapped, "--", "compose.yml", ".github/workflows/ci.yml", "alpha.json", "config/a/b/app.toml", "deep/compose.yml")
	res.ok(t)
	m.wantTasks(t, records, []discoveryTask{
		{"dup2", []string{"compose.yml", "deep/compose.yml"}},
		{"alpha", []string{".github/workflows/ci.yml"}},
		{"delta", []string{"alpha.json"}},
		{"beta", []string{"config/a/b/app.toml"}},
	})
}

func (m *discoveryMatching) schemaFlag(t *testing.T) {
	t.Helper()

	outside := filepath.Join(t.TempDir(), "outside.json")
	writeFile(t, outside, []byte("{}\n"))

	files := []string{"compose.yml", "alpha.json", "README.md", `config\app.toml`, ".github/workflows/ci.yml", outside}

	for _, config := range []string{m.plain, m.mapped} {
		res, records := m.run(t, config, append([]string{"--schema", "gamma", "--"}, files...)...)
		res.ok(t)
		m.wantTasks(t, records, []discoveryTask{{"gamma", files}})
	}
}

func (m *discoveryMatching) ignoreUnmatched(t *testing.T) {
	t.Helper()

	t.Run("no match fails without the flag", func(t *testing.T) {
		res, records := m.run(t, m.plain, "--", "alpha.json", "README.md")
		discoveryWantNoMatch(t, "run README.md", res, "README.md")

		if len(records) != 0 {
			t.Errorf("%d consumers started although planning failed", len(records))
		}
	})

	t.Run("unmatched files are skipped and reported", func(t *testing.T) {
		m.ignoreUnmatchedSkips(t)
	})

	t.Run("ambiguous files still fail", func(t *testing.T) {
		res, records := m.run(t, m.plain, "--ignore-unmatched", "--", "alpha.json", "README.md", "compose.yml")
		discoveryWantAmbiguous(t, "run --ignore-unmatched compose.yml", res, "compose.yml")

		if len(records) != 0 {
			t.Errorf("%d consumers started although compose.yml is ambiguous", len(records))
		}
	})
}

func (m *discoveryMatching) ignoreUnmatchedSkips(t *testing.T) {
	t.Helper()

	report := filepath.Join(t.TempDir(), "report.json")
	res, records := m.run(t, m.plain, "--ignore-unmatched", "--report", report, "--",
		"README.md", "alpha.json", `config\app.toml`, ".github/workflows/ci.yml", "config/a/b/app.toml")
	res.ok(t)

	m.wantTasks(t, records, []discoveryTask{
		{"alpha", []string{"alpha.json", ".github/workflows/ci.yml"}},
		{"beta", []string{"config/a/b/app.toml"}},
	})

	skipped := []string{filepath.Join(m.ws, "README.md"), filepath.Join(m.ws, `config\app.toml`)}
	for _, f := range skipped {
		if !bytes.Contains(res.Stderr, []byte(f)) {
			t.Errorf("stderr does not report the skipped file %s:\n%s", f, res.Stderr)
		}
	}

	rep := decodeJSON[discoveryReport](t, readFile(t, report))
	if rep.ReportVersion != 1 || rep.Mode != "batch" || rep.ExitCode != 0 {
		t.Errorf("report header %+v", rep)
	}

	wantSkipped := []discoverySkipped{{skipped[0], "unmatched"}, {skipped[1], "unmatched"}}

	gotSkipped := slices.Clone(rep.Skipped)
	slices.SortFunc(gotSkipped, func(a, b discoverySkipped) int { return strings.Compare(a.File, b.File) })
	slices.SortFunc(wantSkipped, func(a, b discoverySkipped) int { return strings.Compare(a.File, b.File) })

	if !slices.Equal(gotSkipped, wantSkipped) {
		t.Errorf("report skipped %+v, want %+v", rep.Skipped, wantSkipped)
	}

	tasks := make([]string, 0, len(rep.Tasks))
	for _, task := range rep.Tasks {
		tasks = append(tasks, task.SchemaID+" "+task.Status+" "+strconv.Itoa(task.ExitCode)+" "+strings.Join(task.Files, ","))
	}

	abs := func(rel string) string { return filepath.Join(m.ws, filepath.FromSlash(rel)) }
	want := []string{
		"alpha ok 0 " + abs("alpha.json") + "," + abs(".github/workflows/ci.yml"),
		"beta ok 0 " + abs("config/a/b/app.toml"),
	}

	if !slices.Equal(tasks, want) {
		t.Errorf("report tasks %q, want %q", tasks, want)
	}
}

// wantTasks checks the consumer processes of a batch run: one per schema,
// in order, each with the materialized schema of that ID as {schema}, the
// ID in SCHEMA_ID and its files as absolute paths in input order. Relative
// expected files are relative to the workspace.
func (m *discoveryMatching) wantTasks(t *testing.T, records []discoveryRecord, want []discoveryTask) {
	t.Helper()

	if len(records) != len(want) {
		t.Errorf("%d consumer processes, want %d (%v):\n%+v", len(records), len(want), want, records)

		return
	}

	for i, rec := range records {
		w := want[i]

		if len(rec.Argv) < 2 {
			t.Errorf("process %d: argv %q", i, rec.Argv)

			continue
		}

		if got := rec.Env["SCHEMA_ID"]; got != w.schema {
			t.Errorf("process %d: SCHEMA_ID %q, want %q", i, got, w.schema)
		}

		schemaFile := rec.Argv[1]
		if got, want := rec.ArgFiles[schemaFile].SHA256, sha256Hex(preparedSchema(t, m.prepared, w.schema)); got != want {
			t.Errorf("process %d: {schema} %s has sha256 %q, want the %s schema (%s)", i, schemaFile, got, w.schema, want)
		}

		files := make([]string, 0, len(w.files))
		for _, f := range w.files {
			if !filepath.IsAbs(f) {
				f = filepath.Join(m.ws, filepath.FromSlash(f))
			}

			files = append(files, f)
		}

		if !slices.Equal(rec.Argv[2:], files) {
			t.Errorf("process %d (%s): files %q, want %q", i, w.schema, rec.Argv[2:], files)
		}
	}
}

var discoveryRecordName = regexp.MustCompile(`^([0-9]+)-[0-9]+\.json$`)

// discoveryRecords reads the testconsumer records of dir in start order.
// Any other entry fails the test: a consumer that started but did not
// finish its record leaves a .testconsumer-*.tmp file behind, so an empty
// result proves that no consumer started.
func discoveryRecords(t *testing.T, dir string) []discoveryRecord {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read consumer records: %v", err)
	}

	type named struct {
		nano int64
		rec  discoveryRecord
	}

	recs := make([]named, 0, len(entries))

	for _, e := range entries {
		match := discoveryRecordName.FindStringSubmatch(e.Name())
		if match == nil {
			t.Fatalf("unexpected entry %s in the consumer log directory %s", e.Name(), dir)
		}

		nano, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			t.Fatalf("consumer record name %s: %v", e.Name(), err)
		}

		recs = append(recs, named{nano, decodeJSON[discoveryRecord](t, readFile(t, filepath.Join(dir, e.Name())))})
	}

	slices.SortFunc(recs, func(a, b named) int { return cmp.Compare(a.nano, b.nano) })

	out := make([]discoveryRecord, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.rec)
	}

	return out
}

// discoveryCatalog fetches the catalog document of a catalog index
// directly from the registry.
func discoveryCatalog(t *testing.T, reg *registry, path, digest string) catalogDoc {
	t.Helper()

	return reg.Catalog(t, path, digest).Doc
}

// discoveryAuth is the shared state of TestE34_AuthTLS.
type discoveryAuth struct {
	src, dst *registry
	srcPath  string
	pub      publishResult
	catalog  catalogDoc
	// decoys are credentials no registry accepts; they sit next to the real
	// ones in files that must not be used for their host.
	decoys  []credential
	secrets []string
	// start precedes every request of the test; registry log lines from
	// then on belong to it, since no other test runs concurrently.
	start time.Time
}

// discoveryMirrorResult is the documented output of mirror --json.
type discoveryMirrorResult struct {
	Source        string `json:"source"`
	Destination   string `json:"destination"`
	CatalogDigest string `json:"catalogDigest"`
	Revision      string `json:"revision"`
	Schemas       int    `json:"schemas"`
}

var (
	discoveryAuthMessage = regexp.MustCompile(`(?i)authentication|authenticate|unauthori[sz]ed|credential`)
	discoveryTLSMessage  = regexp.MustCompile(`(?i)\btls\b|x509|certificate`)

	discoveryLogStamp   = regexp.MustCompile(`(?:^| )time="([^"]+)"`)
	discoveryClientLine = regexp.MustCompile(`http\.request\.useragent="?schepherd/`)
)

// TestE34_AuthTLS checks the TLS registries with htpasswd: no credentials
// is an authentication failure (exit 4), a Docker config.json for the host
// works, a missing ca_file is a TLS failure (exit 4), and a mirror between
// two registries with different users sends each registry only its own
// credential: the schepherd client authenticated to each registry as its
// own user, and neither registry rejected a credential while the test ran.
// No output ever contains a password.
func TestE34_AuthTLS(t *testing.T) {
	a := newDiscoveryAuth(t)

	t.Run("without credentials", func(t *testing.T) { a.withoutCredentials(t) })
	t.Run("docker config.json for the host", func(t *testing.T) { a.dockerConfig(t) })
	t.Run("without ca_file", func(t *testing.T) { a.withoutCAFile(t) })

	var dstPaths []string

	t.Run("mirror with a docker config.json for both hosts", func(t *testing.T) {
		sb := sandboxOf(t)
		writeDockerConfig(t, sb.DockerConfig, map[string]credential{a.src.Host(): a.src.User(), a.dst.Host(): a.dst.User()})
		dstPaths = append(dstPaths, a.mirror(t, sb, nil))
	})

	t.Run("mirror with a credentials_file per host", func(t *testing.T) {
		sb := sandboxOf(t)
		writeDockerConfig(t, sb.DockerConfig, map[string]credential{a.src.Host(): a.decoys[2], a.dst.Host(): a.decoys[2]})
		srcCreds := writeDockerConfig(t, t.TempDir(), map[string]credential{a.src.Host(): a.src.User(), a.dst.Host(): a.decoys[0]})
		dstCreds := writeDockerConfig(t, t.TempDir(), map[string]credential{a.dst.Host(): a.dst.User(), a.src.Host(): a.decoys[1]})
		dstPaths = append(dstPaths, a.mirror(t, sb, map[string]*registrySettings{
			a.src.Host(): {CAFile: suite.pki.CAFile, CredentialsFile: srcCreds},
			a.dst.Host(): {CAFile: suite.pki.CAFile, CredentialsFile: dstCreds},
		}))
	})

	t.Run("each registry saw only its own user", func(t *testing.T) { a.checkLogs(t, dstPaths) })
}

func newDiscoveryAuth(t *testing.T) *discoveryAuth {
	t.Helper()

	a := &discoveryAuth{src: suite.auth, dst: suite.auth2, srcPath: repoPath(t), start: time.Now()}

	for _, name := range []string{"src-file", "dst-file", "docker-config"} {
		a.decoys = append(a.decoys, credential{Username: "e2e-decoy-" + name + "-" + randomHex(4), Password: randomHex(16)})
	}

	for _, c := range append([]credential{a.src.User(), a.dst.User()}, a.decoys...) {
		a.secrets = append(a.secrets, c.Password, base64.StdEncoding.EncodeToString([]byte(c.Username+":"+c.Password)))
	}

	prepared := prepareSet(t, newSet(t, "set-basic"))
	res := a.check(t, publisher(t, runOpts{}, "publish", "--prepared", prepared, "--repository", a.src.Repo(a.srcPath),
		"--registry-config", publisherRegistryConfig(t), "--json", "--now", "20260101.0000")).ok(t)

	a.pub = decodeJSON[publishResult](t, res.Stdout)
	a.pub.Prepared = prepared

	if a.pub.Status != "published" || !digestPattern.MatchString(a.pub.CatalogDigest) {
		t.Fatalf("publish to %s: %s", a.src.service, res.Stdout)
	}

	a.catalog = discoveryCatalog(t, a.src, a.srcPath, a.pub.CatalogDigest)

	entries := readPrepared(t, prepared).Entries
	if len(entries) == 0 || len(a.catalog.Schemas) != len(entries) {
		t.Fatalf("the catalog lists %d schemas, the prepared set has %d", len(a.catalog.Schemas), len(entries))
	}

	for _, e := range entries {
		a.catalog.entry(t, e.ID)
	}

	return a
}

// check fails the test when a command printed a password or an encoded
// credential, and returns the result.
func (a *discoveryAuth) check(t *testing.T, res result) result {
	t.Helper()

	for _, s := range a.secrets {
		if bytes.Contains(res.Stdout, []byte(s)) || bytes.Contains(res.Stderr, []byte(s)) {
			t.Fatalf("the output of %q contains a secret", strings.Join(res.Args, " "))
		}
	}

	return res
}

func (a *discoveryAuth) cli(t *testing.T, sb *sandbox, args ...string) result {
	t.Helper()

	return a.check(t, cli(t, runOpts{Sandbox: sb}, args...))
}

// srcConfig writes a client configuration for the published snapshot in the
// sandbox's workspace.
func (a *discoveryAuth) srcConfig(t *testing.T, sb *sandbox, registries map[string]*registrySettings) string {
	t.Helper()

	return writeConfig(t, sb.Workspace, clientConfig{
		Repository: a.src.Repo(a.srcPath), Catalog: a.pub.CatalogDigest, Registries: registries,
	}.TOML())
}

func (a *discoveryAuth) withoutCredentials(t *testing.T) {
	t.Helper()

	sb := sandboxOf(t)
	cfg := a.srcConfig(t, sb, nil)

	for _, args := range [][]string{{"catalog", "--json"}, {"path", "alpha"}} {
		res := a.cli(t, sb, append([]string{"--config", cfg}, args...)...).wantCode(t, 4)

		if len(res.Stdout) != 0 || !discoveryAuthMessage.Match(res.Stderr) || discoveryTLSMessage.Match(res.Stderr) {
			t.Errorf("want an authentication error (not a TLS error) on stderr and empty stdout, got:\n%s", res)
		}
	}
}

func (a *discoveryAuth) dockerConfig(t *testing.T) {
	t.Helper()

	sb := sandboxOf(t)
	writeDockerConfig(t, sb.DockerConfig, map[string]credential{a.src.Host(): a.src.User()})
	cfg := a.srcConfig(t, sb, nil)

	res := a.cli(t, sb, "--config", cfg, "catalog", "--json").ok(t)
	if blob := a.src.Catalog(t, a.srcPath, a.pub.CatalogDigest).Blob; !bytes.Equal(res.Stdout, blob) {
		t.Errorf("catalog --json differs from the catalog blob:\n%s", res.Stdout)
	}

	res = a.cli(t, sb, "--config", cfg, "path", "alpha").ok(t)
	if got := readFile(t, strings.TrimSuffix(string(res.Stdout), "\n")); !bytes.Equal(got, preparedSchema(t, a.pub.Prepared, "alpha")) {
		t.Errorf("path alpha: %s is not the prepared alpha schema", res.Stdout)
	}

	res = a.cli(t, sb, "--config", cfg, "cat", "gamma").ok(t)
	if !bytes.Equal(res.Stdout, preparedSchema(t, a.pub.Prepared, "gamma")) {
		t.Error("cat gamma differs from the prepared gamma schema")
	}
}

func (a *discoveryAuth) withoutCAFile(t *testing.T) {
	t.Helper()

	sb := sandboxOf(t)
	writeDockerConfig(t, sb.DockerConfig, map[string]credential{a.src.Host(): a.src.User()})
	cfg := a.srcConfig(t, sb, map[string]*registrySettings{a.src.Host(): nil})

	res := a.cli(t, sb, "--config", cfg, "catalog", "--json").wantCode(t, 4)
	if len(res.Stdout) != 0 || !discoveryTLSMessage.Match(res.Stderr) || discoveryAuthMessage.Match(res.Stderr) {
		t.Errorf("want a TLS error (not an authentication error) on stderr and empty stdout, got:\n%s", res)
	}
}

// mirror copies the published snapshot from registry-auth to a fresh
// repository of registry-auth2 with the sandbox's credentials and registry
// settings, verifies the copy and returns its repository path.
func (a *discoveryAuth) mirror(t *testing.T, sb *sandbox, registries map[string]*registrySettings) string {
	t.Helper()

	dstPath := repoPath(t)
	src, dst := a.src.Repo(a.srcPath), a.dst.Repo(dstPath)
	cfg := writeConfig(t, sb.Workspace, clientConfig{Registries: registries}.TOML())

	res := a.cli(t, sb, "--config", cfg, "mirror", src+"@"+a.pub.CatalogDigest, dst, "--json").ok(t)

	out := decodeJSON[discoveryMirrorResult](t, res.Stdout)
	if out.Source != src || out.Destination != dst || out.CatalogDigest != a.pub.CatalogDigest || out.Revision != a.pub.Revision {
		t.Errorf("mirror result %s", res.Stdout)
	}

	if !bytes.Equal(a.dst.Manifest(t, dstPath, a.pub.CatalogDigest).Body, a.src.Manifest(t, a.srcPath, a.pub.CatalogDigest).Body) {
		t.Error("the mirrored catalog index differs from the source")
	}

	want := map[string]string{"catalog-" + a.pub.Revision: a.pub.CatalogDigest}
	distinct := map[string]bool{}

	for _, e := range a.catalog.Schemas {
		distinct[e.Artifact.Digest] = true

		if status := a.dst.ManifestStatus(t, dstPath, e.Artifact.Digest); status != 200 {
			t.Errorf("schema %s is missing in the destination (HEAD status %d)", e.ID, status)
		}
	}

	if schemas := len(distinct); out.Schemas != schemas {
		t.Errorf("mirror --json reports %d schemas, the catalog lists %d distinct schema manifests", out.Schemas, schemas)
	}

	tags := a.dst.Tags(t, dstPath)

	for _, tag := range slices.Sorted(maps.Keys(want)) {
		if !slices.Contains(tags, tag) {
			t.Errorf("destination tags %v lack %s", tags, tag)

			continue
		}

		if got := a.dst.Manifest(t, dstPath, tag).Digest; got != want[tag] {
			t.Errorf("destination tag %s points to %s, want %s", tag, got, want[tag])
		}
	}

	reader := newSandbox(t)
	writeDockerConfig(t, reader.DockerConfig, map[string]credential{a.dst.Host(): a.dst.User()})
	readCfg := writeConfig(t, reader.Workspace, clientConfig{Repository: dst, Catalog: a.pub.CatalogDigest}.TOML())

	got := a.cli(t, reader, "--config", readCfg, "cat", "beta").ok(t)
	if !bytes.Equal(got.Stdout, preparedSchema(t, a.pub.Prepared, "beta")) {
		t.Error("cat beta from the mirror differs from the prepared schema")
	}

	return dstPath
}

// checkLogs reads the registries' logs. distribution/distribution:3.1.2 names the user only
// on success (auth.user.name=) and when a user of its own htpasswd sends a
// wrong password ("user failed to authenticate", username=); a user it does
// not know yields only "authentication failure". The suite's users and the
// decoys are distinct on the two registries, so a credential sent to the
// wrong registry never shows up as a foreign user name: it shows up as a
// rejected credential, and none may appear from the test's start on,
// whatever the URI. A request without credentials is logged as "invalid
// authorization credential" and is not a rejection.
func (a *discoveryAuth) checkLogs(t *testing.T, dstPaths []string) {
	t.Helper()

	logs := map[string]string{serviceAuth: serviceLogs(t, serviceAuth), serviceAuth2: serviceLogs(t, serviceAuth2)}

	if !discoveryClientAuthenticated(logs[serviceAuth], a.srcPath, a.src.User().Username) {
		t.Errorf("%s logs show no schepherd request for %s authenticated as %s", serviceAuth, a.srcPath, a.src.User().Username)
	}

	for _, p := range dstPaths {
		if !discoveryClientAuthenticated(logs[serviceAuth2], p, a.dst.User().Username) {
			t.Errorf("%s logs show no schepherd request for %s authenticated as %s", serviceAuth2, p, a.dst.User().Username)
		}
	}

	for service, text := range logs {
		for line := range strings.SplitSeq(text, "\n") {
			if !strings.Contains(line, "authentication failure") && !strings.Contains(line, `msg="user failed to authenticate"`) {
				continue
			}

			if stamp, ok := discoveryLogTime(line); !ok || !stamp.Before(a.start) {
				t.Errorf("%s rejected a credential while the test ran:\n%s", service, line)
			}
		}

		for _, s := range a.secrets {
			if strings.Contains(text, s) {
				t.Errorf("%s logs contain a secret", service)
			}
		}
	}
}

// discoveryClientAuthenticated reports whether a registry log records a
// request of the schepherd client (not the publisher's or the harness's) to
// repo that authenticated as user.
func discoveryClientAuthenticated(logs, repo, user string) bool {
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(line, "/v2/"+repo+"/") && strings.Contains(line, " auth.user.name="+user+" ") &&
			discoveryClientLine.MatchString(line) {
			return true
		}
	}

	return false
}

// discoveryLogTime returns the time of a registry application log line.
func discoveryLogTime(line string) (time.Time, bool) {
	match := discoveryLogStamp.FindStringSubmatch(line)
	if match == nil {
		return time.Time{}, false
	}

	stamp, err := time.Parse(time.RFC3339Nano, match[1])

	return stamp, err == nil
}
