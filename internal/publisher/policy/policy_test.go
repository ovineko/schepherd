package policy

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
)

const samplePolicy = `
[[rules]]
id = "store"
decision = "allow"
hosts = ["www.store.example", "json.store.example"]
license = "Apache-2.0"
notice_file = "notices/store.txt"
reason = "store repository license"

[[rules]]
id = "store-raw"
decision = "allow"
hosts = ["raw.example"]
path_prefix = "/Store/store/"
license = "Apache-2.0"
notice_file = "notices/store.txt"
reason = "store repository license via raw"

[[rules]]
id = "mit-lib"
decision = "allow"
hosts = ["mit.example"]
path_prefix = "/lib"
license = "MIT"
notice = "Copyright (c) Example Authors"
reason = "MIT license file checked"

[[rules]]
id = "dual"
decision = "allow"
urls = ["https://dual.example/schema.json", "https://dual.example/schema.json?variant=a"]
license = "MIT OR Apache-2.0"
reason = "dual licensed"

[[rules]]
id = "copied-files"
decision = "review"
hosts = ["www.store.example", "json.store.example"]
file_names = ["copied.json", "Other", "reviewed.json"]
reason = "files copied from another project"

[[rules]]
id = "copied-files-raw"
decision = "exclude"
hosts = ["raw.example"]
path_prefix = "/Store/store/"
file_names = ["copied.json"]
reason = "copied file, raw spelling"

[[rules]]
id = "reviewed-copy"
decision = "allow"
urls = ["https://www.store.example/reviewed.json"]
license = "MIT AND Apache-2.0"
reason = "copied parts reviewed"

[[rules]]
id = "third-party-file"
decision = "review"
urls = ["https://www.store.example/partial.json"]
reason = "file states it was copied from another project"

[[rules]]
id = "banned"
decision = "exclude"
hosts = ["mit.example"]
path_prefix = "/lib/private"
reason = "not redistributable"

[[rules]]
id = "held"
decision = "review"
hosts = ["held.example"]
reason = "owner asked for time"
`

func writePolicy(t *testing.T, content string, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()

	for name, data := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	path := filepath.Join(dir, "licenses.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func loadSample(t *testing.T) *Policy {
	t.Helper()

	p, err := Load(writePolicy(t, samplePolicy, map[string]string{"notices/store.txt": "Store notice\n\n"}))
	if err != nil {
		t.Fatal(err)
	}

	return p
}

func TestDecide(t *testing.T) {
	p := loadSample(t)

	tests := []struct {
		want   Decision
		name   string
		source string
		deps   []string
	}{
		{
			name:   "allowed root",
			source: "https://www.store.example/a.json",
			want: Decision{
				Decision: Allow, License: "Apache-2.0", Notice: "Store notice",
				Reason: "store repository license (rule store)", RuleID: "store",
			},
		},
		{
			name:   "host is case-insensitive and default port ignored",
			source: "https://JSON.Store.Example:443/a.json#/definitions/x",
			want: Decision{
				Decision: Allow, License: "Apache-2.0", Notice: "Store notice",
				Reason: "store repository license (rule store)", RuleID: "store",
			},
		},
		{
			name:   "unknown source",
			source: "https://unknown.example/a.json",
			want:   Decision{Decision: Review, Reason: NoRuleReason},
		},
		{
			name:   "host rule never matches a query",
			source: "https://www.store.example/a.json?v=1",
			want:   Decision{Decision: Review, Reason: NoRuleReason},
		},
		{
			name:   "empty query is still a query",
			source: "https://www.store.example/a.json?",
			want:   Decision{Decision: Review, Reason: NoRuleReason},
		},
		{
			name:   "exact url with its query",
			source: "https://dual.example/schema.json?variant=a",
			want:   Decision{Decision: Allow, License: "MIT OR Apache-2.0", Reason: "dual licensed (rule dual)", RuleID: "dual"},
		},
		{
			name:   "exact url with another query",
			source: "https://dual.example/schema.json?variant=b",
			want:   Decision{Decision: Review, Reason: NoRuleReason},
		},
		{
			name:   "http spelling of an exact url",
			source: "http://www.store.example/partial.json",
			want: Decision{
				Decision: Review, RuleID: "third-party-file",
				Reason: "file states it was copied from another project (rule third-party-file)",
			},
		},
		{
			name:   "percent-encoded spelling of an exact url",
			source: "https://WWW.store.example/p%61rtial.json",
			want: Decision{
				Decision: Review, RuleID: "third-party-file",
				Reason: "file states it was copied from another project (rule third-party-file)",
			},
		},
		{
			name:   "file name on another host of the rule",
			source: "https://json.store.example/copied.json",
			want:   Decision{Decision: Review, RuleID: "copied-files", Reason: "files copied from another project (rule copied-files)"},
		},
		{
			name:   "file name without extension",
			source: "https://www.store.example/copied",
			want:   Decision{Decision: Review, RuleID: "copied-files", Reason: "files copied from another project (rule copied-files)"},
		},
		{
			name:   "file name in another directory and case",
			source: "https://www.store.example/schemas/json/COPIED.JSON",
			want:   Decision{Decision: Review, RuleID: "copied-files", Reason: "files copied from another project (rule copied-files)"},
		},
		{
			name:   "file name over http with a query",
			source: "http://www.store.example/copied.json?v=1#x",
			want:   Decision{Decision: Review, RuleID: "copied-files", Reason: "files copied from another project (rule copied-files)"},
		},
		{
			name:   "file names are case-insensitive",
			source: "https://www.store.example/other.json",
			want:   Decision{Decision: Review, RuleID: "copied-files", Reason: "files copied from another project (rule copied-files)"},
		},
		{
			name:   "file name rule beats a longer host prefix",
			source: "https://raw.example/Store/store/0123abc/src/schemas/json/copied.json",
			want:   Decision{Decision: Exclude, RuleID: "copied-files-raw", Reason: "copied file, raw spelling (rule copied-files-raw)"},
		},
		{
			name:   "file name rule respects its path prefix",
			source: "https://raw.example/Other/repo/copied.json",
			want:   Decision{Decision: Review, Reason: NoRuleReason},
		},
		{
			name:   "similar file names are not matched",
			source: "https://www.store.example/copied.json.bak",
			want: Decision{
				Decision: Allow, License: "Apache-2.0", Notice: "Store notice",
				Reason: "store repository license (rule store)", RuleID: "store",
			},
		},
		{
			name:   "exact allow beats a file name rule",
			source: "https://www.store.example/reviewed.json",
			want: Decision{
				Decision: Allow, License: "MIT AND Apache-2.0", Reason: "copied parts reviewed (rule reviewed-copy)",
				RuleID: "reviewed-copy",
			},
		},
		{
			name:   "aliases of an exactly allowed file stay held",
			source: "https://json.store.example/reviewed.json",
			want:   Decision{Decision: Review, RuleID: "copied-files", Reason: "files copied from another project (rule copied-files)"},
		},
		{
			name:   "dependency alias of a held file",
			source: "https://www.store.example/a.json",
			deps:   []string{"https://json.store.example/copied.json"},
			want: Decision{
				Decision: Review, RuleID: "copied-files",
				Reason: "dependency https://json.store.example/copied.json: files copied from another project (rule copied-files)",
			},
		},
		{
			name:   "non-default port never matches a host rule",
			source: "https://www.store.example:8443/a.json",
			want:   Decision{Decision: Review, Reason: NoRuleReason},
		},
		{
			name:   "exact url beats host rule",
			source: "https://www.store.example/partial.json",
			want: Decision{
				Decision: Review, RuleID: "third-party-file",
				Reason: "file states it was copied from another project (rule third-party-file)",
			},
		},
		{
			name:   "path prefix at segment boundary",
			source: "https://mit.example/library/x.json",
			want:   Decision{Decision: Review, Reason: NoRuleReason},
		},
		{
			name:   "longest prefix wins",
			source: "https://mit.example/lib/private/x.json",
			want:   Decision{Decision: Exclude, RuleID: "banned", Reason: "not redistributable (rule banned)"},
		},
		{
			name:   "raw prefix",
			source: "https://raw.example/Store/store/master/src/x.json",
			want: Decision{
				Decision: Allow, License: "Apache-2.0", Notice: "Store notice",
				Reason: "store repository license via raw (rule store-raw)", RuleID: "store-raw",
			},
		},
		{
			name:   "raw prefix is case-sensitive",
			source: "https://raw.example/store/store/master/src/x.json",
			want:   Decision{Decision: Review, Reason: NoRuleReason},
		},
		{
			name:   "dot segments escaping a prefix are refused",
			source: "https://raw.example/Store/store/../../evil/repo/x.json",
			want: Decision{
				Decision: Review,
				Reason:   "unsupported URL: path must not contain dot segments or encoded separators",
			},
		},
		{
			name:   "encoded slash is refused",
			source: "https://raw.example/Store%2Fstore/x.json",
			want: Decision{
				Decision: Review,
				Reason:   "unsupported URL: path must not contain dot segments or encoded separators",
			},
		},
		{
			name:   "combined licenses and notices",
			source: "https://www.store.example/a.json",
			deps: []string{
				"https://mit.example/lib/b.json",
				"https://dual.example/schema.json",
				"https://raw.example/Store/store/master/src/c.json",
				"https://mit.example/lib/b.json#/definitions/y",
				"http://json-schema.org/draft-07/schema#",
				"https://www.store.example/a.json",
			},
			want: Decision{
				Decision: Allow,
				License:  "Apache-2.0 AND MIT AND (MIT OR Apache-2.0)",
				Notice:   "Store notice\n\nCopyright (c) Example Authors",
				Reason: "store repository license (rule store); dual licensed (rule dual); " +
					"MIT license file checked (rule mit-lib); store repository license via raw (rule store-raw)",
				RuleID: "store",
			},
		},
		{
			name:   "unknown dependency holds the schema",
			source: "https://www.store.example/a.json",
			deps:   []string{"https://unknown.example/dep.json", "https://mit.example/lib/b.json"},
			want: Decision{
				Decision: Review,
				Reason:   "dependency https://unknown.example/dep.json: " + NoRuleReason,
			},
		},
		{
			name:   "excluded dependency wins over review",
			source: "https://unknown.example/root.json",
			deps:   []string{"https://mit.example/lib/private/d.json", "https://held.example/x.json"},
			want: Decision{
				Decision: Exclude, RuleID: "banned",
				Reason: "dependency https://mit.example/lib/private/d.json: not redistributable (rule banned)",
			},
		},
		{
			name:   "all review reasons are listed",
			source: "https://held.example/root.json",
			deps:   []string{"https://unknown.example/dep.json"},
			want: Decision{
				Decision: Review, RuleID: "held",
				Reason: "owner asked for time (rule held); dependency https://unknown.example/dep.json: " + NoRuleReason,
			},
		},
		{
			name:   "invalid dependency URL",
			source: "https://www.store.example/a.json",
			deps:   []string{"ftp://files.example/x.json"},
			want: Decision{
				Decision: Review,
				Reason:   "dependency ftp://files.example/x.json: unsupported URL: scheme must be http or https",
			},
		},
		{
			name:   "credentials",
			source: "https://user:pw@www.store.example/a.json",
			want:   Decision{Decision: Review, Reason: "unsupported URL: must not contain credentials"},
		},
		{
			name:   "empty source",
			source: "",
			want:   Decision{Decision: Review, Reason: "unsupported URL: must be a URL of 1-4096 bytes"},
		},
		{
			name:   "metaschema as root is not skipped",
			source: "https://json-schema.org/draft-07/schema",
			want:   Decision{Decision: Review, Reason: NoRuleReason},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.Decide(tt.source, tt.deps)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Decide =\n%#v\nwant\n%#v", got, tt.want)
			}
		})
	}
}

func TestDecideIsOrderIndependent(t *testing.T) {
	p := loadSample(t)

	deps := []string{"https://mit.example/lib/b.json", "https://dual.example/schema.json", "https://unknown.example/z"}
	reversed := []string{deps[2], deps[1], deps[0]}

	if a, b := p.Decide("https://www.store.example/a.json", deps), p.Decide("https://www.store.example/a.json", reversed); !reflect.DeepEqual(a, b) {
		t.Fatalf("order changed decision:\n%#v\n%#v", a, b)
	}
}

func TestLoadErrors(t *testing.T) {
	base := func(extra string) string {
		return "[[rules]]\nid = \"r\"\ndecision = \"allow\"\nlicense = \"MIT\"\nreason = \"ok\"\n" + extra
	}

	review := func(extra string) string {
		return "[[rules]]\nid = \"r\"\ndecision = \"review\"\nreason = \"held\"\n" + extra
	}

	allowing := func(license string) string {
		return "[[rules]]\nid = \"r\"\ndecision = \"allow\"\nlicense = \"" + license + "\"\nreason = \"x\"\nhosts = [\"a.example\"]\n"
	}

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "no matcher", content: base(""), want: "either urls or hosts is required"},
		{name: "json.schemastore.org url", content: review("urls = [\"https://json.schemastore.org/x.json\"]\n"), want: "another spelling of a SchemaStore URL"},
		{name: "schemas/json url", content: review("urls = [\"https://www.schemastore.org/schemas/json/x.json\"]\n"), want: "another spelling of a SchemaStore URL"},
		{name: "extensionless schemastore url", content: review("urls = [\"https://www.schemastore.org/x\"]\n"), want: "another spelling of a SchemaStore URL"},
		{name: "raw schemastore url", content: review("urls = [\"https://raw.githubusercontent.com/SchemaStore/SCHEMASTORE/master/src/schemas/json/x.json\"]\n"), want: "another spelling of a SchemaStore URL"},
		{name: "github schemastore url", content: review("urls = [\"https://github.com/schemastore/schemastore/raw/master/src/schemas/json/x.json\"]\n"), want: "another spelling of a SchemaStore URL"},
		{name: "urls and hosts", content: base("urls = [\"https://a.example/x\"]\nhosts = [\"a.example\"]\n"), want: "cannot be combined"},
		{name: "file names on allow", content: base("hosts = [\"a.example\"]\nfile_names = [\"x.json\"]\n"), want: "only allowed for exclude and review"},
		{name: "file names and urls", content: review("urls = [\"https://a.example/x\"]\nfile_names = [\"x.json\"]\n"), want: "cannot be combined"},
		{name: "file names without hosts", content: review("file_names = [\"x.json\"]\n"), want: "either urls or hosts is required"},
		{name: "file name with slash", content: review("hosts = [\"a.example\"]\nfile_names = [\"a/x.json\"]\n"), want: "plain, unescaped file name"},
		{name: "escaped file name", content: review("hosts = [\"a.example\"]\nfile_names = [\"x%2Ejson\"]\n"), want: "plain, unescaped file name"},
		{name: "dot file name", content: review("hosts = [\"a.example\"]\nfile_names = [\"..\"]\n"), want: "plain, unescaped file name"},
		{name: "extension only", content: review("hosts = [\"a.example\"]\nfile_names = [\".JSON\"]\n"), want: "no name before its extension"},
		{name: "file name twice", content: review("hosts = [\"a.example\"]\nfile_names = [\"X.json\", \"x\"]\n"), want: "listed twice"},
		{
			name: "overlapping file names",
			content: review("hosts = [\"a.example\"]\nfile_names = [\"x.json\", \"y.json\"]\n") +
				"[[rules]]\nid = \"s\"\ndecision = \"exclude\"\nreason = \"x\"\nhosts = [\"a.example\"]\nfile_names = [\"y\"]\n",
			want: "already covered by rule \"r\"",
		},
		{name: "missing license", content: "[[rules]]\nid = \"r\"\ndecision = \"allow\"\nreason = \"x\"\nhosts = [\"a.example\"]\n", want: "license is required"},
		{name: "bad license", content: "[[rules]]\nid = \"r\"\ndecision = \"allow\"\nlicense = \"MIT;x\"\nreason = \"x\"\nhosts = [\"a.example\"]\n", want: "SPDX-like"},
		{name: "allow NOASSERTION", content: allowing("NOASSERTION"), want: "asserts no license"},
		{name: "allow NONE", content: allowing("NONE"), want: "asserts no license"},
		{name: "allow with a non-assertion", content: allowing("MIT AND noassertion"), want: "asserts no license"},
		{name: "allow a file pointer", content: allowing("SEE LICENSE IN LICENSE.md"), want: "not an SPDX license expression"},
		{name: "allow words", content: allowing("Apache 2.0"), want: "not an SPDX license expression"},
		{name: "missing reason", content: "[[rules]]\nid = \"r\"\ndecision = \"exclude\"\nhosts = [\"a.example\"]\n", want: "reason is required"},
		{name: "bad decision", content: "[[rules]]\nid = \"r\"\ndecision = \"maybe\"\nreason = \"x\"\nhosts = [\"a.example\"]\n", want: "must be allow, exclude or review"},
		{name: "bad id", content: "[[rules]]\nid = \"Bad\"\ndecision = \"review\"\nreason = \"x\"\nhosts = [\"a.example\"]\n", want: "id must match"},
		{name: "uppercase host", content: base("hosts = [\"A.example\"]\n"), want: "lowercase host name"},
		{name: "host with port", content: base("hosts = [\"a.example:443\"]\n"), want: "lowercase host name"},
		{name: "duplicate host", content: base("hosts = [\"a.example\", \"a.example\"]\n"), want: "listed twice"},
		{name: "bad prefix", content: base("hosts = [\"a.example\"]\npath_prefix = \"x/\"\n"), want: "path_prefix"},
		{name: "dot prefix", content: base("hosts = [\"a.example\"]\npath_prefix = \"/a/../b\"\n"), want: "path_prefix"},
		{name: "url with fragment", content: base("urls = [\"https://a.example/x#\"]\n"), want: "fragment"},
		{name: "relative url", content: base("urls = [\"/x\"]\n"), want: "url \"/x\""},
		{name: "both notices", content: base("hosts = [\"a.example\"]\nnotice = \"n\"\nnotice_file = \"n.txt\"\n"), want: "mutually exclusive"},
		{name: "notice escapes", content: base("hosts = [\"a.example\"]\nnotice_file = \"../n.txt\"\n"), want: "inside the policy directory"},
		{name: "missing notice", content: base("hosts = [\"a.example\"]\nnotice_file = \"none.txt\"\n"), want: "none.txt"},
		{name: "control characters", content: base("hosts = [\"a.example\"]\nnotice = \"a\\u0007b\"\n"), want: "control characters"},
		{name: "unknown key", content: base("hosts = [\"a.example\"]\nlicence = \"MIT\"\n"), want: "unknown key"},
		{name: "wrong case key", content: base("Hosts = [\"a.example\"]\n"), want: "unknown key"},
		{
			name:    "duplicate id",
			content: base("hosts = [\"a.example\"]\n") + base("hosts = [\"b.example\"]\n"),
			want:    "duplicate rule id",
		},
		{
			name: "duplicate host prefix",
			content: base("hosts = [\"a.example\"]\n") +
				"[[rules]]\nid = \"s\"\ndecision = \"review\"\nreason = \"x\"\nhosts = [\"a.example\"]\n",
			want: "already covered by rule \"r\"",
		},
		{
			name: "duplicate url",
			content: base("urls = [\"https://a.example/x\"]\n") +
				"[[rules]]\nid = \"s\"\ndecision = \"review\"\nreason = \"x\"\nurls = [\"https://A.example:443/x\"]\n",
			want: "already covered by rule \"r\"",
		},
		{
			name: "duplicate url in another spelling",
			content: base("urls = [\"https://a.example/x.json\"]\n") +
				"[[rules]]\nid = \"s\"\ndecision = \"review\"\nreason = \"x\"\nurls = [\"http://a.example/%78.json\"]\n",
			want: "already covered by rule \"r\"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writePolicy(t, tt.content, nil))
			if err == nil || fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestFileNameRulesBesideHostRules(t *testing.T) {
	content := "[[rules]]\nid = \"host\"\ndecision = \"allow\"\nlicense = \"MIT\"\nreason = \"host\"\nhosts = [\"a.example\"]\n" +
		"[[rules]]\nid = \"x\"\ndecision = \"review\"\nreason = \"x\"\nhosts = [\"a.example\"]\nfile_names = [\"x.json\"]\n" +
		"[[rules]]\nid = \"y\"\ndecision = \"exclude\"\nreason = \"y\"\nhosts = [\"a.example\"]\nfile_names = [\"y.json\"]\n"

	p, err := Load(writePolicy(t, content, nil))
	if err != nil {
		t.Fatal(err)
	}

	for source, want := range map[string]string{
		"https://a.example/dir/x":      "x",
		"https://a.example/y.json?a=b": "y",
		"https://a.example/z.json":     "host",
	} {
		if d := p.Decide(source, nil); d.RuleID != want {
			t.Errorf("Decide(%s) = %#v, want rule %s", source, d, want)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err == nil || fault.KindOf(err) != fault.Usage {
		t.Fatalf("error = %v", err)
	}
}

func TestNoticeFileSymlinkEscape(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	path := writePolicy(t,
		"[[rules]]\nid = \"r\"\ndecision = \"allow\"\nlicense = \"MIT\"\nreason = \"x\"\nhosts = [\"a.example\"]\nnotice_file = \"link.txt\"\n",
		nil)

	if err := os.Symlink(outside, filepath.Join(filepath.Dir(path), "link.txt")); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "link.txt") {
		t.Fatalf("error = %v", err)
	}
}

func TestCheckLicense(t *testing.T) {
	valid := []string{"MIT", "Apache-2.0", "MIT OR Apache-2.0", "(MIT OR Apache-2.0) AND BSD-3-Clause", "LicenseRef-Proprietary", "GPL-2.0+", "Apache-2.0 WITH LLVM-exception"}
	invalid := []string{"", " MIT", "MIT ", "MIT  OR Apache-2.0", "MIT;rm", "MIT\nX", strings.Repeat("A", 257)}

	for _, expr := range valid {
		if err := CheckLicense(expr); err != nil {
			t.Errorf("CheckLicense(%q) = %v", expr, err)
		}
	}

	for _, expr := range invalid {
		if err := CheckLicense(expr); err == nil {
			t.Errorf("CheckLicense(%q) accepted", expr)
		}
	}
}

func TestCheckAssertedLicense(t *testing.T) {
	valid := []string{
		"MIT", "Apache-2.0", "Unlicense", "GPL-2.0+", "LicenseRef-NOASSERTION", "Apache-2.0 WITH LLVM-exception",
		"(MIT OR Apache-2.0) AND BSD-3-Clause", "Apache-2.0 AND MIT AND (MIT OR Apache-2.0)", "((MIT))",
	}

	for _, expr := range valid {
		if err := CheckAssertedLicense(expr); err != nil {
			t.Errorf("CheckAssertedLicense(%q) = %v", expr, err)
		}
	}

	invalid := map[string]string{
		"NOASSERTION": "asserts no license", "noassertion": "asserts no license", "NONE": "asserts no license",
		"MIT AND NONE": "asserts no license", "(NOASSERTION)": "asserts no license", "UNLICENSED": "asserts no license",
		"UNKNOWN": "asserts no license", "Other": "asserts no license", "MIT WITH NONE": "asserts no license",
		"SEE LICENSE IN LICENSE.md": "not an SPDX", "MIT Apache-2.0": "not an SPDX", "MIT OR": "not an SPDX",
		"(MIT": "not an SPDX", "MIT)": "not an SPDX", "AND": "not an SPDX", "MIT WITH": "not an SPDX",
		"mit or apache-2.0": "not an SPDX", "()": "not an SPDX", "": "license is required", "MIT;x": "SPDX-like",
	}

	for expr, want := range invalid {
		if err := CheckAssertedLicense(expr); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CheckAssertedLicense(%q) = %v, want an error containing %q", expr, err, want)
		}
	}
}

func TestNonAssertedLicenseOnHoldingRules(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "licenses.toml")

	content := "[[rules]]\nid = \"held\"\ndecision = \"review\"\nlicense = \"NOASSERTION\"\nreason = \"upstream declares no license\"\nhosts = [\"a.example\"]\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := Load(path)
	if err != nil {
		t.Fatalf("a review rule may record NOASSERTION: %v", err)
	}

	if d := p.Decide("https://a.example/x.json", nil); d.Decision != Review || d.License != "" {
		t.Errorf("decision = %#v", d)
	}
}

func TestIsWellKnownMetaschema(t *testing.T) {
	valid := []string{
		"http://json-schema.org/draft-04/schema",
		"http://json-schema.org/draft-04/schema#",
		"https://json-schema.org/draft-06/schema#",
		"http://json-schema.org/draft-07/schema#",
		"https://json-schema.org/draft/2019-09/schema",
		"https://json-schema.org/draft/2020-12/schema",
		"https://JSON-Schema.org/draft/2020-12/schema#",
	}

	invalid := []string{
		"http://json-schema.org/draft-03/schema#",
		"http://json-schema.org/draft-07/schema#/definitions/x",
		"http://json-schema.org/draft-07/schema?x",
		"http://json-schema.org/draft-07/schema/",
		"http://json-schema.org:80/draft-07/schema",
		"http://user@json-schema.org/draft-07/schema",
		"ftp://json-schema.org/draft-07/schema",
		"https://json-schema.org.evil.example/draft-07/schema",
		"https://json-schema.org/draft/2020-12/meta/core",
		"https://json-schema.org/draft/2020-12/schema#/$defs",
		"",
		"%",
	}

	for _, uri := range valid {
		if !IsWellKnownMetaschema(uri) {
			t.Errorf("IsWellKnownMetaschema(%q) = false", uri)
		}
	}

	for _, uri := range invalid {
		if IsWellKnownMetaschema(uri) {
			t.Errorf("IsWellKnownMetaschema(%q) = true", uri)
		}
	}
}

// heldByFileName returns the file_names rule that matches raw, unless an
// exact rule takes precedence.
func heldByFileName(p *Policy, raw string) string {
	key, err := normalize(raw)
	if err != nil || !key.portless {
		return ""
	}

	if _, ok := p.exact[key.exact]; ok {
		return ""
	}

	for _, r := range p.hosts[key.host] {
		if len(r.fileStems) > 0 && prefixMatches(key.path, r.pathPrefix) && slices.Contains(r.fileStems, key.stem) {
			return r.id
		}
	}

	return ""
}

func FuzzDecide(f *testing.F) {
	f.Add("https://www.store.example/a.json", "https://mit.example/lib/b.json")
	f.Add("https://www.store.example/a.json", "http://JSON.store.example/x/Copied.JSON?q")
	f.Add("https://raw.example/Store/store/x/copied", "")
	f.Add("https://raw.example/Store/store/%2e%2e/x", "http://json-schema.org/draft-07/schema#")
	f.Add("https://mit.example/lib/../private/x", "")

	dir := f.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "notices"), 0o750); err != nil {
		f.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "notices", "store.txt"), []byte("n"), 0o600); err != nil {
		f.Fatal(err)
	}

	path := filepath.Join(dir, "licenses.toml")
	if err := os.WriteFile(path, []byte(samplePolicy), 0o600); err != nil {
		f.Fatal(err)
	}

	p, err := Load(path)
	if err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, source, dep string) {
		d := p.Decide(source, []string{dep})

		switch d.Decision {
		case Allow:
			if d.License == "" || d.RuleID == "" {
				t.Fatalf("allow without license or rule: %#v", d)
			}

			key, err := normalize(source)
			if err != nil || hasDotSegment(key.path) {
				t.Fatalf("allowed unsupported URL %q", source)
			}

			if _, exact := p.exact[key.exact]; key.query && !exact {
				t.Fatalf("allowed %q with a query through a host rule", source)
			}

			for _, raw := range []string{source, dep} {
				if held := heldByFileName(p, raw); held != "" {
					t.Fatalf("allowed %q although file_names rule %s holds %q", source, held, raw)
				}
			}
		case Review, Exclude:
			if d.Reason == "" || d.License != "" || d.Notice != "" {
				t.Fatalf("unexpected decision %#v", d)
			}
		default:
			t.Fatalf("unknown verdict %q", d.Decision)
		}
	})
}

func TestGitHubRepositoriesMatchInAnyCase(t *testing.T) {
	p, err := Load(writePolicy(t, `
[[rules]]
id = "repo"
decision = "allow"
hosts = ["raw.githubusercontent.com", "github.com"]
path_prefix = "/Owner/Repo/"
license = "MIT"
reason = "reviewed"

[[rules]]
id = "held"
decision = "review"
hosts = ["raw.githubusercontent.com", "github.com"]
path_prefix = "/Owner/Repo/"
file_names = ["third-party.json"]
reason = "copied content"

[[rules]]
id = "exact"
decision = "exclude"
urls = ["https://raw.githubusercontent.com/Other/Lib/main/Schema.json"]
reason = "banned"

[[rules]]
id = "elsewhere"
decision = "allow"
hosts = ["example.com"]
path_prefix = "/Owner/Repo/"
license = "MIT"
reason = "reviewed"
`, nil))
	if err != nil {
		t.Fatal(err)
	}

	for url, want := range map[string]string{
		"https://raw.githubusercontent.com/owner/repo/main/a.json":                 "repo",
		"https://raw.githubusercontent.com/OWNER/rEpO/main/a.json":                 "repo",
		"https://github.com/owner/REPO/raw/main/a.json":                            "repo",
		"https://raw.githubusercontent.com/owner/repo/main/third-party.json":       "held",
		"https://github.com/oWnEr/repo/blob/refs/heads/main/x/Third-Party":         "held",
		"https://raw.githubusercontent.com/other/lib/main/Schema.json":             "exact",
		"https://raw.githubusercontent.com/other/lib/main/schema.json":             "",
		"https://raw.githubusercontent.com/owner/repository/main/a.json":           "",
		"https://example.com/Owner/Repo/a.json":                                    "elsewhere",
		"https://example.com/owner/repo/a.json":                                    "",
		"https://raw.githubusercontent.com/Owner/Repo/main/sub/Owner/Repo/a.json":  "repo",
		"https://raw.githubusercontent.com/owner/repo/main/sub/third-party.json#x": "held",
	} {
		if got := p.Decide(url, nil).RuleID; got != want {
			t.Errorf("Decide(%s).RuleID = %q, want %q", url, got, want)
		}
	}

	if _, err := Load(writePolicy(t, `
[[rules]]
id = "one"
decision = "allow"
hosts = ["github.com"]
path_prefix = "/Owner/Repo/"
license = "MIT"
reason = "reviewed"

[[rules]]
id = "two"
decision = "review"
hosts = ["github.com"]
path_prefix = "/owner/repo/"
reason = "the same prefix in another case"
`, nil)); err == nil || !strings.Contains(err.Error(), "already covered") {
		t.Errorf("rules whose prefixes differ only in the case of the repository: %v", err)
	}
}

func TestDecideWithDeclarations(t *testing.T) {
	p := loadSample(t)

	declarations := map[string]string{
		"https://mit.example/lib/a.json":         "BSD-3-Clause",
		"https://mit.example/lib/private/x.json": "MIT",
		"https://held.example/x.json":            "MIT",
		"https://other.example/root.json":        "MIT",
		"https://other.example/isc.json":         "ISC",
		"https://other.example/also-mit.json":    "MIT",
		"https://other.example/none.json":        "NOASSERTION",
		"urn:example:x":                          "MIT",
	}
	declared := func(url string) (string, bool) {
		license, ok := declarations[url]

		return license, ok
	}

	tests := []struct {
		want   Decision
		name   string
		source string
		deps   []string
	}{
		{
			name: "over an allow rule", source: "https://mit.example/lib/a.json",
			want: Decision{Decision: Allow, License: "BSD-3-Clause", Reason: DeclaredReason},
		},
		{
			name: "over no rule", source: "https://other.example/root.json",
			want: Decision{Decision: Allow, License: "MIT", Reason: DeclaredReason},
		},
		{
			name: "over an unsupported URL", source: "urn:example:x",
			want: Decision{Decision: Allow, License: "MIT", Reason: DeclaredReason},
		},
		{
			name: "under an exclude rule", source: "https://mit.example/lib/private/x.json",
			want: Decision{Decision: Exclude, RuleID: "banned", Reason: "not redistributable (rule banned)"},
		},
		{
			name: "under a review rule", source: "https://held.example/x.json",
			want: Decision{Decision: Review, RuleID: "held", Reason: "owner asked for time (rule held)"},
		},
		{
			name: "asserting no license", source: "https://other.example/root.json", deps: []string{"https://other.example/none.json"},
			want: Decision{Decision: Review, Reason: "dependency https://other.example/none.json: the local source file declares no usable license: " +
				`license "NOASSERTION" asserts no license: NOASSERTION grants no permission to redistribute`},
		},
		{
			name: "undeclared dependency", source: "https://other.example/root.json", deps: []string{"https://other.example/dep.json"},
			want: Decision{Decision: Review, Reason: "dependency https://other.example/dep.json: " + NoRuleReason},
		},
		{
			name:   "combined with rules",
			source: "https://other.example/root.json",
			deps: []string{
				"https://www.store.example/a.json", "https://other.example/isc.json", "https://other.example/also-mit.json",
				"https://mit.example/lib/y.json", "http://json-schema.org/draft-07/schema#",
			},
			want: Decision{
				Decision: Allow, License: "Apache-2.0 AND ISC AND MIT", Notice: "Copyright (c) Example Authors\n\nStore notice",
				Reason: DeclaredReason + "; MIT license file checked (rule mit-lib); store repository license (rule store)",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.DecideWith(tt.source, tt.deps, nil, declared); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DecideWith = %#v\nwant %#v", got, tt.want)
			}
		})
	}

	var none *Policy

	if got := none.DecideWith("https://other.example/root.json", []string{"https://other.example/isc.json"}, nil, declared); !reflect.DeepEqual(got,
		Decision{Decision: Allow, License: "ISC AND MIT", Reason: DeclaredReason}) {
		t.Errorf("without a policy = %#v", got)
	}

	if got := none.DecideWith("https://other.example/dep.json", nil, nil, declared); !reflect.DeepEqual(got,
		Decision{Decision: Review, Reason: NoRuleReason}) {
		t.Errorf("undeclared without a policy = %#v", got)
	}
}

func TestDeclaredURLsAreNotDetected(t *testing.T) {
	p, err := Load(writePolicy(t, "[auto]\nenabled = true\n", nil))
	if err != nil {
		t.Fatal(err)
	}

	var asked []string

	lookup := func(url string) (Finding, bool) {
		asked = append(asked, url)

		return Finding{Failure: RefusedNoLicense, Detail: "none"}, true
	}

	got := p.DecideWith("https://a.example/x.json", []string{"https://b.example/y.json"}, lookup, func(url string) (string, bool) {
		return "MIT", url == "https://a.example/x.json"
	})

	if !slices.Equal(asked, []string{"https://b.example/y.json"}) || got.Decision != Review || len(got.Detections) != 1 {
		t.Errorf("asked %v, decision %#v", asked, got)
	}
}
