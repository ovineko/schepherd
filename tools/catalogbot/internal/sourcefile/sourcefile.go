// Package sourcefile records another upstream commit in the SchemaStore
// source description (sources/schemastore.toml) without touching anything
// else in it, comments included, so the bot's commit shows exactly that one
// line changing.
package sourcefile

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/upstream"
)

const maxSourceBytes = 1 << 20

var (
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// commitLine matches the top-level commit key as the repository writes
	// it; any other spelling is refused rather than guessed at.
	commitLine = regexp.MustCompile(`(?m)^commit = "[0-9a-f]{40}"\r?$`)
	tableLine  = regexp.MustCompile(`(?m)^\s*\[`)
)

// SetCommit returns data with the value of its top-level commit key replaced.
func SetCommit(data []byte, commit string) ([]byte, error) {
	if !commitPattern.MatchString(commit) {
		return nil, fault.New(fault.Usage, "commit %q must be 40 lowercase hex digits", commit)
	}

	matches := commitLine.FindAllIndex(data, -1)
	if len(matches) != 1 {
		return nil, fault.New(fault.Usage, "the source description must have exactly one line `commit = \"<40 hex digits>\"`, found %d", len(matches))
	}

	if table := tableLine.FindIndex(data); table != nil && table[0] < matches[0][0] {
		return nil, fault.New(fault.Usage, "the commit key must come before the first table")
	}

	const key = `commit = "`

	start := matches[0][0]

	var out bytes.Buffer

	out.Write(data[:start])
	out.WriteString(key + commit)
	out.Write(data[start+len(key)+len(commit):])

	return out.Bytes(), nil
}

// Rewrite reads the source description at in, records commit and writes the
// result to out. The result must load as an upstream source description
// that differs from the input in the commit only.
func Rewrite(in, out, commit string) error {
	data, err := readLimited(in)
	if err != nil {
		return err
	}

	before, err := upstream.LoadSchemaStoreConfig(in)
	if err != nil {
		return err //nolint:wrapcheck // already fault.Usage
	}

	updated, err := SetCommit(data, commit)
	if err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Clean(out), updated, 0o600); err != nil {
		return fault.Wrap(fault.Usage, err, "write %s", out)
	}

	after, err := upstream.LoadSchemaStoreConfig(out)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "the rewritten source description does not load")
	}

	want := *before
	want.Commit = commit

	if *after != want {
		return fault.New(fault.Internal, "the rewritten source description changed more than the commit")
	}

	return nil
}

func readLimited(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "source description")
	}

	if !info.Mode().IsRegular() || info.Size() > maxSourceBytes {
		return nil, fault.New(fault.Usage, "source description %s must be a regular file of at most %d bytes", path, maxSourceBytes)
	}

	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "source description")
	}

	return data, nil
}
