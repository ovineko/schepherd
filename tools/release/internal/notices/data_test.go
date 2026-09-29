package notices

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, name, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

const fixtureData = `[schemastore]
rules = ["schemastore"]
notice = "Example Store"
`

func TestLoadDataRefusesIncompleteOrUnknownData(t *testing.T) {
	for name, content := range map[string]string{
		"no rules":       "[schemastore]\nnotice = \"Example Store\"\n",
		"no notice":      "[schemastore]\nrules = [\"schemastore\"]\n",
		"unknown key":    fixtureData + "\n[images]\n",
		"removed tables": fixtureData + "\n[tools.alpha]\nname = \"Alpha\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, filepath.FromSlash(DataFile)), content)

			if _, err := loadData(root); err == nil {
				t.Error("loadData accepted it")
			}
		})
	}
}

// TestLoadDataOfACRLFCheckout keeps the notices identical on a Windows
// checkout, where the data file has CRLF line endings.
func TestLoadDataOfACRLFCheckout(t *testing.T) {
	root := t.TempDir()
	content := strings.Replace(fixtureData, `notice = "Example Store"`, "notice = \"\"\"\nExample Store\nCopyright 2015 Example\n\"\"\"", 1)
	writeFile(t, filepath.Join(root, filepath.FromSlash(DataFile)), strings.ReplaceAll(content, "\n", "\r\n"))

	d, err := loadData(root)
	if err != nil {
		t.Fatal(err)
	}

	if d.SchemaStore.Notice != "Example Store\nCopyright 2015 Example\n" {
		t.Errorf("notice = %q", d.SchemaStore.Notice)
	}
}
