package ids

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
)

func TestValid(t *testing.T) {
	valid := []string{"a", "0", "package", "a.b", "a-b_c", strings.Repeat("a", 128)}
	invalid := []string{"", "-a", "a-", ".a", "a.", "A", "a..b", "a/b", "a b", strings.Repeat("a", 129), "ä"}

	for _, id := range valid {
		if !Valid(id) {
			t.Errorf("Valid(%q) = false", id)
		}
	}

	for _, id := range invalid {
		if Valid(id) {
			t.Errorf("Valid(%q) = true", id)
		}
	}
}

func TestDerive(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{url: "https://www.schemastore.org/tsconfig.json", want: "tsconfig"},
		{url: "https://json.schemastore.org/package", want: "package"},
		{url: "https://example.com/a/b/Config.Schema.JSON", want: "config"},
		{url: "https://docs.renovatebot.com/renovate-schema.json", want: "renovate-schema"},
		{url: "https://example.com/dir/", want: "dir"},
		{url: "https://meta.open-rpc.org/", want: "meta.open-rpc.org"},
		{url: "https://example.com/My%20Schema%20(v2).json", want: "my-schema-v2"},
		{url: "https://example.com/weird..name...json", want: "weird.name"},
		{url: "https://example.com/.hidden.json", want: "hidden"},
		{url: "https://example.com/--x--.json", want: "x"},
		{url: "https://example.com/über.json", want: "ber"},
		{url: "https://example.com/crowdsec.yaml", want: "crowdsec.yaml"},
		{url: "https://appsemble.app/api.json#/components/schemas/AppDefinition", want: "api-appdefinition"},
		{url: "https://raw.githubusercontent.com/ansible/ansible-lint/main/src/ansiblelint/schemas/ansible.json#/$defs/tasks", want: "ansible-tasks"},
		{url: "https://www.schemastore.org/a.json#/definitions/name", want: "a-name"},
		{url: "https://example.com/a.json#anchor_1", want: "a-anchor_1"},
		{url: "https://example.com/a.json#/definitions/My%20Item", want: "a-my-item"},
		{url: "https://example.com/a.json#", want: "a"},
		{url: "https://example.com/a.json#/", want: "a"},
		{url: "https://example.com/" + strings.Repeat("y", 200) + ".json#/" + strings.Repeat("z", 200), want: strings.Repeat("y", 63) + "-" + strings.Repeat("z", 64)},
		{url: "https://example.com/.json", want: "example.com"},
		{url: "https://example.com/" + strings.Repeat("x", 200) + ".json", want: strings.Repeat("x", 128)},
		{url: "urn:example:thing", want: "example-thing"},
		{url: "not a url/at all.json", want: "at-all"},
		{url: "", want: "schema"},
		{url: "https://@@@/", want: "schema"},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got := Derive(tt.url)
			if got != tt.want {
				t.Fatalf("Derive(%q) = %q, want %q", tt.url, got, tt.want)
			}

			if !Valid(got) {
				t.Fatalf("Derive(%q) = %q is not valid", tt.url, got)
			}
		})
	}
}

func TestAssignPrecedence(t *testing.T) {
	sources := []string{
		"https://a.example/schema.json",
		"https://b.example/tool.json",
		"https://c.example/tool.json",
		"https://d.example/renamed.json",
	}

	overrides := map[string]string{
		"https://d.example/renamed.json": "custom",
		"https://absent.example/x.json":  "unused",
	}

	previous := map[string]string{
		"https://c.example/tool.json":    "tool",
		"https://d.example/renamed.json": "old-name",
	}

	got, collisions, err := Assign(sources, overrides, previous)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"https://a.example/schema.json":  "schema",
		"https://b.example/tool.json":    "tool-b-example",
		"https://c.example/tool.json":    "tool",
		"https://d.example/renamed.json": "custom",
	}

	if !maps.Equal(got, want) {
		t.Fatalf("Assign = %v, want %v", got, want)
	}

	wantCollisions := []Collision{{
		ID:         "tool",
		Sources:    []string{"https://b.example/tool.json", "https://c.example/tool.json"},
		Resolution: "https://c.example/tool.json keeps tool (published state); https://b.example/tool.json gets tool-b-example",
	}}

	if !reflect.DeepEqual(collisions, wantCollisions) {
		t.Fatalf("collisions = %#v", collisions)
	}
}

func TestAssignNewSourcesSuffixes(t *testing.T) {
	sources := []string{
		"https://x.example/b/schema.json",
		"https://x.example/a/schema.json",
		"https://y.example/schema.json",
		"https://x.example/c/schema.json",
		"https://www.z.example/schema-2.json",
	}

	got, collisions, err := Assign(sources, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"https://x.example/a/schema.json":     "schema",
		"https://x.example/b/schema.json":     "schema-x-example",
		"https://x.example/c/schema.json":     "schema-3",
		"https://y.example/schema.json":       "schema-y-example",
		"https://www.z.example/schema-2.json": "schema-2",
	}

	if !maps.Equal(got, want) {
		t.Fatalf("Assign = %v, want %v", got, want)
	}

	if len(collisions) != 1 || collisions[0].ID != "schema" || len(collisions[0].Sources) != 4 {
		t.Fatalf("collisions = %#v", collisions)
	}

	wantResolution := "https://x.example/a/schema.json keeps schema (derived, first in sorted order); " +
		"https://x.example/b/schema.json gets schema-x-example; " +
		"https://x.example/c/schema.json gets schema-3; " +
		"https://y.example/schema.json gets schema-y-example"
	if collisions[0].Resolution != wantResolution {
		t.Fatalf("resolution = %q", collisions[0].Resolution)
	}
}

func TestAssignOverrideBeatsPrevious(t *testing.T) {
	sources := []string{"https://a.example/x.json", "https://b.example/y.json"}
	overrides := map[string]string{"https://b.example/y.json": "x"}
	previous := map[string]string{"https://a.example/x.json": "x"}

	got, collisions, err := Assign(sources, overrides, previous)
	if err != nil {
		t.Fatal(err)
	}

	if got["https://b.example/y.json"] != "x" || got["https://a.example/x.json"] != "x-a-example" {
		t.Fatalf("Assign = %v", got)
	}

	if len(collisions) != 1 || collisions[0].ID != "x" {
		t.Fatalf("collisions = %#v", collisions)
	}

	if want := "https://b.example/y.json keeps x (override); https://a.example/x.json gets x-a-example"; collisions[0].Resolution != want {
		t.Fatalf("resolution = %q", collisions[0].Resolution)
	}
}

func TestAssignDuplicatePreviousIDs(t *testing.T) {
	sources := []string{"https://b.example/n.json", "https://a.example/m.json"}
	previous := map[string]string{"https://a.example/m.json": "same", "https://b.example/n.json": "same"}

	got, collisions, err := Assign(sources, nil, previous)
	if err != nil {
		t.Fatal(err)
	}

	if got["https://a.example/m.json"] != "same" || got["https://b.example/n.json"] != "n" {
		t.Fatalf("Assign = %v", got)
	}

	if len(collisions) != 1 || collisions[0].ID != "same" {
		t.Fatalf("collisions = %#v", collisions)
	}
}

func TestAssignIsOrderIndependent(t *testing.T) {
	sources := []string{
		"https://a.example/schema.json", "https://b.example/schema.json", "https://c.example/schema.json",
		"https://a.example/x/schema.json", "https://d.example/tool.json", "https://e.example/tool.json",
		"https://a.example/schema.json",
	}
	previous := map[string]string{"https://e.example/tool.json": "tool"}

	first, firstCollisions, err := Assign(sources, nil, previous)
	if err != nil {
		t.Fatal(err)
	}

	for i := range 20 {
		shuffled := slices.Clone(sources)
		for j := range shuffled {
			k := (j*7 + i*3) % len(shuffled)
			shuffled[j], shuffled[k] = shuffled[k], shuffled[j]
		}

		got, collisions, err := Assign(shuffled, nil, maps.Clone(previous))
		if err != nil {
			t.Fatal(err)
		}

		if !maps.Equal(got, first) || !reflect.DeepEqual(collisions, firstCollisions) {
			t.Fatalf("order %v changed the result: %v vs %v", shuffled, got, first)
		}
	}

	if len(first) != 6 {
		t.Fatalf("duplicates not collapsed: %v", first)
	}

	seen := map[string]bool{}
	for _, id := range first {
		if seen[id] || !Valid(id) {
			t.Fatalf("duplicate or invalid id %q in %v", id, first)
		}

		seen[id] = true
	}
}

func TestAssignLongIDSuffix(t *testing.T) {
	long := strings.Repeat("a", 128)
	sources := []string{"https://a.example/" + long + ".json", "https://b.example/" + long + ".json"}

	got, _, err := Assign(sources, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	second := got["https://b.example/"+long+".json"]
	if !Valid(second) || !strings.HasSuffix(second, "-b-example") || len(second) != MaxLength {
		t.Fatalf("suffixed id = %q", second)
	}
}

func TestAssignErrors(t *testing.T) {
	tests := []struct {
		overrides map[string]string
		previous  map[string]string
		name      string
		sources   []string
	}{
		{
			name:      "two overrides to one id",
			sources:   []string{"https://a.example/a.json"},
			overrides: map[string]string{"https://a.example/a.json": "x", "https://b.example/b.json": "x"},
		},
		{
			name:      "invalid override",
			sources:   []string{"https://a.example/a.json"},
			overrides: map[string]string{"https://a.example/a.json": "Bad ID"},
		},
		{
			name:     "invalid previous",
			sources:  []string{"https://a.example/a.json"},
			previous: map[string]string{"https://a.example/a.json": "a..b"},
		},
		{
			name:    "empty source",
			sources: []string{"", "https://a.example/a.json"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Assign(tt.sources, tt.overrides, tt.previous)
			if err == nil || fault.KindOf(err) != fault.Usage {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLoadOverrides(t *testing.T) {
	dir := t.TempDir()

	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		return path
	}

	got, err := LoadOverrides(write("ok.json", `{"https://example.com/a.json":"alpha","http://example.org/b":"beta"}`))
	if err != nil {
		t.Fatal(err)
	}

	if !maps.Equal(got, map[string]string{"https://example.com/a.json": "alpha", "http://example.org/b": "beta"}) {
		t.Fatalf("overrides = %v", got)
	}

	empty, err := LoadOverrides(write("empty.json", "{}\n"))
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty = %v, %v", empty, err)
	}

	bad := map[string]string{
		"dup.json":      `{"https://e.com/a":"a","https://e.com/a":"b"}`,
		"sameid.json":   `{"https://e.com/a":"x","https://e.com/b":"x"}`,
		"badid.json":    `{"https://e.com/a":"X"}`,
		"relative.json": `{"a.json":"a"}`,
		"creds.json":    `{"https://u:p@e.com/a":"a"}`,
		"array.json":    `["a"]`,
		"null.json":     `null`,
		"number.json":   `{"https://e.com/a":1}`,
		"trailing.json": `{} {}`,
	}

	for name, content := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := LoadOverrides(write(name, content))
			if err == nil || fault.KindOf(err) != fault.Usage {
				t.Fatalf("error = %v", err)
			}
		})
	}

	_, err = LoadOverrides(filepath.Join(dir, "missing.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file error = %v", err)
	}
}

func FuzzDerive(f *testing.F) {
	f.Add("https://www.schemastore.org/tsconfig.json")
	f.Add("https://example.com/a..b.schema.json")
	f.Add("")
	f.Add("%%%")

	f.Fuzz(func(t *testing.T, source string) {
		if id := Derive(source); !Valid(id) {
			t.Fatalf("Derive(%q) = %q is invalid", source, id)
		}
	})
}

func FuzzAssign(f *testing.F) {
	f.Add("https://a.example/schema.json", "https://b.example/schema.json", "https://c.example/x.json", "schema")
	f.Add("https://a.example/x", "https://a.example/x/", "https://a.example/y", "x")

	f.Fuzz(func(t *testing.T, a, b, c, prev string) {
		sources := []string{a, b, c}
		if slices.Contains(sources, "") || !Valid(prev) {
			return
		}

		previous := map[string]string{c: prev}

		got, _, err := Assign(sources, nil, previous)
		if err != nil {
			t.Fatal(err)
		}

		seen := map[string]bool{}
		for _, id := range got {
			if !Valid(id) || seen[id] {
				t.Fatalf("invalid or duplicate id %q in %v", id, got)
			}

			seen[id] = true
		}

		if got[c] != prev {
			t.Fatalf("previous id %q not kept: %v", prev, got)
		}

		reversed, _, err := Assign([]string{c, b, a}, nil, previous)
		if err != nil || !maps.Equal(got, reversed) {
			t.Fatalf("order-dependent result: %v vs %v", got, reversed)
		}
	})
}
