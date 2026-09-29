package bundle

import (
	"regexp"
	"slices"
	"strings"
)

// legacyAssertionKeywords are the draft-04/06/07 keywords that constrain an
// instance (the union over the three dialects). Next to a top-level $ref
// those dialects ignore them while many tools still apply them, so a
// document that has one means different things to different consumers and
// no rewrite can keep its meaning for all of them.
var legacyAssertionKeywords = map[string]struct{}{
	"additionalItems": {}, "additionalProperties": {}, "allOf": {}, "anyOf": {}, "const": {}, "contains": {},
	"contentEncoding": {}, "contentMediaType": {}, "dependencies": {}, "else": {}, "enum": {}, "exclusiveMaximum": {},
	"exclusiveMinimum": {}, "format": {}, "if": {}, "items": {}, "maxItems": {}, "maxLength": {}, "maxProperties": {},
	"maximum": {}, "minItems": {}, "minLength": {}, "minProperties": {}, "minimum": {}, "multipleOf": {}, "not": {},
	"oneOf": {}, "pattern": {}, "patternProperties": {}, "properties": {}, "propertyNames": {}, "required": {},
	"then": {}, "type": {}, "uniqueItems": {},
}

// wrapLegacyRef rewrites a draft-04/06/07 document whose top level is a $ref
// into one that can carry an identifier and embedded resources: the $ref
// member becomes "allOf":[{"$ref":…}] in place and every other byte is kept.
// Those dialects ignore the siblings of $ref, including the identifier and
// the definitions bundling adds, which is why the bundler refuses the
// original form. The rewrite keeps the document's meaning only when no
// sibling constrains an instance: identifiers, $schema, definitions,
// annotations and unknown keywords are inert whether ignored or not, and
// JSON Pointers into the document keep their targets because no member
// moves. A document with another sibling is rejected.
func wrapLegacyRef(data []byte, tl topLevel, uri, dialect string) ([]byte, error) {
	var active []string

	for key := range tl.members {
		if _, ok := legacyAssertionKeywords[key]; ok {
			active = append(active, key)
		}
	}

	if len(active) > 0 {
		slices.Sort(active)

		return nil, reject(ReasonTopLevelRefDraft7,
			"%s is a top-level $ref in %s next to %s, which that dialect ignores but other tools apply, so no rewrite keeps its meaning",
			uri, dialect, strings.Join(active, ", "))
	}

	s := tl.members["$ref"]

	out := make([]byte, 0, len(data)+len(`"allOf":[{"$ref":}]`))
	out = append(out, data[:s.key]...)
	out = append(out, `"allOf":[{"$ref":`...)
	out = append(out, data[s.start:s.end]...)
	out = append(out, "}]"...)

	return append(out, data[s.end:]...), nil
}

// plainName is the syntax of a plain-name fragment ($anchor in 2019-09 and
// later, "#name" identifiers before).
var plainName = regexp.MustCompile(`^[A-Za-z_][-A-Za-z0-9._]*$`)

// validFragment reports whether a root URI fragment names a subschema the
// way references do: a JSON Pointer or a plain name.
func validFragment(fragment string) bool {
	return strings.HasPrefix(fragment, "/") || plainName.MatchString(fragment)
}

// fragmentWrapper returns the root the bundler receives for a root URI with
// a fragment: a schema that only applies the subschema the fragment names.
// The document itself is embedded next to it as a resource, so every
// reference inside it keeps resolving exactly as in the original. The
// wrapper is identified by id only while the CLI works on it.
func fragmentWrapper(d dialect, dialectURI, id, target string) ([]byte, error) {
	schema, err := encodeString(dialectURI)
	if err != nil {
		return nil, err
	}

	idKey, err := encodeString(d.idKey)
	if err != nil {
		return nil, err
	}

	idValue, err := encodeString(id)
	if err != nil {
		return nil, err
	}

	ref, err := encodeString(target)
	if err != nil {
		return nil, err
	}

	out := []byte(`{"$schema":`)
	out = append(out, schema...)
	out = append(out, ',')
	out = append(out, idKey...)
	out = append(out, ':')
	out = append(out, idValue...)
	out = append(out, `,"allOf":[{"$ref":`...)
	out = append(out, ref...)

	return append(out, "}]}"...), nil
}
