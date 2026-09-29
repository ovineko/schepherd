// Package buildinfo exposes the values injected at link time by release
// builds and what the go command recorded about the build. Every consumer
// reads them through the accessors below; nothing else references the raw
// ldflag variables.
package buildinfo

import (
	"runtime/debug"

	"github.com/ovineko/schepherd/internal/buildinfo/semver"
)

// Set by release builds through
// -X github.com/ovineko/schepherd/internal/buildinfo.<name>=<value>.
var (
	version    = ""
	commit     = ""
	commitDate = ""
)

// DevVersion is reported by builds that were not produced from a release tag.
const DevVersion = "dev"

// Info describes the running binary.
type Info struct {
	Version    string `json:"version"`
	Commit     string `json:"commit,omitempty"`
	CommitDate string `json:"commitDate,omitempty"`
	GoVersion  string `json:"goVersion"`
	Release    bool   `json:"release"`
}

// Get returns the build information. A binary is a release build when a
// release version X.Y.Z[-prerelease] was injected at link time (GoReleaser
// injects the version of the release tag without its "v"), or, when nothing
// was injected, when the go command recorded a release tag
// vX.Y.Z[-prerelease] as the main module version (go install ...@vX.Y.Z).
// Anything else, including the 0.0.0 snapshot versions of CI builds, reports
// "dev" together with whatever VCS metadata the Go toolchain recorded.
func Get() Info {
	bi, _ := debug.ReadBuildInfo()

	return describe(version, commit, commitDate, bi)
}

// describe is Get for the given link-time values and build information,
// which is nil when the binary has none.
func describe(injected, rev, date string, bi *debug.BuildInfo) Info {
	info := Info{Version: DevVersion, Commit: rev, CommitDate: date}

	if v, err := semver.ParseRelease(injected); err == nil {
		info.Version, info.Release = v.String(), true
	}

	if bi != nil {
		info = fromBuildInfo(info, bi, injected != "")
	}

	return info
}

// fromBuildInfo completes info from what the go command recorded. A module
// build of a release tag (go install ...@vX.Y.Z, or go build in a clean
// checkout of that tag) records the tag as the main module version;
// pseudo-versions, "(devel)" and "+dirty" versions are never release tags and
// stay "dev". A version injected at link time is authoritative even when it
// is not a release: a GoReleaser snapshot of a tagged commit records that tag
// as its module version too, and must still report "dev" like the
// 0.0.0-snapshot npm packages built from it.
func fromBuildInfo(info Info, bi *debug.BuildInfo, injected bool) Info {
	info.GoVersion = bi.GoVersion

	if !injected {
		if v, err := semver.ParseReleaseTag(bi.Main.Version); err == nil {
			info.Version, info.Release = v.String(), true
		}
	}

	if info.Commit == "" {
		info.Commit = vcsSetting(bi, "vcs.revision")
	}

	if info.CommitDate == "" {
		info.CommitDate = vcsSetting(bi, "vcs.time")
	}

	return info
}

func vcsSetting(bi *debug.BuildInfo, key string) string {
	for _, s := range bi.Settings {
		if s.Key == key {
			return s.Value
		}
	}

	return ""
}
