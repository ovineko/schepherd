// Package sources_test checks that the publisher's committed source
// descriptions in this directory load with the publisher's own loaders and
// encode the reviewed decisions.
package sources_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/publisher/ids"
	"github.com/ovineko/schepherd/internal/publisher/policy"
	"github.com/ovineko/schepherd/internal/publisher/upstream"
)

// sampleCommit stands for any SchemaStore commit in URLs; the imported
// commit itself changes with every weekly catalog update.
const sampleCommit = "05b037b7a2b68ead6893c857db2594bcad9951d4"

var importedCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

func TestSchemaStoreSource(t *testing.T) {
	cfg, err := upstream.LoadSchemaStoreConfig("schemastore.toml")
	if err != nil {
		t.Fatal(err)
	}

	if !importedCommit.MatchString(cfg.Commit) {
		t.Fatalf("schemastore.toml commit %q is not 40 lowercase hex digits", cfg.Commit)
	}

	want := upstream.SchemaStoreConfig{
		Commit:          cfg.Commit,
		TarballBaseURL:  "https://codeload.github.com/SchemaStore/schemastore/tar.gz",
		MaxTarballBytes: 64 << 20,
		Dependencies: upstream.DependencyLimits{
			MaxDepth:         8,
			MaxPerSchema:     64,
			MaxDocumentBytes: 16 << 20,
			MaxTotalBytes:    256 << 20,
		},
	}

	if *cfg != want {
		t.Fatalf("schemastore.toml = %#v", cfg)
	}
}

func TestIDOverrides(t *testing.T) {
	overrides, err := ids.LoadOverrides("ids.json")
	if err != nil {
		t.Fatal(err)
	}

	if len(overrides) != 0 {
		t.Fatalf("ids.json = %v", overrides)
	}
}

func TestLicensePolicy(t *testing.T) {
	p, err := policy.Load("licenses.toml")
	if err != nil {
		t.Fatal(err)
	}

	const reason = "SchemaStore repository LICENSE: Apache-2.0, verified at commit 05b037b7a2b68ead6893c857db2594bcad9951d4"

	allowed := map[string]string{
		"https://www.schemastore.org/tsconfig.json":                                                  "schemastore",
		"https://json.schemastore.org/package.json":                                                  "schemastore",
		"https://raw.githubusercontent.com/SchemaStore/schemastore/master/src/schemas/json/foo.json": "schemastore-raw",
	}

	for source, rule := range allowed {
		d := p.Decide(source, []string{"http://json-schema.org/draft-07/schema#"})

		if d.Decision != policy.Allow || d.License != "Apache-2.0" || d.RuleID != rule {
			t.Errorf("Decide(%s) = %#v", source, d)
		}

		if d.Reason != reason+" (rule "+rule+")" {
			t.Errorf("Decide(%s).Reason = %q", source, d.Reason)
		}

		if !strings.Contains(d.Notice, "Apache") || !strings.Contains(d.Notice, "NOTICE file") {
			t.Errorf("Decide(%s).Notice = %q", source, d.Notice)
		}
	}

	held := []string{
		"https://raw.githubusercontent.com/other/repo/main/schema.json",
		"https://json-schema.org/draft-07/schema",
		"https://docs.renovatebot.com/renovate-schema.json",
		"https://www.schemastore.org/tsconfig.json?v=1",
	}

	for _, source := range held {
		if d := p.Decide(source, nil); d.Decision != policy.Review {
			t.Errorf("Decide(%s) = %#v, want review", source, d)
		}
	}

	for _, file := range []string{"partial-pyright", "partial-mypy", "rustfmt", "cargo-config"} {
		for _, source := range thirdPartySpellings(file) {
			if d := p.Decide(source, nil); d.Decision != policy.Review || !strings.HasSuffix(d.RuleID, "third-party-content") {
				t.Errorf("Decide(%s) = %#v, want review by a third-party-content rule", source, d)
			}
		}
	}

	// Rules take precedence over automatic detection, whatever it finds: a
	// spelling no rule matches would be decided by the repository license.
	apache := func(string) (policy.Finding, bool) {
		return policy.Finding{
			Source: "github:schemastore/schemastore@" + sampleCommit, License: "Apache-2.0", Notice: "Apache License",
		}, true
	}

	for _, file := range []string{"partial-pyright", "partial-mypy", "rustfmt", "cargo-config"} {
		for _, source := range thirdPartySpellings(file) {
			if d := p.DecideWith(source, nil, apache, nil); d.Decision != policy.Review || !strings.HasSuffix(d.RuleID, "third-party-content") {
				t.Errorf("DecideWith(%s) = %#v, want review by a third-party-content rule", source, d)
			}
		}
	}

	for _, dep := range thirdPartySpellings("partial-pyright") {
		d := p.Decide("https://www.schemastore.org/pyproject.json", []string{dep})
		if d.Decision != policy.Review || !strings.HasSuffix(d.RuleID, "third-party-content") {
			t.Errorf("pyproject embedding %s = %#v", dep, d)
		}
	}
}

// thirdPartySpellings lists URLs under which SchemaStore serves, or a $ref can
// name, the schema file <name>.json.
func thirdPartySpellings(name string) []string {
	return []string{
		"https://www.schemastore.org/" + name + ".json",
		"https://www.schemastore.org/" + name,
		"https://json.schemastore.org/" + name + ".json",
		"https://json.schemastore.org/" + name,
		"https://www.schemastore.org/schemas/json/" + name + ".json",
		"https://json.schemastore.org/schemas/json/" + name + ".json",
		"http://www.schemastore.org/" + name + ".json",
		"https://www.schemastore.org/" + name + ".json?v=1",
		"https://WWW.SchemaStore.org:443/" + name + ".json#/definitions/x",
		"https://raw.githubusercontent.com/SchemaStore/schemastore/master/src/schemas/json/" + name + ".json",
		"https://raw.githubusercontent.com/SchemaStore/schemastore/refs/heads/master/src/schemas/json/" + name + ".json",
		"https://raw.githubusercontent.com/SchemaStore/schemastore/" + sampleCommit + "/src/schemas/json/" + name + ".json",
		"https://raw.githubusercontent.com/schemastore/SCHEMASTORE/main/src/schemas/json/" + name + ".json",
		"https://github.com/SchemaStore/schemastore/raw/master/src/schemas/json/" + name + ".json",
		"https://github.com/schemastore/schemastore/blob/refs/heads/master/src/schemas/json/" + name + ".json",
	}
}
