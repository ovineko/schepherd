package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/ovineko/schepherd/internal/fault"
)

func validParserHostResult(result parserHostResult) bool {
	if result.ABI != 2 || !result.Recognized || !result.Partial || result.ExtractionPartial || len(result.Diagnostics) != 1 || !strings.Contains(result.Diagnostics[0].Message, "host smoke diagnostic") {
		return false
	}
	if result.Skipped == nil {
		return true
	}
	return len(result.Skipped) == 1 && result.Skipped[0].File == "/workspace/skip.toml" && result.Skipped[0].Reason == "no compatible processor" && result.Skipped[0].Category == "catalog-format-unavailable"
}

type parserHostResult struct {
	ABI               int  `json:"abi"`
	Recognized        bool `json:"recognized"`
	Partial           bool `json:"partial"`
	ExtractionPartial bool `json:"extractionPartial"`
	Skipped           []struct {
		File     string `json:"file"`
		Reason   string `json:"reason"`
		Category string `json:"category"`
	} `json:"skipped"`
	Diagnostics []struct {
		Message string `json:"message"`
	} `json:"diagnostics"`
}

func checkParserHost(ctx context.Context, dir string, environ []string) error {
	module, err := filepath.Abs(filepath.Join(dir, parserFilename))
	if err != nil {
		return fault.Wrap(fault.Internal, err, "resolve parser module")
	}
	input := `{"reportVersion":3,"workspace":"/workspace","mode":"per-file","exitCode":2,"tasks":[],"skipped":[{"file":"/workspace/skip.toml","reason":"no compatible processor","category":"catalog-format-unavailable"}],"error":{"kind":"usage","message":"host smoke diagnostic","file":"/workspace/config.json"}}`
	cmd := parserCommand(ctx, "", environ, "pnpm", "--silent", "dm", "devtools", "parsers", "run", "schepherd", "--wasm", module, "--exit-code", "2")
	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fault.New(fault.Integrity, "Datamitsu parser host failed: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var result parserHostResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return fault.Wrap(fault.Integrity, err, "decode Datamitsu parser host output")
	}
	if !validParserHostResult(result) {
		return fault.New(fault.Integrity, "Datamitsu parser host did not return the expected classified diagnostic and compatible partial result")
	}
	return nil
}
