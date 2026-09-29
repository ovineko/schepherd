// Package ids assigns stable catalog IDs to schema sources.
//
// An ID, once published for a source, is part of every user's configuration
// (mappings and `schepherd path <id>` calls), so assignment favors stability
// over tidiness: explicit overrides first, then the ID the source already had
// in the published state (catalog/state.json), and only then an ID derived
// from the URL. The result depends only on the set of inputs, never on
// their order.
package ids

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/jsonutil"
)

// MaxLength is the longest valid ID.
const MaxLength = 128

const (
	fallbackID        = "schema"
	maxHostSlugLength = 63
	maxOverridesBytes = 4 << 20
	maxOverridesDepth = 4
)

var pattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,126}[a-z0-9])?$`)

// Valid reports whether id matches the catalog ID syntax
// ^[a-z0-9](?:[a-z0-9._-]{0,126}[a-z0-9])?$ and contains no "..".
func Valid(id string) bool {
	return pattern.MatchString(id) && !strings.Contains(id, "..")
}

// Derive returns a valid ID for sourceURL built from the last path segment:
// ".schema.json" or ".json" is stripped, letters are lowercased, runs of
// other characters become "-", repeated dots collapse and the result is
// trimmed to the ID syntax. A URL without a usable segment falls back to its
// host name and then to "schema". A URL with a fragment names one subschema
// of the document, so the last segment of the fragment is appended
// ("ansible.json#/$defs/tasks" becomes "ansible-tasks").
func Derive(sourceURL string) string {
	id := deriveDocument(sourceURL)

	_, fragment, _ := strings.Cut(sourceURL, "#")

	name := fragment[strings.LastIndexByte(fragment, '/')+1:]
	if unescaped, err := url.PathUnescape(name); err == nil {
		name = unescaped
	}

	suffix := trimID(slug(strings.ToLower(name), false), MaxLength/2)
	if suffix == "" {
		return id
	}

	return trimID(id, MaxLength-len(suffix)-1) + "-" + suffix
}

func deriveDocument(sourceURL string) string {
	segment, host := lastSegment(sourceURL)

	segment = strings.ToLower(segment)
	if trimmed, ok := strings.CutSuffix(segment, ".schema.json"); ok {
		segment = trimmed
	} else {
		segment = strings.TrimSuffix(segment, ".json")
	}

	if id := slug(segment, true); id != "" {
		return id
	}

	if id := slug(strings.ToLower(host), true); id != "" {
		return id
	}

	return fallbackID
}

func lastSegment(sourceURL string) (segment, host string) {
	p := sourceURL

	if u, err := url.Parse(sourceURL); err == nil {
		p, host = u.Path, u.Hostname()
		if u.Opaque != "" {
			p = u.Opaque
		}
	}

	p = strings.TrimRight(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		p = p[i+1:]
	}

	return p, host
}

// slug maps s (already lowercased) to ID characters: runs of other
// characters become "-" and runs of dots collapse to one. Without keepDots,
// dots count as other characters.
func slug(s string, keepDots bool) string {
	var b strings.Builder

	dash := false

	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' && (!keepDots || r != '.') {
			dash = true

			continue
		}

		if dash {
			b.WriteByte('-')

			dash = false
		}

		b.WriteRune(r)
	}

	out := b.String()
	for strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", ".")
	}

	return trimID(out, MaxLength)
}

func trimID(s string, limit int) string {
	s = strings.TrimLeft(s, "._-")
	if limit <= 0 {
		return ""
	}

	if len(s) > limit {
		s = s[:limit]
	}

	return strings.TrimRight(s, "._-")
}

// Collision describes one ID that more than one source wanted.
type Collision struct {
	// ID is the contested ID.
	ID string
	// Resolution says who kept ID and what every other source received.
	Resolution string
	// Sources lists every source that wanted ID, sorted.
	Sources []string
}

type claim int

const (
	claimOverride claim = iota
	claimPrevious
	claimDerived
)

func (c claim) String() string {
	switch c {
	case claimOverride:
		return "override"
	case claimPrevious:
		return "published state"
	case claimDerived:
		return "derived, first in sorted order"
	}

	return "unknown"
}

type assigner struct {
	result   map[string]string
	owner    map[string]string
	how      map[string]claim
	contests map[string][]string
}

// Assign maps every source to a unique ID. Precedence: overrides (source URL
// to ID, from sources/ids.json), then the ID the source had in previous, then
// Derive. When several sources want the same ID, the one that already owns it
// keeps it and the others receive "-<host-slug>", or failing that "-2", "-3",
// … suffixes, in sorted source order. Every such conflict is reported as a
// Collision, sorted by ID. Invalid override or previous IDs, empty sources and
// two sources overriding to the same ID are fault.Usage errors.
func Assign(sources []string, overrides, previous map[string]string) (map[string]string, []Collision, error) {
	if err := checkOverrides(overrides); err != nil {
		return nil, nil, err
	}

	for _, source := range sortedKeys(previous) {
		if !Valid(previous[source]) {
			return nil, nil, fault.New(fault.Usage, "the published state assigns invalid ID %q to %s", previous[source], source)
		}
	}

	sorted := slices.Clone(sources)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)

	if len(sorted) > 0 && sorted[0] == "" {
		return nil, nil, fault.New(fault.Usage, "empty source URL")
	}

	a := &assigner{
		result:   make(map[string]string, len(sorted)),
		owner:    make(map[string]string, len(sorted)),
		how:      make(map[string]claim, len(sorted)),
		contests: make(map[string][]string),
	}

	var pending []string

	for _, source := range sorted {
		if id, ok := overrides[source]; ok {
			a.take(id, source, claimOverride)
		}
	}

	for _, source := range sorted {
		if _, done := a.result[source]; done {
			continue
		}

		if id, ok := previous[source]; ok && a.claim(id, source, claimPrevious) {
			continue
		}

		pending = append(pending, source)
	}

	var losers []string

	for _, source := range pending {
		if !a.claim(Derive(source), source, claimDerived) {
			losers = append(losers, source)
		}
	}

	for _, source := range losers {
		a.take(a.suffixed(Derive(source), source), source, claimDerived)
	}

	return a.result, a.collisions(), nil
}

func checkOverrides(overrides map[string]string) error {
	byID := make(map[string]string, len(overrides))

	for _, source := range sortedKeys(overrides) {
		id := overrides[source]

		if source == "" {
			return fault.New(fault.Usage, "ID override for an empty source URL")
		}

		if !Valid(id) {
			return fault.New(fault.Usage, "ID override %q for %s is not a valid ID", id, source)
		}

		if other, ok := byID[id]; ok {
			return fault.New(fault.Usage, "ID overrides map both %s and %s to %q", other, source, id)
		}

		byID[id] = source
	}

	return nil
}

func (a *assigner) take(id, source string, how claim) {
	a.result[source] = id
	a.owner[id] = source
	a.how[id] = how
}

func (a *assigner) claim(id, source string, how claim) bool {
	if _, taken := a.owner[id]; taken {
		if !slices.Contains(a.contests[id], source) {
			a.contests[id] = append(a.contests[id], source)
		}

		return false
	}

	a.take(id, source, how)

	return true
}

func (a *assigner) suffixed(base, source string) string {
	if host := hostSlug(source); host != "" {
		if id := withSuffix(base, "-"+host); a.free(id) {
			return id
		}
	}

	for n := 2; ; n++ {
		if id := withSuffix(base, "-"+strconv.Itoa(n)); a.free(id) {
			return id
		}
	}
}

func (a *assigner) free(id string) bool {
	_, taken := a.owner[id]

	return !taken && Valid(id)
}

func (a *assigner) collisions() []Collision {
	out := make([]Collision, 0, len(a.contests))

	for _, id := range sortedKeys(a.contests) {
		winner := a.owner[id]

		var resolution strings.Builder

		fmt.Fprintf(&resolution, "%s keeps %s (%s)", winner, id, a.how[id])

		losers := slices.Clone(a.contests[id])
		slices.Sort(losers)

		for _, source := range losers {
			fmt.Fprintf(&resolution, "; %s gets %s", source, a.result[source])
		}

		sources := append([]string{winner}, losers...)
		slices.Sort(sources)

		out = append(out, Collision{ID: id, Sources: sources, Resolution: resolution.String()})
	}

	return out
}

func withSuffix(base, suffix string) string {
	return trimID(base, MaxLength-len(suffix)) + suffix
}

func hostSlug(source string) string {
	u, err := url.Parse(source)
	if err != nil {
		return ""
	}

	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")

	return trimID(slug(host, false), maxHostSlugLength)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}

// LoadOverrides reads an ID override file (sources/ids.json): one JSON object
// mapping absolute http(s) source URLs to IDs. Duplicate keys, invalid IDs and
// two URLs mapped to the same ID are fault.Usage errors.
func LoadOverrides(path string) (map[string]string, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "read ID overrides")
	}

	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxOverridesBytes+1))
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "read ID overrides %s", path)
	}

	if len(data) > maxOverridesBytes {
		return nil, fault.New(fault.Usage, "ID overrides %s exceed %d bytes", path, maxOverridesBytes)
	}

	overrides, err := ParseOverrides(data)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "ID overrides %s", path)
	}

	return overrides, nil
}

// ParseOverrides validates the content of an ID override file.
func ParseOverrides(data []byte) (map[string]string, error) {
	if err := jsonutil.Check(data, maxOverridesDepth); err != nil {
		return nil, fault.Wrap(fault.Usage, err, "invalid JSON")
	}

	var overrides map[string]string

	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&overrides); err != nil {
		return nil, fault.Wrap(fault.Usage, err, "expected an object of source URL to ID strings")
	}

	if overrides == nil {
		return nil, fault.New(fault.Usage, "expected an object of source URL to ID strings, got null")
	}

	for _, source := range sortedKeys(overrides) {
		u, err := url.Parse(source)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return nil, fault.New(fault.Usage, "override key %q is not an absolute http(s) URL without credentials", source)
		}
	}

	if err := checkOverrides(overrides); err != nil {
		return nil, err
	}

	return overrides, nil
}
