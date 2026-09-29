//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Every read of the harness's own process environment goes through this
// file. The suite does not import the product's internal/env package, so it
// never depends on the code it verifies.
const (
	// envArtifactsDir overrides where logs and reports of a run are kept
	// (default tests/e2e/.artifacts/<project>). A relative value is resolved
	// against the repository root, which is how Taskfile.yaml passes it.
	envArtifactsDir = "E2E_ARTIFACTS_DIR"
	// envPerf turns the TestPerf_* measurements on (strconv.ParseBool
	// syntax, for example E2E_PERF=1; unset or empty means off).
	envPerf = "E2E_PERF"
	// envPrepare makes the test binary pull and build everything the
	// scenarios use and exit without running any test (strconv.ParseBool
	// syntax; task test:e2e:prepare sets E2E_PREPARE=1).
	envPrepare = "E2E_PREPARE"
	// envReaper carries the cleanup job of a reaper process; see reaper_test.go.
	envReaper = "SCHEPHERD_E2E_REAPER"
	// envOwner marks the docker processes a run starts for its compose
	// project (value: the project name). The docker CLI passes it on to the
	// compose plugin it executes, so teardown and the reaper find both; see
	// stopOwnedProcesses.
	envOwner = "SCHEPHERD_E2E_OWNER"
)

// Variables that point compose.yaml at the files the harness generates.
const (
	envComposeCertsDir = "SCHEPHERD_E2E_CERTS_DIR"
	envComposeAuthDir  = "SCHEPHERD_E2E_AUTH_DIR"
	envComposeAuth2Dir = "SCHEPHERD_E2E_AUTH2_DIR"
)

func artifactsDirSetting() string {
	return strings.TrimSpace(os.Getenv(envArtifactsDir))
}

func perfSetting() (bool, error) {
	v := strings.TrimSpace(os.Getenv(envPerf))
	if v == "" {
		return false, nil
	}

	on, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s=%q is not a boolean; use %s=1 to run the TestPerf_* measurements", envPerf, v, envPerf)
	}

	return on, nil
}

func prepareSetting() (bool, error) {
	v := strings.TrimSpace(os.Getenv(envPrepare))
	if v == "" {
		return false, nil
	}

	on, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s=%q is not a boolean; use %s=1 to prepare the images of the suite", envPrepare, v, envPrepare)
	}

	return on, nil
}

func reaperJob() (string, bool) {
	return os.LookupEnv(envReaper)
}

// noGoDownloads keeps the Go commands the harness and the scenarios run on
// the host (builds, go run, GoReleaser) on the module cache the preparation
// step filled (go mod download): a missing module fails instead of being
// downloaded during a scenario.
const noGoDownloads = "GOPROXY=off"

// ownerEntry is the environment entry that marks the docker processes of
// one project.
func ownerEntry(project string) string {
	return envOwner + "=" + project
}

// hostEnviron is the environment of tools the harness drives on the
// developer's behalf (go, docker). Docker needs DOCKER_HOST, DOCKER_CONTEXT
// and the developer's registry credentials to pull base images; COMPOSE_*
// variables are dropped so a developer's COMPOSE_FILE or COMPOSE_PROFILES
// cannot change which services a run starts or removes. Binaries under test
// never receive this environment; see sandbox.environ.
func hostEnviron(extra ...string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+len(extra))

	for _, kv := range base {
		if strings.HasPrefix(kv, "COMPOSE_") || strings.HasPrefix(kv, envReaper+"=") || strings.HasPrefix(kv, envOwner+"=") {
			continue
		}

		out = append(out, kv)
	}

	return append(out, extra...)
}
