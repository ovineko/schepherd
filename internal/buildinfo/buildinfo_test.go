package buildinfo

import (
	"encoding/json"
	"runtime"
	"runtime/debug"
	"testing"
)

func inject(t *testing.T, injected, rev, date string) {
	t.Helper()

	saved := [3]string{version, commit, commitDate}
	version, commit, commitDate = injected, rev, date

	t.Cleanup(func() { version, commit, commitDate = saved[0], saved[1], saved[2] })
}

func TestReleaseBuild(t *testing.T) {
	for _, injected := range []string{"0.1.0", "0.2.0-rc.1", "1.0.0", "10.20.30-beta.11"} {
		t.Run(injected, func(t *testing.T) {
			inject(t, injected, "0123456789abcdef0123456789abcdef01234567", "2026-09-23T10:11:12Z")

			info := Get()
			want := Info{
				Version:    injected,
				Commit:     "0123456789abcdef0123456789abcdef01234567",
				CommitDate: "2026-09-23T10:11:12Z",
				GoVersion:  runtime.Version(),
				Release:    true,
			}

			if info != want {
				t.Errorf("Get() = %+v, want %+v", info, want)
			}
		})
	}
}

func TestNonReleaseVersionsReportDev(t *testing.T) {
	for _, injected := range []string{
		"",
		"v0.1.0",
		"0.1",
		"01.0.0",
		"0.1.0-rc.01",
		"0.1.0+dirty",
		"0.0.0",
		"0.0.0-snapshot-0123456789ab",
		"0.1.1-0.20260925080000-0123456789ab",
		"20260923.1",
		"0.20260923.1-SNAPSHOT-0123456+x",
		" 0.1.0",
	} {
		t.Run(injected, func(t *testing.T) {
			inject(t, injected, "abc", "2026-09-23T10:11:12Z")

			info := Get()
			if info.Version != DevVersion || info.Release {
				t.Errorf("Get() with the injected version %q = %+v, want a dev build", injected, info)
			}

			if info.Commit != "abc" || info.CommitDate != "2026-09-23T10:11:12Z" {
				t.Errorf("injected commit metadata was not kept: %+v", info)
			}

			if info.GoVersion != runtime.Version() {
				t.Errorf("GoVersion = %q, want %q", info.GoVersion, runtime.Version())
			}
		})
	}
}

// TestJSONFields pins the members of version --json that come from this
// package; the scheme has no second version form.
func TestJSONFields(t *testing.T) {
	data, err := json.Marshal(Info{Version: "0.1.0", Commit: "c", CommitDate: "d", GoVersion: "go1.27.1", Release: true})
	if err != nil {
		t.Fatal(err)
	}

	var members map[string]any
	if err := json.Unmarshal(data, &members); err != nil {
		t.Fatal(err)
	}

	want := map[string]any{"version": "0.1.0", "commit": "c", "commitDate": "d", "goVersion": "go1.27.1", "release": true}
	if len(members) != len(want) {
		t.Fatalf("Info encodes as %s, want exactly the members %v", data, want)
	}

	for k, v := range want {
		if members[k] != v {
			t.Errorf("member %s = %v, want %v (%s)", k, members[k], v, data)
		}
	}
}

func moduleBuild(version string) *debug.BuildInfo {
	return &debug.BuildInfo{
		GoVersion: "go1.27.1",
		Main:      debug.Module{Path: "github.com/ovineko/schepherd", Version: version},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"},
			{Key: "vcs.time", Value: "2026-09-24T12:00:00Z"},
		},
	}
}

func TestModuleVersionOfAReleaseTag(t *testing.T) {
	for tag, want := range map[string]string{"v0.1.0": "0.1.0", "v0.2.0-rc.1": "0.2.0-rc.1", "v1.4.2": "1.4.2"} {
		got := fromBuildInfo(Info{Version: DevVersion}, moduleBuild(tag), false)
		info := Info{
			Version:    want,
			Commit:     "0123456789abcdef0123456789abcdef01234567",
			CommitDate: "2026-09-24T12:00:00Z",
			GoVersion:  "go1.27.1",
			Release:    true,
		}

		if got != info {
			t.Errorf("fromBuildInfo(%s) = %+v, want %+v", tag, got, info)
		}
	}
}

func TestOtherModuleVersionsReportDev(t *testing.T) {
	for _, moduleVersion := range []string{
		"",
		"(devel)",
		"0.1.0",
		"v0.0.0-20260924120000-0123456789ab",
		"v0.1.1-0.20260925080000-0123456789ab",
		"v0.2.0-rc.1.0.20260925080000-0123456789ab",
		"v0.1.0+dirty",
		"v2.0.0+incompatible",
		"v0.1",
		"v0.01.0",
		"v0.20260924.03",
	} {
		t.Run(moduleVersion, func(t *testing.T) {
			got := fromBuildInfo(Info{Version: DevVersion}, moduleBuild(moduleVersion), false)
			if got.Version != DevVersion || got.Release {
				t.Errorf("module version %q gives %+v, want a dev build", moduleVersion, got)
			}

			if got.GoVersion != "go1.27.1" || got.Commit == "" || got.CommitDate == "" {
				t.Errorf("module version %q: the recorded build metadata was not kept: %+v", moduleVersion, got)
			}
		})
	}
}

func TestInjectedVersionWinsOverTheModuleVersion(t *testing.T) {
	injected := Info{
		Version:    "0.1.0",
		Commit:     "89abcdef0123456789abcdef0123456789abcdef",
		CommitDate: "2026-09-23T10:11:12Z",
		Release:    true,
	}

	got := fromBuildInfo(injected, moduleBuild("v0.2.0"), true)

	want := injected
	want.GoVersion = "go1.27.1"

	if got != want {
		t.Errorf("fromBuildInfo = %+v, want the injected release %+v", got, want)
	}
}

// TestInjectedSnapshotIgnoresTheModuleVersion covers a GoReleaser snapshot
// of a commit that carries a release tag: the go command records the tag as
// the module version, but the injected snapshot version decides, so the
// binary reports "dev" like the 0.0.0-snapshot packages built from it. A
// build without an injected version still takes the tag.
func TestInjectedSnapshotIgnoresTheModuleVersion(t *testing.T) {
	for _, injected := range []string{"0.0.0-snapshot-0123456", "0.0.0-snapshot-89abcde", "20260923.1"} {
		got := describe(injected, "abc", "", moduleBuild("v0.1.0"))
		if got.Version != DevVersion || got.Release || got.Commit != "abc" || got.GoVersion != "go1.27.1" {
			t.Errorf("a build injected with %q of a commit tagged v0.1.0 reports %+v, want a dev build", injected, got)
		}
	}

	if got := describe("", "", "", moduleBuild("v0.1.0")); got.Version != "0.1.0" || !got.Release {
		t.Errorf("a build of v0.1.0 without an injected version reports %+v, want release 0.1.0", got)
	}

	if got := describe("0.2.0", "", "", nil); got.Version != "0.2.0" || !got.Release || got.GoVersion != "" {
		t.Errorf("a release build without build information reports %+v", got)
	}
}
