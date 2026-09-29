package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/tools/release/internal/notices"
)

func newNoticesCommand(environ []string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "notices",
		Short: "Generate and check " + notices.File + ", the third-party notices of the repository",
		Args:  cobra.ArbitraryArgs,
		RunE:  requireSubcommand,
	}

	var root, statePath, out string

	generate := &cobra.Command{
		Use:   "generate",
		Short: "Write " + notices.File + " from the linked modules, the pinned tools and images and the catalog state",
		Long: "Lists the Go modules that `go list -deps` reports for ./cmd/schepherd and ./cmd/schepherd-publisher on every " +
			"release target, with the license identified from each license file (an unrecognized license is an error), " +
			"the copied packages of the module, the tools pinned in tools/pins, the Python requirements and container " +
			"images named in " + notices.DataFile + ", and the schemas of the catalog that --state records, grouped by " +
			"the sources of their content and their licenses. Without --state, a missing " + notices.StateFile + " means " +
			"that no catalog has been published; a --state file must exist.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if statePath != "" {
				if _, err := os.Stat(statePath); err != nil {
					return fault.Wrap(fault.Usage, err, "--state")
				}
			}

			data, err := notices.Generate(cmd.Context(), notices.Options{Root: root, State: statePath, Env: environ})
			if err != nil {
				return fault.Reclassify(fault.Internal, err, "notices generate")
			}

			target := out
			if target == "" {
				target = filepath.Join(root, notices.File)
			}

			if err := os.WriteFile(target, data, licensesPerm); err != nil {
				return fault.Wrap(fault.Internal, err, "notices generate")
			}

			return writeLine(cmd, target)
		},
	}

	generate.Flags().StringVar(&root, "root", ".", "repository root")
	generate.Flags().StringVar(&statePath, "state", "", "catalog state file (default: <root>/"+notices.StateFile+")")
	generate.Flags().StringVar(&out, "out", "", "output file (default: <root>/"+notices.File+")")

	check := &cobra.Command{
		Use:   "check",
		Short: "Fail when " + notices.File + " differs from what notices generate writes",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			violations, err := notices.Check(cmd.Context(), notices.Options{Root: root, Env: environ})
			if err != nil {
				return fault.Reclassify(fault.Internal, err, "notices check")
			}

			if len(violations) > 0 {
				for _, v := range violations {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "violation: "+v)
				}

				return fault.New(fault.Internal, "%d third-party notices violation(s)", len(violations))
			}

			return writeLine(cmd, "third-party notices OK")
		},
	}

	check.Flags().StringVar(&root, "root", ".", "repository root")

	cmd.AddCommand(generate, check)

	return cmd
}
