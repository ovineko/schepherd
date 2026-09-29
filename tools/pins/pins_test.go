package pins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJSONSchemaManifest(t *testing.T) {
	tool, err := JSONSchema()
	if err != nil {
		t.Fatalf("JSONSchema() = %v", err)
	}

	if tool.Version != "16.12.0" || tool.License != "AGPL-3.0" {
		t.Errorf("version/license = %q/%q", tool.Version, tool.License)
	}

	for _, platform := range []string{
		"linux-x86_64", "linux-x86_64-musl", "linux-arm64", "linux-arm64-musl",
		"darwin-x86_64", "darwin-arm64", "windows-x86_64",
	} {
		link, asset, err := tool.AssetURL(platform)
		if err != nil {
			t.Errorf("AssetURL(%s) = %v", platform, err)

			continue
		}

		want := "https://github.com/sourcemeta/jsonschema/releases/download/v16.12.0/jsonschema-16.12.0-" + platform + ".zip"
		if link != want {
			t.Errorf("AssetURL(%s) = %s, want %s", platform, link, want)
		}

		if !strings.HasPrefix(asset.Binary, "jsonschema-16.12.0-"+platform+"/bin/jsonschema") {
			t.Errorf("asset %s binary = %s", platform, asset.Binary)
		}
	}

	linux, _, err := tool.AssetURL("linux-x86_64")
	if err != nil || !strings.HasSuffix(linux, ".zip") {
		t.Fatalf("linux asset: %v", err)
	}

	if tool.Assets["linux-x86_64"].SHA256 != "9808618bc9579a11412154ab329f6db81bbd8e51e05167e3a6fab8e771f54183" {
		t.Errorf("linux-x86_64 sha256 = %s", tool.Assets["linux-x86_64"].SHA256)
	}

	if _, _, err := tool.AssetURL("windows-arm64"); err == nil {
		t.Error("AssetURL(windows-arm64) succeeded for an unpublished platform")
	}
}

func TestParseRejectsInvalidManifests(t *testing.T) {
	valid := `"version":"1.0.0","license":"MIT","download":"https://example.com/d/"`
	asset := func(file, sum, binary string) string {
		return `"assets":{"p":{"file":"` + file + `","sha256":"` + sum + `","binary":"` + binary + `"}}`
	}
	sum := strings.Repeat("a", 64)

	cases := map[string]string{
		"unknown field":     `{` + valid + `,"extra":1,` + asset("t-1.0.0.zip", sum, "t/bin/t") + `}`,
		"missing version":   `{"license":"MIT","download":"https://example.com/d/",` + asset("t-1.0.0.zip", sum, "t/bin/t") + `}`,
		"http download":     `{"version":"1.0.0","license":"MIT","download":"http://example.com/d/",` + asset("t-1.0.0.zip", sum, "t/bin/t") + `}`,
		"download no slash": `{"version":"1.0.0","license":"MIT","download":"https://example.com/d",` + asset("t-1.0.0.zip", sum, "t/bin/t") + `}`,
		"no assets":         `{` + valid + `,"assets":{}}`,
		"short sha":         `{` + valid + `,` + asset("t-1.0.0.zip", "abc", "t/bin/t") + `}`,
		"upper sha":         `{` + valid + `,` + asset("t-1.0.0.zip", strings.Repeat("A", 64), "t/bin/t") + `}`,
		"file wrong ver":    `{` + valid + `,` + asset("t-2.0.0.zip", sum, "t/bin/t") + `}`,
		"file with dir":     `{` + valid + `,` + asset("x/t-1.0.0.zip", sum, "t/bin/t") + `}`,
		"binary absolute":   `{` + valid + `,` + asset("t-1.0.0.zip", sum, "/bin/t") + `}`,
		"binary escapes":    `{` + valid + `,` + asset("t-1.0.0.zip", sum, "../t") + `}`,
		"binary unclean":    `{` + valid + `,` + asset("t-1.0.0.zip", sum, "t//bin/t") + `}`,
		"trailing garbage":  `{` + valid + `,` + asset("t-1.0.0.zip", sum, "t/bin/t") + `} x`,
	}

	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parse([]byte(input)); err == nil {
				t.Errorf("parse accepted %s", input)
			}
		})
	}

	if _, err := parse([]byte(`{` + valid + `,` + asset("t-1.0.0.zip", sum, "t/bin/t") + `}`)); err != nil {
		t.Errorf("parse rejected a valid manifest: %v", err)
	}
}

// TestRubyVersionFile keeps the Ruby that ruby/setup-ruby installs from
// packaging/ruby/.ruby-version on RubyVersion.
func TestRubyVersionFile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "packaging", "ruby", ".ruby-version"))
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.TrimSpace(string(data)); got != RubyVersion {
		t.Errorf("packaging/ruby/.ruby-version is %q, RubyVersion is %q", got, RubyVersion)
	}
}
