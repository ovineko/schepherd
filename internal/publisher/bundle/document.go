package bundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// dialect is a JSON Schema dialect both the bundler and the verifying
// validator implement.
type dialect struct {
	draft *jsonschema.Draft
	// idKey is the keyword that sets a resource identifier.
	idKey string
	// legacy dialects ignore every keyword next to $ref.
	legacy bool
}

var dialects = map[string]dialect{
	"json-schema.org/draft-04/schema":      {draft: jsonschema.Draft4, idKey: "id", legacy: true},
	"json-schema.org/draft-06/schema":      {draft: jsonschema.Draft6, idKey: "$id", legacy: true},
	"json-schema.org/draft-07/schema":      {draft: jsonschema.Draft7, idKey: "$id", legacy: true},
	"json-schema.org/draft/2019-09/schema": {draft: jsonschema.Draft2019, idKey: "$id"},
	"json-schema.org/draft/2020-12/schema": {draft: jsonschema.Draft2020, idKey: "$id"},
}

// vocabularyMetaschemas are the official vocabulary metaschemas that both the
// bundler and the validator carry built in, so they are never fetched.
var vocabularyMetaschemas = map[string]struct{}{
	"json-schema.org/draft/2019-09/meta/core":              {},
	"json-schema.org/draft/2019-09/meta/applicator":        {},
	"json-schema.org/draft/2019-09/meta/validation":        {},
	"json-schema.org/draft/2019-09/meta/meta-data":         {},
	"json-schema.org/draft/2019-09/meta/format":            {},
	"json-schema.org/draft/2019-09/meta/content":           {},
	"json-schema.org/draft/2020-12/meta/core":              {},
	"json-schema.org/draft/2020-12/meta/applicator":        {},
	"json-schema.org/draft/2020-12/meta/unevaluated":       {},
	"json-schema.org/draft/2020-12/meta/validation":        {},
	"json-schema.org/draft/2020-12/meta/meta-data":         {},
	"json-schema.org/draft/2020-12/meta/format-annotation": {},
	"json-schema.org/draft/2020-12/meta/format-assertion":  {},
	"json-schema.org/draft/2020-12/meta/content":           {},
}

func metaschemaKey(uri string) (string, bool) {
	uri = strings.TrimSuffix(uri, "#")
	if rest, ok := strings.CutPrefix(uri, "https://"); ok {
		return rest, true
	}

	return strings.CutPrefix(uri, "http://")
}

func lookupDialect(uri string) (dialect, bool) {
	key, ok := metaschemaKey(uri)
	if !ok {
		return dialect{}, false
	}

	d, ok := dialects[key]

	return d, ok
}

// isWellKnownMetaschema reports whether uri names an official metaschema
// that the bundler embeds from its own copy and the validator loads from its
// own copy. Such references are never fetched.
func isWellKnownMetaschema(uri string) bool {
	key, ok := metaschemaKey(uri)
	if !ok {
		return false
	}

	if _, ok := dialects[key]; ok {
		return true
	}

	_, ok = vocabularyMetaschemas[key]

	return ok
}

// span locates one object member: key is the offset of its name, start and
// end delimit its value.
type span struct {
	key, start, end int
}

// topLevel locates the members of a top-level JSON object by byte offset so
// that one member can be added, replaced or removed without re-encoding the
// others.
type topLevel struct {
	members  map[string]span
	open     int
	closing  int
	isObject bool
}

// scanTopLevel indexes the top-level members of data, which must already
// have passed jsonutil.Check (so member names are unique). A document whose
// top-level value is not an object yields isObject == false.
func scanTopLevel(data []byte) (topLevel, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return topLevel{}, fmt.Errorf("read top-level value: %w", err)
	}

	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return topLevel{}, nil
	}

	tl := topLevel{isObject: true, open: int(dec.InputOffset()) - 1, members: map[string]span{}}
	if tl.open < 0 || data[tl.open] != '{' {
		return topLevel{}, errors.New("cannot locate the top-level object")
	}

	for dec.More() {
		keyAt := int(dec.InputOffset())

		keyTok, err := dec.Token()
		if err != nil {
			return topLevel{}, fmt.Errorf("read member name: %w", err)
		}

		key, ok := keyTok.(string)
		if !ok {
			return topLevel{}, fmt.Errorf("unexpected token %v in object", keyTok)
		}

		quote := bytes.IndexByte(data[keyAt:], '"')
		if quote < 0 {
			return topLevel{}, fmt.Errorf("cannot locate member name %q", key)
		}

		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return topLevel{}, fmt.Errorf("read member %q: %w", key, err)
		}

		end := int(dec.InputOffset())
		start := end - len(raw)

		if start < 0 || !bytes.Equal(data[start:end], raw) {
			return topLevel{}, fmt.Errorf("cannot locate member %q", key)
		}

		tl.members[key] = span{key: keyAt + quote, start: start, end: end}
	}

	if _, err := dec.Token(); err != nil {
		return topLevel{}, fmt.Errorf("read object end: %w", err)
	}

	tl.closing = int(dec.InputOffset()) - 1
	if data[tl.closing] != '}' {
		return topLevel{}, errors.New("cannot locate the end of the top-level object")
	}

	return tl, nil
}

// stringMember returns the string value of a top-level member. present is
// false when the member is absent; a present non-string value is an error.
func (tl topLevel) stringMember(data []byte, key string) (value string, present bool, err error) {
	s, ok := tl.members[key]
	if !ok {
		return "", false, nil
	}

	if err := json.Unmarshal(data[s.start:s.end], &value); err != nil {
		return "", true, fmt.Errorf("top-level %q is not a string", key)
	}

	return value, true, nil
}

// setStringMember returns a copy of data in which the top-level member key
// has the string value. An existing member has only its value bytes
// replaced; a missing member is inserted as the first member. Every other
// byte, including number literals, escapes and whitespace, is preserved.
func setStringMember(data []byte, tl topLevel, key, value string) ([]byte, error) {
	if !tl.isObject {
		return nil, errors.New("top-level value is not an object")
	}

	encoded, err := encodeString(value)
	if err != nil {
		return nil, err
	}

	if s, ok := tl.members[key]; ok {
		return replaceValue(data, s, encoded), nil
	}

	name, err := encodeString(key)
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, len(data)+len(name)+len(encoded)+2)
	out = append(out, data[:tl.open+1]...)
	out = append(out, name...)
	out = append(out, ':')
	out = append(out, encoded...)

	if len(tl.members) > 0 {
		out = append(out, ',')
	}

	return append(out, data[tl.open+1:]...), nil
}

func replaceValue(data []byte, s span, value []byte) []byte {
	out := make([]byte, 0, len(data)-(s.end-s.start)+len(value))
	out = append(out, data[:s.start]...)
	out = append(out, value...)

	return append(out, data[s.end:]...)
}

// withoutMembers returns the object in data without the named members. The
// kept members are copied verbatim and joined without whitespace.
func withoutMembers(data []byte, tl topLevel, drop map[string]struct{}) []byte {
	kept := make([]span, 0, len(tl.members))

	for key, s := range tl.members {
		if _, ok := drop[key]; !ok {
			kept = append(kept, s)
		}
	}

	slices.SortFunc(kept, func(a, b span) int { return a.key - b.key })

	out := make([]byte, 0, len(data))
	out = append(out, data[:tl.open+1]...)

	for i, s := range kept {
		if i > 0 {
			out = append(out, ',')
		}

		out = append(out, data[s.key:s.end]...)
	}

	return append(out, data[tl.closing:]...)
}

func encodeString(s string) ([]byte, error) {
	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	if err := enc.Encode(s); err != nil {
		return nil, fmt.Errorf("encode %q: %w", s, err)
	}

	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

var referenceKeywords = map[string]struct{}{"$ref": {}, "$dynamicRef": {}, "$recursiveRef": {}}

// hasNonFragmentRef reports whether any member named $ref, $dynamicRef or
// $recursiveRef anywhere in data has a string value that is neither a
// same-document fragment nor a URI in an official metaschema. It is a purely
// syntactic, conservative check used where the dialect, and therefore the
// real keyword locations, is unknown.
func hasNonFragmentRef(data []byte) (bool, error) {
	_, found, err := findMemberString(data, referenceKeywords, func(value string) bool {
		return !isSelfContainedRef(value)
	})

	return found, err
}

// isSelfContainedRef reports whether a reference resolves without the
// document's base URI and without bundling.
func isSelfContainedRef(value string) bool {
	document, _, _ := strings.Cut(value, "#")

	return strings.HasPrefix(value, "#") || isWellKnownMetaschema(document)
}

var metaschemaKeywords = map[string]struct{}{"$schema": {}}

// localMetaschema returns the first $schema value anywhere in data that uses
// the file scheme. The CLI loads such a metaschema from the local disk while
// it analyses the document, before any other check: file:///dev/zero grows
// its memory without bound and other files crash it. The scan is syntactic,
// so it also covers $schema members in subschemas of every dialect.
func localMetaschema(data []byte) (string, bool, error) {
	return findMemberString(data, metaschemaKeywords, isFileURI)
}

func isFileURI(value string) bool {
	trimmed := strings.TrimLeftFunc(value, func(r rune) bool { return r <= ' ' })
	scheme, _, ok := strings.Cut(trimmed, ":")

	return ok && strings.EqualFold(scheme, "file")
}

// findMemberString returns the first string value of a member named in keys,
// at any depth, for which match reports true.
func findMemberString(data []byte, keys map[string]struct{}, match func(string) bool) (string, bool, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	type frame struct {
		object  bool
		wantKey bool
	}

	var (
		stack   []frame
		pending bool
	)

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return "", false, nil
		}

		if err != nil {
			return "", false, fmt.Errorf("scan members: %w", err)
		}

		top := len(stack) - 1

		if key, ok := tok.(string); ok && top >= 0 && stack[top].object && stack[top].wantKey {
			stack[top].wantKey = false
			_, pending = keys[key]

			continue
		}

		if delim, ok := tok.(json.Delim); ok && (delim == '}' || delim == ']') {
			stack = stack[:top]
			pending = false

			continue
		}

		if top >= 0 && stack[top].object {
			stack[top].wantKey = true
		}

		if value, ok := tok.(string); ok && pending && match(value) {
			return value, true, nil
		}

		pending = false

		if delim, ok := tok.(json.Delim); ok {
			stack = append(stack, frame{object: delim == '{', wantKey: delim == '{'})
		}
	}
}

// retrievalBase validates a URI a document is fetched from. It returns the
// URI without an empty trailing fragment, unchanged otherwise, together with
// its parsed form.
func retrievalBase(uri string) (string, *url.URL, error) {
	uri = strings.TrimSuffix(uri, "#")

	u, err := url.Parse(uri)
	if err != nil {
		return "", nil, fmt.Errorf("invalid URI %q: %w", uri, err)
	}

	if !u.IsAbs() {
		return "", nil, fmt.Errorf("URI %q is not absolute", uri)
	}

	if strings.EqualFold(u.Scheme, "file") {
		return "", nil, fmt.Errorf("URI %q points at the local file system", uri)
	}

	if u.Fragment != "" || strings.Contains(uri, "#") {
		return "", nil, fmt.Errorf("URI %q has a fragment", uri)
	}

	return uri, u, nil
}

// resolveID resolves a declared identifier against the retrieval URI and
// drops an empty fragment, which older dialects allow on identifiers.
func resolveID(base *url.URL, id string) (string, error) {
	ref, err := url.Parse(id)
	if err != nil {
		return "", fmt.Errorf("invalid identifier %q: %w", id, err)
	}

	resolved := base.ResolveReference(ref)
	if resolved.Fragment != "" {
		return "", fmt.Errorf("identifier %q has a non-empty fragment", id)
	}

	return strings.TrimSuffix(resolved.String(), "#"), nil
}

// canonicalURI applies the RFC 3986 normalization the bundler CLI applies to
// every identifier and reference base it reports: lowercase scheme and host,
// no empty or default port, percent-encoded unreserved characters decoded and
// the remaining escapes in upper case, dot segments removed, and no
// fragment. URIs the CLI reported must only be compared in this form.
func canonicalURI(raw string) string {
	if !validEscapes(raw) {
		return raw
	}

	// Decoding unreserved escapes first also covers the host, where url.Parse
	// rejects escaped ASCII such as "ex%41mple.com" that the CLI accepts.
	u, err := url.Parse(normalizeEscapes(raw))
	if err != nil || u.Scheme == "" {
		return raw
	}

	scheme := strings.ToLower(u.Scheme)

	var b strings.Builder

	b.WriteString(scheme)
	b.WriteByte(':')

	if u.Opaque != "" {
		b.WriteString(normalizeEscapes(u.Opaque))
	} else {
		authority := canonicalAuthority(u, scheme)
		if authority != "" {
			b.WriteString("//")
			b.WriteString(authority)
		}

		path := removeDotSegments(normalizeEscapes(u.EscapedPath()))
		if authority == "" && strings.HasPrefix(path, "//") {
			// Without an authority a path starting with "//" would read as
			// one; the WHATWG URL standard keeps it a path the same way.
			path = "/." + path
		}

		b.WriteString(path)
	}

	if u.ForceQuery || u.RawQuery != "" {
		b.WriteByte('?')
		b.WriteString(normalizeEscapes(u.RawQuery))
	}

	return b.String()
}

var defaultPorts = map[string]string{"http": "80", "https": "443"}

func canonicalAuthority(u *url.URL, scheme string) string {
	var b strings.Builder

	if u.User != nil {
		b.WriteString(u.User.String())
		b.WriteByte('@')
	}

	host := strings.ToLower(normalizeEscapes(u.Hostname()))
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}

	b.WriteString(host)

	if port := u.Port(); port != "" && port != defaultPorts[scheme] {
		b.WriteByte(':')
		b.WriteString(port)
	}

	return b.String()
}

func normalizeEscapes(s string) string {
	const hex = "0123456789ABCDEF"

	if !strings.Contains(s, "%") {
		return s
	}

	var b strings.Builder

	for i := 0; i < len(s); i++ {
		if s[i] != '%' || i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
			b.WriteByte(s[i])

			continue
		}

		c := unhex(s[i+1])<<4 | unhex(s[i+2])
		if isUnreserved(c) {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		}

		i += 2
	}

	return b.String()
}

func validEscapes(s string) bool {
	for i := range len(s) {
		if s[i] == '%' && (i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2])) {
			return false
		}
	}

	return true
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c <= 'F':
		return c - 'A' + 10
	default:
		return c - 'a' + 10
	}
}

func isUnreserved(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}

// removeDotSegments is the algorithm of RFC 3986 section 5.2.4; each element
// of out is one output segment together with its leading slash.
func removeDotSegments(in string) string {
	var out []string

	pop := func() {
		if len(out) > 0 {
			out = out[:len(out)-1]
		}
	}

	for in != "" {
		switch {
		case strings.HasPrefix(in, "../"):
			in = in[3:]
		case strings.HasPrefix(in, "./"):
			in = in[2:]
		case strings.HasPrefix(in, "/./"):
			in = in[2:]
		case in == "/.":
			in = "/"
		case strings.HasPrefix(in, "/../"):
			in = in[3:]
			pop()
		case in == "/..":
			in = "/"
			pop()
		case in == "." || in == "..":
			in = ""
		default:
			end := strings.IndexByte(in[1:], '/') + 1
			if end == 0 {
				end = len(in)
			}

			out = append(out, in[:end])
			in = in[end:]
		}
	}

	return strings.Join(out, "")
}
