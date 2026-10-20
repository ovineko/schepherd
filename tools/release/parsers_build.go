package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/ovineko/schepherd/internal/fault"
)

func buildParsers(ctx context.Context, dir, version string, environ []string, stderr io.Writer) error {
	root, err := os.Getwd()
	if err != nil {
		return fault.Wrap(fault.Internal, err, "parser build directory")
	}
	var toolchain struct {
		Toolchain struct {
			Channel string `toml:"channel"`
		} `toml:"toolchain"`
	}
	data, err := os.ReadFile(filepath.Join(root, "parsers", "rust-toolchain.toml")) //nolint:gosec // G304: fixed source file under the maintainer's current checkout
	if err != nil {
		return fault.Wrap(fault.Internal, err, "read parser Rust toolchain")
	}
	if err := toml.Unmarshal(data, &toolchain); err != nil || toolchain.Toolchain.Channel == "" {
		return fault.New(fault.Usage, "parser Rust toolchain must pin a channel")
	}
	crate := filepath.Join(root, "parsers")
	baseEnv := parserBuildEnvironment(environ)
	sysrootCmd := parserCommand(ctx, crate, baseEnv, "rustc", "--print", "sysroot")
	sysroot, err := sysrootCmd.Output()
	if err != nil {
		return fault.Wrap(fault.Internal, err, "locate pinned Rust sysroot (install the toolchain in parsers/rust-toolchain.toml)")
	}
	target, err := os.MkdirTemp("", "schepherd-parsers-build-")
	if err != nil {
		return fault.Wrap(fault.Internal, err, "create isolated Cargo target")
	}
	defer func() { _ = os.RemoveAll(target) }()
	home, err := os.UserHomeDir()
	if err != nil {
		return fault.Wrap(fault.Internal, err, "locate Cargo home")
	}
	cargoHome := filepath.Join(home, ".cargo")
	for _, entry := range environ {
		if value, ok := strings.CutPrefix(entry, "CARGO_HOME="); ok && value != "" {
			cargoHome = value
		}
	}
	cargoHome, err = filepath.Abs(cargoHome)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "resolve Cargo home")
	}
	flags := []string{"--remap-path-prefix=" + root + "=/src", "--remap-path-prefix=" + cargoHome + "=/cargo", "--remap-path-prefix=" + strings.TrimSpace(string(sysroot)) + "=/rust", "--remap-path-prefix=" + target + "=/target", "-C", "debuginfo=0"}
	baseEnv = append(baseEnv, "SCHEPHERD_PARSERS_VERSION="+version, "CARGO_ENCODED_RUSTFLAGS="+strings.Join(flags, "\x1f"), "CARGO_TARGET_DIR="+target, "CARGO_INCREMENTAL=0", "SOURCE_DATE_EPOCH=0")
	cmd := parserCommand(ctx, crate, baseEnv, "cargo", "build", "--locked", "--release", "--target", "wasm32-unknown-unknown")
	cmd.Stdout, cmd.Stderr = stderr, stderr
	if err := cmd.Run(); err != nil {
		return fault.Wrap(fault.Internal, err, "build parser module with Rust %s", toolchain.Toolchain.Channel)
	}
	module, err := os.ReadFile(filepath.Join(target, "wasm32-unknown-unknown", "release", "schepherd_parsers.wasm")) //nolint:gosec // G304: fixed Cargo output filename under the private directory returned by os.MkdirTemp
	if err != nil {
		return fault.Wrap(fault.Internal, err, "read built parser module")
	}
	for _, path := range []string{root, cargoHome, strings.TrimSpace(string(sysroot)), target} {
		if bytes.Contains(module, []byte(path)) {
			return fault.New(fault.Integrity, "parser module contains an unremapped build path")
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: parser release outputs must be publicly readable
		return fault.Wrap(fault.Internal, err, "create parser output")
	}
	// The WASM module is a public release asset consumed by packaging and local hosts.
	// bearer:disable go_gosec_file_permissions_file_perm
	if err := os.WriteFile(filepath.Join(dir, parserFilename), module, 0o644); err != nil { //nolint:gosec // G306: the WASM module is a public release asset
		return fault.Wrap(fault.Internal, err, "write parser module")
	}
	return nil
}

func parserBuildEnvironment(environ []string) []string {
	result := make([]string, 0, len(environ))
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		if key == "RUSTFLAGS" || key == "RUSTDOCFLAGS" || key == "RUSTC_WRAPPER" || key == "RUSTC_WORKSPACE_WRAPPER" || key == "RUSTUP_TOOLCHAIN" || key == "RUSTC" || key == "SCHEPHERD_PARSERS_VERSION" || key == "SOURCE_DATE_EPOCH" || key == "CARGO_ENCODED_RUSTFLAGS" || key == "CARGO_TARGET_DIR" || key == "CARGO_INCREMENTAL" || strings.HasPrefix(key, "CARGO_PROFILE_") || strings.HasPrefix(key, "CARGO_TARGET_") || key == "CARGO_BUILD_RUSTFLAGS" {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func checkParserModule(ctx context.Context, module []byte, version string, environ []string) error {
	cmd := parserCommand(ctx, "", environ, "node", "--input-type=module", "-e", parserSmokeScript, version)
	cmd.Stdin = bytes.NewReader(module)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fault.New(fault.Integrity, "parser ABI smoke failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

const parserSmokeScript = `
import { readFileSync } from 'node:fs';
import assert from 'node:assert/strict';
const { instance } = await WebAssembly.instantiate(readFileSync(0), {});
const e = instance.exports;
for (const name of ['alloc','dealloc','parse','describe','reset']) assert.equal(typeof e[name], 'function', name);
assert.ok(e.memory instanceof WebAssembly.Memory);
const decoder = new TextDecoder('utf-8', { fatal: true });
function result(packed) {
  const pointer = Number(packed >> 32n), length = Number(packed & 0xffffffffn);
  assert.ok(pointer > 0 && length > 0);
  const text = decoder.decode(new Uint8Array(e.memory.buffer, pointer, length));
  e.dealloc(pointer, length);
  return JSON.parse(text);
}
function parse(tool, stdout, exitCode) {
  const allocations = [tool, stdout, ''].map(text => {
    const bytes = new TextEncoder().encode(text);
    if (!bytes.length) return [0, 0];
    const pointer = e.alloc(bytes.length);
    assert.ok(pointer > 0);
    new Uint8Array(e.memory.buffer, pointer, bytes.length).set(bytes);
    return [pointer, bytes.length];
  });
  try { return result(e.parse(...allocations.flat(), exitCode)); }
  finally { for (const [pointer, length] of allocations) e.dealloc(pointer, length); e.reset(); }
}
const capabilities = result(e.describe());
assert.equal(capabilities.schemaVersion, 3);
assert.equal(capabilities.abi, 2);
assert.equal(capabilities.module, 'schepherd-parsers');
assert.equal(capabilities.version, process.argv[1]);
assert.deepEqual(capabilities.tools.map(t => t.name).sort(), ['ajv','check_jsonschema','schepherd','sourcemeta']);
const errors = JSON.stringify([{instancePath:'/name',schemaPath:'#/properties/name/type',keyword:'type',params:{type:'string'},message:'must be string'}]);
const direct = parse('ajv', errors, 1);
assert.equal(direct.abi, 2);
assert.equal(direct.recognized, true);
assert.ok(direct.diagnostics.length > 0);
const stream = text => ({bytes:Buffer.byteLength(text),text,truncated:false});
const file = '/workspace/config.json';
const report = {reportVersion:2,workspace:'/workspace',mode:'per-file',exitCode:1,tasks:[{schemaId:'config',schemaRef:'local',origin:'local',files:[file],paths:[file],cwd:'/workspace',processor:{name:'ajv',type:'ajv',output:'ajv-json',mode:'per-file'},status:'failed',exitCode:1,durationMs:1,stdout:stream(errors),stderr:stream('')}],skipped:[]};
const attributed = parse('schepherd', JSON.stringify(report), 1);
assert.equal(attributed.recognized, true);
assert.ok(attributed.diagnostics.some(d => d.file === file));
assert.equal(parse('schepherd', '{}', 0).recognized, false);
`
