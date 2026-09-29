// Package tomlfile decodes the publisher's TOML source descriptions
// (sources/*.toml) strictly: unknown keys are errors with their position, and
// keys must match the declared names exactly. The exact-case check exists
// because go-toml matches keys case-insensitively even in strict mode, which
// would let "URL" and "url" silently override each other.
package tomlfile

import (
	"bytes"
	"encoding"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"

	"github.com/ovineko/schepherd/internal/fault"
)

var textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()

// Read returns the content of path, refusing files larger than maxBytes.
// Failures are fault.Usage errors.
func Read(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "read %s", path)
	}

	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "read %s", path)
	}

	if int64(len(data)) > maxBytes {
		return nil, fault.New(fault.Usage, "%s is larger than %d bytes", path, maxBytes)
	}

	return data, nil
}

// Decode reads path (at most maxBytes) and decodes it into v, which must be a
// pointer to a struct whose fields carry toml tags. Errors are fault.Usage and
// name the file.
func Decode(path string, maxBytes int64, v any) error {
	data, err := Read(path, maxBytes)
	if err != nil {
		return err
	}

	if err := DecodeBytes(data, v); err != nil {
		return fault.Wrap(fault.Usage, err, "%s", path)
	}

	return nil
}

// DecodeBytes decodes data into v with the same rules as Decode.
func DecodeBytes(data []byte, v any) error {
	if !utf8.Valid(data) {
		return errors.New("TOML is not valid UTF-8")
	}

	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		return describe(err)
	}

	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		return describe(err)
	}

	return exactKeys(raw, reflect.TypeOf(v), "")
}

func describe(err error) error {
	if strict, ok := errors.AsType[*toml.StrictMissingError](err); ok {
		msgs := make([]string, 0, len(strict.Errors))

		for i := range strict.Errors {
			row, col := strict.Errors[i].Position()
			msgs = append(msgs, fmt.Sprintf("line %d, column %d: unknown key %q",
				row, col, strings.Join(strict.Errors[i].Key(), ".")))
		}

		return errors.New(strings.Join(msgs, "; "))
	}

	if decodeErr, ok := errors.AsType[*toml.DecodeError](err); ok {
		row, col := decodeErr.Position()

		return fmt.Errorf("line %d, column %d: %w", row, col, err)
	}

	return err
}

func exactKeys(value any, t reflect.Type, at string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	if reflect.PointerTo(t).Implements(textUnmarshaler) {
		return nil
	}

	kind := t.Kind()

	if kind == reflect.Slice || kind == reflect.Array {
		items, _ := value.([]any)

		for i, item := range items {
			if err := exactKeys(item, t.Elem(), fmt.Sprintf("%s[%d]", at, i)); err != nil {
				return err
			}
		}

		return nil
	}

	table, ok := value.(map[string]any)
	if !ok || (kind != reflect.Struct && kind != reflect.Map) {
		return nil
	}

	var fields map[string]reflect.Type
	if kind == reflect.Struct {
		fields = tomlFields(t)
	}

	for _, key := range sortedKeys(table) {
		var child reflect.Type

		if kind == reflect.Map {
			child = t.Elem()
		} else if child, ok = fields[key]; !ok {
			return fmt.Errorf("unknown key %q (keys are case-sensitive)", join(at, key))
		}

		if err := exactKeys(table[key], child, join(at, key)); err != nil {
			return err
		}
	}

	return nil
}

func tomlFields(t reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type, t.NumField())

	for field := range t.Fields() {
		if !field.IsExported() {
			continue
		}

		name, _, _ := strings.Cut(field.Tag.Get("toml"), ",")

		switch name {
		case "-":
			continue
		case "":
			name = field.Name
		}

		fields[name] = field.Type
	}

	return fields
}

func join(at, key string) string {
	if at == "" {
		return key
	}

	return at + "." + key
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}
