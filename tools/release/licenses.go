package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/ovineko/schepherd/tools/release/internal/licenses"
)

// licensesPerm makes the generated file world-readable: it is committed and
// shipped in every archive and npm platform package.
const licensesPerm = 0o644

func newLicensesCommand(environ []string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "licenses",
		Short: "Generate and check " + licenses.File + ", the license texts shipped with the client binary",
		Args:  cobra.ArbitraryArgs,
		RunE:  requireSubcommand,
	}

	var root, out string

	generate := &cobra.Command{
		Use:   "generate",
		Short: "Write the license and notice files of every module linked into the client for any release target",
		Long: "Runs `go list -deps` on ./cmd/schepherd for every release target with the toolchain named in go.mod " +
			"and CGO_ENABLED=0, and writes the license, notice and patent files of the Go standard library and of " +
			"each module, read from GOROOT and the module cache, to " + licenses.File + ".",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			data, err := licenses.Generate(cmd.Context(), licenses.Options{Root: root, Env: environ})
			if err != nil {
				return fmt.Errorf("licenses generate: %w", err)
			}

			target := out
			if target == "" {
				target = filepath.Join(root, licenses.File)
			}

			if err := os.WriteFile(target, data, licensesPerm); err != nil {
				return fmt.Errorf("licenses generate: %w", err)
			}

			return writeLine(cmd, target)
		},
	}

	generate.Flags().StringVar(&root, "root", ".", "repository root")
	generate.Flags().StringVar(&out, "out", "", "output file (default: <root>/"+licenses.File+")")

	check := &cobra.Command{
		Use:   "check",
		Short: "Fail when " + licenses.File + " differs from the linked dependencies",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			violations, err := licenses.Check(cmd.Context(), licenses.Options{Root: root, Env: environ})
			if err != nil {
				return fmt.Errorf("licenses check: %w", err)
			}

			if len(violations) > 0 {
				for _, v := range violations {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "violation: "+v)
				}

				return fmt.Errorf("%d third-party license violation(s)", len(violations))
			}

			return writeLine(cmd, "third-party licenses OK")
		},
	}

	check.Flags().StringVar(&root, "root", ".", "repository root")

	cmd.AddCommand(generate, check)

	return cmd
}
