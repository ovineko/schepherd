package prepare

import (
	"slices"
	"strings"

	"github.com/ovineko/schepherd/internal/publisher/licensedetect"
	"github.com/ovineko/schepherd/internal/publisher/policy"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

// decider makes license decisions. For SchemaStore sources it is the policy
// applied to canonical URLs. A local source may additionally declare the
// license of its own entries and documents (see policy.DecideWith).
// Every other URI needs a policy rule or, when the policy's [auto] section
// is enabled, a license found by automatic detection (see detectLicenses);
// rules and declarations take precedence over detection.
type decider struct {
	policy    *policy.Policy
	canonical func(string) string
	declared  map[string]string
	detector  *licensedetect.Detector
}

// canon maps a URI to its source identity. A fragment is kept: it names one
// subschema, a different source than the document.
func (d *decider) canon(uri string) string {
	if d.canonical == nil {
		return uri
	}

	document, fragment := splitFragment(uri)
	if fragment == "" {
		return d.canonical(document)
	}

	return d.canonical(document) + "#" + fragment
}

// decide combines the verdicts for root and every dependency: exclude wins
// over review, which wins over allow.
func (d *decider) decide(root string, deps []string) policy.Decision {
	return d.decideWith(root, deps, d.lookup())
}

// decideWith is decide with the findings of lookup in place of those of
// the detector.
func (d *decider) decideWith(root string, deps []string, lookup policy.Lookup) policy.Decision {
	canonical := make([]string, 0, len(deps))
	for _, dep := range deps {
		canonical = append(canonical, d.canon(dep))
	}

	return d.policy.DecideWith(d.canon(root), canonical, lookup, d.declaration())
}

// single decides one URI on its own, as a dependency is decided before it
// is fetched.
func (d *decider) single(uri string) policy.Decision {
	return d.singleWith(uri, d.lookup())
}

func (d *decider) singleWith(uri string, lookup policy.Lookup) policy.Decision {
	return d.decideWith(uri, nil, lookup)
}

// declaration looks up the licenses of the local source file. Declarations
// exist only for local sources, whose URIs are their own canonical form.
func (d *decider) declaration() policy.Declared {
	if len(d.declared) == 0 {
		return nil
	}

	return func(uri string) (string, bool) {
		license, ok := d.declared[normalizeURI(uri)]

		return license, ok
	}
}

// basis returns what an allowing decision for root and deps rests on, the
// record the publisher state keeps of it: the rules that decide any of the
// URIs, the detections that allowed the others and the redirects that
// served documents of the schema (whose targets deps includes).
func (d *decider) basis(root string, deps []string, decision *policy.Decision, redirects []state.Redirect) state.LicenseDecision {
	var rules []string

	if d.policy != nil {
		for _, uri := range append([]string{root}, deps...) {
			if policy.IsWellKnownMetaschema(uri) {
				continue
			}

			if id := d.policy.RuleID(d.canon(uri)); id != "" {
				rules = append(rules, id)
			}
		}
	}

	slices.Sort(rules)
	rules = slices.Compact(rules)

	var served []state.Redirect
	if len(redirects) > 0 {
		served = slices.Clone(redirects)
		slices.SortFunc(served, func(a, b state.Redirect) int { return strings.Compare(a.URL, b.URL) })
	}

	return state.LicenseDecision{Rules: rules, Detections: licenseSources(decision), Redirects: served}
}

// recordedNotice stands in for the license text of a recorded detection.
// Deciding again from recorded findings only checks that the decision still
// holds; the notice itself travels in the recorded artifact.
const recordedNotice = "(the license text the published artifact carries)"

// recordedLookup answers with the findings a state record's license
// decision recorded, so that a policy decides again without asking the
// detection services.
func recordedLookup(detections []policy.Detection) policy.Lookup {
	findings := make(map[string]policy.Finding, len(detections))

	for i := range detections {
		findings[detections[i].URL] = recordedFinding(&detections[i])
	}

	return func(url string) (policy.Finding, bool) {
		f, ok := findings[url]

		return f, ok
	}
}

// recordedFinding is the finding a recorded detection stands for.
func recordedFinding(det *policy.Detection) policy.Finding {
	return policy.Finding{
		Source: det.Source, License: det.License, LicenseFile: det.LicenseFile, LicenseDigest: det.LicenseDigest,
		NoticeFile: det.NoticeFile, NoticeDigest: det.NoticeDigest, Notice: recordedNotice,
	}
}
