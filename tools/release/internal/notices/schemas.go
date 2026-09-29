package notices

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/policy"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

// Kinds of schema groups, in the order the notices list them.
const (
	groupSchemaStore = iota
	groupDetected
	groupRule
	groupDeclared
)

// schemaGroup is one source of schema content under one license, with the
// schemas of the catalog that contain content from it.
type schemaGroup struct {
	heading string
	text    []string
	details []string
	ids     []string
	kind    int
	key     string
}

// schemaGroups groups the schemas of the current catalog by the sources of
// their content that the recorded license decisions name: the SchemaStore
// repository, each repository or package whose license automatic detection
// found, each other policy rule, and the license a source description
// declares when the decision names nothing. A schema is in every group of
// its content; excluded schemas are not in the catalog.
func schemaGroups(st *state.State, rules map[string]policyRule, ss schemaStoreData) ([]*schemaGroup, error) {
	schemaStore, err := schemaStoreGroup(st, rules, ss)
	if err != nil {
		return nil, err
	}

	type groupKey struct {
		kind int
		key  string
	}

	groups := map[groupKey]*schemaGroup{{schemaStore.kind, schemaStore.key}: schemaStore}

	add := func(g *schemaGroup, id string) {
		if existing, ok := groups[groupKey{g.kind, g.key}]; ok {
			existing.details = appendNew(existing.details, g.details...)
			g = existing
		} else {
			groups[groupKey{g.kind, g.key}] = g
		}

		g.ids = appendNew(g.ids, id)
	}

	for i := range st.Schemas {
		rec := &st.Schemas[i]
		if rec.Excluded() {
			continue
		}

		for _, id := range rec.License.Rules {
			if slices.Contains(ss.Rules, id) {
				add(schemaStore, rec.ID)
			} else {
				add(ruleGroup(id, rules), rec.ID)
			}
		}

		for j := range rec.License.Detections {
			add(detectedGroup(&rec.License.Detections[j]), rec.ID)
		}

		if len(rec.License.Rules) == 0 && len(rec.License.Detections) == 0 {
			add(declaredGroup(rec.Entry.Provenance.License), rec.ID)
		}
	}

	var out []*schemaGroup

	for _, g := range groups {
		if len(g.ids) > 0 {
			slices.Sort(g.ids)
			slices.Sort(g.details)
			out = append(out, g)
		}
	}

	slices.SortFunc(out, func(a, b *schemaGroup) int {
		return cmp.Or(cmp.Compare(a.kind, b.kind), strings.Compare(a.key, b.key))
	})

	return out, nil
}

// schemaStoreLicense is the license of the SchemaStore rules, which must be
// allow rules of one license.
func schemaStoreLicense(rules map[string]policyRule, ss schemaStoreData) (string, error) {
	var license string

	for _, id := range ss.Rules {
		r, ok := rules[id]

		switch {
		case !ok || r.Decision != string(policy.Allow):
			return "", fault.New(fault.Usage, "%s: schemastore.rules names %q, which is no allow rule of %s", DataFile, id, PolicyFile)
		case license != "" && r.License != license:
			return "", fault.New(fault.Usage, "%s: the schemastore rules of %s allow different licenses (%s, %s)", DataFile, PolicyFile, license, r.License)
		}

		license = r.License
	}

	return license, nil
}

func schemaStoreGroup(st *state.State, rules map[string]policyRule, ss schemaStoreData) (*schemaGroup, error) {
	license, err := schemaStoreLicense(rules, ss)
	if err != nil {
		return nil, err
	}

	repository, at := state.SchemaStoreRepository, ""
	if st.Source.Kind == state.KindSchemaStore {
		repository, at = st.Source.Repository, " at commit "+code(st.Source.Commit)
	}

	names := make([]string, 0, len(ss.Rules))
	for _, id := range ss.Rules {
		names = append(names, code(id))
	}

	text := []string{
		fmt.Sprintf("Files of the SchemaStore repository %s%s, allowed by %s of %s. The notice layer of each of these "+
			"schemas also carries the LICENSE and NOTICE files of the repository. Its NOTICE file reads:",
			code(repository), at, plural(len(names), "the rule ", "the rules ")+joinWords(names), code(PolicyFile)),
		fenced(ss.Notice),
	}

	return &schemaGroup{kind: groupSchemaStore, key: "schemastore", heading: "SchemaStore repository (" + license + ")", text: text}, nil
}

func ruleGroup(id string, rules map[string]policyRule) *schemaGroup {
	g := &schemaGroup{kind: groupRule, key: id}

	r, ok := rules[id]
	if !ok {
		g.heading = "Rule " + code(id)
		g.text = []string{fmt.Sprintf("The rule is no longer in %s; the catalog entries of these schemas record the license "+
			"they were published under.", code(PolicyFile))}

		return g
	}

	g.heading = "Rule " + code(id) + " (" + r.License + ")"
	reason := strings.TrimSuffix(strings.Join(strings.Fields(r.Reason), " "), ".")
	g.text = []string{fmt.Sprintf("Allowed by the rule %s of %s: %s.", code(id), code(PolicyFile), reason)}

	return g
}

// detectedGroup names the repository or package that automatic license
// detection read, without the commit or version it was pinned to, which the
// details list instead.
func detectedGroup(d *policy.Detection) *schemaGroup {
	kind, name, pin := "Source", d.Source, ""

	if repo, ok := strings.CutPrefix(d.Source, "github:"); ok {
		if at := strings.LastIndex(repo, "@"); at > 0 {
			kind, name, pin = "GitHub repository", repo[:at], "commit "+code(repo[at+1:])
		}
	} else if pkg, ok := strings.CutPrefix(d.Source, "npm:"); ok {
		if at := strings.LastIndex(pkg, "@"); at > 0 {
			kind, name, pin = "npm package", pkg[:at], "version "+code(pkg[at+1:])
		}
	}

	detail := code(d.LicenseFile)
	if pin != "" {
		detail += " at " + pin
	}

	if d.NoticeFile != "" {
		detail += " with the notice file " + code(d.NoticeFile)
	}

	return &schemaGroup{
		kind:    groupDetected,
		key:     kind + "\x00" + name + "\x00" + d.License,
		heading: kind + " " + code(name) + " (" + d.License + ")",
		details: []string{detail},
	}
}

func declaredGroup(license string) *schemaGroup {
	if license == "" {
		license = "none recorded"
	}

	return &schemaGroup{
		kind:    groupDeclared,
		key:     license,
		heading: "Declared by the source (" + license + ")",
		text:    []string{"The source description declares the license of these schemas; no rule or detection decided it."},
	}
}

func appendNew(list []string, items ...string) []string {
	for _, item := range items {
		if !slices.Contains(list, item) {
			list = append(list, item)
		}
	}

	return list
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}

	return many
}

// joinWords joins items as "a", "a and b" or "a, b and c".
func joinWords(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}

	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// code formats text as a Markdown code span, with a fence longer than any
// run of backticks in it.
func code(text string) string {
	fence := "`"
	for strings.Contains(text, fence) {
		fence += "`"
	}

	if strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`") {
		text = " " + text + " "
	}

	return fence + text + fence
}

// fenced formats text as a fenced code block, which keeps its line breaks.
func fenced(text string) string {
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}

	return fence + "text\n" + strings.Trim(text, "\n") + "\n" + fence
}
