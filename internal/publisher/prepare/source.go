package prepare

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/match"
	"github.com/ovineko/schepherd/internal/publisher/bundle"
	"github.com/ovineko/schepherd/internal/publisher/httpfetch"
	"github.com/ovineko/schepherd/internal/publisher/ids"
	"github.com/ovineko/schepherd/internal/publisher/policy"
	"github.com/ovineko/schepherd/internal/publisher/upstream"
	"github.com/ovineko/schepherd/internal/publisher/upstream/tomlfile"
)

const (
	maxSourceFileBytes = 4 << 20
	maxLocalEntries    = 20000
	maxLocalDocuments  = 20000
	maxInstances       = 1000
	maxURLBytes        = 4096

	defaultMaxDepth         = 8
	defaultMaxPerSchema     = 64
	defaultMaxDocumentBytes = 16 << 20
	defaultMaxTotalBytes    = 256 << 20
)

// source is a loaded source description.
type source struct {
	upstream *upstream.SchemaStoreConfig
	local    *localSet
	// path is the absolute source file; dir its directory.
	path   string
	dir    string
	policy string
	ids    string
	fetch  httpfetch.Policy
	limits bundle.Limits
}

type localSet struct {
	name      string
	entries   []localEntry
	documents []localDocument
}

type localEntry struct {
	id          string
	name        string
	description string
	url         string
	file        string
	license     string
	fileMatch   []string
	instances   []instanceFile
}

type instanceFile struct {
	// name is the path as written in the source file; it names the
	// instance in reports without revealing local directories.
	name string
	path string
}

type localDocument struct {
	uri     string
	file    string
	license string
}

type localDoc struct {
	Dependencies *dependenciesDoc `toml:"dependencies"`
	Fetch        *fetchDoc        `toml:"fetch"`
	Kind         string           `toml:"kind"`
	Name         string           `toml:"name"`
	Policy       string           `toml:"policy"`
	IDs          string           `toml:"ids"`
	Entries      []localEntryDoc  `toml:"entries"`
	Documents    []documentDoc    `toml:"documents"`
}

type dependenciesDoc struct {
	MaxDepth         int   `toml:"max_depth"`
	MaxPerSchema     int   `toml:"max_per_schema"`
	MaxDocumentBytes int64 `toml:"max_document_bytes"`
	MaxTotalBytes    int64 `toml:"max_total_bytes"`
}

type fetchDoc struct {
	AllowPrivateHosts []string `toml:"allow_private_hosts"`
	AllowHTTP         bool     `toml:"allow_http"`
}

type localEntryDoc struct {
	ID          string   `toml:"id"`
	Name        string   `toml:"name"`
	Description string   `toml:"description"`
	URL         string   `toml:"url"`
	File        string   `toml:"file"`
	License     string   `toml:"license"`
	FileMatch   []string `toml:"file_match"`
	Instances   []string `toml:"instances"`
}

type documentDoc struct {
	URI     string `toml:"uri"`
	File    string `toml:"file"`
	License string `toml:"license"`
}

// loadSource reads a source description. The kind is read first; "upstream"
// files are the pinned SchemaStore description (sources/schemastore.toml)
// and are decoded by package upstream, "local" files by loadLocal.
func loadSource(path string) (*source, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "source file")
	}

	data, err := tomlfile.Read(path, maxSourceFileBytes)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "source file")
	}

	var probe struct {
		Kind string `toml:"kind"`
	}
	if err := toml.Unmarshal(data, &probe); err != nil {
		return nil, fault.Wrap(fault.Usage, err, "source file %s", path)
	}

	src := &source{path: path, dir: filepath.Dir(path)}

	switch probe.Kind {
	case upstream.KindUpstream:
		cfg, err := upstream.LoadSchemaStoreConfig(path)
		if err != nil {
			return nil, fault.Wrap(fault.Usage, err, "source file")
		}

		src.upstream = cfg
		src.limits = limitsOf(cfg.Dependencies)
		src.fetch = httpfetch.Policy{MaxBytes: cfg.Dependencies.MaxDocumentBytes}
	case upstream.KindLocal:
		if err := src.loadLocal(data); err != nil {
			return nil, fault.Wrap(fault.Usage, err, "local source file %s", path)
		}
	default:
		return nil, fault.New(fault.Usage, "source file %s: kind must be %q or %q, got %q", path, upstream.KindUpstream, upstream.KindLocal, probe.Kind)
	}

	return src, nil
}

func limitsOf(d upstream.DependencyLimits) bundle.Limits {
	return bundle.Limits{
		MaxDepth:         d.MaxDepth,
		MaxDocuments:     d.MaxPerSchema,
		MaxDocumentBytes: d.MaxDocumentBytes,
		MaxTotalBytes:    d.MaxTotalBytes,
	}
}

// loadLocal decodes a local source:
//
//	kind = "local"
//	name = "fixtures"
//	policy = "licenses.toml"   # optional, relative to this file
//	ids = "ids.json"           # optional, relative to this file
//
//	[dependencies]             # optional; zero keeps the default
//	max_depth = 8
//	max_per_schema = 64
//	max_document_bytes = 16777216
//	max_total_bytes = 268435456
//
//	[fetch]                    # optional
//	allow_http = false
//	allow_private_hosts = ["127.0.0.1:8080"]
//
//	[[entries]]
//	id = "company-config"      # optional
//	name = "company.json"
//	description = "…"          # optional
//	url = "https://schemas.example/company.json"
//	file = "schemas/company.json"            # optional
//	file_match = ["config/company.json"]     # optional
//	license = "MIT"                          # optional
//	instances = ["tests/company/valid.json"] # optional
//
//	[[documents]]              # dependency URIs served from local files
//	uri = "https://schemas.example/common.json"
//	file = "schemas/common.json"
//	license = "MIT"            # optional
func (s *source) loadLocal(data []byte) error {
	var doc localDoc
	if err := tomlfile.DecodeBytes(data, &doc); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	if strings.TrimSpace(doc.Name) == "" || len(doc.Name) > 256 || strings.ContainsFunc(doc.Name, isControl) {
		return errors.New("name is required: a single line of at most 256 bytes")
	}

	switch {
	case len(doc.Entries) == 0:
		return errors.New("no [[entries]]")
	case len(doc.Entries) > maxLocalEntries:
		return fmt.Errorf("more than %d entries", maxLocalEntries)
	case len(doc.Documents) > maxLocalDocuments:
		return fmt.Errorf("more than %d documents", maxLocalDocuments)
	}

	limits, err := localLimits(doc.Dependencies)
	if err != nil {
		return err
	}

	s.limits = limits
	s.fetch = httpfetch.Policy{MaxBytes: limits.MaxDocumentBytes}

	if doc.Fetch != nil {
		s.fetch.AllowHTTP = doc.Fetch.AllowHTTP
		s.fetch.AllowPrivateHosts = doc.Fetch.AllowPrivateHosts
	}

	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return fmt.Errorf("open source directory: %w", err)
	}

	defer func() { _ = root.Close() }()

	if s.policy, err = optionalFile(root, s.dir, "policy", doc.Policy); err != nil {
		return err
	}

	if s.ids, err = optionalFile(root, s.dir, "ids", doc.IDs); err != nil {
		return err
	}

	set := &localSet{name: doc.Name}
	if set.entries, err = localEntries(root, s.dir, doc.Entries); err != nil {
		return err
	}

	if set.documents, err = localDocuments(root, s.dir, doc.Documents, set.entries); err != nil {
		return err
	}

	s.local = set

	return nil
}

func localLimits(d *dependenciesDoc) (bundle.Limits, error) {
	limits := bundle.Limits{
		MaxDepth: defaultMaxDepth, MaxDocuments: defaultMaxPerSchema,
		MaxDocumentBytes: defaultMaxDocumentBytes, MaxTotalBytes: defaultMaxTotalBytes,
	}

	if d == nil {
		return limits, nil
	}

	if d.MaxDepth < 0 || d.MaxPerSchema < 0 || d.MaxDocumentBytes < 0 || d.MaxTotalBytes < 0 {
		return bundle.Limits{}, errors.New("dependencies limits must not be negative")
	}

	if d.MaxDepth > 0 {
		limits.MaxDepth = d.MaxDepth
	}

	if d.MaxPerSchema > 0 {
		limits.MaxDocuments = d.MaxPerSchema
	}

	if d.MaxDocumentBytes > 0 {
		limits.MaxDocumentBytes = d.MaxDocumentBytes
	}

	if d.MaxTotalBytes > 0 {
		limits.MaxTotalBytes = d.MaxTotalBytes
	}

	if limits.MaxDocumentBytes > limits.MaxTotalBytes {
		return bundle.Limits{}, errors.New("dependencies.max_document_bytes must not exceed dependencies.max_total_bytes")
	}

	return limits, nil
}

func localEntries(root *os.Root, dir string, docs []localEntryDoc) ([]localEntry, error) {
	entries := make([]localEntry, 0, len(docs))
	seenIDs := map[string]int{}
	seenURLs := map[string]int{}

	for i := range docs {
		e, err := localEntryOf(root, dir, &docs[i])
		if err != nil {
			return nil, fmt.Errorf("entry %d (%q): %w", i+1, docs[i].Name, err)
		}

		if e.id != "" {
			if prev, ok := seenIDs[e.id]; ok {
				return nil, fmt.Errorf("entries %d and %d both use id %q", prev, i+1, e.id)
			}

			seenIDs[e.id] = i + 1
		}

		key := normalizeURI(e.url)
		if prev, ok := seenURLs[key]; ok {
			return nil, fmt.Errorf("entries %d and %d both use url %q", prev, i+1, e.url)
		}

		seenURLs[key] = i + 1

		entries = append(entries, e)
	}

	return entries, nil
}

func localEntryOf(root *os.Root, dir string, d *localEntryDoc) (localEntry, error) {
	if d.ID != "" && !ids.Valid(d.ID) {
		return localEntry{}, fmt.Errorf("id %q is not a valid schema ID", d.ID)
	}

	if err := checkSourceURL(d.URL); err != nil {
		return localEntry{}, fmt.Errorf("url: %w", err)
	}

	if d.License != "" {
		if err := policy.CheckLicense(d.License); err != nil {
			return localEntry{}, fmt.Errorf("license: %w", err)
		}
	}

	for _, pattern := range d.FileMatch {
		if err := match.ValidatePattern(pattern); err != nil {
			return localEntry{}, fmt.Errorf("file_match: %w", err)
		}
	}

	patterns := dedupe(d.FileMatch)
	if err := CheckMetadata(d.Name, d.Description, "", patterns, nil); err != nil {
		return localEntry{}, err
	}

	e := localEntry{
		id: d.ID, name: d.Name, description: d.Description, url: d.URL, license: d.License, fileMatch: patterns,
	}

	var err error

	if d.File != "" {
		if e.file, err = regularFile(root, dir, d.File); err != nil {
			return localEntry{}, fmt.Errorf("file: %w", err)
		}
	}

	if len(d.Instances) > maxInstances {
		return localEntry{}, fmt.Errorf("more than %d instances", maxInstances)
	}

	for _, rel := range d.Instances {
		file, err := regularFile(root, dir, rel)
		if err != nil {
			return localEntry{}, fmt.Errorf("instances: %w", err)
		}

		e.instances = append(e.instances, instanceFile{name: rel, path: file})
	}

	return e, nil
}

func localDocuments(root *os.Root, dir string, docs []documentDoc, entries []localEntry) ([]localDocument, error) {
	taken := map[string]string{}

	for _, e := range entries {
		if e.file != "" {
			taken[normalizeURI(e.url)] = "an entry with a file"
		}
	}

	out := make([]localDocument, 0, len(docs))

	for i, d := range docs {
		if err := checkSourceURL(d.URI); err != nil {
			return nil, fmt.Errorf("document %d: uri: %w", i+1, err)
		}

		key := normalizeURI(d.URI)
		if owner, ok := taken[key]; ok {
			return nil, fmt.Errorf("document %d: %s is already served by %s", i+1, d.URI, owner)
		}

		taken[key] = fmt.Sprintf("document %d", i+1)

		if d.License != "" {
			if err := policy.CheckLicense(d.License); err != nil {
				return nil, fmt.Errorf("document %d: %w", i+1, err)
			}
		}

		file, err := regularFile(root, dir, d.File)
		if err != nil {
			return nil, fmt.Errorf("document %d: file: %w", i+1, err)
		}

		out = append(out, localDocument{uri: d.URI, file: file, license: d.License})
	}

	return out, nil
}

func optionalFile(root *os.Root, dir, key, rel string) (string, error) {
	if rel == "" {
		return "", nil
	}

	file, err := regularFile(root, dir, rel)
	if err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}

	return file, nil
}

// regularFile resolves rel against the source directory and requires a
// regular file inside it; symbolic links that leave the directory are
// refused by the os.Root lookup.
func regularFile(root *os.Root, dir, rel string) (string, error) {
	if rel == "" {
		return "", errors.New("path is empty")
	}

	name := filepath.FromSlash(rel)
	if strings.Contains(rel, "\\") || !filepath.IsLocal(name) {
		return "", fmt.Errorf("%q must be a relative path inside %s", rel, dir)
	}

	info, err := root.Stat(name)
	if err != nil {
		return "", fmt.Errorf("%q: %w", rel, err)
	}

	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a regular file", rel)
	}

	return filepath.Join(dir, name), nil
}

// checkSourceURL accepts absolute http(s) URLs without credentials or a
// fragment: a source always names a whole document.
func checkSourceURL(raw string) error {
	if raw == "" || len(raw) > maxURLBytes {
		return fmt.Errorf("must be a URL of 1-%d bytes", maxURLBytes)
	}

	u, err := url.Parse(raw)

	switch {
	case err != nil:
		return fmt.Errorf("invalid URL: %w", err)
	case u.Scheme != "https" && u.Scheme != "http", u.Host == "", u.Opaque != "":
		return fmt.Errorf("%q is not an absolute http(s) URL", raw)
	case u.User != nil:
		return fmt.Errorf("%q contains credentials", raw)
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return fmt.Errorf("%q has a fragment", raw)
	}

	return nil
}

// normalizeURI is the lookup key for URIs served from local files: scheme
// and host are case-insensitive and an empty fragment is dropped.
func normalizeURI(raw string) string {
	u, err := url.Parse(strings.TrimSuffix(raw, "#"))
	if err != nil {
		return raw
	}

	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)

	return u.String()
}

func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f
}

func dedupe(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))

	for _, v := range values {
		if _, ok := seen[v]; ok {
			continue
		}

		seen[v] = struct{}{}
		out = append(out, v)
	}

	return out
}
