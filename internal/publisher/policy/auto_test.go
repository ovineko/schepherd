package policy

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
)

const autoPolicy = `
[auto]
enabled = true

[[rules]]
id = "store"
decision = "allow"
hosts = ["www.store.example"]
license = "Apache-2.0"
notice = "Store notice"
reason = "store repository license"

[[rules]]
id = "held-repo"
decision = "review"
hosts = ["raw.example"]
path_prefix = "/held/"
reason = "owner asked for time"

[[rules]]
id = "banned-repo"
decision = "exclude"
hosts = ["raw.example"]
path_prefix = "/banned/"
reason = "not redistributable"
`

func loadAuto(t *testing.T, content string) *Policy {
	t.Helper()

	p, err := Load(writePolicy(t, content, nil))
	if err != nil {
		t.Fatal(err)
	}

	return p
}

// findings is a Lookup over a fixed map that records which URLs it was
// asked about.
type findings struct {
	byURL map[string]Finding
	asked []string
}

func (f *findings) lookup(url string) (Finding, bool) {
	f.asked = append(f.asked, url)
	finding, ok := f.byURL[url]

	return finding, ok
}

func mit(source string) Finding {
	return Finding{
		Source: source, License: "MIT", LicenseFile: "LICENSE", LicenseDigest: "sha256:" + strings.Repeat("a", 64),
		Notice: "MIT license of " + source,
	}
}

func TestAutoSectionDefaults(t *testing.T) {
	a := loadAuto(t, autoPolicy).Auto()

	want := Auto{
		Enabled: true,
		Allowed: []string{"MIT", "MIT-0", "Apache-2.0", "BSD-2-Clause", "BSD-3-Clause", "ISC", "0BSD", "CC0-1.0", "Unlicense", "BlueOak-1.0.0"},
		Hosts:   []string{"api.github.com", "registry.npmjs.org", "unpkg.com", "cdn.jsdelivr.net"},
	}
	if !reflect.DeepEqual(a, want) {
		t.Fatalf("Auto() = %#v", a)
	}

	a.Allowed[0] = "GPL-3.0-only"
	if loadAuto(t, autoPolicy).Auto().Allowed[0] != "MIT" {
		t.Fatal("Auto() returned internal storage")
	}

	if got := loadSample(t).Auto(); got.Enabled || got.Allowed != nil || got.Hosts != nil {
		t.Fatalf("a policy without [auto] = %#v", got)
	}

	custom := loadAuto(t, "[auto]\nenabled = true\nallow = [\"MIT\", \"ISC\"]\nhosts = [\"registry.npmjs.org\", \"unpkg.com\"]\n")
	if got := custom.Auto(); !slices.Equal(got.Allowed, []string{"MIT", "ISC"}) || !slices.Equal(got.Hosts, []string{"registry.npmjs.org", "unpkg.com"}) {
		t.Fatalf("custom Auto() = %#v", got)
	}

	if loadAuto(t, "[auto]\nallow = [\"MIT\"]\n").Auto().Enabled {
		t.Fatal("[auto] without enabled = true is enabled")
	}
}

func TestAutoSectionErrors(t *testing.T) {
	cases := []struct {
		name, content, want string
	}{
		{"empty allow", "[auto]\nenabled = true\nallow = []\n", "auto.allow must not be empty"},
		{"expression in allow", "[auto]\nallow = [\"MIT OR ISC\"]\n", "single SPDX license identifier"},
		{"non-assertion in allow", "[auto]\nallow = [\"NOASSERTION\"]\n", "asserts no license"},
		{"npm non-assertion in allow", "[auto]\nallow = [\"UNLICENSED\"]\n", "asserts no license"},
		{"duplicate allow", "[auto]\nallow = [\"MIT\", \"mit\"]\n", "twice"},
		{"bad identifier", "[auto]\nallow = [\"MIT;rm\"]\n", "SPDX-like"},
		{"LicenseRef in allow", "[auto]\nallow = [\"MIT\", \"LicenseRef-Permissive\"]\n", "LicenseRef or DocumentRef"},
		{"DocumentRef in allow", "[auto]\nallow = [\"documentref-spdx:LicenseRef-MIT\"]\n", "LicenseRef or DocumentRef"},
		{"empty hosts", "[auto]\nhosts = []\n", "auto.hosts must not be empty"},
		{"unknown host", "[auto]\nhosts = [\"evil.example\"]\n", "not one of api.github.com"},
		{"raw host is not contacted", "[auto]\nhosts = [\"raw.githubusercontent.com\"]\n", "not one of"},
		{"duplicate host", "[auto]\nhosts = [\"unpkg.com\", \"unpkg.com\"]\n", "twice"},
		{"unknown key", "[auto]\nenable = true\n", "unknown key"},
		{"wrong case key", "[auto]\nEnabled = true\n", "Enabled"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writePolicy(t, tc.content, nil))
			if err == nil || fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want fault.Usage containing %q", err, tc.want)
			}
		})
	}
}

func TestAutoPermits(t *testing.T) {
	a := loadAuto(t, autoPolicy).Auto()

	cases := map[string]bool{
		"MIT":                           true,
		"mit":                           true,
		"Apache-2.0":                    true,
		"Unlicense":                     true,
		"0BSD":                          true,
		"BlueOak-1.0.0":                 true,
		"GPL-3.0-only":                  false,
		"MIT OR GPL-3.0-only":           true,
		"GPL-3.0-only OR MIT":           true,
		"MIT AND GPL-3.0-only":          false,
		"MIT AND ISC":                   true,
		"(MIT OR GPL-3.0-only) AND ISC": true,
		"(MIT OR GPL-3.0-only) AND LGPL-2.1-only": false,
		// AND binds tighter than OR: MIT OR (GPL-3.0-only AND GPL-2.0-only).
		"MIT OR GPL-3.0-only AND GPL-2.0-only":  true,
		"GPL-2.0-only AND MIT OR ISC":           true,
		"GPL-2.0-only AND (MIT OR ISC)":         false,
		"Apache-2.0 WITH LLVM-exception":        false,
		"MIT OR Apache-2.0 WITH LLVM-exception": true,
		"Apache-2.0+":                           false,
		"LicenseRef-Permissive":                 false,
		"NOASSERTION":                           false,
		"UNLICENSED":                            false,
		"MIT OR NONE":                           false,
		"SEE LICENSE IN LICENSE.txt":            false,
		"MIT or ISC":                            false,
		"":                                      false,
		"(MIT":                                  false,
	}

	for expr, want := range cases {
		if got := a.Permits(expr); got != want {
			t.Errorf("Permits(%q) = %v, want %v", expr, got, want)
		}
	}
}

func TestAutoNeverPermitsLocalReferences(t *testing.T) {
	a := Auto{Enabled: true, Allowed: []string{"MIT", "LicenseRef-MIT", "DocumentRef-spdx:LicenseRef-MIT"}}

	for expr, want := range map[string]bool{
		"MIT":                             true,
		"LicenseRef-MIT":                  false,
		"licenseref-mit":                  false,
		"DocumentRef-spdx:LicenseRef-MIT": false,
		"LicenseRef-MIT OR MIT":           true,
		"LicenseRef-MIT AND MIT":          false,
	} {
		if got := a.Permits(expr); got != want {
			t.Errorf("Permits(%q) = %v, want %v", expr, got, want)
		}
	}
}

func TestDecideWithDetection(t *testing.T) {
	p := loadAuto(t, autoPolicy)

	const (
		root   = "https://raw.example/owner/repo/main/schema.json"
		dep    = "https://raw.example/other/lib/v1/dep.json"
		store  = "https://www.store.example/a.json"
		held   = "https://raw.example/held/repo/main/x.json"
		banned = "https://raw.example/banned/repo/main/x.json"
	)

	allowedRoot := Detection{
		URL: root, Source: "github:owner/repo@abc", License: "MIT", LicenseFile: "LICENSE",
		LicenseDigest: "sha256:" + strings.Repeat("a", 64), Verdict: Allow,
	}

	t.Run("permitted license allows the source", func(t *testing.T) {
		f := &findings{byURL: map[string]Finding{root: mit("github:owner/repo@abc")}}

		got := p.DecideWith(root, nil, f.lookup, nil)
		want := Decision{
			Decision: Allow, License: "MIT", Notice: "MIT license of github:owner/repo@abc",
			Reason: "license MIT detected in github:owner/repo@abc (automatic)", Detections: []Detection{allowedRoot},
		}

		if !reflect.DeepEqual(got, want) {
			t.Fatalf("DecideWith =\n%#v\nwant\n%#v", got, want)
		}
	})

	t.Run("rules and detections combine", func(t *testing.T) {
		f := &findings{byURL: map[string]Finding{dep: mit("github:other/lib@def"), root: mit("github:owner/repo@abc")}}

		got := p.DecideWith(store, []string{dep, root, dep + "#/definitions/x"}, f.lookup, nil)
		if got.Decision != Allow || got.RuleID != "store" || got.License != "Apache-2.0 AND MIT" ||
			got.Notice != "Store notice\n\nMIT license of github:other/lib@def\n\nMIT license of github:owner/repo@abc" {
			t.Fatalf("DecideWith = %#v", got)
		}

		if len(got.Detections) != 2 || got.Detections[0].URL != dep || got.Detections[1].URL != root {
			t.Fatalf("detections = %#v", got.Detections)
		}

		if slices.Contains(f.asked, store) {
			t.Fatalf("detection was asked about a URL a rule decides: %v", f.asked)
		}
	})

	t.Run("identical notices are carried once", func(t *testing.T) {
		shared := mit("github:owner/repo@abc")
		other := root + "2"
		f := &findings{byURL: map[string]Finding{root: shared, other: shared}}

		got := p.DecideWith(root, []string{other}, f.lookup, nil)
		if got.Decision != Allow || got.Notice != shared.Notice || len(got.Detections) != 2 {
			t.Fatalf("DecideWith = %#v", got)
		}
	})

	t.Run("explicit rules keep precedence", func(t *testing.T) {
		f := &findings{byURL: map[string]Finding{held: mit("github:held/repo@abc"), banned: mit("github:banned/repo@abc"), root: mit("x")}}

		if got := p.DecideWith(held, nil, f.lookup, nil); got.Decision != Review || got.RuleID != "held-repo" || got.AutoReason != "" {
			t.Errorf("review rule = %#v", got)
		}

		if got := p.DecideWith(root, []string{banned}, f.lookup, nil); got.Decision != Exclude || got.RuleID != "banned-repo" {
			t.Errorf("exclude rule = %#v", got)
		}

		if slices.Contains(f.asked, held) || slices.Contains(f.asked, banned) {
			t.Errorf("detection was asked about ruled URLs: %v", f.asked)
		}
	})

	refusals := []struct {
		finding Finding
		name    string
		reason  string
		detail  string
	}{
		{name: "no assertion", finding: Finding{Source: "github:o/r@1", License: "NOASSERTION", Notice: "x"}, reason: RefusedNotAsserted, detail: "asserts no license"},
		{name: "npm unlicensed", finding: Finding{Source: "npm:x@1.0.0", License: "UNLICENSED", Notice: "x"}, reason: RefusedNotAsserted, detail: "UNLICENSED"},
		{name: "not an expression", finding: Finding{Source: "npm:x@1.0.0", License: "SEE LICENSE IN LICENSE", Notice: "x"}, reason: RefusedNotAsserted, detail: "not an SPDX license expression"},
		{name: "copyleft", finding: Finding{Source: "github:o/r@1", License: "GPL-3.0-only", Notice: "x"}, reason: RefusedNotPermissive, detail: "not on the [auto] allow list"},
		{name: "one copyleft operand of AND", finding: Finding{Source: "npm:x@1.0.0", License: "MIT AND GPL-3.0-only", Notice: "x"}, reason: RefusedNotPermissive},
		{name: "no license", finding: Finding{Source: "github:o/r@1", Failure: RefusedNoLicense, Detail: "the repository has no license file"}, reason: RefusedNoLicense, detail: "no license file"},
		{name: "empty license", finding: Finding{Source: "github:o/r@1"}, reason: RefusedNoLicense},
		{name: "license without text", finding: Finding{Source: "github:o/r@1", License: "MIT"}, reason: RefusedNoLicense, detail: "no license text"},
		{name: "fetch failed", finding: Finding{Failure: RefusedFetchFailed, Detail: "GitHub API rate limit exceeded"}, reason: RefusedFetchFailed, detail: "rate limit"},
		{name: "unsupported host", finding: Finding{Failure: RefusedUnsupportedHost, Detail: "raw.example is not supported"}, reason: RefusedUnsupportedHost},
	}

	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			f := &findings{byURL: map[string]Finding{root: tc.finding}}

			got := p.DecideWith(root, nil, f.lookup, nil)
			if got.Decision != Review || got.AutoReason != tc.reason || got.RuleID != "" || got.License != "" || got.Notice != "" ||
				!strings.HasPrefix(got.Reason, NoRuleReason+"; automatic license detection ("+tc.reason+"): ") || !strings.Contains(got.Reason, tc.detail) {
				t.Fatalf("DecideWith = %#v", got)
			}

			if len(got.Detections) != 1 || got.Detections[0].Verdict != Review || got.Detections[0].Reason != tc.reason {
				t.Fatalf("detections = %#v", got.Detections)
			}
		})
	}

	t.Run("a refused dependency holds an allowed source", func(t *testing.T) {
		f := &findings{byURL: map[string]Finding{root: mit("github:owner/repo@abc"), dep: {Source: "github:other/lib@def", License: "GPL-2.0-only", Notice: "x"}}}

		got := p.DecideWith(root, []string{dep}, f.lookup, nil)
		if got.Decision != Review || got.AutoReason != RefusedNotPermissive || !strings.HasPrefix(got.Reason, "dependency "+dep+": ") {
			t.Fatalf("DecideWith = %#v", got)
		}

		if len(got.Detections) != 2 || got.Detections[0].Verdict != Allow || got.Detections[1].Verdict != Review {
			t.Fatalf("detections = %#v", got.Detections)
		}
	})

	t.Run("a review rule before a refusal names the rule", func(t *testing.T) {
		f := &findings{byURL: map[string]Finding{root: {Failure: RefusedFetchFailed, Detail: "down"}}}

		got := p.DecideWith(held, []string{root}, f.lookup, nil)
		if got.Decision != Review || got.RuleID != "held-repo" || got.AutoReason != "" || len(got.Detections) != 1 {
			t.Fatalf("DecideWith = %#v", got)
		}
	})

	t.Run("no finding leaves the URL unruled", func(t *testing.T) {
		got := p.DecideWith(root, nil, (&findings{}).lookup, nil)
		if !reflect.DeepEqual(got, Decision{Decision: Review, Reason: NoRuleReason}) {
			t.Fatalf("DecideWith = %#v", got)
		}
	})

	t.Run("an unusable license value is not recorded", func(t *testing.T) {
		f := &findings{byURL: map[string]Finding{root: {Source: "npm:x@1.0.0", License: "MIT\u0007", Notice: "x"}}}

		got := p.DecideWith(root, nil, f.lookup, nil)
		if got.AutoReason != RefusedNotAsserted || got.Detections[0].License != "" || !strings.Contains(got.Reason, `\a`) {
			t.Fatalf("DecideWith = %#v", got)
		}
	})

	t.Run("notices beyond the limit hold the schema", func(t *testing.T) {
		big := strings.Repeat("x", MaxCombinedNoticeBytes/2+1)
		a, b := mit("github:a/a@1"), mit("github:b/b@1")
		a.Notice, b.Notice = "a"+big, "b"+big
		f := &findings{byURL: map[string]Finding{root: a, dep: b}}

		got := p.DecideWith(root, []string{dep}, f.lookup, nil)
		if got.Decision != Review || !strings.Contains(got.Reason, "more than the limit") || got.Notice != "" || len(got.Detections) != 2 {
			t.Fatalf("DecideWith = %.300s", got.Reason)
		}
	})
}

func TestDecideWithNeedsEnabledAuto(t *testing.T) {
	const url = "https://raw.example/owner/repo/main/schema.json"

	for _, p := range []*Policy{loadSample(t), loadAuto(t, "[auto]\nenabled = false\n")} {
		f := &findings{byURL: map[string]Finding{url: mit("github:owner/repo@abc")}}

		got := p.DecideWith(url, nil, f.lookup, nil)
		if !reflect.DeepEqual(got, Decision{Decision: Review, Reason: NoRuleReason}) || len(f.asked) != 0 {
			t.Fatalf("DecideWith with detection disabled = %#v (asked %v)", got, f.asked)
		}
	}
}

func TestDecideIgnoresDetection(t *testing.T) {
	const url = "https://raw.example/owner/repo/main/schema.json"

	if got := loadAuto(t, autoPolicy).Decide(url, nil); !reflect.DeepEqual(got, Decision{Decision: Review, Reason: NoRuleReason}) {
		t.Fatalf("Decide = %#v", got)
	}
}

func TestHasRule(t *testing.T) {
	p := loadAuto(t, autoPolicy)

	for url, want := range map[string]string{
		"https://www.store.example/a.json":         "store",
		"https://raw.example/held/repo/x.json":     "held-repo",
		"https://raw.example/banned/repo/x.json":   "banned-repo",
		"https://raw.example/owner/repo/x.json":    "",
		"https://www.store.example/a.json?v=1":     "",
		"ftp://www.store.example/a.json":           "",
		"https://raw.example/held/../owner/x.json": "",
	} {
		if got := p.HasRule(url); got != (want != "") {
			t.Errorf("HasRule(%s) = %v, want %v", url, got, want != "")
		}

		if got := p.RuleID(url); got != want {
			t.Errorf("RuleID(%s) = %q, want %q", url, got, want)
		}
	}

	for id, want := range map[string]bool{"store": true, "held-repo": true, "a.b_c-1": true, "Bad Rule": false, "-x": false, "": false} {
		if got := ValidRuleID(id); got != want {
			t.Errorf("ValidRuleID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestCommittedPolicyEnablesDetection(t *testing.T) {
	p, err := Load("../../../sources/licenses.toml")
	if err != nil {
		t.Fatal(err)
	}

	a := p.Auto()
	if !a.Enabled || !slices.Equal(a.Allowed, defaultAutoAllowed) || !slices.Equal(a.Hosts, detectionHosts) {
		t.Fatalf("sources/licenses.toml [auto] = %#v", a)
	}

	for _, url := range []string{
		"https://www.schemastore.org/tsconfig.json",
		"https://raw.githubusercontent.com/SchemaStore/schemastore/master/src/schemas/json/rustfmt.json",
	} {
		if !p.HasRule(url) {
			t.Errorf("%s is left to detection; SchemaStore files keep their explicit rules", url)
		}
	}
}
