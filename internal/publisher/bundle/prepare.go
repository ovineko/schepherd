package bundle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/jsonutil"
)

// outputFactor bounds the bundler's pretty-printed output relative to the
// size of its inputs; indentation typically doubles or triples compact JSON.
const (
	outputFactor  = 8
	outputMinimum = 16 << 20
)

// Instance is a test instance for the behaviour comparison. Only names with
// a .json extension are compared; other formats are counted as skipped.
type Instance struct {
	Name string
	Data []byte
}

// Dependency identifies a document embedded into a bundle.
type Dependency struct {
	URI    string
	Digest string
}

// Checked summarises the behaviour comparison. Valid and Invalid count the
// verdicts of the original schema, Agreed the instances on which the bundle
// returned the same verdict. All counters stay zero when nothing was bundled:
// the published bytes are then the upstream bytes.
type Checked struct {
	Valid          int
	Invalid        int
	Agreed         int
	SkippedNonJSON int
}

// Prepared is the schema to publish.
type Prepared struct {
	// Schema is compact JSON: the root byte-for-byte minus insignificant
	// whitespace when Bundled is false, the verified bundle otherwise.
	Schema []byte
	// Dialect is the root's $schema, or "".
	Dialect string
	// Dependencies are the embedded documents other than the root document
	// (which a fragment root embeds too), sorted by URI: exactly the
	// documents fetched for references, so the license gate sees all
	// embedded content.
	// Official metaschemas are never embedded; references to them keep
	// their official URIs, which validators resolve from built-in copies.
	Dependencies []Dependency
	Checked      Checked
	Bundled      bool
}

// Prepare produces the schema to publish from a closure returned by Resolve.
//
// A root that references nothing outside itself except official
// metaschemas is published as jsonutil.Compact of its bytes, without running
// the bundler. Otherwise the root and its dependencies are bundled with the
// CLI (offline, empty configuration), the metaschema copies the CLI embeds
// are removed, and the compacted result must: contain no local path, carry
// exactly the references and anchors of the documents it was made from,
// have no external reference other than to an official metaschema
// according to `inspect`, compile with a validator that may not load
// anything, and agree with the original schema (compiled against exactly
// the closure documents) on every JSON instance. Checked tells how many
// instances took part; with none, only the structural checks vouch for the
// bundle.
func Prepare(ctx context.Context, t *Tool, closure *Closure, instances []Instance) (*Prepared, error) {
	if t == nil || closure == nil || len(closure.nodes) == 0 {
		return nil, fault.New(fault.Internal, "bundle.Prepare needs a tool and a closure returned by Resolve")
	}

	if !closure.bundle {
		schema, err := jsonutil.Compact(closure.Root.Bytes, jsonutil.DefaultMaxDepth)
		if err != nil {
			return nil, rejectWrap(ReasonInvalidJSON, err, "%s", closure.Root.URI)
		}

		return &Prepared{Schema: schema, Dialect: closure.Dialect}, nil
	}

	schema, err := bundleClosure(ctx, t, closure)
	if err != nil {
		return nil, err
	}

	original, err := compileOriginal(closure)
	if err != nil {
		return nil, err
	}

	bundled, err := compileBundle(closure, schema)
	if err != nil {
		return nil, err
	}

	checked, err := compareBehaviour(original, bundled, instances)
	if err != nil {
		return nil, err
	}

	deps := make([]Dependency, 0, len(closure.Dependencies))
	for _, doc := range closure.Dependencies {
		deps = append(deps, Dependency{URI: doc.URI, Digest: doc.Digest})
	}

	return &Prepared{Schema: schema, Dialect: closure.Dialect, Dependencies: deps, Checked: checked, Bundled: true}, nil
}

func bundleClosure(ctx context.Context, t *Tool, closure *Closure) ([]byte, error) {
	ws, err := newWorkspace()
	if err != nil {
		return nil, err
	}

	defer ws.close()

	var (
		root  string
		deps  []string
		total int64
	)

	for i, n := range closure.nodes {
		file, err := ws.write(n.doc.URI, n.prepared)
		if err != nil {
			return nil, err
		}

		total += int64(len(n.prepared))

		if i == 0 {
			root = file
		} else {
			deps = append(deps, file)
		}
	}

	out, err := t.bundle(ctx, ws, root, deps, closure.Dialect, max(outputMinimum, outputFactor*total))
	if err != nil {
		return nil, closure.nameWrapper(err)
	}

	schema, err := jsonutil.Compact(out, jsonutil.DefaultMaxDepth)
	if err != nil {
		return nil, rejectWrap(ReasonBundlerError, err, "bundler output for %s is not strict JSON", closure.Root.URI)
	}

	schema, err = stripEmbeddedMetaschemas(schema, closure.nodes[0].prepared)
	if err != nil {
		return nil, rejectWrap(ReasonBundlerError, err, "bundler output for %s", closure.Root.URI)
	}

	if closure.wrapperID != "" {
		if schema, err = stripWrapperID(schema, closure); err != nil {
			return nil, err
		}
	}

	if err := checkLocalPaths(ws, closure, schema); err != nil {
		return nil, err
	}

	if err := checkInventory(closure, schema); err != nil {
		return nil, err
	}

	file, err := ws.write(closure.Root.URI, schema)
	if err != nil {
		return nil, err
	}

	in, err := t.inspect(ctx, ws, file, "")
	if err != nil {
		return nil, err
	}

	for _, ref := range in.external() {
		if !isWellKnownMetaschema(ref.Base) {
			return nil, reject(ReasonNotSelfContained, "bundle of %s still references %s at %s", closure.Root.URI, ref.Destination, ref.Origin)
		}
	}

	return schema, nil
}

// nameWrapper replaces the temporary identifier of a fragment root's
// wrapper in a rejection with the root URI the wrapper stands for.
func (c *Closure) nameWrapper(err error) error {
	if failure, ok := errors.AsType[*Failure](err); ok && c.wrapperID != "" {
		failure.Detail = strings.ReplaceAll(failure.Detail, c.wrapperID, c.Root.URI)
	}

	return err
}

// stripWrapperID removes the temporary identifier of a fragment root's
// wrapper from the bundle. The wrapper's only reference is absolute, so the
// root needs no identifier, and none that could collide with a location the
// schema is later loaded from is published.
func stripWrapperID(schema []byte, closure *Closure) ([]byte, error) {
	d, _ := lookupDialect(closure.Dialect)

	tl, err := scanTopLevel(schema)
	if err != nil {
		return nil, rejectWrap(ReasonBundlerError, err, "bundler output for %s", closure.Root.URI)
	}

	if id, _, err := tl.stringMember(schema, d.idKey); err != nil || id != closure.wrapperID {
		return nil, reject(ReasonBundlerError, "the bundle of %s does not keep the root identifier it was given", closure.Root.URI)
	}

	schema = withoutMembers(schema, tl, map[string]struct{}{d.idKey: {}})
	if bytes.Contains(schema, []byte(closure.wrapperID)) {
		return nil, reject(ReasonBundlerError, "the bundle of %s refers to the temporary root identifier", closure.Root.URI)
	}

	return schema, nil
}

// bundleContainers are the members the CLI embeds resources into:
// "definitions" up to draft-07, "$defs" from 2019-09.
var bundleContainers = []string{"definitions", "$defs"}

// stripEmbeddedMetaschemas removes the official metaschemas the CLI embeds
// for $ref (never for $schema) to one. Validators carry these themselves, and
// the CLI's copy is not identical to theirs (its identifiers lack the "#" of
// the draft-04/06/07 originals), so a validator that preloads metaschemas
// can reject the bundle for declaring a known identifier twice. Members the
// root itself declares in the container are always kept.
func stripEmbeddedMetaschemas(bundled, root []byte) ([]byte, error) {
	for _, container := range bundleContainers {
		tl, err := scanTopLevel(bundled)
		if err != nil {
			return nil, err
		}

		s, ok := tl.members[container]
		if !ok {
			continue
		}

		value := bundled[s.start:s.end]

		members, err := scanTopLevel(value)
		if err != nil {
			return nil, err
		}

		upstream, hadContainer, err := containerKeys(root, container)
		if err != nil {
			return nil, err
		}

		drop := map[string]struct{}{}

		for key := range members.members {
			if _, own := upstream[key]; !own && isWellKnownMetaschema(key) {
				drop[key] = struct{}{}
			}
		}

		switch {
		case len(drop) == 0:
		case len(drop) == len(members.members) && !hadContainer:
			bundled = withoutMembers(bundled, tl, map[string]struct{}{container: {}})
		default:
			bundled = replaceValue(bundled, s, withoutMembers(value, members, drop))
		}
	}

	return bundled, nil
}

func containerKeys(root []byte, container string) (map[string]struct{}, bool, error) {
	tl, err := scanTopLevel(root)
	if err != nil {
		return nil, false, err
	}

	s, ok := tl.members[container]
	if !ok {
		return nil, false, nil
	}

	members, err := scanTopLevel(root[s.start:s.end])
	if err != nil {
		return nil, false, err
	}

	keys := make(map[string]struct{}, len(members.members))
	for key := range members.members {
		keys[key] = struct{}{}
	}

	return keys, true, nil
}

// checkLocalPaths rejects output that mentions the workspace or carries more
// file:// URIs than the upstream documents did: the CLI injects file://
// identifiers for inputs it cannot otherwise name, which would leak build
// paths and make the output irreproducible. Counting instead of forbidding
// the scheme keeps descriptions that legitimately mention file:// URLs.
func checkLocalPaths(ws *workspace, closure *Closure, schema []byte) error {
	for _, dir := range []string{ws.dir, filepath.ToSlash(ws.dir)} {
		if bytes.Contains(schema, []byte(dir)) {
			return reject(ReasonBundlerError, "bundle of %s contains a local temporary path", closure.Root.URI)
		}
	}

	allowed := 0

	for _, n := range closure.nodes {
		count, err := countFileURIs(n.doc.Bytes)
		if err != nil {
			return fault.Wrap(fault.Internal, err, "scan %s", n.doc.URI)
		}

		allowed += count
	}

	got, err := countFileURIs(schema)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "scan bundle")
	}

	if got > allowed {
		return reject(ReasonBundlerError, "bundle of %s contains %d file:// URIs, its sources %d", closure.Root.URI, got, allowed)
	}

	return nil
}

// countFileURIs counts "file://" in decoded member names and string values,
// so escaped spellings such as file:\/\/ are counted like plain ones.
func countFileURIs(data []byte) (int, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	count := 0

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return count, nil
		}

		if err != nil {
			return 0, fmt.Errorf("scan strings: %w", err)
		}

		if s, ok := tok.(string); ok {
			count += strings.Count(s, "file://")
		}
	}
}

// closureLoader serves exactly the closure documents and nothing else. Its
// keys are canonical URIs: the documents were fetched under the CLI's
// normalized spelling, while the validator asks for the spelling a
// reference uses.
type closureLoader map[string]any

func (l closureLoader) Load(uri string) (any, error) {
	if doc, ok := l[canonicalURI(uri)]; ok {
		return doc, nil
	}

	return nil, fmt.Errorf("%s is not part of the dependency closure", uri)
}

// offlineLoader refuses every URL: a self-contained bundle needs none.
type offlineLoader struct{}

func (offlineLoader) Load(uri string) (any, error) {
	return nil, fmt.Errorf("a self-contained bundle must not need %s", uri)
}

func compileOriginal(closure *Closure) (*jsonschema.Schema, error) {
	loader := closureLoader{}

	for _, n := range closure.nodes[1:] {
		if n == closure.source {
			continue
		}

		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(n.doc.Bytes))
		if err != nil {
			return nil, rejectWrap(ReasonInvalidJSON, err, "%s", n.doc.URI)
		}

		loader[canonicalURI(n.doc.URI)] = doc
	}

	sch, err := compileTarget(closure.original, closure.target, closure.Root.Bytes, closure.Dialect, loader)
	if err != nil {
		if _, ok := errors.AsType[*jsonschema.LoadURLError](err); ok {
			return nil, rejectWrap(ReasonUnresolvedRef, err, "the verifier needs a resource outside the closure of %s", closure.Root.URI)
		}

		return nil, rejectWrap(ReasonInvalidSchema, err, "the verifier rejects %s", closure.Root.URI)
	}

	return sch, nil
}

func compileBundle(closure *Closure, schema []byte) (*jsonschema.Schema, error) {
	loc := closure.Root.URI
	if closure.wrapperID != "" {
		loc = closure.wrapperID
	}

	sch, err := compile(loc, schema, closure.Dialect, offlineLoader{})
	if err != nil {
		if loadErr, ok := errors.AsType[*jsonschema.LoadURLError](err); ok {
			if canonical := canonicalURI(loadErr.URL); canonical != loadErr.URL {
				return nil, rejectWrap(ReasonNotSelfContained, err,
					"the bundle of %s references %s but embeds that document as %s, the normalized form validators that compare identifiers literally do not match",
					closure.Root.URI, loadErr.URL, canonical)
			}

			return nil, rejectWrap(ReasonNotSelfContained, err, "the bundle of %s does not compile offline", closure.Root.URI)
		}

		return nil, rejectWrap(ReasonBundlerError, err, "the verifier rejects the bundle of %s", closure.Root.URI)
	}

	return sch, nil
}

func compile(loc string, data []byte, dialectURI string, loader jsonschema.URLLoader) (*jsonschema.Schema, error) {
	return compileTarget(loc, loc, data, dialectURI, loader)
}

// compileTarget adds data as the document at loc and compiles the schema at
// target, loc itself or loc with a fragment.
func compileTarget(loc, target string, data []byte, dialectURI string, loader jsonschema.URLLoader) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", loc, err)
	}

	c := jsonschema.NewCompiler()
	c.UseLoader(loader)
	c.UseRegexpEngine(compilePattern)

	if d, ok := lookupDialect(dialectURI); ok {
		c.DefaultDraft(d.draft)
	}

	if err := c.AddResource(loc, doc); err != nil {
		return nil, fmt.Errorf("add %s: %w", loc, err)
	}

	sch, err := c.Compile(target)
	if err != nil {
		return nil, fmt.Errorf("compile %s: %w", target, err)
	}

	return sch, nil
}

// unsupportedPattern stands in for an ECMA-262 pattern that Go's RE2 engine
// cannot compile (lookaround, backreferences). It matches everything; both
// sides of the comparison use the same engine, so the comparison stays
// symmetric while such schemas remain publishable.
type unsupportedPattern string

func (p unsupportedPattern) String() string {
	return string(p)
}

func (unsupportedPattern) MatchString(string) bool {
	return true
}

func compilePattern(pattern string) (jsonschema.Regexp, error) {
	if re := goRegexp(pattern); re != nil {
		return re, nil
	}

	return unsupportedPattern(pattern), nil
}

func goRegexp(pattern string) *regexp.Regexp {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}

	return re
}

func compareBehaviour(original, bundled *jsonschema.Schema, instances []Instance) (Checked, error) {
	var checked Checked

	for _, instance := range instances {
		if !strings.EqualFold(filepath.Ext(instance.Name), ".json") {
			checked.SkippedNonJSON++

			continue
		}

		if err := jsonutil.Check(instance.Data, jsonutil.DefaultMaxDepth); err != nil {
			return Checked{}, rejectWrap(ReasonInvalidInstance, err, "%s", instance.Name)
		}

		value, err := jsonschema.UnmarshalJSON(bytes.NewReader(instance.Data))
		if err != nil {
			return Checked{}, rejectWrap(ReasonInvalidInstance, err, "%s", instance.Name)
		}

		want := original.Validate(value) == nil
		got := bundled.Validate(value) == nil

		if want {
			checked.Valid++
		} else {
			checked.Invalid++
		}

		if want != got {
			return Checked{}, reject(ReasonBehaviourMismatch, "%s is %s for the original schema but %s for the bundle",
				instance.Name, verdict(want), verdict(got))
		}

		checked.Agreed++
	}

	return checked, nil
}

func verdict(valid bool) string {
	if valid {
		return "valid"
	}

	return "invalid"
}
