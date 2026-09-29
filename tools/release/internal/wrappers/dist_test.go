package wrappers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckBinary(t *testing.T) {
	for _, target := range Targets {
		if err := CheckBinary(target, fake{goos: target.GOOS, goarch: target.GOARCH}.bytes()); err != nil {
			t.Errorf("%s: %v", target, err)
		}
	}

	cases := []struct {
		goos, goarch string
		data         fake
		want         string
	}{
		{"linux", "amd64", fake{goos: "linux", goarch: "amd64", interpreter: "/lib64/ld-linux-x86-64.so.2"}, "static binary"},
		{"linux", "arm64", fake{goos: "linux", goarch: "amd64"}, "want a 64-bit EM_AARCH64 executable"},
		{"linux", "amd64", fake{goos: "darwin", goarch: "amd64"}, "not an ELF executable"},
		{"darwin", "amd64", fake{goos: "darwin", goarch: "arm64"}, "want a 64-bit CpuAmd64 executable"},
		{"darwin", "arm64", fake{goos: "darwin", goarch: "arm64", macOS: 12}, "requires macOS 12.0, but the platform tag macosx_13_0_arm64 promises macOS 13.0"},
		{"darwin", "arm64", fake{goos: "linux", goarch: "arm64"}, "not a Mach-O executable"},
		{"windows", "arm64", fake{goos: "windows", goarch: "amd64"}, "want an executable image for machine 0xaa64"},
		{"windows", "amd64", fake{goos: "linux", goarch: "amd64"}, "not a PE executable"},
	}

	for _, c := range cases {
		target, _ := TargetFor(c.goos, c.goarch)
		if err := CheckBinary(target, c.data.bytes()); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s/%s: %v, want an error containing %q", c.goos, c.goarch, err, c.want)
		}
	}

	if err := CheckBinary(Target{GOOS: "freebsd", GOARCH: "amd64"}, fake{goos: "linux", goarch: "amd64"}.bytes()); err == nil {
		t.Error("a binary of another target passed")
	}
}

func TestDistBinaries(t *testing.T) {
	dist := fakeDist(t, allFakes()...)

	binaries, err := DistBinaries(dist, true)
	if err != nil || len(binaries) != len(Targets) {
		t.Fatalf("DistBinaries = %v, %v", binaries, err)
	}

	for i, b := range binaries {
		if b.Target.String() != Targets[i].String() || b.Path != filepath.Join(dist, "schepherd_"+b.Target.GOOS+"_"+b.Target.GOARCH, b.Target.Binary()) {
			t.Errorf("binary %d is %+v", i, b)
		}
	}

	single := fakeDist(t, fake{goos: "linux", goarch: "arm64"})
	if _, err := DistBinaries(single, true); err == nil || !strings.Contains(err.Error(), "every release target is required") {
		t.Errorf("a single target with allTargets: %v", err)
	}

	if b, err := DistBinaries(single, false); err != nil || len(b) != 1 || b[0].Target.GOARCH != "arm64" {
		t.Errorf("a single target: %v, %v", b, err)
	}

	broken := fakeDist(t, fake{goos: "linux", goarch: "amd64", interpreter: "/lib/ld.so"})
	if _, err := DistBinaries(broken, false); err == nil || !strings.Contains(err.Error(), "static binary") {
		t.Errorf("a dynamic binary: %v", err)
	}

	for name, a := range map[string]artifact{
		"is not a release target": {Name: "schepherd", Path: "dist/x/schepherd", GOOS: "freebsd", GOARCH: "amd64"},
		"unexpected linux/amd64":  {Name: "schepherd.exe", Path: "dist/x/schepherd.exe", GOOS: "linux", GOARCH: "amd64"},
		"unexpected artifact":     {Name: "schepherd", Path: "dist/../../etc/schepherd", GOOS: "linux", GOARCH: "amd64"},
	} {
		a.Type, a.Extra = "Binary", map[string]any{"ID": "schepherd"}

		data, err := json.Marshal([]artifact{a})
		if err != nil {
			t.Fatal(err)
		}

		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "artifacts.json"), data)

		if _, err := DistBinaries(dir, false); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%+v: %v, want %q", a, err, name)
		}
	}

	if _, err := DistBinaries(t.TempDir(), false); err == nil {
		t.Error("a dist without artifacts.json passed")
	}
}

func TestSourceDateEpoch(t *testing.T) {
	binaries, err := DistBinaries(fakeDist(t, fake{goos: "linux", goarch: "amd64"}, fake{goos: "linux", goarch: "arm64"}), false)
	if err != nil {
		t.Fatal(err)
	}

	commit := time.Unix(1790000000, 0)
	for _, b := range binaries {
		if err := os.Chtimes(b.Path, commit, commit); err != nil {
			t.Fatal(err)
		}
	}

	if epoch, err := SourceDateEpoch(binaries); err != nil || epoch != commit.Unix() {
		t.Errorf("SourceDateEpoch = %d, %v", epoch, err)
	}

	if err := os.Chtimes(binaries[1].Path, commit, commit.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := SourceDateEpoch(binaries); err == nil {
		t.Error("binaries with different times gave an epoch")
	}
}

func touch(t *testing.T, name string, unix int64) {
	t.Helper()

	if err := os.Chtimes(name, time.Unix(unix, 0), time.Unix(unix, 0)); err != nil {
		t.Fatal(err)
	}
}
