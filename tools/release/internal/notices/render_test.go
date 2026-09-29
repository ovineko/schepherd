package notices

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/tools/release/internal/licenses"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var (
	linuxAMD64   = licenses.Target{GOOS: "linux", GOARCH: "amd64"}
	linuxARM64   = licenses.Target{GOOS: "linux", GOARCH: "arm64"}
	windowsAMD64 = licenses.Target{GOOS: "windows", GOARCH: "amd64"}
	windowsARM64 = licenses.Target{GOOS: "windows", GOARCH: "arm64"}
	allTargets   = []licenses.Target{linuxAMD64, linuxARM64, windowsAMD64, windowsARM64}
)

func licenseFile(name, text string) []licenses.LicenseFile {
	return []licenses.LicenseFile{{Name: name, Text: text}}
}

// fixtureInputs is a fake module set: the client links the standard library,
// an MIT module on every target and an Apache-2.0 module only on Windows.
func fixtureInputs(t *testing.T) *inputs {
	t.Helper()

	return &inputs{
		targets: allTargets,
		client: []licenses.Component{
			{
				Name: licenses.Stdlib, Version: "go1.99.0", Targets: allTargets,
				Files: []licenses.LicenseFile{{Name: "LICENSE", Text: goLicense}, {Name: "PATENTS", Text: goPatentGrant}},
			},
			{Name: "example.test/mit", Version: "v1.0.0", Targets: allTargets, Files: licenseFile("LICENSE", mitLicense)},
			{
				Name: "example.test/winonly", Version: "v2.0.0", Targets: []licenses.Target{windowsAMD64, windowsARM64},
				Files: []licenses.LicenseFile{{Name: "LICENSE.txt", Text: apacheLicense}, {Name: "NOTICE", Text: "Winonly\n"}},
			},
		},
		data:  &data{SchemaStore: fixtureSchemaStore()},
		rules: fixtureRules(),
		state: fixtureCatalog(t),
	}
}

func TestRenderMatchesTheGoldenFile(t *testing.T) {
	got, err := render(fixtureInputs(t))
	if err != nil {
		t.Fatal(err)
	}

	golden := filepath.Join("testdata", "THIRD_PARTY_NOTICES.golden")

	if *update {
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != strings.ReplaceAll(string(want), "\r\n", "\n") {
		t.Errorf("render differs from %s (go test -run %s -update rewrites it):\n%s", golden, t.Name(), got)
	}
}

func TestRenderWithoutCatalog(t *testing.T) {
	in := fixtureInputs(t)
	in.state = nil

	got, err := render(in)
	if err != nil {
		t.Fatal(err)
	}

	text := string(got)
	if !strings.HasSuffix(text, "\n\n"+noCatalog) || strings.Contains(text, "### ") {
		t.Errorf("notices without a catalog:\n%s", text)
	}

	in.rules = map[string]policyRule{}
	if _, err := render(in); err == nil || !strings.Contains(err.Error(), "schemastore.rules") {
		t.Errorf("stale SchemaStore rules without a catalog: err = %v", err)
	}
}

func TestRenderRefusesAnUnrecognizedModuleLicense(t *testing.T) {
	in := fixtureInputs(t)
	in.client[1].Files = licenseFile("LICENSE", iscLicense)

	want := "example.test/mit v1.0.0: LICENSE: the license text is not recognized"
	if _, err := render(in); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want %q", err, want)
	}
}

func TestScope(t *testing.T) {
	darwin := licenses.Target{GOOS: "darwin", GOARCH: "arm64"}
	all := append([]licenses.Target{darwin}, allTargets...)

	cases := []struct {
		targets []licenses.Target
		want    string
	}{
		{targets: all},
		{targets: []licenses.Target{windowsAMD64, windowsARM64}, want: "Windows builds only"},
		{targets: []licenses.Target{darwin, linuxAMD64, linuxARM64}, want: "macOS and Linux builds only"},
		{targets: []licenses.Target{linuxAMD64, windowsAMD64}, want: "linux/amd64, windows/amd64 only"},
	}

	for _, tc := range cases {
		if got := scope(tc.targets, all); got != tc.want {
			t.Errorf("scope(%v) = %q, want %q", tc.targets, got, tc.want)
		}
	}
}
