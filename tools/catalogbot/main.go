// Command catalogbot is the Git side of the weekly schema catalog update in
// .github/workflows/update-schemas.yml: it records the publisher's state on
// the main branch as one commit that GitHub signs, tags and releases the
// catalog revision, writes the release notes and the job summary of every
// run, and tells whether an earlier run left a revision unfinished. It never
// talks to the OCI registry; that is schepherd-publisher's job.
//
// Run it with `go run ./tools/catalogbot <command>`. It reads GH_TOKEN,
// GITHUB_API_URL and GITHUB_GRAPHQL_URL from the environment and never
// prints the token. Exit status follows internal/fault: 0 success, 1
// internal error, 2 invalid usage, 3 not found, 4 GitHub API or network
// failure, 5 conflicting state (a moved tag, files changed under the commit).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/internal/fault"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, env.Lookup)

	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, lookup env.LookupFunc) int {
	cfg := loadSettings(lookup)

	root := newRoot(cfg, stderr) //nolint:contextcheck // commands receive ctx from ExecuteContext and read it with cmd.Context()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)

	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}

	if errors.Is(err, context.Canceled) {
		_, _ = fmt.Fprintf(stderr, "catalogbot: %s\n", cfg.redact(err.Error()))

		return fault.Canceled.ExitCode()
	}

	// Cobra reports missing required flags and unknown commands unclassified.
	if _, classified := errors.AsType[*fault.Error](err); !classified {
		err = fault.Wrap(fault.Usage, err, "invalid command line")
	}

	_, _ = fmt.Fprintf(stderr, "catalogbot: %s\n", cfg.redact(err.Error()))

	return fault.ExitCodeOf(err)
}

// Environment variables the bot reads. GitHub Actions sets the API URLs;
// the token comes from the GitHub App step of the workflow.
const (
	keyToken      = "GH_TOKEN"
	keyAPIURL     = "GITHUB_API_URL"
	keyGraphQLURL = "GITHUB_GRAPHQL_URL"
)

type settings struct {
	token      string
	apiURL     string
	graphqlURL string
}

func loadSettings(lookup env.LookupFunc) settings {
	get := func(key string) string {
		value, _ := lookup(key)

		return strings.TrimSpace(value)
	}

	return settings{token: get(keyToken), apiURL: get(keyAPIURL), graphqlURL: get(keyGraphQLURL)}
}

// redact removes the token from text that is about to be printed, in case
// an error ever quotes a request.
func (s settings) redact(text string) string {
	if s.token == "" {
		return text
	}

	return strings.ReplaceAll(text, s.token, "***")
}
