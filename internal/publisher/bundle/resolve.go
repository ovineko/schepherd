// Package bundle turns an upstream JSON Schema and the resources it
// references into one self-contained schema and verifies the result.
//
// Resolve discovers the dependency closure with the Sourcemeta JSON Schema
// CLI's `inspect` command and obtains every external resource through a
// caller-supplied fetch function, so network policy (SSRF protection, source
// pinning) stays with the caller; the CLI itself never gets network access.
// Prepare then either keeps the root byte-for-byte (when it references
// nothing outside itself) or runs the CLI's `bundle` command and verifies the
// output: it must carry exactly the references and anchors of its sources,
// `inspect` must find no external reference, an offline validator must
// compile it, and the bundle must agree with the original schema on every
// JSON test instance.
//
// A schema that cannot be handled is rejected with a *Failure carrying a
// stable reason code; operational problems are fault-classified errors.
package bundle

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/jsonutil"
)

// Document is one JSON document taking part in a bundle.
type Document struct {
	// URI is the absolute URI the document was retrieved from. Resolve
	// records a dependency under the URI it requested, which is also the
	// identifier the bundle embeds it under.
	URI string
	// Digest is the sha256 digest of Bytes. Resolve computes it when it is
	// empty and rejects a document whose digest does not match.
	Digest string
	// Bytes is the document exactly as retrieved.
	Bytes []byte
}

// Limits bound the dependency closure. A zero or negative field takes the
// value from DefaultLimits. The fields correspond one to one to the
// [dependencies] table of sources/schemastore.toml.
type Limits struct {
	// MaxDepth is the longest chain of references from the root to a
	// dependency; direct dependencies have depth 1.
	MaxDepth int
	// MaxDocuments is the number of dependency documents a root may pull
	// in; the root itself is not counted (max_per_schema).
	MaxDocuments int
	// MaxDocumentBytes bounds each document, the root included.
	MaxDocumentBytes int64
	// MaxTotalBytes bounds the sum of all documents, the root included.
	MaxTotalBytes int64
}

// DefaultLimits returns the documented publisher limits: 8 levels, 64
// dependency documents, 16 MiB per document and 256 MiB in total.
func DefaultLimits() Limits {
	return Limits{MaxDepth: 8, MaxDocuments: 64, MaxDocumentBytes: 16 << 20, MaxTotalBytes: 256 << 20}
}

func (l Limits) withDefaults() Limits {
	def := DefaultLimits()

	if l.MaxDepth <= 0 {
		l.MaxDepth = def.MaxDepth
	}

	if l.MaxDocuments <= 0 {
		l.MaxDocuments = def.MaxDocuments
	}

	if l.MaxDocumentBytes <= 0 {
		l.MaxDocumentBytes = def.MaxDocumentBytes
	}

	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = def.MaxTotalBytes
	}

	return l
}

// Closure is a root schema together with every document it transitively
// references. Only Resolve creates usable closures.
type Closure struct {
	// Root is the upstream document as retrieved; the URI of a fragment
	// root keeps its fragment.
	Root Document
	// Dialect is the root's $schema, or "" when it declares none.
	Dialect string
	// Dependencies are the documents fetched for references, sorted by URI.
	Dependencies []Document

	// nodes[0] is the root the CLI bundles; source is the node holding
	// Root, which is nodes[0] unless Root names a fragment.
	nodes  []*node
	source *node
	// original is the location the verifier loads Root from and target the
	// schema it compiles: original itself, or original plus the fragment.
	original string
	target   string
	// wrapperID identifies the synthetic root of a fragment root while the
	// CLI works on it; Prepare removes it from the bundle.
	wrapperID string
	limits    Limits
	bundle    bool
}

// node is a closure document plus the copy handed to the CLI, whose
// top-level identifier is made absolute (inserted or rewritten) so that the
// CLI resolves references against the retrieval URI and never against the
// temporary file path.
type node struct {
	doc      Document
	dialect  string
	prepared []byte
	bases    []string
}

type resolver struct {
	tool      *Tool
	fetch     func(ctx context.Context, uri string) (Document, error)
	ws        *workspace
	known     map[string]struct{}
	wrapper   *node
	original  string
	fragment  string
	wrapperID string
	nodes     []*node
	limits    Limits
	total     int64
	bundle    bool
}

// Resolve discovers the dependency closure of root breadth-first and in
// sorted order, calling fetch exactly once per external resource URI. fetch
// receives absolute URIs without fragment, in the RFC 3986 normalized form
// (lowercase host, no default port); the caller implements SSRF-safe
// retrieval and source pinning. Cycles are followed only once. Official
// metaschemas are never fetched and never become dependencies: every
// validator carries them, so a reference to one needs no bundling.
//
// A root URI with a fragment stands for the subschema the fragment names:
// the document is embedded as a resource and the bundle root only applies
// that subschema, so the prepared schema represents the resource, never the
// whole document. A draft-04/06/07 document that is a top-level $ref and
// needs bundling is rewritten as described at wrapLegacyRef.
//
// Resolve rejects, with a *Failure: a root fragment that is neither a JSON
// Pointer nor a plain name, a document whose $schema is a file: URI, a
// reference to the local file system or one that cannot be made absolute, a
// resource fetch fails for, a dependency whose identifier differs from its
// URI, a root without $schema that references anything outside itself or
// names a fragment, a draft-04/06/07 top-level $ref next to keywords those
// dialects ignore, an unsupported dialect where bundling needs one, and
// exceeded limits.
func Resolve(ctx context.Context, t *Tool, root Document, fetch func(ctx context.Context, uri string) (Document, error), limits Limits) (*Closure, error) {
	if t == nil || fetch == nil {
		return nil, fault.New(fault.Internal, "bundle.Resolve needs a tool and a fetch function")
	}

	ws, err := newWorkspace()
	if err != nil {
		return nil, err
	}

	defer ws.close()

	r := &resolver{tool: t, fetch: fetch, ws: ws, limits: limits.withDefaults(), known: map[string]struct{}{}}

	return r.resolve(ctx, root)
}

func (r *resolver) resolve(ctx context.Context, root Document) (*Closure, error) {
	rootNode, err := r.admitRoot(ctx, root)
	if err != nil {
		return nil, err
	}

	level := []*node{rootNode}

	for depth := 1; r.bundle && len(level) > 0; depth++ {
		wanted := r.unknownBases(level)
		if len(wanted) > 0 && depth > r.limits.MaxDepth {
			return nil, reject(ReasonDependencyLimit, "%s is more than %d references away from the root", wanted[0], r.limits.MaxDepth)
		}

		var next []*node

		for _, base := range wanted {
			if _, ok := r.known[base]; ok {
				continue
			}

			n, err := r.admitDependency(ctx, base)
			if err != nil {
				return nil, err
			}

			next = append(next, n)
		}

		level = next
	}

	c := &Closure{
		Root: rootNode.doc, Dialect: rootNode.dialect, nodes: r.nodes, source: rootNode,
		original: r.original, target: r.original, limits: r.limits, bundle: r.bundle,
	}

	for _, n := range r.nodes[1:] {
		c.Dependencies = append(c.Dependencies, n.doc)
	}

	if r.wrapper != nil {
		c.nodes = append([]*node{r.wrapper}, r.nodes...)
		c.Root.URI += "#" + r.fragment
		c.target += "#" + r.fragment
		c.wrapperID = r.wrapperID
	}

	slices.SortFunc(c.Dependencies, func(a, b Document) int { return strings.Compare(a.URI, b.URI) })

	return c, nil
}

func (r *resolver) unknownBases(level []*node) []string {
	seen := map[string]struct{}{}

	var wanted []string

	for _, n := range level {
		for _, base := range n.bases {
			if _, ok := r.known[base]; ok {
				continue
			}

			if _, ok := seen[base]; ok {
				continue
			}

			seen[base] = struct{}{}
			wanted = append(wanted, base)
		}
	}

	slices.Sort(wanted)

	return wanted
}

func (r *resolver) admitRoot(ctx context.Context, root Document) (*node, error) {
	docURI, fragment, _ := strings.Cut(root.URI, "#")

	uri, base, err := retrievalBase(docURI)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "invalid root retrieval URI")
	}

	root.URI = uri
	r.original = uri

	n, err := r.admitBytes(root)
	if err != nil {
		return nil, err
	}

	tl, err := scanTopLevel(n.doc.Bytes)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "index root schema")
	}

	switch {
	case fragment != "":
		return n, r.admitFragmentRoot(ctx, n, tl, base, fragment)
	case tl.isObject:
		return n, r.admitObjectRoot(ctx, n, tl, base)
	case !isBoolean(n.doc.Bytes):
		return nil, reject(ReasonInvalidSchema, "root of %s is neither an object nor a boolean", uri)
	default:
		return n, nil
	}
}

func (r *resolver) admitObjectRoot(ctx context.Context, n *node, tl topLevel, base *url.URL) error {
	schema, present, err := tl.stringMember(n.doc.Bytes, "$schema")
	if err != nil {
		return rejectWrap(ReasonInvalidSchema, err, "%s", n.doc.URI)
	}

	n.dialect = schema

	d, known := lookupDialect(schema)

	switch {
	case !present:
		return r.requireFragmentRefsOnly(n, ReasonUndeclaredDialect, "declares no $schema")
	case !known:
		return r.inspectUnsupportedRoot(ctx, n)
	}

	data, wrapped := n.doc.Bytes, false

	if d.legacy && hasMember(tl, "$ref") {
		found, err := hasNonFragmentRef(n.doc.Bytes)
		if err != nil {
			return fault.Wrap(fault.Internal, err, "scan root references")
		}

		if !found {
			return nil
		}

		if data, tl, err = rewriteLegacyRef(n.doc.Bytes, tl, n.doc.URI, schema); err != nil {
			return err
		}

		wrapped = true
	}

	hadAbsoluteID, err := r.prepareRoot(n, data, tl, base, d)
	if err != nil {
		return r.passThroughSelfContained(n, err)
	}

	in, err := r.inspectNode(ctx, n, "")
	if err != nil {
		return r.passThroughSelfContained(n, err)
	}

	if err := collectBases(n, in); err != nil {
		return err
	}

	if wrapped {
		// Only the rewritten form carries an effective identifier, so it is
		// published even when every reference resolves inside the document.
		r.bundle = true

		return r.adoptIdentity(ctx, n, d)
	}

	r.bundle = len(n.bases) > 0 || !hadAbsoluteID && dependsOnBase(in)

	return nil
}

// admitFragmentRoot prepares a root URI with a fragment: the document is
// admitted like any root and a wrapper that applies the named subschema
// becomes the root the CLI bundles.
func (r *resolver) admitFragmentRoot(ctx context.Context, n *node, tl topLevel, base *url.URL, fragment string) error {
	uri := n.doc.URI

	if !validFragment(fragment) {
		return reject(ReasonFragmentRoot, "root URI %s#%s names neither a JSON Pointer nor a plain-name anchor", uri, fragment)
	}

	if !tl.isObject {
		return reject(ReasonFragmentRoot, "%s is not a JSON object, so #%s names nothing in it", uri, fragment)
	}

	schema, present, err := tl.stringMember(n.doc.Bytes, "$schema")
	if err != nil {
		return rejectWrap(ReasonInvalidSchema, err, "%s", uri)
	}

	if !present {
		return reject(ReasonUndeclaredDialect, "%s declares no $schema, so the reference semantics of #%s are unknown", uri, fragment)
	}

	n.dialect = schema

	d, known := lookupDialect(schema)
	if !known {
		return reject(ReasonUnsupportedDialect, "%s uses $schema %q, which cannot be bundled and verified", uri, schema)
	}

	data, wrapped := n.doc.Bytes, false

	if d.legacy && hasMember(tl, "$ref") {
		if data, tl, err = rewriteLegacyRef(n.doc.Bytes, tl, uri, schema); err != nil {
			return err
		}

		wrapped = true
	}

	if _, err := r.prepareRoot(n, data, tl, base, d); err != nil {
		return err
	}

	in, err := r.inspectNode(ctx, n, "")
	if err != nil {
		return err
	}

	if err := collectBases(n, in); err != nil {
		return err
	}

	if wrapped {
		if err := r.adoptIdentity(ctx, n, d); err != nil {
			return err
		}
	}

	identity, err := preparedID(n, d)
	if err != nil {
		return err
	}

	r.fragment = fragment
	r.wrapperID = "urn:schepherd:fragment-root:" + digest.Hex(n.doc.Digest)

	// An identifier with an empty fragment ("…/doc.json#", common up to
	// draft-07) names the document itself; a second "#" would not parse.
	wrapper, err := fragmentWrapper(d, schema, r.wrapperID, strings.TrimSuffix(identity, "#")+"#"+fragment)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "build the root for %s#%s", uri, fragment)
	}

	r.wrapper = &node{doc: Document{URI: r.wrapperID, Bytes: wrapper, Digest: digest.FromBytes(wrapper)}, dialect: schema, prepared: wrapper}
	r.bundle = true

	return nil
}

func rewriteLegacyRef(data []byte, tl topLevel, uri, dialectURI string) ([]byte, topLevel, error) {
	wrapped, err := wrapLegacyRef(data, tl, uri, dialectURI)
	if err != nil {
		return nil, topLevel{}, err
	}

	wtl, err := scanTopLevel(wrapped)
	if err != nil {
		return nil, topLevel{}, fault.Wrap(fault.Internal, err, "index the rewritten %s", uri)
	}

	return wrapped, wtl, nil
}

// preparedID returns the absolute identifier the CLI copy of n carries.
func preparedID(n *node, d dialect) (string, error) {
	tl, err := scanTopLevel(n.prepared)
	if err != nil {
		return "", fault.Wrap(fault.Internal, err, "index the prepared %s", n.doc.URI)
	}

	id, _, err := tl.stringMember(n.prepared, d.idKey)
	if err != nil {
		return "", rejectWrap(ReasonInvalidSchema, err, "%s", n.doc.URI)
	}

	return id, nil
}

// adoptIdentity settles the base URI of a rewritten root document. Next to
// a top-level $ref the declared identifier is ignored, so the original
// resolves against its retrieval URI, while the rewrite makes the
// identifier effective. Both agree when the identifier is the retrieval URI
// or when the identifier serves the very same bytes, which is the case for
// the mirrored hosts of one repository; the original is then verified as
// retrieved from the identifier. Otherwise the root is rejected.
func (r *resolver) adoptIdentity(ctx context.Context, n *node, d dialect) error {
	identity, err := preparedID(n, d)
	if err != nil {
		return err
	}

	if canonicalURI(identity) == canonicalURI(n.doc.URI) {
		return nil
	}

	uri, _, err := retrievalBase(identity)
	if err != nil {
		return rejectWrap(ReasonIDMismatch, err, "%s", n.doc.URI)
	}

	doc, err := r.fetch(ctx, uri)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fault.Wrap(fault.KindOf(ctxErr), ctxErr, "fetch %s", uri)
		}

		return rejectWrap(ReasonIDMismatch, err,
			"%s declares %s %q, which %s ignores next to its top-level $ref; the rewrite makes it effective, but it cannot be fetched to confirm it serves the same document",
			n.doc.URI, d.idKey, identity, n.dialect)
	}

	if digest.FromBytes(doc.Bytes) != n.doc.Digest {
		return reject(ReasonIDMismatch,
			"%s declares %s %q, which %s ignores next to its top-level $ref; the rewrite would make it effective, but it serves a different document",
			n.doc.URI, d.idKey, identity, n.dialect)
	}

	r.original = uri

	return nil
}

// passThroughSelfContained handles a root that cannot be prepared for the
// CLI or that the CLI refuses to analyse: an identifier with a fragment (a
// draft-06/07 plain-name anchor such as "#top") or one that does not parse,
// or JSON Pointers used as draft-07 "$id" anchors. Such a root needs no
// bundling, and is published unchanged, when every reference in it is a
// same-document fragment; otherwise the rejection stands.
func (r *resolver) passThroughSelfContained(n *node, cause error) error {
	if _, ok := errors.AsType[*Failure](cause); !ok {
		return cause
	}

	found, err := hasNonFragmentRef(n.doc.Bytes)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "scan root references")
	}

	if found {
		return cause
	}

	return nil
}

// requireFragmentRefsOnly accepts a root whose references cannot be
// discovered reliably only if every reference is a same-document fragment.
func (r *resolver) requireFragmentRefsOnly(n *node, reason, what string) error {
	found, err := hasNonFragmentRef(n.doc.Bytes)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "scan root references")
	}

	if found {
		return reject(reason, "%s %s and references other resources", n.doc.URI, what)
	}

	return nil
}

// inspectUnsupportedRoot passes through a root in a dialect the verifier
// does not implement (for example draft-03) only when it needs no bundling.
func (r *resolver) inspectUnsupportedRoot(ctx context.Context, n *node) error {
	n.prepared = n.doc.Bytes

	in, err := r.inspectNode(ctx, n, "")
	if err != nil {
		return err
	}

	external := slices.DeleteFunc(in.external(), func(ref Ref) bool { return ref.Origin == "/$schema" })
	if len(external) > 0 || dependsOnBase(in) {
		return reject(ReasonUnsupportedDialect, "%s uses $schema %q, which cannot be bundled and verified", n.doc.URI, n.dialect)
	}

	return nil
}

// prepareRoot gives the CLI copy of the root, data, an absolute
// identifier. It reports whether the root already declared one; a root that
// did not relies on its retrieval URI for every non-fragment reference.
func (r *resolver) prepareRoot(n *node, data []byte, tl topLevel, base *url.URL, d dialect) (bool, error) {
	id, present, err := tl.stringMember(data, d.idKey)
	if err != nil {
		return false, rejectWrap(ReasonInvalidSchema, err, "%s", n.doc.URI)
	}

	target := n.doc.URI

	if present {
		ref, parseErr := url.Parse(id)
		if parseErr == nil && ref.IsAbs() && ref.Fragment == "" {
			n.prepared = data

			return true, nil
		}

		target, err = resolveID(base, id)
		if err != nil {
			return false, rejectWrap(ReasonIDMismatch, err, "root %s", n.doc.URI)
		}
	}

	n.prepared, err = setStringMember(data, tl, d.idKey, target)
	if err != nil {
		return false, fault.Wrap(fault.Internal, err, "set root identifier")
	}

	return false, nil
}

func (r *resolver) admitDependency(ctx context.Context, uri string) (*node, error) {
	if dependencies := len(r.nodes) - 1; dependencies >= r.limits.MaxDocuments {
		return nil, reject(ReasonDependencyLimit, "fetching %s would exceed %d dependency documents", uri, r.limits.MaxDocuments)
	}

	doc, err := r.fetch(ctx, uri)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fault.Wrap(fault.KindOf(ctxErr), ctxErr, "fetch %s", uri)
		}

		return nil, rejectWrap(ReasonUnresolvedRef, err, "cannot fetch %s", uri)
	}

	doc.URI = uri

	n, err := r.admitBytes(doc)
	if err != nil {
		return nil, err
	}

	r.known[uri] = struct{}{}

	defaultDialect, err := r.prepareDependency(n)
	if err != nil {
		return nil, err
	}

	in, err := r.inspectNode(ctx, n, defaultDialect)
	if err != nil {
		return nil, err
	}

	if err := collectBases(n, in); err != nil {
		return nil, err
	}

	return n, nil
}

// prepareDependency checks a fetched document and gives its CLI copy the
// identifier it was requested under. It returns the dialect to assume when
// the document declares none: the root's, as the bundle embeds it that way.
func (r *resolver) prepareDependency(n *node) (string, error) {
	uri := n.doc.URI

	tl, err := scanTopLevel(n.doc.Bytes)
	if err != nil {
		return "", fault.Wrap(fault.Internal, err, "index %s", uri)
	}

	if !tl.isObject {
		return "", reject(ReasonUnresolvedRef, "%s is not a JSON object, so it cannot carry the identifier bundling needs", uri)
	}

	schema, present, err := tl.stringMember(n.doc.Bytes, "$schema")
	if err != nil {
		return "", rejectWrap(ReasonInvalidSchema, err, "%s", uri)
	}

	defaultDialect := ""
	if !present {
		schema = r.nodes[0].dialect
		defaultDialect = schema
	}

	n.dialect = schema

	d, ok := lookupDialect(schema)
	if !ok {
		return "", reject(ReasonUnsupportedDialect, "%s uses $schema %q, which cannot be bundled and verified", uri, schema)
	}

	data := n.doc.Bytes

	if d.legacy && hasMember(tl, "$ref") {
		if data, tl, err = rewriteLegacyRef(data, tl, uri, schema); err != nil {
			return "", err
		}
	}

	id, present, err := tl.stringMember(data, d.idKey)
	if err != nil {
		return "", rejectWrap(ReasonInvalidSchema, err, "%s", uri)
	}

	if present {
		_, base, err := retrievalBase(uri)
		if err != nil {
			return "", rejectWrap(ReasonUnresolvedRef, err, "%s", uri)
		}

		resolved, err := resolveID(base, id)
		if err != nil {
			return "", rejectWrap(ReasonIDMismatch, err, "%s", uri)
		}

		if canonicalURI(resolved) != canonicalURI(uri) {
			return "", reject(ReasonIDMismatch, "%s declares %s %q (%s) instead of the URI it was fetched under", uri, d.idKey, id, resolved)
		}

		if ref, err := url.Parse(id); err == nil && ref.IsAbs() {
			n.prepared = data

			return defaultDialect, nil
		}
	}

	n.prepared, err = setStringMember(data, tl, d.idKey, uri)
	if err != nil {
		return "", fault.Wrap(fault.Internal, err, "set identifier of %s", uri)
	}

	return defaultDialect, nil
}

// admitBytes enforces the size limits, strict JSON, the digest and the
// absence of local metaschemas, and registers the document as part of the
// closure. Every document passes here before the CLI sees it.
func (r *resolver) admitBytes(doc Document) (*node, error) {
	size := int64(len(doc.Bytes))

	if size > r.limits.MaxDocumentBytes {
		return nil, reject(ReasonDependencyLimit, "%s has %d bytes, more than the limit of %d", doc.URI, size, r.limits.MaxDocumentBytes)
	}

	if r.total+size > r.limits.MaxTotalBytes {
		return nil, reject(ReasonDependencyLimit, "adding %s exceeds the closure limit of %d bytes", doc.URI, r.limits.MaxTotalBytes)
	}

	if err := jsonutil.Check(doc.Bytes, jsonutil.DefaultMaxDepth); err != nil {
		return nil, rejectWrap(ReasonInvalidJSON, err, "%s", doc.URI)
	}

	sum := digest.FromBytes(doc.Bytes)
	if doc.Digest != "" && doc.Digest != sum {
		return nil, fault.New(fault.Integrity, "digest %s of %s does not match its content (%s)", doc.Digest, doc.URI, sum)
	}

	if err := checkLocalMetaschema(doc.URI, doc.Bytes); err != nil {
		return nil, err
	}

	doc.Digest = sum
	r.total += size

	n := &node{doc: doc}
	r.nodes = append(r.nodes, n)

	return n, nil
}

// inspectNode runs the CLI on the prepared copy and records the resources it
// declares as known.
func (r *resolver) inspectNode(ctx context.Context, n *node, defaultDialect string) (*inspection, error) {
	file, err := r.ws.write(n.doc.URI, n.prepared)
	if err != nil {
		return nil, err
	}

	in, err := r.tool.inspect(ctx, r.ws, file, defaultDialect)
	if err != nil {
		return nil, err
	}

	for resource := range in.resources {
		r.known[resource] = struct{}{}
	}

	return in, nil
}

// collectBases records the external resources n needs fetched. Official
// metaschemas are left out: every validator carries them, and Prepare removes
// the copies the CLI embeds for them.
func collectBases(n *node, in *inspection) error {
	for _, ref := range in.external() {
		if isWellKnownMetaschema(ref.Base) {
			continue
		}

		if _, _, err := retrievalBase(ref.Base); err != nil {
			return rejectWrap(ReasonUnresolvedRef, err, "reference at %s in %s", ref.Origin, n.doc.URI)
		}

		n.bases = append(n.bases, ref.Base)
	}

	return nil
}

func checkLocalMetaschema(uri string, data []byte) error {
	value, found, err := localMetaschema(data)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "scan %s", uri)
	}

	if found {
		return reject(ReasonUnsupportedDialect, "%s declares $schema %q, a metaschema on the local file system", uri, value)
	}

	return nil
}

// dependsOnBase reports whether a reference in the document is written
// relative to, or as, the document's own location, so that it only resolves
// once the document carries an absolute identifier. References to official
// metaschemas resolve anywhere.
func dependsOnBase(in *inspection) bool {
	for _, ref := range in.refs {
		if !isSchemaKeyword(ref.Origin) && !strings.HasPrefix(ref.Original, "#") && !isWellKnownMetaschema(ref.Base) {
			return true
		}
	}

	return false
}

func hasMember(tl topLevel, key string) bool {
	_, ok := tl.members[key]

	return ok
}

func isBoolean(data []byte) bool {
	trimmed := bytes.TrimSpace(data)

	return bytes.Equal(trimmed, []byte("true")) || bytes.Equal(trimmed, []byte("false"))
}
