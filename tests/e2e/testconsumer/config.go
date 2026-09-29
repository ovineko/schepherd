package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

const (
	keyLogDir             = "TC_LOG_DIR"
	keyExit               = "TC_EXIT"
	keyExitIfArgContains  = "TC_EXIT_IF_ARG_CONTAINS"
	keyReadStdin          = "TC_READ_STDIN"
	keyHang               = "TC_HANG"
	keySpawnGrandchild    = "TC_SPAWN_GRANDCHILD"
	keyRole               = "TC_ROLE"
	keyStdout             = "TC_STDOUT"
	keyStderr             = "TC_STDERR"
	grandchildRole        = "grandchild"
	recordedPrefixConfig  = "TC_"
	recordedPrefixProduct = "SCHEMA_"
)

var recordedNames = []string{"PATH", "NO_COLOR"}

type config struct {
	logDir          string
	exitCode        int
	failArgSubstr   string
	readStdin       bool
	hang            bool
	spawnGrandchild bool
	role            string
	stdout          string
	stderr          string
}

// environ is the package's only access to the process environment.
func environ() []string {
	return os.Environ()
}

func parseConfig(vars []string) (config, error) {
	values := make(map[string]string, len(vars))
	for _, kv := range vars {
		if name, value, ok := strings.Cut(kv, "="); ok {
			values[name] = value
		}
	}

	cfg := config{
		logDir:        values[keyLogDir],
		failArgSubstr: values[keyExitIfArgContains],
		role:          values[keyRole],
		stdout:        values[keyStdout],
		stderr:        values[keyStderr],
	}

	if cfg.logDir == "" {
		return config{}, errors.New(keyLogDir + " is required")
	}

	if raw := values[keyExit]; raw != "" {
		code, err := strconv.Atoi(raw)
		if err != nil || code < 0 || code > 255 {
			return config{}, fmt.Errorf("%s=%q is not an exit status between 0 and 255", keyExit, raw)
		}

		cfg.exitCode = code
	}

	flags := []struct {
		key string
		dst *bool
	}{
		{keyReadStdin, &cfg.readStdin},
		{keyHang, &cfg.hang},
		{keySpawnGrandchild, &cfg.spawnGrandchild},
	}
	for _, flag := range flags {
		raw := values[flag.key]
		if raw == "" {
			continue
		}

		value, err := strconv.ParseBool(raw)
		if err != nil {
			return config{}, fmt.Errorf("%s=%q is not a boolean", flag.key, raw)
		}

		*flag.dst = value
	}

	return cfg, nil
}

func (c config) exitStatus(args []string) int {
	if c.failArgSubstr != "" {
		for _, arg := range args {
			if strings.Contains(arg, c.failArgSubstr) {
				return 1
			}
		}
	}

	return c.exitCode
}

func recordedEnv(vars []string) map[string]string {
	out := map[string]string{}
	for _, kv := range vars {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}

		if strings.HasPrefix(name, recordedPrefixConfig) || strings.HasPrefix(name, recordedPrefixProduct) ||
			slices.Contains(recordedNames, name) {
			out[name] = value
		}
	}

	return out
}

// grandchildEnv keeps TC_LOG_DIR so the grandchild logs its own invocation,
// and drops every behavior switch so it neither recurses nor duplicates the
// parent's output.
func grandchildEnv(vars []string) []string {
	dropped := []string{
		keySpawnGrandchild, keyHang, keyRole, keyStdout, keyStderr,
		keyReadStdin, keyExit, keyExitIfArgContains,
	}

	out := make([]string, 0, len(vars)+2)
	for _, kv := range vars {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(dropped, name) {
			out = append(out, kv)
		}
	}

	return append(out, keyHang+"=1", keyRole+"="+grandchildRole)
}
