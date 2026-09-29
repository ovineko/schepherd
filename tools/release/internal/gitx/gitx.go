// Package gitx runs the git executable for the release tooling.
//
// Every invocation receives an explicit environment. Variables that redirect
// git to another repository, index or object store are always removed first,
// so a command started from inside a git hook still operates on the
// repository it was pointed at and never on the hook's index.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

var repositoryVariables = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_COMMON_DIR",
	"GIT_NAMESPACE",
	"GIT_PREFIX",
	"GIT_QUARANTINE_PATH",
}

var configVariables = []string{
	"GIT_CONFIG",
	"GIT_CONFIG_GLOBAL",
	"GIT_CONFIG_SYSTEM",
	"GIT_CONFIG_NOSYSTEM",
	"GIT_CONFIG_COUNT",
	"GIT_CONFIG_PARAMETERS",
	"GIT_AUTHOR_NAME",
	"GIT_AUTHOR_EMAIL",
	"GIT_AUTHOR_DATE",
	"GIT_COMMITTER_NAME",
	"GIT_COMMITTER_EMAIL",
	"GIT_COMMITTER_DATE",
}

// CommandError reports a git invocation that exited unsuccessfully.
type CommandError struct {
	Err    error
	Stderr string
	Args   []string
}

func (e *CommandError) Error() string {
	msg := "git " + strings.Join(e.Args, " ")
	if e.Stderr != "" {
		return msg + ": " + e.Stderr
	}

	return msg + ": " + e.Err.Error()
}

func (e *CommandError) Unwrap() error {
	return e.Err
}

// ExitCode returns the exit status of the failed command, or -1 when it did
// not start or was killed.
func (e *CommandError) ExitCode() int {
	if exitErr, ok := errors.AsType[*exec.ExitError](e.Err); ok {
		return exitErr.ExitCode()
	}

	return -1
}

// Runner runs git in Dir with exactly the environment Env.
type Runner struct {
	Dir string
	Env []string
}

// New returns a runner for dir whose environment is base without the
// variables that would point git at a different repository.
func New(dir string, base []string) Runner {
	return Runner{Dir: dir, Env: Without(base, repositoryVariables...)}
}

// Isolated returns a runner for dir that ignores the system and global git
// configuration of the host. The release tooling's tests use it for throwaway
// repositories whose results must depend only on their content: the global
// configuration may enable signing, CRLF conversion or filters that would make
// tree hashes and commits differ between machines. emptyConfig must be a path
// to an empty file that the caller owns.
func Isolated(dir string, base []string, emptyConfig string) Runner {
	env := Without(base, repositoryVariables...)
	env = Without(env, configVariables...)
	env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+emptyConfig)

	return Runner{Dir: dir, Env: env}
}

// NewEmptyConfig creates an empty file suitable for Isolated inside dir.
func NewEmptyConfig(dir string) (string, error) {
	name := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(name, nil, 0o600); err != nil {
		return "", fmt.Errorf("create empty git config: %w", err)
	}

	return name, nil
}

// With returns a copy of r whose environment additionally contains vars
// (NAME=value), replacing earlier values of the same names.
func (r Runner) With(vars ...string) Runner {
	names := make([]string, 0, len(vars))
	for _, v := range vars {
		name, _, _ := strings.Cut(v, "=")
		names = append(names, name)
	}

	env := Without(r.Env, names...)

	return Runner{Dir: r.Dir, Env: append(env, vars...)}
}

// Run runs git with args and returns its standard output.
func (r Runner) Run(ctx context.Context, args ...string) ([]byte, error) {
	return r.RunInput(ctx, nil, args...)
}

// RunInput runs git with args, feeding stdin, and returns its standard output.
func (r Runner) RunInput(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer

	cmd := r.Command(ctx, args...)
	cmd.Stdin = stdin
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &CommandError{Args: args, Stderr: strings.TrimSpace(stderr.String()), Err: err}
	}

	return stdout.Bytes(), nil
}

// Output runs git with args and returns its trimmed standard output.
func (r Runner) Output(ctx context.Context, args ...string) (string, error) {
	out, err := r.Run(ctx, args...)

	return strings.TrimSpace(string(out)), err
}

// Command prepares an unstarted git command with the runner's directory and
// environment.
func (r Runner) Command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: this package exists to run git with caller-supplied arguments
	cmd.Dir = r.Dir
	cmd.Env = slices.Clone(r.Env)

	return cmd
}

// Without returns env without the entries for the given variable names.
func Without(env []string, names ...string) []string {
	out := make([]string, 0, len(env))

	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(names, name) {
			out = append(out, entry)
		}
	}

	return out
}
