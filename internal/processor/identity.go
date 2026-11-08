package processor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/interp"
	"github.com/ovineko/schepherd/internal/jsonutil"
	"github.com/ovineko/schepherd/internal/runner"
)

// Identity returns stable tool and settings identities for catalog compatibility profiles.
// It runs only the validator's identity operation, never schema compilation or document validation.
func Identity(ctx context.Context, definition Definition, task runner.Task) (toolDigest, settingsDigest string, err error) {
	if definition.Type == "custom" {
		return "", "", fault.New(fault.Usage, "custom processors have no stable compatibility identity")
	}
	file, err := os.Open(task.Path)
	if err != nil {
		return "", "", fault.Wrap(fault.ConsumerStart, err, "open processor executable identity")
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return "", "", fault.Wrap(fault.ConsumerStart, err, "hash processor executable identity")
	}
	identity := digest.FromHash(hash)
	if definition.Type != "sourcemeta" {
		stdout, stderr, runErr := identityCommand(ctx, task)
		if runErr != nil {
			return "", "", runErr
		}
		if err := jsonutil.Check(stdout, jsonutil.DefaultMaxDepth); err != nil {
			return "", "", fault.Wrap(fault.Integrity, err, "processor identity output")
		}
		if len(stderr) != 0 {
			return "", "", fault.New(fault.Integrity, "processor identity wrote unexpected stderr")
		}
		identity += ":" + digest.FromBytes(stdout)
	}
	toolDigest = identity
	settings := semanticEnvironment(definition.Type, task.Env, definition.Spec.Env)
	encoded, err := json.Marshal(struct {
		Processor string   `json:"processor"`
		Tool      string   `json:"tool"`
		Args      []string `json:"args"`
		Output    string   `json:"output"`
		Formats   []string `json:"formats"`
		Settings  []string `json:"settings"`
	}{definition.Type, toolDigest, definition.Spec.Args, definition.Output, definition.Formats, settings})
	if err != nil {
		return "", "", fault.Wrap(fault.Internal, err, "processor settings identity")
	}
	return toolDigest, digest.FromBytes(encoded), nil
}

func semanticEnvironment(kind string, entries []string, explicit map[string]string) []string {
	settings := make([]string, 0, len(explicit))
	for _, entry := range entries {
		name, _, _ := strings.Cut(entry, "=")
		if runtimeLocatorEnvironment(kind, name, explicit) {
			continue
		}
		raw, configured := explicit[name]
		if configured {
			template, err := interp.Parse(raw)
			if err != nil || template.Uses(interp.Schema) || template.Uses(interp.SchemaID) || template.Uses(interp.SchemaRef) || template.Uses(interp.File) {
				continue
			}
		} else if !identityEnvironment(kind, name) {
			continue
		}
		settings = append(settings, entry)
	}
	slices.Sort(settings)
	return settings
}

func identityCommand(ctx context.Context, task runner.Task) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	// bearer:disable go_gosec_injection_subproc_injection
	// The executable is explicitly bound by trusted processor configuration; identity is a fixed argument and no shell is used.
	cmd := exec.CommandContext(ctx, task.Path, "identity") //nolint:gosec // G204: explicitly configured processor executable
	cmd.Dir, cmd.Env = task.Dir, task.Env
	cmd.WaitDelay = runner.DefaultKillGrace
	var stdout, stderr boundedOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, nil, fault.Wrap(fault.ConsumerTimeout, ctx.Err(), "processor identity")
		}
		return nil, nil, fault.Wrap(fault.ConsumerStart, err, "processor identity")
	}
	if stdout.exceeded || stderr.exceeded {
		return nil, nil, fault.New(fault.Integrity, "processor identity output exceeds limit")
	}
	return stdout.data, stderr.data, nil
}

func identityEnvironment(kind, name string) bool {
	if strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") {
		return true
	}
	switch kind {
	case "ajv":
		return name == "NODE_OPTIONS" || name == "NODE_PATH"
	case "check-jsonschema":
		return strings.HasPrefix(name, "PYTHON") && name != "PYTHONUNBUFFERED" && name != "PYTHONDONTWRITEBYTECODE" && name != "PYTHONIOENCODING"
	default:
		return false
	}
}

func runtimeLocatorEnvironment(kind, name string, explicit map[string]string) bool {
	return kind == "check-jsonschema" && name == "CHECK_JSONSCHEMA_BIN" && explicit[name] == "${CHECK_JSONSCHEMA_BIN}"
}
