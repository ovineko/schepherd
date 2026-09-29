package prepare

import (
	"context"
	"errors"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/publisher/policy"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

// reusable returns the state record whose artifact and license decision g
// may keep: the published schema of g's source, when g keeps its ID and no
// refresh asks for it to be prepared again.
func (p *preparer) reusable(g *group) *state.Schema {
	if p.opts.State == nil || p.opts.RefreshAll {
		return nil
	}

	id, ok := p.previous[g.key]
	if !ok || p.refresh[id] {
		return nil
	}

	if override, ok := p.overrides[g.key]; ok && override != id {
		return nil
	}

	if claimant, ok := p.claimed[id]; ok && claimant != g.key {
		return nil
	}

	rec, ok := p.opts.State.Lookup(id)
	if !ok || rec.Excluded() || rec.Entry.Provenance == nil || rec.Entry.Provenance.Source != g.key {
		return nil
	}

	return rec
}

// recordedDependencies returns the URIs a recorded decision covers besides
// the source: the recorded dependencies and every URL that served one of
// the schema's documents through a redirect.
func recordedDependencies(rec *state.Schema) []string {
	prov := rec.Entry.Provenance

	deps := make([]string, 0, len(prov.Dependencies)+len(rec.License.Redirects))
	for _, dep := range prov.Dependencies {
		deps = append(deps, dep.Source)
	}

	for _, r := range rec.License.Redirects {
		deps = append(deps, r.Target)
	}

	return deps
}

// reuse decides a published schema from its state record alone, so that an
// unchanged schema costs no license detection request and no bundling. The
// license policy decides again on the recorded findings of automatic license
// detection: an exclude or review rule, or a recorded license the policy no
// longer permits, stops the schema without contacting anything. When the
// policy still allows it on exactly the recorded rules and findings, and the
// source and every recorded dependency still have their recorded digests
// and are served where the record says (directly, or through a redirect to
// the recorded target), the entry keeps the recorded artifact; its metadata
// comes from upstream. done is false when the schema must be prepared in
// full instead, which decides anew and asks detection again.
func (p *preparer) reuse(ctx context.Context, g *group, rec *state.Schema) (o outcome, done bool, err error) {
	prov := rec.Entry.Provenance
	deps := recordedDependencies(rec)

	d := p.decider.decideWith(g.key, deps, recordedLookup(rec.License.Detections))

	switch {
	case d.Decision == policy.Exclude, d.Decision == policy.Review && (d.RuleID != "" || d.AutoReason != ""):
		return notAllowed(d), true, nil
	case d.Decision != policy.Allow, d.License != prov.License:
		return outcome{}, false, nil
	}

	if basis := p.decider.basis(g.key, deps, &d, rec.License.Redirects); !basis.Equal(&rec.License) {
		return outcome{}, false, nil
	}

	served := servedAsRecorded(rec.License.Redirects)

	root, err := p.rootDocument(ctx, g)
	if err != nil {
		return reuseFailure(ctx, err)
	}

	if !served.match(g.retrieve, root, prov.SourceDigest) {
		return outcome{}, false, nil
	}

	for _, dep := range prov.Dependencies {
		doc, err := p.provider.obtain(ctx, dep.Source)
		if err != nil {
			return reuseFailure(ctx, err)
		}

		if !served.match(dep.Source, doc, dep.Digest) {
			return outcome{}, false, nil
		}
	}

	if !served.complete() {
		return outcome{}, false, nil
	}

	if err := CheckMetadata(g.name, g.description, rec.Entry.Dialect, g.patterns, prov); err != nil {
		return failed(ReasonInvalidMetadata, err.Error()), true, nil
	}

	return outcome{status: StatusIncluded, reused: rec, decision: d, basis: rec.License, sourceDigest: prov.SourceDigest}, true, nil
}

// reuseFailure classifies an error of obtaining a recorded document. A
// redirect the fetcher refused prepares the schema in full (done is false):
// the document is no longer served where the record says, and a full
// preparation decides the new target with fresh findings. Every other
// failure is the record's verdict, as in a full preparation.
func reuseFailure(ctx context.Context, err error) (o outcome, done bool, _ error) {
	if _, refused := errors.AsType[*redirectRefusedError](err); refused && ctx.Err() == nil {
		return outcome{}, false, nil
	}

	bad, err := obtainFailure(ctx, err)

	return deref(bad), true, err
}

// recordedRedirects compares the documents obtained for reuse with the
// redirects a license decision recorded.
type recordedRedirects struct {
	byURL   map[string]state.Redirect
	matched int
}

func servedAsRecorded(redirects []state.Redirect) *recordedRedirects {
	r := &recordedRedirects{byURL: make(map[string]state.Redirect, len(redirects))}
	for _, redirect := range redirects {
		r.byURL[redirect.URL] = redirect
	}

	return r
}

// match reports whether doc, obtained for uri, has the recorded digest and
// was served as recorded: through a redirect to the recorded target with
// the recorded digest when the decision records one for uri, directly
// otherwise.
func (r *recordedRedirects) match(uri string, doc document, want string) bool {
	got := digest.FromBytes(doc.body)
	if got != want {
		return false
	}

	recorded, ok := r.byURL[uri]
	if !ok {
		return doc.redirect == ""
	}

	r.matched++

	return doc.redirect == recorded.Target && recorded.Digest == got
}

// complete reports whether every recorded redirect belonged to a document
// that was compared.
func (r *recordedRedirects) complete() bool {
	return r.matched == len(r.byURL)
}
