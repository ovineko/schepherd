package upstream

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/upstream/tomlfile"
)

// Kinds of source description files (the top-level "kind" key). This
// package reads "upstream" files; package prepare reads "local" ones.
const (
	KindLocal    = "local"
	KindUpstream = "upstream"
)

const maxSourceFileBytes = 4 << 20

// SchemaStoreConfig is the pinned SchemaStore upstream description
// (sources/schemastore.toml, kind "upstream").
type SchemaStoreConfig struct {
	// Commit is the pinned SchemaStore commit (full lowercase SHA).
	Commit string
	// TarballBaseURL is passed to FetchSchemaStore.
	TarballBaseURL string
	// MaxTarballBytes bounds the compressed tarball download.
	MaxTarballBytes int64
	// Dependencies bounds fetching of external $ref targets.
	Dependencies DependencyLimits
}

// DependencyLimits bound the resolution of one schema's external references;
// every limit applies per schema (docs/publishing.md), never to a whole run.
type DependencyLimits struct {
	// MaxDepth bounds the reference chain from a schema to a dependency.
	MaxDepth int
	// MaxPerSchema bounds distinct dependencies of one schema.
	MaxPerSchema int
	// MaxDocumentBytes bounds one fetched document.
	MaxDocumentBytes int64
	// MaxTotalBytes bounds all dependency bytes fetched for one schema.
	MaxTotalBytes int64
}

type upstreamDoc struct {
	Kind            string          `toml:"kind"`
	Commit          string          `toml:"commit"`
	TarballBaseURL  string          `toml:"tarball_base_url"`
	MaxTarballBytes int64           `toml:"max_tarball_bytes"`
	Dependencies    dependenciesDoc `toml:"dependencies"`
}

type dependenciesDoc struct {
	MaxDepth         int   `toml:"max_depth"`
	MaxPerSchema     int   `toml:"max_per_schema"`
	MaxDocumentBytes int64 `toml:"max_document_bytes"`
	MaxTotalBytes    int64 `toml:"max_total_bytes"`
}

// LoadSchemaStoreConfig reads an upstream description:
//
//	kind = "upstream"
//	commit = "<40 lowercase hex digits>"
//	tarball_base_url = "https://codeload.github.com/SchemaStore/schemastore/tar.gz"
//	max_tarball_bytes = 67108864
//
//	[dependencies]
//	max_depth = 8
//	max_per_schema = 64
//	max_document_bytes = 16777216
//	max_total_bytes = 268435456
//
// Every key is required and every limit must be positive; limits cannot be
// disabled. Errors are fault.Usage.
func LoadSchemaStoreConfig(path string) (*SchemaStoreConfig, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "upstream source file")
	}

	var doc upstreamDoc
	if err := tomlfile.Decode(path, maxSourceFileBytes, &doc); err != nil {
		return nil, fault.Wrap(fault.Usage, err, "upstream source file")
	}

	if err := doc.validate(); err != nil {
		return nil, fault.Wrap(fault.Usage, err, "upstream source file %s", path)
	}

	return &SchemaStoreConfig{
		Commit:          doc.Commit,
		TarballBaseURL:  doc.TarballBaseURL,
		MaxTarballBytes: doc.MaxTarballBytes,
		Dependencies: DependencyLimits{
			MaxDepth:         doc.Dependencies.MaxDepth,
			MaxPerSchema:     doc.Dependencies.MaxPerSchema,
			MaxDocumentBytes: doc.Dependencies.MaxDocumentBytes,
			MaxTotalBytes:    doc.Dependencies.MaxTotalBytes,
		},
	}, nil
}

func (d *upstreamDoc) validate() error {
	switch {
	case d.Kind != KindUpstream:
		return fmt.Errorf("kind must be %q, got %q", KindUpstream, d.Kind)
	case !commitPattern.MatchString(d.Commit):
		return fmt.Errorf("commit %q must be a full lowercase 40-hex SHA", d.Commit)
	case d.TarballBaseURL == "":
		return errors.New("tarball_base_url is required")
	}

	if err := checkHTTPURL(d.TarballBaseURL); err != nil {
		return fmt.Errorf("tarball_base_url: %w", err)
	}

	limits := []struct {
		name  string
		value int64
	}{
		{"max_tarball_bytes", d.MaxTarballBytes},
		{"dependencies.max_depth", int64(d.Dependencies.MaxDepth)},
		{"dependencies.max_per_schema", int64(d.Dependencies.MaxPerSchema)},
		{"dependencies.max_document_bytes", d.Dependencies.MaxDocumentBytes},
		{"dependencies.max_total_bytes", d.Dependencies.MaxTotalBytes},
	}

	for _, limit := range limits {
		if limit.value <= 0 {
			return fmt.Errorf("%s must be a positive number", limit.name)
		}
	}

	if d.Dependencies.MaxDocumentBytes > d.Dependencies.MaxTotalBytes {
		return errors.New("dependencies.max_document_bytes must not exceed dependencies.max_total_bytes")
	}

	return nil
}
