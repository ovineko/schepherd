package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"

	"github.com/ovineko/schepherd/internal/buildinfo/semver"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/registry"
)

const (
	parserFilename         = "schepherd-parsers.wasm"
	parserMetadataFilename = "schepherd-parsers.json"
	parserManifestFilename = "schepherd-parsers.oci.json"
	parserRepository       = "ghcr.io/ovineko/schepherd-parsers"
	parserArtifactType     = "application/vnd.datamitsu.parsers.v1+wasm"
	parserMaxBytes         = 64 << 20
)

type parserMetadata struct {
	Version          string `json:"version"`
	ABI              int    `json:"abi"`
	CapabilitySchema int    `json:"capabilitySchema"`
	File             string `json:"file"`
	Hash             string `json:"hash"`
	Size             int    `json:"size"`
	URL              string `json:"url"`
	OCI              struct {
		Ref    string `json:"ref"`
		Digest string `json:"digest"`
	} `json:"oci"`
}

func newParsersCommand(environ []string) *cobra.Command {
	cmd := &cobra.Command{Use: "parsers", Short: "Build, inspect and publish the Schepherd WASM parsers", Args: cobra.ArbitraryArgs, RunE: requireSubcommand}
	for _, action := range []string{"build", "check", "metadata", "host", "publish", "verify-published"} {
		var dir, version string
		sub := &cobra.Command{Use: action, Args: usageArgs(cobra.NoArgs)}
		sub.Flags().StringVar(&dir, "dir", "dist/parsers", "Module and metadata directory")
		sub.Flags().StringVar(&version, "version", "0.0.0-dev", "Module SemVer (release version for publication)")
		sub.RunE = func(cmd *cobra.Command, _ []string) error {
			return runParserAction(cmd, action, dir, version, environ)
		}
		cmd.AddCommand(sub)
	}
	return cmd
}

func runParserAction(cmd *cobra.Command, action, dir, version string, environ []string) error {
	if action == "host" {
		return checkParserHost(cmd.Context(), dir, environ)
	}
	if _, err := semver.Parse(version); err != nil {
		return fault.Wrap(fault.Usage, err, "parser version")
	}
	if action == "build" {
		if err := buildParsers(cmd.Context(), dir, version, environ, cmd.ErrOrStderr()); err != nil {
			return err
		}
	}
	module, err := readParserModule(dir)
	if err != nil {
		return err
	}
	metadata, manifest, err := parserArtifact(module, version)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "parser artifact")
	}
	if action == "build" || action == "metadata" {
		if err := checkParserModule(cmd.Context(), module, version, environ); err != nil {
			return err
		}
		return writeParserMetadata(dir, metadata, manifest)
	}
	return checkParserAction(cmd, action, dir, module, manifest, metadata, environ)
}

func checkParserAction(cmd *cobra.Command, action, dir string, module, manifest []byte, metadata parserMetadata, environ []string) error {
	if err := checkParserFiles(dir, metadata, manifest); err != nil {
		return err
	}
	if action != "verify-published" {
		if err := checkParserModule(cmd.Context(), module, metadata.Version, environ); err != nil {
			return err
		}
	}
	if action != "check" {
		if _, err := semver.ParseRelease(metadata.Version); err != nil {
			return fault.Wrap(fault.Usage, err, "parser publication requires a release version")
		}
		if action == "publish" {
			if err := publishParsers(cmd.Context(), module, manifest, metadata); err != nil {
				return err
			}
		} else if err := verifyPublishedParsers(cmd.Context(), metadata, false); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), metadata.OCI.Ref+"@"+metadata.OCI.Digest); err != nil {
		return fault.Wrap(fault.Internal, err, "write parser OCI reference")
	}
	return nil
}

func parserArtifact(module []byte, version string) (parserMetadata, []byte, error) {
	m := parserMetadata{Version: version, ABI: 2, CapabilitySchema: 3, File: parserFilename, Hash: parserHash(module), Size: len(module), URL: "https://github.com/ovineko/schepherd/releases/download/v" + version + "/" + parserFilename}
	m.OCI.Ref = parserRepository
	manifest := ocispec.Manifest{
		SchemaVersion: 2, MediaType: ocispec.MediaTypeImageManifest, ArtifactType: parserArtifactType,
		Config:      parserDescriptor([]byte("{}"), ocispec.MediaTypeEmptyJSON),
		Layers:      []ocispec.Descriptor{parserDescriptor(module, "application/wasm")},
		Annotations: map[string]string{ocispec.AnnotationVersion: version, "com.datamitsu.parsers-version": version},
	}
	manifest.Layers[0].Annotations = map[string]string{ocispec.AnnotationTitle: parserFilename}
	body, err := json.Marshal(manifest)
	if err != nil {
		return m, nil, fault.Wrap(fault.Internal, err, "encode parser OCI manifest")
	}
	m.OCI.Digest = parserHash(body)
	return m, body, nil
}

func parserDescriptor(data []byte, mediaType string) ocispec.Descriptor {
	return ocispec.Descriptor{MediaType: mediaType, Digest: godigest.Digest(parserHash(data)), Size: int64(len(data))}
}

func parserHash(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func readParserModule(dir string) ([]byte, error) {
	// The maintainer explicitly selects this directory; the module filename is fixed.
	// bearer:disable go_gosec_filesystem_filereadtaint
	file, err := os.Open(filepath.Join(dir, parserFilename)) //nolint:gosec // G304: dir is an explicit maintainer CLI argument, not registry input
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "read parser module")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "stat parser module")
	}
	if !info.Mode().IsRegular() || info.Size() <= 8 || info.Size() > parserMaxBytes {
		return nil, fault.New(fault.Integrity, "parser module must be a regular WASM file within 64 MiB")
	}
	data := make([]byte, info.Size())
	if _, err := file.ReadAt(data, 0); err != nil {
		return nil, fault.Wrap(fault.Internal, err, "read parser module")
	}
	if !bytes.Equal(data[:8], []byte{0, 'a', 's', 'm', 1, 0, 0, 0}) {
		return nil, fault.New(fault.Integrity, "invalid WASM header")
	}
	return data, nil
}

func parserFiles(metadata parserMetadata, manifest []byte) (map[string][]byte, error) {
	body, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "encode parser metadata")
	}
	body = append(body, '\n')
	files := map[string][]byte{parserMetadataFilename: body, parserManifestFilename: manifest}
	var checksums strings.Builder
	for _, name := range []string{parserFilename, parserMetadataFilename, parserManifestFilename} {
		hash := metadata.Hash
		if name != parserFilename {
			hash = parserHash(files[name])
		}
		fmt.Fprintf(&checksums, "%s  %s\n", strings.TrimPrefix(hash, "sha256:"), name)
	}
	files["SHA256SUMS"] = []byte(checksums.String())
	return files, nil
}

func writeParserMetadata(dir string, metadata parserMetadata, manifest []byte) error {
	files, err := parserFiles(metadata, manifest)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "encode parser metadata")
	}
	for name, data := range files {
		// Release metadata and checksums are public assets, not credentials.
		// bearer:disable go_gosec_file_permissions_file_perm
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil { //nolint:gosec // G306: parser release assets are public
			return fault.Wrap(fault.Internal, err, "write parser metadata")
		}
	}
	return nil
}

func checkParserFiles(dir string, metadata parserMetadata, manifest []byte) error {
	files, err := parserFiles(metadata, manifest)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "encode parser metadata")
	}
	for name, want := range files {
		// The maintainer selects the directory; each name is a fixed release artifact.
		// bearer:disable go_gosec_filesystem_filereadtaint
		got, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // G304: dir is a maintainer CLI argument and name is a fixed artifact filename
		if err != nil {
			return fault.Wrap(fault.Internal, err, "read parser metadata")
		}
		if !bytes.Equal(got, want) {
			return fault.New(fault.Integrity, "parser artifact %s does not match module and version", name)
		}
	}
	return nil
}

func publishParsers(ctx context.Context, module, manifest []byte, metadata parserMetadata) error {
	repository, err := registry.ParseRepository(metadata.OCI.Ref)
	if err != nil {
		return fault.Wrap(fault.Usage, err, "parse parser OCI repository")
	}
	repo, err := registry.NewClient(registry.Options{}).Open(repository)
	if err != nil {
		return fault.Wrap(fault.Registry, err, "open parser OCI repository")
	}
	for _, blob := range []struct {
		data  []byte
		media string
	}{{[]byte("{}"), ocispec.MediaTypeEmptyJSON}, {module, "application/wasm"}} {
		if _, err := repo.PushBlob(ctx, parserDescriptor(blob.data, blob.media), blob.data); err != nil {
			return fault.Wrap(fault.Registry, err, "publish parser OCI blob")
		}
	}
	desc := parserDescriptor(manifest, ocispec.MediaTypeImageManifest)
	if _, err := repo.PushManifest(ctx, desc, manifest); err != nil {
		return fault.Wrap(fault.Registry, err, "publish parser OCI manifest")
	}
	_, err = repo.EnsureTag(ctx, desc, "v"+metadata.Version)
	if err != nil {
		return fault.Wrap(fault.Registry, err, "bind parser OCI release tag")
	}
	return nil
}

func parserCommand(ctx context.Context, dir string, environ []string, name string, args ...string) *exec.Cmd {
	// Callers supply fixed maintainer programs and argument lists without a shell.
	// bearer:disable go_gosec_injection_subproc_injection
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: fixed maintainer programs and argument lists supplied only by this package
	cmd.Dir = dir
	cmd.Env = environ
	return cmd
}
