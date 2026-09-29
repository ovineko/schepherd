package config

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/jsonutil"
)

// Read returns the content of the local schema file after checking that it
// is a readable regular file (a symlink is followed) of at most maxBytes that
// holds exactly one valid JSON document. Only well-formedness is checked:
// the client has no JSON Schema engine, so whether the document is a usable
// schema is up to the consumer. Every error is fault.Usage and names the file
// and the declaring configuration with the key schemas.<id>.path.
func (s *LocalSchema) Read(maxBytes int64) ([]byte, error) {
	info, err := os.Stat(s.Path)
	if err != nil {
		return nil, s.wrap(err)
	}

	switch {
	case info.IsDir():
		return nil, s.fail("%s is a directory, not a JSON Schema file", s.Path)
	case !info.Mode().IsRegular():
		return nil, s.fail("%s is not a regular file", s.Path)
	case info.Size() > maxBytes:
		return nil, s.fail("%s is larger than limits.max_schema_bytes (%d bytes)", s.Path, maxBytes)
	}

	// Stat refused FIFOs and devices above, so opening cannot block.
	f, err := os.Open(s.Path)
	if err != nil {
		return nil, s.wrap(err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, s.wrap(fmt.Errorf("read %s: %w", s.Path, err))
	}

	if int64(len(data)) > maxBytes {
		return nil, s.fail("%s is larger than limits.max_schema_bytes (%d bytes)", s.Path, maxBytes)
	}

	if err := jsonutil.Check(data, 0); err != nil {
		return nil, s.fail("%s is not valid JSON: %v", s.Path, err)
	}

	return data, nil
}

func (s *LocalSchema) context() string {
	if s.at != "" {
		return s.at
	}

	return s.DeclaredIn + ": " + joinKey(joinKey("schemas", s.ID), "path")
}

func (s *LocalSchema) fail(format string, args ...any) error {
	return fault.New(fault.Usage, "%s: %s", s.context(), fmt.Sprintf(format, args...))
}

func (s *LocalSchema) wrap(err error) error {
	return fault.Reclassify(fault.Usage, err, "%s", s.context())
}

// CheckLocalSchemas reads every local schema as Read does and reports every
// problem at once.
func (c *Config) CheckLocalSchemas() error {
	var problems []string

	for i := range c.LocalSchemas {
		if _, err := c.LocalSchemas[i].Read(c.Limits.MaxSchemaBytes); err != nil {
			problems = append(problems, err.Error())
		}
	}

	if len(problems) == 0 {
		return nil
	}

	return fault.New(fault.Usage, "%s", strings.Join(problems, "\n"))
}
