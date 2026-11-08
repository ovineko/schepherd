package processor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/jsonutil"
	"github.com/ovineko/schepherd/internal/runner"
)

// Convert runs only a declared converter and preserves the original file outside its temporary input.
func Convert(ctx context.Context, executable string, task runner.Task, input, directory string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	args := task.Args[1:]
	if len(args) == 0 {
		return "", fault.New(fault.Usage, "converter arguments are required")
	}
	// The converter executable and literal argv come from trusted processor configuration; no shell is used.
	// bearer:disable go_gosec_injection_subproc_injection
	cmd := exec.CommandContext(ctx, executable, args...) //nolint:gosec // G204: explicitly configured converter executable
	cmd.WaitDelay = runner.DefaultKillGrace
	cmd.Env = task.Env
	cmd.Dir = task.Dir
	var stdout, stderr boundedOutput
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			kind := fault.ConsumerTimeout
			if errors.Is(ctx.Err(), context.Canceled) {
				kind = fault.Canceled
			}
			return "", fault.Wrap(kind, ctx.Err(), "convert %s", input)
		}
		return "", fault.Wrap(fault.ConsumerStart, err, "converter %s failed for %s", executable, input)
	}
	if stdout.exceeded || stderr.exceeded {
		return "", fault.New(fault.Integrity, "converter output exceeds limit for %s", input)
	}
	if err := jsonutil.Check(stdout.data, jsonutil.DefaultMaxDepth); err != nil {
		return "", fault.Wrap(fault.Usage, err, "converter output for %s must be exactly one JSON document", input)
	}
	file, err := os.CreateTemp(directory, "input-*.json")
	if err != nil {
		return "", fault.Wrap(fault.Internal, err, "create converted input")
	}
	path := file.Name()
	_, writeErr := file.Write(stdout.data)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return "", fault.Wrap(fault.Internal, err, "write converted input")
	}
	return filepath.Clean(path), nil
}
