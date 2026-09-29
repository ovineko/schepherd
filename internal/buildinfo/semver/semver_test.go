package semver

import (
	"slices"
	"strings"
	"testing"
)

func TestParseAcceptsTheSpecification(t *testing.T) {
	cases := map[string]Version{
		"0.1.0":                       {Minor: 1},
		"1.2.3":                       {Major: 1, Minor: 2, Patch: 3},
		"10.20.30":                    {Major: 10, Minor: 20, Patch: 30},
		"0.1.0-rc.1":                  {Minor: 1, Prerelease: "rc.1"},
		"1.0.0-alpha-beta.0.x-y":      {Major: 1, Prerelease: "alpha-beta.0.x-y"},
		"1.0.0-0A.is.legal":           {Major: 1, Prerelease: "0A.is.legal"},
		"1.0.0+build.007":             {Major: 1, Build: "build.007"},
		"1.0.0-rc.1+exp.sha.5114f85":  {Major: 1, Prerelease: "rc.1", Build: "exp.sha.5114f85"},
		"18446744073709551615.0.0":    {Major: 18446744073709551615},
		"0.0.0-snapshot-0123456789ab": {Prerelease: "snapshot-0123456789ab"},
	}

	for s, want := range cases {
		got, err := Parse(s)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", s, got, err, want)
		}

		if got.String() != s {
			t.Errorf("Parse(%q).String() = %q", s, got.String())
		}

		tag, err := ParseTag("v" + s)
		if err != nil || tag != want || tag.Tag() != "v"+s {
			t.Errorf("ParseTag(v%s) = %+v (tag %q), %v", s, tag, tag.Tag(), err)
		}
	}
}

func TestParseRefusesWhatTheSpecificationForbids(t *testing.T) {
	for _, s := range []string{
		"", "1", "1.2", "1.2.3.4", "v1.2.3", "V1.2.3", " 1.2.3", "1.2.3 ", "1.2.3\n",
		"01.2.3", "1.02.3", "1.2.03", "-1.2.3", "1.-2.3", "+1.2.3",
		"1.2.3-", "1.2.3-rc..1", "1.2.3-rc.", "1.2.3-.rc", "1.2.3-01", "1.2.3-rc.01", "1.2.3-rc_1", "1.2.3-ü",
		"1.2.3+", "1.2.3+a..b", "1.2.3+a+b", "1.2.3+a_b",
		"18446744073709551616.0.0", "1.2.x",
	} {
		if v, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) = %+v, want an error", s, v)
		}
	}

	// Hyphens are part of the identifier charset, so these two are valid.
	for _, s := range []string{"1.2.3-rc.1-", "1.2.3--"} {
		if _, err := Parse(s); err != nil {
			t.Errorf("Parse(%q): %v", s, err)
		}
	}

	for _, tag := range []string{"0.1.0", "vv0.1.0", "v", "v01.0.0", "catalog-20260924.0905", "V0.1.0"} {
		if v, err := ParseTag(tag); err == nil {
			t.Errorf("ParseTag(%q) = %+v, want an error", tag, v)
		}
	}
}

func TestParseReleaseRules(t *testing.T) {
	accepted := []string{"0.1.0", "0.0.1", "1.0.0", "0.2.0-rc.1", "1.0.0-beta.11", "2.0.0-0.3.7", "0.1.1-0.x"}
	for _, s := range accepted {
		if v, err := ParseRelease(s); err != nil || v.String() != s {
			t.Errorf("ParseRelease(%q) = %q, %v", s, v, err)
		}

		if v, err := ParseReleaseTag("v" + s); err != nil || v.Tag() != "v"+s {
			t.Errorf("ParseReleaseTag(v%s) = %q, %v", s, v.Tag(), err)
		}
	}

	refused := map[string]string{
		"0.1.0+dirty":                           "build metadata",
		"1.0.0+incompatible":                    "build metadata",
		"0.0.0":                                 "0.0.0 is reserved",
		"0.0.0-snapshot-0123456789ab":           "0.0.0 is reserved",
		"1.0.0-20260924120000-0123456789ab":     "pseudo-version",
		"0.1.1-0.20260925080000-0123456789ab":   "pseudo-version",
		"0.2.0-rc.1.0.20260925080000-abcdef012": "pseudo-version",
		"20260924.1":                            "want MAJOR.MINOR.PATCH",
		"v0.1.0":                                "want MAJOR.MINOR.PATCH",
	}

	for s, want := range refused {
		_, err := ParseRelease(s)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseRelease(%q) error = %v, want it to mention %q", s, err, want)
		}

		if _, err := ParseReleaseTag("v" + s); err == nil {
			t.Errorf("ParseReleaseTag(v%s) accepted a non-release version", s)
		}
	}
}

// TestPseudoVersionShapesStayReleasable pins that only the go command's
// exact pseudo-version forms are refused, not every timestamp-like
// identifier.
func TestPseudoVersionShapesStayReleasable(t *testing.T) {
	for _, s := range []string{
		"1.2.0-20260924120000-0123456789ab",
		"0.1.1-rc.20260925080000-0123456789ab",
		"0.1.1-0.2026092508000-0123456789ab",
		"0.1.1-0.20260925080000-0123-abc",
	} {
		if _, err := ParseRelease(s); err != nil {
			t.Errorf("ParseRelease(%q): %v", s, err)
		}
	}
}

func TestComparePrecedence(t *testing.T) {
	// The ordering example of the specification (section 11), extended.
	ordered := []string{
		"0.0.1", "0.1.0-alpha", "0.1.0-alpha.1", "0.1.0-alpha.beta", "0.1.0-beta", "0.1.0-beta.2",
		"0.1.0-beta.11", "0.1.0-rc.1", "0.1.0", "0.1.1", "0.2.0-rc.1", "0.2.0", "0.10.0", "1.0.0-0",
		"1.0.0-1", "1.0.0-10", "1.0.0-A", "1.0.0-a", "1.0.0", "2.0.0", "10.0.0",
	}

	versions := make([]Version, 0, len(ordered))
	for _, s := range ordered {
		v, err := Parse(s)
		if err != nil {
			t.Fatal(err)
		}

		versions = append(versions, v)
	}

	for i := range versions {
		for j := range versions {
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}

			if got := Compare(versions[i], versions[j]); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", versions[i], versions[j], got, want)
			}
		}
	}

	shuffled := slices.Clone(versions)
	slices.Reverse(shuffled)
	slices.SortFunc(shuffled, Compare)

	if !slices.Equal(shuffled, versions) {
		t.Errorf("sorted by Compare: %v", shuffled)
	}

	a, _ := Parse("1.0.0+a")
	b, _ := Parse("1.0.0+b")

	if Compare(a, b) != 0 {
		t.Error("build metadata must not take part in precedence")
	}
}

func TestIsPrerelease(t *testing.T) {
	for s, want := range map[string]bool{"0.1.0": false, "0.1.0-rc.1": true, "1.0.0-0": true, "1.0.0+build": false} {
		v, err := Parse(s)
		if err != nil || v.IsPrerelease() != want {
			t.Errorf("Parse(%q).IsPrerelease() = %v, %v; want %v", s, v.IsPrerelease(), err, want)
		}
	}
}

// FuzzParse checks that strict parsing means canonical text: whatever
// Parse accepts prints back unchanged, and release parsing only narrows it.
func FuzzParse(f *testing.F) {
	for _, s := range []string{"0.1.0", "1.0.0-rc.1+build.5", "0.0.0-snapshot-abc", "01.0.0", "1.0.0-01", "v1.0.0"} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		v, err := Parse(s)
		if err != nil {
			if _, err := ParseRelease(s); err == nil {
				t.Fatalf("ParseRelease accepted %q, which Parse refuses", s)
			}

			return
		}

		if v.String() != s {
			t.Fatalf("Parse(%q).String() = %q", s, v.String())
		}

		again, err := Parse(v.String())
		if err != nil || Compare(again, v) != 0 || again != v {
			t.Fatalf("round trip of %q: %+v, %v", s, again, err)
		}

		if r, err := ParseRelease(s); err == nil && (r.Build != "" || r == (Version{Prerelease: r.Prerelease})) {
			t.Fatalf("ParseRelease accepted %q", s)
		}
	})
}
