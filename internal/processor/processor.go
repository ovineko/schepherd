// Package processor selects explicitly configured validators before any document is validated.
package processor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/runner"
)

// Definition binds a declared validator to its executable and input policy.
type Definition struct {
	Spec    runner.Spec
	Type    string
	Output  string
	Formats []string
	Convert string
}

// Types is the deterministic preference order, independent of TOML table ordering.
var Types = []string{"sourcemeta", "check-jsonschema", "ajv", "custom"}

// DefaultArgs returns the native invocation contract of a typed processor.
func DefaultArgs(kind string) []string {
	switch kind {
	case "sourcemeta":
		return []string{"validate", "{schema}", "{file}", "--json"}
	case "check-jsonschema", "ajv":
		return []string{"validate", "{schema}", "{file}"}
	default:
		return nil
	}
}

// DefaultFormats lists formats handled without an explicit conversion.
func DefaultFormats(kind string) []string {
	switch kind {
	case "sourcemeta":
		return []string{"json", "yaml"}
	case "check-jsonschema":
		return []string{"json", "yaml", "toml", "json5", "jsonc"}
	case "ajv":
		return []string{"json"}
	default:
		return nil
	}
}

// Format derives an explicit format from the input's extension; unknown names stay unsupported.
func Format(path string) string {
	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".yml", ".yaml":
		return "yaml"
	case ".json", ".toml", ".json5", ".jsonc":
		return ext[1:]
	default:
		return ""
	}
}

// Accepts distinguishes native input from an explicitly requested conversion.
func (d Definition) Accepts(path string) (supported, convert bool) {
	format := Format(path)
	if d.Type == "custom" && len(d.Formats) == 0 {
		return true, false
	}
	if format == "" {
		return false, false
	}
	if d.Convert != "" && slices.Contains(d.Formats, format) {
		return true, true
	}
	if d.Type == "custom" {
		return slices.Contains(d.Formats, format), false
	}
	return slices.Contains(DefaultFormats(d.Type), format), false
}

const (
	probeTimeout   = 30 * time.Second
	maxProbeOutput = 1 << 20
)

type boundedOutput struct {
	data     []byte
	exceeded bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxProbeOutput - len(b.data)
	if len(p) > remaining {
		b.exceeded = true
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	return n, nil
}

// Probe performs a real schema compilation/capability check using the selected executable.
// The returned reason is nonempty only for a declared incompatibility, never for infrastructure failures.
func Probe(ctx context.Context, definition Definition, task runner.Task, schema string) (string, error) {
	if definition.Type == "custom" {
		return "", nil
	}
	args := []string{"probe", schema}
	if definition.Type == "sourcemeta" {
		args = []string{"compile", schema}
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	// bearer:disable go_gosec_injection_subproc_injection
	// The executable and arguments come from the explicitly trusted processor configuration, never schema content.
	cmd := exec.CommandContext(ctx, task.Path, args...) //nolint:gosec // G204: trusted executable binding; arguments never invoke a shell
	cmd.WaitDelay = runner.DefaultKillGrace
	cmd.Dir = task.Dir
	cmd.Env = task.Env
	var stdout, stderr boundedOutput
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if definition.Type == "sourcemeta" {
		cmd.Stdout = io.Discard
	}
	err := cmd.Run()
	if ctx.Err() != nil {
		kind := fault.ConsumerTimeout
		if errors.Is(ctx.Err(), context.Canceled) {
			kind = fault.Canceled
		}
		return "", fault.Wrap(kind, ctx.Err(), "processor %s capability probe", definition.Spec.Name)
	}
	if err != nil {
		return classifyProbeFailure(definition, err, stderr.data)
	}
	if stdout.exceeded || stderr.exceeded {
		return "", fault.New(fault.Integrity, "processor %s capability output exceeds limit", definition.Spec.Name)
	}
	if definition.Type == "sourcemeta" {
		return "", nil
	}
	return decodeCapability(definition.Spec.Name, stdout.data)
}

func decodeCapability(name string, data []byte) (string, error) {
	var result struct {
		Supported *bool  `json:"supported"`
		Reason    string `json:"reason"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(&result); err != nil {
		return "", fault.Wrap(fault.ConsumerStart, err, "processor %s returned invalid capability output", name)
	}
	if result.Supported == nil {
		return "", fault.New(fault.ConsumerStart, "processor %s capability output has no supported field", name)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", fault.New(fault.ConsumerStart, "processor %s returned extra capability output", name)
	}
	if !*result.Supported {
		if result.Reason == "" {
			return "", fault.New(fault.ConsumerStart, "processor %s returned unsupported without a reason", name)
		}
		return result.Reason, nil
	}
	return "", nil
}

func classifyProbeFailure(definition Definition, err error, stderr []byte) (string, error) {
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		message := strings.TrimSpace(string(stderr))
		if definition.Type == "sourcemeta" && exit.ExitCode() == 3 {
			return "schema is not supported by Sourcemeta: " + message, nil
		}
		if exit.ExitCode() == 4 {
			return "schema rejected during capability check (exit 4): " + message, nil
		}
		if definition.Type != "sourcemeta" && exit.ExitCode() == 2 {
			return "", fault.New(fault.Usage, "processor %s configuration rejected: %s", definition.Spec.Name, message)
		}
		return "", fault.New(fault.ConsumerStart, "processor %s capability probe failed (exit %d): %s", definition.Spec.Name, exit.ExitCode(), message)
	}
	return "", fault.Wrap(fault.ConsumerStart, err, "processor %s capability probe", definition.Spec.Name)
}

// Unsupported is a selection failure, distinct from a consumer that failed to start.
func Unsupported(file string, reasons []string) error {
	return fault.New(fault.Usage, "unsupported input %s: %s", file, strings.Join(reasons, "; "))
}

// CheckDefinition rejects silently ignored conversion and format declarations.
func CheckDefinition(d Definition) error {
	if !slices.Contains(Types, d.Type) {
		return fmt.Errorf("unknown processor type %q", d.Type)
	}
	if d.Convert != "" && len(d.Formats) == 0 {
		return errors.New("convert requires a nonempty formats list")
	}
	if d.Convert == "" && len(d.Formats) > 0 && d.Type != "custom" {
		return errors.New("formats requires convert for typed processors")
	}
	for _, format := range d.Formats {
		if !slices.Contains([]string{"json", "yaml", "toml", "jsonc", "json5"}, format) {
			return fmt.Errorf("unsupported format %q", format)
		}
	}
	if err := runner.ValidateSpec(d.Spec); err != nil {
		return fault.Wrap(fault.Usage, err, "processor specification")
	}
	return nil
}
