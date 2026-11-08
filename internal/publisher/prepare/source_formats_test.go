package prepare

import (
	"slices"
	"testing"
)

func TestSourceFormatsIncludesJsoncAndExtensionlessConfigs(t *testing.T) {
	formats := sourceFormats([]string{"**/tsconfig.json", "**/.prettierrc", "**/*.yaml"})
	for _, want := range []struct {
		format string
		name   string
	}{
		{"jsonc", "tsconfig.json"},
		{"jsonc", ".prettierrc"},
		{"yaml", ""},
	} {
		found := false
		for _, actual := range formats {
			if actual.Format != want.format {
				continue
			}
			if want.name == "" || slices.Contains(actual.Filenames, want.name) {
				found = true
			}
		}
		if !found {
			t.Errorf("sourceFormats() omitted %s (%s): %#v", want.format, want.name, formats)
		}
	}
}
