// Command release is the maintainer tooling for Schepherd client releases:
// SemVer checks of release tags for workflows, the npm, PyPI and RubyGems
// packages of the GoReleaser binaries with resumable publication plans, the
// third-party license and notice files, and the end-to-end report.
//
// Run it from the repository root with `go run ./tools/release <command>`.
// Exit status: 0 success, 1 failure or failed check, 2 invalid usage.
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

	"github.com/spf13/cobra"

	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/internal/fault"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, env.Environ())

	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, environ []string) int {
	root := newRoot(environ) //nolint:contextcheck // commands receive ctx from ExecuteContext and read it with cmd.Context()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)

	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}

	_, _ = fmt.Fprintf(stderr, "release: %v\n", err)

	if errors.Is(err, context.Canceled) {
		return fault.Canceled.ExitCode()
	}

	return fault.ExitCodeOf(err)
}

func newRoot(environ []string) *cobra.Command {
	root := &cobra.Command{
		Use:           "release",
		Short:         "Schepherd release tooling",
		Args:          cobra.ArbitraryArgs,
		RunE:          requireSubcommand,
		SilenceUsage:  true,
		SilenceErrors: true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
	}

	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return fault.Wrap(fault.Usage, err, "invalid flags")
	})

	root.AddCommand(
		newLicensesCommand(environ),
		newNoticesCommand(environ),
		newPackagesCommand(environ),
		newE2EReportCommand(),
		newE2EStreamCommand(),
		newVersionCommand(environ),
	)

	return root
}

// usageArgs classifies argument count errors as usage errors.
func usageArgs(check cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := check(cmd, args); err != nil {
			return fault.Wrap(fault.Usage, err, "%s", cmd.CommandPath())
		}

		return nil
	}
}

// suggestionDistance is cobra's default for its own unknown-command
// suggestions, which SuggestionsFor does not apply by itself.
const suggestionDistance = 2

// requireSubcommand is the RunE of commands that only group subcommands.
// Without it cobra prints help and exits 0 for a mistyped subcommand of a
// group, and reports an unclassified error for one of the root command.
func requireSubcommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return fault.New(fault.Usage, "%s requires a command; see %s --help", cmd.CommandPath(), cmd.CommandPath())
	}

	if cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = suggestionDistance
	}

	msg := fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath())
	if suggestions := cmd.SuggestionsFor(args[0]); len(suggestions) > 0 {
		msg += "; did you mean " + strings.Join(suggestions, " or ") + "?"
	}

	return fault.New(fault.Usage, "%s", msg)
}

func requireFlags(cmd *cobra.Command, names ...string) error {
	for _, name := range names {
		if flag := cmd.Flags().Lookup(name); flag == nil || flag.Value.String() == "" {
			return fault.New(fault.Usage, "--%s is required", name)
		}
	}

	return nil
}
