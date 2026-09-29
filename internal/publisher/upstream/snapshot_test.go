package upstream

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
)

func fetchTestSnapshot(t *testing.T) *Snapshot {
	t.Helper()

	ts := newTarballServer(t, gzipBytes(t, buildTar(t, validEntries())))

	snap, err := FetchSchemaStore(t.Context(), ts.fetcher, ts.base(), testCommit, filepath.Join(t.TempDir(), "snapshot"))
	if err != nil {
		t.Fatal(err)
	}

	return snap
}

func TestOpenSnapshot(t *testing.T) {
	fetched := fetchTestSnapshot(t)

	snap, err := OpenSnapshot(fetched.Dir)
	if err != nil {
		t.Fatal(err)
	}

	if snap.Commit != testCommit || snap.TarballDigest != fetched.TarballDigest || snap.Dir != fetched.Dir {
		t.Fatalf("snapshot = %+v", snap)
	}

	if got := slices.Sorted(maps.Keys(snap.schemas)); !reflect.DeepEqual(got, []string{"a.json", "b.json", "tsconfig.json"}) {
		t.Fatalf("schemas = %v", got)
	}

	if string(snap.License()) != "Apache License\n" || string(snap.Notice()) != "Test notice\n" {
		t.Fatalf("license %q notice %q", snap.License(), snap.Notice())
	}

	snap.License()[0] = 'X'
	if string(snap.License()) != "Apache License\n" {
		t.Fatal("License returned internal storage")
	}
}

func TestSnapshotCatalog(t *testing.T) {
	snap := fetchTestSnapshot(t)

	entries, err := snap.Catalog()
	if err != nil {
		t.Fatal(err)
	}

	want := []CatalogEntry{
		{Name: "A config", Description: "A", URL: "https://www.schemastore.org/a.json", FileMatch: []string{"a.json", "**/.a/*.yml"}},
		{Name: "B", Description: "B", URL: "https://json.schemastore.org/b", Versions: map[string]string{"1.0": "https://www.schemastore.org/b.json"}},
		{
			Name: "tsconfig", Description: "TS", FileMatch: []string{},
			URL: "https://raw.githubusercontent.com/SchemaStore/schemastore/master/src/schemas/json/tsconfig.json",
		},
		{Name: "External", Description: "E", URL: "https://example.com/external.json", FileMatch: []string{"external.json"}},
	}

	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("Catalog =\n%#v\nwant\n%#v", entries, want)
	}
}

func TestLocalPath(t *testing.T) {
	snap := fetchTestSnapshot(t)
	schemas := filepath.Join(snap.Dir, "src", "schemas", "json")

	tests := []struct {
		url  string
		want string
	}{
		{url: "https://www.schemastore.org/a.json", want: "a.json"},
		{url: "https://www.schemastore.org/a", want: "a.json"},
		{url: "https://json.schemastore.org/a.json", want: "a.json"},
		{url: "https://json.schemastore.org/b", want: "b.json"},
		{url: "https://www.schemastore.org/schemas/json/a.json", want: "a.json"},
		{url: "https://json.schemastore.org/schemas/json/b", want: "b.json"},
		{url: "https://WWW.SchemaStore.org/a.json", want: "a.json"},
		{url: "https://json.schemastore.org/a.json#/definitions/x", want: "a.json"},
		{url: "https://raw.githubusercontent.com/SchemaStore/schemastore/master/src/schemas/json/tsconfig.json", want: "tsconfig.json"},
		{url: "https://raw.githubusercontent.com/SchemaStore/schemastore/master/src/schemas/json/tsconfig"},
		{url: "https://raw.githubusercontent.com/SchemaStore/schemastore/main/src/schemas/json/tsconfig.json"},
		{url: "https://raw.githubusercontent.com/schemastore/schemastore/master/src/schemas/json/tsconfig.json"},
		{url: "http://www.schemastore.org/a.json"},
		{url: "https://www.schemastore.org/a.json?x=1"},
		{url: "https://www.schemastore.org/a.json?"},
		{url: "https://www.schemastore.org:443/a.json"},
		{url: "https://user@www.schemastore.org/a.json"},
		{url: "https://www.schemastore.org/missing.json"},
		{url: "https://www.schemastore.org/sub/x.json"},
		{url: "https://www.schemastore.org/schemas/json/sub/x.json"},
		{url: "https://www.schemastore.org/../a.json"},
		{url: "https://www.schemastore.org/%61.json"},
		{url: "https://www.schemastore.org/.a.json"},
		{url: "https://www.schemastore.org/"},
		{url: "https://www.schemastore.org/api/json/catalog.json"},
		{url: "https://example.com/a.json"},
		{url: "not a url %"},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got, ok := snap.LocalPath(tt.url)
			if tt.want == "" {
				if ok {
					t.Fatalf("LocalPath(%q) = %q", tt.url, got)
				}

				return
			}

			if !ok || got != filepath.Join(schemas, tt.want) {
				t.Fatalf("LocalPath(%q) = %q, %v", tt.url, got, ok)
			}

			canonical, ok := snap.CanonicalURL(tt.url)
			if !ok || canonical != CanonicalSchemaStoreBase+tt.want {
				t.Fatalf("CanonicalURL(%q) = %q, %v", tt.url, canonical, ok)
			}
		})
	}
}

func TestSnapshotTests(t *testing.T) {
	snap := fetchTestSnapshot(t)
	abs := func(rel string) string { return filepath.Join(snap.Dir, filepath.FromSlash(rel)) }

	for _, name := range []string{"a", "a.json"} {
		positive, negative := snap.Tests(name)

		if !reflect.DeepEqual(positive, []string{abs("src/test/a/one.json"), abs("src/test/a/two.yaml")}) {
			t.Fatalf("positive(%s) = %v", name, positive)
		}

		if !reflect.DeepEqual(negative, []string{abs("src/negative_test/a/bad.json")}) {
			t.Fatalf("negative(%s) = %v", name, negative)
		}
	}

	positive, negative := snap.Tests("b")
	if !reflect.DeepEqual(positive, []string{abs("src/test/b/nested/deep.toml")}) || negative != nil {
		t.Fatalf("Tests(b) = %v %v", positive, negative)
	}

	for _, name := range []string{"tsconfig", "missing", "../a", "", "stray"} {
		if positive, negative := snap.Tests(name); positive != nil || negative != nil {
			t.Fatalf("Tests(%q) = %v %v", name, positive, negative)
		}
	}
}

func TestOpenSnapshotErrors(t *testing.T) {
	good := fetchTestSnapshot(t)

	clone := func(t *testing.T) string {
		t.Helper()

		dst := filepath.Join(t.TempDir(), "copy")
		if err := os.CopyFS(dst, os.DirFS(good.Dir)); err != nil {
			t.Fatal(err)
		}

		return dst
	}

	writeMeta := func(t *testing.T, dir, content string) {
		t.Helper()

		if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	digestValue := good.TarballDigest

	tests := []struct {
		prepare func(t *testing.T, dir string)
		name    string
		kind    fault.Kind
	}{
		{name: "no metadata", kind: fault.Usage, prepare: func(t *testing.T, dir string) {
			t.Helper()

			if err := os.Remove(filepath.Join(dir, "snapshot.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unknown member", kind: fault.Integrity, prepare: func(t *testing.T, dir string) {
			t.Helper()
			writeMeta(t, dir, `{"commit":"`+testCommit+`","tarballDigest":"`+digestValue+`","formatVersion":1,"x":1}`)
		}},
		{name: "wrong case", kind: fault.Integrity, prepare: func(t *testing.T, dir string) {
			t.Helper()
			writeMeta(t, dir, `{"Commit":"`+testCommit+`","tarballDigest":"`+digestValue+`","formatVersion":1}`)
		}},
		{name: "bad commit", kind: fault.Integrity, prepare: func(t *testing.T, dir string) {
			t.Helper()
			writeMeta(t, dir, `{"commit":"main","tarballDigest":"`+digestValue+`","formatVersion":1}`)
		}},
		{name: "bad digest", kind: fault.Integrity, prepare: func(t *testing.T, dir string) {
			t.Helper()
			writeMeta(t, dir, `{"commit":"`+testCommit+`","tarballDigest":"md5:x","formatVersion":1}`)
		}},
		{name: "future format", kind: fault.Usage, prepare: func(t *testing.T, dir string) {
			t.Helper()
			writeMeta(t, dir, `{"commit":"`+testCommit+`","tarballDigest":"`+digestValue+`","formatVersion":2}`)
		}},
		{name: "wrong type", kind: fault.Integrity, prepare: func(t *testing.T, dir string) {
			t.Helper()
			writeMeta(t, dir, `{"commit":1,"tarballDigest":"`+digestValue+`","formatVersion":1}`)
		}},
		{name: "missing catalog", kind: fault.Integrity, prepare: func(t *testing.T, dir string) {
			t.Helper()

			if err := os.Remove(filepath.Join(dir, "src", "api", "json", "catalog.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing license", kind: fault.Integrity, prepare: func(t *testing.T, dir string) {
			t.Helper()

			if err := os.Remove(filepath.Join(dir, "LICENSE")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing schemas", kind: fault.Integrity, prepare: func(t *testing.T, dir string) {
			t.Helper()

			if err := os.RemoveAll(filepath.Join(dir, "src", "schemas")); err != nil {
				t.Fatal(err)
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := clone(t)
			tt.prepare(t, dir)

			_, err := OpenSnapshot(dir)
			if err == nil || fault.KindOf(err) != tt.kind {
				t.Fatalf("error = %v (kind %v), want kind %v", err, fault.KindOf(err), tt.kind)
			}
		})
	}

	t.Run("missing directory", func(t *testing.T) {
		_, err := OpenSnapshot(filepath.Join(t.TempDir(), "absent"))
		if fault.KindOf(err) != fault.Usage {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("notice is optional", func(t *testing.T) {
		dir := clone(t)
		if err := os.Remove(filepath.Join(dir, "NOTICE")); err != nil {
			t.Fatal(err)
		}

		snap, err := OpenSnapshot(dir)
		if err != nil || snap.Notice() != nil {
			t.Fatalf("snapshot = %v, %v", snap, err)
		}
	})

	t.Run("symlinks are not indexed", func(t *testing.T) {
		dir := clone(t)
		outside := filepath.Join(t.TempDir(), "outside.json")

		if err := os.WriteFile(outside, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := os.Symlink(outside, filepath.Join(dir, "src", "schemas", "json", "link.json")); err != nil {
			t.Fatal(err)
		}

		if err := os.Symlink(outside, filepath.Join(dir, "src", "test", "a", "zz.json")); err != nil {
			t.Fatal(err)
		}

		snap, err := OpenSnapshot(dir)
		if err != nil {
			t.Fatal(err)
		}

		if _, ok := snap.LocalPath("https://www.schemastore.org/link.json"); ok {
			t.Fatal("symlinked schema was indexed")
		}

		if positive, _ := snap.Tests("a"); len(positive) != 2 {
			t.Fatalf("symlinked test was indexed: %v", positive)
		}
	})

	t.Run("corrupt catalog", func(t *testing.T) {
		dir := clone(t)
		if err := os.WriteFile(filepath.Join(dir, "src", "api", "json", "catalog.json"), []byte(`{"schemas":[{}`), 0o600); err != nil {
			t.Fatal(err)
		}

		snap, err := OpenSnapshot(dir)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := snap.Catalog(); fault.KindOf(err) != fault.Integrity {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestParseCatalogErrors(t *testing.T) {
	entry := `{"name":"n","description":"d","url":"https://e.example/x.json"}`

	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "not object", data: `[]`, want: "expected a JSON object"},
		{name: "wrong version", data: `{"version":2,"schemas":[]}`, want: "version must be 1"},
		{name: "schema not string", data: `{"$schema":1,"schemas":[]}`, want: "$schema must be a string"},
		{name: "no schemas", data: `{"version":1}`, want: "no schemas"},
		{name: "schemas null", data: `{"schemas":null}`, want: `"schemas" must not be null`},
		{name: "schema null", data: `{"$schema":null,"schemas":[]}`, want: `"$schema" must not be null`},
		{name: "version null", data: `{"version":null,"schemas":[]}`, want: `"version" must not be null`},
		{name: "schemas object", data: `{"schemas":{}}`, want: "must be an array"},
		{name: "duplicate key", data: `{"schemas":[{"name":"n","name":"m","description":"d","url":"https://e.example/x"}]}`, want: "duplicate"},
		{name: "trailing data", data: `{"schemas":[` + entry + `]} x`, want: "malformed"},
		{name: "invalid utf-8", data: "{\"schemas\":[{\"name\":\"\xff\",\"description\":\"d\",\"url\":\"https://e.example/x\"}]}", want: "UTF-8"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseCatalog([]byte(tt.data))
			if err == nil || fault.KindOf(err) != fault.Integrity || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}

	entries, err := ParseCatalog([]byte(`{"schemas":[` + entry + `]}`))
	if err != nil || len(entries) != 1 || entries[0].FileMatch != nil {
		t.Fatalf("minimal catalog = %#v, %v", entries, err)
	}
}

// SchemaStore owns the catalog format and adds members without notice; a
// member Schepherd does not consume must not hold every published schema.
func TestParseCatalogIgnoresUnknownMembers(t *testing.T) {
	const x = "https://e.example/x.json"

	tests := []struct {
		name string
		data string
	}{
		{"entry member", `{"schemas":[{"name":"n","description":"d","url":"` + x + `","fileMatch":["a"],"deprecated":true}]}`},
		{"top-level member", `{"$schema":"s","version":1,"generated":"2026-01-01","schemas":[{"name":"n","description":"d","url":"` + x + `","fileMatch":["a"]}]}`},
		{"null in unknown members", `{"extra":null,"schemas":[{"name":"n","description":"d","url":"` + x + `","fileMatch":["a"],"removedIn":null}]}`},
		{"object in unknown member", `{"schemas":[{"name":"n","description":"d","url":"` + x + `","fileMatch":["a"],"meta":{"a":[null,1]}}]}`},
	}

	want := []CatalogEntry{{Name: "n", Description: "d", URL: x, FileMatch: []string{"a"}}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries, err := ParseCatalog([]byte(tt.data))
			if err != nil || !reflect.DeepEqual(entries, want) {
				t.Fatalf("ParseCatalog = %#v, %v", entries, err)
			}
		})
	}

	_, err := ParseCatalog([]byte(`{"schemas":[],"extra":{"a":1,"a":2}}`))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("a duplicate key in an unknown member: %v", err)
	}
}

// An entry that breaks the rules is one bad upstream record, not a bad
// catalog: it comes back with its problem and, when they are usable, its
// name and URL, and the entries after it are read as usual.
func TestParseCatalogKeepsInvalidEntries(t *testing.T) {
	const (
		valid = `{"name":"ok","description":"d","url":"https://e.example/ok.json"}`
		x     = "https://e.example/x"
	)

	var (
		named     = CatalogEntry{Name: "n", URL: x}
		nameAlone = CatalogEntry{Name: "n"}
		urlAlone  = CatalogEntry{URL: x}
	)

	tests := []struct {
		name  string
		entry string
		want  string
		ident CatalogEntry
	}{
		{"name null", `{"name":null,"description":"d","url":"` + x + `"}`, `"name" must not be null`, urlAlone},
		{"name not string", `{"name":1,"description":"d","url":"` + x + `"}`, `"name" has the wrong type`, urlAlone},
		{"empty name", `{"name":"","description":"d","url":"` + x + `"}`, "empty name", urlAlone},
		{"description null", `{"name":"n","description":null,"url":"` + x + `"}`, `"description" must not be null`, named},
		{"missing description", `{"name":"n","url":"` + x + `"}`, `missing "description"`, named},
		{"url null", `{"name":"n","description":"d","url":null}`, `"url" must not be null`, nameAlone},
		{"missing url", `{"name":"n","description":"d"}`, `missing "url"`, nameAlone},
		{"relative url", `{"name":"n","description":"d","url":"x.json"}`, "not an absolute http(s) URL", nameAlone},
		{"ftp url", `{"name":"n","description":"d","url":"ftp://e.example/x"}`, "not an absolute http(s) URL", nameAlone},
		{"credentials", `{"name":"n","description":"d","url":"https://u:secret@e.example/x"}`, "credentials", nameAlone},
		{"fileMatch null", `{"name":"n","description":"d","url":"` + x + `","fileMatch":null}`, `"fileMatch" must not be null`, named},
		{"fileMatch null item", `{"name":"n","description":"d","url":"` + x + `","fileMatch":[null,"a"]}`, "fileMatch item 0 must be a non-empty string", named},
		{"fileMatch empty item", `{"name":"n","description":"d","url":"` + x + `","fileMatch":["a",""]}`, "fileMatch item 1 must be a non-empty string", named},
		{"fileMatch number item", `{"name":"n","description":"d","url":"` + x + `","fileMatch":[1]}`, `"fileMatch" has the wrong type`, named},
		{"fileMatch type", `{"name":"n","description":"d","url":"` + x + `","fileMatch":"a"}`, `"fileMatch" has the wrong type`, named},
		{"versions null", `{"name":"n","description":"d","url":"` + x + `","versions":null}`, `"versions" must not be null`, named},
		{"version value null", `{"name":"n","description":"d","url":"` + x + `","versions":{"1":null}}`, `version "1" must be a string`, named},
		{"bad version url", `{"name":"n","description":"d","url":"` + x + `","versions":{"1":"x"}}`, `version "1"`, named},
		{"wrong case", `{"name":"n","description":"d","URL":"` + x + `"}`, `missing "url"`, nameAlone},
		{"malformed consumed member next to an unknown one", `{"name":"n","description":"d","url":"` + x + `","fileMatch":"a","x":1}`, `"fileMatch" has the wrong type`, named},
		{"entry not object", `1`, "expected a JSON object", CatalogEntry{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries, err := ParseCatalog([]byte(`{"schemas":[` + tt.entry + `,` + valid + `]}`))
			if err != nil || len(entries) != 2 {
				t.Fatalf("ParseCatalog = %#v, %v", entries, err)
			}

			bad := entries[0]
			if !strings.Contains(bad.Problem, tt.want) || !strings.HasPrefix(bad.Problem, "catalog entry 0: ") || strings.Contains(bad.Problem, "secret") {
				t.Errorf("problem = %q, want it to contain %q", bad.Problem, tt.want)
			}

			bad.Problem = ""
			if !reflect.DeepEqual(bad, tt.ident) {
				t.Errorf("invalid entry = %#v, want %#v", bad, tt.ident)
			}

			if ok := entries[1]; ok.Problem != "" || ok.Name != "ok" || ok.URL != "https://e.example/ok.json" {
				t.Errorf("the entry after it = %#v", ok)
			}
		})
	}
}

func containsNull(v any) bool {
	switch v := v.(type) {
	case nil:
		return true
	case []any:
		return slices.ContainsFunc(v, containsNull)
	case map[string]any:
		for _, member := range v {
			if containsNull(member) {
				return true
			}
		}
	}

	return false
}

// consumedNull reports a null anywhere in the members ParseCatalog consumes.
func consumedNull(doc any) bool {
	top, ok := doc.(map[string]any)
	if !ok {
		return false
	}

	for _, name := range []string{"$schema", "version"} {
		if v, ok := top[name]; ok && v == nil {
			return true
		}
	}

	schemas, ok := top["schemas"]
	if !ok {
		return false
	}

	items, ok := schemas.([]any)
	if !ok {
		return schemas == nil
	}

	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}

		for _, name := range []string{"name", "description", "url", "fileMatch", "versions"} {
			if v, ok := entry[name]; ok && containsNull(v) {
				return true
			}
		}
	}

	return false
}

func FuzzParseCatalog(f *testing.F) {
	f.Add([]byte(testCatalog))
	f.Add([]byte(`{"schemas":[{"name":"n","description":null,"url":"https://e.example/x","fileMatch":[null,"a"],"versions":null}]}`))
	f.Add([]byte(`{"schemas":[{"name":"n","description":"d","url":"https://e.example/x","fileMatch":[]}]}`))
	f.Add([]byte(`{"schemas":[{"URL":1}]}`))
	f.Add([]byte(`{"extra":null,"schemas":[{"name":"n","description":"d","url":"https://e.example/x","x":[null]}]}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		entries, err := ParseCatalog(data)
		if err != nil {
			return
		}

		var doc any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("accepted a catalog that is not JSON: %s", data)
		}

		if !slices.ContainsFunc(entries, func(e CatalogEntry) bool { return e.Problem != "" }) && consumedNull(doc) {
			t.Fatalf("accepted a catalog with null values: %s", data)
		}

		for _, e := range entries {
			if e.Problem != "" {
				if e.Description != "" || e.FileMatch != nil || e.Versions != nil || (e.URL != "" && checkHTTPURL(e.URL) != nil) {
					t.Fatalf("an invalid entry carries more than its identity: %#v", e)
				}

				continue
			}

			if e.Name == "" || checkHTTPURL(e.URL) != nil || slices.Contains(e.FileMatch, "") {
				t.Fatalf("accepted invalid entry %#v", e)
			}

			for _, u := range e.Versions {
				if checkHTTPURL(u) != nil {
					t.Fatalf("accepted invalid version URL in %#v", e)
				}
			}
		}
	})
}

func FuzzLocalPath(f *testing.F) {
	f.Add("https://www.schemastore.org/a.json")
	f.Add("https://json.schemastore.org/schemas/json/b")
	f.Add("https://www.schemastore.org/%2e%2e/a.json")

	snap := &Snapshot{Dir: filepath.Join(f.TempDir(), "snap"), schemas: map[string]struct{}{"a.json": {}, "b.json": {}}}
	base := filepath.Join(snap.Dir, "src", "schemas", "json")

	f.Fuzz(func(t *testing.T, raw string) {
		p, ok := snap.LocalPath(raw)
		if !ok {
			return
		}

		if filepath.Dir(p) != base {
			t.Fatalf("LocalPath(%q) = %q escapes %s", raw, p, base)
		}

		if _, known := snap.schemas[filepath.Base(p)]; !known {
			t.Fatalf("LocalPath(%q) = %q is not a known schema", raw, p)
		}
	})
}
