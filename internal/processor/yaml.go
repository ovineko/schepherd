package processor

import (
	"errors"
	"io"
	"os"

	"github.com/ovineko/schepherd/internal/fault"
	"go.yaml.in/yaml/v3"
)

const maxYAMLInspectionBytes = 64 << 20

// YAMLDocumentCount distinguishes single-document input from streams without parsing past the second document.
func YAMLDocumentCount(path string) (count int, resultErr error) {
	// The path is an explicit user-selected run input; only its YAML document count is inspected.
	// bearer:disable go_gosec_filesystem_filereadtaint
	file, err := os.Open(path) //nolint:gosec // G304: YAMLDocumentCount only reads the explicit input path
	if err != nil {
		return 0, fault.Wrap(fault.Usage, err, "open YAML input %s", path)
	}
	defer func() {
		if err := file.Close(); err != nil && resultErr == nil {
			count = 0
			resultErr = fault.Wrap(fault.Usage, err, "close YAML input %s", path)
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return 0, fault.Wrap(fault.Usage, err, "stat YAML input %s", path)
	}
	if info.Size() > maxYAMLInspectionBytes {
		return 0, fault.New(fault.Usage, "YAML input %s exceeds the %d-byte format inspection limit", path, maxYAMLInspectionBytes)
	}
	decoder := yaml.NewDecoder(io.LimitReader(file, maxYAMLInspectionBytes+1))
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			return count, nil
		}
		if err != nil {
			if count > 0 {
				return 2, nil
			}
			return 1, nil
		}
		count++
		if count > 1 {
			return 2, nil
		}
	}
}
