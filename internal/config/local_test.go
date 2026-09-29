package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
)

func TestLocalSchemas(t *testing.T) {
	cfg, dir := mustLoadDoc(t, `config_version = 1

[schemas."company-config"]
path = "schemas/company-config.schema.json"
file_match = ["config/company.json", "!config/legacy/**"]

[schemas.a]
path = "../elsewhere/a.json"
`, nil)

	want := []LocalSchema{
		{ID: "a", Path: filepath.Join(filepath.Dir(dir), "elsewhere", "a.json"), DeclaredIn: filepath.Join(dir, "schepherd.toml")},
		{
			ID: "company-config", Path: filepath.Join(dir, "schemas", "company-config.schema.json"), DeclaredIn: filepath.Join(dir, "schepherd.toml"),
			FileMatch: []string{"config/company.json", "!config/legacy/**"},
		},
	}

	if got := withoutOrigins(cfg.LocalSchemas); !reflect.DeepEqual(got, want) {
		t.Fatalf("LocalSchemas\n got %#v\nwant %#v", got, want)
	}

	if ls, ok := cfg.LocalSchema("company-config"); !ok || ls.Path != want[1].Path {
		t.Errorf("LocalSchema(company-config) = %+v, %v", ls, ok)
	}

	if _, ok := cfg.LocalSchema("company"); ok {
		t.Error("LocalSchema found an undeclared id")
	}

	defaults, err := Load(LoadOptions{Cwd: dir})
	if err != nil || len(defaults.LocalSchemas) != 0 {
		t.Errorf("defaults have local schemas %v (%v)", defaults.LocalSchemas, err)
	}
}

func withoutOrigins(list []LocalSchema) []LocalSchema {
	out := make([]LocalSchema, 0, len(list))
	for _, ls := range list {
		ls.at = ""
		out = append(out, ls)
	}

	return out
}

// [schemas."<id>"] merges key by key like every other table: a later file
// can set path or file_match of an id alone, a relative path stays relative
// to the file that set it, and ids a later file does not mention are kept.
func TestLocalSchemasThroughExtends(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"shared/base.toml": `config_version = 1

[schemas.kept]
path = "schemas/kept.json"
file_match = ["kept.json"]

[schemas.moved]
path = "schemas/moved.json"
file_match = ["moved.json"]

[schemas.rerouted]
path = "schemas/rerouted.json"
file_match = ["old/*.json"]

[schemas.redefined]
path = "schemas/redefined.json"
file_match = ["redefined.json"]
`,
		"project/schepherd.toml": `config_version = 1
extends = ["../shared/base.toml"]

[schemas.moved]
path = "own/moved.json"

[schemas.rerouted]
file_match = ["new/*.json"]

[schemas.redefined]
path = "own/redefined.json"
file_match = ["own.json"]

[schemas.added]
path = "${ADDED}"
`,
	})

	abs := filepath.Join(t.TempDir(), "added.json")

	cfg, err := Load(LoadOptions{Path: filepath.Join(dir, "project", "schepherd.toml"), Cwd: t.TempDir(), Lookup: lookupFrom(map[string]string{"ADDED": abs})})
	if err != nil {
		t.Fatal(err)
	}

	base, project := filepath.Join(dir, "shared", "base.toml"), filepath.Join(dir, "project", "schepherd.toml")

	want := []LocalSchema{
		{ID: "added", Path: abs, DeclaredIn: project},
		{ID: "kept", Path: filepath.Join(dir, "shared", "schemas", "kept.json"), DeclaredIn: base, FileMatch: []string{"kept.json"}},
		{ID: "moved", Path: filepath.Join(dir, "project", "own", "moved.json"), DeclaredIn: project, FileMatch: []string{"moved.json"}},
		{ID: "redefined", Path: filepath.Join(dir, "project", "own", "redefined.json"), DeclaredIn: project, FileMatch: []string{"own.json"}},
		{ID: "rerouted", Path: filepath.Join(dir, "shared", "schemas", "rerouted.json"), DeclaredIn: base, FileMatch: []string{"new/*.json"}},
	}

	if got := withoutOrigins(cfg.LocalSchemas); !reflect.DeepEqual(got, want) {
		t.Errorf("LocalSchemas\n got %#v\nwant %#v", got, want)
	}

	if ls, _ := cfg.LocalSchema("rerouted"); ls == nil || !strings.HasPrefix(ls.context(), base+":12:1: schemas.rerouted.path") {
		t.Errorf("a path from a base must be reported at the base: %+v", ls)
	}
}

// path is required once the chain is merged; the error points at the last
// declaration of the id.
func TestLocalSchemaWithoutPathInTheWholeChain(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"base.toml": "config_version = 1\n\n[schemas.a]\nfile_match = [\"a.json\"]\n",
		"top.toml":  "config_version = 1\nextends = [\"base.toml\"]\n\n[schemas.a]\nfile_match = [\"b.json\"]\n",
		"only.toml": "config_version = 1\nextends = [\"base.toml\"]\n",
	})

	_, err := Load(LoadOptions{Path: filepath.Join(dir, "top.toml"), Cwd: dir})
	wantUsage(t, err, filepath.Join(dir, "top.toml")+":4:10: schemas.a.path: required key is missing")

	_, err = Load(LoadOptions{Path: filepath.Join(dir, "only.toml"), Cwd: dir})
	wantUsage(t, err, filepath.Join(dir, "base.toml")+":3:10: schemas.a.path: required key is missing")

	cfg, err := Load(LoadOptions{Path: filepath.Join(dir, "top.toml"), Cwd: dir, WithoutCatalog: true})
	if err != nil || len(cfg.LocalSchemas) != 0 {
		t.Errorf("WithoutCatalog must skip [schemas]: %v, %v", cfg, err)
	}
}

func TestLocalSchemaPathInterpolation(t *testing.T) {
	doc := "config_version = 1\n[schemas.a]\npath = \"${SCHEMAS}/a.json\"\n"

	cfg, dir := mustLoadDoc(t, doc, map[string]string{"SCHEMAS": "vendor"})
	if ls, _ := cfg.LocalSchema("a"); ls == nil || ls.Path != filepath.Join(dir, "vendor", "a.json") {
		t.Errorf("expanded path = %+v", ls)
	}

	_, _, err := loadDoc(t, doc, nil)
	wantUsage(t, err, "schemas.a.path", "${SCHEMAS}")

	_, _, err = loadDoc(t, "config_version = 1\n[schemas.a]\npath = \"${EMPTY}\"\n", map[string]string{"EMPTY": ""})
	wantUsage(t, err, "schemas.a.path: expands to an empty path")

	_, _, err = loadDoc(t, doc, map[string]string{"SCHEMAS": "https://schemas.example"})
	wantUsage(t, err, "schemas.a.path", "is not a local file")

	literal, _ := mustLoadDoc(t, "config_version = 1\n[schemas.a]\npath = \"$${SCHEMAS}.json\"\n", nil)
	if ls, _ := literal.LocalSchema("a"); ls == nil || filepath.Base(ls.Path) != "${SCHEMAS}.json" {
		t.Errorf("escaped reference = %+v", ls)
	}

	dir = writeTree(t, map[string]string{"s.toml": doc})

	cfg, err = Load(LoadOptions{Path: filepath.Join(dir, "s.toml"), Cwd: dir, WithoutCatalog: true})
	if err != nil || len(cfg.LocalSchemas) != 0 {
		t.Errorf("WithoutCatalog must skip [schemas]: %v, %v", cfg, err)
	}
}

func TestLocalSchemaDeclarationErrors(t *testing.T) {
	cases := []struct {
		doc      string
		fragment string
	}{
		{"[schemas.\"Company Config\"]\npath = \"a.json\"", `schemas."Company Config": schema id "Company Config" must match`},
		{"[schemas.\"a..b\"]\npath = \"a.json\"", `must not contain ".."`},
		{"[schemas.a]\nfile_match = [\"a.json\"]", "schemas.a.path: required key is missing"},
		{"[schemas.a]\npath = \"a.json\"\nfileMatch = [\"a.json\"]", "schemas.a.fileMatch: unknown key"},
		{"[schemas.a]\nPath = \"a.json\"", `schemas.a.Path: unknown key (keys are case-sensitive; did you mean "path"?)`},
		{"[schemas.a]\npath = \"a.json\"\nurl = \"https://x\"", "schemas.a.url: unknown key"},
		{"[schemas.a]\npath = 1", "schemas.a.path: expected a string, got an integer"},
		{"[schemas.a]\npath = \"\"", "schemas.a.path: must not be empty"},
		{"[schemas.a]\npath = \"https://schemas.example/a.json\"", `schemas.a.path: "https://schemas.example/a.json" is not a local file`},
		{"[schemas.a]\npath = \"{workspace}/a.json\"", "schemas.a.path: placeholder {workspace} is not allowed here"},
		{"[schemas.a]\npath = \"a.json\"\nfile_match = []", "schemas.a.file_match: must list at least one pattern"},
		{"[schemas.a]\npath = \"a.json\"\nfile_match = [\"config/[.json\"]", `schemas.a.file_match[0]: invalid pattern "config/[.json"`},
		{"[schemas.a]\npath = \"a.json\"\nfile_match = \"a.json\"", "schemas.a.file_match: expected an array, got a string"},
		{"schemas = [\"a\"]", "schemas: expected a table, got an array"},
		{"[schemas]\na = \"a.json\"", "schemas.a: expected a table, got a string"},
	}

	for _, tc := range cases {
		t.Run(tc.fragment, func(t *testing.T) {
			_, _, err := loadDoc(t, "config_version = 1\n"+tc.doc+"\n", nil)
			wantUsage(t, err, tc.fragment)
		})
	}

	_, _, err := loadDoc(t, "config_version = 1\n[schemas.\"a:b\"]\npath = \"./a:b.json\"\n", nil)
	wantUsage(t, err, `schema id "a:b" must match`)

	cfg, dir := mustLoadDoc(t, "config_version = 1\n[schemas.a]\npath = \"./a:b.json\"\n", nil)
	if ls, _ := cfg.LocalSchema("a"); ls == nil || ls.Path != filepath.Join(dir, "a:b.json") {
		t.Errorf("./ must make a file name with a colon local: %+v", ls)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLocalSchemaRead(t *testing.T) {
	cfg, dir := mustLoadDoc(t, `config_version = 1

[limits]
max_schema_bytes = 64

[schemas.ok]
path = "schemas/ok.json"

[schemas.missing]
path = "schemas/missing.json"

[schemas.dir]
path = "schemas"

[schemas.malformed]
path = "schemas/malformed.json"

[schemas.duplicate]
path = "schemas/duplicate.json"

[schemas.large]
path = "schemas/large.json"
`, nil)

	config := filepath.Join(dir, "schepherd.toml")
	schemas := filepath.Join(dir, "schemas")
	content := `{"$ref":"common.json#/$defs/x"}`

	writeFile(t, filepath.Join(schemas, "ok.json"), content)
	writeFile(t, filepath.Join(schemas, "malformed.json"), `{"type":`)
	writeFile(t, filepath.Join(schemas, "duplicate.json"), `{"type":"object","type":"array"}`)
	writeFile(t, filepath.Join(schemas, "large.json"), `{"description":"`+strings.Repeat("x", 64)+`"}`)

	ok, _ := cfg.LocalSchema("ok")

	data, err := ok.Read(cfg.Limits.MaxSchemaBytes)
	if err != nil || string(data) != content {
		t.Fatalf("Read = %q, %v", data, err)
	}

	cases := map[string]string{
		"missing":   "no such file",
		"dir":       "is a directory, not a JSON Schema file",
		"malformed": "is not valid JSON: malformed JSON",
		"duplicate": `is not valid JSON: duplicate object key "type"`,
		"large":     "is larger than limits.max_schema_bytes (64 bytes)",
	}

	if runtime.GOOS == "windows" {
		cases["missing"] = "missing.json"
	}

	for id, fragment := range cases {
		ls, _ := cfg.LocalSchema(id)

		_, err := ls.Read(cfg.Limits.MaxSchemaBytes)
		wantUsage(t, err, config+":", "schemas."+id+".path", fragment)

		if id != "missing" && !strings.Contains(err.Error(), ls.Path) {
			t.Errorf("%s: error %q does not name %s", id, err, ls.Path)
		}
	}

	err = cfg.CheckLocalSchemas()
	wantUsage(t, err, "schemas.missing.path", "schemas.dir.path", "schemas.malformed.path", "schemas.duplicate.path", "schemas.large.path")

	if strings.Contains(err.Error(), "schemas.ok.path") {
		t.Errorf("CheckLocalSchemas reports the valid schema: %v", err)
	}

	if got := strings.Count(err.Error(), "\n") + 1; got != len(cases) {
		t.Errorf("CheckLocalSchemas reported %d problems, want %d: %v", got, len(cases), err)
	}
}

func TestLocalSchemaReadFollowsSymlinks(t *testing.T) {
	cfg, dir := mustLoadDoc(t, "config_version = 1\n[schemas.link]\npath = \"link.json\"\n[schemas.dangling]\npath = \"dangling.json\"\n", nil)
	target := filepath.Join(dir, "real", "schema.json")
	writeFile(t, target, `{"type":"object"}`)

	for link, dest := range map[string]string{"link.json": target, "dangling.json": filepath.Join(dir, "gone.json")} {
		if err := os.Symlink(dest, filepath.Join(dir, link)); err != nil {
			t.Skipf("symlinks are not available: %v", err)
		}
	}

	link, _ := cfg.LocalSchema("link")
	if data, err := link.Read(cfg.Limits.MaxSchemaBytes); err != nil || string(data) != `{"type":"object"}` {
		t.Errorf("Read through a symlink = %q, %v", data, err)
	}

	if link.Path != filepath.Join(dir, "link.json") {
		t.Errorf("Path = %s, want the symlink itself so relative $refs resolve next to it", link.Path)
	}

	dangling, _ := cfg.LocalSchema("dangling")
	_, err := dangling.Read(cfg.Limits.MaxSchemaBytes)
	wantUsage(t, err, "schemas.dangling.path", "dangling.json")
}

func TestCheckLocalSchemasWithoutSchemas(t *testing.T) {
	cfg, _ := mustLoadDoc(t, "config_version = 1\n", nil)
	if err := cfg.CheckLocalSchemas(); err != nil {
		t.Errorf("CheckLocalSchemas = %v", err)
	}
}

func TestLocalSchemaSummary(t *testing.T) {
	cfg, dir := mustLoadDoc(t, "config_version = 1\n[schemas.b]\npath = \"b.json\"\n[schemas.a]\npath = \"a.json\"\nfile_match = [\"*.a.json\"]\n", nil)

	data, err := json.Marshal(cfg.Summary())
	if err != nil {
		t.Fatal(err)
	}

	var got struct {
		Schemas map[string]map[string]any `json:"schemas"`
	}

	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(dir, "schepherd.toml")
	want := map[string]map[string]any{
		"a": {"path": filepath.Join(dir, "a.json"), "declaredIn": file, "fileMatch": []any{"*.a.json"}},
		"b": {"path": filepath.Join(dir, "b.json"), "declaredIn": file, "fileMatch": []any{}},
	}

	if !reflect.DeepEqual(got.Schemas, want) {
		t.Errorf("summary schemas\n got %#v\nwant %#v", got.Schemas, want)
	}

	defaults, err := Load(LoadOptions{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	data, _ = json.Marshal(defaults.Summary())
	if !strings.Contains(string(data), `"schemas":{}`) {
		t.Errorf("default summary %s lacks an empty schemas object", data)
	}
}

func TestLocalSchemaReadErrorsAreUsage(t *testing.T) {
	ls := &LocalSchema{ID: "x", Path: filepath.Join(t.TempDir(), "x.json"), DeclaredIn: "/conf/schepherd.toml"}

	_, err := ls.Read(1 << 20)
	if kind := fault.KindOf(err); kind != fault.Usage || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Read of a missing file: kind %v, %v", kind, err)
	}

	if !strings.Contains(err.Error(), "/conf/schepherd.toml: schemas.x.path: ") {
		t.Errorf("error %q does not name the declaring file and the key", err)
	}
}
