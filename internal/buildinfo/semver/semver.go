// Package semver parses the versions of the client release train: strict
// Semantic Versioning 2.0.0 (https://semver.org/spec/v2.0.0.html), written
// X.Y.Z[-prerelease] and tagged vX.Y.Z[-prerelease]. The client binary, its
// npm packages and the Go module all carry the same version. Catalog
// revisions are a separate scheme and never parse here.
package semver

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/mod/module"
	xsemver "golang.org/x/mod/semver"
)

// TagPrefix starts every release tag of the client.
const TagPrefix = "v"

// Version is a parsed SemVer 2.0.0 version.
type Version struct {
	// Prerelease holds the dot-separated pre-release identifiers without the
	// leading hyphen; empty for a normal version.
	Prerelease string
	// Build holds the build metadata without the leading plus sign. Release
	// versions never carry it.
	Build string

	Major, Minor, Patch uint64
}

// Parse parses s as a SemVer 2.0.0 version without a "v" prefix.
func Parse(s string) (Version, error) {
	v, err := parse(s)
	if err != nil {
		return Version{}, fmt.Errorf("invalid semantic version %q: %w", s, err)
	}

	return v, nil
}

// ParseTag parses a tag vX.Y.Z[-prerelease][+build].
func ParseTag(tag string) (Version, error) {
	rest, ok := strings.CutPrefix(tag, TagPrefix)
	if !ok {
		return Version{}, fmt.Errorf("invalid version tag %q: it must start with %q", tag, TagPrefix)
	}

	v, err := parse(rest)
	if err != nil {
		return Version{}, fmt.Errorf("invalid version tag %q: %w", tag, err)
	}

	return v, nil
}

// ParseRelease parses the version of a client release, X.Y.Z[-prerelease]:
// a valid SemVer version without build metadata, above 0.0.0, and not in any
// of the pseudo-version forms the go command gives untagged commits.
func ParseRelease(s string) (Version, error) {
	v, err := Parse(s)
	if err != nil {
		return Version{}, err
	}

	if err := v.releasable(); err != nil {
		return Version{}, fmt.Errorf("%q is not a release version: %w", s, err)
	}

	return v, nil
}

// ParseReleaseTag parses the tag of a client release, vX.Y.Z[-prerelease],
// with the rules of ParseRelease.
func ParseReleaseTag(tag string) (Version, error) {
	v, err := ParseTag(tag)
	if err != nil {
		return Version{}, err
	}

	if err := v.releasable(); err != nil {
		return Version{}, fmt.Errorf("%q is not a release tag: %w", tag, err)
	}

	return v, nil
}

// String returns the version as written by the specification, without a "v"
// prefix. For every version Parse accepted it is the parsed text.
func (v Version) String() string {
	s := strconv.FormatUint(v.Major, 10) + "." + strconv.FormatUint(v.Minor, 10) + "." + strconv.FormatUint(v.Patch, 10)

	if v.Prerelease != "" {
		s += "-" + v.Prerelease
	}

	if v.Build != "" {
		s += "+" + v.Build
	}

	return s
}

// Tag returns the git tag of the version.
func (v Version) Tag() string {
	return TagPrefix + v.String()
}

// IsPrerelease reports whether the version has pre-release identifiers.
// Pre-releases are published to npm under the dist-tag "next" and marked as
// pre-releases on GitHub; they never become "latest".
func (v Version) IsPrerelease() bool {
	return v.Prerelease != ""
}

// Compare returns -1, 0 or +1 as a has lower, equal or higher precedence
// than b. Build metadata does not take part in precedence.
func Compare(a, b Version) int {
	return xsemver.Compare(a.Tag(), b.Tag())
}

func (v Version) releasable() error {
	switch {
	case v.Build != "":
		return errors.New("build metadata is not allowed")
	case v.Major == 0 && v.Minor == 0 && v.Patch == 0:
		return errors.New("0.0.0 is reserved for snapshot and development builds")
	case v.isPseudoVersion():
		return errors.New("it has the form of a Go pseudo-version")
	}

	return nil
}

// isPseudoVersion reports whether the go command would read the version as
// one of the pseudo-versions it derives for untagged commits, never for a
// release.
func (v Version) isPseudoVersion() bool {
	return module.IsPseudoVersion(v.Tag())
}

// parse parses s with golang.org/x/mod/semver, which enforces the SemVer 2.0.0
// grammar, and additionally refuses its shorthands vMAJOR and vMAJOR.MINOR.
func parse(s string) (Version, error) {
	tag := TagPrefix + s
	if !xsemver.IsValid(tag) {
		return Version{}, errors.New("want MAJOR.MINOR.PATCH[-prerelease][+build] with identifiers of [0-9A-Za-z-] and numbers without leading zeros")
	}

	build := xsemver.Build(tag)
	if xsemver.Canonical(tag)+build != tag {
		return Version{}, errors.New("want MAJOR.MINOR.PATCH")
	}

	pre := xsemver.Prerelease(tag)
	core := strings.TrimSuffix(strings.TrimSuffix(s, build), pre)
	v := Version{Prerelease: strings.TrimPrefix(pre, "-"), Build: strings.TrimPrefix(build, "+")}

	parts := strings.Split(core, ".")
	for i, dst := range []*uint64{&v.Major, &v.Minor, &v.Patch} {
		n, err := strconv.ParseUint(parts[i], 10, 64)
		if err != nil {
			return Version{}, fmt.Errorf("%q is too large", parts[i])
		}

		*dst = n
	}

	return v, nil
}
