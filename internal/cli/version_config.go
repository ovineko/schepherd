package cli

import (
	"github.com/spf13/cobra"

	"github.com/ovineko/schepherd/internal/buildinfo"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/internal/fault"
)

// configFormatVersion is the config_version this build understands.
const configFormatVersion = 1

type versionOutput struct {
	buildinfo.Info

	CatalogFormatVersions []int `json:"catalogFormatVersions"`
	ConfigFormatVersions  []int `json:"configFormatVersions"`
}

func (a *app) newVersionCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version of this binary (X.Y.Z[-prerelease] for a release build, dev otherwise)",
		Args:  exactArgs(0, "no arguments"),
		RunE: func(_ *cobra.Command, _ []string) error {
			info := buildinfo.Get()
			if !asJSON {
				return a.writeLine("%s", info.Version)
			}

			return a.writeJSON(versionOutput{
				Info:                  info,
				CatalogFormatVersions: []int{catalog.FormatVersion},
				ConfigFormatVersions:  []int{configFormatVersion},
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "include build metadata and supported format versions")

	return cmd
}

func (a *app) newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect the configuration",
	}

	var asJSON bool

	check := &cobra.Command{
		Use:   "check",
		Short: "Validate the configuration, its extends chain and its local schema files without running anything",
		Args:  exactArgs(0, "no arguments"),
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := a.config()
			if err != nil {
				return err
			}

			if cfg.RunnerConfigured {
				if err := cfg.CheckRunnerEnv(env.Lookup); err != nil {
					return fault.Wrap(fault.Usage, err, "runner")
				}
			}

			if err := cfg.CheckLocalSchemas(); err != nil {
				return fault.Wrap(fault.Usage, err, "local schemas")
			}

			if asJSON {
				return a.writeJSON(cfg.Summary())
			}

			if len(cfg.Files) == 0 {
				return a.writeLine("ok (no configuration file; defaults only)")
			}

			return a.writeLine("ok (%d file(s))", len(cfg.Files))
		},
	}
	check.Flags().BoolVar(&asJSON, "json", false, "print the effective configuration")

	cmd.AddCommand(check)

	return cmd
}
