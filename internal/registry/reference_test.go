package registry

import (
	"errors"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

var testDigest = digest.FromBytes([]byte("{}"))

func TestParseRepository(t *testing.T) {
	valid := []struct {
		in   string
		want Repository
	}{
		{"ghcr.io/org/schemas", Repository{Host: "ghcr.io", Path: "org/schemas"}},
		{"localhost:5000/schemas", Repository{Host: "localhost:5000", Path: "schemas"}},
		{"127.0.0.1:5000/a/b/c", Repository{Host: "127.0.0.1:5000", Path: "a/b/c"}},
		{"[::1]:5000/a/b", Repository{Host: "[::1]:5000", Path: "a/b"}},
		{"registry.example/a-b_c.d/e__f", Repository{Host: "registry.example", Path: "a-b_c.d/e__f"}},
	}

	for _, tc := range valid {
		got, err := ParseRepository(tc.in)
		if err != nil {
			t.Errorf("ParseRepository(%q): %v", tc.in, err)

			continue
		}

		if got != tc.want {
			t.Errorf("ParseRepository(%q) = %+v, want %+v", tc.in, got, tc.want)
		}

		if got.String() != tc.in {
			t.Errorf("ParseRepository(%q).String() = %q", tc.in, got.String())
		}
	}

	invalid := []struct {
		in      string
		message string
	}{
		{"", "invalid repository"},
		{"ghcr.io", "missing registry or repository"},
		{"ghcr.io/", "invalid repository"},
		{"ghcr.io/org/schemas:1", "must not include a tag or digest"},
		{"ghcr.io/org/schemas:", "must not include a tag or digest"},
		{"ghcr.io/org/schemas@" + testDigest, "must not include a tag or digest"},
		{"ghcr.io/Org/schemas", "upper-case"},
		{"GHCR.io/org/schemas", "upper-case letters are not allowed; registry hosts and repository paths must be lower case"},
		{"https://ghcr.io/org/schemas", "invalid repository"},
		{"ghcr.io//schemas", "invalid repository"},
		{"ghcr.io/schemas/", "invalid repository"},
		{"ghcr.io/a b", "invalid repository"},
		{"user@ghcr.io/a", "invalid repository"},
		{"localhost:0/a", "the port must be between 1 and 65535"},
		{"localhost:65536/a", "the port must be between 1 and 65535"},
		{"localhost:abc/a", "invalid registry"},
	}

	for _, tc := range invalid {
		_, err := ParseRepository(tc.in)
		if err == nil {
			t.Errorf("ParseRepository(%q) succeeded", tc.in)

			continue
		}

		requireKind(t, err, fault.Usage)
		requireMessage(t, err, tc.message, `"`+tc.in+`"`)
	}
}

func TestParseReference(t *testing.T) {
	repo := Repository{Host: "ghcr.io", Path: "org/schemas"}
	sha512 := "sha512:" + strings.Repeat("a", 128)

	valid := []struct {
		in   string
		want Reference
	}{
		{"ghcr.io/org/schemas:catalog-20260101.1", Reference{Repository: repo, Tag: "catalog-20260101.1"}},
		{"ghcr.io/org/schemas:V1.0_rc", Reference{Repository: repo, Tag: "V1.0_rc"}},
		{"ghcr.io/org/schemas@" + testDigest, Reference{Repository: repo, Digest: testDigest}},
		{"localhost:5000/s:latest", Reference{Repository: Repository{Host: "localhost:5000", Path: "s"}, Tag: "latest"}},
		{"localhost:5000/s@" + testDigest, Reference{Repository: Repository{Host: "localhost:5000", Path: "s"}, Digest: testDigest}},
	}

	for _, tc := range valid {
		got, err := ParseReference(tc.in)
		if err != nil {
			t.Errorf("ParseReference(%q): %v", tc.in, err)

			continue
		}

		if got != tc.want {
			t.Errorf("ParseReference(%q) = %+v, want %+v", tc.in, got, tc.want)
		}

		if got.String() != tc.in {
			t.Errorf("ParseReference(%q).String() = %q", tc.in, got.String())
		}
	}

	invalid := []struct {
		in      string
		message string
	}{
		{"", "expected host/path:tag"},
		{"ghcr.io/org/schemas", "expected host/path:tag"},
		{"localhost:5000", "expected host/path:tag"},
		{"ghcr.io/org/schemas:", "empty tag"},
		{"ghcr.io/org/schemas:1@" + testDigest, "must not combine a tag and a digest"},
		{"ghcr.io/org/schemas@" + sha512, "unsupported digest algorithm"},
		{"ghcr.io/org/schemas@blake3:" + strings.Repeat("a", 64), "unsupported digest algorithm"},
		{"ghcr.io/org/schemas@sha256:abc", "invalid digest"},
		{"ghcr.io/org/schemas@" + strings.ToUpper(testDigest), "invalid digest"},
		{"ghcr.io/org/schemas@", "invalid digest"},
		{"ghcr.io/org/schemas:-bad", "invalid tag"},
		{"ghcr.io/org/schemas:" + strings.Repeat("a", 129), "invalid tag"},
		{"ghcr.io/Org/schemas:1", "upper-case"},
		{"ghcr.io:1", "expected host/path:tag"},
		{"localhost:5000@" + testDigest, "missing registry or repository"},
		{"ghcr.io/org/schemas@" + testDigest + "@" + testDigest, "invalid digest"},
	}

	for _, tc := range invalid {
		_, err := ParseReference(tc.in)
		if err == nil {
			t.Errorf("ParseReference(%q) succeeded", tc.in)

			continue
		}

		requireKind(t, err, fault.Usage)
		requireMessage(t, err, tc.message, `"`+tc.in+`"`)
	}
}

func FuzzParseRepository(f *testing.F) {
	for _, seed := range []string{"ghcr.io/org/schemas", "localhost:5000/a", "[::1]:5000/a/b", "a/b:c", "", "ghcr.io/A"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		repo, err := ParseRepository(s)
		if err != nil {
			if fault.KindOf(err) != fault.Usage {
				t.Fatalf("ParseRepository(%q) error kind %s", s, fault.KindOf(err))
			}

			return
		}

		if repo.String() != s {
			t.Fatalf("ParseRepository(%q).String() = %q", s, repo.String())
		}

		if repo.Host == "" || repo.Path == "" || s != strings.ToLower(s) {
			t.Fatalf("ParseRepository(%q) accepted %+v", s, repo)
		}
	})
}

func FuzzParseReference(f *testing.F) {
	for _, seed := range []string{"ghcr.io/org/schemas:1", "ghcr.io/org/schemas@" + testDigest, "a/b:c@" + testDigest, "a:1/b:", "", "x/y@sha512:00"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		ref, err := ParseReference(s)
		if err != nil {
			if fault.KindOf(err) != fault.Usage {
				t.Fatalf("ParseReference(%q) error kind %s", s, fault.KindOf(err))
			}

			return
		}

		if ref.String() != s {
			t.Fatalf("ParseReference(%q).String() = %q", s, ref.String())
		}

		if (ref.Tag == "") == (ref.Digest == "") {
			t.Fatalf("ParseReference(%q) = %+v, want exactly one of tag and digest", s, ref)
		}

		if ref.Digest != "" && digest.Validate(ref.Digest) != nil {
			t.Fatalf("ParseReference(%q) accepted digest %q", s, ref.Digest)
		}

		again, err := ParseReference(ref.String())
		if err != nil || again != ref {
			t.Fatalf("round trip of %q: %+v, %v", s, again, err)
		}
	})
}

func TestValidateHost(t *testing.T) {
	for _, host := range []string{"ghcr.io", "localhost", "localhost:1", "localhost:65535", "127.0.0.1:5000", "[::1]", "[fe80::1]:5000"} {
		if err := ValidateHost(host); err != nil {
			t.Errorf("ValidateHost(%q): %v", host, err)
		}

		if _, err := ParseRepository(host + "/x"); err != nil {
			t.Errorf("ParseRepository(%q): %v", host+"/x", err)
		}
	}

	invalid := []struct {
		host    string
		message string
	}{
		{"", "invalid registry host"},
		{"GHCR.io", "invalid registry host"},
		{"-r.example", "invalid registry host"},
		{"r_example", "invalid registry host"},
		{"localhost:", "invalid registry host"},
		{"localhost:0", `host "localhost:0": the port must be between 1 and 65535`},
		{"localhost:99999", "the port must be between 1 and 65535"},
		{"[1.2.3.4]:5000", `host "[1.2.3.4]:5000": "1.2.3.4" is not an IPv6 address`},
		{"[:]", `":" is not an IPv6 address`},
	}

	for _, tc := range invalid {
		err := ValidateHost(tc.host)
		if err == nil || !strings.Contains(err.Error(), tc.message) {
			t.Errorf("ValidateHost(%q) = %v, want %q", tc.host, err, tc.message)
		}

		if strings.HasPrefix(tc.message, "invalid") != errors.Is(err, ErrInvalidHost) {
			t.Errorf("ValidateHost(%q) = %v: ErrInvalidHost only for syntax errors", tc.host, err)
		}

		if _, err := ParseRepository(tc.host + "/x"); err == nil {
			t.Errorf("ParseRepository accepts %q", tc.host+"/x")
		}
	}

	for _, path := range []string{"x", "a/b", "a-b_c.d/e__f"} {
		if err := ValidatePath(path); err != nil {
			t.Errorf("ValidatePath(%q): %v", path, err)
		}
	}

	for _, path := range []string{"", "/x", "x/", "a//b", "A", "a b", "a:b"} {
		if ValidatePath(path) == nil {
			t.Errorf("ValidatePath(%q) succeeded", path)
		}
	}
}
