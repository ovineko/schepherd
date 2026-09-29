package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type kind int

const (
	kindString kind = iota + 1
	kindInteger
	kindObject
	kindArray
)

var kindNames = map[kind]string{
	kindString:  "a string",
	kindInteger: "an integer",
	kindObject:  "an object",
	kindArray:   "an array",
}

var integerLiteral = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)$`)

type shape struct {
	members    map[string]*shape
	elem       *shape
	maxItems   func(Limits) int
	required   []string
	kind       kind
	allowEmpty bool
}

func text() *shape {
	return &shape{kind: kindString}
}

func integer() *shape {
	return &shape{kind: kindInteger}
}

func object(members map[string]*shape, required ...string) *shape {
	return &shape{kind: kindObject, members: members, required: required}
}

func list(elem *shape, maxItems func(Limits) int) *shape {
	return &shape{kind: kindArray, elem: elem, maxItems: maxItems}
}

func fixed(n int) func(Limits) int {
	return func(Limits) int { return n }
}

var catalogShape = func() *shape {
	descriptor := object(map[string]*shape{
		"digest":    text(),
		"mediaType": text(),
		"size":      integer(),
	}, "digest", "mediaType", "size")

	dependency := object(map[string]*shape{
		"digest": text(),
		"source": text(),
	}, "digest", "source")

	provenance := object(map[string]*shape{
		"dependencies": list(dependency, fixed(maxDependencies)),
		"license":      text(),
		"source":       text(),
		"sourceDigest": text(),
	}, "source")

	entry := object(map[string]*shape{
		"artifact":    descriptor,
		"description": text(),
		"dialect":     text(),
		"fileMatch":   list(text(), fixed(maxPatternsPerEntry)),
		"id":          text(),
		"name":        text(),
		"provenance":  provenance,
	}, "artifact", "id", "name")

	schemas := list(entry, func(l Limits) int { return l.MaxEntries })
	schemas.allowEmpty = true

	return object(map[string]*shape{
		"formatVersion": integer(),
		"revision":      text(),
		"schemas":       schemas,
	}, "formatVersion", "revision", "schemas")
}()

type pathSegment struct {
	key     string
	index   int
	isIndex bool
}

type walker struct {
	dec    *json.Decoder
	path   []pathSegment
	limits Limits
}

// checkStructure verifies member names, required members, JSON types and
// array bounds on the token stream before typed decoding, because
// encoding/json matches member names case-insensitively, turns null into a
// zero value and cannot tell an empty optional member from a missing one.
// Every string and array except the schemas list must be non-empty: optional
// members are omitted rather than left empty, which keeps the canonical
// encoding unique. Arrays are bounded while streaming, so an oversized
// catalog is rejected before it is decoded.
func checkStructure(data []byte, limits Limits) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	w := walker{dec: dec, limits: limits}

	return w.value(catalogShape)
}

func (w *walker) value(s *shape) error {
	tok, err := w.dec.Token()
	if err != nil {
		return fmt.Errorf("%s: %w", w.location(), err)
	}

	if tok == nil {
		return w.errorf("must not be null")
	}

	switch s.kind {
	case kindObject:
		if tok == json.Delim('{') {
			return w.object(s)
		}
	case kindArray:
		if tok == json.Delim('[') {
			return w.array(s)
		}
	case kindString:
		if str, ok := tok.(string); ok {
			if str == "" {
				return w.errorf("must not be empty")
			}

			return nil
		}
	case kindInteger:
		if num, ok := tok.(json.Number); ok {
			return w.integer(num)
		}
	}

	return w.errorf("must be %s", kindNames[s.kind])
}

func (w *walker) integer(num json.Number) error {
	literal := num.String()
	if !integerLiteral.MatchString(literal) {
		return w.errorf("must be an integer, got %s", clip(literal))
	}

	if _, err := strconv.ParseInt(literal, 10, 64); err != nil {
		return w.errorf("integer %s is out of range", clip(literal))
	}

	return nil
}

func (w *walker) object(s *shape) error {
	seen := make(map[string]bool, len(s.members))

	for w.dec.More() {
		tok, err := w.dec.Token()
		if err != nil {
			return fmt.Errorf("%s: %w", w.location(), err)
		}

		key, ok := tok.(string)
		if !ok {
			return w.errorf("unexpected token %v", tok)
		}

		child, known := s.members[key]
		if !known {
			return w.errorf("unknown member %s", quote(key))
		}

		seen[key] = true

		w.path = append(w.path, pathSegment{key: key})
		if err := w.value(child); err != nil {
			return err
		}

		w.path = w.path[:len(w.path)-1]
	}

	if err := w.closing('}'); err != nil {
		return err
	}

	for _, name := range s.required {
		if !seen[name] {
			return w.errorf("missing required member %q", name)
		}
	}

	return nil
}

func (w *walker) array(s *shape) error {
	limit := s.maxItems(w.limits)
	count := 0

	for w.dec.More() {
		if count == limit {
			return w.errorf("has more than %d items", limit)
		}

		w.path = append(w.path, pathSegment{index: count, isIndex: true})
		if err := w.value(s.elem); err != nil {
			return err
		}

		w.path = w.path[:len(w.path)-1]
		count++
	}

	if err := w.closing(']'); err != nil {
		return err
	}

	if count == 0 && !s.allowEmpty {
		return w.errorf("must not be empty")
	}

	return nil
}

func (w *walker) closing(want json.Delim) error {
	tok, err := w.dec.Token()
	if err != nil {
		return fmt.Errorf("%s: %w", w.location(), err)
	}

	if tok != want {
		return w.errorf("expected %v, got %v", want, tok)
	}

	return nil
}

func (w *walker) location() string {
	var b strings.Builder

	for _, seg := range w.path {
		if seg.isIndex {
			b.WriteString("[" + strconv.Itoa(seg.index) + "]")

			continue
		}

		if b.Len() > 0 {
			b.WriteByte('.')
		}

		b.WriteString(seg.key)
	}

	if b.Len() == 0 {
		return "document"
	}

	return b.String()
}

func (w *walker) errorf(format string, args ...any) error {
	return errors.New(w.location() + ": " + fmt.Sprintf(format, args...))
}
