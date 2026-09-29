// Package pins exposes the pinned releases of external maintainer tools.
//
// The JSON manifests next to this file are the single source of truth: the
// installer downloads exactly the assets they list and verifies the recorded
// checksums, and packages that require a tool version assert in their tests
// that their constant still equals the manifest.
package pins

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
)

//go:embed jsonschema.json
var jsonSchemaManifest []byte

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Asset is one downloadable release archive of a pinned tool.
type Asset struct {
	// File is the archive name, appended to Tool.Download to form its URL.
	File string `json:"file"`
	// SHA256 is the lowercase hex digest of the archive.
	SHA256 string `json:"sha256"`
	// Binary is the slash-separated path of the executable inside the archive.
	Binary string `json:"binary"`
}

// Tool is a pinned release of an external tool, keyed by release platform
// names such as "linux-x86_64" or "linux-arm64-musl".
type Tool struct {
	Assets   map[string]Asset `json:"assets"`
	Version  string           `json:"version"`
	License  string           `json:"license"`
	Download string           `json:"download"`
}

// JSONSchema returns the pinned Sourcemeta JSON Schema CLI release. The CLI
// is AGPL-3.0 licensed and is only ever run as a separate maintainer-side
// process; it is never linked into or shipped with Schepherd.
func JSONSchema() (*Tool, error) {
	return parse(jsonSchemaManifest)
}

// AssetURL fails for a platform without a pinned asset instead of falling
// back to a similar build, such as the glibc one for a musl platform.
func (t *Tool) AssetURL(platform string) (string, Asset, error) {
	asset, ok := t.Assets[platform]
	if !ok {
		return "", Asset{}, fmt.Errorf("no pinned asset for platform %q", platform)
	}

	return t.Download + asset.File, asset, nil
}

func parse(data []byte) (*Tool, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var tool Tool
	if err := dec.Decode(&tool); err != nil {
		return nil, fmt.Errorf("decode pins manifest: %w", err)
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode pins manifest: trailing data after the manifest")
	}

	if err := tool.validate(); err != nil {
		return nil, err
	}

	return &tool, nil
}

func (t *Tool) validate() error {
	if t.Version == "" || t.License == "" {
		return errors.New("pins manifest: version and license are required")
	}

	u, err := url.Parse(t.Download)
	if err != nil || u.Scheme != "https" || u.Host == "" || !strings.HasSuffix(u.Path, "/") {
		return fmt.Errorf("pins manifest: download %q must be an https URL ending in /", t.Download)
	}

	if len(t.Assets) == 0 {
		return errors.New("pins manifest: no assets")
	}

	for platform, asset := range t.Assets {
		if !sha256Hex.MatchString(asset.SHA256) {
			return fmt.Errorf("pins manifest: asset %s: sha256 must be 64 lowercase hex digits", platform)
		}

		if !strings.Contains(asset.File, t.Version) || path.Base(asset.File) != asset.File {
			return fmt.Errorf("pins manifest: asset %s: file %q must be a bare name containing version %s", platform, asset.File, t.Version)
		}

		if asset.Binary == "" || path.IsAbs(asset.Binary) || path.Clean(asset.Binary) != asset.Binary || strings.HasPrefix(asset.Binary, "../") {
			return fmt.Errorf("pins manifest: asset %s: binary %q must be a clean relative path", platform, asset.Binary)
		}
	}

	return nil
}
