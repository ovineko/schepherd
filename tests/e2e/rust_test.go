//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func preparedRustEnv(root string) []string {
	home := filepath.Join(root, ".tools", "e2e-rust")
	return []string{
		"CARGO_HOME=" + filepath.Join(home, "cargo"),
		"RUSTUP_HOME=" + filepath.Join(home, "rustup"),
		"CARGO_NET_OFFLINE=true",
		"RUSTUP_AUTO_INSTALL=0",
	}
}

func rustToolEnviron(root string, offline bool) []string {
	var base []string
	for _, entry := range hostEnviron() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "CARGO_") || strings.HasPrefix(key, "RUSTUP_") {
			continue
		}
		base = append(base, entry)
	}
	base = append(base, preparedRustEnv(root)...)
	if !offline {
		base = mergeEnv(base, []string{"CARGO_NET_OFFLINE=false"})
	}
	return base
}

func prepareRust(ctx context.Context, root string) error {
	crate := filepath.Join(root, "parsers")
	data, err := os.ReadFile(filepath.Join(crate, "rust-toolchain.toml"))
	if err != nil {
		return err
	}
	var pin struct {
		Toolchain struct {
			Channel    string   `toml:"channel"`
			Profile    string   `toml:"profile"`
			Components []string `toml:"components"`
			Targets    []string `toml:"targets"`
		} `toml:"toolchain"`
	}
	if err := toml.Unmarshal(data, &pin); err != nil {
		return fmt.Errorf("read parser Rust toolchain: %w", err)
	}
	if pin.Toolchain.Channel == "" || pin.Toolchain.Profile == "" {
		return errors.New("parser Rust toolchain must pin a channel and profile")
	}
	args := []string{"toolchain", "install", pin.Toolchain.Channel, "--profile", pin.Toolchain.Profile}
	for _, component := range pin.Toolchain.Components {
		args = append(args, "--component", component)
	}
	for _, target := range pin.Toolchain.Targets {
		args = append(args, "--target", target)
	}
	environ := rustToolEnviron(root, false)
	if out, err := runTool(ctx, crate, environ, "rustup", args...); err != nil {
		return fmt.Errorf("prepare pinned parser Rust toolchain: %w\n%s", err, out)
	}
	if out, err := runTool(ctx, crate, environ, "cargo", "fetch", "--locked", "--target", "wasm32-unknown-unknown"); err != nil {
		return fmt.Errorf("prepare locked parser Cargo dependencies: %w\n%s", err, out)
	}
	fmt.Fprintln(os.Stderr, "e2e prepare: cached pinned parser Rust toolchain and locked WASM dependencies in .tools/e2e-rust")
	return nil
}

func releaseBuildEnv() []string {
	return mergeEnv(rustToolEnviron(suite.root, true), releaseGitEnv())
}

func TestHarness_PreparedRustToolchain(t *testing.T) {
	t.Parallel()

	sb := newSandbox(t)
	var isolated []string
	for _, entry := range sb.environ() {
		if !strings.HasPrefix(entry, "PATH=") {
			isolated = append(isolated, entry)
		}
	}
	environ := mergeEnv(rustToolEnviron(suite.root, true), isolated)
	opts := runOpts{Dir: filepath.Join(suite.root, "parsers"), Env: environ}
	sysroot := strings.TrimSpace(string(hostTool(t, opts, "rustc", "--print", "sysroot").ok(t).Stdout))
	toolchains := filepath.Join(suite.root, ".tools", "e2e-rust", "rustup", "toolchains")
	rel, err := filepath.Rel(toolchains, sysroot)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		t.Fatalf("parser sysroot %q is not in prepared toolchains %q: %v", sysroot, toolchains, err)
	}
	hostTool(t, opts, "cargo", "metadata", "--format-version", "1", "--locked", "--offline", "--filter-platform", "wasm32-unknown-unknown").ok(t)

	// A fresh Cargo home must fail offline rather than silently use the developer's cache.
	opts.Env = mergeEnv(environ, []string{"CARGO_HOME=" + filepath.Join(sb.Dir, "empty-cargo")})
	if res := hostTool(t, opts, "cargo", "metadata", "--format-version", "1", "--locked", "--offline", "--filter-platform", "wasm32-unknown-unknown"); res.Code == 0 || !strings.Contains(string(res.Stderr), "offline") {
		t.Fatalf("Cargo did not reject uncached dependencies offline:\n%s", res)
	}
}
