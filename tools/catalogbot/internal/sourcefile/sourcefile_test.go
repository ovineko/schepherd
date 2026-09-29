package sourcefile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/tools/catalogbot/internal/sourcefile"
)

const newCommit = "0123456789abcdef0123456789abcdef01234567"

func TestSetCommitChangesOnlyTheCommitLine(t *testing.T) {
	in := "# Last imported commit.\nkind = \"upstream\"\ncommit = \"05b037b7a2b68ead6893c857db2594bcad9951d4\"\n" +
		"# commit = \"ffffffffffffffffffffffffffffffffffffffff\" is only a comment\n[dependencies]\nmax_depth = 8\n"

	got, err := sourcefile.SetCommit([]byte(in), newCommit)
	if err != nil {
		t.Fatal(err)
	}

	want := strings.Replace(in, "05b037b7a2b68ead6893c857db2594bcad9951d4", newCommit, 1)
	if string(got) != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}

	crlf := strings.ReplaceAll(in, "\n", "\r\n")

	got, err = sourcefile.SetCommit([]byte(crlf), newCommit)
	if err != nil || string(got) != strings.ReplaceAll(want, "\n", "\r\n") {
		t.Errorf("CRLF: %q, %v", got, err)
	}
}

func TestSetCommitRefusesWhatItCannotRewriteExactly(t *testing.T) {
	cases := map[string]struct{ data, commit string }{
		"short commit":     {"commit = \"05b037b7a2b68ead6893c857db2594bcad9951d4\"\n", "abc"},
		"uppercase commit": {"commit = \"05b037b7a2b68ead6893c857db2594bcad9951d4\"\n", strings.ToUpper(newCommit)},
		"no commit":        {"kind = \"upstream\"\n", newCommit},
		"other spelling":   {"commit=\"05b037b7a2b68ead6893c857db2594bcad9951d4\"\n", newCommit},
		"two commits":      {"commit = \"05b037b7a2b68ead6893c857db2594bcad9951d4\"\ncommit = \"05b037b7a2b68ead6893c857db2594bcad9951d4\"\n", newCommit},
		"inside a table":   {"[x]\ncommit = \"05b037b7a2b68ead6893c857db2594bcad9951d4\"\n", newCommit},
	}

	for name, tc := range cases {
		if _, err := sourcefile.SetCommit([]byte(tc.data), tc.commit); fault.KindOf(err) != fault.Usage {
			t.Errorf("%s: %v, want a usage error", name, err)
		}
	}
}

func TestRewriteTheRepositorySource(t *testing.T) {
	in := filepath.Join("..", "..", "..", "..", "sources", "schemastore.toml")
	out := filepath.Join(t.TempDir(), "schemastore.toml")

	if err := sourcefile.Rewrite(in, out, newCommit); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	beforeLines, afterLines := strings.Split(string(before), "\n"), strings.Split(string(after), "\n")
	if len(beforeLines) != len(afterLines) {
		t.Fatalf("line count %d -> %d", len(beforeLines), len(afterLines))
	}

	changed := 0

	for i := range beforeLines {
		if beforeLines[i] != afterLines[i] {
			changed++

			if afterLines[i] != `commit = "`+newCommit+`"` {
				t.Errorf("line %d = %q", i+1, afterLines[i])
			}
		}
	}

	if changed != 1 {
		t.Errorf("%d lines changed, want exactly the commit line", changed)
	}
}

func TestRewriteRefusesAnInvalidSource(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.toml")

	if err := os.WriteFile(in, []byte("kind = \"upstream\"\ncommit = \"05b037b7a2b68ead6893c857db2594bcad9951d4\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := sourcefile.Rewrite(in, filepath.Join(dir, "out.toml"), newCommit); fault.KindOf(err) != fault.Usage {
		t.Errorf("an incomplete source description was rewritten: %v", err)
	}
}
