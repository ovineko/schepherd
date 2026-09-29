package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/tools/release/internal/e2ereport"
)

// reportPerm keeps the scenario report private to the user; it may quote
// local paths.
const reportPerm = 0o600

func newE2EStreamCommand() *cobra.Command {
	var (
		save, report string
		strict       bool
	)

	cmd := &cobra.Command{
		Use:   "e2e-stream",
		Short: "Print the test output of a `go test -json` stream from stdin and fail if any test failed",
		Long: "With --report the scenario report is written once the stream ends, whatever its outcome, so a failed " +
			"or interrupted run still leaves one. --save keeps a copy of the raw stream as it arrives.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runE2EStream(cmd, save, report, strict)
		},
	}

	cmd.Flags().StringVar(&save, "save", "", "write a copy of the raw go test -json stream to this file")
	cmd.Flags().StringVar(&report, "report", "", "write the Markdown scenario report to this file when the stream ends")
	cmd.Flags().BoolVar(&strict, "strict", false, "also fail when a required scenario did not pass (needs --report)")

	return cmd
}

func runE2EStream(cmd *cobra.Command, save, report string, strict bool) error {
	if strict && report == "" {
		return fault.New(fault.Usage, "--strict needs --report")
	}

	var sinks []io.Writer

	if save != "" {
		f, err := os.Create(filepath.Clean(save))
		if err != nil {
			return fmt.Errorf("create --save: %w", err)
		}

		defer func() { _ = f.Close() }()

		sinks = append(sinks, f)
	}

	// The report is parsed while the stream arrives, so a long run is never
	// held in memory.
	var (
		reportIn *io.PipeWriter
		parsed   chan parsedReport
	)

	if report != "" {
		pr, pw := io.Pipe()
		reportIn, parsed = pw, make(chan parsedReport, 1)

		go func() {
			r, err := e2ereport.Parse(pr)
			// A parse error must not block the stream on the pipe.
			_, _ = io.Copy(io.Discard, pr)
			parsed <- parsedReport{report: r, err: err}
		}()

		sinks = append(sinks, pw)
	}

	in := cmd.InOrStdin()
	if len(sinks) > 0 {
		in = io.TeeReader(in, io.MultiWriter(sinks...))
	}

	summary, streamErr := e2ereport.Stream(in, cmd.OutOrStdout())

	var errs []error
	if streamErr != nil {
		errs = append(errs, fmt.Errorf("e2e-stream: %w", streamErr))
	}

	line := fmt.Sprintf("e2e-stream: %d passed, %d failed (%d package failures), %d skipped",
		summary.Passed, summary.Failed, summary.Packages, summary.Skipped)
	if err := writeLine(cmd, line); err != nil {
		errs = append(errs, err)
	}

	if reportIn != nil {
		_ = reportIn.Close()

		p := <-parsed
		if p.err != nil {
			errs = append(errs, fmt.Errorf("parse stdin: %w", p.err))
		} else {
			errs = append(errs, writeE2EReport(cmd, p.report, "stdin", report, strict))
		}
	}

	if !summary.OK() {
		errs = append(errs, errors.New("tests failed"))
	}

	return errors.Join(errs...)
}

type parsedReport struct {
	report *e2ereport.Report
	err    error
}

func newE2EReportCommand() *cobra.Command {
	var (
		in, out string
		strict  bool
	)

	cmd := &cobra.Command{
		Use:   "e2e-report",
		Short: "Render `go test -json` output of the Test<ID>_<Name> scenarios as a Markdown report",
		Long: "Every required scenario (RequiredScenarios in tools/release/internal/e2ereport) is listed; one without any " +
			"test in the input is NOT RUN. " +
			"With --strict the command fails when a required scenario did not pass.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireFlags(cmd, "in", "out"); err != nil {
				return err
			}

			f, err := os.Open(filepath.Clean(in))
			if err != nil {
				return fmt.Errorf("open --in: %w", err)
			}

			defer func() { _ = f.Close() }()

			report, err := e2ereport.Parse(f)
			if err != nil {
				return fmt.Errorf("parse %s: %w", in, err)
			}

			return writeE2EReport(cmd, report, in, out, strict)
		},
	}

	cmd.Flags().StringVar(&in, "in", "", "file with go test -json output")
	cmd.Flags().StringVar(&out, "out", "", "Markdown report path")
	cmd.Flags().BoolVar(&strict, "strict", false, "fail when a required scenario did not pass")

	return cmd
}

// writeE2EReport writes the Markdown report of a go test -json stream to out
// and prints a summary line.
func writeE2EReport(cmd *cobra.Command, report *e2ereport.Report, name, out string, strict bool) error {
	if err := os.WriteFile(out, []byte(report.Markdown()), reportPerm); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	if report.SkippedLines > 0 {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: skipped %d undecodable line(s) of %s; the report may be incomplete\n", report.SkippedLines, name)
	}

	pass, fail, notRun := report.Counts()
	sPass, sFail, sNotRun := report.ScenarioCounts()

	line := fmt.Sprintf("%d tests: %d passed, %d failed, %d not run; %d required scenarios: %d passed, %d failed, %d not run; report: %s",
		len(report.Rows), pass, fail, notRun, len(e2ereport.RequiredScenarios), sPass, sFail, sNotRun, out)
	if err := writeLine(cmd, line); err != nil {
		return err
	}

	if unpassed := report.Unpassed(); strict && len(unpassed) > 0 {
		return fmt.Errorf("%d required scenario(s) did not pass: %s", len(unpassed), strings.Join(unpassed, ", "))
	}

	return nil
}
