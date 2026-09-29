package prepare

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/jsonutil"
	"github.com/ovineko/schepherd/internal/publisher/bundle"
	"github.com/ovineko/schepherd/internal/publisher/policy"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

// policyStopError aborts dependency resolution before an unreviewed or
// excluded dependency is fetched.
type policyStopError struct {
	uri      string
	decision policy.Decision
}

func (e *policyStopError) Error() string {
	return "dependency " + e.uri + " is not allowed: " + e.decision.Reason
}

func failed(reason, detail string) outcome {
	return outcome{status: StatusFailed, reason: reason, detail: detail}
}

// notAllowed is the outcome of a record the license policy excludes or
// holds for review.
func notAllowed(d policy.Decision) outcome {
	status := StatusPendingReview
	if d.Decision == policy.Exclude {
		status = StatusExcluded
	}

	return outcome{status: status, decision: d, detail: d.Reason}
}

// process prepares one group. Verdicts about the input become the outcome;
// only operational failures (cancellation, a broken bundler, unreadable local
// files) are returned as errors and stop the run.
func (p *preparer) process(ctx context.Context, g *group) (outcome, error) {
	if g.failReason != "" {
		return failed(g.failReason, g.failDetail), nil
	}

	if rec := p.reusable(g); rec != nil {
		o, done, err := p.reuse(ctx, g, rec)
		if err != nil || done {
			return o, err
		}
	}

	return p.prepareAnew(ctx, g)
}

// prepareAnew decides the license of one group, obtains its documents and
// bundles and verifies them.
func (p *preparer) prepareAnew(ctx context.Context, g *group) (outcome, error) {
	d, err := p.decider.decideContext(ctx, g.key, nil)
	if err != nil {
		return outcome{}, err
	}

	if d.Decision != policy.Allow {
		return notAllowed(d), nil
	}

	root, bad, err := p.root(ctx, g)
	if err != nil || bad != nil {
		return deref(bad), err
	}

	var served []state.Redirect

	if root.redirect != "" {
		d, err := p.decider.singleContext(ctx, root.redirect)
		if err != nil {
			return outcome{}, err
		}

		if d.Decision != policy.Allow {
			d.Reason = "redirected to " + root.redirect + ": " + d.Reason

			return notAllowed(d), nil
		}

		served = append(served, state.Redirect{URL: g.retrieve, Target: root.redirect, Digest: digest.FromBytes(root.body)})
	}

	if err := jsonutil.Check(root.body, jsonutil.DefaultMaxDepth); err != nil {
		if errors.Is(err, jsonutil.ErrDuplicateKey) {
			return failed(ReasonDuplicateKeys, g.retrieve+": "+err.Error()), nil
		}

		return failed(bundle.ReasonInvalidJSON, g.retrieve+": "+err.Error()), nil
	}

	rootURI := g.retrieve
	if g.fragment != "" {
		rootURI += "#" + g.fragment
	}

	closure, err := bundle.Resolve(ctx, p.opts.Tool, bundle.Document{URI: rootURI, Bytes: root.body}, p.dependencies(&served), p.src.limits)
	if err != nil {
		return rejection(err)
	}

	deps := make([]string, 0, len(closure.Dependencies)+len(served))
	for _, dep := range closure.Dependencies {
		deps = append(deps, dep.URI)
	}

	for _, r := range served {
		deps = append(deps, r.Target)
	}

	decision, err := p.decider.decideContext(ctx, g.key, deps)
	if err != nil {
		return outcome{}, err
	}

	if decision.Decision != policy.Allow {
		return notAllowed(decision), nil
	}

	o, err := p.finishBundle(ctx, g, closure, decision, root.fromSnapshot)
	if err != nil || o.status != StatusIncluded {
		return o, err
	}

	o.basis = p.decider.basis(g.key, deps, &decision, served)

	return admit(g, o), nil
}

// admissionID stands in for the ID of an entry that is checked before IDs
// are assigned.
const admissionID = "x"

// admit applies the rules prepared.json and clients apply to a prepared
// entry to one record while it is prepared: a schema or notice above the
// default artifact limits of clients, a notice that is not UTF-8 text, or
// an entry the format rejects (such as a detected license file name the
// state cannot record) fails that record. Checked only for the whole set,
// the same problem would fail the run, and with it the weekly update.
func admit(g *group, o outcome) outcome {
	limits := artifact.DefaultLimits()

	switch {
	case int64(len(o.prepared.Schema)) > limits.MaxSchemaBytes:
		return failed(ReasonTooLarge, fmt.Sprintf("the prepared schema has %d bytes; clients accept at most %d", len(o.prepared.Schema), limits.MaxSchemaBytes))
	case int64(len(o.notice)) > limits.MaxNoticeBytes:
		return failed(ReasonTooLarge, fmt.Sprintf("the notice has %d bytes; clients accept at most %d", len(o.notice), limits.MaxNoticeBytes))
	case !utf8.Valid(o.notice):
		return failed(ReasonInvalidMetadata, "the notice is not UTF-8 text")
	}

	entry := preparedEntryOf(g, &o, admissionID)
	if err := entry.validate(); err != nil {
		return failed(ReasonInvalidMetadata, err.Error())
	}

	return o
}

func deref(o *outcome) outcome {
	if o == nil {
		return outcome{}
	}

	return *o
}

func (p *preparer) finishBundle(
	ctx context.Context, g *group, closure *bundle.Closure, decision policy.Decision, rootFromSnapshot bool,
) (outcome, error) {
	instances, bad, err := p.instances(g)
	if err != nil || bad != nil {
		return deref(bad), err
	}

	prepared, err := bundle.Prepare(ctx, p.opts.Tool, closure, instances)
	if err != nil {
		return rejection(err)
	}

	prov := catalog.Provenance{
		Source: g.key, SourceDigest: closure.Root.Digest, License: decision.License, Dependencies: dependencies(prepared),
	}
	if err := CheckMetadata(g.name, g.description, prepared.Dialect, g.patterns, &prov); err != nil {
		return failed(ReasonInvalidMetadata, err.Error()), nil
	}

	fromSnapshot := rootFromSnapshot

	for _, dep := range closure.Dependencies {
		if p.snap != nil {
			if _, ok := p.snap.LocalPath(dep.URI); ok {
				fromSnapshot = true
			}
		}
	}

	return outcome{
		status: StatusIncluded, prepared: prepared, decision: decision, sourceDigest: closure.Root.Digest,
		notice: p.notice(decision, fromSnapshot),
	}, nil
}

func dependencies(prepared *bundle.Prepared) []catalog.Dependency {
	if len(prepared.Dependencies) == 0 {
		return nil
	}

	deps := make([]catalog.Dependency, 0, len(prepared.Dependencies))
	for _, dep := range prepared.Dependencies {
		deps = append(deps, catalog.Dependency{Source: dep.URI, Digest: dep.Digest})
	}

	return deps
}

// root obtains the root document. A record-level problem is returned as an
// outcome, an operational one as an error.
func (p *preparer) root(ctx context.Context, g *group) (document, *outcome, error) {
	doc, err := p.rootDocument(ctx, g)
	if err == nil {
		return doc, nil, nil
	}

	o, err := obtainFailure(ctx, err)

	return document{}, o, err
}

// rootDocument obtains the root document from its local file or the
// provider.
func (p *preparer) rootDocument(ctx context.Context, g *group) (document, error) {
	if g.localFile == "" {
		return p.provider.obtain(ctx, g.retrieve)
	}

	body, err := readLimited(g.localFile, g.key, p.src.limits.MaxDocumentBytes)

	return document{body: body}, err
}

// obtainFailure maps an error of obtaining a document to the outcome of its
// record; operational errors stay errors.
func obtainFailure(ctx context.Context, err error) (*outcome, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, err
	}

	var o outcome

	if refusal, ok := errors.AsType[*redirectRefusedError](err); ok {
		o = notAllowed(refusal.decisionFor())
	} else if limit, ok := errors.AsType[*limitError](err); ok {
		o = failed(bundle.ReasonDependencyLimit, limit.Error())
	} else if fetch, ok := errors.AsType[*fetchError](err); ok {
		o = failed(ReasonFetchFailed, fetch.Error())
	} else if errors.Is(err, errNotInSnapshot) {
		o = failed(ReasonFetchFailed, err.Error())
	} else {
		return nil, err
	}

	return &o, nil
}

// dependencies returns the fetch callback for bundle.Resolve of one record.
// The policy is consulted before a URL is requested, so an unreviewed URL is
// never requested directly. A redirect is only followed to content the
// policy allows as well; it is appended to served so that the record's
// license and notices cover its target and the state records it.
func (p *preparer) dependencies(served *[]state.Redirect) func(context.Context, string) (bundle.Document, error) {
	return func(ctx context.Context, uri string) (bundle.Document, error) {
		d, err := p.decider.singleContext(ctx, uri)
		if err != nil {
			return bundle.Document{}, err
		}

		if d.Decision != policy.Allow {
			return bundle.Document{}, &policyStopError{uri: uri, decision: d}
		}

		doc, err := p.provider.obtain(ctx, uri)
		if refusal, ok := errors.AsType[*redirectRefusedError](err); ok {
			return bundle.Document{}, &policyStopError{uri: uri, decision: refusal.decisionFor()}
		}

		if err != nil {
			return bundle.Document{}, err
		}

		if doc.redirect != "" {
			d, err := p.decider.singleContext(ctx, doc.redirect)
			if err != nil {
				return bundle.Document{}, err
			}

			if d.Decision != policy.Allow {
				d.Reason = "redirected to " + doc.redirect + ": " + d.Reason

				return bundle.Document{}, &policyStopError{uri: uri, decision: d}
			}

			*served = append(*served, state.Redirect{URL: uri, Target: doc.redirect, Digest: digest.FromBytes(doc.body)})
		}

		return bundle.Document{URI: uri, Bytes: doc.body, Digest: digest.FromBytes(doc.body)}, nil
	}
}

// rejection maps a Resolve or Prepare error to an outcome. Errors that are
// not bundle failures are operational and returned unchanged.
func rejection(err error) (outcome, error) {
	if stop, ok := errors.AsType[*policyStopError](err); ok {
		d := stop.decision
		d.Reason = "dependency " + stop.uri + ": " + d.Reason

		return notAllowed(d), nil
	}

	failure, ok := errors.AsType[*bundle.Failure](err)
	if !ok {
		return outcome{}, err
	}

	detail := failure.Detail
	if failure.Err != nil {
		detail += ": " + failure.Err.Error()
	}

	if _, ok := errors.AsType[*limitError](err); ok {
		return failed(bundle.ReasonDependencyLimit, detail), nil
	}

	if _, ok := errors.AsType[*fetchError](err); ok {
		return failed(ReasonFetchFailed, detail), nil
	}

	return failed(failure.Reason, detail), nil
}

// instances loads the JSON test instances of a group: the SchemaStore
// positive and negative tests of the root file, or the instances listed in a
// local source. The tests of a file are instances of the whole document, so
// an entry for a fragment of it has none.
func (p *preparer) instances(g *group) ([]bundle.Instance, *outcome, error) {
	files := g.instances

	if g.fragment != "" {
		return nil, nil, nil
	}

	if g.snapshotFile != "" && p.snap != nil {
		positive, negative := p.snap.Tests(g.snapshotFile)
		files = nil

		for _, file := range append(positive, negative...) {
			rel, err := filepath.Rel(p.snap.Dir, file)
			if err != nil {
				rel = filepath.Base(file)
			}

			files = append(files, instanceFile{name: filepath.ToSlash(rel), path: file})
		}
	}

	out := make([]bundle.Instance, 0, len(files))

	for _, f := range files {
		data, err := readLimited(f.path, f.name, p.src.limits.MaxDocumentBytes)
		if err != nil {
			if limit, ok := errors.AsType[*limitError](err); ok {
				o := failed(bundle.ReasonInvalidInstance, limit.Error())

				return nil, &o, nil
			}

			return nil, nil, err
		}

		out = append(out, bundle.Instance{Name: f.name, Data: data})
	}

	return out, nil, nil
}

// notice is the text of the artifact's notice layer: the notices of every
// policy rule and detected license involved (a detected one is a header
// naming the repository ref or package version, the license text and an
// Apache-2.0 NOTICE file when there is one) and, when any document came
// from the SchemaStore snapshot, that repository's LICENSE and NOTICE files,
// as Apache-2.0 section 4 requires. It contains nothing that changes without
// the schema or those texts changing.
func (p *preparer) notice(d policy.Decision, fromSnapshot bool) []byte {
	var parts []string

	if d.Notice != "" {
		parts = append(parts, d.Notice)
	}

	if fromSnapshot && p.upstreamNotice != "" {
		parts = append(parts, p.upstreamNotice)
	}

	if len(parts) == 0 {
		return nil
	}

	return []byte(strings.Join(parts, "\n\n") + "\n")
}
