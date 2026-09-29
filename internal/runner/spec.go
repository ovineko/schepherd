// Package runner executes the configured consumer for a set of input files:
// it groups inputs by schema, expands argument templates without any shell,
// and starts one process per batch or per file.
package runner

import (
	"errors"
	"fmt"
	"maps"
	"runtime"
	"slices"
	"time"

	"github.com/ovineko/schepherd/internal/interp"
)

// Mode selects how inputs are handed to the consumer.
type Mode string

// Supported modes.
const (
	ModeBatch   Mode = "batch"
	ModePerFile Mode = "per-file"
	ModeStdin   Mode = "stdin"
)

// Limits and defaults of the runner configuration.
const (
	DefaultTimeout = 5 * time.Minute
	DefaultJobs    = 1
	MaxJobs        = 64
	DefaultCwd     = "{" + interp.Workspace + "}"
)

// DefaultMaxArgsBytes is the argv budget of one batch process. Windows limits
// a whole command line to 32767 UTF-16 units; POSIX systems allow far more but
// cap a single argument at 128 KiB on Linux, so 256 KiB stays well inside
// every supported limit while keeping batches large.
func DefaultMaxArgsBytes() int {
	if runtime.GOOS == "windows" {
		return 30000
	}

	return 256 << 10
}

// Spec is the runner section of the configuration with templates unexpanded.
type Spec struct {
	Env          map[string]string
	Mode         Mode
	Command      string
	Cwd          string
	CwdBase      string
	Args         []string
	Timeout      time.Duration
	Jobs         int
	MaxArgsBytes int
	FailFast     bool
	InheritEnv   bool
}

// Uses reports whether any template of the spec contains the placeholder
// name; a template that does not parse uses nothing (ValidateSpec rejects it).
func (s Spec) Uses(name string) bool {
	for _, raw := range slices.Concat([]string{s.Command, s.Cwd}, s.Args, slices.Collect(maps.Values(s.Env))) {
		if tpl, err := interp.Parse(raw); err == nil && tpl.Uses(name) {
			return true
		}
	}

	return false
}

var (
	anywhere   = []string{interp.Schema, interp.SchemaID, interp.SchemaRef, interp.File, interp.Workspace, interp.Cache}
	fixedPaths = []string{interp.Workspace, interp.Cache}
)

// ValidateSpec checks everything that can be checked without inputs: mode,
// numeric bounds, template syntax and where each placeholder may appear.
func ValidateSpec(spec Spec) error {
	if !slices.Contains([]Mode{ModeBatch, ModePerFile, ModeStdin}, spec.Mode) {
		return fmt.Errorf("runner.mode %q must be one of batch, per-file, stdin", spec.Mode)
	}

	if spec.Command == "" {
		return errors.New("runner.command is required")
	}

	if spec.Timeout <= 0 {
		return errors.New("runner.timeout must be positive")
	}

	if spec.Jobs < 1 || spec.Jobs > MaxJobs {
		return fmt.Errorf("runner.jobs must be between 1 and %d", MaxJobs)
	}

	if spec.MaxArgsBytes < 0 {
		return errors.New("runner.max_args_bytes must not be negative")
	}

	if err := checkTemplate("runner.command", spec.Command, fixedPaths...); err != nil {
		return err
	}

	if err := checkTemplate("runner.cwd", spec.Cwd, fixedPaths...); err != nil {
		return err
	}

	fileUses, filesLists, err := checkArgs(spec)
	if err != nil {
		return err
	}

	for name, value := range spec.Env {
		tpl, err := parseAllowed("runner.env."+name, value, anywhere...)
		if err != nil {
			return err
		}

		if tpl.Uses(interp.File) {
			fileUses++
		}
	}

	switch spec.Mode {
	case ModeBatch:
		if fileUses > 0 {
			return errors.New("runner: {file} is not available in batch mode; use {files...}")
		}

		if filesLists != 1 {
			return errors.New("runner.args: batch mode needs exactly one argument that is exactly {files...}")
		}
	case ModePerFile:
		if fileUses == 0 {
			return errors.New("runner: per-file mode needs {file} in runner.args or runner.env")
		}
	case ModeStdin:
	}

	return nil
}

func checkArgs(spec Spec) (fileUses, filesLists int, err error) {
	for i, arg := range spec.Args {
		field := fmt.Sprintf("runner.args[%d]", i)

		tpl, err := interp.Parse(arg)
		if err != nil {
			return 0, 0, fmt.Errorf("%s: %w", field, err)
		}

		if tpl.IsFilesList() {
			if spec.Mode != ModeBatch {
				return 0, 0, fmt.Errorf("%s: {files...} is only available in batch mode", field)
			}

			filesLists++

			continue
		}

		if err := tpl.Restrict(field, anywhere...); err != nil {
			if tpl.Uses(interp.Files) {
				return 0, 0, fmt.Errorf("%s: {files...} must be a whole argument", field)
			}

			return 0, 0, fmt.Errorf("invalid runner configuration: %w", err)
		}

		if tpl.Uses(interp.File) {
			fileUses++
		}
	}

	return fileUses, filesLists, nil
}

func checkTemplate(field, raw string, allowed ...string) error {
	_, err := parseAllowed(field, raw, allowed...)

	return err
}

func parseAllowed(field, raw string, allowed ...string) (*interp.Template, error) {
	tpl, err := interp.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}

	if err := tpl.Restrict(field, allowed...); err != nil {
		return nil, fmt.Errorf("invalid runner configuration: %w", err)
	}

	return tpl, nil
}
