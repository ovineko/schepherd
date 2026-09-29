package wrappers

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const root = "../../../.."

func readRepo(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

// TestTargetsMatchTheNpmLauncher keeps TARGETS of lib/platform.js identical
// to Targets, in the same order.
func TestTargetsMatchTheNpmLauncher(t *testing.T) {
	script := "const p = require(process.argv[1]); process.stdout.write(JSON.stringify(p.TARGETS.map((t) => [p.packageName(t.platform, t.arch), p.binaryName(t.platform)])))"

	platformJS, err := filepath.Abs(filepath.Join(root, "packaging", "npm", "schepherd", "lib", "platform.js"))
	if err != nil {
		t.Fatal(err)
	}

	out, err := exec.CommandContext(t.Context(), "node", "-e", script, platformJS).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}

	var launcher [][2]string

	want := make([][2]string, 0, len(Targets))
	if err := json.Unmarshal(out, &launcher); err != nil {
		t.Fatal(err)
	}

	for _, target := range Targets {
		want = append(want, [2]string{target.NpmPackage(), target.Binary()})
	}

	if !slices.Equal(launcher, want) {
		t.Errorf("lib/platform.js targets %v, Targets %v", launcher, want)
	}
}

// TestTargetsMatchTheRubyLibrary keeps TARGETS of lib/schepherd.rb, which
// names the libexec directories, identical to Targets.
func TestTargetsMatchTheRubyLibrary(t *testing.T) {
	block := regexp.MustCompile(`(?s)TARGETS = \[(.*?)\]\.freeze`).FindStringSubmatch(readRepo(t, "packaging/ruby/lib/schepherd.rb"))
	if block == nil {
		t.Fatal("TARGETS not found in packaging/ruby/lib/schepherd.rb")
	}

	pairs := regexp.MustCompile(`%w\[(\w+) (\w+)\]`).FindAllStringSubmatch(block[1], -1)
	ruby, want := make([]string, 0, len(pairs)), make([]string, 0, len(Targets))

	for _, m := range pairs {
		ruby = append(ruby, "libexec/schepherd-"+m[1]+"-"+m[2]+"/schepherd")
	}

	for _, target := range Targets {
		want = append(want, strings.TrimSuffix(target.GemBinary(), ".exe"))
	}

	if !slices.Equal(ruby, want) {
		t.Errorf("lib/schepherd.rb TARGETS gives %v, Targets %v", ruby, want)
	}
}

func TestTargetsAreTheGoReleaserBuild(t *testing.T) {
	goreleaser := readRepo(t, ".goreleaser.yaml")

	for _, target := range Targets {
		for _, want := range []string{"      - " + target.GOOS + "\n", "      - " + target.GOARCH + "\n"} {
			if !strings.Contains(goreleaser, want) {
				t.Errorf(".goreleaser.yaml does not build %s", target)
			}
		}
	}

	if len(Targets) != 6 {
		t.Errorf("%d targets; .goreleaser.yaml builds 3 systems on 2 architectures", len(Targets))
	}
}

func TestWheelTags(t *testing.T) {
	suffix := map[string]map[string]string{
		"linux":   {"amd64": "_x86_64", "arm64": "_aarch64"},
		"darwin":  {"amd64": "_x86_64", "arm64": "_arm64"},
		"windows": {"amd64": "win_amd64", "arm64": "win_arm64"},
	}

	for _, target := range Targets {
		if len(target.Wheel) == 0 || !slices.IsSorted(target.Wheel) {
			t.Errorf("%s: platform tags %v must be sorted", target, target.Wheel)
		}

		for _, tag := range target.Wheel {
			if !strings.HasSuffix(tag, suffix[target.GOOS][target.GOARCH]) {
				t.Errorf("%s: platform tag %s names another architecture", target, tag)
			}
		}

		if p := target.WheelPlatform(); target.GOOS == "linux" && (!strings.Contains(p, "manylinux_") || !strings.Contains(p, "musllinux_")) {
			t.Errorf("%s: the static binary must install on glibc and musl: %v", target, target.Wheel)
		}
	}

	linux, _ := TargetFor("linux", "amd64")
	if got := linux.WheelFile("1.0.0rc1"); got != "schepherd-1.0.0rc1-py3-none-manylinux2014_x86_64.manylinux_2_17_x86_64.musllinux_1_1_x86_64.whl" {
		t.Errorf("WheelFile = %s", got)
	}

	if got := NpmFile(linux.NpmPackage(), "1.0.0-rc.1"); got != "ovineko-schepherd-linux-x64-1.0.0-rc.1.tgz" {
		t.Errorf("NpmFile = %s", got)
	}
}

func TestToolPinsAgree(t *testing.T) {
	pin := regexp.MustCompile(`(?m)^hatchling==(\S+)`)
	in := pin.FindStringSubmatch(readRepo(t, "packaging/python/build-requirements.in"))
	constraints := pin.FindStringSubmatch(readRepo(t, "packaging/python/build-constraints.txt"))

	if in == nil || constraints == nil || in[1] != constraints[1] ||
		!strings.Contains(readRepo(t, "packaging/python/pyproject.toml"), `requires = ["hatchling==`+in[1]+`"]`) {
		t.Errorf("hatchling pins differ: build-requirements.in %v, build-constraints.txt %v, or pyproject.toml; recompile the constraints", in, constraints)
	}
}
