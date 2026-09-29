// Package wrappers builds the npm, PyPI and RubyGems packages that wrap the
// schepherd binary with the standard builders of each ecosystem (npm pack,
// hatchling through uv build, gem build), checks that every package holds
// exactly the files staged for it, installs them for a smoke test, and plans
// resumable publications from their checksum files.
//
// The packaging metadata lives in packaging/: packaging/python/pyproject.toml
// and its build hook, packaging/ruby/schepherd.gemspec, and the npm launcher
// in packaging/npm/schepherd, whose package.json files this package writes
// because they list the platform packages. This package only maps the
// release targets and versions to each ecosystem and stages the inputs.
package wrappers

import (
	"slices"
	"strings"
)

// Identity shared by the packages.
const (
	BinaryName = "schepherd"
	// BuildID is the GoReleaser build whose binaries the packages carry.
	BuildID  = "schepherd"
	NpmRoot  = "@ovineko/schepherd"
	Homepage = "https://schepherd.ovineko.com"
	// ThirdPartyLicenses holds the license texts of everything linked into
	// the binary; every package carries it next to LICENSE.
	ThirdPartyLicenses = "THIRD_PARTY_LICENSES.txt"
)

// Target is one release target and its name in each ecosystem. The launchers
// in packaging/npm/schepherd/lib/platform.js and packaging/ruby/lib/schepherd.rb
// hold the same table; a test keeps them identical.
type Target struct {
	GOOS   string
	GOARCH string
	// NpmOS and CPU are the npm os and cpu values; the gem's libexec
	// directory is schepherd-<GOOS>-<CPU>.
	NpmOS string
	CPU   string
	// Wheel lists the PEP 425 platform tags of the wheel, sorted as the
	// compressed tag set of its file name requires.
	//
	// The Linux binaries are static (CGO_ENABLED=0, which CheckBinary
	// enforces), so one wheel serves glibc and musl: manylinux_2_17 with its
	// alias manylinux2014 for pip before 20.3, and musllinux_1_1, the lowest
	// musllinux tag. The macOS tag is the minimum OS version the Go toolchain
	// records in the binary; CheckBinary refuses a binary that requires
	// another one.
	Wheel []string
}

// Targets lists every release target in the order of the npm package names.
var Targets = []Target{
	{"darwin", "arm64", "darwin", "arm64", []string{"macosx_13_0_arm64"}},
	{"darwin", "amd64", "darwin", "x64", []string{"macosx_13_0_x86_64"}},
	{"linux", "arm64", "linux", "arm64", []string{"manylinux2014_aarch64", "manylinux_2_17_aarch64", "musllinux_1_1_aarch64"}},
	{"linux", "amd64", "linux", "x64", []string{"manylinux2014_x86_64", "manylinux_2_17_x86_64", "musllinux_1_1_x86_64"}},
	{"windows", "arm64", "win32", "arm64", []string{"win_arm64"}},
	{"windows", "amd64", "win32", "x64", []string{"win_amd64"}},
}

// TargetFor returns the target of a Go os/arch pair.
func TargetFor(goos, goarch string) (Target, bool) {
	i := slices.IndexFunc(Targets, func(t Target) bool { return t.GOOS == goos && t.GOARCH == goarch })
	if i < 0 {
		return Target{}, false
	}

	return Targets[i], true
}

func (t Target) String() string { return t.GOOS + "/" + t.GOARCH }

// Binary is the file name of the executable.
func (t Target) Binary() string {
	if t.GOOS == "windows" {
		return BinaryName + ".exe"
	}

	return BinaryName
}

// NpmPackage is the name of the platform package.
func (t Target) NpmPackage() string { return NpmRoot + "-" + t.NpmOS + "-" + t.CPU }

// GemBinary is the path of the binary inside the gem.
func (t Target) GemBinary() string {
	return "libexec/" + BinaryName + "-" + t.GOOS + "-" + t.CPU + "/" + t.Binary()
}

// WheelPlatform is the compressed platform tag of the wheel file name.
func (t Target) WheelPlatform() string { return strings.Join(t.Wheel, ".") }

// NpmFile is the tarball `npm pack` writes for a package at a version.
func NpmFile(pkg, version string) string {
	return strings.ReplaceAll(strings.TrimPrefix(pkg, "@"), "/", "-") + "-" + version + ".tgz"
}

// WheelFile is the file name of the target's wheel at a PEP 440 version.
func (t Target) WheelFile(pep440 string) string {
	return BinaryName + "-" + pep440 + "-py3-none-" + t.WheelPlatform() + ".whl"
}

// GemFile is the file name of the gem at a RubyGems version.
func GemFile(gemVersion string) string { return BinaryName + "-" + gemVersion + ".gem" }
