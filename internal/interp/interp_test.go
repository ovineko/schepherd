package interp

import (
	"errors"
	"slices"
	"testing"
)

func lookupFrom(m map[string]string) LookupFunc {
	return func(name string) (string, bool) {
		v, ok := m[name]

		return v, ok
	}
}

func TestExpand(t *testing.T) {
	env := lookupFrom(map[string]string{"BIN": "/opt/v", "TRICKY": "{schema} ${BIN} $${X}", "EMPTY": ""})
	values := map[string]string{Schema: "/c/schema.json", SchemaID: "pkg", Workspace: "/ws", File: "/ws/a b.json"}

	cases := map[string]string{
		"${BIN}":                  "/opt/v",
		"{schema}":                "/c/schema.json",
		"--schema={schema}":       "--schema=/c/schema.json",
		"${TRICKY}":               "{schema} ${BIN} $${X}",
		"$${BIN}":                 "${BIN}",
		"{{schema}}":              "{schema}",
		"{{{schema}}}":            "{/c/schema.json}",
		"a}}b{{c":                 "a}b{c",
		"$BIN":                    "$BIN",
		"$$":                      "$$",
		"${EMPTY}x":               "x",
		"{file}":                  "/ws/a b.json",
		"$(rm -rf /) `id` ~ *":    "$(rm -rf /) `id` ~ *",
		"{workspace}/{schema-id}": "/ws/pkg",
	}

	for raw, want := range cases {
		tpl, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}

		got, err := tpl.Expand(env, values)
		if err != nil {
			t.Fatalf("Expand(%q): %v", raw, err)
		}

		if got != want {
			t.Errorf("Expand(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, raw := range []string{"{", "}", "a}b", "{unknown}", "{files}", "${", "${}", "${1A}", "${A-B}", "{schema", "x{ schema }"} {
		if _, err := Parse(raw); !errors.Is(err, ErrSyntax) {
			t.Errorf("Parse(%q) = %v, want ErrSyntax", raw, err)
		}
	}
}

func TestUnsetEnvIsError(t *testing.T) {
	tpl, err := Parse("${MISSING}")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := tpl.Expand(lookupFrom(nil), nil); !errors.Is(err, ErrUnsetEnv) {
		t.Errorf("Expand = %v, want ErrUnsetEnv", err)
	}
}

// expandList expands a template the way the runner expands one argument: a
// whole {files...} becomes the files, anything else one argument.
func expandList(t *Template, lookup LookupFunc, values map[string]string, files []string) ([]string, error) {
	if t.IsFilesList() {
		return append([]string(nil), files...), nil
	}

	s, err := t.Expand(lookup, values)
	if err != nil {
		return nil, err
	}

	return []string{s}, nil
}

func TestFilesList(t *testing.T) {
	whole, err := Parse("{files...}")
	if err != nil {
		t.Fatal(err)
	}

	args, err := expandList(whole, lookupFrom(nil), nil, []string{"/a", "/b c", "--help"})
	if err != nil || !slices.Equal(args, []string{"/a", "/b c", "--help"}) {
		t.Errorf("expandList = %v, %v", args, err)
	}

	embedded, err := Parse("--files={files...}")
	if err != nil {
		t.Fatal(err)
	}

	if embedded.IsFilesList() {
		t.Error("embedded {files...} reported as a whole argument")
	}

	if _, err := expandList(embedded, lookupFrom(nil), nil, []string{"/a"}); err == nil {
		t.Error("embedded {files...} expanded")
	}
}

func TestRestrictAndIntrospection(t *testing.T) {
	tpl, err := Parse("${A}{schema}${B}{file}")
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(tpl.EnvVars(), []string{"A", "B"}) || !slices.Equal(tpl.Placeholders(), []string{Schema, File}) {
		t.Errorf("introspection = %v %v", tpl.EnvVars(), tpl.Placeholders())
	}

	if err := tpl.Restrict("runner.cwd", Workspace, Cache); err == nil {
		t.Error("Restrict accepted {schema} in cwd")
	}

	if err := tpl.Restrict("runner.args", Schema, File); err != nil {
		t.Errorf("Restrict rejected allowed placeholders: %v", err)
	}
}

func FuzzParseExpand(f *testing.F) {
	for _, seed := range []string{"${A}", "{schema}", "$${", "{{", "}}", "{files...}", "a{b}c"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		tpl, err := Parse(raw)
		if err != nil {
			return
		}

		values := map[string]string{Schema: "{schema}", SchemaID: "${A}", SchemaRef: "r", File: "f", Workspace: "w", Cache: "c"}

		out, err := tpl.Expand(func(string) (string, bool) { return "{file}", true }, values)
		if err != nil {
			return
		}

		again, err := tpl.Expand(func(string) (string, bool) { return "{file}", true }, values)
		if err != nil || again != out {
			t.Fatalf("expansion is not deterministic for %q", raw)
		}
	})
}
