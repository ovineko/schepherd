package wrappers

import (
	"bytes"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// DistBinary is the GoReleaser binary of one target.
type DistBinary struct {
	Target Target
	Path   string
}

// DistBinaries reads the binaries of BuildID from <dist>/artifacts.json in
// the order of Targets, after CheckBinary accepted each. allTargets requires
// every target; otherwise the single-target builds of CI smoke tests give the
// targets they have.
func DistBinaries(dist string, allTargets bool) ([]DistBinary, error) {
	data, err := os.ReadFile(filepath.Clean(filepath.Join(dist, "artifacts.json")))
	if err != nil {
		return nil, fmt.Errorf("read GoReleaser artifacts: %w", err)
	}

	var artifacts []struct {
		Extra  map[string]any `json:"extra"`
		Name   string         `json:"name"`
		Path   string         `json:"path"`
		GOOS   string         `json:"goos"`
		GOARCH string         `json:"goarch"`
		Type   string         `json:"type"`
	}

	if err := json.Unmarshal(data, &artifacts); err != nil {
		return nil, fmt.Errorf("parse artifacts.json: %w", err)
	}

	found := map[string]string{}

	for _, a := range artifacts {
		if a.Type != "Binary" || a.Extra["ID"] != BuildID {
			continue
		}

		t, ok := TargetFor(a.GOOS, a.GOARCH)

		switch _, dup := found[t.String()]; {
		case !ok:
			return nil, fmt.Errorf("artifacts.json: %s/%s is not a release target", a.GOOS, a.GOARCH)
		case a.Name != t.Binary() || dup:
			return nil, fmt.Errorf("artifacts.json: unexpected %s binary %q", t, a.Name)
		}

		// GoReleaser records paths relative to where it ran (dist/<dir>/<name>),
		// so a dist directory that was moved or downloaded still resolves.
		recorded := filepath.Clean(a.Path)
		_, rest, cut := strings.Cut(filepath.ToSlash(recorded), "/")

		if !filepath.IsLocal(recorded) || !cut || !filepath.IsLocal(filepath.FromSlash(rest)) {
			return nil, fmt.Errorf("artifacts.json: unexpected artifact path %q", a.Path)
		}

		found[t.String()] = filepath.Join(dist, filepath.FromSlash(rest))
	}

	var binaries []DistBinary

	for _, t := range Targets {
		path, ok := found[t.String()]
		if !ok && allTargets {
			return nil, fmt.Errorf("dist has no %s binary; every release target is required", t)
		}

		if !ok {
			continue
		}

		data, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return nil, fmt.Errorf("read the %s binary: %w", t, err)
		}

		if err := CheckBinary(t, data); err != nil {
			return nil, fmt.Errorf("the %s binary %s: %w", t, path, err)
		}

		binaries = append(binaries, DistBinary{Target: t, Path: path})
	}

	if len(binaries) == 0 {
		return nil, fmt.Errorf("artifacts.json lists no binary of build %q", BuildID)
	}

	return binaries, nil
}

// SourceDateEpoch is the modification time GoReleaser gave every binary
// (mod_timestamp: the commit time), which the builders record in the
// packages so the same commit gives the same bytes.
func SourceDateEpoch(binaries []DistBinary) (int64, error) {
	var epoch int64

	for _, b := range binaries {
		info, err := os.Stat(b.Path)
		if err != nil {
			return 0, fmt.Errorf("binary: %w", err)
		}

		if mtime := info.ModTime().Unix(); epoch == 0 {
			epoch = mtime
		} else if mtime != epoch {
			return 0, errors.New("the binaries have different modification times")
		}
	}

	if epoch <= 0 {
		return 0, errors.New("the binaries have no usable modification time")
	}

	return epoch, nil
}

// Mach-O LC_BUILD_VERSION: cmd, cmdsize, platform, minos, ... as uint32; minos
// encodes X.Y.Z as xxxx.yy.zz.
const (
	loadCmdBuildVersion = 0x32
	platformMacOS       = 1
)

// CheckBinary confirms that data is an executable of the target's format and
// CPU that its wheel tags describe truthfully: static on Linux, where the
// manylinux and musllinux tags promise no shared libraries, and requiring
// exactly the macOS version of the macosx tag.
func CheckBinary(t Target, data []byte) error {
	r := bytes.NewReader(data)

	switch t.GOOS {
	case "linux":
		f, err := elf.NewFile(r)
		if err != nil {
			return fmt.Errorf("not an ELF executable: %w", err)
		}

		machine := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[t.GOARCH]
		if f.Class != elf.ELFCLASS64 || f.Machine != machine || f.Type != elf.ET_EXEC {
			return fmt.Errorf("ELF %s %s %s, want a 64-bit %s executable", f.Class, f.Type, f.Machine, machine)
		}

		libs, err := f.ImportedLibraries()
		if err != nil || len(libs) > 0 || slices.ContainsFunc(f.Progs, func(p *elf.Prog) bool { return p.Type == elf.PT_INTERP }) {
			return errors.New("dynamically linked; the Linux wheels need a static binary (CGO_ENABLED=0)")
		}
	case "darwin":
		f, err := macho.NewFile(r)
		if err != nil {
			return fmt.Errorf("not a Mach-O executable: %w", err)
		}

		cpu := map[string]macho.Cpu{"amd64": macho.CpuAmd64, "arm64": macho.CpuArm64}[t.GOARCH]
		if f.Magic != macho.Magic64 || f.Cpu != cpu || f.Type != macho.TypeExec {
			return fmt.Errorf("Mach-O %s type %d, want a 64-bit %s executable", f.Cpu, f.Type, cpu)
		}

		return checkMacOS(t, f)
	case "windows":
		f, err := pe.NewFile(r)
		if err != nil {
			return fmt.Errorf("not a PE executable: %w", err)
		}

		machine := map[string]uint16{"amd64": pe.IMAGE_FILE_MACHINE_AMD64, "arm64": pe.IMAGE_FILE_MACHINE_ARM64}[t.GOARCH]
		if f.Machine != machine || f.Characteristics&pe.IMAGE_FILE_EXECUTABLE_IMAGE == 0 {
			return fmt.Errorf("PE machine %#x with characteristics %#x, want an executable image for machine %#x", f.Machine, f.Characteristics, machine)
		}
	default:
		return fmt.Errorf("%s is not a release target", t)
	}

	return nil
}

func checkMacOS(t Target, f *macho.File) error {
	parts := strings.Split(t.WheelPlatform(), "_")
	want := parts[1] + "." + parts[2]

	for _, l := range f.Loads {
		raw := l.Raw()
		if len(raw) < 16 || f.ByteOrder.Uint32(raw) != loadCmdBuildVersion || f.ByteOrder.Uint32(raw[8:]) != platformMacOS {
			continue
		}

		minos := f.ByteOrder.Uint32(raw[12:])
		if got := strconv.Itoa(int(minos>>16)) + "." + strconv.Itoa(int(minos>>8&0xff)); got != want {
			return fmt.Errorf("the binary requires macOS %s, but the platform tag %s promises macOS %s; "+
				"update the macOS tags of wrappers.Targets and the documented requirement", got, t.WheelPlatform(), want)
		}

		return nil
	}

	return errors.New("no LC_BUILD_VERSION load command for macOS")
}
