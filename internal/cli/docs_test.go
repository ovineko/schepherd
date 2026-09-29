package cli

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// docSection is a heading of docs/cli.md with the text up to the next one.
type docSection struct {
	heading string
	body    string
}

func cliReference(t *testing.T) []docSection {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "cli.md"))
	if err != nil {
		t.Fatal(err)
	}

	var (
		sections []docSection
		current  docSection
		fenced   bool
	)

	for line := range strings.SplitSeq(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		}

		if !fenced && strings.HasPrefix(line, "#") {
			sections = append(sections, current)
			current = docSection{heading: strings.TrimSpace(strings.TrimLeft(line, "#"))}
		}

		current.body += line + "\n"
	}

	return append(sections, current)
}

func commandTree() *cobra.Command {
	a := &app{stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard}
	a.root = a.newRoot()
	a.root.InitDefaultHelpCmd()

	return a.root
}

func visibleCommands(root *cobra.Command) []*cobra.Command {
	var all []*cobra.Command

	var walk func(*cobra.Command)

	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if sub.Hidden || sub.Deprecated != "" {
				continue
			}

			all = append(all, sub)
			walk(sub)
		}
	}

	walk(root)

	return all
}

func commandName(c *cobra.Command) string {
	return strings.TrimPrefix(c.CommandPath(), c.Root().Name()+" ")
}

func flagPattern(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[^\w-])--` + regexp.QuoteMeta(name) + `(?:[^\w-]|$)`)
}

// TestCommandReferenceCoversTheCommandTree fails when a command or a flag of
// the real command tree is missing from docs/cli.md: every command needs a
// section that names it in code format, and each of its flags must appear in
// one of those sections. Global flags belong in the "Global flags" section.
func TestCommandReferenceCoversTheCommandTree(t *testing.T) {
	sections := cliReference(t)
	root := commandTree()

	global := slices.IndexFunc(sections, func(s docSection) bool { return s.heading == "Global flags" })
	if global < 0 {
		t.Fatal(`docs/cli.md has no "Global flags" section`)
	}

	root.PersistentFlags().VisitAll(func(f *pflag.Flag) {
		if !f.Hidden && !flagPattern(f.Name).MatchString(sections[global].body) {
			t.Errorf("global flag --%s is missing from the Global flags section of docs/cli.md", f.Name)
		}
	})

	commands := visibleCommands(root)
	if len(commands) == 0 {
		t.Fatal("the command tree has no commands")
	}

	for _, c := range commands {
		name := commandName(c)
		mention := regexp.MustCompile("`" + regexp.QuoteMeta(name) + "[` \\[]")

		var documented []string

		for _, s := range sections {
			if mention.MatchString(s.body) {
				documented = append(documented, s.body)
			}
		}

		if len(documented) == 0 {
			t.Errorf("command %q is missing from docs/cli.md", name)

			continue
		}

		c.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
			if f.Hidden || f.Name == "help" {
				return
			}

			if !slices.ContainsFunc(documented, flagPattern(f.Name).MatchString) {
				t.Errorf("flag --%s of %q is missing from the docs/cli.md section that documents %q", f.Name, name, name)
			}
		})
	}
}

// TestCommandReferenceNamesOnlyRealCommandsAndFlags fails when docs/cli.md
// documents a flag no command has, or a command heading or table row that
// the command tree does not have.
func TestCommandReferenceNamesOnlyRealCommandsAndFlags(t *testing.T) {
	sections := cliReference(t)
	root := commandTree()

	flags := map[string]bool{"help": true}

	collect := func(f *pflag.Flag) { flags[f.Name] = true }
	root.PersistentFlags().VisitAll(collect)

	commands := map[string]bool{}

	for _, c := range visibleCommands(root) {
		commands[commandName(c)] = true
		c.LocalNonPersistentFlags().VisitAll(collect)
	}

	flagMention := regexp.MustCompile(`(?:^|[^\w-])--([a-z][a-z0-9-]*)`)
	// A command is named in code format at the start of a heading or of a
	// table's first cell: the words before its first argument or flag.
	commandMention := regexp.MustCompile("^(?:\\|\\s*)?`([a-z][a-z0-9-]*(?: [a-z][a-z0-9-]*)*)(?:[` \\[<]|$)")

	for _, s := range sections {
		for _, m := range flagMention.FindAllStringSubmatch(s.body, -1) {
			if !flags[m[1]] {
				t.Errorf("docs/cli.md section %q names --%s, which no command has", s.heading, m[1])
			}
		}

		candidates := []string{s.heading}

		for line := range strings.SplitSeq(s.body, "\n") {
			if strings.HasPrefix(line, "|") {
				candidates = append(candidates, line)
			}
		}

		for _, text := range candidates {
			m := commandMention.FindStringSubmatch(text)
			if m == nil {
				continue
			}

			if !commands[m[1]] {
				t.Errorf("docs/cli.md section %q documents command %q, which does not exist", s.heading, m[1])
			}
		}
	}
}
