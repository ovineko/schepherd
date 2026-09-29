package runner

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var defaultExts = []string{".com", ".exe", ".bat", ".cmd"}

func foldEnvKey(key string) string {
	return strings.ToUpper(key)
}

// executableExts reads PATHEXT from the consumer environment, falling back to
// the Windows default when it is unset or empty.
func executableExts(env []string) []string {
	value, _ := lookupEnv(env, "PATHEXT")

	var exts []string

	for e := range strings.SplitSeq(strings.ToLower(value), ";") {
		if e == "" {
			continue
		}

		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}

		exts = append(exts, e)
	}

	if len(exts) == 0 {
		return defaultExts
	}

	return exts
}

func findExecutable(path string, exts []string) (string, error) {
	if filepath.Ext(path) != "" {
		if err := statFile(path); err == nil {
			return path, nil
		}
	}

	for _, e := range exts {
		if candidate := path + e; statFile(candidate) == nil {
			return candidate, nil
		}
	}

	if err := statFile(path); err != nil {
		return "", err
	}

	return "", fmt.Errorf("%s has no executable extension from PATHEXT: %w", path, fs.ErrNotExist)
}

func statFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat executable: %w", err)
	}

	if info.IsDir() {
		return fmt.Errorf("%s is a directory: %w", path, fs.ErrInvalid)
	}

	return nil
}

// isShellScriptHost reports batch files: CreateProcess runs them through
// cmd.exe, which re-parses the whole command line, so file names would be
// interpreted by a shell.
func isShellScriptHost(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))

	return ext == ".bat" || ext == ".cmd"
}

// argBytes counts one argument as it appears on the Windows command line:
// quoted and escaped, followed by a separating space. UTF-8 length is an
// upper bound of the UTF-16 length that the 32767-unit limit applies to.
func argBytes(arg string) int {
	return len(syscall.EscapeArg(arg)) + 1
}

// envBytes is zero on Windows: the environment block is not part of the
// command line and has no shared length limit with it.
func envBytes([]string) int {
	return 0
}
