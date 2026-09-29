package runner

import (
	"path/filepath"
	"strings"

	"github.com/ovineko/schepherd/internal/fault"
)

// executable resolves the configured command for a consumer environment. The
// result depends only on PATH and PATHEXT of that environment, so it is cached
// per distinct pair.
func (p *planner) executable(env []string) (string, error) {
	pathList, _ := lookupEnv(env, "PATH")
	exts := executableExts(env)
	key := pathList + "\x00" + strings.Join(exts, "\x00")

	if path, ok := p.executables[key]; ok {
		return path, nil
	}

	path, err := resolveExecutable(p.command, p.dir, pathList, exts)
	if err != nil {
		return "", err
	}

	p.executables[key] = path

	return path, nil
}

// resolveExecutable applies the documented rules: an absolute command is used
// as is, a command containing a separator resolves against the consumer's
// working directory, and a bare name is searched in the consumer's PATH
// without ever considering the current directory.
func resolveExecutable(command, dir, pathList string, exts []string) (string, error) {
	switch {
	case filepath.IsAbs(command):
		return checked(command, command, exts)
	case isDriveRelative(command):
		return "", fault.New(fault.Usage, "runner.command %q is relative to a drive; use an absolute path", command)
	case strings.ContainsFunc(command, isSeparator):
		return checked(command, filepath.Join(dir, command), exts)
	}

	for _, entry := range filepath.SplitList(pathList) {
		if entry == "" || !filepath.IsAbs(entry) {
			continue
		}

		if path, err := findExecutable(filepath.Join(entry, command), exts); err == nil {
			return rejectScript(command, path)
		}
	}

	return "", fault.New(fault.ConsumerStart,
		"runner.command %q was not found in the PATH of the consumer environment (relative and empty PATH entries are ignored)", command)
}

func checked(command, path string, exts []string) (string, error) {
	found, err := findExecutable(path, exts)
	if err != nil {
		return "", fault.Reclassify(fault.ConsumerStart, err, "runner.command %q cannot be executed", command)
	}

	return rejectScript(command, found)
}

func rejectScript(command, path string) (string, error) {
	if isShellScriptHost(path) {
		return "", fault.New(fault.ConsumerStart,
			"runner.command %q resolves to %s, which Windows runs through cmd.exe; configure a real executable instead", command, path)
	}

	return path, nil
}

func isSeparator(r rune) bool {
	return r == '/' || r == filepath.Separator
}
