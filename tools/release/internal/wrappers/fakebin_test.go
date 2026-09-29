package wrappers

import (
	"bytes"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// fake describes a minimal executable that debug/elf, debug/macho and
// debug/pe accept.
type fake struct {
	goos, goarch string
	// interpreter makes an ELF executable dynamically linked.
	interpreter string
	// macOS is the minimum macOS major version; zero means 13.
	macOS uint32
}

func (f fake) bytes() []byte {
	var buf bytes.Buffer

	write := func(v any) {
		if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
			panic(err)
		}
	}

	switch f.goos {
	case "linux":
		const headerSize, progSize = 64, 56

		progs := []elf.Prog64{{Type: uint32(elf.PT_LOAD), Flags: uint32(elf.PF_R | elf.PF_X), Align: 0x1000}}
		if f.interpreter != "" {
			progs = append(progs, elf.Prog64{Type: uint32(elf.PT_INTERP), Flags: uint32(elf.PF_R), Align: 1})
		}

		end := uint64(headerSize + progSize*len(progs))
		if f.interpreter != "" {
			progs[1].Off, progs[1].Filesz, progs[1].Memsz = end, uint64(len(f.interpreter)+1), uint64(len(f.interpreter)+1)
			end += uint64(len(f.interpreter) + 1)
		}

		progs[0].Filesz, progs[0].Memsz = end, end

		var ident [elf.EI_NIDENT]byte
		copy(ident[:], elf.ELFMAG)
		ident[elf.EI_CLASS], ident[elf.EI_DATA], ident[elf.EI_VERSION] = byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)
		machine := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[f.goarch]

		write(elf.Header64{
			Ident: ident, Type: uint16(elf.ET_EXEC), Machine: uint16(machine), Version: uint32(elf.EV_CURRENT),
			Phoff: headerSize, Ehsize: headerSize, Phentsize: progSize, Phnum: uint16(len(progs)), Shentsize: 64,
		})

		for _, p := range progs {
			write(p)
		}

		if f.interpreter != "" {
			buf.WriteString(f.interpreter + "\x00")
		}
	case "darwin":
		major := f.macOS
		if major == 0 {
			major = 13
		}

		cpu := map[string]macho.Cpu{"amd64": macho.CpuAmd64, "arm64": macho.CpuArm64}[f.goarch]
		write(macho.FileHeader{Magic: macho.Magic64, Cpu: cpu, Type: macho.TypeExec, Ncmd: 1, Cmdsz: 24})
		write(uint32(0))
		write([6]uint32{loadCmdBuildVersion, 24, platformMacOS, major << 16, 26 << 16, 0})
	case "windows":
		dos := make([]byte, 0x80)
		copy(dos, "MZ")
		binary.LittleEndian.PutUint32(dos[0x3c:], 0x80)
		buf.Write(dos)
		buf.WriteString("PE\x00\x00")
		write(pe.FileHeader{
			Machine:         map[string]uint16{"amd64": pe.IMAGE_FILE_MACHINE_AMD64, "arm64": pe.IMAGE_FILE_MACHINE_ARM64}[f.goarch],
			Characteristics: pe.IMAGE_FILE_EXECUTABLE_IMAGE | pe.IMAGE_FILE_LARGE_ADDRESS_AWARE,
		})
	}

	return buf.Bytes()
}

type artifact struct {
	Extra  map[string]any `json:"extra"`
	Name   string         `json:"name"`
	Path   string         `json:"path"`
	GOOS   string         `json:"goos"`
	GOARCH string         `json:"goarch"`
	Type   string         `json:"type"`
}

// fakeDist writes a GoReleaser dist directory, as if GoReleaser had run in
// another directory, whose artifacts.json lists the executables as binaries of
// the build "schepherd", next to an archive and a binary of another build.
func fakeDist(t *testing.T, fakes ...fake) string {
	t.Helper()

	dist := filepath.Join(t.TempDir(), "downloaded")
	artifacts := make([]artifact, 0, 2+len(fakes))
	artifacts = append(artifacts,
		artifact{Name: "schepherd_0.1.0_linux_amd64.tar.gz", Path: "dist/schepherd_0.1.0_linux_amd64.tar.gz", GOOS: "linux", GOARCH: "amd64", Type: "Archive", Extra: map[string]any{"ID": "default"}},
		artifact{Name: "other", Path: "dist/other_linux_amd64_v1/other", GOOS: "linux", GOARCH: "amd64", Type: "Binary", Extra: map[string]any{"ID": "other"}},
	)

	for _, f := range fakes {
		name, dir := "schepherd", "schepherd_"+f.goos+"_"+f.goarch
		if f.goos == "windows" {
			name += ".exe"
		}

		writeFile(t, filepath.Join(dist, dir, name), f.bytes())
		artifacts = append(artifacts, artifact{
			Name: name, Path: "dist/" + dir + "/" + name, GOOS: f.goos, GOARCH: f.goarch, Type: "Binary", Extra: map[string]any{"ID": "schepherd"},
		})
	}

	data, err := json.Marshal(artifacts)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(dist, "artifacts.json"), data)

	return dist
}

func allFakes() []fake {
	fakes := make([]fake, 0, len(Targets))
	for _, t := range Targets {
		fakes = append(fakes, fake{goos: t.GOOS, goarch: t.GOARCH})
	}

	return fakes
}

func writeFile(t *testing.T, name string, data []byte) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
