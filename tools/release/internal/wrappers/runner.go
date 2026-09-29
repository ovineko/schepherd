package wrappers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/ovineko/schepherd/tools/pins"
)

// Runner runs the builders, installers and packaged programs, without a
// shell: on the host, or in a container of Image (pins.RubyImage for the gem
// when the host has no Ruby pins.RubyVersion) without network, with Dir
// mounted at the same path.
type Runner struct {
	Image string
	Dir   string
	Env   []string
}

// Platform is the Go os/arch of the programs r runs.
func (r Runner) Platform() (string, string) {
	if r.Image != "" {
		return "linux", runtime.GOARCH
	}

	return runtime.GOOS, runtime.GOARCH
}

// Run runs a program in Dir and returns its standard output, standard error
// and exit status; only a program that cannot start is an error.
func (r Runner) Run(ctx context.Context, env []string, args ...string) (stdout, stderr []byte, code int, err error) {
	name, argv, environ := args[0], args[1:], slices.Concat(r.Env, env)

	if r.Image != "" {
		prefix := []string{"run", "--rm", "--network", "none", "-v", r.Dir + ":" + r.Dir, "-w", r.Dir, "-e", "HOME=/tmp"}
		if uid := os.Getuid(); uid >= 0 {
			prefix = append(prefix, "--user", strconv.Itoa(uid)+":"+strconv.Itoa(os.Getgid()))
		}

		for _, kv := range env {
			prefix = append(prefix, "-e", kv)
		}

		name, argv, environ = "docker", slices.Concat(prefix, []string{r.Image}, args), r.Env
	}

	var out, errOut bytes.Buffer

	// bearer:disable go_gosec_injection_subproc_injection
	// Runs a builder, installer, docker or a packaged program with arguments of this package, without a shell.
	cmd := exec.CommandContext(ctx, name, argv...) //nolint:gosec // G204: fixed programs of this package
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = r.Dir, environ, &out, &errOut

	err = cmd.Run()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return out.Bytes(), errOut.Bytes(), exit.ExitCode(), nil
	}

	if err != nil {
		return nil, nil, 0, fmt.Errorf("run %s: %w", name, err)
	}

	return out.Bytes(), errOut.Bytes(), 0, nil
}

// Must runs a program and fails unless it exits with status 0.
func (r Runner) Must(ctx context.Context, env []string, args ...string) ([]byte, error) {
	stdout, stderr, code, err := r.Run(ctx, env, args...)
	if err == nil && code != 0 {
		err = fmt.Errorf("%s exited with status %d: %s", strings.Join(args[:min(2, len(args))], " "), code, bytes.TrimSpace(append(stderr, stdout...)))
	}

	return stdout, err
}

// CheckToolchain requires the pinned Ruby and RubyGems.
func (r Runner) CheckToolchain(ctx context.Context) error {
	hint := "install Ruby " + pins.RubyVersion + " (packaging/ruby/.ruby-version) or use --ruby docker"

	got, err := r.Must(ctx, nil, "ruby", "-e", `print RUBY_VERSION, " ", Gem::VERSION`)
	if err != nil {
		return fmt.Errorf("%w; %s", err, hint)
	}

	if want := pins.RubyVersion + " " + pins.RubyGemsVersion; string(got) != want {
		return fmt.Errorf("ruby reports Ruby and RubyGems %q, want %s; %s", got, want, hint)
	}

	return nil
}
