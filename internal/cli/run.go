package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/match"
	"github.com/ovineko/schepherd/internal/runner"
)

type runFlags struct {
	schema          string
	report          string
	ignoreUnmatched bool
}

func (a *app) newRunCmd() *cobra.Command {
	var f runFlags

	cmd := &cobra.Command{
		Use:   "run [--schema <id>] [--ignore-unmatched] [--report <file>] -- <files...>",
		Short: "Run the configured consumer for the given files",
		Long: "Resolves a schema for every file (or uses --schema), materializes the catalog schemas,\n" +
			"checks the local ones in place and starts the consumer from the [runner] section of the\n" +
			"configuration without a shell.\n" +
			"Files are taken literally; directories are not walked and globs are not expanded.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.run(cmd, f, args)
		},
	}
	cmd.Flags().StringVar(&f.schema, "schema", "", "use this schema id for every file")
	cmd.Flags().BoolVar(&f.ignoreUnmatched, "ignore-unmatched", false, "skip files that match no schema (ambiguous files still fail)")
	cmd.Flags().StringVar(&f.report, "report", "", "write a JSON report to this file")

	return cmd
}

func (a *app) run(cmd *cobra.Command, f runFlags, files []string) error {
	if len(files) == 0 {
		return fault.New(fault.Usage, "no input files; pass them after --")
	}

	cfg, err := a.config()
	if err != nil {
		return err
	}

	if !cfg.RunnerConfigured {
		return fault.New(fault.Usage, "no [runner] section with a command in the configuration")
	}

	if err := cfg.CheckRunnerEnv(env.Lookup); err != nil {
		return fault.Wrap(fault.Usage, err, "runner")
	}

	paths, err := inputPaths(files)
	if err != nil {
		return err
	}

	ctx, cancel, err := a.withTimeout(cmd.Context())
	if err != nil {
		return err
	}
	defer cancel()

	src, err := a.openSources(ctx)
	if err != nil {
		return err
	}
	defer src.close()

	inputs, skipped, err := a.assignSchemas(ctx, src, f, paths)
	if err != nil {
		return err
	}

	schemas := make(map[string]runner.SchemaInfo)

	for _, in := range inputs {
		if _, done := schemas[in.SchemaID]; done {
			continue
		}

		sch, err := src.prepare(ctx, in.SchemaID)
		if err != nil {
			return err
		}

		schemas[in.SchemaID] = sch.info
	}

	cancel()

	cacheDir, err := src.cacheDir()
	if err != nil {
		return err
	}

	opts := runner.Options{
		Spec:      cfg.Runner,
		Workspace: cfg.Workspace,
		CacheDir:  cacheDir,
		Lookup:    env.Lookup,
		Environ:   env.Environ(),
		Stdout:    a.stdout,
		Stderr:    a.stderr,
	}

	report := &runner.Report{ReportVersion: 1, Mode: cfg.Runner.Mode, Tasks: []runner.TaskReport{}, Skipped: []runner.Skipped{}}

	var runErr error

	if len(inputs) > 0 {
		plan, err := runner.BuildPlan(inputs, schemas, opts)
		if err != nil {
			runErr = fault.Wrap(fault.Usage, err, "runner")
			report.ExitCode = fault.ExitCodeOf(runErr)

			if failed := runner.PlanFailureReport(inputs, schemas, cfg.Runner.Mode, runErr); failed != nil {
				report = failed
			}
		} else {
			report, runErr = runner.Execute(cmd.Context(), plan, opts)
		}
	}

	report.Skipped = append(report.Skipped, skipped...)

	if f.report != "" {
		if err := runner.WriteReport(f.report, report); err != nil {
			return errors.Join(runErr, fault.Wrap(fault.Internal, err, "write report"))
		}
	}

	if runErr != nil {
		return fault.Wrap(fault.Internal, runErr, "run")
	}

	return nil
}

func inputPaths(files []string) ([]string, error) {
	paths := make([]string, 0, len(files))

	for _, file := range files {
		abs, err := filepath.Abs(file)
		if err != nil {
			return nil, fault.Wrap(fault.Usage, err, "input %q", file)
		}

		info, err := os.Stat(abs)
		if err != nil {
			return nil, fault.Wrap(fault.Usage, err, "input %s", abs)
		}

		if !info.Mode().IsRegular() {
			return nil, fault.New(fault.Usage, "input %s is not a regular file", abs)
		}

		// bearer:disable go_gosec_filesystem_filereadtaint
		// Opening the inputs the user named is the purpose; only readability is checked.
		fh, err := os.Open(abs) //nolint:gosec // opening the inputs the user named is the purpose; the file is only checked for readability
		if err != nil {
			return nil, fault.Wrap(fault.Usage, err, "input %s is not readable", abs)
		}

		_ = fh.Close()

		paths = append(paths, abs)
	}

	return paths, nil
}

func (a *app) assignSchemas(ctx context.Context, src *sources, f runFlags, paths []string) ([]runner.Input, []runner.Skipped, error) {
	inputs := make([]runner.Input, 0, len(paths))

	if f.schema != "" {
		if err := src.known(ctx, f.schema); err != nil {
			return nil, nil, err
		}

		for _, p := range paths {
			inputs = append(inputs, runner.Input{Path: p, SchemaID: f.schema})
		}

		return inputs, nil, nil
	}

	r, err := newResolver(src)
	if err != nil {
		return nil, nil, err
	}

	var skipped []runner.Skipped

	for _, p := range paths {
		_, res, err := r.resolve(ctx, p)
		if err != nil {
			reason := ""

			switch {
			case errors.Is(err, match.ErrNoMatch):
				reason = "unmatched"
			case errors.Is(err, match.ErrOutsideWorkspace):
				reason = "outside-workspace"
			}

			if f.ignoreUnmatched && reason != "" {
				a.logf("skipping %s: %s", p, reason)

				skipped = append(skipped, runner.Skipped{File: p, Reason: reason})

				continue
			}

			return nil, nil, err
		}

		inputs = append(inputs, runner.Input{Path: p, SchemaID: res.SchemaID})
	}

	return inputs, skipped, nil
}
