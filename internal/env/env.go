// Package env is the single access point for environment variables read by
// Schepherd. Values are parsed and validated here so the rest of the program
// only sees typed values.
package env

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Keys of the documented SCHEPHERD_* overrides.
const (
	KeyConfig     = "SCHEPHERD_CONFIG"
	KeyRepository = "SCHEPHERD_REPOSITORY"
	KeyCatalog    = "SCHEPHERD_CATALOG"
	KeyCacheDir   = "SCHEPHERD_CACHE_DIR"
	KeyOffline    = "SCHEPHERD_OFFLINE"
	KeyWorkspace  = "SCHEPHERD_WORKSPACE"
	KeyTimeout    = "SCHEPHERD_TIMEOUT"
)

// LookupFunc resolves a variable name to its value and presence.
type LookupFunc func(name string) (string, bool)

// Lookup returns the value of an arbitrary variable from the process
// environment. It backs ${NAME} interpolation in configuration files.
func Lookup(name string) (string, bool) {
	return os.LookupEnv(name)
}

// Environ returns a copy of the process environment for child processes.
func Environ() []string {
	return os.Environ()
}

// Config returns the configuration file path from SCHEPHERD_CONFIG.
func Config() (string, bool) {
	return nonEmpty(KeyConfig)
}

// Repository returns the repository override from SCHEPHERD_REPOSITORY.
func Repository() (string, bool) {
	return nonEmpty(KeyRepository)
}

// Catalog returns the catalog digest override from SCHEPHERD_CATALOG.
func Catalog() (string, bool) {
	return nonEmpty(KeyCatalog)
}

// CacheDir returns the cache directory override from SCHEPHERD_CACHE_DIR.
func CacheDir() (string, bool) {
	return nonEmpty(KeyCacheDir)
}

// Workspace returns the workspace override from SCHEPHERD_WORKSPACE.
func Workspace() (string, bool) {
	return nonEmpty(KeyWorkspace)
}

// Offline returns the SCHEPHERD_OFFLINE override. Accepted values are the
// ones strconv.ParseBool understands.
func Offline() (value, set bool, err error) {
	raw, ok := nonEmpty(KeyOffline)
	if !ok {
		return false, false, nil
	}

	value, err = strconv.ParseBool(raw)
	if err != nil {
		return false, true, fmt.Errorf("%s=%q is not a boolean", KeyOffline, raw)
	}

	return value, true, nil
}

// Timeout returns the SCHEPHERD_TIMEOUT override as a positive duration.
func Timeout() (value time.Duration, set bool, err error) {
	raw, ok := nonEmpty(KeyTimeout)
	if !ok {
		return 0, false, nil
	}

	value, err = time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, true, fmt.Errorf("%s=%q is not a positive duration", KeyTimeout, raw)
	}

	return value, true, nil
}

func nonEmpty(key string) (string, bool) {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return "", false
	}

	return value, true
}
