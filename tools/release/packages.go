package main

import (
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/tools/pins"
	"github.com/ovineko/schepherd/tools/release/internal/wrappers"
)

func newPackagesCommand(environ []string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "packages",
		Short: "Build and smoke-test the npm, PyPI and RubyGems packages, and plan their publication",
		Args:  cobra.ArbitraryArgs,
		RunE:  requireSubcommand,
	}

	cmd.AddCommand(newPackagesBuildCommand(environ), newPackagesPlanCommand(), &cobra.Command{
		Use:   "ruby-image",
		Short: "Print the pinned Ruby image of --ruby docker, so scripts need no copy of the pin",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return writeLine(cmd, pins.RubyImage)
		},
	})

	return cmd
}

func newPackagesBuildCommand(environ []string) *cobra.Command {
	opts := wrappers.BuildOptions{Root: ".", Env: environ}

	var ruby string

	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build the packages of a GoReleaser dist directory with npm pack, uv build and gem build",
		Long: "Stages the binaries with packaging/, LICENSE and " + wrappers.ThirdPartyLicenses + " and builds <out>/npm " +
			"(a platform package per target, then @ovineko/schepherd), <out>/pypi (a wheel per target, with the hatchling " +
			"of packaging/python/build-constraints.txt) and <out>/gem, with SOURCE_DATE_EPOCH set to the commit time of the " +
			"binaries. Every package must hold exactly its staged files; <out>/schepherd_<version>_<kind>_checksums.txt lists " +
			"them in publication order. --smoke then installs the packages of this platform offline and requires each " +
			"launcher to give the output and exit status of the binary for version --json and a failing offline command.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireFlags(cmd, "dist", "version", "out"); err != nil {
				return err
			}

			for _, k := range opts.Kinds {
				if !slices.Contains(wrappers.Kinds, k) {
					return fault.New(fault.Usage, "--kinds: unknown package kind %q", k)
				}
			}

			switch ruby {
			case "host":
			case "docker":
				opts.RubyImage = pins.RubyImage
			default:
				return fault.New(fault.Usage, "--ruby %q, want host or docker", ruby)
			}

			files, err := wrappers.Build(cmd.Context(), opts)
			if err != nil {
				return fmt.Errorf("packages build: %w", err)
			}

			for _, f := range files {
				if err := writeLine(cmd, f); err != nil {
					return err
				}
			}

			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&opts.Dist, "dist", "", "GoReleaser dist directory containing artifacts.json")
	f.StringVar(&opts.Version, "version", "", "release version X.Y.Z[-alpha.N|-beta.N|-rc.N] or snapshot version "+wrappers.SnapshotPrefix+"<commit>")
	f.StringVar(&opts.Out, "out", "", "new or empty directory that receives the packages")
	f.StringSliceVar(&opts.Kinds, "kinds", wrappers.Kinds, "package kinds to build")
	f.BoolVar(&opts.AllTargets, "all-targets", true, "require a binary for every release target")
	f.BoolVar(&opts.Smoke, "smoke", false, "install and run the packages of this platform")
	f.StringVar(&ruby, "ruby", "host", "Ruby that builds and installs the gem: host (Ruby "+pins.RubyVersion+
		" from PATH, as ruby/setup-ruby provides it) or docker (the pinned image "+pins.RubyImage+")")

	return cmd
}

func newPackagesPlanCommand() *cobra.Command {
	var opts wrappers.PlanOptions

	cmd := &cobra.Command{
		Use:   "publish-plan",
		Short: "Print the packages of a release that the registry does not have yet, in publication order",
		Long: "Requires --dir to hold exactly the packages of --kind that the checksum file of packages build lists for " +
			"--version, in publication order (npm platform packages first, @ovineko/schepherd last), with their SHA-256, " +
			"and asks the registry for each. Prints the files still to publish; files the registry has with the same " +
			"bytes go to stderr, so a re-run finishes an interrupted publication. A file the registry has with other " +
			"bytes fails, and so does a snapshot for PyPI or RubyGems.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireFlags(cmd, "kind", "dir", "checksums", "version"); err != nil {
				return err
			}

			if !slices.Contains(wrappers.Kinds, opts.Kind) {
				return fault.New(fault.Usage, "--kind %q, want npm, pypi or gem", opts.Kind)
			}

			plan, err := wrappers.Plan(cmd.Context(), opts)
			if err != nil {
				return fmt.Errorf("packages publish-plan: %w", err)
			}

			for _, p := range plan {
				if p.Published {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "already published: %s\n", p.File)
				} else if err := writeLine(cmd, p.File); err != nil {
					return err
				}
			}

			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&opts.Kind, "kind", "", "npm, pypi or gem")
	f.StringVar(&opts.Dir, "dir", "", "directory holding the packages")
	f.StringVar(&opts.Checksums, "checksums", "", "checksum file written by packages build")
	f.StringVar(&opts.Version, "version", "", "SemVer version of the release")
	f.StringVar(&opts.Registry, "registry", "", "registry to ask (default: "+wrappers.Registries[wrappers.Npm]+", "+wrappers.Registries[wrappers.PyPI]+" or "+wrappers.Registries[wrappers.Gem]+")")

	return cmd
}
