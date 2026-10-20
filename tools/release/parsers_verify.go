package main

import (
	"context"
	"encoding/json"
	"io"
	"os"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/registry"
)

func verifyPublishedParsers(ctx context.Context, metadata parserMetadata, plainHTTP bool) error {
	repository, err := registry.ParseRepository(metadata.OCI.Ref)
	if err != nil {
		return fault.Wrap(fault.Usage, err, "parse parser OCI repository")
	}
	if err := validatePublishedParserMetadata(metadata); err != nil {
		return err
	}
	credentials, err := anonymousParserCredentials()
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(credentials) }()
	client := registry.NewClient(registry.Options{Hosts: map[string]registry.HostConfig{
		repository.Host: {PlainHTTP: plainHTTP, CredentialsFile: credentials},
	}})
	repo, err := client.Open(repository)
	if err != nil {
		return fault.Wrap(fault.Registry, err, "open anonymous parser OCI repository")
	}
	body, err := repo.FetchManifestByDigest(ctx, metadata.OCI.Digest, ocispec.MediaTypeImageManifest, 0)
	if err != nil {
		return fault.Wrap(fault.Registry, err, "fetch published parser manifest anonymously")
	}
	manifest, err := parsePublishedParserManifest(body, metadata)
	if err != nil {
		return err
	}
	if err := repo.FetchTo(ctx, manifest.Config, io.Discard); err != nil {
		return fault.Wrap(fault.Registry, err, "fetch published parser config anonymously")
	}
	if err := repo.FetchTo(ctx, manifest.Layers[0], io.Discard); err != nil {
		return fault.Wrap(fault.Registry, err, "fetch published parser WASM anonymously")
	}
	return nil
}

func validatePublishedParserMetadata(metadata parserMetadata) error {
	if err := digest.Validate(metadata.OCI.Digest); err != nil {
		return fault.Wrap(fault.Integrity, err, "parser OCI manifest digest")
	}
	if err := digest.Validate(metadata.Hash); err != nil {
		return fault.Wrap(fault.Integrity, err, "parser payload digest")
	}
	if metadata.Size <= 8 || metadata.Size > parserMaxBytes {
		return fault.New(fault.Integrity, "parser payload size must be within 64 MiB")
	}
	return nil
}

func anonymousParserCredentials() (string, error) {
	// An explicit empty store prevents a prior push login or credential helper
	// from turning a private package into a successful anonymous smoke check.
	credentials, err := os.CreateTemp("", "schepherd-parser-anonymous-*.json")
	if err != nil {
		return "", fault.Wrap(fault.Internal, err, "create anonymous credential store")
	}
	_, writeErr := credentials.WriteString("{}")
	closeErr := credentials.Close()
	if writeErr != nil {
		_ = os.Remove(credentials.Name())
		return "", fault.Wrap(fault.Internal, writeErr, "write anonymous credential store")
	}
	if closeErr != nil {
		_ = os.Remove(credentials.Name())
		return "", fault.Wrap(fault.Internal, closeErr, "close anonymous credential store")
	}
	return credentials.Name(), nil
}

func parsePublishedParserManifest(body []byte, metadata parserMetadata) (ocispec.Manifest, error) {
	var manifest ocispec.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return manifest, fault.Wrap(fault.Integrity, err, "parse published parser manifest")
	}
	config := parserDescriptor([]byte("{}"), ocispec.MediaTypeEmptyJSON)
	if manifest.SchemaVersion != 2 || manifest.MediaType != ocispec.MediaTypeImageManifest || manifest.ArtifactType != parserArtifactType ||
		manifest.Config.MediaType != config.MediaType || manifest.Config.Digest != config.Digest || manifest.Config.Size != config.Size {
		return manifest, fault.New(fault.Integrity, "published parser manifest has an incompatible artifact or config type")
	}
	if len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != "application/wasm" {
		return manifest, fault.New(fault.Integrity, "published parser manifest must contain exactly one uncompressed application/wasm layer")
	}
	layer := manifest.Layers[0]
	if layer.Digest.String() != metadata.Hash || layer.Size != int64(metadata.Size) {
		return manifest, fault.New(fault.Integrity, "published parser layer does not match release metadata")
	}
	if manifest.Annotations[ocispec.AnnotationVersion] != metadata.Version || manifest.Annotations["com.datamitsu.parsers-version"] != metadata.Version {
		return manifest, fault.New(fault.Integrity, "published parser version does not match release metadata")
	}
	return manifest, nil
}
