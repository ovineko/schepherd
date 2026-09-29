package match

import (
	"errors"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func mustSet(t *testing.T, origin Origin, rules ...Rule) *Set {
	t.Helper()

	set, err := NewSet(origin, rules)
	if err != nil {
		t.Fatal(err)
	}

	return set
}

func TestPatternSemantics(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"package.json", "package.json", true},
		{"package.json", "packages/a/package.json", true},
		{"package.json", "package.json5", false},
		{"Package.json", "package.json", false},
		{"*.schema.json", "deep/dir/x.schema.json", true},
		{"**/.github/workflows/*.yml", ".github/workflows/ci.yml", true},
		{"**/.github/workflows/*.yml", "sub/.github/workflows/ci.yml", true},
		{".github/workflows/*.yml", ".github/workflows/ci.yml", true},
		{".github/workflows/*.yml", "sub/.github/workflows/ci.yml", false},
		{".eslintrc", "a/b/.eslintrc", true},
		{"/tsconfig.json", "tsconfig.json", true},
		{"/tsconfig.json", "pkg/tsconfig.json", false},
		{"./tsconfig.json", "tsconfig.json", true},
		{"./tsconfig.json", "pkg/tsconfig.json", false},
		{"tsconfig.*.json", "tsconfig.build.json", true},
		{"?.json", "a.json", true},
		{"?.json", "ab.json", false},
		{"[ab].json", "b.json", true},
		{"[!ab].json", "b.json", false},
		{"{a,b}.yaml", "b.yaml", true},
		{"config/**/*.toml", "config/x/y/z.toml", true},
		{"config/**/*.toml", "config/z.toml", true},
		{"config/**/*.toml", "other/config/z.toml", false},
		{`a\*b.json`, "a*b.json", true},
		{`a\*b.json`, "axb.json", false},
		{`a\!(b).json`, "a!(b).json", true},
	}

	for _, tc := range cases {
		p, err := ParsePattern(tc.pattern)
		if err != nil {
			t.Fatalf("ParsePattern(%q): %v", tc.pattern, err)
		}

		if got := p.matches(tc.path); got != tc.want {
			t.Errorf("%q vs %q = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestInvalidPatterns(t *testing.T) {
	for _, raw := range []string{"", "!", "/", "./", "dir/", "[", "{a,b", "!./", "**/.github/ISSUE_TEMPLATE/!(config).yml", "!(a).json", "@(a|b).json", "+(x)", "a*(b)"} {
		if err := ValidatePattern(raw); err == nil {
			t.Errorf("ValidatePattern(%q) accepted an invalid pattern", raw)
		}
	}
}

func TestNegativePatterns(t *testing.T) {
	set := mustSet(t, OriginCatalog, Rule{SchemaID: "json", Patterns: []string{"*.json", "!package.json", "!**/node_modules/**"}})

	cases := map[string]bool{
		"a.json":                    true,
		"package.json":              false,
		"sub/package.json":          false,
		"node_modules/x/a.json":     false,
		"sub/node_modules/x/a.json": false,
	}

	for rel, want := range cases {
		if got := len(set.Match(rel)) == 1; got != want {
			t.Errorf("Match(%q) = %v, want %v", rel, got, want)
		}
	}

	onlyNegative := mustSet(t, OriginCatalog, Rule{SchemaID: "x", Patterns: []string{"!a.json"}})
	if ids := onlyNegative.Match("b.json"); len(ids) != 0 {
		t.Errorf("rule with only negative patterns matched: %v", ids)
	}
}

func TestResolvePrecedenceAndAmbiguity(t *testing.T) {
	catalog := mustSet(t, OriginCatalog,
		Rule{SchemaID: "compose-a", Patterns: []string{"compose.yaml"}},
		Rule{SchemaID: "compose-b", Patterns: []string{"compose.yaml", "compose.yml"}},
		Rule{SchemaID: "package", Patterns: []string{"package.json"}},
	)
	mappings := mustSet(t, OriginMapping, Rule{SchemaID: "company", Patterns: []string{"config/company.json", "compose.yml"}})

	res, err := ResolveFirst("packages/x/package.json", mappings, catalog)
	if err != nil || res.SchemaID != "package" || res.Origin != OriginCatalog {
		t.Errorf("catalog resolution = %+v, %v", res, err)
	}

	res, err = ResolveFirst("compose.yml", mappings, catalog)
	if err != nil || res.SchemaID != "company" || res.Origin != OriginMapping {
		t.Errorf("mapping override = %+v, %v", res, err)
	}

	_, err = ResolveFirst("compose.yaml", mappings, catalog)

	var ambiguous *AmbiguousError
	if !errors.As(err, &ambiguous) || !slices.Equal(ambiguous.Candidates, []string{"compose-a", "compose-b"}) {
		t.Errorf("ambiguity = %v", err)
	}

	if _, err := ResolveFirst("README.md", mappings, catalog); !errors.Is(err, ErrNoMatch) {
		t.Errorf("no match = %v", err)
	}
}

func TestResolveFirstAppliesLocalSchemasBetweenMappingsAndCatalog(t *testing.T) {
	mappings := mustSet(t, OriginMapping, Rule{SchemaID: "company", Patterns: []string{"config/company.json"}})
	local := mustSet(t, OriginLocal,
		Rule{SchemaID: "local-company", Patterns: []string{"config/*.json", "!config/legacy.json"}},
		Rule{SchemaID: "deploy-a", Patterns: []string{"deploy.json"}},
		Rule{SchemaID: "deploy-b", Patterns: []string{"deploy.json"}},
	)
	catalog := mustSet(t, OriginCatalog,
		Rule{SchemaID: "json", Patterns: []string{"*.json"}},
		Rule{SchemaID: "package", Patterns: []string{"package.json"}},
		Rule{SchemaID: "manifest", Patterns: []string{"package.json"}},
	)

	cases := []struct {
		rel    string
		id     string
		origin Origin
	}{
		{"config/company.json", "company", OriginMapping},
		{"config/app.json", "local-company", OriginLocal},
		{"config/legacy.json", "json", OriginCatalog},
		{"other.json", "json", OriginCatalog},
	}

	for _, c := range cases {
		res, err := ResolveFirst(c.rel, mappings, local, catalog)
		if err != nil || res.SchemaID != c.id || res.Origin != c.origin {
			t.Errorf("%s: %+v, %v; want %s via %s", c.rel, res, err, c.id, c.origin)
		}
	}

	var ambiguous *AmbiguousError

	_, err := ResolveFirst("deploy.json", mappings, local, catalog)
	if !errors.As(err, &ambiguous) || ambiguous.Origin != OriginLocal || !slices.Equal(ambiguous.Candidates, []string{"deploy-a", "deploy-b"}) {
		t.Errorf("ambiguity among local schemas = %v", err)
	}

	if err != nil && !strings.Contains(err.Error(), "via local rules") {
		t.Errorf("ambiguity message %q does not name the local level", err)
	}

	res, err := ResolveFirst("config/app.json", nil, local, nil)
	if err != nil || res.SchemaID != "local-company" {
		t.Errorf("nil levels are skipped: %+v, %v", res, err)
	}

	if _, err := ResolveFirst("README.md", mappings, local); !errors.Is(err, ErrNoMatch) {
		t.Errorf("no match = %v", err)
	}

	if _, err := ResolveFirst("x.json"); !errors.Is(err, ErrNoMatch) {
		t.Errorf("no sets = %v", err)
	}
}

func TestOriginNames(t *testing.T) {
	for origin, name := range map[Origin]string{OriginMapping: "mapping", OriginLocal: "local", OriginCatalog: "catalog"} {
		if got := origin.String(); got != name {
			t.Errorf("%d.String() = %q, want %q", origin, got, name)
		}
	}
}

func TestMatchOrderIsDeterministic(t *testing.T) {
	rules := []Rule{
		{SchemaID: "b", Patterns: []string{"*.json"}},
		{SchemaID: "a", Patterns: []string{"x.json"}},
		{SchemaID: "b", Patterns: []string{"x.*"}},
	}
	set := mustSet(t, OriginCatalog, rules...)

	for range 50 {
		if got := set.Match("x.json"); !slices.Equal(got, []string{"b", "a"}) {
			t.Fatalf("Match order = %v", got)
		}
	}
}

func TestRelativePath(t *testing.T) {
	root := t.TempDir()

	rel, err := RelativePath(root, filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil || rel != ".github/workflows/ci.yml" {
		t.Errorf("RelativePath = %q, %v", rel, err)
	}

	for _, outside := range []string{filepath.Dir(root), root, filepath.Join(root, "..", "x.json")} {
		if _, err := RelativePath(root, outside); !errors.Is(err, ErrOutsideWorkspace) {
			t.Errorf("RelativePath(%q) = %v, want ErrOutsideWorkspace", outside, err)
		}
	}

	if runtime.GOOS == "windows" {
		rel, err := RelativePath(`C:\ws`, `C:\ws\a\b.json`)
		if err != nil || rel != "a/b.json" {
			t.Errorf("windows separators = %q, %v", rel, err)
		}
	}
}
