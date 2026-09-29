package prepare

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"unicode/utf8"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/jsonutil"
	"github.com/ovineko/schepherd/internal/publisher/bundle"
	"github.com/ovineko/schepherd/internal/publisher/ids"
	"github.com/ovineko/schepherd/internal/publisher/policy"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

// FormatVersion is the only prepared.json format this build writes and reads.
const FormatVersion = 1

// BundlerName identifies the bundler in Recipe.
const BundlerName = "sourcemeta-jsonschema"

// Recipe names the preparation rules together with the pinned bundler,
// whose serialization is part of every bundled schema. It changes whenever
// the same upstream input would produce different prepared bytes, a bundler
// upgrade included, so a prepared set is only ever published by the build
// that made it.
const Recipe = "schepherd-prepare/2+" + BundlerName + "-" + bundle.PinnedVersion

// Files and directories of a prepared set.
const (
	PreparedFile = "prepared.json"
	ReportFile   = "report.json"
	SchemasDir   = "schemas"
	NoticesDir   = "notices"
)

// Source kinds recorded in prepared.json.
const (
	KindSchemaStore = "schemastore"
	KindLocal       = "local"
)

const (
	maxPreparedBytes = 64 << 20
	maxEntries       = 20000
	maxDocumentDepth = 32
)

var (
	noticePath    = regexp.MustCompile(`^notices/[0-9a-f]{64}\.txt$`)
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Source identifies the upstream a prepared set was built from. It holds no
// dates, hosts or local paths, so it only changes when the input changes.
type Source struct {
	Kind          string `json:"kind"`
	Name          string `json:"name,omitempty"`
	Commit        string `json:"commit,omitempty"`
	TarballDigest string `json:"tarballDigest,omitempty"`
}

// Entry is one schema of the next catalog. A prepared entry has new
// content: Schema and Notice are paths relative to the prepared directory,
// Schema always "schemas/<id>.json" and Notice, when present,
// "notices/<sha256 hex of the notice bytes>.txt". A reused entry (Reused)
// keeps the artifact the publisher state records for its ID, because its
// source and dependencies did not change: it has no files, and
// ContentDigest and NoticeDigest are the recorded ones. Its metadata is
// fresh from upstream all the same. License is the license decision behind
// Provenance.License.
type Entry struct {
	ID            string                `json:"id"`
	Name          string                `json:"name"`
	Description   string                `json:"description,omitempty"`
	Dialect       string                `json:"dialect,omitempty"`
	FileMatch     []string              `json:"fileMatch"`
	Schema        string                `json:"schema,omitempty"`
	ContentDigest string                `json:"contentDigest"`
	Notice        string                `json:"notice,omitempty"`
	NoticeDigest  string                `json:"noticeDigest,omitempty"`
	Reused        *catalog.Descriptor   `json:"reusedArtifact,omitempty"`
	Provenance    catalog.Provenance    `json:"provenance"`
	License       state.LicenseDecision `json:"licenseDecision,omitzero"`
}

// Hold names a published schema that the next catalog keeps with its last
// entry and artifact, and why (one of state.HeldReasons).
type Hold struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// Exclusion names a published schema that an explicit exclude rule removes
// from the next catalog, and the rule.
type Exclusion struct {
	ID   string `json:"id"`
	Rule string `json:"rule,omitempty"`
}

// Document is the content of prepared.json. Entries, Held and Excluded are
// sorted by ID and name distinct IDs. Together they account for every
// schema the publisher state lists in its catalog; a schema the state
// records as excluded that none of them names stays excluded.
type Document struct {
	FormatVersion int         `json:"formatVersion"`
	Recipe        string      `json:"recipe"`
	Source        Source      `json:"source"`
	Entries       []Entry     `json:"entries"`
	Held          []Hold      `json:"held"`
	Excluded      []Exclusion `json:"excluded"`
}

// Set is a prepared directory held in memory: the document plus the bytes of
// every schema (by entry ID) and notice (by relative path) its prepared
// entries reference.
type Set struct {
	Schemas  map[string][]byte
	Notices  map[string][]byte
	Document Document
}

// SchemaPath returns the prepared file name of the schema with the given ID.
func SchemaPath(id string) string {
	return SchemasDir + "/" + id + ".json"
}

// NoticePathFor returns the prepared file name of a notice with the given
// bytes.
func NoticePathFor(notice []byte) string {
	return NoticesDir + "/" + digest.Hex(digest.FromBytes(notice)) + ".txt"
}

// Encode returns the canonical prepared.json bytes of doc: two-space
// indentation, fixed member order, no HTML escaping and a final newline.
func Encode(doc *Document) ([]byte, error) {
	wire := *doc
	if wire.Entries == nil {
		wire.Entries = []Entry{}
	}

	if wire.Held == nil {
		wire.Held = []Hold{}
	}

	if wire.Excluded == nil {
		wire.Excluded = []Exclusion{}
	}

	return encodeJSON(&wire)
}

func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")

	if err := enc.Encode(v); err != nil {
		return nil, fault.Wrap(fault.Internal, err, "encode JSON")
	}

	return buf.Bytes(), nil
}

// ParseDocument validates prepared.json. The document must be in the
// canonical form Encode produces, which rules out unknown, duplicate or
// differently cased members. A different formatVersion or recipe is
// fault.Usage; every other defect is fault.Integrity.
func ParseDocument(data []byte) (*Document, error) {
	if err := jsonutil.Check(data, maxDocumentDepth); err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "invalid %s", PreparedFile)
	}

	var probe struct {
		FormatVersion json.RawMessage `json:"formatVersion"`
		Recipe        json.RawMessage `json:"recipe"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "invalid %s", PreparedFile)
	}

	if string(probe.FormatVersion) != "1" {
		return nil, fault.New(fault.Usage, "%s has formatVersion %s; this build reads %d", PreparedFile, clip(probe.FormatVersion), FormatVersion)
	}

	if string(probe.Recipe) != `"`+Recipe+`"` {
		return nil, fault.New(fault.Usage, "%s was made with recipe %s; this build reads %q", PreparedFile, clip(probe.Recipe), Recipe)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var doc Document
	if err := dec.Decode(&doc); err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "invalid %s", PreparedFile)
	}

	if err := doc.validate(); err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "invalid %s", PreparedFile)
	}

	canonical, err := Encode(&doc)
	if err != nil {
		return nil, err
	}

	if !bytes.Equal(canonical, data) {
		return nil, fault.New(fault.Integrity, "invalid %s: not in the canonical form written by prepare", PreparedFile)
	}

	return &doc, nil
}

func clip(raw json.RawMessage) string {
	const limit = 64

	if len(raw) == 0 {
		return "(missing)"
	}

	if len(raw) > limit {
		return string(raw[:limit]) + "..."
	}

	return string(raw)
}

func (d *Document) validate() error {
	if err := d.Source.validate(); err != nil {
		return err
	}

	if len(d.Entries)+len(d.Held)+len(d.Excluded) > maxEntries {
		return fmt.Errorf("%d entries, holds and exclusions exceed the limit of %d", len(d.Entries)+len(d.Held)+len(d.Excluded), maxEntries)
	}

	named := make(map[string]string, len(d.Entries)+len(d.Held)+len(d.Excluded))

	for i := range d.Entries {
		e := &d.Entries[i]
		if err := e.validate(); err != nil {
			return fmt.Errorf("entry %d (%q): %w", i, e.ID, err)
		}

		if i > 0 && d.Entries[i-1].ID >= e.ID {
			return fmt.Errorf("entries must be sorted by id without duplicates (%q before %q)", d.Entries[i-1].ID, e.ID)
		}

		named[e.ID] = "entries"
	}

	return d.validateRetained(named)
}

func (s *Source) validate() error {
	switch s.Kind {
	case KindSchemaStore:
		if !commitPattern.MatchString(s.Commit) || s.Name != "" {
			return errors.New("a schemastore source needs a 40-hex commit, a tarballDigest and no name")
		}

		if err := digest.Validate(s.TarballDigest); err != nil {
			return fmt.Errorf("source.tarballDigest: %w", err)
		}
	case KindLocal:
		if s.Name == "" || s.Commit != "" || s.TarballDigest != "" {
			return errors.New("a local source needs a name and no commit or tarballDigest")
		}
	default:
		return fmt.Errorf("unknown source kind %q", s.Kind)
	}

	return nil
}

// validateRetained checks the held and excluded lists; named maps the IDs
// of the entries to their list.
func (d *Document) validateRetained(named map[string]string) error {
	for i, h := range d.Held {
		if !slices.Contains(state.HeldReasons, h.Reason) {
			return fmt.Errorf("held %d (%q): reason %q is not one of %v", i, h.ID, h.Reason, state.HeldReasons)
		}

		if err := checkListed("held", named, i, h.ID, d.Held, func(h Hold) string { return h.ID }); err != nil {
			return err
		}
	}

	for i, x := range d.Excluded {
		if x.Rule != "" && !policy.ValidRuleID(x.Rule) {
			return fmt.Errorf("excluded %d (%q): rule %q is not a valid rule ID", i, x.ID, x.Rule)
		}

		if err := checkListed("excluded", named, i, x.ID, d.Excluded, func(x Exclusion) string { return x.ID }); err != nil {
			return err
		}
	}

	return nil
}

// checkListed checks the ID of item i of a held or excluded list: valid,
// sorted without duplicates and named by no other list.
func checkListed[T any](list string, named map[string]string, i int, id string, items []T, idOf func(T) string) error {
	if !ids.Valid(id) {
		return fmt.Errorf("%s %d: invalid id %q", list, i, id)
	}

	if i > 0 && idOf(items[i-1]) >= id {
		return fmt.Errorf("%s must be sorted by id without duplicates (%q before %q)", list, idOf(items[i-1]), id)
	}

	if other, ok := named[id]; ok {
		return fmt.Errorf("%q is listed in both %s and %s", id, other, list)
	}

	named[id] = list

	return nil
}

func (e *Entry) validate() error {
	if !ids.Valid(e.ID) {
		return errors.New("invalid id")
	}

	if err := e.validateContent(); err != nil {
		return err
	}

	if err := e.License.Validate(); err != nil {
		return fmt.Errorf("licenseDecision: %w", err)
	}

	if e.FileMatch == nil {
		return errors.New("fileMatch is required (use [] for none)")
	}

	if e.Provenance.SourceDigest == "" || e.Provenance.License == "" {
		return errors.New("provenance needs source, sourceDigest and license")
	}

	if err := policy.CheckAssertedLicense(e.Provenance.License); err != nil {
		return fmt.Errorf("provenance: %w", err)
	}

	return CheckMetadata(e.Name, e.Description, e.Dialect, e.FileMatch, &e.Provenance)
}

func (e *Entry) validateContent() error {
	if err := digest.Validate(e.ContentDigest); err != nil {
		return fmt.Errorf("contentDigest: %w", err)
	}

	if e.Reused != nil {
		if e.Schema != "" || e.Notice != "" {
			return errors.New("a reused entry has no schema or notice file")
		}

		if e.NoticeDigest != "" {
			if err := digest.Validate(e.NoticeDigest); err != nil {
				return fmt.Errorf("noticeDigest: %w", err)
			}
		}

		if err := e.Reused.Validate(0); err != nil {
			return fmt.Errorf("reusedArtifact: %w", err)
		}

		return nil
	}

	if e.Schema != SchemaPath(e.ID) {
		return fmt.Errorf("schema must be %q", SchemaPath(e.ID))
	}

	if e.NoticeDigest != "" {
		return errors.New("noticeDigest belongs to reused entries; a prepared entry has its notice file")
	}

	if e.Notice != "" && !noticePath.MatchString(e.Notice) {
		return fmt.Errorf("notice %q must be notices/<64 hex digits>.txt", e.Notice)
	}

	return nil
}

// placeholderArtifact stands in for the not yet packed manifest when catalog
// rules are checked before publishing.
var placeholderArtifact = catalog.Descriptor{
	MediaType: catalog.ManifestMediaType,
	Digest:    digest.FromBytes(nil),
	Size:      1,
}

// CheckMetadata applies the catalog's rules for names, descriptions,
// dialects, fileMatch patterns and provenance to one prospective entry, so a
// prepared set never contains an entry a client would reject.
func CheckMetadata(name, description, dialect string, fileMatch []string, prov *catalog.Provenance) error {
	c := catalog.Catalog{
		FormatVersion: catalog.FormatVersion,
		Revision:      "20000101.0000",
		Schemas: []catalog.Entry{{
			ID: "x", Name: name, Description: description, Dialect: dialect, FileMatch: fileMatch,
			Artifact: placeholderArtifact, Provenance: prov,
		}},
	}

	if err := c.Validate(catalog.DefaultLimits()); err != nil {
		if inner := errors.Unwrap(err); inner != nil {
			return inner
		}

		return fmt.Errorf("catalog rules: %w", err)
	}

	return nil
}

// Load reads and verifies a prepared directory: prepared.json must parse
// (see ParseDocument), and every schema and notice file must be a regular
// file inside the directory whose bytes match the recorded digest. Schemas
// must be compact JSON within the default artifact limits. A missing
// directory or file is fault.Usage; mismatches are fault.Integrity.
func Load(dir string) (*Set, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "open prepared directory")
	}

	defer func() { _ = root.Close() }()

	data, err := readRegular(root, PreparedFile, maxPreparedBytes)
	if err != nil {
		return nil, err
	}

	doc, err := ParseDocument(data)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "prepared directory %s", dir)
	}

	limits := artifact.DefaultLimits()
	set := &Set{Document: *doc, Schemas: make(map[string][]byte, len(doc.Entries)), Notices: map[string][]byte{}}

	for i := range doc.Entries {
		if err := set.loadEntry(root, &doc.Entries[i], limits); err != nil {
			return nil, fault.Wrap(fault.Integrity, err, "prepared directory %s", dir)
		}
	}

	return set, nil
}

func (s *Set) loadEntry(root *os.Root, e *Entry, limits artifact.Limits) error {
	if e.Reused != nil {
		return nil
	}

	schema, err := readRegular(root, e.Schema, limits.MaxSchemaBytes)
	if err != nil {
		return err
	}

	if got := digest.FromBytes(schema); got != e.ContentDigest {
		return fault.New(fault.Integrity, "%s has digest %s, prepared.json records %s", e.Schema, got, e.ContentDigest)
	}

	compact, err := jsonutil.Compact(schema, jsonutil.DefaultMaxDepth)
	if err != nil || !bytes.Equal(compact, schema) {
		return fault.New(fault.Integrity, "%s is not compact strict JSON", e.Schema)
	}

	s.Schemas[e.ID] = schema

	if e.Notice == "" {
		return nil
	}

	if _, ok := s.Notices[e.Notice]; ok {
		return nil
	}

	notice, err := readRegular(root, e.Notice, limits.MaxNoticeBytes)
	if err != nil {
		return err
	}

	if NoticePathFor(notice) != e.Notice {
		return fault.New(fault.Integrity, "%s does not hold the notice its name promises", e.Notice)
	}

	if !utf8.Valid(notice) {
		return fault.New(fault.Integrity, "%s is not valid UTF-8", e.Notice)
	}

	s.Notices[e.Notice] = notice

	return nil
}

func readRegular(root *os.Root, name string, limit int64) ([]byte, error) {
	f, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fault.Wrap(fault.Usage, err, "prepared file %s", name)
		}

		return nil, fault.Wrap(fault.Integrity, err, "prepared file %s", name)
	}

	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "prepared file %s", name)
	}

	if !info.Mode().IsRegular() {
		return nil, fault.New(fault.Integrity, "prepared file %s is not a regular file", name)
	}

	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "read prepared file %s", name)
	}

	if int64(len(data)) > limit {
		return nil, fault.New(fault.Integrity, "prepared file %s exceeds %d bytes", name, limit)
	}

	return data, nil
}

// WriteSet writes set into dir, which must not exist or be empty, together
// with extra files (relative slash-separated names such as report.json). The
// files are written into a temporary sibling directory that replaces dir
// only when complete, so dir never holds a partial set.
func WriteSet(dir string, set *Set, extra map[string][]byte) error {
	doc, err := Encode(&set.Document)
	if err != nil {
		return err
	}

	files := map[string][]byte{PreparedFile: doc}

	for _, e := range set.Document.Entries {
		if e.Reused != nil {
			continue
		}

		schema, ok := set.Schemas[e.ID]
		if !ok {
			return fault.New(fault.Internal, "no schema bytes for %s", e.ID)
		}

		files[e.Schema] = schema

		if e.Notice != "" {
			notice, ok := set.Notices[e.Notice]
			if !ok {
				return fault.New(fault.Internal, "no notice bytes for %s", e.Notice)
			}

			files[e.Notice] = notice
		}
	}

	maps.Copy(files, extra)

	return writeDir(dir, files)
}

func writeDir(dir string, files map[string][]byte) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return fault.Wrap(fault.Usage, err, "output directory")
	}

	if err := checkOutDir(dir); err != nil {
		return err
	}

	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return fault.Wrap(fault.Usage, err, "create %s", parent)
	}

	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(dir)+".tmp-")
	if err != nil {
		return fault.Wrap(fault.Usage, err, "create a temporary directory next to %s", dir)
	}

	if err := populate(tmp, files); err != nil {
		_ = os.RemoveAll(tmp)

		return err
	}

	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = os.RemoveAll(tmp)

		return fault.Wrap(fault.Usage, err, "replace empty output directory %s", dir)
	}

	if err := os.Rename(tmp, dir); err != nil {
		_ = os.RemoveAll(tmp)

		return fault.Wrap(fault.Internal, err, "move prepared set into %s", dir)
	}

	return nil
}

func populate(dir string, files map[string][]byte) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "open temporary output directory")
	}

	defer func() { _ = root.Close() }()

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}

	slices.Sort(names)

	for _, name := range names {
		local := filepath.FromSlash(name)

		if d := path.Dir(name); d != "." {
			if err := root.MkdirAll(filepath.FromSlash(d), 0o755); err != nil {
				return fault.Wrap(fault.Internal, err, "create %s", d)
			}
		}

		if err := root.WriteFile(local, files[name], 0o644); err != nil {
			return fault.Wrap(fault.Internal, err, "write %s", name)
		}
	}

	return nil
}

// checkOutDir accepts a directory that does not exist or is empty.
func checkOutDir(dir string) error {
	entries, err := os.ReadDir(dir)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fault.Wrap(fault.Usage, err, "output directory %s", dir)
	case len(entries) > 0:
		return fault.New(fault.Usage, "output directory %s is not empty", dir)
	default:
		return nil
	}
}
