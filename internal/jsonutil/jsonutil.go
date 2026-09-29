// Package jsonutil provides strict, lossless helpers for untrusted JSON.
//
// Validation walks the token stream instead of decoding into Go values so
// numbers are never converted to float64 and duplicate object keys, which
// RFC 8259 leaves implementation-defined, are rejected rather than silently
// resolved differently by different consumers.
package jsonutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// DefaultMaxDepth bounds nesting of arrays and objects.
const DefaultMaxDepth = 512

// ErrDuplicateKey reports an object with two members of the same name.
var ErrDuplicateKey = errors.New("duplicate object key")

// ErrTooDeep reports nesting beyond the configured maximum.
var ErrTooDeep = errors.New("JSON nesting too deep")

// Check verifies that data is exactly one well-formed JSON value encoded as
// UTF-8, contains no duplicate object keys and nests at most maxDepth levels.
func Check(data []byte, maxDepth int) error {
	if maxDepth <= 0 {
		maxDepth = DefaultMaxDepth
	}

	if !utf8.Valid(data) {
		return errors.New("JSON is not valid UTF-8")
	}

	if !json.Valid(data) {
		return errors.New("malformed JSON")
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	w := walker{maxDepth: maxDepth}

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("malformed JSON: %w", err)
		}

		if err := w.visit(tok); err != nil {
			return err
		}
	}
}

type frame struct {
	keys     map[string]struct{}
	isObject bool
	wantKey  bool
}

type walker struct {
	stack    []*frame
	maxDepth int
}

func (w *walker) visit(tok json.Token) error {
	var top *frame
	if len(w.stack) > 0 {
		top = w.stack[len(w.stack)-1]
	}

	if key, ok := tok.(string); ok && top != nil && top.isObject && top.wantKey {
		if _, seen := top.keys[key]; seen {
			return fmt.Errorf("%w %q", ErrDuplicateKey, key)
		}

		top.keys[key] = struct{}{}
		top.wantKey = false

		return nil
	}

	delim, isDelim := tok.(json.Delim)

	switch {
	case isDelim && (delim == '}' || delim == ']'):
		w.stack = w.stack[:len(w.stack)-1]

		return nil
	case top != nil && top.isObject:
		top.wantKey = true
	}

	if isDelim {
		if len(w.stack) >= w.maxDepth {
			return fmt.Errorf("%w (limit %d)", ErrTooDeep, w.maxDepth)
		}

		w.stack = append(w.stack, &frame{isObject: delim == '{', wantKey: delim == '{', keys: map[string]struct{}{}})
	}

	return nil
}

// Compact removes insignificant whitespace without re-encoding any value, so
// number literals, string escapes and member order are preserved exactly.
func Compact(data []byte, maxDepth int) ([]byte, error) {
	if err := Check(data, maxDepth); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		return nil, fmt.Errorf("compact JSON: %w", err)
	}

	return buf.Bytes(), nil
}
