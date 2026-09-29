// Package upstream provides the publisher's SchemaStore source: the pinned
// upstream description and an extracted, verified snapshot of it. Local
// source files are read by package prepare. Nothing here runs in the client
// binary.
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/jsonutil"
)

// Layout of a SchemaStore snapshot, relative to its directory. Paths mirror
// the upstream repository so a snapshot can be compared with a checkout.
const (
	catalogFile = "src/api/json/catalog.json"
	schemasDir  = "src/schemas/json"
	positiveDir = "src/test"
	negativeDir = "src/negative_test"
	licenseFile = "LICENSE"
	noticeFile  = "NOTICE"
	metaFile    = "snapshot.json"

	snapshotFormatVersion = 1

	maxMetaBytes    = 64 << 10
	maxLegalBytes   = 1 << 20
	maxCatalogBytes = 64 << 20
	maxIndexedFiles = 200000
)

// CanonicalSchemaStoreBase is the prefix of the canonical URL of every
// SchemaStore-hosted schema: CanonicalSchemaStoreBase + "<file>.json".
const CanonicalSchemaStoreBase = "https://www.schemastore.org/"

const rawSchemaStorePrefix = "/SchemaStore/schemastore/master/src/schemas/json/"

type snapshotMeta struct {
	Commit        string `json:"commit"`
	TarballDigest string `json:"tarballDigest"`
	FormatVersion int    `json:"formatVersion"`
}

// Snapshot is an extracted SchemaStore tree at one commit.
type Snapshot struct {
	schemas  map[string]struct{}
	positive map[string][]string
	negative map[string][]string
	// Dir is the absolute snapshot directory.
	Dir string
	// Commit is the upstream commit SHA the snapshot was extracted from.
	Commit string
	// TarballDigest is the sha256 digest of the downloaded tarball.
	TarballDigest string
	license       []byte
	notice        []byte
}

// OpenSnapshot opens a snapshot previously written by FetchSchemaStore
// without network access. It indexes schema and test files once; symbolic
// links and other non-regular files inside the snapshot are ignored. A
// missing directory or metadata file is fault.Usage; a malformed snapshot is
// fault.Integrity.
func OpenSnapshot(dir string) (*Snapshot, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "snapshot directory")
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "open snapshot")
	}

	defer func() { _ = root.Close() }()

	s := &Snapshot{Dir: dir}

	if err := s.readMeta(root); err != nil {
		return nil, err
	}

	fsys := root.FS()

	if s.schemas, err = indexSchemas(fsys); err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "snapshot %s", dir)
	}

	if s.positive, err = indexTests(fsys, positiveDir); err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "snapshot %s", dir)
	}

	if s.negative, err = indexTests(fsys, negativeDir); err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "snapshot %s", dir)
	}

	if info, err := fs.Stat(fsys, catalogFile); err != nil || !info.Mode().IsRegular() {
		return nil, fault.New(fault.Integrity, "snapshot %s has no %s", dir, catalogFile)
	}

	if s.license, err = readRegular(fsys, licenseFile, maxLegalBytes); err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "snapshot %s", dir)
	}

	s.notice, err = readRegular(fsys, noticeFile, maxLegalBytes)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fault.Wrap(fault.Integrity, err, "snapshot %s", dir)
	}

	return s, nil
}

func readRegular(fsys fs.FS, name string, limit int64) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, fmt.Errorf("read snapshot file: %w", err)
	}

	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("read snapshot file: %w", err)
	}

	if !info.Mode().IsRegular() {
		return nil, &fs.PathError{Op: "read", Path: name, Err: errors.New("not a regular file")}
	}

	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read snapshot file: %w", err)
	}

	if int64(len(data)) > limit {
		return nil, &fs.PathError{Op: "read", Path: name, Err: errors.New("file too large")}
	}

	return data, nil
}

func indexSchemas(fsys fs.FS) (map[string]struct{}, error) {
	entries, err := fs.ReadDir(fsys, schemasDir)
	if err != nil {
		return nil, fmt.Errorf("list schemas: %w", err)
	}

	if len(entries) > maxIndexedFiles {
		return nil, errors.New("too many schema files")
	}

	schemas := make(map[string]struct{}, len(entries))

	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json") {
			schemas[entry.Name()] = struct{}{}
		}
	}

	return schemas, nil
}

func indexTests(fsys fs.FS, dir string) (map[string][]string, error) {
	tests := map[string][]string{}

	if _, err := fs.Stat(fsys, dir); errors.Is(err, fs.ErrNotExist) {
		return tests, nil
	}

	count := 0

	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.Type().IsRegular() {
			return nil
		}

		count++
		if count > maxIndexedFiles {
			return errors.New("too many test files")
		}

		id, _, nested := strings.Cut(strings.TrimPrefix(p, dir+"/"), "/")
		if nested {
			tests[id] = append(tests[id], p)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("index tests: %w", err)
	}

	for id := range tests {
		slices.Sort(tests[id])
	}

	return tests, nil
}

// Catalog parses the snapshot's src/api/json/catalog.json with ParseCatalog.
func (s *Snapshot) Catalog() ([]CatalogEntry, error) {
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "open snapshot")
	}

	defer func() { _ = root.Close() }()

	data, err := readRegular(root.FS(), catalogFile, maxCatalogBytes)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "snapshot %s", s.Dir)
	}

	entries, err := ParseCatalog(data)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "snapshot %s: %s", s.Dir, catalogFile)
	}

	return entries, nil
}

// LocalPath maps a SchemaStore-hosted schema URL to the file in the snapshot
// that holds the same bytes at the pinned commit. It recognizes
// https://www.schemastore.org/<f>(.json),
// https://www.schemastore.org/schemas/json/<f>(.json), the same paths on
// json.schemastore.org, and
// https://raw.githubusercontent.com/SchemaStore/schemastore/master/src/schemas/json/<f>.json.
// A fragment is ignored; a query, credentials or a port make the URL
// unknown. The second result is false when the URL is not one of these forms
// or the file does not exist in the snapshot.
func (s *Snapshot) LocalPath(rawURL string) (string, bool) {
	name, ok := s.schemaFile(rawURL)
	if !ok {
		return "", false
	}

	return filepath.Join(s.Dir, filepath.FromSlash(schemasDir), name), true
}

// CanonicalURL returns CanonicalSchemaStoreBase + "<f>.json" for any URL
// LocalPath maps, so that aliases of one document share one identity in
// policy decisions, provenance and ID assignment.
func (s *Snapshot) CanonicalURL(rawURL string) (string, bool) {
	name, ok := s.schemaFile(rawURL)
	if !ok {
		return "", false
	}

	return CanonicalSchemaStoreBase + name, true
}

func plainFileName(name string) bool {
	if name == "" || name[0] == '.' {
		return false
	}

	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return false
		}
	}

	return true
}

// Tests returns the positive (src/test/<id>/**) and negative
// (src/negative_test/<id>/**) test files for a schema, as sorted absolute
// paths. name is the schema file name with or without ".json", which is how
// SchemaStore names test directories.
func (s *Snapshot) Tests(name string) (positive, negative []string) {
	id := strings.TrimSuffix(name, ".json")
	if !plainFileName(id) {
		return nil, nil
	}

	return s.absolute(s.positive[id]), s.absolute(s.negative[id])
}

// License returns the upstream LICENSE file.
func (s *Snapshot) License() []byte {
	return slices.Clone(s.license)
}

// Notice returns the upstream NOTICE file, or nil if the commit has none.
func (s *Snapshot) Notice() []byte {
	return slices.Clone(s.notice)
}

func (s *Snapshot) readMeta(root *os.Root) error {
	data, err := readRegular(root.FS(), metaFile, maxMetaBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return fault.New(fault.Usage, "%s is not a snapshot directory (no %s)", s.Dir, metaFile)
	}

	if err != nil {
		return fault.Wrap(fault.Integrity, err, "snapshot %s", s.Dir)
	}

	fields, err := strictObject(data, "commit", "tarballDigest", "formatVersion")
	if err != nil {
		return fault.Wrap(fault.Integrity, err, "snapshot metadata %s", filepath.Join(s.Dir, metaFile))
	}

	var meta snapshotMeta
	if err := decodeFields(fields, map[string]any{
		"commit": &meta.Commit, "tarballDigest": &meta.TarballDigest, "formatVersion": &meta.FormatVersion,
	}); err != nil {
		return fault.Wrap(fault.Integrity, err, "snapshot metadata %s", filepath.Join(s.Dir, metaFile))
	}

	switch {
	case meta.FormatVersion != snapshotFormatVersion:
		return fault.New(fault.Usage, "snapshot %s has format version %d, expected %d",
			s.Dir, meta.FormatVersion, snapshotFormatVersion)
	case !commitPattern.MatchString(meta.Commit):
		return fault.New(fault.Integrity, "snapshot %s records invalid commit %q", s.Dir, meta.Commit)
	}

	if err := digest.Validate(meta.TarballDigest); err != nil {
		return fault.Wrap(fault.Integrity, err, "snapshot %s tarball digest", s.Dir)
	}

	s.Commit, s.TarballDigest = meta.Commit, meta.TarballDigest

	return nil
}

func (s *Snapshot) schemaFile(rawURL string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery {
		return "", false
	}

	escaped := u.EscapedPath()

	var name string

	switch strings.ToLower(u.Host) {
	case "www.schemastore.org", "json.schemastore.org":
		rest, ok := strings.CutPrefix(escaped, "/schemas/json/")
		if !ok {
			rest = strings.TrimPrefix(escaped, "/")
		}

		name = rest
	case "raw.githubusercontent.com":
		rest, ok := strings.CutPrefix(escaped, rawSchemaStorePrefix)
		if !ok || !strings.HasSuffix(rest, ".json") {
			return "", false
		}

		name = rest
	default:
		return "", false
	}

	if !plainFileName(name) {
		return "", false
	}

	if !strings.HasSuffix(name, ".json") {
		name += ".json"
	}

	if _, ok := s.schemas[name]; !ok {
		return "", false
	}

	return name, true
}

func (s *Snapshot) absolute(rel []string) []string {
	if len(rel) == 0 {
		return nil
	}

	out := make([]string, len(rel))
	for i, p := range rel {
		out[i] = filepath.Join(s.Dir, filepath.FromSlash(p))
	}

	return out
}

// CatalogEntry is one entry of the SchemaStore catalog. FileMatch holds the
// upstream patterns verbatim (minimatch syntax, not Schepherd's dialect).
// Problem explains why an entry breaks the catalog's rules; such an entry
// keeps only its name and URL, each when it is usable, to be reported.
type CatalogEntry struct {
	Versions    map[string]string
	Name        string
	Description string
	URL         string
	Problem     string
	FileMatch   []string
}

const (
	maxUpstreamEntries   = 100000
	maxUpstreamJSONDepth = 16
)

// ParseCatalog parses a SchemaStore catalog (catalog.json). It checks the
// members Schepherd consumes strictly: "version" is 1, "$schema" a string,
// "schemas" an array; each entry has name, description and url and may have
// fileMatch and versions, with exact key case and no null values; fileMatch
// patterns are non-empty strings; URLs are absolute http(s) URLs without
// credentials. Duplicate keys, malformed JSON and oversized documents are
// rejected everywhere. Other members, at the top level or in an entry, are
// ignored whatever their value: SchemaStore owns the format and may add
// fields at any time. A document that breaks these rules is an error, an
// entry that breaks them an entry with a Problem: one bad record must not
// keep every other record from being prepared.
func ParseCatalog(data []byte) ([]CatalogEntry, error) {
	if err := jsonutil.Check(data, maxUpstreamJSONDepth); err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "catalog")
	}

	top, err := jsonObject(data)
	if err == nil {
		err = rejectNull(top, "$schema", "version", "schemas")
	}

	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "catalog")
	}

	if raw, ok := top["version"]; ok {
		var version int
		if err := json.Unmarshal(raw, &version); err != nil || version != 1 {
			return nil, fault.New(fault.Integrity, "catalog version must be 1, got %s", raw)
		}
	}

	if raw, ok := top["$schema"]; ok {
		var schema string
		if err := json.Unmarshal(raw, &schema); err != nil {
			return nil, fault.New(fault.Integrity, "catalog $schema must be a string")
		}
	}

	rawSchemas, ok := top["schemas"]
	if !ok {
		return nil, fault.New(fault.Integrity, "catalog has no schemas")
	}

	var items []json.RawMessage
	if err := json.Unmarshal(rawSchemas, &items); err != nil || items == nil {
		return nil, fault.New(fault.Integrity, "catalog schemas must be an array")
	}

	if len(items) > maxUpstreamEntries {
		return nil, fault.New(fault.Integrity, "catalog has more than %d entries", maxUpstreamEntries)
	}

	entries := make([]CatalogEntry, 0, len(items))

	for i, item := range items {
		entry, err := parseEntry(item)
		if err != nil {
			entry = invalidEntry(item, fmt.Sprintf("catalog entry %d: %v", i, err))
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

// invalidEntry identifies an entry that breaks the rules by the name and
// URL it has, when they are strings (and the URL a usable one).
func invalidEntry(item json.RawMessage, problem string) CatalogEntry {
	e := CatalogEntry{Problem: problem}

	var fields map[string]json.RawMessage
	if json.Unmarshal(item, &fields) != nil {
		return e
	}

	var name, uri string
	if json.Unmarshal(fields["name"], &name) == nil {
		e.Name = name
	}

	if json.Unmarshal(fields["url"], &uri) == nil && checkHTTPURL(uri) == nil {
		e.URL = uri
	}

	return e
}

func parseEntry(item json.RawMessage) (CatalogEntry, error) {
	fields, err := jsonObject(item)
	if err != nil {
		return CatalogEntry{}, err
	}

	for _, required := range []string{"name", "description", "url"} {
		if _, ok := fields[required]; !ok {
			return CatalogEntry{}, fmt.Errorf("missing %q", required)
		}
	}

	var (
		e         CatalogEntry
		fileMatch []*string
		versions  map[string]*string
	)

	if err := decodeFields(fields, map[string]any{
		"name": &e.Name, "description": &e.Description, "url": &e.URL, "fileMatch": &fileMatch, "versions": &versions,
	}); err != nil {
		return CatalogEntry{}, err
	}

	if e.Name == "" {
		return CatalogEntry{}, errors.New("empty name")
	}

	if e.FileMatch, err = nonNullStrings(e.Name, fileMatch); err != nil {
		return CatalogEntry{}, err
	}

	if e.Versions, err = nonNullValues(e.Name, versions); err != nil {
		return CatalogEntry{}, err
	}

	if err := checkHTTPURL(e.URL); err != nil {
		return CatalogEntry{}, fmt.Errorf("%q url: %w", e.Name, err)
	}

	for _, version := range slices.Sorted(maps.Keys(e.Versions)) {
		if err := checkHTTPURL(e.Versions[version]); err != nil {
			return CatalogEntry{}, fmt.Errorf("%q version %q: %w", e.Name, version, err)
		}
	}

	return e, nil
}

// nonNullStrings keeps an absent fileMatch nil and an empty one non-nil.
func nonNullStrings(name string, items []*string) ([]string, error) {
	var out []string
	if items != nil {
		out = make([]string, len(items))
	}

	for i, item := range items {
		if item == nil || *item == "" {
			return nil, fmt.Errorf("%q fileMatch item %d must be a non-empty string", name, i)
		}

		out[i] = *item
	}

	return out, nil
}

func nonNullValues(name string, items map[string]*string) (map[string]string, error) {
	var out map[string]string
	if items != nil {
		out = make(map[string]string, len(items))
	}

	for key, value := range items {
		if value == nil {
			return nil, fmt.Errorf("%q version %q must be a string", name, key)
		}

		out[key] = *value
	}

	return out, nil
}

func jsonObject(data []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, errors.New("expected a JSON object")
	}

	return fields, nil
}

// strictObject decodes a JSON object and rejects members other than allowed.
// encoding/json matches struct fields case-insensitively, so members are
// compared here by exact name before any typed decoding.
func strictObject(data []byte, allowed ...string) (map[string]json.RawMessage, error) {
	fields, err := jsonObject(data)
	if err != nil {
		return nil, err
	}

	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if !slices.Contains(allowed, name) {
			return nil, fmt.Errorf("unknown member %q", name)
		}
	}

	return fields, nil
}

// decodeFields decodes the members named in targets and ignores the others.
// Null targets are refused: encoding/json treats null as "leave unchanged",
// which would make a null string, array or object indistinguishable from an
// empty or absent one.
func decodeFields(fields map[string]json.RawMessage, targets map[string]any) error {
	names := slices.Sorted(maps.Keys(targets))
	if err := rejectNull(fields, names...); err != nil {
		return err
	}

	for _, name := range names {
		raw, ok := fields[name]
		if !ok {
			continue
		}

		if err := json.Unmarshal(raw, targets[name]); err != nil {
			return fmt.Errorf("member %q has the wrong type", name)
		}
	}

	return nil
}

func rejectNull(fields map[string]json.RawMessage, names ...string) error {
	for _, name := range names {
		if raw, ok := fields[name]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("member %q must not be null", name)
		}
	}

	return nil
}

func checkHTTPURL(raw string) error {
	u, err := url.Parse(raw)

	switch {
	case err != nil:
		if parseErr, ok := errors.AsType[*url.Error](err); ok {
			err = parseErr.Err
		}

		return fmt.Errorf("invalid URL: %w", err)
	case u.Scheme != "https" && u.Scheme != "http", u.Host == "", u.Opaque != "":
		return fmt.Errorf("%q is not an absolute http(s) URL", raw)
	case u.User != nil:
		return fmt.Errorf("%q contains credentials", u.Redacted())
	}

	return nil
}
