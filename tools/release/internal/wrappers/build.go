package wrappers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Package kinds, in the order Build builds them.
const (
	Npm  = "npm"
	PyPI = "pypi"
	Gem  = "gem"
)

// Kinds lists every package kind.
var Kinds = []string{Npm, PyPI, Gem}

// BuildOptions configures Build.
type BuildOptions struct {
	// Root is the repository root: packaging/, LICENSE and
	// THIRD_PARTY_LICENSES.txt.
	Root string
	// Dist is the GoReleaser dist directory containing artifacts.json.
	Dist    string
	Version string
	// Out must not exist or be empty. It receives <kind>/<packages> and
	// schepherd_<version>_<kind>_checksums.txt, which lists them in
	// publication order, for each kind.
	Out   string
	Kinds []string
	// AllTargets requires a binary for every entry of Targets.
	AllTargets bool
	// Smoke installs the packages of the running platform offline and
	// compares each launcher with the binary.
	Smoke bool
	// RubyImage, when set, builds the gem in that container instead of with
	// the Ruby on PATH.
	RubyImage string
	// Env is the environment of the builders.
	Env []string
}

type builder struct {
	BuildOptions

	v        Versions
	binaries []DistBinary
	epoch    int64
	tmp      string
}

// Build builds the packages of opts.Kinds with the standard builders, checks
// that each holds exactly the files staged for it, writes their checksum
// files and, with opts.Smoke, smoke-tests them. It returns the paths of the
// packages.
func Build(ctx context.Context, opts BuildOptions) ([]string, error) {
	b := builder{BuildOptions: opts}

	var err error

	// The builders run in the staging directories.
	if b.Root, err = filepath.Abs(opts.Root); err != nil {
		return nil, fmt.Errorf("resolve the repository root: %w", err)
	}

	if b.Out, err = filepath.Abs(opts.Out); err != nil {
		return nil, fmt.Errorf("resolve --out: %w", err)
	}

	if b.v, err = VersionsOf(opts.Version); err != nil {
		return nil, err
	}

	if b.binaries, err = DistBinaries(opts.Dist, opts.AllTargets); err != nil {
		return nil, err
	}

	if b.epoch, err = SourceDateEpoch(b.binaries); err != nil {
		return nil, err
	}

	if entries, err := os.ReadDir(b.Out); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("--out %s is not empty", opts.Out)
	}

	if b.tmp, err = os.MkdirTemp("", "schepherd-packages-*"); err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}

	defer func() { _ = os.RemoveAll(b.tmp) }()

	// Docker mounts the staging directory by its real path.
	if b.tmp, err = filepath.EvalSymlinks(b.tmp); err != nil {
		return nil, fmt.Errorf("resolve staging directory: %w", err)
	}

	var all []string

	for _, kind := range Kinds {
		if !slices.Contains(opts.Kinds, kind) {
			continue
		}

		dir := filepath.Join(b.Out, kind)
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: release outputs are world-readable
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}

		build := map[string]func(context.Context, string) ([]string, error){Npm: b.npm, PyPI: b.pypi, Gem: b.gem}[kind]

		files, err := build(ctx, dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", kind, err)
		}

		var sums strings.Builder

		for _, f := range files {
			data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, f)))
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", f, err)
			}

			fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(data), f)
			all = append(all, filepath.Join(dir, f))
		}

		if err := os.WriteFile(filepath.Join(b.Out, BinaryName+"_"+opts.Version+"_"+kind+"_checksums.txt"), []byte(sums.String()), 0o644); err != nil { //nolint:gosec // G306: world-readable
			return nil, fmt.Errorf("write checksums: %w", err)
		}
	}

	if opts.Smoke {
		return all, b.smoke(ctx)
	}

	return all, nil
}

// stage is a directory holding exactly the files of one package.
type stage struct {
	dir   string
	files map[string]member
}

func (b *builder) stage(name string) *stage {
	return &stage{dir: filepath.Join(b.tmp, name), files: map[string]member{}}
}

// put writes a file with the mode the package records: 0755 for
// executables, 0644 otherwise, whatever the umask.
func (s *stage) put(rel string, data []byte, exec bool) error {
	name := filepath.Clean(filepath.Join(s.dir, filepath.FromSlash(rel)))
	mode := os.FileMode(0o644)

	if exec {
		mode = 0o755
	}

	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil { //nolint:gosec // G301: package sources are world-readable
		return fmt.Errorf("stage %s: %w", rel, err)
	}

	//nolint:gosec // G703: rel is a fixed package path below the staging directory of this package
	if err := errors.Join(os.WriteFile(name, data, mode), os.Chmod(name, mode)); err != nil {
		return fmt.Errorf("stage %s: %w", rel, err)
	}

	info, err := os.Stat(name)
	if err != nil {
		return fmt.Errorf("stage %s: %w", rel, err)
	}

	// Windows records no execute bit, so neither does a package built there.
	s.files[rel] = member{sum: sha256.Sum256(data), exec: info.Mode()&0o100 != 0}

	return nil
}

// copy stages files of the repository, relative to root, under dst.
func (s *stage) copy(root, dst string, exec bool, srcs ...string) error {
	for _, src := range srcs {
		data, err := os.ReadFile(filepath.Clean(filepath.Join(root, filepath.FromSlash(src))))
		if err != nil {
			return fmt.Errorf("package source: %w", err)
		}

		if err := s.put(path.Join(dst, path.Base(src)), data, exec); err != nil {
			return err
		}
	}

	return nil
}

// sources returns the repository files a pattern relative to root matches,
// relative to root.
func sources(root, pattern string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
	if err != nil {
		return nil, fmt.Errorf("package sources: %w", err)
	}

	if len(matches) == 0 {
		return nil, fmt.Errorf("no package sources match %s", pattern)
	}

	for i, m := range matches {
		matches[i] = path.Join(path.Dir(pattern), filepath.Base(m))
	}

	return matches, nil
}

func (s *stage) licenses(root string) error {
	return s.copy(root, "", false, "LICENSE", ThirdPartyLicenses)
}

func (s *stage) binary(rel string, bin DistBinary) error {
	data, err := os.ReadFile(bin.Path)
	if err != nil {
		return fmt.Errorf("read the %s binary: %w", bin.Target, err)
	}

	return s.put(rel, data, bin.Target.GOOS != "windows")
}

type npmManifest struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Description          string            `json:"description"`
	Homepage             string            `json:"homepage"`
	License              string            `json:"license"`
	Repository           map[string]string `json:"repository"`
	Bin                  map[string]string `json:"bin,omitempty"`
	OS                   []string          `json:"os,omitempty"`
	CPU                  []string          `json:"cpu,omitempty"`
	Engines              map[string]string `json:"engines,omitempty"`
	Type                 string            `json:"type,omitempty"`
	PreferUnplugged      bool              `json:"preferUnplugged,omitempty"`
	OptionalDependencies map[string]string `json:"optionalDependencies,omitempty"`
}

// npm packs one platform package per binary and then the launcher package,
// which depends on them and is therefore published last.
func (b *builder) npm(ctx context.Context, out string) ([]string, error) {
	base := npmManifest{
		Version: b.v.SemVer, Homepage: Homepage, License: "MIT",
		Repository: map[string]string{"type": "git", "url": "git+https://github.com/ovineko/schepherd.git"},
	}

	var files []string

	root := base
	root.Name, root.Type, root.OptionalDependencies = NpmRoot, "commonjs", map[string]string{}
	root.Description = "Delivers pinned JSON Schemas from an OCI catalog to verified local files. Installs the native schepherd binary for your platform."
	root.Bin, root.Engines = map[string]string{BinaryName: "bin/schepherd.js"}, map[string]string{"node": ">=18"}

	for _, bin := range b.binaries {
		t := bin.Target
		m := base
		m.Name, m.OS, m.CPU, m.PreferUnplugged = t.NpmPackage(), []string{t.NpmOS}, []string{t.CPU}, true
		m.Description = "The schepherd native binary for " + t.NpmOS + "-" + t.CPU + ". Install " + NpmRoot + " instead; npm selects this package automatically."
		root.OptionalDependencies[m.Name] = b.v.SemVer

		readme := "# " + m.Name + "\n\nThe native `schepherd` binary for " + t.NpmOS + "-" + t.CPU + ", with the license texts of the\n" +
			"third-party code linked into it (" + ThirdPartyLicenses + ") and nothing else.\n\n" +
			"Do not install this package directly: install [`" + NpmRoot + "`](https://www.npmjs.com/package/" + NpmRoot + "),\n" +
			"which lists every platform package as an optional dependency, so npm installs only the one\n" +
			"that matches your system. Documentation: " + Homepage + "\n"

		s := b.stage("npm-" + t.NpmOS + "-" + t.CPU)
		if err := errors.Join(s.licenses(b.Root), s.binary("bin/"+t.Binary(), bin), s.put("README.md", []byte(readme), false)); err != nil {
			return nil, err
		}

		file, err := b.npmPack(ctx, s, m, out)
		if err != nil {
			return nil, err
		}

		files = append(files, file)
	}

	src := "packaging/npm/schepherd"

	libs, err := sources(b.Root, src+"/lib/*.js")
	if err != nil {
		return nil, err
	}

	s := b.stage("npm-root")
	if err := errors.Join(s.copy(b.Root, "", false, "LICENSE", src+"/README.md"), s.copy(b.Root, "bin", true, src+"/bin/schepherd.js"),
		s.copy(b.Root, "lib", false, libs...)); err != nil {
		return nil, err
	}

	file, err := b.npmPack(ctx, s, root, out)
	if err != nil {
		return nil, err
	}

	return append(files, file), nil
}

func (b *builder) npmPack(ctx context.Context, s *stage, m npmManifest, out string) (string, error) {
	var manifest bytes.Buffer

	enc := json.NewEncoder(&manifest)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")

	if err := enc.Encode(m); err != nil {
		return "", fmt.Errorf("package.json of %s: %w", m.Name, err)
	}

	if err := s.put("package.json", manifest.Bytes(), false); err != nil {
		return "", err
	}

	dest, err := filepath.Abs(out)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", out, err)
	}

	stdout, err := Runner{Dir: s.dir, Env: b.Env}.Must(ctx, nil, "npm", "pack", "--json", "--ignore-scripts", "--pack-destination", dest)
	if err != nil {
		return "", err
	}

	var packed []struct {
		Filename string `json:"filename"`
	}

	if err := json.Unmarshal(stdout, &packed); err != nil {
		return "", fmt.Errorf("npm pack of %s: %w", m.Name, err)
	}

	file := NpmFile(m.Name, m.Version)
	if len(packed) != 1 || packed[0].Filename != file {
		return "", fmt.Errorf("npm pack of %s wrote %+v, want %s", m.Name, packed, file)
	}

	return file, checkPackage(filepath.Join(out, file), s.files, nil)
}

// pypi builds one wheel per binary with hatchling, pinned with hashes by
// packaging/python/build-constraints.txt; the build hook of
// packaging/python/hatch_build.py gives it the target's platform tags.
func (b *builder) pypi(ctx context.Context, out string) ([]string, error) {
	src := "packaging/python"

	modules, err := sources(b.Root, src+"/schepherd/*.py")
	if err != nil {
		return nil, err
	}

	constraints := filepath.Join(b.Root, filepath.FromSlash(src), "build-constraints.txt")
	info := BinaryName + "-" + b.v.PEP440 + ".dist-info/"

	files := make([]string, 0, len(b.binaries))

	for _, bin := range b.binaries {
		t := bin.Target

		s := b.stage("pypi-" + t.GOOS + "-" + t.GOARCH)
		if err := errors.Join(s.licenses(b.Root), s.copy(b.Root, "", false, src+"/pyproject.toml", src+"/hatch_build.py", src+"/README.md"),
			s.copy(b.Root, "schepherd", false, modules...), s.binary("schepherd/bin/"+t.Binary(), bin)); err != nil {
			return nil, err
		}

		env := []string{"SOURCE_DATE_EPOCH=" + strconv.FormatInt(b.epoch, 10), "SCHEPHERD_VERSION=" + b.v.PEP440, "SCHEPHERD_WHEEL_PLATFORM=" + t.WheelPlatform()}
		if _, err := (Runner{Dir: s.dir, Env: b.Env}).Must(ctx, env, "uv", "build", "--wheel", "--no-create-gitignore", "--build-constraints", constraints,
			"--require-hashes", "--out-dir", out, s.dir); err != nil {
			return nil, err
		}

		// The wheel holds the launcher and the licenses under .dist-info, not
		// the build inputs; hatchling writes the rest of .dist-info.
		want := map[string]member{info + "licenses/LICENSE": s.files["LICENSE"], info + "licenses/" + ThirdPartyLicenses: s.files[ThirdPartyLicenses]}
		for rel, m := range s.files {
			if strings.HasPrefix(rel, "schepherd/") {
				want[rel] = m
			}
		}

		file := t.WheelFile(b.v.PEP440)
		generated := []string{info + "METADATA", info + "WHEEL", info + "RECORD", info + "entry_points.txt"}

		if err := checkPackage(filepath.Join(out, file), want, generated); err != nil {
			return nil, err
		}

		files = append(files, file)
	}

	return files, nil
}

// gem builds one gem with the binaries of every target under libexec/ from
// packaging/ruby/schepherd.gemspec.
func (b *builder) gem(ctx context.Context, out string) ([]string, error) {
	src := "packaging/ruby"

	s := b.stage("gem")
	if err := errors.Join(s.licenses(b.Root), s.copy(b.Root, "", false, src+"/README.md", src+"/schepherd.gemspec"),
		s.copy(b.Root, "bin", true, src+"/bin/schepherd"), s.copy(b.Root, "lib", false, src+"/lib/schepherd.rb")); err != nil {
		return nil, err
	}

	for _, bin := range b.binaries {
		if err := s.binary(bin.Target.GemBinary(), bin); err != nil {
			return nil, err
		}
	}

	ruby := Runner{Image: b.RubyImage, Dir: s.dir, Env: b.Env}

	if err := ruby.CheckToolchain(ctx); err != nil {
		return nil, err
	}

	built := filepath.Join(s.dir, "out.gem")
	env := []string{"SOURCE_DATE_EPOCH=" + strconv.FormatInt(b.epoch, 10), "SCHEPHERD_GEM_VERSION=" + b.v.Gem}

	if _, err := ruby.Must(ctx, env, "ruby", "-S", "gem", "build", "--strict", "--norc", "--output", built, "schepherd.gemspec"); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(filepath.Clean(built))
	if err != nil {
		return nil, fmt.Errorf("read the gem: %w", err)
	}

	file := GemFile(b.v.Gem)
	if err := os.WriteFile(filepath.Join(out, file), data, 0o644); err != nil { //nolint:gosec // G306: world-readable
		return nil, fmt.Errorf("write the gem: %w", err)
	}

	delete(s.files, "schepherd.gemspec")

	return []string{file}, checkPackage(filepath.Join(out, file), s.files, nil)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}
