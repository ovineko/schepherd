package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/testutil/clismoke"
)

// TestRegistryRoundTripInProcess runs the client smoke scenario through Main,
// so the command layer is covered against a registry on every platform and
// under the race detector. cmd/schepherd runs the same scenario on the
// compiled binary.
func TestRegistryRoundTripInProcess(t *testing.T) {
	clismoke.Run(t, func(t *testing.T, dir string, args ...string) clismoke.Result {
		t.Helper()
		t.Chdir(dir)

		var stdout, stderr bytes.Buffer

		code := Main(t.Context(), args, strings.NewReader(""), &stdout, &stderr)

		return clismoke.Result{Stdout: stdout.String(), Stderr: stderr.String(), Code: code}
	})
}
