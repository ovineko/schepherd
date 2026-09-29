// Command smokeconsumer stands in for a schema validator in client smoke
// tests; it is never shipped. It is configured only through its arguments, so
// a test needs neither a shell nor extra environment variables:
//
//	smokeconsumer -record-dir <dir> -schema-id <id> [-schema-ref <ref>] [-fail-if-contains <text>] [-fail-status <n>] <schema> <files...>
//
// It writes <dir>/<schema-id>.json with the schema ref, the absolute path and
// hex SHA-256 of the schema and of every input, prints "smokeconsumer: <schema-id> <n>
// file(s)" to stdout and exits with -fail-status when an input contains
// -fail-if-contains.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// exitHarnessError tells a broken consumer apart from a configured failure.
const exitHarnessError = 99

// File identifies one file the consumer was given.
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Record is what one invocation writes to <record-dir>/<schema-id>.json.
type Record struct {
	SchemaID  string `json:"schemaId"`
	SchemaRef string `json:"schemaRef"`
	Cwd       string `json:"cwd"`
	Schema    File   `json:"schema"`
	Files     []File `json:"files"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("smokeconsumer", flag.ContinueOnError)
	flags.SetOutput(stderr)

	recordDir := flags.String("record-dir", "", "directory that receives <schema-id>.json")
	schemaID := flags.String("schema-id", "", "schema id, used as the record name")
	schemaRef := flags.String("schema-ref", "", "schema ref, recorded as given")
	failIfContains := flags.String("fail-if-contains", "", "fail when an input contains this text")
	failStatus := flags.Int("fail-status", 1, "exit status for a failing input")

	if err := flags.Parse(args); err != nil {
		return exitHarnessError
	}

	if *recordDir == "" || *schemaID == "" || flags.NArg() < 2 {
		_, _ = fmt.Fprintln(stderr, "smokeconsumer: -record-dir, -schema-id, a schema and at least one file are required")

		return exitHarnessError
	}

	status, err := consume(Record{SchemaID: *schemaID, SchemaRef: *schemaRef}, *recordDir, *failIfContains, *failStatus, flags.Args(), stdout)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "smokeconsumer: %v\n", err)

		return exitHarnessError
	}

	return status
}

func consume(rec Record, recordDir, failIfContains string, failStatus int, paths []string, stdout io.Writer) (int, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return 0, fmt.Errorf("working directory: %w", err)
	}

	schema, schemaData, err := inspect(paths[0])
	if err != nil {
		return 0, err
	}

	if !json.Valid(schemaData) {
		return 0, errors.New("the schema is not valid JSON")
	}

	rec.Cwd, rec.Schema, rec.Files = cwd, schema, make([]File, 0, len(paths)-1)
	status := 0

	for _, p := range paths[1:] {
		file, data, err := inspect(p)
		if err != nil {
			return 0, err
		}

		rec.Files = append(rec.Files, file)

		if failIfContains != "" && bytes.Contains(data, []byte(failIfContains)) {
			status = failStatus
		}
	}

	data, err := json.Marshal(rec)
	if err != nil {
		return 0, fmt.Errorf("encode record: %w", err)
	}

	if err := os.WriteFile(filepath.Join(recordDir, rec.SchemaID+".json"), data, 0o600); err != nil {
		return 0, fmt.Errorf("write record: %w", err)
	}

	if _, err := fmt.Fprintf(stdout, "smokeconsumer: %s %d file(s)\n", rec.SchemaID, len(rec.Files)); err != nil {
		return 0, fmt.Errorf("write stdout: %w", err)
	}

	return status, nil
}

//nolint:gosec // reading the files named on the command line is the purpose of this fake consumer
func inspect(path string) (File, []byte, error) {
	if !filepath.IsAbs(path) {
		return File{}, nil, fmt.Errorf("argument %q is not an absolute path", path)
	}

	// bearer:disable go_gosec_filesystem_filereadtaint
	// Reading the files named on the command line is the purpose of this fake consumer.
	data, err := os.ReadFile(path)
	if err != nil {
		return File{}, nil, fmt.Errorf("read argument: %w", err)
	}

	sum := sha256.Sum256(data)

	return File{Path: path, SHA256: hex.EncodeToString(sum[:])}, data, nil
}
