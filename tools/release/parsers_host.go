package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/ovineko/schepherd/internal/fault"
)

func checkParserHost(ctx context.Context, dir string, environ []string) error {
	module, err := filepath.Abs(filepath.Join(dir, parserFilename))
	if err != nil {
		return fault.Wrap(fault.Internal, err, "resolve parser module")
	}
	input := `{"reportVersion":2,"workspace":"/workspace","mode":"per-file","exitCode":2,"tasks":[],"skipped":[],"error":{"kind":"usage","message":"host smoke diagnostic","file":"/workspace/config.json"}}`
	cmd := parserCommand(ctx, "", environ, "pnpm", "--silent", "dm", "devtools", "parsers", "run", "schepherd", "--wasm", module, "--exit-code", "2")
	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fault.New(fault.Integrity, "Datamitsu parser host failed: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var result struct {
		ABI         int  `json:"abi"`
		Recognized  bool `json:"recognized"`
		Diagnostics []struct {
			Message string `json:"message"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return fault.Wrap(fault.Integrity, err, "decode Datamitsu parser host output")
	}
	if result.ABI != 2 || !result.Recognized || len(result.Diagnostics) != 1 || !strings.Contains(result.Diagnostics[0].Message, "host smoke diagnostic") {
		return fault.New(fault.Integrity, "Datamitsu parser host did not return the expected classified diagnostic")
	}
	return nil
}
