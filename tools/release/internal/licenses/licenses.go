// Package licenses produces THIRD_PARTY_LICENSES.txt, the license and notice
// texts of everything linked into the schepherd client, and checks that file
// against the dependencies `go list -deps` reports for every release target.
//
// MIT and BSD licenses require their copyright notice and license text in
// binary redistributions, and Apache-2.0 requires a copy of the license and
// the NOTICE file, so every archive and npm platform package ships the
// generated file next to the binary.
package licenses

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ovineko/schepherd/tools/release/internal/gitx"
	"github.com/ovineko/schepherd/tools/release/internal/wrappers"
)

// Repository files the package reads and writes.
const (
	File          = wrappers.ThirdPartyLicenses
	ClientPackage = "./cmd/schepherd"
	Stdlib        = "Go standard library"
)

// licenseFilePrefixes select the files of a module or GOROOT root that carry
// its license, notices and patent grant; Go source files such as notice.go
// never do.
var licenseFilePrefixes = []string{"LICENSE", "LICENCE", "COPYING", "NOTICE", "PATENTS"}

// Target is one GOOS/GOARCH pair the client is released for.
type Target struct {
	GOOS   string
	GOARCH string
}

func (t Target) String() string {
	return t.GOOS + "/" + t.GOARCH
}

// ReleaseTargets are the targets GoReleaser builds and the packages carry.
func ReleaseTargets() []Target {
	targets := make([]Target, 0, len(wrappers.Targets))
	for _, t := range wrappers.Targets {
		targets = append(targets, Target{GOOS: t.GOOS, GOARCH: t.GOARCH})
	}

	return sortTargets(targets)
}

// Options configures Collect, Generate and Check.
type Options struct {
	// Root is the module root of the repository.
	Root string
	// Package is the main package whose dependencies are collected;
	// empty means ClientPackage.
	Package string
	// Targets are the builds to cover; empty means ReleaseTargets.
	Targets []Target
	// Env is the environment of the go command.
	Env []string
}

// Component is the Go standard library or one module linked into the client.
type Component struct {
	Name    string
	Version string
	Targets []Target
	Files   []LicenseFile
}

// LicenseFile is one license, notice or patent file of a component.
type LicenseFile struct {
	Name string
	Text string
}

type listedModule struct {
	Replace *listedModule `json:"Replace"`
	Path    string        `json:"Path"`
	Version string        `json:"Version"`
	Dir     string        `json:"Dir"`
	Main    bool          `json:"Main"`
}

type listedPackage struct {
	Module   *listedModule `json:"Module"`
	Standard bool          `json:"Standard"`
}

type modFile struct {
	Go        string `json:"Go"`
	Toolchain string `json:"Toolchain"`
}

type goEnv struct {
	GOROOT    string `json:"GOROOT"`
	GOVERSION string `json:"GOVERSION"`
}

// Collect lists the components linked into the package for every target,
// the Go standard library first, and reads their license files. Packages of
// the main module are Schepherd's own code under its LICENSE.
func Collect(ctx context.Context, opts Options) ([]Component, error) {
	opts = withDefaults(opts)

	environ, err := goEnviron(ctx, opts)
	if err != nil {
		return nil, err
	}

	var goenv goEnv
	if err := goJSON(ctx, opts.Root, environ, &goenv, "env", "-json", "GOROOT", "GOVERSION"); err != nil {
		return nil, err
	}

	if goenv.GOROOT == "" || goenv.GOVERSION == "" {
		return nil, errors.New("go env reports no GOROOT or GOVERSION")
	}

	stdlib := &Component{Name: Stdlib, Version: goenv.GOVERSION}
	modules := map[string]*Component{}
	dirs := map[string]string{}

	for _, target := range opts.Targets {
		packages, err := listDeps(ctx, opts, environ, target)
		if err != nil {
			return nil, err
		}

		for _, p := range packages {
			switch {
			case p.Standard:
				stdlib.addTarget(target)
			case p.Module != nil && !p.Module.Main:
				if err := addModule(modules, dirs, p.Module, target); err != nil {
					return nil, err
				}
			}
		}
	}

	if len(stdlib.Targets) == 0 {
		return nil, fmt.Errorf("go list reports no standard library packages for %s", opts.Package)
	}

	if stdlib.Files, err = readLicenseFiles(goenv.GOROOT); err != nil {
		return nil, fmt.Errorf("%s %s: %w", Stdlib, stdlib.Version, err)
	}

	components := []Component{*stdlib}

	for _, name := range slices.Sorted(maps.Keys(modules)) {
		c := modules[name]
		if c.Files, err = readLicenseFiles(dirs[name]); err != nil {
			return nil, fmt.Errorf("%s %s: %w", c.Name, c.Version, err)
		}

		components = append(components, *c)
	}

	return components, nil
}

func addModule(modules map[string]*Component, dirs map[string]string, m *listedModule, target Target) error {
	c, dir, err := moduleComponent(m)
	if err != nil {
		return err
	}

	if modules[c.Name] == nil {
		modules[c.Name] = &c
		dirs[c.Name] = dir
	}

	modules[c.Name].addTarget(target)

	return nil
}

// Generate renders the license file for the current dependencies.
func Generate(ctx context.Context, opts Options) ([]byte, error) {
	opts = withDefaults(opts)

	components, err := Collect(ctx, opts)
	if err != nil {
		return nil, err
	}

	return Render(components, opts.Targets), nil
}

// Check compares the committed license file with the current dependencies.
// It returns every mismatch; an error means the dependencies could not be
// determined.
func Check(ctx context.Context, opts Options) ([]string, error) {
	opts = withDefaults(opts)

	components, err := Collect(ctx, opts)
	if err != nil {
		return nil, err
	}

	var violations []string

	want := Render(components, opts.Targets)

	switch have, err := os.ReadFile(filepath.Join(opts.Root, File)); {
	case errors.Is(err, os.ErrNotExist):
		violations = append(violations, File+" is missing; run `go run ./tools/release licenses generate`")
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", File, err)
	case !bytes.Equal(normalizeNewlines(have), want):
		violations = append(violations, File+" does not match the dependencies of the client; run `go run ./tools/release licenses generate`")
		violations = append(violations, componentDrift(have, want)...)
	}

	return violations, nil
}

// Render formats the components as THIRD_PARTY_LICENSES.txt.
func Render(components []Component, targets []Target) []byte {
	all := sortTargets(slices.Clone(targets))

	var b bytes.Buffer

	b.WriteString("THIRD-PARTY LICENSES OF THE SCHEPHERD CLIENT\n\n")
	b.WriteString("The schepherd binary is built from Schepherd's own source code, licensed\n")
	b.WriteString("under the MIT License (see LICENSE), and links the third-party components\n")
	b.WriteString("listed below. This file reproduces the license, notice and patent files of\n")
	b.WriteString("each component as they ship with it; only line endings and trailing\n")
	b.WriteString("whitespace are normalized.\n\n")
	fmt.Fprintf(&b, "Release targets: %s.\n\n", joinTargets(all))
	b.WriteString("Generated by `go run ./tools/release licenses generate`. Do not edit it by\n")
	b.WriteString("hand: `go run ./tools/release licenses check` fails when it no longer\n")
	b.WriteString("matches the dependencies of the client.\n\n")
	b.WriteString("Components:\n\n")

	for _, c := range components {
		fmt.Fprintf(&b, "  %s\n", componentLine(c, all))
	}

	rule := strings.Repeat("=", 80)

	for _, c := range components {
		for _, f := range c.Files {
			fmt.Fprintf(&b, "\n%s\n%s %s\n", rule, c.Name, c.Version)
			fmt.Fprintf(&b, "Linked into: %s\n", targetScope(c.Targets, all))
			fmt.Fprintf(&b, "File: %s\n%s\n\n%s", f.Name, rule, f.Text)
		}
	}

	return b.Bytes()
}

func (c *Component) addTarget(t Target) {
	if !slices.Contains(c.Targets, t) {
		c.Targets = sortTargets(append(c.Targets, t))
	}
}

func withDefaults(opts Options) Options {
	if opts.Root == "" {
		opts.Root = "."
	}

	if opts.Package == "" {
		opts.Package = ClientPackage
	}

	if len(opts.Targets) == 0 {
		opts.Targets = ReleaseTargets()
	}

	return opts
}

// goEnviron fixes everything that could change the dependency set or the
// standard library between machines: the toolchain named in go.mod (the one
// release builds use), no cgo like GoReleaser, no workspace and no GOFLAGS
// from the caller.
func goEnviron(ctx context.Context, opts Options) ([]string, error) {
	environ := gitx.Without(opts.Env, "GOOS", "GOARCH", "CGO_ENABLED", "GOFLAGS", "GOWORK")
	environ = append(environ, "CGO_ENABLED=0", "GOFLAGS=-mod=readonly", "GOWORK=off")

	var mod modFile
	if err := goJSON(ctx, opts.Root, environ, &mod, "mod", "edit", "-json"); err != nil {
		return nil, err
	}

	toolchain := mod.Toolchain
	// Without a toolchain line, a go line with a patch release names the
	// exact toolchain; a newer local Go must not change the listed stdlib.
	if toolchain == "" && strings.Count(mod.Go, ".") == 2 {
		toolchain = "go" + mod.Go
	}

	if toolchain != "" && toolchain != "default" {
		environ = append(gitx.Without(environ, "GOTOOLCHAIN"), "GOTOOLCHAIN="+toolchain)
	}

	return environ, nil
}

func listDeps(ctx context.Context, opts Options, environ []string, target Target) ([]listedPackage, error) {
	environ = append(slices.Clone(environ), "GOOS="+target.GOOS, "GOARCH="+target.GOARCH)

	out, err := runGo(ctx, opts.Root, environ, "list", "-deps", "-json=Standard,Module", "--", opts.Package)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", target, err)
	}

	var packages []listedPackage

	dec := json.NewDecoder(bytes.NewReader(out))

	for {
		var p listedPackage

		err := dec.Decode(&p)
		if errors.Is(err, io.EOF) {
			return packages, nil
		}

		if err != nil {
			return nil, fmt.Errorf("%s: decode go list output: %w", target, err)
		}

		packages = append(packages, p)
	}
}

func moduleComponent(m *listedModule) (Component, string, error) {
	c := Component{Name: m.Path, Version: m.Version}
	dir := m.Dir

	if r := m.Replace; r != nil {
		c.Version = strings.TrimSpace(fmt.Sprintf("%s => %s %s", m.Version, r.Path, r.Version))
		dir = r.Dir
	}

	if dir == "" {
		return Component{}, "", fmt.Errorf("go list reports no source directory for %s %s", c.Name, c.Version)
	}

	return c, dir, nil
}

func readLicenseFiles(dir string) ([]LicenseFile, error) {
	files, err := licenseFiles(dir)
	if err != nil {
		return nil, err
	}

	if len(files) == 0 {
		return nil, errors.New("no LICENSE, COPYING or NOTICE file; its license cannot be redistributed")
	}

	return files, nil
}

func licenseFiles(dir string) ([]LicenseFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read source directory: %w", err)
	}

	var files []LicenseFile

	for _, e := range entries {
		upper := strings.ToUpper(e.Name())
		if !e.Type().IsRegular() || strings.HasSuffix(upper, ".GO") ||
			!slices.ContainsFunc(licenseFilePrefixes, func(p string) bool { return strings.HasPrefix(upper, p) }) {
			continue
		}

		data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, e.Name())))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}

		text := normalizeText(data)
		if text == "" {
			return nil, fmt.Errorf("%s is empty", e.Name())
		}

		files = append(files, LicenseFile{Name: e.Name(), Text: text})
	}

	return files, nil
}

func normalizeNewlines(data []byte) []byte {
	return bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
}

func normalizeText(data []byte) string {
	lines := strings.Split(strings.ReplaceAll(string(normalizeNewlines(data)), "\r", "\n"), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}

	text := strings.Trim(strings.Join(lines, "\n"), "\n")
	if text == "" {
		return ""
	}

	return text + "\n"
}

func componentLine(c Component, all []Target) string {
	line := c.Name + " " + c.Version
	if len(c.Targets) != len(all) {
		line += " (" + joinTargets(c.Targets) + " only)"
	}

	return line
}

func targetScope(targets, all []Target) string {
	if len(targets) == len(all) {
		return "all release targets"
	}

	return joinTargets(targets)
}

func joinTargets(targets []Target) string {
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.String())
	}

	return strings.Join(names, ", ")
}

func sortTargets(targets []Target) []Target {
	slices.SortFunc(targets, func(a, b Target) int { return strings.Compare(a.String(), b.String()) })

	return targets
}

// componentDrift names the components whose summary line differs between
// the committed and the generated file.
func componentDrift(have, want []byte) []string {
	haveLines := componentLines(normalizeNewlines(have))
	wantLines := componentLines(want)

	var drift []string

	for _, line := range wantLines {
		if !slices.Contains(haveLines, line) {
			drift = append(drift, File+": missing or outdated component "+line)
		}
	}

	for _, line := range haveLines {
		if !slices.Contains(wantLines, line) {
			drift = append(drift, File+": lists "+line+", which the client no longer links")
		}
	}

	return drift
}

func componentLines(data []byte) []string {
	_, rest, ok := bytes.Cut(data, []byte("Components:\n\n"))
	if !ok {
		return nil
	}

	var lines []string

	for line := range strings.SplitSeq(string(rest), "\n") {
		entry, ok := strings.CutPrefix(line, "  ")
		if !ok {
			break
		}

		lines = append(lines, entry)
	}

	return lines
}

func goJSON(ctx context.Context, dir string, environ []string, v any, args ...string) error {
	out, err := runGo(ctx, dir, environ, args...)
	if err != nil {
		return err
	}

	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("go %s: unexpected output: %w", strings.Join(args, " "), err)
	}

	return nil
}

func runGo(ctx context.Context, dir string, environ []string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // G204: runs the go command with arguments built by this package
	cmd.Dir = dir
	cmd.Env = environ
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return stdout.Bytes(), nil
}
