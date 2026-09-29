package licenses

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/env"
)

var (
	linux   = Target{GOOS: "linux", GOARCH: "amd64"}
	windows = Target{GOOS: "windows", GOARCH: "amd64"}
)

// fixtureModule writes a main module whose client links example.test/dep on
// every target and example.test/winonly only on Windows, both through local
// replacements so that the test needs neither network nor module cache.
func fixtureModule(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.test/app\n\ngo 1.26\n\nrequire (\n\texample.test/dep v0.0.0\n\texample.test/winonly v1.2.3\n)\n\n" +
			"replace example.test/dep => ./dep\n\nreplace example.test/winonly v1.2.3 => ./winonly\n",
		"cmd/schepherd/main.go":         "package main\n\nimport \"example.test/dep\"\n\nfunc main() { dep.Hello() }\n",
		"cmd/schepherd/main_windows.go": "package main\n\nimport _ \"example.test/winonly\"\n",
		"dep/go.mod":                    "module example.test/dep\n\ngo 1.26\n",
		"dep/dep.go":                    "package dep\n\nimport \"fmt\"\n\nfunc Hello() { fmt.Println(\"hello\") }\n",
		"dep/LICENSE":                   "\r\nMIT License  \r\n\r\nCopyright (c) Dep Authors\t\r\n\r\n",
		"dep/NOTICE":                    "Dep notice\n",
		"dep/README.md":                 "not a license\n",
		"winonly/go.mod":                "module example.test/winonly\n\ngo 1.26\n",
		"winonly/w.go":                  "package winonly\n",
		"winonly/COPYING.md":            "BSD text\n",
	}

	for name, content := range files {
		writeFile(t, filepath.Join(root, filepath.FromSlash(name)), content)
	}

	return root
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fixtureOptions(root string) Options {
	return Options{Root: root, Targets: []Target{windows, linux}, Env: append(env.Environ(), "GOTOOLCHAIN=local")}
}

func TestCollectFollowsEveryTarget(t *testing.T) {
	root := fixtureModule(t)

	components, err := Collect(t.Context(), fixtureOptions(root))
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(components))
	for _, c := range components {
		names = append(names, c.Name+" "+c.Version)
	}

	if len(components) != 3 || components[0].Name != Stdlib || !strings.HasPrefix(components[0].Version, "go1.") ||
		!slices.Equal(names[1:], []string{"example.test/dep v0.0.0 => ./dep", "example.test/winonly v1.2.3 => ./winonly"}) {
		t.Fatalf("components = %v", names)
	}

	if want := []Target{linux, windows}; !slices.Equal(components[0].Targets, want) || !slices.Equal(components[1].Targets, want) {
		t.Errorf("targets of the standard library %v and dep %v, want %v", components[0].Targets, components[1].Targets, want)
	}

	if !slices.Equal(components[2].Targets, []Target{windows}) {
		t.Errorf("winonly targets = %v, want only windows", components[2].Targets)
	}

	wantDep := []LicenseFile{{Name: "LICENSE", Text: "MIT License\n\nCopyright (c) Dep Authors\n"}, {Name: "NOTICE", Text: "Dep notice\n"}}
	if !reflect.DeepEqual(components[1].Files, wantDep) {
		t.Errorf("dep files = %q, want %q", components[1].Files, wantDep)
	}

	if !slices.ContainsFunc(components[0].Files, func(f LicenseFile) bool {
		return f.Name == "LICENSE" && strings.Contains(f.Text, "The Go Authors")
	}) {
		t.Errorf("standard library files lack the Go LICENSE: %v", components[0].Files)
	}

	text := string(Render(components, []Target{windows, linux}))
	for _, want := range []string{
		"Release targets: linux/amd64, windows/amd64.",
		"  example.test/winonly v1.2.3 => ./winonly (windows/amd64 only)\n",
		"example.test/dep v0.0.0 => ./dep\nLinked into: all release targets\nFile: NOTICE\n",
		"example.test/winonly v1.2.3 => ./winonly\nLinked into: windows/amd64\nFile: COPYING.md\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered file lacks %q:\n%s", want, text)
		}
	}

	if strings.Contains(text, "not a license") || strings.Contains(text, root) {
		t.Errorf("rendered file contains a README or a local path:\n%s", text)
	}
}

func TestCollectRejectsAModuleWithoutLicense(t *testing.T) {
	root := fixtureModule(t)
	if err := os.Remove(filepath.Join(root, "winonly", "COPYING.md")); err != nil {
		t.Fatal(err)
	}

	_, err := Collect(t.Context(), fixtureOptions(root))
	if err == nil || !strings.Contains(err.Error(), "example.test/winonly") || !strings.Contains(err.Error(), "no LICENSE") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckDetectsDrift(t *testing.T) {
	root := fixtureModule(t)
	opts := fixtureOptions(root)

	generated, err := Generate(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}

	crlf := strings.ReplaceAll(string(generated), "\n", "\r\n")
	writeFile(t, filepath.Join(root, File), crlf)

	if v, err := Check(t.Context(), opts); err != nil || len(v) != 0 {
		t.Fatalf("up-to-date files (checked out with CRLF): violations %v, err %v", v, err)
	}

	writeFile(t, filepath.Join(root, File), strings.Replace(string(generated), "Dep notice", "Edited notice", 1))

	if v, _ := Check(t.Context(), opts); !containsAll(v, File+" does not match") {
		t.Errorf("edited license file: violations %v", v)
	}

	writeFile(t, filepath.Join(root, File), string(generated))

	if err := os.Remove(filepath.Join(root, "cmd", "schepherd", "main_windows.go")); err != nil {
		t.Fatal(err)
	}

	v, err := Check(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}

	if !containsAll(v, File+": lists example.test/winonly v1.2.3 => ./winonly (windows/amd64 only), which the client no longer links") {
		t.Errorf("dropped dependency: violations %v", v)
	}

	if err := os.Remove(filepath.Join(root, File)); err != nil {
		t.Fatal(err)
	}

	if v, _ := Check(t.Context(), opts); !containsAll(v, File+" is missing") {
		t.Errorf("missing license file: violations %v", v)
	}
}

func TestReleaseTargetsCoverEveryPlatform(t *testing.T) {
	want := []Target{
		{GOOS: "darwin", GOARCH: "amd64"},
		{GOOS: "darwin", GOARCH: "arm64"},
		{GOOS: "linux", GOARCH: "amd64"},
		{GOOS: "linux", GOARCH: "arm64"},
		{GOOS: "windows", GOARCH: "amd64"},
		{GOOS: "windows", GOARCH: "arm64"},
	}

	if got := ReleaseTargets(); !slices.Equal(got, want) {
		t.Errorf("ReleaseTargets() = %v, want %v", got, want)
	}
}

func containsAll(violations []string, wants ...string) bool {
	joined := strings.Join(violations, "\n")
	for _, want := range wants {
		if !strings.Contains(joined, want) {
			return false
		}
	}

	return true
}
