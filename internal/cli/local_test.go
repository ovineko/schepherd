package cli

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const (
	companySchema = `{"$schema":"https://json-schema.org/draft/2020-12/schema","$ref":"common.schema.json"}`
	commonSchema  = `{"type":"object","required":["name"]}`
	localAlpha    = `{"type":"object","description":"local override of alpha"}`
)

// localProject is a repository-like workspace: its configuration lives in
// conf/ and names schemas in schemas/ by paths relative to itself.
type localProject struct {
	ws, config, cache, marker string
}

func (p localProject) schema(name string) string {
	return filepath.Join(p.ws, "schemas", name)
}

func (p localProject) file(rel string) string {
	return filepath.Join(p.ws, filepath.FromSlash(rel))
}

// args prefixes the flags every command of a scenario uses.
func (p localProject) args(args ...string) []string {
	return append([]string{"--config", p.config, "--workspace", p.ws, "--cache-dir", p.cache}, args...)
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// localRunner is a batch [runner] section that starts the fake consumer
// with every schema placeholder in its arguments.
func localRunner(t *testing.T, marker string) string {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	return "[runner]\ncommand = " + strconv.Quote(exe) +
		"\nargs = [\"{schema}\", \"--id={schema-id}\", \"--ref={schema-ref}\", \"--cache={cache}\", \"{files...}\"]\n\n[runner.env]\n" +
		consumerMarkerVar + " = " + strconv.Quote(marker) + "\n" +
		consumerExitVar + " = \"0\"\n" +
		"GORACE = \"atexit_sleep_ms=0\"\n" +
		"GOCOVERDIR = " + strconv.Quote(t.TempDir()) + "\n"
}

// newLocalProject writes the workspace and a configuration with the given
// sections after config_version.
func newLocalProject(t *testing.T, sections string) localProject {
	t.Helper()

	root := t.TempDir()
	p := localProject{
		ws:     filepath.Join(root, "ws"),
		cache:  filepath.Join(root, "cache"),
		marker: filepath.Join(root, "consumer-args"),
	}
	p.config = filepath.Join(p.ws, "conf", "schepherd.toml")

	writeTestFile(t, p.schema("company.schema.json"), companySchema)
	writeTestFile(t, p.schema("common.schema.json"), commonSchema)
	writeTestFile(t, p.schema("alpha.schema.json"), localAlpha)

	for _, rel := range []string{"config/company.json", "config/team.json", "config/legacy.json", "alpha.json", "beta.json", "local-alpha.json", "notes/readme.json"} {
		writeTestFile(t, p.file(rel), `{"name":"x"}`)
	}

	writeTestFile(t, p.config, "config_version = 1\n\n"+sections+"\n"+localRunner(t, p.marker))

	return p
}

const companySection = `[schemas.company]
path = "../schemas/company.schema.json"
file_match = ["config/*.json", "!config/legacy.json"]
`

func mustRun(t *testing.T, args ...string) string {
	t.Helper()

	stdout, stderr, code := run(t, args...)
	if code != 0 {
		t.Fatalf("schepherd %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, stdout, stderr)
	}

	return stdout
}

func mustFail(t *testing.T, code int, args ...string) string {
	t.Helper()

	stdout, stderr, got := run(t, args...)
	if got != code || stdout != "" {
		t.Fatalf("schepherd %s: exit %d (want %d), stdout %q, stderr %s", strings.Join(args, " "), got, code, stdout, stderr)
	}

	return stderr
}

func decodeInto[T any](t *testing.T, data string) T {
	t.Helper()

	var v T
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		t.Fatalf("not the expected JSON: %v\n%s", err, data)
	}

	return v
}

func readReport(t *testing.T, path string) localReport {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return decodeInto[localReport](t, string(data))
}

type localReport struct {
	Tasks []struct {
		SchemaID  string   `json:"schemaId"`
		SchemaRef string   `json:"schemaRef"`
		Origin    string   `json:"origin"`
		Status    string   `json:"status"`
		Files     []string `json:"files"`
	} `json:"tasks"`
	Skipped []struct {
		File   string `json:"file"`
		Reason string `json:"reason"`
	} `json:"skipped"`
	ExitCode int `json:"exitCode"`
}

// consumerArgs returns the arguments the fake consumer received and removes
// the record, so the next run starts clean.
func consumerArgs(t *testing.T, marker string) []string {
	t.Helper()

	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the consumer did not run: %v", err)
	}

	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	return strings.Split(string(data), "\n")
}

func noConsumer(t *testing.T, marker string) {
	t.Helper()

	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a consumer ran: %v", err)
	}
}

func noCache(t *testing.T, dir string) {
	t.Helper()

	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the cache directory %s was created (%v); local schemas must not touch it", dir, err)
	}
}

// Without any catalog configured, every command that only needs local
// schemas works, online and offline, and never creates the cache.
func TestLocalSchemasWithoutCatalog(t *testing.T) {
	for _, offline := range []bool{false, true} {
		t.Run("offline="+strconv.FormatBool(offline), func(t *testing.T) {
			p := newLocalProject(t, companySection)
			args := func(a ...string) []string {
				if offline {
					a = append([]string{"--offline"}, a...)
				}

				return p.args(a...)
			}

			company := p.schema("company.schema.json")

			if got := mustRun(t, args("path", "company")...); got != company+"\n" {
				t.Errorf("path = %q, want the local file %s itself", got, company)
			}

			if got := mustRun(t, args("path", "--null", "company")...); got != company+"\x00" {
				t.Errorf("path --null = %q", got)
			}

			if got := mustRun(t, args("cat", "company")...); got != companySchema {
				t.Errorf("cat = %q", got)
			}

			dest := filepath.Join(t.TempDir(), "company.json")
			mustRun(t, args("export", "company", dest)...)
			mustFail(t, 2, args("export", "company", dest)...)
			mustRun(t, args("export", "--force", "company", dest)...)

			if data, err := os.ReadFile(dest); err != nil || string(data) != companySchema {
				t.Errorf("export wrote %q, %v", data, err)
			}

			if got := mustRun(t, args("resolve", "--file", p.file("config/team.json"))...); got != "company\n" {
				t.Errorf("resolve = %q", got)
			}

			got := decodeInto[map[string]any](t, mustRun(t, args("resolve", "--file", p.file("config/team.json"), "--json")...))
			want := map[string]any{"file": p.file("config/team.json"), "path": "config/team.json", "schema": "company", "origin": "local", "schemaPath": company}

			if !reflect.DeepEqual(got, want) {
				t.Errorf("resolve --json\n got %v\nwant %v", got, want)
			}

			mustFail(t, 3, args("resolve", "--file", p.file("config/legacy.json"))...)

			report := filepath.Join(t.TempDir(), "report.json")
			mustRun(t, args("run", "--report", report, "--", p.file("config/company.json"), p.file("config/team.json"))...)

			wantArgs := []string{
				company, "--id=company", "--ref=local:schemas/company.schema.json", "--cache=" + p.cache, p.file("config/company.json"), p.file("config/team.json"),
			}
			if got := consumerArgs(t, p.marker); !reflect.DeepEqual(got, wantArgs) {
				t.Errorf("consumer arguments\n got %q\nwant %q", got, wantArgs)
			}

			rep := readReport(t, report)
			if len(rep.Tasks) != 1 || rep.Tasks[0].Origin != "local" || rep.Tasks[0].SchemaRef != "local:schemas/company.schema.json" || rep.Tasks[0].Status != "ok" {
				t.Errorf("report = %+v", rep)
			}

			mustRun(t, args("run", "--schema", "company", "--", p.file("notes/readme.json"))...)
			consumerArgs(t, p.marker)

			mustRun(t, args("run", "--ignore-unmatched", "--report", report, "--", p.file("notes/readme.json"), p.file("config/team.json"))...)
			consumerArgs(t, p.marker)

			if rep := readReport(t, report); len(rep.Skipped) != 1 || rep.Skipped[0].Reason != "unmatched" || len(rep.Tasks) != 1 {
				t.Errorf("run --ignore-unmatched report = %+v", rep)
			}

			list := decodeInto[[]map[string]any](t, mustRun(t, args("list", "--json")...))
			if want := []map[string]any{{"id": "company", "origin": "local", "schemaPath": company, "shadows": false}}; !reflect.DeepEqual(list, want) {
				t.Errorf("list --json\n got %v\nwant %v", list, want)
			}

			if got := mustRun(t, args("list")...); got != "company\t"+company+"\tlocal\n" {
				t.Errorf("list = %q", got)
			}

			patterns := decodeInto[[]map[string]any](t, mustRun(t, args("patterns", "--json")...))
			wantPatterns := []map[string]any{{
				"id": "company", "fileMatch": []any{"config/*.json", "!config/legacy.json"}, "origin": "local", "schemaPath": company, "shadows": false,
			}}

			if !reflect.DeepEqual(patterns, wantPatterns) {
				t.Errorf("patterns --json\n got %v\nwant %v", patterns, wantPatterns)
			}

			if got := mustRun(t, args("patterns")...); got != "company\tconfig/*.json\tlocal\ncompany\t!config/legacy.json\tlocal\n" {
				t.Errorf("patterns = %q", got)
			}

			if got := mustRun(t, args("config", "check")...); got != "ok (1 file(s))\n" {
				t.Errorf("config check = %q", got)
			}

			stderr := mustFail(t, 2, args("path", "alpha")...)
			if !strings.Contains(stderr, `schema "alpha" is not declared in [schemas] and no catalog is configured`) {
				t.Errorf("a catalog id without a catalog: %s", stderr)
			}

			mustFail(t, 2, args("run", "--schema", "alpha", "--", p.file("alpha.json"))...)
			mustFail(t, 2, args("catalog")...)
			noConsumer(t, p.marker)
			noCache(t, p.cache)
		})
	}
}

func TestLocalSchemasNeverContactTheRegistry(t *testing.T) {
	isolateDockerConfig(t)

	reg := newTestRegistry(t)
	repo := reg.repo("org/schemas", "", "")
	dgst := reg.publishCatalog(t, "org/schemas")

	p := newLocalProject(t, companySection+`
[schemas.standalone]
path = "../schemas/common.schema.json"

[[mappings]]
file_match = ["notes/*.json"]
schema = "standalone"

[catalog]
repository = "`+repo+`"
digest = "`+dgst+`"

[registries.`+strconv.Quote(reg.host())+`]
plain_http = true
`)

	for _, args := range [][]string{
		{"path", "company"},
		{"cat", "standalone"},
		{"export", "company", filepath.Join(t.TempDir(), "company.json")},
		{"resolve", "--file", p.file("config/team.json"), "--json"},
		{"resolve", "--file", p.file("notes/readme.json")},
		{"run", "--", p.file("config/company.json"), p.file("notes/readme.json")},
		{"run", "--schema", "standalone", "--", p.file("alpha.json")},
		{"config", "check"},
	} {
		mustRun(t, p.args(args...)...)
	}

	if n := reg.requestCount(); n != 0 {
		t.Errorf("commands on local schemas sent %d registry request(s)", n)
	}

	noCache(t, p.cache)

	if got := mustRun(t, p.args("resolve", "--file", p.file("notes/readme.json"), "--json")...); !strings.Contains(got, `"origin": "mapping"`) {
		t.Errorf("a mapping to a local schema: %s", got)
	}

	mustRun(t, p.args("path", "alpha")...)

	if reg.requestCount() == 0 {
		t.Error("a catalog schema was served without the registry; the counter proves nothing")
	}
}

// A local schema with the ID of a catalog entry replaces that entry: its
// file is served instead of the artifact and the catalog's fileMatch for the
// ID no longer applies.
func TestLocalSchemaShadowsCatalogEntry(t *testing.T) {
	isolateDockerConfig(t)

	reg := newTestRegistry(t)
	repo := reg.repo("org/schemas", "", "")
	dgst := reg.publishCatalog(t, "org/schemas")

	p := newLocalProject(t, `[schemas.alpha]
path = "../schemas/alpha.schema.json"
file_match = ["local-alpha.json"]

[catalog]
repository = "`+repo+`"
digest = "`+dgst+`"

[registries.`+strconv.Quote(reg.host())+`]
plain_http = true
`)

	alpha := p.schema("alpha.schema.json")

	if got := mustRun(t, p.args("path", "alpha")...); got != alpha+"\n" {
		t.Errorf("path alpha = %q, want the local override", got)
	}

	if got := mustRun(t, p.args("cat", "alpha")...); got != localAlpha {
		t.Errorf("cat alpha = %q", got)
	}

	if n := reg.requestCount(); n != 0 {
		t.Errorf("the local override sent %d registry request(s)", n)
	}

	list := decodeInto[[]map[string]any](t, mustRun(t, p.args("list", "--json")...))
	if len(list) != 2 {
		t.Fatalf("list --json = %v", list)
	}

	if want := map[string]any{"id": "alpha", "origin": "local", "schemaPath": alpha, "shadows": true}; !reflect.DeepEqual(list[0], want) {
		t.Errorf("shadowing entry = %v, want %v", list[0], want)
	}

	if beta := list[1]; beta["id"] != "beta" || beta["origin"] != "catalog" || beta["name"] != "beta.json" || !strings.HasPrefix(beta["digest"].(string), "sha256:") {
		t.Errorf("catalog entry = %v", beta)
	}

	if got := mustRun(t, p.args("list")...); got != "alpha\t"+alpha+"\tlocal, shadows catalog\nbeta\tbeta.json\n" {
		t.Errorf("list = %q", got)
	}

	patterns := decodeInto[[]map[string]any](t, mustRun(t, p.args("patterns", "--json")...))
	if len(patterns) != 2 || patterns[0]["id"] != "alpha" || patterns[0]["shadows"] != true || !reflect.DeepEqual(patterns[0]["fileMatch"], []any{"local-alpha.json"}) ||
		patterns[0]["artifact"] != nil || patterns[1]["id"] != "beta" || patterns[1]["origin"] != "catalog" || patterns[1]["artifact"] == nil {
		t.Errorf("patterns --json = %v", patterns)
	}

	if got := mustRun(t, p.args("patterns")...); got != "alpha\tlocal-alpha.json\tlocal, shadows catalog\nbeta\tbeta.json\n" {
		t.Errorf("patterns = %q", got)
	}

	mustFail(t, 3, p.args("resolve", "--file", p.file("alpha.json"))...)

	resolved := decodeInto[map[string]any](t, mustRun(t, p.args("resolve", "--file", p.file("local-alpha.json"), "--json")...))
	if resolved["origin"] != "local" || resolved["schemaPath"] != alpha || resolved["artifact"] != nil {
		t.Errorf("resolve local-alpha.json = %v", resolved)
	}

	resolved = decodeInto[map[string]any](t, mustRun(t, p.args("resolve", "--file", p.file("beta.json"), "--json")...))
	if resolved["origin"] != "catalog" || resolved["schemaPath"] != nil || resolved["artifact"] == nil {
		t.Errorf("resolve beta.json = %v", resolved)
	}

	report := filepath.Join(t.TempDir(), "report.json")
	mustRun(t, p.args("run", "--report", report, "--", p.file("local-alpha.json"), p.file("beta.json"))...)

	rep := readReport(t, report)
	if len(rep.Tasks) != 2 || rep.Tasks[0].SchemaID != "alpha" || rep.Tasks[0].Origin != "local" || rep.Tasks[0].SchemaRef != "local:schemas/alpha.schema.json" ||
		rep.Tasks[1].SchemaID != "beta" || rep.Tasks[1].Origin != "catalog" || rep.Tasks[1].SchemaRef != repo+"@"+mustCatalogDigestOf(t, p, "beta") {
		t.Errorf("report = %+v", rep)
	}
}

func mustCatalogDigestOf(t *testing.T, p localProject, id string) string {
	t.Helper()

	for _, item := range decodeInto[[]map[string]any](t, mustRun(t, p.args("list", "--json")...)) {
		if item["id"] == id {
			return item["digest"].(string)
		}
	}

	t.Fatalf("%s is not in list --json", id)

	return ""
}

func TestLocalSchemaMatchingPrecedence(t *testing.T) {
	isolateDockerConfig(t)

	reg := newTestRegistry(t)
	repo := reg.repo("org/schemas", "", "")
	dgst := reg.publishCatalog(t, "org/schemas")

	p := newLocalProject(t, `[schemas.company]
path = "../schemas/company.schema.json"
file_match = ["alpha.json", "config/*.json"]

[schemas.team]
path = "../schemas/common.schema.json"
file_match = ["config/team.json"]

[[mappings]]
file_match = ["config/company.json"]
schema = "beta"

[catalog]
repository = "`+repo+`"
digest = "`+dgst+`"

[registries.`+strconv.Quote(reg.host())+`]
plain_http = true
`)

	resolve := func(rel string) map[string]any {
		t.Helper()

		return decodeInto[map[string]any](t, mustRun(t, p.args("resolve", "--json", "--file", p.file(rel))...))
	}

	if got := resolve("alpha.json"); got["schema"] != "company" || got["origin"] != "local" {
		t.Errorf("local file_match must win over the catalog's fileMatch: %v", got)
	}

	if n := reg.requestCount(); n != 0 {
		t.Errorf("a match through local file_match loaded the catalog (%d requests)", n)
	}

	if got := resolve("config/company.json"); got["schema"] != "beta" || got["origin"] != "mapping" {
		t.Errorf("[[mappings]] must win over local file_match: %v", got)
	}

	if got := resolve("beta.json"); got["schema"] != "beta" || got["origin"] != "catalog" {
		t.Errorf("catalog fileMatch applies when nothing local matches: %v", got)
	}

	stderr := mustFail(t, 3, p.args("resolve", "--file", p.file("config/team.json"))...)
	if !strings.Contains(stderr, "config/team.json matches several schemas via local rules: company, team") {
		t.Errorf("ambiguity among local schemas: %s", stderr)
	}

	mustFail(t, 3, p.args("run", "--", p.file("config/team.json"))...)
	noConsumer(t, p.marker)

	mustRun(t, p.args("run", "--schema", "team", "--", p.file("config/team.json"))...)

	if got := consumerArgs(t, p.marker); got[0] != p.schema("common.schema.json") || got[1] != "--id=team" {
		t.Errorf("--schema must beat every rule: %q", got)
	}
}

func TestLocalSchemaFileProblemsAreConfigurationErrors(t *testing.T) {
	p := newLocalProject(t, `[schemas.missing]
path = "../schemas/missing.json"
file_match = ["missing/*.json"]

[schemas.dir]
path = "../schemas"

[schemas.broken]
path = "../schemas/broken.json"
file_match = ["broken.json"]

[schemas.company]
path = "../schemas/company.schema.json"
file_match = ["config/*.json"]
`)

	writeTestFile(t, p.schema("broken.json"), `{"type": "object",}`)
	writeTestFile(t, p.file("broken.json"), `{}`)

	cases := map[string]string{
		"missing": p.schema("missing.json"),
		"dir":     filepath.Join(p.ws, "schemas") + " is a directory",
		"broken":  p.schema("broken.json") + " is not valid JSON",
	}

	for id, fragment := range cases {
		for _, args := range [][]string{{"path", id}, {"cat", id}, {"export", id, filepath.Join(t.TempDir(), "x.json")}, {"run", "--schema", id, "--", p.file("alpha.json")}} {
			stderr := mustFail(t, 2, p.args(args...)...)

			for _, want := range []string{"schepherd: usage error: ", p.config, "schemas." + id + ".path", `local schema "` + id + `"`, fragment} {
				if !strings.Contains(stderr, want) {
					t.Errorf("%s: stderr %q lacks %q", strings.Join(args, " "), stderr, want)
				}
			}
		}
	}

	for id, file := range map[string]string{"missing": "missing/doc.json", "broken": "broken.json"} {
		for _, args := range [][]string{{"resolve", "--file", p.file(file)}, {"resolve", "--json", "--file", p.file(file)}} {
			stderr := mustFail(t, 2, p.args(args...)...)
			if !strings.Contains(stderr, "schemas."+id+".path") || !strings.Contains(stderr, cases[id]) {
				t.Errorf("%s must report the unusable schema file: %s", strings.Join(args, " "), stderr)
			}
		}
	}

	mustFail(t, 2, p.args("run", "--", p.file("config/team.json"), p.file("broken.json"))...)
	noConsumer(t, p.marker)

	stderr := mustFail(t, 2, p.args("config", "check")...)
	for id := range cases {
		if !strings.Contains(stderr, "schemas."+id+".path") {
			t.Errorf("config check does not report %s: %s", id, stderr)
		}
	}

	if strings.Contains(stderr, "schemas.company.path") {
		t.Errorf("config check reports the valid schema: %s", stderr)
	}

	mustRun(t, p.args("path", "company")...)
	noCache(t, p.cache)
}

// A local schema is used in place, so an edit is visible to the next
// command without any cache to invalidate.
func TestLocalSchemaEditsAreVisibleImmediately(t *testing.T) {
	p := newLocalProject(t, companySection)

	if got := mustRun(t, p.args("cat", "company")...); got != companySchema {
		t.Fatalf("cat = %q", got)
	}

	edited := `{"type":"string"}`
	writeTestFile(t, p.schema("company.schema.json"), edited)

	if got := mustRun(t, p.args("cat", "company")...); got != edited {
		t.Errorf("cat after an edit = %q, want %q", got, edited)
	}
}

func TestLocalSchemaOutsideWorkspaceRef(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "vendor.schema.json")
	writeTestFile(t, outside, commonSchema)

	p := newLocalProject(t, "[schemas.vendor]\npath = \"${VENDOR_SCHEMA}\"\n")
	t.Setenv("VENDOR_SCHEMA", outside)

	mustRun(t, p.args("run", "--schema", "vendor", "--", p.file("alpha.json"))...)

	if got := consumerArgs(t, p.marker); got[0] != outside || got[2] != "--ref=local:"+outside {
		t.Errorf("consumer arguments %q, want the absolute path as ref", got)
	}
}

func TestConfigCheckJSONListsLocalSchemas(t *testing.T) {
	p := newLocalProject(t, companySection)

	got := decodeInto[struct {
		Schemas map[string]struct {
			Path       string   `json:"path"`
			DeclaredIn string   `json:"declaredIn"`
			FileMatch  []string `json:"fileMatch"`
		} `json:"schemas"`
	}](t, mustRun(t, p.args("config", "check", "--json")...))

	company := got.Schemas["company"]
	if len(got.Schemas) != 1 || company.Path != p.schema("company.schema.json") || company.DeclaredIn != p.config || len(company.FileMatch) != 2 {
		t.Errorf("config check --json schemas = %+v", got.Schemas)
	}
}

// Without --cache-dir and without a home directory there is no default cache
// directory. A run on local schemas needs one only when the runner expands
// {cache}; everything else works without it.
func TestLocalSchemasWithoutAnyCacheDirectory(t *testing.T) {
	p := newLocalProject(t, companySection)
	noHome := map[string]string{"HOME": unset, "XDG_CACHE_HOME": unset, "LocalAppData": unset, "home": unset}
	team := p.file("config/team.json")

	_, stderr, code := runWith(t, noHome, "--config", p.config, "--workspace", p.ws, "run", "--", team)
	if code != 2 || !strings.Contains(stderr, "no cache directory available; pass --cache-dir") {
		t.Fatalf("a runner that expands {cache} without any cache directory: exit %d, %s", code, stderr)
	}

	noConsumer(t, p.marker)

	data, err := os.ReadFile(p.config)
	if err != nil {
		t.Fatal(err)
	}

	withoutCache := filepath.Join(filepath.Dir(p.config), "no-cache.toml")
	writeTestFile(t, withoutCache, strings.Replace(string(data), `"--cache={cache}", `, "", 1))

	for _, args := range [][]string{
		{"run", "--", team},
		{"run", "--schema", "company", "--", p.file("notes/readme.json")},
	} {
		stdout, stderr, code := runWith(t, noHome, append([]string{"--config", withoutCache, "--workspace", p.ws}, args...)...)
		if code != 0 {
			t.Fatalf("%s without any cache directory: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, stdout, stderr)
		}

		if got := consumerArgs(t, p.marker); len(got) != 4 || got[0] != p.schema("company.schema.json") || got[2] != "--ref=local:schemas/company.schema.json" {
			t.Errorf("consumer arguments %q", got)
		}
	}

	for _, args := range [][]string{{"path", "company"}, {"cat", "company"}, {"resolve", "--file", team}, {"list"}, {"patterns"}, {"config", "check"}} {
		if _, stderr, code := runWith(t, noHome, append([]string{"--config", p.config, "--workspace", p.ws}, args...)...); code != 0 {
			t.Errorf("%s without any cache directory: exit %d, %s", strings.Join(args, " "), code, stderr)
		}
	}
}
