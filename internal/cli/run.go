package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/match"
	"github.com/ovineko/schepherd/internal/processor"
	"github.com/ovineko/schepherd/internal/runner"
)

type runFlags struct {
	schema          string
	report          string
	ignoreUnmatched bool
	json            bool
}

func (a *app) newRunCmd() *cobra.Command {
	var f runFlags

	cmd := &cobra.Command{
		Use:   "run [--schema <id>] [--ignore-unmatched] [--report <file>] [--json] -- <files...>",
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
	cmd.Flags().BoolVar(&f.json, "json", false, "emit one attributed JSON report instead of consumer output")

	return cmd
}

func (a *app) run(cmd *cobra.Command, f runFlags, files []string) error {
	if f.json {
		a.machine = true
	}
	if len(files) == 0 {
		return fault.New(fault.Usage, "no input files; pass them after --")
	}

	cfg, err := a.config()
	if err != nil {
		return err
	}
	if a.machine {
		a.runReport = newRunReport(cfg.Workspace, cfg.Runner.Mode)
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
	if a.runReport != nil {
		a.runReport.Skipped = append(a.runReport.Skipped, skipped...)
	}
	if err != nil {
		return err
	}

	schemas, err := prepareRunSchemas(ctx, src, inputs)
	if err != nil {
		return err
	}

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
		Capture:   a.machine,
	}

	compiler, cleanup, err := src.compiler()
	if err != nil {
		return err
	}
	defer cleanup()
	report, processorSkipped, runErr := a.executeRunPlan(ctx, cmd.Context(), inputs, schemas, opts, compiler)
	cancel()
	skipped = append(skipped, processorSkipped...)
	report.Skipped = append([]runner.Skipped{}, skipped...)
	if a.machine {
		a.runReport = report
	}

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

func prepareRunSchemas(ctx context.Context, src *sources, inputs []runner.Input) (map[string]runner.SchemaInfo, error) {
	schemas := make(map[string]runner.SchemaInfo)
	for _, in := range inputs {
		if _, done := schemas[in.SchemaID]; done {
			continue
		}
		sch, err := src.prepare(ctx, in.SchemaID)
		if err != nil {
			located := &runFilesError{err: err, files: []string{}}
			for _, input := range inputs {
				if input.SchemaID == in.SchemaID {
					located.files = append(located.files, input.Path)
				}
			}
			if local, ok := src.cfg.LocalSchema(in.SchemaID); ok {
				located.file = local.Path
			}
			return nil, located
		}
		schemas[in.SchemaID] = sch.info
	}
	return schemas, nil
}

func (a *app) executeRunPlan(planningCtx, executionCtx context.Context, inputs []runner.Input, schemas map[string]runner.SchemaInfo, opts runner.Options, compiler *processor.Compiler) (*runner.Report, []runner.Skipped, error) {
	report := &runner.Report{ReportVersion: 1, Mode: opts.Spec.Mode, Tasks: []runner.TaskReport{}, Skipped: []runner.Skipped{}}
	if a.machine {
		report = a.runReport
	}
	if len(inputs) == 0 {
		return report, nil, nil
	}
	plan, skipped, cleanup, err := buildCompiledProcessorPlan(planningCtx, a.cfg, inputs, schemas, opts, compiler)
	if cleanup != nil {
		defer cleanup()
	}
	for _, skip := range skipped {
		a.logf("skipping %s: %s", skip.File, skip.Reason)
	}
	if err != nil {
		runErr := fault.Wrap(fault.Usage, err, "runner")
		report.ExitCode = fault.ExitCodeOf(runErr)
		if !a.machine {
			if failed := runner.PlanFailureReport(inputs, schemas, opts.Spec.Mode, runErr); failed != nil {
				report = failed
			}
		}
		return report, skipped, runErr
	}
	report, err = runner.Execute(executionCtx, plan, opts)
	if err != nil {
		return report, skipped, fault.Wrap(fault.Internal, err, "execute runner plan")
	}
	return report, skipped, nil
}

func newRunReport(workspace string, mode runner.Mode) *runner.Report {
	if mode == "" {
		mode = runner.ModeBatch
	}
	if mode == runner.ModeBatch {
		mode = runner.ModePerFile
	}
	return &runner.Report{ReportVersion: runner.CaptureReportVersion, Workspace: workspace, Mode: mode, Tasks: []runner.TaskReport{}, Skipped: []runner.Skipped{}}
}

func (a *app) finishRunJSON(err error) int {
	if a.runReport == nil {
		a.runReport = newRunReport("", runner.ModeBatch)
	}
	report := a.runReport
	report.ExitCode = fault.ExitCodeOf(err)
	if err != nil {
		taskFailure := false
		for _, task := range report.Tasks {
			if task.Status != runner.StatusOK {
				taskFailure = true
				break
			}
		}
		if !taskFailure {
			report.Error = &runner.ReportError{Kind: fault.KindOf(err).String(), Message: err.Error()}
			if located, ok := errors.AsType[*runFilesError](err); ok {
				switch {
				case located.file != "":
					report.Error.File = located.file
					report.Error.Files = located.files
				case len(located.files) == 1:
					report.Error.File = located.files[0]
				default:
					report.Error.Files = located.files
				}
			}
		}
	}
	if writeErr := runner.WriteJSON(a.stdout, report); writeErr != nil {
		return fault.ExitCodeOf(errors.Join(err, writeErr))
	}
	return report.ExitCode
}

type runFilesError struct {
	err   error
	file  string
	files []string
}

func (e *runFilesError) Error() string { return e.err.Error() }
func (e *runFilesError) Unwrap() error { return e.err }

func (a *app) wantsRunJSON(args []string) bool {
	var run *cobra.Command
	for _, command := range a.root.Commands() {
		if command.Name() == "run" {
			run = command
			break
		}
	}
	if run == nil {
		return false
	}
	selected, enabled := false, false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if strings.HasPrefix(arg, "--") {
			name, value, hasValue := strings.Cut(arg[2:], "=")
			flag := a.root.PersistentFlags().Lookup(name)
			if selected {
				if local := run.Flags().Lookup(name); local != nil {
					flag = local
				}
			}
			if flag == nil {
				continue
			}
			if flag.NoOptDefVal == "" && !hasValue {
				i++
				continue
			}
			if selected && name == "json" {
				if !hasValue {
					value = flag.NoOptDefVal
				}
				if parsed, parseErr := strconv.ParseBool(value); parseErr == nil {
					enabled = parsed
				}
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if !selected {
			if arg != "run" {
				return false
			}
			selected = true
		}
	}
	return selected && enabled
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
			return nil, &runFilesError{err: fault.Wrap(fault.Usage, err, "input %s", abs), files: []string{abs}}
		}

		if !info.Mode().IsRegular() {
			return nil, &runFilesError{err: fault.New(fault.Usage, "input %s is not a regular file", abs), files: []string{abs}}
		}

		// bearer:disable go_gosec_filesystem_filereadtaint
		// Opening the inputs the user named is the purpose; only readability is checked.
		fh, err := os.Open(abs) //nolint:gosec // opening the inputs the user named is the purpose; the file is only checked for readability
		if err != nil {
			return nil, &runFilesError{err: fault.Wrap(fault.Usage, err, "input %s is not readable", abs), files: []string{abs}}
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
			return nil, nil, &runFilesError{err: err, files: paths}
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

			return nil, skipped, &runFilesError{err: err, files: []string{p}}
		}

		inputs = append(inputs, runner.Input{Path: p, SchemaID: res.SchemaID})
	}

	return inputs, skipped, nil
}
