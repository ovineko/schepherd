// Package interp parses and expands Schepherd templates.
//
// Two namespaces exist: ${NAME} reads Schepherd's own environment and
// {placeholder} reads runtime values such as {schema} or {file}. Expansion is
// a single pass over the parsed template, so substituted values are never
// scanned again. Escapes: "$${NAME}" yields the literal "${NAME}", "{{"
// yields "{" and "}}" yields "}". A lone "{" or "}" is an error.
package interp

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Placeholder names.
const (
	Schema    = "schema"
	SchemaID  = "schema-id"
	SchemaRef = "schema-ref"
	File      = "file"
	Files     = "files..."
	Workspace = "workspace"
	Cache     = "cache"
)

var known = map[string]struct{}{
	Schema: {}, SchemaID: {}, SchemaRef: {}, File: {}, Files: {}, Workspace: {}, Cache: {},
}

// ErrSyntax reports a malformed template.
var ErrSyntax = errors.New("template syntax error")

// ErrUnsetEnv reports a ${NAME} reference to an unset variable.
var ErrUnsetEnv = errors.New("environment variable is not set")

type kind int

const (
	literal kind = iota
	envVar
	placeholder
)

type token struct {
	text string
	kind kind
}

// Template is a parsed template string.
type Template struct {
	raw    string
	tokens []token
}

// Parse parses raw.
func Parse(raw string) (*Template, error) {
	t := &Template{raw: raw}

	var lit strings.Builder

	flush := func() {
		if lit.Len() > 0 {
			t.tokens = append(t.tokens, token{kind: literal, text: lit.String()})
			lit.Reset()
		}
	}

	for i := 0; i < len(raw); {
		switch {
		case strings.HasPrefix(raw[i:], "$${"):
			end := strings.IndexByte(raw[i+3:], '}')
			if end < 0 {
				lit.WriteString("${")
				i += 3

				continue
			}

			lit.WriteString(raw[i+1 : i+4+end])
			i += 4 + end
		case strings.HasPrefix(raw[i:], "${"):
			end := strings.IndexByte(raw[i+2:], '}')
			if end < 0 {
				return nil, fmt.Errorf("%w in %q: unterminated ${", ErrSyntax, raw)
			}

			name := raw[i+2 : i+2+end]
			if !validEnvName(name) {
				return nil, fmt.Errorf("%w in %q: invalid variable name %q", ErrSyntax, raw, name)
			}

			flush()
			t.tokens = append(t.tokens, token{kind: envVar, text: name})
			i += 3 + end
		case strings.HasPrefix(raw[i:], "{{"):
			lit.WriteByte('{')
			i += 2
		case strings.HasPrefix(raw[i:], "}}"):
			lit.WriteByte('}')
			i += 2
		case raw[i] == '{':
			end := strings.IndexByte(raw[i+1:], '}')
			if end < 0 {
				return nil, fmt.Errorf("%w in %q: unterminated { (write {{ for a literal brace)", ErrSyntax, raw)
			}

			name := raw[i+1 : i+1+end]
			if _, ok := known[name]; !ok {
				return nil, fmt.Errorf("%w in %q: unknown placeholder {%s}", ErrSyntax, raw, name)
			}

			flush()
			t.tokens = append(t.tokens, token{kind: placeholder, text: name})
			i += 2 + end
		case raw[i] == '}':
			return nil, fmt.Errorf("%w in %q: lone } (write }} for a literal brace)", ErrSyntax, raw)
		default:
			lit.WriteByte(raw[i])
			i++
		}
	}

	flush()

	return t, nil
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}

	for i, r := range name {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}

	return true
}

// Placeholders returns the placeholder names used, in order of appearance.
func (t *Template) Placeholders() []string {
	var names []string

	for _, tok := range t.tokens {
		if tok.kind == placeholder {
			names = append(names, tok.text)
		}
	}

	return names
}

// EnvVars returns the environment variable names used, in order of appearance.
func (t *Template) EnvVars() []string {
	var names []string

	for _, tok := range t.tokens {
		if tok.kind == envVar {
			names = append(names, tok.text)
		}
	}

	return names
}

// Uses reports whether the template contains the placeholder.
func (t *Template) Uses(name string) bool {
	for _, tok := range t.tokens {
		if tok.kind == placeholder && tok.text == name {
			return true
		}
	}

	return false
}

// IsFilesList reports whether the whole template is exactly {files...}.
func (t *Template) IsFilesList() bool {
	return len(t.tokens) == 1 && t.tokens[0].kind == placeholder && t.tokens[0].text == Files
}

// Restrict fails if the template uses a placeholder outside allowed.
func (t *Template) Restrict(field string, allowed ...string) error {
	for _, name := range t.Placeholders() {
		if !slices.Contains(allowed, name) {
			if len(allowed) == 0 {
				return fmt.Errorf("%s: placeholder {%s} is not allowed here (no placeholders are)", field, name)
			}

			return fmt.Errorf("%s: placeholder {%s} is not allowed here (allowed: {%s})", field, name, strings.Join(allowed, "}, {"))
		}
	}

	return nil
}

// LookupFunc resolves an environment variable.
type LookupFunc func(name string) (string, bool)

// Expand substitutes every token in one pass. A placeholder missing from
// values is an error, as is an unset environment variable. {files...} can
// only be expanded through ExpandList.
func (t *Template) Expand(lookup LookupFunc, values map[string]string) (string, error) {
	var out strings.Builder

	for _, tok := range t.tokens {
		switch tok.kind {
		case literal:
			out.WriteString(tok.text)
		case envVar:
			value, ok := lookup(tok.text)
			if !ok {
				return "", fmt.Errorf("%w: ${%s} in %q", ErrUnsetEnv, tok.text, t.raw)
			}

			out.WriteString(value)
		case placeholder:
			if tok.text == Files {
				return "", fmt.Errorf("{files...} must be a whole argument, not part of %q", t.raw)
			}

			value, ok := values[tok.text]
			if !ok {
				return "", fmt.Errorf("placeholder {%s} has no value here (in %q)", tok.text, t.raw)
			}

			out.WriteString(value)
		}
	}

	return out.String(), nil
}
