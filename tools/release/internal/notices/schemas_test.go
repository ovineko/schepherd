package notices

import (
	"reflect"
	"strings"
	"testing"
)

func TestSchemaGroupsFollowTheLicenseDecisions(t *testing.T) {
	groups, err := schemaGroups(fixtureCatalog(t), fixtureRules(), fixtureSchemaStore())
	if err != nil {
		t.Fatal(err)
	}

	type summary struct {
		Heading string
		IDs     []string
		Details []string
	}

	got := make([]summary, 0, len(groups))
	for _, g := range groups {
		got = append(got, summary{Heading: g.heading, IDs: g.ids, Details: g.details})
	}

	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	want := []summary{
		{Heading: "SchemaStore repository (Apache-2.0)", IDs: []string{"alpha", "beta"}},
		{
			Heading: "GitHub repository `acme/lib` (MIT)", IDs: []string{"beta", "epsilon"},
			Details: []string{"`LICENSE` at commit `" + a + "`", "`LICENSE` at commit `" + b + "`"},
		},
		{
			Heading: "npm package `@scope/pkg` (ISC)", IDs: []string{"epsilon"},
			Details: []string{"`LICENSE.md` at version `1.2.3` with the notice file `NOTICE`"},
		},
		{Heading: "Rule `gone`", IDs: []string{"gamma"}},
		{Heading: "Rule `vendor` (MIT)", IDs: []string{"beta"}},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("groups:\n%+v\nwant\n%+v", got, want)
	}

	if text := strings.Join(groups[0].text, "\n"); !strings.Contains(text, "at commit `"+fixtureCommit+"`") ||
		!strings.Contains(text, "the rules `schemastore` and `schemastore-raw`") || !strings.HasSuffix(text, "```text\nExample Store\nCopyright 2015 Example Contributors\n```") {
		t.Errorf("SchemaStore text:\n%s", text)
	}

	if text := strings.Join(groups[3].text, "\n"); !strings.Contains(text, "no longer in `sources/licenses.toml`") {
		t.Errorf("text of a rule that is gone:\n%s", text)
	}

	if text := strings.Join(groups[4].text, "\n"); text != "Allowed by the rule `vendor` of `sources/licenses.toml`: the vendor publishes its schemas under MIT." {
		t.Errorf("rule text:\n%s", text)
	}

	again, err := schemaGroups(fixtureCatalog(t), fixtureRules(), fixtureSchemaStore())
	if err != nil || !reflect.DeepEqual(again, groups) {
		t.Errorf("a second grouping differs: %v", err)
	}
}

func TestSchemaGroupsOfDeclaredLicenses(t *testing.T) {
	st := fixtureState(t, true,
		fixtureSchema{id: "one", license: "MIT"},
		fixtureSchema{id: "three", license: "Apache-2.0"},
		fixtureSchema{id: "two", license: "MIT"},
	)

	groups, err := schemaGroups(st, fixtureRules(), fixtureSchemaStore())
	if err != nil {
		t.Fatal(err)
	}

	if len(groups) != 2 || groups[0].heading != "Declared by the source (Apache-2.0)" || !reflect.DeepEqual(groups[0].ids, []string{"three"}) ||
		groups[1].heading != "Declared by the source (MIT)" || !reflect.DeepEqual(groups[1].ids, []string{"one", "two"}) {
		t.Errorf("groups = %+v %+v", groups[0], groups[1])
	}
}

func TestSchemaGroupsCheckTheSchemaStoreRules(t *testing.T) {
	st := fixtureCatalog(t)

	for name, mutate := range map[string]func(map[string]policyRule, *schemaStoreData){
		"missing rule":      func(_ map[string]policyRule, ss *schemaStoreData) { ss.Rules = append(ss.Rules, "nope") },
		"not an allow rule": func(_ map[string]policyRule, ss *schemaStoreData) { ss.Rules = append(ss.Rules, "held-back") },
		"different licenses": func(r map[string]policyRule, _ *schemaStoreData) {
			r["schemastore-raw"] = policyRule{Decision: "allow", License: "MIT"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			rules, ss := fixtureRules(), fixtureSchemaStore()
			mutate(rules, &ss)

			if _, err := schemaGroups(st, rules, ss); err == nil || !strings.Contains(err.Error(), DataFile) {
				t.Errorf("err = %v, want an error naming %s", err, DataFile)
			}
		})
	}
}

func TestCodeSpanFencesBackticks(t *testing.T) {
	for in, want := range map[string]string{"plain": "`plain`", "a`b": "``a`b``", "`x": "`` `x ``"} {
		if got := code(in); got != want {
			t.Errorf("code(%q) = %q, want %q", in, got, want)
		}
	}
}
