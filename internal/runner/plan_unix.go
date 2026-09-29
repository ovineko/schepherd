//go:build unix

package runner

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

var errNotExecutable = errors.New("no execute permission")

func foldEnvKey(key string) string {
	return key
}

func executableExts([]string) []string {
	return nil
}

func findExecutable(path string, _ []string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat executable: %w", err)
	}

	switch {
	case info.IsDir():
		return "", fmt.Errorf("%s is a directory: %w", path, fs.ErrInvalid)
	case !info.Mode().IsRegular():
		return "", fmt.Errorf("%s is not a regular file: %w", path, fs.ErrInvalid)
	}

	if err := checkExecutable(path, info.Mode()); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}

	return path, nil
}

// checkExecutable asks the kernel with the effective IDs, as exec will, so
// the owner/group/other class, ACLs and noexec mounts all count. Like
// os/exec, it falls back to the mode bits only when the check does not
// exist (ENOSYS) or a seccomp filter refuses it (EPERM).
func checkExecutable(path string, mode fs.FileMode) error {
	err := unix.Faccessat(unix.AT_FDCWD, path, unix.X_OK, atEaccess)

	switch {
	case err == nil:
		return nil
	case !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EPERM):
		return fmt.Errorf("%w: %w", errNotExecutable, err)
	case mode.Perm()&0o111 == 0:
		return errNotExecutable
	}

	return nil
}

func isShellScriptHost(string) bool {
	return false
}

// argBytes counts one argv string the way the kernel does for ARG_MAX: the
// bytes plus the terminating NUL.
func argBytes(arg string) int {
	return len(arg) + 1
}

// envBytes counts the environment, which shares the ARG_MAX budget with argv
// on POSIX systems.
func envBytes(env []string) int {
	return argvBytes(env)
}
