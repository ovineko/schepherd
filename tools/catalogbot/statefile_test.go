package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/pelletier/go-toml/v2"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

// The bot commits catalog/state.json to main, and the CI of main runs
// `pnpm dm check` followed by `git diff --exit-code`. The tests in this
// file pin what keeps that green for every state the publisher writes: the
// repository's formatters and typos never touch the file, and the canonical
// form the publisher writes (which state.Parse insists on) already meets the
// .editorconfig rules that editorconfig-checker enforces on every file.

func repositoryFile(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatal(err)
	}

	return data
}

// disabledFor returns the tools the rules of a .datamitsuignore file at the
// repository root disable for path: later rules override earlier ones, and a
// "!" rule enables tools again.
func disabledFor(rules, path string) []string {
	var disabled []string

	for line := range strings.Lines(rules) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		glob, tools, ok := strings.Cut(line, ":")
		negate := strings.HasPrefix(glob, "!")

		if match, err := doublestar.Match(strings.TrimPrefix(glob, "!"), path); !ok || err != nil || !match {
			continue
		}

		for tool := range strings.SplitSeq(tools, ",") {
			tool = strings.TrimSpace(tool)

			switch {
			case negate && tool == "*":
				disabled = nil
			case negate:
				disabled = slices.DeleteFunc(disabled, func(d string) bool { return d == tool })
			default:
				disabled = append(disabled, tool)
			}
		}
	}

	return disabled
}

func TestFormattersLeaveTheStateFileAlone(t *testing.T) {
	// yq-json sort_keys, prettier, eslint and oxfmt would each rewrite the
	// file into a form state.Parse refuses.
	if disabled := disabledFor(string(repositoryFile(t, ".datamitsuignore")), stateFile); !slices.Contains(disabled, "*") {
		t.Errorf(".datamitsuignore disables only %v for %s, want every tool (*)", disabled, stateFile)
	}

	// typos scans the whole repository by itself, past .datamitsuignore, and
	// would report upstream product names as misspellings.
	var typos struct {
		Files struct {
			ExtendExclude []string `toml:"extend-exclude"`
		} `toml:"files"`
	}
	if err := toml.Unmarshal(repositoryFile(t, ".typos.toml"), &typos); err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(typos.Files.ExtendExclude, stateFile) {
		t.Errorf(".typos.toml excludes %v, not %s", typos.Files.ExtendExclude, stateFile)
	}

	// The rules of this test's own parser, as datamitsu documents them.
	for rules, want := range map[string]bool{
		"catalog/state.json: *\n":                          true,
		"catalog/**/*: *\n":                                true,
		"# catalog/state.json: *\n":                        false,
		"catalog/state.json: prettier\n":                   false,
		"catalog/state.json: *\n!catalog/state.json: *\n":  false,
		"catalog/state.json: *\n!**/*.json: prettier\n":    true,
		"catalog/other.json: *\n":                          false,
		"**/*: cspell\n\ncatalog/state.json: yq-json, *\n": true,
	} {
		if got := slices.Contains(disabledFor(rules, stateFile), "*"); got != want {
			t.Errorf("rules %q disable every tool: %v, want %v", rules, got, want)
		}
	}
}

// TestTheStateFileMeetsTheEditorconfig encodes a state whose upstream text
// holds line breaks, tabs, trailing spaces, HTML and non-ASCII characters,
// and whose members are not in sorted order, and checks the properties
// editorconfig-checker enforces: UTF-8, LF line ends, a final newline, no
// trailing whitespace and two-space indentation. Sorting its keys, as
// `yq sort_keys` does, yields a file the publisher refuses.
func TestTheStateFileMeetsTheEditorconfig(t *testing.T) {
	_, st := writeState(t, t.TempDir())

	st.Schemas[0].Entry.Name = "Upstream   config é中"
	st.Schemas[0].Entry.Description = "Line one  \n\tindented line\u2028<b>bold</b> &   trailing\t \n"
	st.Schemas[0].Entry.FileMatch = []string{"a.json", "upstream.json"}

	data, err := state.Encode(st)
	if err != nil {
		t.Fatal(err)
	}

	if !utf8.Valid(data) || bytes.ContainsAny(data, "\r\t") || !bytes.HasSuffix(data, []byte("}\n")) || bytes.HasSuffix(data, []byte("\n\n")) {
		t.Errorf("the state is not UTF-8 with LF line ends and one final newline:\n%s", data)
	}

	for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		indent := len(line) - len(strings.TrimLeft(line, " "))

		if strings.TrimRight(line, " \t") != line || indent%2 != 0 || strings.TrimSpace(line) == "" {
			t.Errorf("line %d %q has trailing whitespace, an odd indentation or nothing else", i+1, line)
		}
	}

	if _, err := state.Parse(data); err != nil {
		t.Fatalf("the encoded state does not parse: %v", err)
	}

	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}

	sorted, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	sorted = append(sorted, '\n')

	if bytes.Equal(sorted, data) {
		t.Fatal("the canonical state has sorted keys; a formatter that sorts them would not show why it is exempt")
	}

	if _, err := state.Parse(sorted); fault.KindOf(err) != fault.Integrity {
		t.Errorf("the state with sorted keys parsed: %v", err)
	}
}
