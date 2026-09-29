// Package releasetag holds the gate a client release tag must pass beyond
// its SemVer syntax, checked against the tagged commit itself: its Go module
// path must accept the tag's major version, because the tag is also the Go
// module version.
package releasetag

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	"github.com/ovineko/schepherd/internal/buildinfo/semver"
	"github.com/ovineko/schepherd/tools/release/internal/gitx"
)

// ModulePath returns the path of the module directive of a go.mod file.
func ModulePath(goMod []byte) (string, error) {
	path := modfile.ModulePath(goMod)
	if path == "" {
		return "", errors.New("go.mod has no module directive")
	}

	return path, nil
}

// CheckModulePath refuses a version the Go module channel cannot serve under
// the module path: `go install <path>/...@vX.Y.Z` requires the suffix /vX
// for every major version X of 2 and above, and no suffix below.
func CheckModulePath(modulePath string, v semver.Version) error {
	_, pathMajor, ok := module.SplitPathVersion(modulePath)
	if !ok {
		return fmt.Errorf("the Go module path %s has an invalid major version suffix", modulePath)
	}

	if module.CheckPathMajor(v.Tag(), pathMajor) == nil {
		return nil
	}

	if pathMajor == "" {
		return fmt.Errorf("%s has major version %d, but the Go module path %s has no /v%d suffix, so `go install %s/cmd/schepherd@%s` "+
			"would fail; change the module path to %s/v%d (and every import of it) before releasing a major version above 1",
			v.Tag(), v.Major, modulePath, v.Major, modulePath, v.Tag(), modulePath, v.Major)
	}

	return fmt.Errorf("%s has major version %d, but the Go module path %s only serves major version %s",
		v.Tag(), v.Major, modulePath, strings.TrimPrefix(pathMajor, "/v"))
}

// CheckCommit applies CheckModulePath to the go.mod of commit, so what is
// checked is exactly what the tag releases.
func CheckCommit(ctx context.Context, git gitx.Runner, commit string, v semver.Version) error {
	goMod, err := git.Run(ctx, "cat-file", "blob", commit+":go.mod")
	if err != nil {
		return fmt.Errorf("read go.mod of commit %s: %w", commit, err)
	}

	modulePath, err := ModulePath(goMod)
	if err != nil {
		return fmt.Errorf("commit %s: %w", commit, err)
	}

	return CheckModulePath(modulePath, v)
}
