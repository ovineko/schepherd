package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/testutil/clismoke"
)

// TestCompiledClientSmoke drives the compiled client as a subprocess against
// a registry, then from the warm cache alone. It is the binary-level smoke
// test for every platform in the CI matrix.
func TestCompiledClientSmoke(t *testing.T) {
	bin := clismoke.Build(t, "schepherd", "github.com/ovineko/schepherd/cmd/schepherd")

	clismoke.Run(t, func(t *testing.T, dir string, args ...string) clismoke.Result {
		t.Helper()

		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()

		var stdout, stderr bytes.Buffer

		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = dir
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		code := 0

		if err := cmd.Run(); err != nil {
			exitErr, ok := errors.AsType[*exec.ExitError](err)
			if !ok {
				t.Fatalf("start %s: %v", bin, err)
			}

			code = exitErr.ExitCode()
		}

		return clismoke.Result{Stdout: stdout.String(), Stderr: stderr.String(), Code: code}
	})
}
