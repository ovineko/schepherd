package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/env"
)

// servedCatalog starts a registry with a published catalog and returns the
// global flags that point schepherd at it with a fresh cache.
func servedCatalog(t *testing.T, extraConfig string) (reg *testRegistry, base []string) {
	t.Helper()
	isolateDockerConfig(t)

	reg = newTestRegistry(t)
	repo := reg.repo("org/schemas", "", "")
	dgst := reg.publishCatalog(t, "org/schemas")

	return reg, []string{"--config", reg.writeConfig(t, extraConfig), "--cache-dir", t.TempDir(), "--repository", repo, "--catalog", dgst}
}

func TestPathNull(t *testing.T) {
	_, base := servedCatalog(t, "")

	stdout, stderr, code := run(t, append(base, "path", "--null", "alpha")...)
	if code != 0 {
		t.Fatalf("path --null: exit %d, stderr %s", code, stderr)
	}

	path, ok := strings.CutSuffix(stdout, "\x00")
	if !ok || strings.ContainsAny(path, "\x00\n") || !filepath.IsAbs(path) {
		t.Fatalf("path --null printed %q, want one absolute path terminated by a single NUL", stdout)
	}

	if data, err := os.ReadFile(path); err != nil || string(data) != testSchemas["alpha"] {
		t.Errorf("%s = %q, %v", path, data, err)
	}

	plain, _, code := run(t, append(base, "path", "alpha")...)
	if code != 0 || plain != path+"\n" {
		t.Errorf("path without --null = %q (exit %d), want %q", plain, code, path+"\n")
	}
}

func TestQuietSilencesOnlySchepherdDiagnostics(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "consumer-ran")
	_, base := servedCatalog(t, consumerRunner(t, marker, 3))
	input := filepath.Join(t.TempDir(), "alpha.json")

	if err := os.WriteFile(input, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	withCache := func(args ...string) []string {
		return append(append(append([]string{}, base...), "--cache-dir", t.TempDir()), args...)
	}

	_, stderr, code := run(t, withCache("path", "alpha")...)
	if code != 0 || !strings.Contains(stderr, "schepherd: fetching catalog") {
		t.Fatalf("without --quiet: exit %d, stderr %q; want the fetch progress", code, stderr)
	}

	stdout, stderr, code := run(t, withCache("--quiet", "path", "alpha")...)
	if code != 0 || stdout == "" || stderr != "" {
		t.Fatalf("--quiet path: exit %d, stdout %q, stderr %q; want a result and no diagnostics", code, stdout, stderr)
	}

	stdout, stderr, code = run(t, withCache("--quiet", "path", "missing")...)
	if code != 3 || stdout != "" || !strings.HasPrefix(stderr, "schepherd: not-found error: ") {
		t.Errorf("--quiet must still report errors: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	for _, quiet := range []bool{false, true} {
		args := withCache("run", "--schema", "alpha", "--", input)
		if quiet {
			args = append([]string{"--quiet"}, args...)
		}

		stdout, stderr, code := run(t, args...)
		if code != 3 || stdout != "consumer stdout\n" || !strings.Contains(stderr, "consumer stderr\n") {
			t.Fatalf("run (quiet %v): exit %d, stdout %q, stderr %q; want the consumer's status and output", quiet, code, stdout, stderr)
		}

		if reported := strings.Contains(stderr, "schepherd: consumer exited with status 3"); reported == quiet {
			t.Errorf("run (quiet %v): stderr %q", quiet, stderr)
		}

		if quiet && strings.Contains(stderr, "schepherd:") {
			t.Errorf("--quiet run printed schepherd diagnostics: %q", stderr)
		}
	}
}

func TestTimeoutBoundsRegistryWork(t *testing.T) {
	// Long enough that a slow runner reaches the registry before it expires.
	const deadline = 2 * time.Second

	cases := []struct {
		vars map[string]string
		name string
		args []string
	}{
		{name: "--timeout", args: []string{"--timeout", deadline.String()}},
		{name: env.KeyTimeout, vars: map[string]string{env.KeyTimeout: deadline.String()}},
		{name: "--timeout beats " + env.KeyTimeout, vars: map[string]string{env.KeyTimeout: "1h"}, args: []string{"--timeout", deadline.String()}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, base := servedCatalog(t, "")
			reg.setHang(true)

			start := time.Now()
			stdout, stderr, code := runWith(t, tc.vars, append(append(tc.args, base...), "path", "alpha")...)
			elapsed := time.Since(start)

			if code != 4 || stdout != "" || !strings.Contains(stderr, "schepherd: registry error: ") || !strings.Contains(stderr, "timed out") {
				t.Fatalf("exit %d, stdout %q, stderr %q; want exit 4 and a timeout", code, stdout, stderr)
			}

			if elapsed < deadline || elapsed > 20*time.Second {
				t.Errorf("returned after %s with a deadline of %s", elapsed, deadline)
			}

			if reg.requestCount() == 0 {
				t.Error("the registry was never asked")
			}
		})
	}
}
