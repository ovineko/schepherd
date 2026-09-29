package notices

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/publisher/policy"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

const (
	fixtureRevision = "20260926.0300"
	fixtureCommit   = "0123456789abcdef0123456789abcdef01234567"
	fixtureNotice   = "Example Store\nCopyright 2015 Example Contributors\n"
)

type fixtureSchema struct {
	id         string
	license    string
	rules      []string
	detections []policy.Detection
	excluded   bool
	held       bool
}

func digestOf(n int) string {
	return fmt.Sprintf("sha256:%064x", n)
}

// fixtureState builds a valid state with the schemas, which must be sorted
// by ID. A local state has no SchemaStore commit.
func fixtureState(t *testing.T, local bool, schemas ...fixtureSchema) *state.State {
	t.Helper()

	st := &state.State{
		FormatVersion: state.FormatVersion,
		Source: state.Source{
			Kind: state.KindSchemaStore, Repository: state.SchemaStoreRepository, Commit: fixtureCommit, TarballDigest: digestOf(1),
		},
		Recipe:  "test-recipe",
		Catalog: state.Catalog{Revision: fixtureRevision, Digest: digestOf(2), Size: 1234},
	}

	if local {
		st.Source = state.Source{Kind: state.KindLocal, Name: "fixture"}
	}

	for i, s := range schemas {
		rec := state.Schema{
			ID: s.id, ContentDigest: digestOf(100 + i), FirstRevision: "20260901.0300", LastChangedRevision: "20260901.0300",
			License: state.LicenseDecision{Rules: s.rules, Detections: s.detections},
			Entry: catalog.Entry{
				ID: s.id, Name: strings.ToUpper(s.id),
				Artifact:   catalog.Descriptor{MediaType: catalog.ManifestMediaType, Digest: digestOf(200 + i), Size: 500},
				Provenance: &catalog.Provenance{Source: "https://json.schemastore.org/" + s.id + ".json", License: s.license},
			},
		}

		if s.excluded {
			rec.ExcludedRevision = fixtureRevision
		}

		if s.held {
			rec.HeldSinceRevision, rec.HeldReason = "20260915.0300", state.HeldFetchFailed
		}

		st.Schemas = append(st.Schemas, rec)
	}

	if err := st.Validate(); err != nil {
		t.Fatal(err)
	}

	return st
}

func detection(source, license, licenseFile, noticeFile string) policy.Detection {
	d := policy.Detection{
		URL: "https://example.test/" + source, Source: source, License: license, LicenseFile: licenseFile, LicenseDigest: digestOf(300),
		Verdict: policy.Allow,
	}

	if noticeFile != "" {
		d.NoticeFile, d.NoticeDigest = noticeFile, digestOf(301)
	}

	return d
}

func fixtureRules() map[string]policyRule {
	return map[string]policyRule{
		"schemastore":     {ID: "schemastore", Decision: "allow", License: "Apache-2.0", Reason: "SchemaStore files"},
		"schemastore-raw": {ID: "schemastore-raw", Decision: "allow", License: "Apache-2.0", Reason: "SchemaStore files on GitHub"},
		"vendor":          {ID: "vendor", Decision: "allow", License: "MIT", Reason: "the vendor publishes its schemas under MIT."},
		"held-back":       {ID: "held-back", Decision: "review", Reason: "needs review"},
	}
}

func fixtureSchemaStore() schemaStoreData {
	return schemaStoreData{Rules: []string{"schemastore", "schemastore-raw"}, Notice: fixtureNotice}
}

// fixtureCatalog has a schema from SchemaStore, one that also embeds a
// vendor schema and a detected repository, one allowed by a rule that is
// gone, an excluded one and a held one with detections of the same
// repository at another commit and of an npm package.
func fixtureCatalog(t *testing.T) *state.State {
	t.Helper()

	repo := func(commit string) policy.Detection {
		return detection("github:acme/lib@"+strings.Repeat(commit, 40), "MIT", "LICENSE", "")
	}

	return fixtureState(t, false,
		fixtureSchema{id: "alpha", license: "Apache-2.0", rules: []string{"schemastore"}},
		fixtureSchema{id: "beta", license: "Apache-2.0 AND MIT", rules: []string{"schemastore-raw", "vendor"}, detections: []policy.Detection{repo("a")}},
		fixtureSchema{id: "delta", license: "Apache-2.0", rules: []string{"schemastore"}, excluded: true},
		fixtureSchema{
			id: "epsilon", license: "ISC AND MIT", held: true,
			detections: []policy.Detection{repo("b"), detection("npm:@scope/pkg@1.2.3", "ISC", "LICENSE.md", "NOTICE")},
		},
		fixtureSchema{id: "gamma", license: "MIT", rules: []string{"gone"}},
	)
}
