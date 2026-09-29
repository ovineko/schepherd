package runner

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/ovineko/schepherd/internal/fault"
)

// ReportVersion is the format version written into every report.
const ReportVersion = 1

// Task statuses of a report.
const (
	StatusOK         = "ok"
	StatusFailed     = "failed"
	StatusTimeout    = "timeout"
	StatusStartError = "start-error"
	StatusCanceled   = "canceled"
	StatusNotStarted = "not-started"
)

// TaskReport is the outcome of one task. Origin is SchemaInfo.Origin of its
// schema. ExitCode is the status the task contributes to the run: the
// consumer's exit code, 128+signal, 124 for a timeout, 7 when it could not
// start, 130 when it was interrupted and -1 when it never started.
type TaskReport struct {
	SchemaID   string   `json:"schemaId"`
	SchemaRef  string   `json:"schemaRef"`
	Origin     string   `json:"origin"`
	Files      []string `json:"files"`
	Status     string   `json:"status"`
	ExitCode   int      `json:"exitCode"`
	DurationMs int64    `json:"durationMs"`
}

// Skipped is an input that was not handed to any consumer.
type Skipped struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

// Report is the machine-readable result of a run. It never contains
// environment values.
type Report struct {
	ReportVersion int          `json:"reportVersion"`
	Mode          Mode         `json:"mode"`
	ExitCode      int          `json:"exitCode"`
	Tasks         []TaskReport `json:"tasks"`
	Skipped       []Skipped    `json:"skipped"`
}

// WriteReport writes r as indented JSON to path atomically: the data goes to
// a temporary file in the same directory that then replaces path, so readers
// never see a partial report.
func WriteReport(path string, r *Report) error {
	if r == nil {
		return fault.New(fault.Internal, "runner: no report to write")
	}

	data, err := marshalReport(r)
	if err != nil {
		return err
	}

	f, err := os.CreateTemp(filepath.Dir(path), ".schepherd-report-*")
	if err != nil {
		return fault.Wrap(fault.Internal, err, "write report %s", path)
	}

	tmp := f.Name()
	committed := false

	defer func() {
		if !committed {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()

	if _, err := f.Write(data); err != nil {
		return fault.Wrap(fault.Internal, err, "write report %s", path)
	}

	if err := f.Chmod(0o644); err != nil {
		return fault.Wrap(fault.Internal, err, "write report %s", path)
	}

	if err := f.Sync(); err != nil {
		return fault.Wrap(fault.Internal, err, "write report %s", path)
	}

	if err := f.Close(); err != nil {
		return fault.Wrap(fault.Internal, err, "write report %s", path)
	}

	if err := os.Rename(tmp, path); err != nil {
		return fault.Wrap(fault.Internal, err, "write report %s", path)
	}

	committed = true

	return nil
}

func marshalReport(r *Report) ([]byte, error) {
	out := *r
	out.Tasks = make([]TaskReport, 0, len(r.Tasks))
	out.Skipped = append(make([]Skipped, 0, len(r.Skipped)), r.Skipped...)

	for _, t := range r.Tasks {
		if t.Files == nil {
			t.Files = []string{}
		}

		out.Tasks = append(out.Tasks, t)
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "encode report")
	}

	return append(data, '\n'), nil
}

// PlanFailureReport describes a run whose planning failed before any consumer
// started. Every task that would have run is listed: with status start-error
// when the failure means the consumer cannot be started (missing or
// non-executable command, unusable working directory), otherwise
// not-started. It returns nil when the inputs cannot even be grouped.
func PlanFailureReport(inputs []Input, schemas map[string]SchemaInfo, mode Mode, err error) *Report {
	groups, gerr := groupInputs(inputs, schemas, mode)
	if gerr != nil {
		return nil
	}

	status := StatusNotStarted
	if fault.KindOf(err) == fault.ConsumerStart {
		status = StatusStartError
	}

	code := fault.ExitCodeOf(err)
	report := &Report{ReportVersion: 1, Mode: mode, ExitCode: code, Tasks: make([]TaskReport, 0, len(groups)), Skipped: []Skipped{}}

	for _, g := range groups {
		task := TaskReport{SchemaID: g.schema.ID, SchemaRef: g.schema.Ref, Origin: g.schema.Origin, Files: g.files, Status: status}
		if status == StatusStartError {
			task.ExitCode = code
		}

		report.Tasks = append(report.Tasks, task)
	}

	return report
}
