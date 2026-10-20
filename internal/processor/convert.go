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

// Convert runs only a declared yq executable and preserves the original file outside its temporary input.
func Convert(ctx context.Context, executable string, task runner.Task, input, directory string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	// bearer:disable go_gosec_injection_subproc_injection
	// The converter is explicitly bound by trusted configuration; source paths are literal arguments without a shell.
	cmd := exec.CommandContext(ctx, executable, "-p="+Format(input), "-o=json", "-I=0", input) //nolint:gosec // G204: explicitly configured yq executable; input remains a literal argument without a shell
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
		return "", fault.Wrap(fault.ConsumerStart, err, "yq conversion failed for %s", input)
	}
	if stdout.exceeded || stderr.exceeded {
		return "", fault.New(fault.Integrity, "yq conversion output exceeds limit for %s", input)
	}
	if err := jsonutil.Check(stdout.data, jsonutil.DefaultMaxDepth); err != nil {
		return "", fault.Wrap(fault.Usage, err, "yq conversion of %s must produce exactly one JSON document", input)
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
