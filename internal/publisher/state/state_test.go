package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/calver"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/policy"
)

var (
	manifestA = digest.FromBytes([]byte("manifest a"))
	manifestB = digest.FromBytes([]byte("manifest b"))
	manifestC = digest.FromBytes([]byte("manifest c"))
	contentA  = digest.FromBytes([]byte("content a"))
	contentB  = digest.FromBytes([]byte("content b"))
	contentC  = digest.FromBytes([]byte("content c"))
	noticeA   = digest.FromBytes([]byte("notice a"))
	licenseD  = digest.FromBytes([]byte("license text"))
	servedD   = digest.FromBytes([]byte("served"))
	catalogD  = digest.FromBytes([]byte("catalog"))
	tarball   = digest.FromBytes([]byte("tarball"))
)

func sample() *State {
	return &State{
		FormatVersion: FormatVersion,
		Source: Source{
			Kind: KindSchemaStore, Repository: SchemaStoreRepository, Commit: strings.Repeat("ab", 20), TarballDigest: tarball,
		},
		Recipe:  "schepherd-prepare/2+sourcemeta-jsonschema-1.2.3",
		Catalog: Catalog{Revision: "20260924.1432", Digest: catalogD, Size: 1234},
		Schemas: []Schema{
			{
				ID: "alpha", ContentDigest: contentA, NoticeDigest: noticeA, FirstRevision: "20260910.0300",
				ArtifactRevision: "20260917.0300", LastChangedRevision: "20260924.1432",
				License: LicenseDecision{
					Rules: []string{"schemastore"},
					Detections: []policy.Detection{{
						URL: "https://raw.githubusercontent.com/owner/repo/main/dep.json", Source: "github:owner/repo@" + strings.Repeat("c", 40),
						License: "MIT", LicenseFile: "LICENSE", LicenseDigest: licenseD, Verdict: policy.Allow,
					}},
					Redirects: []Redirect{{
						URL: "https://github.com/owner/repo/raw/main/dep.json", Target: "https://raw.githubusercontent.com/owner/repo/main/dep.json",
						Digest: servedD,
					}},
				},
				Entry: catalog.Entry{
					ID: "alpha", Name: "Alpha <config>", Description: "Line one.\nLine & two.",
					Dialect:   "https://json-schema.org/draft/2020-12/schema",
					FileMatch: []string{"alpha.json", "**/.alpha/*.json"},
					Artifact:  catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: manifestA, Size: 700},
					Provenance: &catalog.Provenance{
						Source: "https://www.schemastore.org/alpha.json", SourceDigest: contentA, License: "MIT",
					},
				},
			},
			{
				ID: "beta", ContentDigest: contentB, FirstRevision: "20260910.0300", LastChangedRevision: "20260910.0300",
				HeldSinceRevision: "20260917.0300", HeldReason: HeldRemovedUpstream,
				Entry: catalog.Entry{
					ID: "beta", Name: "Beta",
					Artifact:   catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: manifestB, Size: 650},
					Provenance: &catalog.Provenance{Source: "https://www.schemastore.org/beta.json"},
				},
			},
			{
				ID: "gamma", ContentDigest: contentC, FirstRevision: "20260910.0300", LastChangedRevision: "20260910.0300",
				ExcludedRevision: "20260924.1432",
				Entry: catalog.Entry{
					ID: "gamma", Name: "Gamma",
					Artifact:   catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: manifestC, Size: 600},
					Provenance: &catalog.Provenance{Source: "https://www.schemastore.org/gamma.json"},
				},
			},
		},
	}
}

const sampleEncoding = `{
  "formatVersion": 1,
  "source": {
    "kind": "schemastore",
    "repository": "https://github.com/SchemaStore/schemastore",
    "commit": "abababababababababababababababababababab",
    "tarballDigest": "TARBALL"
  },
  "recipe": "schepherd-prepare/2+sourcemeta-jsonschema-1.2.3",
  "catalog": {
    "revision": "20260924.1432",
    "digest": "CATALOG",
    "size": 1234
  },
  "schemas": [
    {
      "id": "alpha",
      "contentDigest": "CONTENTA",
      "noticeDigest": "NOTICEA",
      "firstRevision": "20260910.0300",
      "artifactRevision": "20260917.0300",
      "lastChangedRevision": "20260924.1432",
      "licenseDecision": {
        "rules": [
          "schemastore"
        ],
        "detections": [
          {
            "url": "https://raw.githubusercontent.com/owner/repo/main/dep.json",
            "source": "github:owner/repo@cccccccccccccccccccccccccccccccccccccccc",
            "license": "MIT",
            "licenseFile": "LICENSE",
            "licenseDigest": "LICENSED",
            "verdict": "allow"
          }
        ],
        "redirects": [
          {
            "url": "https://github.com/owner/repo/raw/main/dep.json",
            "target": "https://raw.githubusercontent.com/owner/repo/main/dep.json",
            "digest": "SERVED"
          }
        ]
      },
      "entry": {
        "artifact": {
          "digest": "MANIFESTA",
          "mediaType": "application/vnd.oci.image.manifest.v1+json",
          "size": 700
        },
        "description": "Line one.\nLine & two.",
        "dialect": "https://json-schema.org/draft/2020-12/schema",
        "fileMatch": [
          "alpha.json",
          "**/.alpha/*.json"
        ],
        "id": "alpha",
        "name": "Alpha <config>",
        "provenance": {
          "license": "MIT",
          "source": "https://www.schemastore.org/alpha.json",
          "sourceDigest": "CONTENTA"
        }
      }
    },
    {
      "id": "beta",
      "contentDigest": "CONTENTB",
      "firstRevision": "20260910.0300",
      "lastChangedRevision": "20260910.0300",
      "heldSinceRevision": "20260917.0300",
      "heldReason": "removed-upstream",
      "entry": {
        "artifact": {
          "digest": "MANIFESTB",
          "mediaType": "application/vnd.oci.image.manifest.v1+json",
          "size": 650
        },
        "id": "beta",
        "name": "Beta",
        "provenance": {
          "source": "https://www.schemastore.org/beta.json"
        }
      }
    },
    {
      "id": "gamma",
      "contentDigest": "CONTENTC",
      "firstRevision": "20260910.0300",
      "lastChangedRevision": "20260910.0300",
      "excludedRevision": "20260924.1432",
      "entry": {
        "artifact": {
          "digest": "MANIFESTC",
          "mediaType": "application/vnd.oci.image.manifest.v1+json",
          "size": 600
        },
        "id": "gamma",
        "name": "Gamma",
        "provenance": {
          "source": "https://www.schemastore.org/gamma.json"
        }
      }
    }
  ]
}
`

func expectedEncoding() string {
	return strings.NewReplacer(
		"TARBALL", tarball, "CATALOG", catalogD, "CONTENTA", contentA, "CONTENTB", contentB, "CONTENTC", contentC, "NOTICEA", noticeA,
		"MANIFESTA", manifestA, "MANIFESTB", manifestB, "MANIFESTC", manifestC, "LICENSED", licenseD, "SERVED", servedD,
	).Replace(sampleEncoding)
}

func mustEncode(t *testing.T, s *State) []byte {
	t.Helper()

	data, err := Encode(s)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

func TestEncodeIsCanonical(t *testing.T) {
	data := mustEncode(t, sample())
	if string(data) != expectedEncoding() {
		t.Fatalf("Encode() =\n%s\nwant\n%s", data, expectedEncoding())
	}

	parsed, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(parsed, sample()) {
		t.Errorf("Parse(Encode()) =\n%+v\nwant\n%+v", parsed, sample())
	}

	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatal(err)
	}

	schemas, _ := generic["schemas"].([]any)
	first, _ := schemas[0].(map[string]any)

	want, err := catalog.MarshalEntry(&sample().Schemas[0].Entry)
	if err != nil {
		t.Fatal(err)
	}

	got, err := json.Marshal(first["entry"])
	if err != nil {
		t.Fatal(err)
	}

	if !jsonEqual(t, got, want) {
		t.Errorf("entry %s differs from the catalog encoding %s", got, want)
	}
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()

	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}

	return reflect.DeepEqual(x, y)
}

func TestLocalSource(t *testing.T) {
	s := sample()
	s.Source = Source{Kind: KindLocal, Name: "fixtures"}

	data := mustEncode(t, s)
	if !bytes.Contains(data, []byte(`"source": {
    "kind": "local",
    "name": "fixtures"
  },`)) {
		t.Errorf("local source encoding:\n%s", data)
	}

	if _, err := Parse(data); err != nil {
		t.Fatal(err)
	}
}

// A decision that rests on redirects alone (a license a local source file
// declares for a document another URL served) is recorded, not dropped as
// an empty decision.
func TestDecisionOfRedirectsAloneIsRecorded(t *testing.T) {
	s := sample()
	s.Schemas[0].License = LicenseDecision{Redirects: s.Schemas[0].License.Redirects}

	data := mustEncode(t, s)
	if !bytes.Contains(data, []byte(`"licenseDecision": {
        "redirects": [`)) {
		t.Fatalf("the redirects are not recorded:\n%s", data)
	}

	parsed, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}

	if !parsed.Schemas[0].License.Equal(&s.Schemas[0].License) || parsed.Schemas[0].License.IsZero() {
		t.Errorf("license decision = %+v, want %+v", parsed.Schemas[0].License, s.Schemas[0].License)
	}
}

// A state written before decisions recorded redirects stays valid and
// canonical: a decision without redirects has no redirects member.
func TestDecisionWithoutRedirectsKeepsItsEncoding(t *testing.T) {
	s := sample()
	s.Schemas[0].License.Redirects = nil

	data := mustEncode(t, s)
	if bytes.Contains(data, []byte(`"redirects"`)) {
		t.Fatalf("a decision without redirects encodes them:\n%s", data)
	}

	parsed, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(parsed, s) {
		t.Errorf("Parse(Encode()) = %+v, want %+v", parsed, s)
	}
}

func TestLicenseDecisionsDifferInTheirRedirects(t *testing.T) {
	a := sample().Schemas[0].License
	b := sample().Schemas[0].License
	b.Redirects[0].Target = "https://raw.githubusercontent.com/owner/repo/main/moved.json"

	if a.Equal(&b) {
		t.Error("decisions with different redirect targets are equal")
	}

	b = sample().Schemas[0].License
	b.Redirects = nil

	if a.Equal(&b) {
		t.Error("a decision without its redirects equals the recorded one")
	}
}

func TestEmptyStateIsValid(t *testing.T) {
	s := sample()
	s.Schemas = nil

	data := mustEncode(t, s)
	if !bytes.Contains(data, []byte(`"schemas": []`)) {
		t.Errorf("no schemas:\n%s", data)
	}

	parsed, err := Parse(data)
	if err != nil || len(parsed.Schemas) != 0 || parsed.Schemas == nil {
		t.Errorf("Parse = %+v, %v", parsed, err)
	}
}

// edit applies a textual change to the canonical sample encoding.
func edit(t *testing.T, old, replacement string) []byte {
	t.Helper()

	data := expectedEncoding()
	if !strings.Contains(data, old) {
		t.Fatalf("the sample does not contain %q", old)
	}

	return []byte(strings.Replace(data, old, replacement, 1))
}

type invalidCase struct {
	name string
	doc  []byte
	msg  string
	kind fault.Kind
	// schema tells whether api/state.schema.json can express the rule.
	schema bool
}

func invalidCases(t *testing.T) []invalidCase {
	t.Helper()

	encoded := func(change func(s *State)) []byte {
		s := sample()
		change(s)

		var buf bytes.Buffer

		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")

		wire := wireState{FormatVersion: s.FormatVersion, Source: s.Source, Recipe: s.Recipe, Catalog: s.Catalog}
		for _, rec := range s.Schemas {
			entry, err := catalog.MarshalEntry(&rec.Entry)
			if err != nil {
				t.Fatal(err)
			}

			wire.Schemas = append(wire.Schemas, wireSchema{
				ID: rec.ID, ContentDigest: rec.ContentDigest, NoticeDigest: rec.NoticeDigest, FirstRevision: rec.FirstRevision,
				ArtifactRevision: rec.ArtifactRevision, LastChangedRevision: rec.LastChangedRevision,
				HeldSinceRevision: rec.HeldSinceRevision, HeldReason: rec.HeldReason, ExcludedRevision: rec.ExcludedRevision,
				LicenseDecision: rec.License, Entry: entry,
			})
		}

		if wire.Schemas == nil {
			wire.Schemas = []wireSchema{}
		}

		if err := enc.Encode(&wire); err != nil {
			t.Fatal(err)
		}

		return buf.Bytes()
	}

	integrity := func(schema bool, name string, doc []byte, msg string) invalidCase {
		return invalidCase{name: name, doc: doc, msg: msg, kind: fault.Integrity, schema: schema}
	}

	return []invalidCase{
		{name: "formatVersion 2", doc: edit(t, `"formatVersion": 1`, `"formatVersion": 2`), msg: "formatVersion 2", kind: fault.Usage, schema: true},
		{name: "formatVersion string", doc: edit(t, `"formatVersion": 1`, `"formatVersion": "1"`), msg: "formatVersion \"1\"", kind: fault.Usage, schema: true},
		{name: "formatVersion missing", doc: edit(t, "\"formatVersion\": 1,\n  ", ""), msg: "(missing)", kind: fault.Usage, schema: true},
		integrity(false, "not canonical: compact", []byte(strings.Join(strings.Fields(expectedEncoding()), "")), "canonical form"),
		integrity(false, "not canonical: no final newline", bytes.TrimSuffix([]byte(expectedEncoding()), []byte("\n")), "canonical form"),
		integrity(false, "not canonical: member order", edit(t, "\"kind\": \"schemastore\",\n    \"repository\": \"https://github.com/SchemaStore/schemastore\",",
			"\"repository\": \"https://github.com/SchemaStore/schemastore\",\n    \"kind\": \"schemastore\","), "canonical form"),
		integrity(true, "unknown member", edit(t, `"recipe":`, `"extra": 1,
  "recipe":`), "unknown field"),
		integrity(true, "case variant", edit(t, `"recipe":`, `"Recipe":`), "canonical form"),
		integrity(false, "duplicate member", edit(t, `"recipe":`, `"recipe": "x",
  "recipe":`), "duplicate object key"),
		integrity(true, "null description", edit(t, `"description": "Line one.\nLine & two.",`, `"description": null,`), "canonical form"),
		integrity(true, "empty description", edit(t, `"description": "Line one.\nLine & two.",`, `"description": "",`), "canonical form"),
		integrity(true, "empty heldSinceRevision", edit(t, `"lastChangedRevision": "20260924.1432",`,
			`"lastChangedRevision": "20260924.1432",
      "heldSinceRevision": "",`), "canonical form"),
		integrity(true, "removedUpstreamRevision is retired", edit(t, `"heldSinceRevision": "20260917.0300",`,
			`"removedUpstreamRevision": "20260917.0300",`), "unknown field"),
		integrity(true, "empty licenseDecision", edit(t, `"excludedRevision": "20260924.1432",`,
			`"excludedRevision": "20260924.1432",
      "licenseDecision": {},`), "canonical form"),
		integrity(true, "empty rules", edit(t, `"rules": [
          "schemastore"
        ],`, `"rules": [],`), "canonical form"),
		integrity(true, "unknown source kind", encoded(func(s *State) { s.Source.Kind = "git" }), "unknown kind"),
		integrity(true, "schemastore without commit", encoded(func(s *State) { s.Source.Commit = "" }), "40-hex commit"),
		integrity(true, "schemastore short commit", encoded(func(s *State) { s.Source.Commit = "abc" }), "40-hex commit"),
		integrity(true, "schemastore with name", encoded(func(s *State) { s.Source.Name = "x" }), "no name"),
		integrity(true, "http repository", encoded(func(s *State) { s.Source.Repository = "http://github.com/x" }), "https URL"),
		integrity(true, "repository with query", encoded(func(s *State) { s.Source.Repository = "https://github.com/x?y" }), "https URL"),
		integrity(true, "repository with credentials", encoded(func(s *State) { s.Source.Repository = "https://u@github.com/x" }), "https URL"),
		integrity(true, "repository with space", encoded(func(s *State) { s.Source.Repository = "https://github.com/x y" }), "https URL"),
		integrity(true, "repository without host", encoded(func(s *State) { s.Source.Repository = "https:///x" }), "https URL"),
		integrity(true, "bad tarball digest", encoded(func(s *State) { s.Source.TarballDigest = "sha256:abc" }), "tarballDigest"),
		integrity(true, "local with commit", encoded(func(s *State) { s.Source = Source{Kind: KindLocal, Name: "x", Commit: s.Source.Commit} }),
			"only a name"),
		integrity(true, "local without name", encoded(func(s *State) { s.Source = Source{Kind: KindLocal} }), "name must be"),
		integrity(true, "empty recipe", encoded(func(s *State) { s.Recipe = "" }), "recipe must be"),
		integrity(true, "recipe with a line break", encoded(func(s *State) { s.Recipe = "a\nb" }), "recipe must be"),
		integrity(true, "old revision grammar", encoded(func(s *State) { s.Catalog.Revision = "20260924.1" }), "invalid catalog revision"),
		integrity(true, "revision hour 24", encoded(func(s *State) { s.Catalog.Revision = "20260924.2400" }), "invalid catalog revision"),
		integrity(false, "impossible date", encoded(func(s *State) { s.Catalog.Revision = "20260230.1200" }), "invalid catalog revision"),
		integrity(true, "catalog digest", encoded(func(s *State) { s.Catalog.Digest = "sha256:x" }), "digest"),
		integrity(true, "catalog size 0", encoded(func(s *State) { s.Catalog.Size = 0 }), "size 0"),
		integrity(true, "catalog size too large", encoded(func(s *State) { s.Catalog.Size = 5 << 20 }), "size"),
		integrity(true, "bad content digest", encoded(func(s *State) { s.Schemas[0].ContentDigest = "md5:x" }), "contentDigest"),
		integrity(true, "bad notice digest", encoded(func(s *State) { s.Schemas[0].NoticeDigest = "sha256:abc" }), "noticeDigest"),
		integrity(true, "empty noticeDigest", edit(t, `"noticeDigest": "`+noticeA+`",`, `"noticeDigest": "",`), "canonical form"),
		integrity(false, "id differs from entry", encoded(func(s *State) { s.Schemas[0].ID = "alpha2" }), "the entry has id"),
		integrity(true, "no provenance", encoded(func(s *State) { s.Schemas[1].Entry.Provenance = nil }), "no provenance"),
		integrity(true, "bad first revision", encoded(func(s *State) { s.Schemas[0].FirstRevision = "2026" }), "firstRevision"),
		integrity(false, "first after last change", encoded(func(s *State) { s.Schemas[1].FirstRevision = "20260911.0000" }),
			"firstRevision <= lastChangedRevision"),
		integrity(false, "change after catalog", encoded(func(s *State) { s.Schemas[0].LastChangedRevision = "20260924.1433" }),
			"lastChangedRevision <= catalog.revision"),
		integrity(false, "held before change", encoded(func(s *State) { s.Schemas[1].HeldSinceRevision = "20260910.0300" }),
			"lastChangedRevision < heldSinceRevision"),
		integrity(false, "held after catalog", encoded(func(s *State) { s.Schemas[1].HeldSinceRevision = "20260925.0000" }),
			"heldSinceRevision <= catalog.revision"),
		integrity(true, "held without reason", encoded(func(s *State) { s.Schemas[1].HeldReason = "" }), "heldReason"),
		integrity(true, "reason without hold", encoded(func(s *State) { s.Schemas[0].HeldReason = HeldFetchFailed }), "heldSinceRevision"),
		integrity(true, "unknown held reason", encoded(func(s *State) { s.Schemas[1].HeldReason = "excluded" }), "heldReason"),
		integrity(true, "bad heldSinceRevision", encoded(func(s *State) { s.Schemas[1].HeldSinceRevision = "2026" }), "heldSinceRevision"),
		integrity(false, "excluded before change", encoded(func(s *State) { s.Schemas[2].ExcludedRevision = "20260910.0300" }),
			"lastChangedRevision < excludedRevision"),
		integrity(false, "excluded after catalog", encoded(func(s *State) { s.Schemas[2].ExcludedRevision = "20260925.0000" }),
			"excludedRevision <= catalog.revision"),
		integrity(false, "excluded and held", encoded(func(s *State) {
			s.Schemas[2].HeldSinceRevision, s.Schemas[2].HeldReason = "20260917.0300", HeldFetchFailed
		}), "cannot be held"),
		integrity(false, "artifact revision equal to last change", encoded(func(s *State) { s.Schemas[0].ArtifactRevision = "20260924.1432" }),
			"artifactRevision < lastChangedRevision"),
		integrity(false, "artifact revision before first", encoded(func(s *State) { s.Schemas[0].ArtifactRevision = "20260909.0300" }),
			"firstRevision <= artifactRevision"),
		integrity(true, "bad artifact revision", encoded(func(s *State) { s.Schemas[0].ArtifactRevision = "x" }), "artifactRevision"),
		integrity(true, "invalid rule id", encoded(func(s *State) { s.Schemas[0].License.Rules = []string{"Bad Rule"} }), "valid rule ID"),
		integrity(false, "unsorted rules", encoded(func(s *State) { s.Schemas[0].License.Rules = []string{"b", "a"} }), "sorted"),
		integrity(true, "refused detection", encoded(func(s *State) { s.Schemas[0].License.Detections[0].Verdict = policy.Review }),
			"verdict must be allow"),
		integrity(true, "detection without license", encoded(func(s *State) { s.Schemas[0].License.Detections[0].License = "" }),
			"license is required"),
		integrity(true, "detection without source", encoded(func(s *State) { s.Schemas[0].License.Detections[0].Source = "" }),
			"source must be"),
		integrity(true, "detection without license file", encoded(func(s *State) {
			s.Schemas[0].License.Detections[0].LicenseFile, s.Schemas[0].License.Detections[0].LicenseDigest = "", ""
		}), "license file"),
		integrity(true, "detection file without digest", encoded(func(s *State) { s.Schemas[0].License.Detections[0].NoticeFile = "NOTICE" }),
			"noticeFile and its digest"),
		integrity(true, "detection url", encoded(func(s *State) { s.Schemas[0].License.Detections[0].URL = "ftp://x/y" }), "http(s) URL"),
		integrity(false, "unsorted detections", encoded(func(s *State) {
			d := s.Schemas[0].License.Detections[0]
			s.Schemas[0].License.Detections = []policy.Detection{d, d}
		}), "sorted by url"),
		integrity(true, "empty redirects", edit(t, `"redirects": [
          {
            "url": "https://github.com/owner/repo/raw/main/dep.json",
            "target": "https://raw.githubusercontent.com/owner/repo/main/dep.json",
            "digest": "`+servedD+`"
          }
        ]`, `"redirects": []`), "canonical form"),
		integrity(true, "redirect without digest", encoded(func(s *State) { s.Schemas[0].License.Redirects[0].Digest = "" }), "digest"),
		integrity(true, "redirect digest", encoded(func(s *State) { s.Schemas[0].License.Redirects[0].Digest = "sha256:abc" }), "digest"),
		integrity(true, "redirect url scheme", encoded(func(s *State) { s.Schemas[0].License.Redirects[0].URL = "ftp://x/y" }), "url \"ftp://x/y\""),
		integrity(true, "redirect url fragment", encoded(func(s *State) { s.Schemas[0].License.Redirects[0].URL += "#/definitions/x" }),
			"without credentials or fragment"),
		integrity(true, "redirect target credentials", encoded(func(s *State) {
			s.Schemas[0].License.Redirects[0].Target = "https://u:p@raw.githubusercontent.com/owner/repo/main/dep.json"
		}), "target"),
		integrity(true, "redirect target with space", encoded(func(s *State) { s.Schemas[0].License.Redirects[0].Target = "https://x/a b" }), "target"),
		integrity(false, "redirect to itself", encoded(func(s *State) {
			s.Schemas[0].License.Redirects[0].Target = s.Schemas[0].License.Redirects[0].URL
		}), "target must differ from url"),
		integrity(false, "unsorted redirects", encoded(func(s *State) {
			r := s.Schemas[0].License.Redirects[0]
			s.Schemas[0].License.Redirects = []Redirect{r, r}
		}), "redirects must be sorted by url"),
		integrity(false, "unsorted schemas", encoded(func(s *State) { s.Schemas[0], s.Schemas[1] = s.Schemas[1], s.Schemas[0] }), "sorted"),
		integrity(false, "an excluded schema shares its id", encoded(func(s *State) {
			s.Schemas[2].ID, s.Schemas[2].Entry.ID = "beta", "beta"
			s.Schemas[1], s.Schemas[2] = s.Schemas[2], s.Schemas[1]
		}), "duplicate schema id"),
		integrity(false, "invalid excluded entry", encoded(func(s *State) { s.Schemas[2].Entry.Name = "a\u0007" }), "forbidden character"),
		integrity(false, "duplicate id", encoded(func(s *State) {
			s.Schemas[1] = s.Schemas[0]
		}), "duplicate schema id"),
		integrity(true, "invalid entry name", encoded(func(s *State) { s.Schemas[1].Entry.Name = "a\u0007" }), "forbidden character"),
		integrity(true, "entry artifact media type", encoded(func(s *State) {
			s.Schemas[1].Entry.Artifact.MediaType = "application/json"
		}), "mediaType"),
		integrity(false, "entries disagree on a size", encoded(func(s *State) {
			s.Schemas[1].Entry.Artifact = catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: manifestA, Size: 1}
		}), "sizes"),
		integrity(false, "not JSON", []byte("{"), "malformed"),
	}
}

func TestParseRejects(t *testing.T) {
	for _, tc := range invalidCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.doc)
			if err == nil {
				t.Fatalf("Parse accepted:\n%s", tc.doc)
			}

			if fault.KindOf(err) != tc.kind || !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("err = %v (kind %s), want %s containing %q", err, fault.KindOf(err), tc.kind, tc.msg)
			}
		})
	}
}

func TestEncodeValidates(t *testing.T) {
	s := sample()
	s.Schemas[0].LastChangedRevision = "20270101.0000"

	if _, err := Encode(s); fault.KindOf(err) != fault.Integrity {
		t.Errorf("Encode of an invalid state = %v", err)
	}

	if err := Save(filepath.Join(t.TempDir(), "state.json"), s); err == nil {
		t.Error("Save wrote an invalid state")
	}
}

func TestLookupAndEntries(t *testing.T) {
	s := sample()

	if rec, ok := s.Lookup("beta"); !ok || rec.ID != "beta" || !rec.Held() || rec.Excluded() || rec.ArtifactSince() != "20260910.0300" {
		t.Errorf("Lookup(beta) = %+v, %v", rec, ok)
	}

	if rec, ok := s.Lookup("alpha"); !ok || rec.Held() || rec.Excluded() || rec.ArtifactSince() != "20260917.0300" {
		t.Errorf("Lookup(alpha) = %+v, %v", rec, ok)
	}

	if rec, ok := s.Lookup("gamma"); !ok || !rec.Excluded() || rec.Held() {
		t.Errorf("Lookup(gamma) = %+v, %v", rec, ok)
	}

	if _, ok := s.Lookup("delta"); ok {
		t.Error("Lookup found a missing id")
	}

	entries := s.Entries()
	if len(entries) != 2 || entries[0].ID != "alpha" || entries[1].Artifact.Digest != manifestB {
		t.Errorf("Entries() = %+v; an excluded schema is not in the catalog", entries)
	}
}

func TestLoadAndSave(t *testing.T) {
	dir := t.TempDir()

	missing, err := Load(filepath.Join(dir, "state.json"))
	if missing != nil || err != nil {
		t.Fatalf("a missing file = %v, %v; want no state and no error", missing, err)
	}

	path := filepath.Join(dir, "catalog", "state.json")
	if err := Save(path, sample()); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil || string(data) != expectedEncoding() {
		t.Fatalf("saved bytes differ: %v\n%s", err, data)
	}

	loaded, err := Load(path)
	if err != nil || !reflect.DeepEqual(loaded, sample()) {
		t.Fatalf("Load = %+v, %v", loaded, err)
	}

	changed := sample()
	changed.Catalog.Size = 99

	if err := Save(path, changed); err != nil {
		t.Fatal(err)
	}

	if loaded, err := Load(path); err != nil || loaded.Catalog.Size != 99 {
		t.Errorf("overwritten state = %+v, %v", loaded, err)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Errorf("Save left temporary files: %v", entries)
	}

	if _, err := Load(dir); fault.KindOf(err) != fault.Usage {
		t.Errorf("a directory as the state file = %v", err)
	}

	if err := os.WriteFile(path, []byte(`{"formatVersion":2}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), path) {
		t.Errorf("a newer format = %v", err)
	}

	if err := os.WriteFile(path, []byte(`{"formatVersion":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); fault.KindOf(err) != fault.Integrity {
		t.Errorf("an invalid state = %v", err)
	}

	if err := Save(dir, sample()); err == nil {
		t.Error("Save replaced a directory")
	}
}

func TestRevisionsUseTheCatalogGrammar(t *testing.T) {
	s := sample()

	for _, rec := range s.Schemas {
		for _, rev := range []string{rec.FirstRevision, rec.ArtifactSince(), rec.LastChangedRevision} {
			if _, err := calver.ParseRevision(rev); err != nil {
				t.Errorf("sample revision %q: %v", rev, err)
			}
		}
	}

	s.Catalog.Revision = "20260924.1432"
	s.Schemas[0].FirstRevision, s.Schemas[0].ArtifactRevision = s.Catalog.Revision, ""

	if _, err := Encode(s); err != nil {
		t.Errorf("a schema first published by the current revision: %v", err)
	}

	if !errors.Is(sampleRevisionError(), calver.ErrInvalid) {
		t.Error("an invalid revision is not reported as calver.ErrInvalid")
	}
}

func sampleRevisionError() error {
	s := sample()
	s.Catalog.Revision = "20260924.1"

	return s.Validate()
}
