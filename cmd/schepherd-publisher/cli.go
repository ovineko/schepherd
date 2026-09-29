package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/ovineko/schepherd/internal/buildinfo"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/registry"
)

const name = "schepherd-publisher"

type app struct {
	stdout io.Writer
	stderr io.Writer
	now    func() time.Time
	root   *cobra.Command
	quiet  bool
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	a := &app{stdout: stdout, stderr: stderr, now: time.Now}
	a.root = a.newRoot() //nolint:contextcheck // commands read ctx with cmd.Context(); bundle.FindTool bounds its version probe with its own timeout and takes no context
	a.root.SetArgs(args)
	a.root.SetOut(stdout)
	a.root.SetErr(stderr)

	err := a.root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}

	if _, classified := errors.AsType[*fault.Error](err); !classified {
		err = fault.Wrap(fault.Usage, err, "invalid command line")
	}

	kind := fault.KindOf(err)
	_, _ = fmt.Fprintf(a.stderr, "%s: %s error: %v\n", name, kind, err)

	if kind == fault.Usage {
		_, _ = fmt.Fprintf(a.stderr, "Run '%s --help' for usage.\n", name)
	}

	return fault.ExitCodeOf(err)
}

func (a *app) newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   name,
		Short: "Prepare upstream JSON Schemas and publish them as Schepherd catalog revisions",
		Long: "schepherd-publisher is the maintainer tool of Schepherd. 'prepare' turns a source description into\n" +
			"verified, self-contained schemas; 'diff' compares them with the publisher state (catalog/state.json)\n" +
			"without a registry; 'publish' packs what changed into OCI artifacts, pushes a new catalog revision and\n" +
			"writes the new state; 'latest' points catalog-latest at the catalog the state records. Results go to\n" +
			"standard output, progress and diagnostics to standard error.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return fault.Wrap(fault.Usage, err, "invalid flag")
	})

	root.PersistentFlags().BoolVar(&a.quiet, "quiet", false, "print only errors on stderr")

	root.AddCommand(a.newPrepareCmd(), a.newDiffCmd(), a.newPublishCmd(), a.newLatestCmd(), a.newVersionCmd())

	return root
}

func (a *app) newVersionCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version of this binary (dev for non-release builds)",
		Args:  noArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			info := buildinfo.Get()
			if asJSON {
				return a.writeJSON(info)
			}

			return a.writeText(info.Version + "\n")
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "include build metadata")

	return cmd
}

func noArgs(_ *cobra.Command, args []string) error {
	if len(args) > 0 {
		return fault.New(fault.Usage, "unexpected arguments %q; this command takes flags only", args)
	}

	return nil
}

func (a *app) logf(format string, args ...any) {
	if a.quiet {
		return
	}

	_, _ = fmt.Fprintf(a.stderr, name+": "+format+"\n", args...)
}

func (a *app) writeJSON(v any) error {
	enc := json.NewEncoder(a.stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")

	if err := enc.Encode(v); err != nil {
		return fault.Wrap(fault.Internal, err, "write result")
	}

	return nil
}

func (a *app) writeText(s string) error {
	if _, err := io.WriteString(a.stdout, s); err != nil {
		return fault.Wrap(fault.Internal, err, "write result")
	}

	return nil
}

func markRequired(cmd *cobra.Command, flags ...string) {
	for _, flag := range flags {
		if err := cmd.MarkFlagRequired(flag); err != nil {
			panic(err)
		}
	}
}

// openRepository parses repository and opens it with the per-host settings
// of the registry configuration file.
func openRepository(repository, registryConfig string) (*registry.Repo, error) {
	repo, err := registry.ParseRepository(repository)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "--repository")
	}

	hosts, err := loadRegistries(registryConfig)
	if err != nil {
		return nil, err
	}

	client := registry.NewClient(registry.Options{Hosts: hosts, UserAgent: name + "/" + buildinfo.Get().Version})

	target, err := client.Open(repo)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "open %s", repo)
	}

	return target, nil
}

// defaultBundler is where 'go run ./tools/install-jsonschema' installs the
// pinned bundler, relative to the module root.
func defaultBundler() string {
	path := filepath.Join(".tools", "bin", "jsonschema")
	if runtime.GOOS == "windows" {
		path += ".exe"
	}

	return path
}
