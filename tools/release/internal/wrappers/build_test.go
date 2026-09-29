package wrappers

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildRefusesBadInputs(t *testing.T) {
	dist := fakeDist(t, allFakes()...)

	binaries, err := DistBinaries(dist, true)
	if err != nil {
		t.Fatal(err)
	}

	for _, b := range binaries {
		touch(t, b.Path, 1790000000)
	}
	full := t.TempDir()
	writeFile(t, filepath.Join(full, "old.tgz"), nil)

	for want, opts := range map[string]BuildOptions{
		"no PEP 440 form":                  {Dist: dist, Version: "1.0.0-preview.1", Out: t.TempDir()},
		"every release target is required": {Dist: fakeDist(t, fake{goos: "linux", goarch: "amd64"}), Version: "1.0.0", Out: t.TempDir(), AllTargets: true},
		"is not empty":                     {Dist: dist, Version: "1.0.0", Out: full},
		"different modification times":     {Dist: fakeDist(t, fake{goos: "linux", goarch: "amd64"}, fake{goos: "linux", goarch: "arm64"}), Version: "1.0.0"},
	} {
		if want == "different modification times" {
			binaries, err := DistBinaries(opts.Dist, false)
			if err != nil {
				t.Fatal(err)
			}

			touch(t, binaries[0].Path, 1)
			touch(t, binaries[1].Path, 2)
		}

		if _, err := Build(t.Context(), opts); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %v, want an error containing %q", opts, err, want)
		}
	}
}
