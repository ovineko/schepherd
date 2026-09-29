// Package catalog defines the Schepherd catalog document (formatVersion 2):
// the list of schema IDs, their fileMatch rules and the OCI manifest
// descriptors of the individual schema artifacts, which the catalog index
// references as well (package artifact). Descriptors are resolved
// against the repository the catalog itself was fetched from, which is why the
// document never contains registry hosts or repository names.
//
// Parse is strict: member names must match exactly (encoding/json alone would
// accept any letter case), unknown or duplicate members, null values and empty
// optional members are rejected, so every accepted document has exactly one
// meaning and one canonical encoding produced by Marshal.
package catalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/calver"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/jsonutil"
	"github.com/ovineko/schepherd/internal/match"
)

// FormatVersion is the only catalog format this build understands.
const FormatVersion = 2

// ManifestMediaType is the only descriptor media type allowed for entries.
const ManifestMediaType = "application/vnd.oci.image.manifest.v1+json"

const (
	maxIDLength          = 128
	maxNameBytes         = 512
	maxDescriptionBytes  = 8192
	maxPatternsPerEntry  = 256
	maxPatternBytes      = 1024
	maxURIBytes          = 4096
	maxLicenseBytes      = 256
	maxDependencies      = 1024
	maxQuotedBytes       = 64
	maxMessageBytes      = 256
	maxDigestBytes       = len(digest.Algorithm) + 1 + 2*sha256.Size
	defaultMaxEntries    = 20000
	defaultManifestBytes = 4 << 20

	// maxNesting is the depth encoding/json refuses beyond anyway. A lower
	// bound would reject a deeply nested document of a future format before
	// its version is read; format 2 needs no bound of its own because
	// checkStructure never descends below the few levels of its shape.
	maxNesting = 10000
)

var (
	idPattern      = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,126}[a-z0-9])?$`)
	versionLiteral = regexp.MustCompile(`^[1-9][0-9]*$`)
)

// Limits bounds what Parse and Validate accept. Zero or negative fields
// select the defaults: limits can be changed but never disabled.
type Limits struct {
	MaxEntries       int
	MaxManifestBytes int64
}

// DefaultLimits returns the limits used when none are configured.
func DefaultLimits() Limits {
	return Limits{MaxEntries: defaultMaxEntries, MaxManifestBytes: defaultManifestBytes}
}

func (l Limits) withDefaults() Limits {
	if l.MaxEntries <= 0 {
		l.MaxEntries = defaultMaxEntries
	}

	if l.MaxManifestBytes <= 0 {
		l.MaxManifestBytes = defaultManifestBytes
	}

	return l
}

// Catalog is the decoded formatVersion 2 document. Methods that search it
// rely on Schemas being sorted by ID, which Parse and Validate guarantee.
type Catalog struct {
	Revision      string  `json:"revision"`
	Schemas       []Entry `json:"schemas"`
	FormatVersion int     `json:"formatVersion"`
}

// Entry is one schema. Unset optional members are empty strings or nil
// slices: Marshal omits them, and Parse rejects them when present but empty.
type Entry struct {
	Provenance  *Provenance `json:"provenance,omitempty"`
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Dialect     string      `json:"dialect,omitempty"`
	FileMatch   []string    `json:"fileMatch,omitempty"`
	Artifact    Descriptor  `json:"artifact"`
}

// Descriptor is the OCI descriptor of a schema artifact manifest.
type Descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// Provenance records where a schema came from. It is informational only and
// is never used as a download location.
type Provenance struct {
	Source       string       `json:"source"`
	SourceDigest string       `json:"sourceDigest,omitempty"`
	License      string       `json:"license,omitempty"`
	Dependencies []Dependency `json:"dependencies,omitempty"`
}

// Dependency is an external resource embedded into a bundled schema.
type Dependency struct {
	Source string `json:"source"`
	Digest string `json:"digest"`
}

// Parse validates data as a formatVersion 2 catalog. Only well-formedness
// (UTF-8, JSON syntax, unique member names, the nesting encoding/json can
// read) is checked before the format version, so a well-formed document with
// an unsupported version is a fault.Usage error whatever else it contains.
// Every other defect is fault.Integrity.
func Parse(data []byte, limits Limits) (*Catalog, error) {
	limits = limits.withDefaults()

	if err := jsonutil.Check(data, maxNesting); err != nil {
		return nil, fault.Wrap(fault.Integrity, shortError{err}, "invalid catalog")
	}

	if err := checkFormatVersion(data); err != nil {
		return nil, err
	}

	if err := checkStructure(data, limits); err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "invalid catalog")
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var c Catalog
	if err := dec.Decode(&c); err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "invalid catalog")
	}

	if err := c.Validate(limits); err != nil {
		return nil, err
	}

	return &c, nil
}

// checkFormatVersion reads only the top-level formatVersion member, so a
// future format is recognized as unsupported whatever else it contains.
// Positive integer literals other than 2 name a format this build does not
// know; anything else (strings, 2.0, 0, null, a missing member) is malformed.
func checkFormatVersion(data []byte) error {
	if trimmed := bytes.TrimLeft(data, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return fault.New(fault.Integrity, "invalid catalog: the document must be a JSON object")
	}

	var top map[string]memberProbe
	if err := json.Unmarshal(data, &top); err != nil {
		return fault.Wrap(fault.Integrity, err, "invalid catalog")
	}

	version, ok := top["formatVersion"]

	switch {
	case !ok:
		return fault.New(fault.Integrity, "invalid catalog: formatVersion is missing")
	case version.text == strconv.Itoa(FormatVersion):
		return nil
	case version.positiveInteger:
		return fault.New(fault.Usage, "unsupported catalog formatVersion %s: this build supports %d", version.text, FormatVersion)
	default:
		return fault.New(fault.Integrity, "invalid catalog: formatVersion must be a positive integer, got %s", version.text)
	}
}

// memberProbe summarizes a top-level member without copying it, so probing
// the version of a large catalog does not duplicate its schemas list.
type memberProbe struct {
	text            string
	positiveInteger bool
}

func (p *memberProbe) UnmarshalJSON(data []byte) error {
	p.positiveInteger = versionLiteral.Match(data)
	p.text = jsonText(data[:min(len(data), maxQuotedBytes+1)])

	return nil
}

// jsonText renders a raw JSON value for an error message. Printable ASCII is
// kept so the text still reads as the literal the document contains; every
// other character, including the C1 and bidirectional controls JSON allows
// unescaped inside strings, becomes a JSON \u escape that cannot drive a
// terminal or reorder the message.
func jsonText(raw []byte) string {
	var b strings.Builder

	for _, r := range clip(string(raw)) {
		if r >= ' ' && r <= '~' {
			b.WriteRune(r)

			continue
		}

		for _, unit := range utf16.Encode([]rune{r}) {
			fmt.Fprintf(&b, `\u%04x`, unit)
		}
	}

	return b.String()
}

// Validate checks every rule of the format that a Go value can break; Parse
// also checks the JSON encoding itself. Another positive FormatVersion is
// a fault.Usage error, every other violation is fault.Integrity.
func (c *Catalog) Validate(limits Limits) error {
	limits = limits.withDefaults()

	switch {
	case c.FormatVersion > 0 && c.FormatVersion != FormatVersion:
		return fault.New(fault.Usage, "unsupported catalog formatVersion %d: this build supports %d", c.FormatVersion, FormatVersion)
	case c.FormatVersion != FormatVersion:
		return fault.New(fault.Integrity, "invalid catalog: formatVersion %d is not a positive integer", c.FormatVersion)
	}

	if _, err := calver.ParseRevision(c.Revision); err != nil {
		return fault.Wrap(fault.Integrity, err, "invalid catalog revision")
	}

	if len(c.Schemas) > limits.MaxEntries {
		return fault.New(fault.Integrity, "catalog has %d entries, limit is %d", len(c.Schemas), limits.MaxEntries)
	}

	sizes := make(map[string]int64, len(c.Schemas))

	for i := range c.Schemas {
		entry := &c.Schemas[i]
		if err := entry.validate(limits); err != nil {
			return fault.Wrap(fault.Integrity, err, "invalid catalog entry %d (%s)", i, quote(entry.ID))
		}

		if i > 0 && c.Schemas[i-1].ID >= entry.ID {
			if c.Schemas[i-1].ID == entry.ID {
				return fault.New(fault.Integrity, "invalid catalog: duplicate schema id %q", entry.ID)
			}

			return fault.New(fault.Integrity, "invalid catalog: entries must be sorted by id (%q before %q)", c.Schemas[i-1].ID, entry.ID)
		}

		if size, seen := sizes[entry.Artifact.Digest]; seen && size != entry.Artifact.Size {
			return fault.New(fault.Integrity, "invalid catalog: digest %s is listed with sizes %d and %d", entry.Artifact.Digest, size, entry.Artifact.Size)
		}

		sizes[entry.Artifact.Digest] = entry.Artifact.Size
	}

	return nil
}

func (e *Entry) validate(limits Limits) error {
	if len(e.ID) > maxIDLength || !idPattern.MatchString(e.ID) || strings.Contains(e.ID, "..") {
		return fmt.Errorf("id %s must match %s and must not contain \"..\"", quote(e.ID), idPattern)
	}

	if err := checkText("name", e.Name, maxNameBytes, false); err != nil {
		return err
	}

	if strings.IndexFunc(e.Name, isVisible) < 0 {
		return fmt.Errorf("name %s has no visible characters", quote(e.Name))
	}

	if err := checkText("description", e.Description, maxDescriptionBytes, true); err != nil {
		return err
	}

	if e.Dialect != "" {
		if err := checkURI("dialect", e.Dialect, false); err != nil {
			return err
		}
	}

	if err := checkPatterns(e.FileMatch); err != nil {
		return err
	}

	if err := e.Artifact.Validate(limits.MaxManifestBytes); err != nil {
		return err
	}

	if e.Provenance != nil {
		return e.Provenance.validate()
	}

	return nil
}

func checkPatterns(patterns []string) error {
	if len(patterns) > maxPatternsPerEntry {
		return fmt.Errorf("fileMatch has %d patterns, limit is %d", len(patterns), maxPatternsPerEntry)
	}

	seen := make(map[string]struct{}, len(patterns))

	for _, pattern := range patterns {
		if err := checkText("fileMatch pattern", pattern, maxPatternBytes, false); err != nil {
			return err
		}

		if err := match.ValidatePattern(pattern); err != nil {
			return fmt.Errorf("fileMatch: %w", err)
		}

		if _, dup := seen[pattern]; dup {
			return fmt.Errorf("fileMatch pattern %s is listed twice", quote(pattern))
		}

		seen[pattern] = struct{}{}
	}

	return nil
}

// Validate checks that d describes an OCI image manifest of at most
// maxManifestBytes bytes; a non-positive limit selects the default.
func (d Descriptor) Validate(maxManifestBytes int64) error {
	if maxManifestBytes <= 0 {
		maxManifestBytes = defaultManifestBytes
	}

	if d.MediaType != ManifestMediaType {
		return fmt.Errorf("artifact mediaType %s is not %s", quote(d.MediaType), ManifestMediaType)
	}

	if err := checkDigest("artifact digest", d.Digest); err != nil {
		return err
	}

	if d.Size <= 0 || d.Size > maxManifestBytes {
		return fmt.Errorf("artifact size %d is outside 1-%d", d.Size, maxManifestBytes)
	}

	return nil
}

func (p *Provenance) validate() error {
	if err := checkURI("provenance.source", p.Source, true); err != nil {
		return err
	}

	if p.SourceDigest != "" {
		if err := checkDigest("provenance.sourceDigest", p.SourceDigest); err != nil {
			return err
		}
	}

	if err := checkText("provenance.license", p.License, maxLicenseBytes, false); err != nil {
		return err
	}

	if len(p.Dependencies) > maxDependencies {
		return fmt.Errorf("provenance lists %d dependencies, limit is %d", len(p.Dependencies), maxDependencies)
	}

	for i, dep := range p.Dependencies {
		if err := checkURI("provenance.dependencies.source", dep.Source, true); err != nil {
			return err
		}

		if err := checkDigest("provenance.dependencies.digest", dep.Digest); err != nil {
			return err
		}

		if i > 0 && p.Dependencies[i-1].Source >= dep.Source {
			return fmt.Errorf("provenance.dependencies must be sorted by source without duplicates (%s)", quote(dep.Source))
		}
	}

	return nil
}

// checkDigest hands digest.Validate at most one byte more than a sha256
// digest has, because its errors quote their input in full. That prefix is
// never valid when the value is longer and still carries the algorithm, so an
// over-long sha512 digest is reported as unsupported rather than malformed.
func checkDigest(field, value string) error {
	prefix := value[:min(len(value), maxDigestBytes+1)]

	err := digest.Validate(prefix)

	switch {
	case err == nil:
		return nil
	case len(prefix) == len(value):
		return fmt.Errorf("%s: %w", field, err)
	case errors.Is(err, digest.ErrUnsupportedAlgorithm):
		return fmt.Errorf("%s: %w in %s (only %s is supported)", field, digest.ErrUnsupportedAlgorithm, quote(value), digest.Algorithm)
	default:
		return fmt.Errorf("%s: %w %s: expected %s:<%d lowercase hex digits>", field, digest.ErrInvalid, quote(value), digest.Algorithm, 2*sha256.Size)
	}
}

// checkText rejects text that could corrupt or disguise terminal output:
// control characters, Unicode bidirectional controls, which can visually
// reorder what a user reads, and outside multiline text also tab and the
// line breaks line feed, U+2028 and U+2029, because "list" and "patterns"
// print tab-separated lines and Unicode-aware tools split lines at all three.
func checkText(field, value string, limit int, multiline bool) error {
	if len(value) > limit {
		return fmt.Errorf("%s is longer than %d bytes", field, limit)
	}

	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", field)
	}

	for _, r := range value {
		if forbiddenRune(r, multiline) {
			return fmt.Errorf("%s contains the forbidden character %U", field, r)
		}
	}

	return nil
}

func forbiddenRune(r rune, multiline bool) bool {
	switch r {
	case '\n', '\t', '\u2028', '\u2029':
		return !multiline
	}

	return unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r)
}

func isVisible(r rune) bool {
	return unicode.IsGraphic(r) && !unicode.IsSpace(r)
}

// checkURI accepts absolute RFC 3986 URIs only. The character check keeps
// the Go parser, which tolerates spaces and other characters in paths, in
// line with the pattern in api/catalog.schema.json.
func checkURI(field, value string, network bool) error {
	if value == "" || len(value) > maxURIBytes {
		return fmt.Errorf("%s must be a URI of 1-%d bytes", field, maxURIBytes)
	}

	if i := strings.IndexFunc(value, isNotURIChar); i >= 0 {
		return fmt.Errorf("%s %s contains a character that is not allowed in a URI at byte %d", field, quote(value), i)
	}

	u, err := url.Parse(value)
	if err != nil || !u.IsAbs() {
		return fmt.Errorf("%s %s is not an absolute URI", field, quote(value))
	}

	if u.User != nil {
		return fmt.Errorf("%s must not contain credentials", field)
	}

	if !network {
		return nil
	}

	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("%s %s must use http or https", field, quote(value))
	}

	if u.Hostname() == "" {
		return fmt.Errorf("%s %s must name a host", field, quote(value))
	}

	return nil
}

func isNotURIChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	default:
		return !strings.ContainsRune("-._~:/?#[]@!$&'()*+,;=%", r)
	}
}

// Marshal encodes c in the canonical wire form: entries sorted by id, object
// members sorted by name at every level, no HTML escaping and no
// insignificant whitespace. Member order comes from the sort rather than from
// struct field order, so reordering struct fields never changes catalog bytes.
func Marshal(c *Catalog) ([]byte, error) {
	sorted := *c

	sorted.Schemas = slices.Clone(c.Schemas)
	if sorted.Schemas == nil {
		sorted.Schemas = []Entry{}
	}

	slices.SortStableFunc(sorted.Schemas, func(a, b Entry) int { return strings.Compare(a.ID, b.ID) })

	data, err := canonical(&sorted)
	if err != nil {
		return nil, fmt.Errorf("encode catalog: %w", err)
	}

	return data, nil
}

// MarshalEntry encodes one entry exactly as Marshal writes it inside the
// schemas list.
func MarshalEntry(e *Entry) ([]byte, error) {
	data, err := canonical(e)
	if err != nil {
		return nil, fmt.Errorf("encode catalog entry %s: %w", quote(e.ID), err)
	}

	return data, nil
}

func canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	if err := enc.Encode(tree); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}

	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Lookup binary-searches the id-sorted entries and returns a pointer into
// c.Schemas.
func (c *Catalog) Lookup(id string) (*Entry, bool) {
	i, found := slices.BinarySearchFunc(c.Schemas, id, func(e Entry, target string) int {
		return strings.Compare(e.ID, target)
	})
	if !found {
		return nil, false
	}

	return &c.Schemas[i], true
}

// Artifacts returns the distinct schema manifest descriptors of the
// entries in ascending digest order: the schema children the catalog index
// must reference.
func (c *Catalog) Artifacts() []ocispec.Descriptor {
	descs := make([]ocispec.Descriptor, 0, len(c.Schemas))

	for _, e := range c.Schemas {
		descs = append(descs, ocispec.Descriptor{MediaType: e.Artifact.MediaType, Digest: godigest.Digest(e.Artifact.Digest), Size: e.Artifact.Size})
	}

	slices.SortFunc(descs, func(a, b ocispec.Descriptor) int { return strings.Compare(a.Digest.String(), b.Digest.String()) })

	return slices.CompactFunc(descs, func(a, b ocispec.Descriptor) bool { return a.Digest == b.Digest })
}

// IDs returns every schema id in catalog order.
func (c *Catalog) IDs() []string {
	ids := make([]string, 0, len(c.Schemas))
	for _, e := range c.Schemas {
		ids = append(ids, e.ID)
	}

	return ids
}

// Rules returns the fileMatch rules of the entries that have any, in id
// order, with pattern slices the caller may modify.
func (c *Catalog) Rules() []match.Rule {
	rules := make([]match.Rule, 0, len(c.Schemas))

	for _, e := range c.Schemas {
		if len(e.FileMatch) > 0 {
			rules = append(rules, match.Rule{SchemaID: e.ID, Patterns: slices.Clone(e.FileMatch)})
		}
	}

	return rules
}

// ListItem is the projection printed by "schepherd list".
type ListItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Dialect     string `json:"dialect,omitempty"`
	Digest      string `json:"digest"`
}

// List returns every entry in id order, without fileMatch rules and
// provenance.
func (c *Catalog) List() []ListItem {
	items := make([]ListItem, 0, len(c.Schemas))
	for _, e := range c.Schemas {
		items = append(items, ListItem{ID: e.ID, Name: e.Name, Description: e.Description, Dialect: e.Dialect, Digest: e.Artifact.Digest})
	}

	return items
}

// PatternItem is the projection printed by "schepherd patterns": each schema
// keeps its patterns and descriptor together so integrations never lose the
// association between a glob and the artifact it selects.
type PatternItem struct {
	ID        string     `json:"id"`
	FileMatch []string   `json:"fileMatch"`
	Artifact  Descriptor `json:"artifact"`
}

// Patterns returns the entries that have fileMatch rules, in id order, with
// pattern slices the caller may modify.
func (c *Catalog) Patterns() []PatternItem {
	items := make([]PatternItem, 0, len(c.Schemas))

	for _, e := range c.Schemas {
		if len(e.FileMatch) > 0 {
			items = append(items, PatternItem{ID: e.ID, FileMatch: slices.Clone(e.FileMatch), Artifact: e.Artifact})
		}
	}

	return items
}

// quote renders untrusted text for error messages, shortened so a hostile
// catalog cannot produce multi-megabyte diagnostics.
func quote(s string) string {
	if len(s) <= maxQuotedBytes {
		return strconv.Quote(s)
	}

	return strconv.Quote(truncate(s, maxQuotedBytes)) + "..."
}

func clip(s string) string {
	if len(s) <= maxQuotedBytes {
		return s
	}

	return truncate(s, maxQuotedBytes) + "..."
}

func truncate(s string, limit int) string {
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut]
}

// shortError bounds the message of a jsonutil error, which quotes a
// duplicate member name in full, however long; the chain stays intact for
// errors.Is.
type shortError struct {
	err error
}

func (e shortError) Error() string {
	msg := e.err.Error()
	if len(msg) <= maxMessageBytes {
		return msg
	}

	return truncate(msg, maxMessageBytes) + "..."
}

func (e shortError) Unwrap() error {
	return e.err
}
