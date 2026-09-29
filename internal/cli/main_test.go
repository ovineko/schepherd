package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/env"
)

// The test binary doubles as the consumer of "schepherd run": with
// consumerMarkerVar set it records its arguments in that file, writes one
// line to stdout and stderr each and exits with consumerExitVar.
const (
	consumerMarkerVar = "SCHEPHERD_CLI_TEST_MARKER"
	consumerExitVar   = "SCHEPHERD_CLI_TEST_EXIT"
)

// unset in runWith's variables removes the variable instead of setting it.
const unset = "\x00unset"

var schepherdKeys = []string{
	env.KeyConfig, env.KeyRepository, env.KeyCatalog, env.KeyCacheDir,
	env.KeyOffline, env.KeyWorkspace, env.KeyTimeout,
}

func TestMain(m *testing.M) {
	if marker, ok := env.Lookup(consumerMarkerVar); ok {
		os.Exit(fakeConsumer(marker))
	}

	os.Exit(m.Run())
}

func fakeConsumer(marker string) int {
	if err := os.WriteFile(marker, []byte(strings.Join(os.Args[1:], "\n")), 0o600); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "fake consumer:", err)

		return 90
	}

	_, _ = fmt.Fprintln(os.Stdout, "consumer stdout")
	_, _ = fmt.Fprintln(os.Stderr, "consumer stderr")

	raw, _ := env.Lookup(consumerExitVar)
	code, _ := strconv.Atoi(raw)

	return code
}

// consumerRunner is a [runner] section that starts the fake consumer in
// batch mode.
func consumerRunner(t *testing.T, marker string, exit int) string {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	if strings.ContainsAny(exe, "{}") || strings.Contains(exe, "${") {
		t.Fatalf("test binary path %q cannot be used as a literal template", exe)
	}

	return "[runner]\ncommand = " + strconv.Quote(exe) + "\nargs = [\"{schema}\", \"{files...}\"]\n\n[runner.env]\n" +
		consumerMarkerVar + " = " + strconv.Quote(marker) + "\n" +
		consumerExitVar + " = \"" + strconv.Itoa(exit) + "\"\n" +
		"GORACE = \"atexit_sleep_ms=0\"\n" +
		"GOCOVERDIR = " + strconv.Quote(t.TempDir()) + "\n"
}

// runWith runs the command line with every SCHEPHERD_* variable empty except
// those in vars.
func runWith(t *testing.T, vars map[string]string, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	for _, key := range schepherdKeys {
		t.Setenv(key, "")
	}

	for key, value := range vars {
		if value == unset {
			t.Setenv(key, "")

			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}

			continue
		}

		t.Setenv(key, value)
	}

	var out, errOut bytes.Buffer

	code = Main(context.Background(), args, strings.NewReader(""), &out, &errOut)

	return out.String(), errOut.String(), code
}
