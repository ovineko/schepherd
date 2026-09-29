package gitx

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestEnvironmentHandling(t *testing.T) {
	base := []string{"PATH=/bin", "GIT_DIR=/elsewhere", "GIT_INDEX_FILE=/hook/index", "GIT_CONFIG_GLOBAL=/srv/cfg", "GIT_AUTHOR_NAME=someone"}

	plain := New("/repo", base)
	if !slices.Equal(plain.Env, []string{"PATH=/bin", "GIT_CONFIG_GLOBAL=/srv/cfg", "GIT_AUTHOR_NAME=someone"}) {
		t.Errorf("New env = %v", plain.Env)
	}

	isolated := Isolated("/repo", base, "/tmp/empty")
	if !slices.Equal(isolated.Env, []string{"PATH=/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/tmp/empty"}) {
		t.Errorf("Isolated env = %v", isolated.Env)
	}

	with := isolated.With("GIT_CONFIG_GLOBAL=/other", "A=1")
	if !slices.Equal(with.Env, []string{"PATH=/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/other", "A=1"}) {
		t.Errorf("With env = %v", with.Env)
	}

	if len(isolated.Env) != 3 {
		t.Error("With modified the receiver")
	}
}

func TestCommandError(t *testing.T) {
	config, err := NewEmptyConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	git := Isolated(t.TempDir(), nil, config)

	_, err = git.Run(t.Context(), "rev-parse", "--show-toplevel")

	cmdErr, ok := errors.AsType[*CommandError](err)
	if !ok {
		t.Fatalf("error = %v, want *CommandError", err)
	}

	if cmdErr.ExitCode() <= 0 || !strings.Contains(cmdErr.Error(), "git rev-parse --show-toplevel: ") {
		t.Errorf("exit code %d, message %q", cmdErr.ExitCode(), cmdErr.Error())
	}

	out, err := git.Output(t.Context(), "--version")
	if err != nil || !strings.HasPrefix(out, "git version ") {
		t.Errorf("git --version = %q, %v", out, err)
	}
}
