package bundle

import (
	"fmt"
)

// Reasons reported by Failure. They are stable machine-readable codes that
// the publisher records verbatim when it rejects an upstream schema.
const (
	// ReasonUnresolvedRef means a referenced resource could not be obtained:
	// the fetch callback failed, the reference points at the local file
	// system, is not absolute, or names a fragment that does not exist.
	ReasonUnresolvedRef = "unresolved-ref"
	// ReasonDependencyLimit means a document, the closure or its depth
	// exceeded Limits.
	ReasonDependencyLimit = "dependency-limit"
	// ReasonUndeclaredDialect means the root has no $schema although it
	// references other resources, so the reference semantics are unknown.
	ReasonUndeclaredDialect = "undeclared-dialect"
	// ReasonUnsupportedDialect means a document declares a $schema other than
	// draft-04, draft-06, draft-07, 2019-09 or 2020-12 where bundling needs it.
	ReasonUnsupportedDialect = "unsupported-dialect"
	// ReasonTopLevelRefDraft7 means a draft-04/06/07 document that must be
	// bundled is a top-level $ref next to a keyword that constrains
	// instances. Those dialects ignore it while other tools apply it, so no
	// rewrite into a bundle keeps the document's meaning.
	ReasonTopLevelRefDraft7 = "top-level-ref-draft7"
	// ReasonIDMismatch means a dependency declares an identifier other than
	// the URI it was fetched under, or two documents claim one identifier.
	ReasonIDMismatch = "id-mismatch"
	// ReasonFragmentRoot means the root retrieval URI carries a fragment
	// that cannot name a subschema: it is neither a JSON Pointer nor a
	// plain-name anchor, or the document is not an object.
	ReasonFragmentRoot = "fragment-root"
	// ReasonInvalidJSON means a document is not strict JSON (syntax,
	// encoding, duplicate member names or nesting depth).
	ReasonInvalidJSON = "invalid-json"
	// ReasonInvalidSchema means the upstream schema itself is rejected by the
	// verifying validator, for example because it violates its metaschema.
	ReasonInvalidSchema = "invalid-schema"
	// ReasonBundlerError means the external bundler failed for a reason not
	// covered by a more specific code, or produced unusable output.
	ReasonBundlerError = "bundler-error"
	// ReasonNotSelfContained means the bundled output still needs a resource
	// that is not embedded in it.
	ReasonNotSelfContained = "not-self-contained"
	// ReasonBehaviourMismatch means the bundle and the original schema
	// disagree about a test instance.
	ReasonBehaviourMismatch = "behaviour-mismatch"
	// ReasonReferenceMismatch means the bundle does not carry exactly the
	// $ref, $dynamicRef, $recursiveRef, $anchor, $dynamicAnchor and
	// $recursiveAnchor members of the documents it was made from, so the
	// bundler changed what a reference resolves to.
	ReasonReferenceMismatch = "reference-mismatch"
	// ReasonInvalidInstance means a JSON test instance is not strict JSON.
	ReasonInvalidInstance = "invalid-instance"
)

// Failure rejects an upstream schema: it cannot be turned into a verified,
// self-contained schema. Reason is one of the Reason constants; Detail is a
// human-readable explanation that never contains local temporary paths.
// Err, when set, is the underlying cause (for example the error returned by
// the fetch callback) and is exposed through Unwrap.
//
// A Failure is a verdict about the input, not an operational error: the
// caller decides how it maps to an exit status.
type Failure struct { //nolint:errname // the publisher contract names this rejection type Failure
	Err    error
	Reason string
	Detail string
}

// Error reads "bundle rejected (<reason>): <detail>", followed by the
// underlying cause when there is one.
func (f *Failure) Error() string {
	msg := "bundle rejected (" + f.Reason + ")"
	if f.Detail != "" {
		msg += ": " + f.Detail
	}

	if f.Err != nil {
		msg += ": " + f.Err.Error()
	}

	return msg
}

// Unwrap lets errors.Is and errors.As reach the fetch callback's or the
// verifier's error behind a rejection, for example a network error.
func (f *Failure) Unwrap() error {
	return f.Err
}

func reject(reason, format string, args ...any) *Failure {
	return &Failure{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

func rejectWrap(reason string, err error, format string, args ...any) *Failure {
	return &Failure{Reason: reason, Detail: fmt.Sprintf(format, args...), Err: err}
}
