package tomlfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
)

type inner struct {
	Name string `toml:"name"`
}

type sample struct {
	Tables  map[string]inner `toml:"tables"`
	Kind    string           `toml:"kind"`
	Items   []inner          `toml:"items"`
	Nested  inner            `toml:"nested"`
	Ignored string           `toml:"-"`
	Limit   int64            `toml:"limit"`
}

func TestDecodeBytes(t *testing.T) {
	data := `
kind = "local"
limit = 5

[nested]
name = "n"

[[items]]
name = "a"

[[items]]
name = "b"

[tables.x]
name = "t"
`

	var got sample
	if err := DecodeBytes([]byte(data), &got); err != nil {
		t.Fatal(err)
	}

	if got.Kind != "local" || got.Limit != 5 || got.Nested.Name != "n" || len(got.Items) != 2 ||
		got.Items[1].Name != "b" || got.Tables["x"].Name != "t" {
		t.Fatalf("decoded %+v", got)
	}
}

func TestDecodeBytesErrors(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "unknown top-level key", data: "kind = \"x\"\nextra = 1\n", want: `line 2, column 1: unknown key "extra"`},
		{name: "unknown nested key", data: "[[items]]\nname = \"a\"\nnme = \"b\"\n", want: `unknown key "items.nme"`},
		{name: "wrong case", data: "Kind = \"x\"\n", want: `unknown key "Kind" (keys are case-sensitive)`},
		{name: "wrong case nested", data: "[nested]\nNAME = \"x\"\n", want: `unknown key "nested.NAME"`},
		{name: "wrong case in array", data: "[[items]]\nName = \"x\"\n", want: `unknown key "items[0].Name"`},
		{name: "wrong case in map value", data: "[tables.k]\nName = \"x\"\n", want: `unknown key "tables.k.Name"`},
		{name: "type mismatch", data: "limit = \"five\"\n", want: "line 1, column"},
		{name: "syntax", data: "kind = \n", want: "line 1, column"},
		{name: "duplicate", data: "kind = \"a\"\nkind = \"b\"\n", want: "line 2"},
		{name: "ignored field", data: "Ignored = \"x\"\n", want: "unknown key"},
		{name: "invalid utf-8", data: "kind = \"\xff\"\n", want: "UTF-8"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got sample

			err := DecodeBytes([]byte(tt.data), &got)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestDecodeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.toml")

	if err := os.WriteFile(path, []byte("kind = \"ok\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var got sample
	if err := Decode(path, 1024, &got); err != nil || got.Kind != "ok" {
		t.Fatalf("Decode = %+v, %v", got, err)
	}

	err := Decode(path, 4, &got)
	if err == nil || fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), "larger than 4 bytes") {
		t.Fatalf("size error = %v", err)
	}

	err = Decode(filepath.Join(dir, "missing.toml"), 1024, &got)
	if err == nil || fault.KindOf(err) != fault.Usage {
		t.Fatalf("missing error = %v", err)
	}

	if err := os.WriteFile(path, []byte("bogus = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = Decode(path, 1024, &got)
	if err == nil || fault.KindOf(err) != fault.Usage || !strings.Contains(err.Error(), path) {
		t.Fatalf("decode error = %v", err)
	}
}

func FuzzDecodeBytes(f *testing.F) {
	f.Add("kind = \"x\"\n[[items]]\nname = \"a\"\n")
	f.Add("[tables.a]\nname = \"b\"\n")
	f.Add("Kind = 1")

	f.Fuzz(func(_ *testing.T, data string) {
		var got sample

		_ = DecodeBytes([]byte(data), &got)
	})
}
