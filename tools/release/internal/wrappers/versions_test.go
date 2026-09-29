package wrappers

import (
	"cmp"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/buildinfo/semver"
)

func TestVersionsOf(t *testing.T) {
	for in, want := range map[string]Versions{
		"0.1.0":                  {SemVer: "0.1.0", PEP440: "0.1.0", Gem: "0.1.0"},
		"12.30.4":                {SemVer: "12.30.4", PEP440: "12.30.4", Gem: "12.30.4"},
		"1.0.0-alpha.1":          {SemVer: "1.0.0-alpha.1", PEP440: "1.0.0a1", Gem: "1.0.0.alpha.1"},
		"1.0.0-beta.2":           {SemVer: "1.0.0-beta.2", PEP440: "1.0.0b2", Gem: "1.0.0.beta.2"},
		"2.3.4-rc.12":            {SemVer: "2.3.4-rc.12", PEP440: "2.3.4rc12", Gem: "2.3.4.rc.12"},
		"0.0.0-snapshot-f94b599": {SemVer: "0.0.0-snapshot-f94b599", PEP440: "0.0.0+snapshot.f94b599", Gem: "0.0.0.1.snapshot.f94b599", Snapshot: true},
	} {
		if got, err := VersionsOf(in); err != nil || got != want {
			t.Errorf("VersionsOf(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
}

func TestVersionsOfRejects(t *testing.T) {
	for in, want := range map[string]string{
		"":                             "invalid semantic version",
		"v0.1.0":                       "invalid semantic version",
		"0.1":                          "invalid semantic version",
		"0.1.0+build.1":                "build metadata",
		"0.0.0":                        "reserved for snapshot",
		"0.0.0-20260101120000-abcdef0": "reserved for snapshot",
		"0.1.0-rc":                     "no PEP 440 form",
		"0.1.0-rc1":                    "no PEP 440 form",
		"0.1.0-rc.1.2":                 "no PEP 440 form",
		"0.1.0-RC.1":                   "no PEP 440 form",
		"0.1.0-preview.1":              "no PEP 440 form",
		"1.0.0-snapshot-f94b599":       "no PEP 440 form",
		"0.1.0-rc.0":                   "no RubyGems form",
		"0.0.0-snapshot-":              "lowercase hexadecimal",
		"0.0.0-snapshot-F94B599":       "lowercase hexadecimal",
		"0.0.0-snapshot-f94b599+x":     "lowercase hexadecimal",
	} {
		if got, err := VersionsOf(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("VersionsOf(%q) = %+v, %v; want an error containing %q", in, got, err, want)
		}
	}
}

func TestGemVersionRefusesFormsRubyGemsReorders(t *testing.T) {
	for _, in := range []string{"1.0.0-1", "1.0.0-rc.beta", "1.0.0-rc.0", "1.0.0-rc.1.0", "1.0.0-pre-release", "1.0.0-RC.1"} {
		v, err := semver.Parse(in)
		if err != nil {
			t.Fatal(err)
		}

		if got, err := GemVersion(v); err == nil {
			t.Errorf("GemVersion(%s) = %s, want an error", in, got)
		}
	}
}

// rubyCanonical returns Gem::Version#canonical_segments: numbers as
// integers, trailing zeros removed from the numeric part before the first
// string and from the part after it.
func rubyCanonical(version string) []any {
	var numeric, rest []any

	for _, s := range regexp.MustCompile(`[0-9]+|[a-zA-Z]+`).FindAllString(version, -1) {
		var seg any = s
		if n, err := strconv.Atoi(s); err == nil {
			seg = n
		}

		if _, isString := seg.(string); isString || len(rest) > 0 {
			rest = append(rest, seg)
		} else {
			numeric = append(numeric, seg)
		}
	}

	trim := func(segs []any) []any {
		for len(segs) > 0 && segs[len(segs)-1] == 0 {
			segs = segs[:len(segs)-1]
		}

		return segs
	}

	return append(trim(numeric), trim(rest)...)
}

// rubyCompare is Gem::Version#<=>: a missing segment is 0 and a string sorts
// below a number.
func rubyCompare(a, b string) int {
	l, r := rubyCanonical(a), rubyCanonical(b)

	for i := range max(len(l), len(r)) {
		var lhs, rhs any = 0, 0
		if i < len(l) {
			lhs = l[i]
		}

		if i < len(r) {
			rhs = r[i]
		}

		ls, lString := lhs.(string)
		rs, rString := rhs.(string)
		ln, _ := lhs.(int)
		rn, _ := rhs.(int)

		switch {
		case lhs == rhs:
			continue
		case lString && rString:
			return cmp.Compare(ls, rs)
		case lString:
			return -1
		case rString:
			return 1
		default:
			return cmp.Compare(ln, rn)
		}
	}

	return 0
}

func TestRubyCompareFollowsRubyGems(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1", 0},
		{"1.0.0.rc.1", "1.0.0", -1},
		{"1.0.0.rc.1.0", "1.0.0.rc.1", 0},
		{"1.0.0.rc.beta", "1.0.0.rc.1", -1},
		{"1.0.0.rc10", "1.0.0.rc.9", 1},
		{"0.0.0.snapshot.abc", "0", -1},
		{"0.0.0.1.snapshot.abc", "0", 1},
	} {
		if got := rubyCompare(c.a, c.b); got != c.want {
			t.Errorf("rubyCompare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestGemVersionsSortLikeSemVer checks that RubyGems orders the mapped
// versions as SemVer orders the client versions and never treats two of
// them as equal, and that each satisfies ">= 0" and ">= 0.a", the
// requirements RubyGems runs an installed executable under.
func TestGemVersionsSortLikeSemVer(t *testing.T) {
	versions := []string{
		"0.0.0-snapshot-bcaba9f", "0.1.0", "0.1.1", "0.2.0-alpha.1", "0.2.0-alpha.2", "0.2.0-alpha.10", "0.2.0-beta.1",
		"0.2.0-beta.2", "0.2.0-rc.1", "0.2.0-rc.2", "0.2.0", "0.10.0", "1.0.0-alpha.1", "1.0.0-rc.1", "1.0.0", "1.0.1", "10.0.0",
	}

	for i, a := range versions {
		va, err := VersionsOf(a)
		if err != nil {
			t.Fatal(err)
		}

		if rubyCompare(va.Gem, "0") < 0 || rubyCompare(va.Gem, "0.a") < 0 {
			t.Errorf("%s (from %s) is below 0 or 0.a for RubyGems, so its executable would never run", va.Gem, a)
		}

		for j, b := range versions {
			vb, err := VersionsOf(b)
			if err != nil {
				t.Fatal(err)
			}

			if got := rubyCompare(va.Gem, vb.Gem); got != cmp.Compare(i, j) {
				t.Errorf("RubyGems orders %s and %s as %d, SemVer orders %s and %s as %d", va.Gem, vb.Gem, got, a, b, cmp.Compare(i, j))
			}
		}
	}
}
