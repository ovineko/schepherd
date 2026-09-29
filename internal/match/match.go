// Package match implements Schepherd's file-path matching dialect.
//
// Paths are workspace-relative and use "/" as separator on every platform.
// Matching is case-sensitive everywhere. A pattern without "/" is compared
// with the basename, so "package.json" matches at any depth; a pattern that
// contains "/" is compared with the whole relative path. A leading "./" or
// "/" anchors the pattern at the workspace root. A leading "!" marks a
// negative pattern: a rule matches a path when at least one positive pattern
// matches and no negative pattern does. Glob syntax (*, ?, [...], [!...],
// {a,b}, ** and backslash escapes) follows github.com/bmatcuk/doublestar/v4.
// Extended globs such as !(x) are rejected instead of being matched literally.
package match

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// ErrNoMatch is returned when no rule matches a path.
var ErrNoMatch = errors.New("no schema matches the file")

// ErrOutsideWorkspace is returned for paths that are not inside the workspace.
var ErrOutsideWorkspace = errors.New("file is outside the workspace")

// Origin identifies where a rule came from.
type Origin int

const (
	// OriginMapping is a local [[mappings]] entry from the configuration.
	OriginMapping Origin = iota
	// OriginCatalog is a fileMatch list from the pinned catalog.
	OriginCatalog
	// OriginLocal is the file_match list of a local [schemas] entry.
	OriginLocal
)

// String returns the machine-readable origin name.
func (o Origin) String() string {
	switch o {
	case OriginMapping:
		return "mapping"
	case OriginLocal:
		return "local"
	case OriginCatalog:
	}

	return "catalog"
}

// Pattern is a validated, normalized pattern.
type Pattern struct {
	glob     string
	negated  bool
	fullPath bool
}

// ParsePattern validates and normalizes one pattern.
func ParsePattern(raw string) (Pattern, error) {
	var p Pattern
	body := raw

	if rest, ok := strings.CutPrefix(body, "!"); ok {
		p.negated = true
		body = rest
	}

	switch {
	case strings.HasPrefix(body, "./"):
		body = strings.TrimPrefix(body, "./")
		p.fullPath = true
	case strings.HasPrefix(body, "/"):
		body = strings.TrimPrefix(body, "/")
		p.fullPath = true
	}

	switch {
	case body == "":
		return Pattern{}, fmt.Errorf("invalid pattern %q: empty", raw)
	case hasExtglob(raw):
		return Pattern{}, fmt.Errorf("invalid pattern %q: extended globs such as !(...) are not supported", raw)
	case strings.HasSuffix(body, "/"):
		return Pattern{}, fmt.Errorf("invalid pattern %q: directory patterns are not supported", raw)
	case !doublestar.ValidatePattern(body):
		return Pattern{}, fmt.Errorf("invalid pattern %q: %w", raw, doublestar.ErrBadPattern)
	}

	if strings.Contains(body, "/") {
		p.fullPath = true
	}

	p.glob = body

	return p, nil
}

func hasExtglob(raw string) bool {
	for i := 0; i+1 < len(raw); i++ {
		switch {
		case raw[i] == '\\':
			i++
		case raw[i+1] == '(' && strings.IndexByte("!?*+@", raw[i]) >= 0:
			return true
		}
	}

	return false
}

// ValidatePattern reports whether raw is a valid pattern in this dialect.
func ValidatePattern(raw string) error {
	_, err := ParsePattern(raw)

	return err
}

func (p Pattern) matches(rel string) bool {
	target := rel
	if !p.fullPath {
		target = path.Base(rel)
	}

	return doublestar.MatchUnvalidated(p.glob, target)
}

// Rule associates patterns with a schema ID.
type Rule struct {
	SchemaID string
	Patterns []string
}

type compiledRule struct {
	schemaID string
	positive []Pattern
	negative []Pattern
}

func (r compiledRule) matches(rel string) bool {
	hit := false

	for _, p := range r.positive {
		if p.matches(rel) {
			hit = true

			break
		}
	}

	if !hit {
		return false
	}

	for _, p := range r.negative {
		if p.matches(rel) {
			return false
		}
	}

	return true
}

// Set is an ordered collection of rules with one origin.
type Set struct {
	rules  []compiledRule
	origin Origin
}

// NewSet compiles rules. Rule order is preserved and determines the order of
// reported candidates.
func NewSet(origin Origin, rules []Rule) (*Set, error) {
	set := &Set{origin: origin, rules: make([]compiledRule, 0, len(rules))}

	for _, rule := range rules {
		compiled := compiledRule{schemaID: rule.SchemaID}

		for _, raw := range rule.Patterns {
			p, err := ParsePattern(raw)
			if err != nil {
				return nil, fmt.Errorf("schema %q: %w", rule.SchemaID, err)
			}

			if p.negated {
				compiled.negative = append(compiled.negative, p)
			} else {
				compiled.positive = append(compiled.positive, p)
			}
		}

		set.rules = append(set.rules, compiled)
	}

	return set, nil
}

// Match returns the distinct schema IDs whose rules match rel, in rule order.
func (s *Set) Match(rel string) []string {
	if s == nil {
		return nil
	}

	var ids []string

	for _, rule := range s.rules {
		if rule.matches(rel) && !slices.Contains(ids, rule.schemaID) {
			ids = append(ids, rule.schemaID)
		}
	}

	return ids
}

// AmbiguousError lists every schema that matched a path within one origin.
type AmbiguousError struct {
	Path       string
	Candidates []string
	Origin     Origin
}

// Error implements the error interface.
func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("%s matches several schemas via %s rules: %s", e.Path, e.Origin, strings.Join(e.Candidates, ", "))
}

// Resolution is the outcome of resolving one path.
type Resolution struct {
	SchemaID string
	Origin   Origin
}

// ResolveFirst tries sets in order of precedence and skips nil ones. Within
// the first set that matches, exactly one schema must match; a set that
// matches nothing is never consulted for ambiguity.
func ResolveFirst(rel string, sets ...*Set) (Resolution, error) {
	for _, set := range sets {
		if set == nil {
			continue
		}

		ids := set.Match(rel)

		switch len(ids) {
		case 0:
			continue
		case 1:
			return Resolution{SchemaID: ids[0], Origin: set.origin}, nil
		default:
			return Resolution{}, &AmbiguousError{Path: rel, Candidates: ids, Origin: set.origin}
		}
	}

	return Resolution{}, fmt.Errorf("%s: %w", rel, ErrNoMatch)
}

// RelativePath converts an absolute file path into the workspace-relative,
// slash-separated form used for matching.
func RelativePath(workspace, file string) (string, error) {
	if !filepath.IsAbs(workspace) || !filepath.IsAbs(file) {
		return "", fmt.Errorf("workspace %q and file %q must be absolute", workspace, file)
	}

	rel, err := filepath.Rel(filepath.Clean(workspace), filepath.Clean(file))
	if err != nil {
		return "", fmt.Errorf("%s: %w", file, ErrOutsideWorkspace)
	}

	rel = filepath.ToSlash(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%s: %w", file, ErrOutsideWorkspace)
	}

	return rel, nil
}
