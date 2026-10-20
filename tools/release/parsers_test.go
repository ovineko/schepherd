package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestParserArtifactPinsExactPayloadAndManifest(t *testing.T) {
	module := []byte{0, 'a', 's', 'm', 1, 0, 0, 0, 0}
	metadata, manifest, err := parserArtifact(module, "1.2.3-rc.1")
	if err != nil {
		t.Fatal(err)
	}
	var decoded ocispec.Manifest
	if err := json.Unmarshal(manifest, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ArtifactType != parserArtifactType || decoded.MediaType != ocispec.MediaTypeImageManifest || decoded.SchemaVersion != 2 {
		t.Fatalf("incompatible OCI manifest: %s", manifest)
	}
	if len(decoded.Layers) != 1 || decoded.Layers[0].MediaType != "application/wasm" || decoded.Layers[0].Digest.String() != metadata.Hash || decoded.Layers[0].Size != int64(len(module)) {
		t.Fatalf("wrong WASM layer: %+v", decoded.Layers)
	}
	if metadata.OCI.Digest != parserHash(manifest) || metadata.Hash != parserHash(module) {
		t.Fatalf("metadata does not pin actual artifact: %+v", metadata)
	}
	if metadata.URL != "https://github.com/ovineko/schepherd/releases/download/v1.2.3-rc.1/schepherd-parsers.wasm" {
		t.Fatalf("release asset URL: %s", metadata.URL)
	}
	_, repeated, err := parserArtifact(module, "1.2.3-rc.1")
	if err != nil || !bytes.Equal(manifest, repeated) {
		t.Fatalf("manifest is not deterministic: %v", err)
	}
	changed := append(bytes.Clone(module), 1)
	other, _, err := parserArtifact(changed, "1.2.3-rc.1")
	if err != nil || other.OCI.Digest == metadata.OCI.Digest || other.Hash == metadata.Hash {
		t.Fatal("changed payload retained a pin")
	}
}

func TestParserMetadataRejectsTampering(t *testing.T) {
	dir := t.TempDir()
	module := []byte{0, 'a', 's', 'm', 1, 0, 0, 0, 0}
	metadata, manifest, err := parserArtifact(module, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeParserMetadata(dir, metadata, manifest); err != nil {
		t.Fatal(err)
	}
	if err := checkParserFiles(dir, metadata, manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, parserManifestFilename), append(manifest, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkParserFiles(dir, metadata, manifest); err == nil {
		t.Fatal("accepted changed OCI manifest bytes")
	}
}

func TestParserReadRejectsNonWasm(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, parserFilename), []byte("not a WASM module"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readParserModule(dir); err == nil {
		t.Fatal("accepted non-WASM artifact")
	}
}

func TestParserBuildEnvironmentRejectsInjectedFlags(t *testing.T) {
	got := parserBuildEnvironment([]string{"PATH=/bin", "CARGO_HOME=/cargo", "RUSTUP_TOOLCHAIN=nightly", "RUSTFLAGS=bad", "CARGO_PROFILE_RELEASE_OPT_LEVEL=0", "CARGO_ENCODED_RUSTFLAGS=bad", "SCHEPHERD_PARSERS_VERSION=bad", "RUSTC_WRAPPER=bad"})
	if strings.Join(got, "\n") != "PATH=/bin\nCARGO_HOME=/cargo" {
		t.Fatalf("build environment: %q", got)
	}
}

func TestParserCommandsDoNotRequireRustForHelp(t *testing.T) {
	for _, args := range [][]string{{"parsers", "--help"}, {"parsers", "build", "--help"}, {"parsers", "check", "--help"}, {"parsers", "metadata", "--help"}, {"parsers", "publish", "--help"}, {"parsers", "verify-published", "--help"}} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr, []string{}); code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, stderr.String())
		}
	}
}

func TestPublishedParserCommandRejectsUnverifiedReleaseMetadata(t *testing.T) {
	dir := t.TempDir()
	module := []byte{0, 'a', 's', 'm', 1, 0, 0, 0, 0}
	metadata, manifest, err := parserArtifact(module, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, parserFilename), module, 0o644); err != nil {
		t.Fatal(err)
	}
	metadata.Hash = parserHash([]byte("substituted module"))
	if err := writeParserMetadata(dir, metadata, manifest); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"parsers", "verify-published", "--dir", dir, "--version", "1.2.3"}, &stdout, &stderr, []string{})
	if code == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "does not match module and version") {
		t.Fatalf("unverified metadata returned an OCI subject: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}
