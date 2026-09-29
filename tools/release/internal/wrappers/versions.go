package wrappers

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/ovineko/schepherd/internal/buildinfo/semver"
)

// SnapshotPrefix starts the version GoReleaser gives a snapshot build
// (snapshot.version_template in .goreleaser.yaml), which is followed by the
// abbreviated commit.
const SnapshotPrefix = "0.0.0-snapshot-"

var (
	snapshotCommit = regexp.MustCompile(`^[0-9a-f]{4,40}$`)
	gemLetters     = regexp.MustCompile(`^[a-z]+$`)
	gemNumber      = regexp.MustCompile(`^(?:0|[1-9][0-9]*)$`)
	pep440Number   = regexp.MustCompile(`^[0-9]+$`)
)

// pep440Kinds maps the SemVer pre-releases PEP 440 can express to its
// pre-release segments; both schemes order alpha < beta < rc < release.
var pep440Kinds = map[string]string{"alpha": "a", "beta": "b", "rc": "rc"}

// Versions is one client version in the form of each registry.
type Versions struct {
	// SemVer is the client and npm version.
	SemVer string
	PEP440 string
	Gem    string
	// Snapshot marks 0.0.0-snapshot-<commit>, which PyPI and RubyGems never
	// receive: its PEP 440 form has a local version label, which PyPI
	// refuses, and the gemspec names a push host that does not exist.
	Snapshot bool
}

// VersionsOf maps a release version X.Y.Z[-prerelease] or a snapshot version
// to every registry, and fails for a version that PyPI or RubyGems cannot
// express in SemVer order.
func VersionsOf(version string) (Versions, error) {
	if commit, ok := strings.CutPrefix(version, SnapshotPrefix); ok {
		if !snapshotCommit.MatchString(commit) {
			return Versions{}, fmt.Errorf("snapshot version %s: want %s<lowercase hexadecimal commit>", version, SnapshotPrefix)
		}

		return Versions{SemVer: version, PEP440: "0.0.0+snapshot." + commit, Gem: "0.0.0.1.snapshot." + commit, Snapshot: true}, nil
	}

	v, err := semver.ParseRelease(version)
	if err != nil {
		return Versions{}, fmt.Errorf("package version: %w", err)
	}

	pep440, err := PEP440(v)
	if err != nil {
		return Versions{}, err
	}

	gem, err := GemVersion(v)
	if err != nil {
		return Versions{}, err
	}

	return Versions{SemVer: v.String(), PEP440: pep440, Gem: gem}, nil
}

// PEP440 maps a release version to PEP 440: X.Y.Z stays, and X.Y.Z-alpha.N,
// -beta.N and -rc.N become X.Y.ZaN, X.Y.ZbN and X.Y.ZrcN. No other
// pre-release has a PEP 440 form that keeps the SemVer order.
func PEP440(v semver.Version) (string, error) {
	core := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Prerelease == "" {
		return core, nil
	}

	kind, number, _ := strings.Cut(v.Prerelease, ".")
	if segment, ok := pep440Kinds[kind]; ok && pep440Number.MatchString(number) {
		return core + segment + number, nil
	}

	return "", fmt.Errorf("version %s: the pre-release %q has no PEP 440 form; use -alpha.N, -beta.N or -rc.N", v, v.Prerelease)
}

// GemVersion maps a release version to RubyGems: X.Y.Z stays, and a
// pre-release keeps its identifiers, 0.2.0-rc.1 becoming 0.2.0.rc.1, but only
// in the form <letters>[.<number>...] without a trailing .0: RubyGems sorts
// letters below numbers where SemVer does the opposite, splits rc1 into rc.1
// and ignores trailing zeros, so no other form keeps the order and identity
// of the SemVer version. The snapshot form 0.0.0.1.snapshot.<commit> of
// VersionsOf sorts above 0, the implicit requirement of an installed
// executable, and no release has four numbers.
func GemVersion(v semver.Version) (string, error) {
	gem := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Prerelease == "" {
		return gem, nil
	}

	ids := strings.Split(v.Prerelease, ".")
	valid := gemLetters.MatchString(ids[0]) && (len(ids) == 1 || ids[len(ids)-1] != "0")

	for _, id := range ids[1:] {
		valid = valid && gemNumber.MatchString(id)
	}

	if !valid {
		return "", fmt.Errorf("version %s: the pre-release %q has no RubyGems form; use a lowercase word and numbers without a trailing .0, such as rc.1", v, v.Prerelease)
	}

	return gem + "." + v.Prerelease, nil
}
