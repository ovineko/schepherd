// Package state reads and writes catalog/state.json, the record in Git of
// what has been published (docs/publishing.md). Per schema it holds the
// exact catalog entry, the digests of the prepared schema bytes and of its
// notice text, the license decision the entry was published with, the
// revisions that added it, gave it its artifact and last changed it, and
// why the current catalog keeps it without refreshing it (a hold) or no
// longer lists it (an exclusion); globally the upstream input, the prepare
// recipe and the current catalog. The publisher decides from it alone
// whether anything changed, which artifacts and license decisions to reuse
// and which IDs stay reserved, so the registry never has to be read back for
// that.
//
// The file is canonical: fixed member order, catalog entries encoded as in
// catalog.json, two-space indentation and a final newline. It therefore
// changes only when its content does, and Parse accepts nothing else.
package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/calver"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/jsonutil"
	"github.com/ovineko/schepherd/internal/publisher/policy"
)

// FormatVersion is the only state format this build reads and writes.
const FormatVersion = 1

// Source kinds, as in prepared.json.
const (
	KindSchemaStore = "schemastore"
	KindLocal       = "local"
)

// SchemaStoreRepository is the upstream repository of a schemastore source.
const SchemaStoreRepository = "https://github.com/SchemaStore/schemastore"

const (
	maxFileBytes   = 64 << 20
	maxDepth       = 32
	maxEntries     = 20000
	maxRecipeBytes = 256
	maxNameBytes   = 256

	maxRepositoryBytes = 1024
	maxDecisionRules   = 1025
	maxDetections      = 1025
	maxRedirects       = 1025
	maxDetectionText   = 512
	maxDetectionURL    = 4096
)

// Reasons why the catalog keeps a published schema with its last entry and
// artifact instead of refreshing it (Schema.HeldReason).
const (
	// HeldRemovedUpstream: no upstream record maps to the schema's ID any
	// more (the source was removed, or an ID override moved it elsewhere).
	HeldRemovedUpstream = "removed-upstream"
	// HeldFetchFailed: the source or a dependency could not be downloaded.
	HeldFetchFailed = "fetch-failed"
	// HeldLicenseDetectionFailed: automatic license detection could not
	// answer (a failed request, a rate limit, a host it cannot pin).
	HeldLicenseDetectionFailed = "license-detection-failed"
	// HeldLicenseRefused: automatic license detection found a license the
	// policy does not accept, or none.
	HeldLicenseRefused = "license-refused"
	// HeldLicenseReview: the license policy holds the source for review (a
	// review rule, no rule at all, a refused redirect, oversized notices).
	HeldLicenseReview = "license-review"
	// HeldPrepareFailed: the upstream catalog entry is malformed, or
	// bundling, compiling, verification, the catalog's metadata rules or the
	// clients' artifact limits rejected the new upstream content.
	HeldPrepareFailed = "prepare-failed"
)

// HeldReasons lists every valid Schema.HeldReason.
var HeldReasons = []string{
	HeldRemovedUpstream, HeldFetchFailed, HeldLicenseDetectionFailed, HeldLicenseRefused, HeldLicenseReview, HeldPrepareFailed,
}

var (
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	textPattern   = regexp.MustCompile(`^[\x21-\x7e](?:[\x20-\x7e]*[\x21-\x7e])?$`)
	// repositoryPattern admits printable ASCII without spaces, so the URL
	// needs no further escaping wherever it is shown or linked.
	repositoryPattern = regexp.MustCompile(`^[\x21-\x7e]+$`)
)

// State is the decoded state file. Schemas are sorted by ID.
type State struct {
	Source        Source
	Recipe        string
	Catalog       Catalog
	Schemas       []Schema
	FormatVersion int
}

// Source identifies the upstream input of the current catalog. A
// schemastore source has Repository, Commit and TarballDigest; a local
// source only Name.
type Source struct {
	Kind          string `json:"kind"`
	Repository    string `json:"repository,omitempty"`
	Name          string `json:"name,omitempty"`
	Commit        string `json:"commit,omitempty"`
	TarballDigest string `json:"tarballDigest,omitempty"`
}

// Catalog names the current catalog: its revision and the digest and size
// of its OCI image index, which is what clients pin.
type Catalog struct {
	Revision string `json:"revision"`
	Digest   string `json:"digest"`
	Size     int64  `json:"size"`
}

// Schema is the record of one published schema. ContentDigest and
// NoticeDigest (empty when the artifact has no notice layer) are the digests
// of the prepared schema bytes and of the notice text: together they are
// the content the published artifact carries. License is the decision the
// entry was published with, which a later preparation of unchanged content
// reuses instead of deciding again.
//
// FirstRevision added the schema to the catalog (again, after an
// exclusion), ArtifactRevision gave the entry its current artifact and
// LastChangedRevision last changed the entry in any way. ArtifactRevision is
// empty when it equals LastChangedRevision, that is unless the last change
// only touched metadata.
//
// A held schema (HeldSinceRevision and HeldReason) stays in the catalog with
// its last entry and artifact because it could not be refreshed;
// HeldSinceRevision is the first published revision that kept it so. An
// excluded schema (ExcludedRevision) left the catalog by an explicit exclude
// rule: its record stays so that its ID remains reserved, and Entry is the
// entry it last had.
type Schema struct {
	ID                  string
	ContentDigest       string
	NoticeDigest        string
	FirstRevision       string
	ArtifactRevision    string
	LastChangedRevision string
	HeldSinceRevision   string
	HeldReason          string
	ExcludedRevision    string
	License             LicenseDecision
	Entry               catalog.Entry
}

// LicenseDecision records why a license policy allowed a schema: the IDs of
// the rules that allowed its source and dependencies, sorted, and what
// automatic license detection found for the others, sorted by URL. Both are
// empty for a license a local source file declares. Redirects lists the
// documents of the schema that another URL served through an HTTP redirect,
// sorted by URL; the rules and detections cover those targets as well, so
// that the decision can be checked again without asking detection.
type LicenseDecision struct {
	Rules      []string           `json:"rules,omitempty"`
	Detections []policy.Detection `json:"detections,omitempty"`
	Redirects  []Redirect         `json:"redirects,omitempty"`
}

// Redirect records that the request for URL, the retrieval URL of the root
// document or a dependency's source, was redirected to Target, which served
// the bytes of Digest. Both are http(s) URLs without fragment.
type Redirect struct {
	URL    string `json:"url"`
	Target string `json:"target"`
	Digest string `json:"digest"`
}

// IsZero reports whether the decision records nothing.
func (d *LicenseDecision) IsZero() bool {
	return len(d.Rules) == 0 && len(d.Detections) == 0 && len(d.Redirects) == 0
}

// Equal reports whether two decisions record the same rules, findings and
// redirects.
func (d *LicenseDecision) Equal(other *LicenseDecision) bool {
	return slices.Equal(d.Rules, other.Rules) && slices.Equal(d.Detections, other.Detections) && slices.Equal(d.Redirects, other.Redirects)
}

// Validate checks the decision: rule IDs valid, sorted and distinct;
// detections allowing, sorted by URL, distinct, with a pinned source and an
// asserted license; redirects sorted by URL, distinct, each to another URL
// and with a valid digest.
func (d *LicenseDecision) Validate() error {
	if len(d.Rules) > maxDecisionRules || len(d.Detections) > maxDetections || len(d.Redirects) > maxRedirects {
		return fmt.Errorf("at most %d rules, %d detections and %d redirects", maxDecisionRules, maxDetections, maxRedirects)
	}

	for i, id := range d.Rules {
		if !policy.ValidRuleID(id) {
			return fmt.Errorf("rule %q is not a valid rule ID", clipText(id))
		}

		if i > 0 && d.Rules[i-1] >= id {
			return fmt.Errorf("rules must be sorted without duplicates (%q)", id)
		}
	}

	for i := range d.Detections {
		det := &d.Detections[i]
		if err := checkDetection(det); err != nil {
			return fmt.Errorf("detection %d: %w", i, err)
		}

		if i > 0 && d.Detections[i-1].URL >= det.URL {
			return fmt.Errorf("detections must be sorted by url without duplicates (%q)", clipText(det.URL))
		}
	}

	for i := range d.Redirects {
		r := &d.Redirects[i]
		if err := r.validate(); err != nil {
			return fmt.Errorf("redirect %d: %w", i, err)
		}

		if i > 0 && d.Redirects[i-1].URL >= r.URL {
			return fmt.Errorf("redirects must be sorted by url without duplicates (%q)", clipText(r.URL))
		}
	}

	return nil
}

func (r *Redirect) validate() error {
	for _, u := range []struct{ name, value string }{{"url", r.URL}, {"target", r.Target}} {
		if !httpURL(u.value) || strings.Contains(u.value, "#") {
			return fmt.Errorf("%s %q must be an http(s) URL of printable ASCII without credentials or fragment", u.name, clipText(u.value))
		}
	}

	if r.Target == r.URL {
		return errors.New("target must differ from url")
	}

	if err := digest.Validate(r.Digest); err != nil {
		return fmt.Errorf("digest: %w", err)
	}

	return nil
}

// httpURL reports whether raw is an http(s) URL with a host, of printable
// ASCII and without credentials.
func httpURL(raw string) bool {
	u, err := url.Parse(raw)

	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil &&
		len(raw) <= maxDetectionURL && repositoryPattern.MatchString(raw)
}

func checkDetection(det *policy.Detection) error {
	if !httpURL(det.URL) {
		return fmt.Errorf("url %q must be an http(s) URL of printable ASCII without credentials", clipText(det.URL))
	}

	if det.Verdict != policy.Allow || det.Reason != "" {
		return errors.New("only allowing detections are recorded: verdict must be allow and reason empty")
	}

	if err := policy.CheckAssertedLicense(det.License); err != nil {
		return fmt.Errorf("license: %w", err)
	}

	if len(det.Source) > maxDetectionText || !textPattern.MatchString(det.Source) {
		return fmt.Errorf("source must be printable ASCII of 1-%d bytes", maxDetectionText)
	}

	for _, file := range []struct{ name, value, digest string }{
		{"licenseFile", det.LicenseFile, det.LicenseDigest}, {"noticeFile", det.NoticeFile, det.NoticeDigest},
	} {
		if (file.value == "") != (file.digest == "") {
			return fmt.Errorf("%s and its digest must be given together", file.name)
		}

		if file.value == "" {
			continue
		}

		if len(file.value) > maxDetectionText || !textPattern.MatchString(file.value) {
			return fmt.Errorf("%s must be printable ASCII of 1-%d bytes", file.name, maxDetectionText)
		}

		if err := digest.Validate(file.digest); err != nil {
			return fmt.Errorf("%s digest: %w", file.name, err)
		}
	}

	if det.LicenseFile == "" {
		return errors.New("an allowing detection names the license file it read")
	}

	return nil
}

// Held reports whether the catalog keeps the schema without refreshing it.
func (rec *Schema) Held() bool {
	return rec.HeldSinceRevision != ""
}

// Excluded reports whether an exclude rule removed the schema from the
// catalog.
func (rec *Schema) Excluded() bool {
	return rec.ExcludedRevision != ""
}

// ArtifactSince returns the revision that gave the entry its artifact.
func (rec *Schema) ArtifactSince() string {
	if rec.ArtifactRevision != "" {
		return rec.ArtifactRevision
	}

	return rec.LastChangedRevision
}

type wireState struct {
	FormatVersion int          `json:"formatVersion"`
	Source        Source       `json:"source"`
	Recipe        string       `json:"recipe"`
	Catalog       Catalog      `json:"catalog"`
	Schemas       []wireSchema `json:"schemas"`
}

type wireSchema struct {
	ID                  string          `json:"id"`
	ContentDigest       string          `json:"contentDigest"`
	NoticeDigest        string          `json:"noticeDigest,omitempty"`
	FirstRevision       string          `json:"firstRevision"`
	ArtifactRevision    string          `json:"artifactRevision,omitempty"`
	LastChangedRevision string          `json:"lastChangedRevision"`
	HeldSinceRevision   string          `json:"heldSinceRevision,omitempty"`
	HeldReason          string          `json:"heldReason,omitempty"`
	ExcludedRevision    string          `json:"excludedRevision,omitempty"`
	LicenseDecision     LicenseDecision `json:"licenseDecision,omitzero"`
	Entry               json.RawMessage `json:"entry"`
}

type decodedState struct {
	FormatVersion int             `json:"formatVersion"`
	Source        Source          `json:"source"`
	Recipe        string          `json:"recipe"`
	Catalog       Catalog         `json:"catalog"`
	Schemas       []decodedSchema `json:"schemas"`
}

type decodedSchema struct {
	ID                  string          `json:"id"`
	ContentDigest       string          `json:"contentDigest"`
	NoticeDigest        string          `json:"noticeDigest"`
	FirstRevision       string          `json:"firstRevision"`
	ArtifactRevision    string          `json:"artifactRevision"`
	LastChangedRevision string          `json:"lastChangedRevision"`
	HeldSinceRevision   string          `json:"heldSinceRevision"`
	HeldReason          string          `json:"heldReason"`
	ExcludedRevision    string          `json:"excludedRevision"`
	LicenseDecision     LicenseDecision `json:"licenseDecision"`
	Entry               catalog.Entry   `json:"entry"`
}

// Encode validates s and returns the canonical file bytes.
func Encode(s *State) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}

	wire := wireState{
		FormatVersion: s.FormatVersion, Source: s.Source, Recipe: s.Recipe, Catalog: s.Catalog,
		Schemas: make([]wireSchema, 0, len(s.Schemas)),
	}

	for i := range s.Schemas {
		rec := &s.Schemas[i]

		entry, err := catalog.MarshalEntry(&rec.Entry)
		if err != nil {
			return nil, fault.Wrap(fault.Internal, err, "encode state")
		}

		wire.Schemas = append(wire.Schemas, wireSchema{
			ID: rec.ID, ContentDigest: rec.ContentDigest, NoticeDigest: rec.NoticeDigest, FirstRevision: rec.FirstRevision,
			ArtifactRevision: rec.ArtifactRevision, LastChangedRevision: rec.LastChangedRevision,
			HeldSinceRevision: rec.HeldSinceRevision, HeldReason: rec.HeldReason, ExcludedRevision: rec.ExcludedRevision,
			LicenseDecision: rec.License, Entry: entry,
		})
	}

	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")

	if err := enc.Encode(&wire); err != nil {
		return nil, fault.Wrap(fault.Internal, err, "encode state")
	}

	return buf.Bytes(), nil
}

// Parse validates a state file. Only the canonical form Encode writes is
// accepted, which rules out unknown, duplicate, differently cased, null and
// empty optional members. A formatVersion other than 1 is fault.Usage,
// every other defect fault.Integrity.
func Parse(data []byte) (*State, error) {
	if err := jsonutil.Check(data, maxDepth); err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "invalid state")
	}

	var probe struct {
		FormatVersion json.RawMessage `json:"formatVersion"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "invalid state")
	}

	if string(probe.FormatVersion) != "1" {
		return nil, fault.New(fault.Usage, "the state has formatVersion %s; this build reads %d", clip(probe.FormatVersion), FormatVersion)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var doc decodedState
	if err := dec.Decode(&doc); err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "invalid state")
	}

	s := &State{
		FormatVersion: doc.FormatVersion, Source: doc.Source, Recipe: doc.Recipe, Catalog: doc.Catalog,
		Schemas: make([]Schema, 0, len(doc.Schemas)),
	}

	for _, rec := range doc.Schemas {
		s.Schemas = append(s.Schemas, Schema{
			ID: rec.ID, ContentDigest: rec.ContentDigest, NoticeDigest: rec.NoticeDigest, FirstRevision: rec.FirstRevision,
			ArtifactRevision: rec.ArtifactRevision, LastChangedRevision: rec.LastChangedRevision,
			HeldSinceRevision: rec.HeldSinceRevision, HeldReason: rec.HeldReason, ExcludedRevision: rec.ExcludedRevision,
			License: rec.LicenseDecision, Entry: rec.Entry,
		})
	}

	canonical, err := Encode(s)
	if err != nil {
		return nil, err
	}

	if !bytes.Equal(canonical, data) {
		return nil, fault.New(fault.Integrity, "invalid state: not in the canonical form written by the publisher")
	}

	return s, nil
}

func clip(raw json.RawMessage) string {
	const limit = 64

	switch {
	case len(raw) == 0:
		return "(missing)"
	case len(raw) > limit:
		return string(raw[:limit]) + "..."
	default:
		return string(raw)
	}
}

// Validate checks every rule of the format. Errors are fault.Integrity.
func (s *State) Validate() error {
	if err := s.validate(); err != nil {
		return fault.Wrap(fault.Integrity, err, "invalid state")
	}

	return nil
}

func (src *Source) validate() error {
	switch src.Kind {
	case KindSchemaStore:
		if src.Name != "" || !commitPattern.MatchString(src.Commit) {
			return errors.New("a schemastore source needs a repository, a 40-hex commit, a tarballDigest and no name")
		}

		if err := checkRepository(src.Repository); err != nil {
			return err
		}

		if err := digest.Validate(src.TarballDigest); err != nil {
			return fmt.Errorf("tarballDigest: %w", err)
		}
	case KindLocal:
		if src.Repository != "" || src.Commit != "" || src.TarballDigest != "" {
			return errors.New("a local source has only a name")
		}

		if len(src.Name) > maxNameBytes || !textPattern.MatchString(src.Name) {
			return fmt.Errorf("name must be printable ASCII of 1-%d bytes", maxNameBytes)
		}
	default:
		return fmt.Errorf("unknown kind %q", clipText(src.Kind))
	}

	return nil
}

func checkRepository(repository string) error {
	u, err := url.Parse(repository)
	if err != nil || !strings.HasPrefix(repository, "https://") || u.Host == "" || u.User != nil ||
		len(repository) > maxRepositoryBytes || strings.ContainsAny(repository, "?#") || !repositoryPattern.MatchString(repository) {
		return fmt.Errorf("repository %q must be an https URL of printable ASCII without credentials, query or fragment", clipText(repository))
	}

	return nil
}

func (c *Catalog) validate() (calver.Revision, error) {
	revision, err := calver.ParseRevision(c.Revision)
	if err != nil {
		return calver.Revision{}, fmt.Errorf("revision: %w", err)
	}

	if err := digest.Validate(c.Digest); err != nil {
		return calver.Revision{}, fmt.Errorf("digest: %w", err)
	}

	if limit := artifact.DefaultLimits().MaxManifestBytes; c.Size <= 0 || c.Size > limit {
		return calver.Revision{}, fmt.Errorf("index size %d is outside 1-%d", c.Size, limit)
	}

	return revision, nil
}

func (rec *Schema) validate(current calver.Revision) error {
	if rec.ID != rec.Entry.ID {
		return fmt.Errorf("the entry has id %q", clipText(rec.Entry.ID))
	}

	if err := digest.Validate(rec.ContentDigest); err != nil {
		return fmt.Errorf("contentDigest: %w", err)
	}

	if rec.NoticeDigest != "" {
		if err := digest.Validate(rec.NoticeDigest); err != nil {
			return fmt.Errorf("noticeDigest: %w", err)
		}
	}

	if rec.Entry.Provenance == nil {
		return errors.New("the entry has no provenance, so its source cannot keep its ID")
	}

	if err := rec.License.Validate(); err != nil {
		return fmt.Errorf("licenseDecision: %w", err)
	}

	changed, err := rec.validateRevisions(current)
	if err != nil {
		return err
	}

	if rec.Held() || rec.HeldReason != "" {
		if err := rec.validateHold(changed, current); err != nil {
			return err
		}
	}

	if !rec.Excluded() {
		return nil
	}

	excluded, err := calver.ParseRevision(rec.ExcludedRevision)
	if err != nil {
		return fmt.Errorf("excludedRevision: %w", err)
	}

	switch {
	case rec.Held():
		return errors.New("an excluded schema is not in the catalog, so it cannot be held there")
	case calver.Compare(changed, excluded) >= 0 || calver.Compare(excluded, current) > 0:
		return fmt.Errorf("revisions must satisfy lastChangedRevision < excludedRevision <= catalog.revision (%s, %s, %s)", changed, excluded, current)
	}

	return nil
}

// validateRevisions checks firstRevision <= artifactRevision <
// lastChangedRevision <= catalog.revision and returns lastChangedRevision.
func (rec *Schema) validateRevisions(current calver.Revision) (calver.Revision, error) {
	first, err := calver.ParseRevision(rec.FirstRevision)
	if err != nil {
		return calver.Revision{}, fmt.Errorf("firstRevision: %w", err)
	}

	changed, err := calver.ParseRevision(rec.LastChangedRevision)
	if err != nil {
		return calver.Revision{}, fmt.Errorf("lastChangedRevision: %w", err)
	}

	if calver.Compare(first, changed) > 0 || calver.Compare(changed, current) > 0 {
		return calver.Revision{}, fmt.Errorf("revisions must satisfy firstRevision <= lastChangedRevision <= catalog.revision (%s, %s, %s)",
			first, changed, current)
	}

	if rec.ArtifactRevision == "" {
		return changed, nil
	}

	artifact, err := calver.ParseRevision(rec.ArtifactRevision)
	if err != nil {
		return calver.Revision{}, fmt.Errorf("artifactRevision: %w", err)
	}

	// artifactRevision is left out when it equals lastChangedRevision, so a
	// present one always names an older revision.
	if calver.Compare(first, artifact) > 0 || calver.Compare(artifact, changed) >= 0 {
		return calver.Revision{}, fmt.Errorf("revisions must satisfy firstRevision <= artifactRevision < lastChangedRevision (%s, %s, %s)",
			first, artifact, changed)
	}

	return changed, nil
}

func (rec *Schema) validateHold(changed, current calver.Revision) error {
	if !slices.Contains(HeldReasons, rec.HeldReason) {
		return fmt.Errorf("heldReason %q must be one of %s", clipText(rec.HeldReason), strings.Join(HeldReasons, ", "))
	}

	held, err := calver.ParseRevision(rec.HeldSinceRevision)
	if err != nil {
		return fmt.Errorf("heldSinceRevision: %w", err)
	}

	// A held entry is never changed: any change ends the hold. So the hold
	// always began after the revision that last changed the entry.
	if calver.Compare(changed, held) >= 0 || calver.Compare(held, current) > 0 {
		return fmt.Errorf("revisions must satisfy lastChangedRevision < heldSinceRevision <= catalog.revision (%s, %s, %s)", changed, held, current)
	}

	return nil
}

func clipText(s string) string {
	const limit = 64

	if len(s) <= limit {
		return s
	}

	return s[:limit] + "..."
}

// Entries returns the entries of the current catalog in ID order: every
// schema that is not excluded.
func (s *State) Entries() []catalog.Entry {
	entries := make([]catalog.Entry, 0, len(s.Schemas))
	for i := range s.Schemas {
		if !s.Schemas[i].Excluded() {
			entries = append(entries, s.Schemas[i].Entry)
		}
	}

	return entries
}

// Lookup binary-searches the ID-sorted records.
func (s *State) Lookup(id string) (*Schema, bool) {
	i, found := slices.BinarySearchFunc(s.Schemas, id, func(rec Schema, target string) int {
		return strings.Compare(rec.ID, target)
	})
	if !found {
		return nil, false
	}

	return &s.Schemas[i], true
}

func (s *State) excludedEntries() []catalog.Entry {
	var entries []catalog.Entry

	for i := range s.Schemas {
		if s.Schemas[i].Excluded() {
			entries = append(entries, s.Schemas[i].Entry)
		}
	}

	return entries
}

func (s *State) validate() error {
	if s.FormatVersion != FormatVersion {
		return fmt.Errorf("formatVersion %d is not %d", s.FormatVersion, FormatVersion)
	}

	if err := s.Source.validate(); err != nil {
		return fmt.Errorf("source: %w", err)
	}

	if len(s.Recipe) > maxRecipeBytes || !textPattern.MatchString(s.Recipe) {
		return fmt.Errorf("recipe must be printable ASCII of 1-%d bytes", maxRecipeBytes)
	}

	current, err := s.Catalog.validate()
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}

	if len(s.Schemas) > maxEntries {
		return fmt.Errorf("%d schemas exceed the limit of %d", len(s.Schemas), maxEntries)
	}

	for i := range s.Schemas {
		if err := s.Schemas[i].validate(current); err != nil {
			return fmt.Errorf("schema %d (%q): %w", i, clipText(s.Schemas[i].ID), err)
		}

		if i > 0 && s.Schemas[i-1].ID >= s.Schemas[i].ID {
			if s.Schemas[i-1].ID == s.Schemas[i].ID {
				return fmt.Errorf("duplicate schema id %q", clipText(s.Schemas[i].ID))
			}

			return fmt.Errorf("schemas must be sorted by id (%q before %q)", clipText(s.Schemas[i-1].ID), clipText(s.Schemas[i].ID))
		}
	}

	c := catalog.Catalog{FormatVersion: catalog.FormatVersion, Revision: s.Catalog.Revision, Schemas: s.Entries()}
	if err := c.Validate(catalog.DefaultLimits()); err != nil {
		return fmt.Errorf("the entries do not form a valid catalog: %w", err)
	}

	if excluded := s.excludedEntries(); len(excluded) > 0 {
		c.Schemas = excluded
		if err := c.Validate(catalog.DefaultLimits()); err != nil {
			return fmt.Errorf("the entries of excluded schemas are invalid: %w", err)
		}
	}

	return nil
}

// Load reads the state file at path. A file that does not exist means that
// nothing has been published yet: Load returns nil and no error. An
// unreadable file is fault.Usage, invalid content as in Parse.
func Load(path string) (*State, error) {
	f, err := os.Open(filepath.Clean(path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil //nolint:nilnil // a missing file is no publication yet, not an error
	}

	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "state file")
	}

	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "state file %s", path)
	}

	if !info.Mode().IsRegular() {
		return nil, fault.New(fault.Usage, "state file %s is not a regular file", path)
	}

	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "read state file %s", path)
	}

	if len(data) > maxFileBytes {
		return nil, fault.New(fault.Integrity, "state file %s exceeds %d bytes", path, maxFileBytes)
	}

	s, err := Parse(data)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "state file %s", path)
	}

	return s, nil
}

// Save writes s to path through a temporary file in the same directory, so
// a reader never sees a partial file; missing parent directories are
// created.
func Save(path string, s *State) error {
	data, err := Encode(s)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fault.Wrap(fault.Usage, err, "write state file %s", path)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fault.Wrap(fault.Usage, err, "write state file %s", path)
	}

	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return fault.Wrap(fault.Internal, err, "write state file %s", path)
	}

	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()

		return fault.Wrap(fault.Internal, err, "write state file %s", path)
	}

	if err := tmp.Close(); err != nil {
		return fault.Wrap(fault.Internal, err, "write state file %s", path)
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		return fault.Wrap(fault.Usage, err, "write state file %s", path)
	}

	return nil
}
