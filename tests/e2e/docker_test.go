//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/schepherd/tools/pins"
)

const (
	validatorsRepository = "schepherd-e2e-validators"
	// nodeImage and bunImage install and run the npm packages (E37) from
	// verdaccio on composeNetwork().
	nodeImage = "node:26.10.0-trixie-slim@sha256:ec7758ee051e457b468b32bde57b0879010b325bb9862718e9615225ce4aaae1"
	bunImage  = "oven/bun:1.4.2@sha256:9114c058aeae42162ee16dd5084b95fe9473970bb6bcb5b232ab1630f0546895"
	// pythonImage installs and runs the PyPI wheel (E41); it is also the
	// base of the validators image (TestHarness_WrapperImages).
	pythonImage = "python:3.14-slim@sha256:caaf356f40667c496d405780745b9ac25771c189a51dfcc42430d531ea09f8a2"
	// rubyImage builds the gem with the release tooling (--ruby docker, E41)
	// and installs and runs it.
	rubyImage = pins.RubyImage
)

// compose runs docker compose for this run's project only. Every command
// names the project and the file explicitly and carries the variables the
// file requires, so nothing depends on the caller's directory or on
// COMPOSE_* settings.
type compose struct {
	project string
	file    string
	env     []string
}

func (c *compose) run(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"compose", "--project-name", c.project, "--file", c.file}, args...)

	return runTool(ctx, filepath.Dir(c.file), c.env, "docker", full...)
}

// port returns the 127.0.0.1:<port> address docker published for a
// service's container port.
func (c *compose) port(ctx context.Context, service string, containerPort int) (string, error) {
	out, err := c.run(ctx, "port", service, strconv.Itoa(containerPort))
	if err != nil {
		return "", fmt.Errorf("docker compose port %s %d: %w\n%s", service, containerPort, err, out)
	}

	addr := trimOutput(out)
	if host, _, err := net.SplitHostPort(addr); err != nil || host != "127.0.0.1" {
		return "", fmt.Errorf("docker compose port %s %d returned %q, want a 127.0.0.1 address", service, containerPort, addr)
	}

	return addr, nil
}

// runTool runs a developer tool and returns its combined output.
func runTool(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.WaitDelay = 10 * time.Second

	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}

	return out, nil
}

func docker(ctx context.Context, args ...string) ([]byte, error) {
	return runTool(ctx, "", hostEnviron(), "docker", args...)
}

// removeProject removes the containers, anonymous volumes and network of one
// compose project plus the containers the harness started with docker run,
// then verifies that none of them is left. It is shared by the normal
// teardown and the reaper. A second pass runs when the first leaves
// something behind: the daemon may still complete a request of a docker
// command that was killed just before.
func removeProject(c *compose, artifacts string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*dockerCmdTimeout)
	defer cancel()

	err := removeProjectOnce(ctx, c)
	if err != nil {
		err = removeProjectOnce(ctx, c)
	}

	if err != nil {
		return fmt.Errorf("%w (see %s; remove leftovers with the commands in tests/e2e/README.md)", err, artifacts)
	}

	return nil
}

func removeProjectOnce(ctx context.Context, c *compose) error {
	var errs []error

	runFilter := "label=" + runLabel + "=" + c.project
	composeFilter := "label=com.docker.compose.project=" + c.project

	if ids := dockerIDs(ctx, "ps", "--all", "--quiet", "--filter", runFilter); len(ids) > 0 {
		if out, err := docker(ctx, append([]string{"rm", "--force", "--volumes"}, ids...)...); err != nil {
			errs = append(errs, fmt.Errorf("remove run containers: %w\n%s", err, out))
		}
	}

	volumes := projectVolumes(ctx, c.project)

	out, err := c.run(ctx, "--profile", profileAuth, "--profile", profileNpm, "down", "--volumes", "--remove-orphans", "--timeout", "10")
	if err != nil {
		errs = append(errs, fmt.Errorf("docker compose down: %w\n%s", err, out))
	}

	for _, filter := range []string{runFilter, composeFilter} {
		if left := dockerIDs(ctx, "ps", "--all", "--quiet", "--filter", filter); len(left) > 0 {
			errs = append(errs, fmt.Errorf("containers with %s are still present: %s", filter, strings.Join(left, " ")))
		}
	}

	for _, v := range volumes {
		if _, err := docker(ctx, "volume", "inspect", v); err == nil {
			errs = append(errs, fmt.Errorf("volume %s of project %s is still present", v, c.project))
		}
	}

	if _, err := docker(ctx, "network", "inspect", c.project+"_default"); err == nil {
		errs = append(errs, fmt.Errorf("network %s_default is still present", c.project))
	}

	return errors.Join(errs...)
}

func dockerIDs(ctx context.Context, args ...string) []string {
	out, err := docker(ctx, args...)
	if err != nil {
		return nil
	}

	return strings.Fields(string(out))
}

// projectVolumes lists the volumes mounted by the project's containers:
// the anonymous volumes behind the images' VOLUME declarations, which
// "down --volumes" must remove.
func projectVolumes(ctx context.Context, project string) []string {
	ids := dockerIDs(ctx, "ps", "--all", "--quiet", "--filter", "label=com.docker.compose.project="+project)
	if len(ids) == 0 {
		return nil
	}

	args := append([]string{"inspect", "--format", `{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}} {{end}}{{end}}`}, ids...)

	return dockerIDs(ctx, args...)
}

// composeNetwork is the network of the compose project; a container started
// with dockerRun and this network reaches services by their service names
// (for example http://verdaccio:4873).
func composeNetwork() string {
	return suite.project + "_default"
}

// stopService stops a compose service. The service is started again when
// the test ends if the test did not do so itself. Tests that stop shared
// services must not run in parallel with other tests. Until startService,
// reg.Host() and npmRegistry(t) keep returning the address of the stopped
// container.
func stopService(t *testing.T, service string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), dockerCmdTimeout)
	defer cancel()

	if out, err := suite.compose.run(ctx, "stop", service); err != nil {
		t.Fatalf("stop %s: %v\n%s", service, err, out)
	}

	t.Cleanup(func() {
		if !serviceRunning(service) {
			startService(t, service)
		}
	})
}

// startService starts a stopped compose service, waits until it runs and
// answers the harness's readiness probe, and rediscovers its host port,
// which changes on every start: a registry's handle (reg.Host()) and, for
// verdaccio, npmRegistry(t) return the new address afterwards, while the
// suite proxies keep their addresses.
func startService(t *testing.T, service string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), composeUpTimeout)
	defer cancel()

	if out, err := suite.compose.run(ctx, "start", "--wait", service); err != nil {
		t.Fatalf("start %s: %v\n%s", service, err, out)
	}

	if r, ok := suite.services[service]; ok {
		if err := suite.discover(ctx, r); err != nil {
			t.Fatalf("start %s: %v", service, err)
		}
	}

	if service == serviceNpm {
		base, err := discoverNpm(ctx)

		suite.npmMu.Lock()
		suite.npmURL, suite.npmErr = base, err
		suite.npmMu.Unlock()

		if err != nil {
			t.Fatalf("start %s: %v", service, err)
		}
	}
}

func serviceRunning(service string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), dockerCmdTimeout)
	defer cancel()

	out, err := suite.compose.run(ctx, "ps", "--status", "running", "--quiet", service)

	return err == nil && trimOutput(out) != ""
}

// serviceLogs returns the logs of one compose service.
func serviceLogs(t *testing.T, service string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), dockerCmdTimeout)
	defer cancel()

	out, err := suite.compose.run(ctx, "--profile", profileAuth, "--profile", profileNpm, "logs", "--no-color", service)
	if err != nil {
		t.Fatalf("logs of %s: %v\n%s", service, err, out)
	}

	return string(out)
}

// npmRegistry starts the verdaccio service (profile npm) on first use and
// returns its current base URL on the host, http://127.0.0.1:<port>; the
// port changes when the service restarts (see startService). Containers on
// composeNetwork() reach it as http://verdaccio:4873.
func npmRegistry(t *testing.T) string {
	t.Helper()

	suite.npmMu.Lock()
	defer suite.npmMu.Unlock()

	if suite.npmURL == "" && suite.npmErr == nil {
		suite.npmURL, suite.npmErr = startNpmRegistry()
	}

	if suite.npmErr != nil {
		t.Fatalf("npm registry: %v", suite.npmErr)
	}

	return suite.npmURL
}

func startNpmRegistry() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), composeUpTimeout)
	defer cancel()

	out, err := suite.compose.run(ctx, "--profile", profileAuth, "--profile", profileNpm, "up", "--detach", "--wait", "--pull", "never", serviceNpm)
	if err != nil {
		return "", fmt.Errorf("docker compose up %s: %w\n%s", serviceNpm, err, out)
	}

	return discoverNpm(ctx)
}

// discoverNpm reads verdaccio's current host port and waits until it
// answers /-/ping.
func discoverNpm(ctx context.Context) (string, error) {
	addr, err := suite.compose.port(ctx, serviceNpm, 4873)
	if err != nil {
		return "", err
	}

	base := "http://" + addr
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}

	err = poll(ctx, readinessDeadline, func() error {
		return expectStatus(ctx, client, base+"/-/ping", http.StatusOK)
	})
	if err != nil {
		return "", fmt.Errorf("%s is not ready: %w", serviceNpm, err)
	}

	return base, nil
}

// mount is a bind mount of dockerRun. Mounts are read-only unless Writable.
type mount struct {
	Host      string
	Container string
	Writable  bool
}

// dockerOpts configures dockerRun.
type dockerOpts struct {
	Mounts []mount
	// Env entries are passed with -e KEY=VALUE.
	Env []string
	// Workdir is the working directory inside the container.
	Workdir string
	// Network defaults to "none"; use composeNetwork() to reach services.
	Network string
	// User defaults to the uid:gid of the test process, so files written to
	// writable mounts stay removable by t.TempDir cleanup.
	User string
	// Writable drops --read-only (the root file system is read-only by
	// default; /tmp is always a tmpfs).
	Writable bool
	// Stdin is passed to the container (docker run --interactive).
	Stdin   []byte
	Timeout time.Duration
}

// dockerRun runs a container of this run (removed afterwards, labeled so
// teardown can find it if the test dies) and returns the result of its
// command. HOME is /tmp inside the container.
func dockerRun(t *testing.T, image string, opts dockerOpts, args ...string) result {
	t.Helper()

	suite.runSeq.Lock()
	suite.runCount++
	name := fmt.Sprintf("%s-run-%d", suite.project, suite.runCount)
	suite.runSeq.Unlock()

	network := opts.Network
	if network == "" {
		network = "none"
	}

	user := opts.User
	if user == "" {
		user = strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	}

	full := []string{
		"run", "--rm", "--pull", "never", "--name", name, "--label", runLabel + "=" + suite.project,
		"--network", network, "--user", user, "--tmpfs", "/tmp:rw,exec,mode=1777", "--env", "HOME=/tmp",
	}

	if !opts.Writable {
		full = append(full, "--read-only")
	}

	if opts.Stdin != nil {
		full = append(full, "--interactive")
	}

	if opts.Workdir != "" {
		full = append(full, "--workdir", opts.Workdir)
	}

	for _, kv := range opts.Env {
		full = append(full, "--env", kv)
	}

	for _, m := range opts.Mounts {
		spec := "type=bind,source=" + m.Host + ",target=" + m.Container
		if !m.Writable {
			spec += ",readonly"
		}

		full = append(full, "--mount", spec)
	}

	full = append(full, image)
	full = append(full, args...)

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), dockerCmdTimeout)
		defer cancel()

		_, _ = docker(ctx, "rm", "--force", "--volumes", name)
	})

	return runProcess(t, "docker", runOpts{Env: hostEnviron(ownerEntry(suite.project)), Stdin: opts.Stdin, Timeout: opts.Timeout, rawEnv: true}, full...)
}

// dockerRunValidators runs a command in the validators image (check-jsonschema
// and the pinned Sourcemeta jsonschema CLI in /usr/local/bin) without any
// network. Mount binMount() to use the static schepherd binaries inside.
func dockerRunValidators(t *testing.T, mounts []mount, args ...string) result {
	t.Helper()

	return dockerRun(t, validatorsImage(t), dockerOpts{Mounts: mounts, Network: "none"}, args...)
}

// binMount mounts the binaries of this run (schepherd, schepherd-publisher,
// testconsumer, docker-credential-e2e) read-only at /e2e/bin.
func binMount() mount {
	return mount{Host: suite.bin, Container: "/e2e/bin"}
}

// validatorsImage returns the tag of the validators image, which the
// preparation step built and setUp found in the local image store.
func validatorsImage(t *testing.T) string {
	t.Helper()

	return suite.validatorsImage
}

// writeArtifact stores a file in the run's artifacts directory and returns
// its path.
func writeArtifact(name string, data []byte) (string, error) {
	if err := os.MkdirAll(suite.artifacts, 0o755); err != nil {
		return "", fmt.Errorf("create artifacts directory: %w", err)
	}

	path := filepath.Join(suite.artifacts, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create artifacts directory: %w", err)
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write artifact: %w", err)
	}

	return path, nil
}

// poll calls check every readinessInterval until it succeeds or timeout
// elapses and returns the last error.
func poll(ctx context.Context, timeout time.Duration, check func() error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(readinessInterval)
	defer ticker.Stop()

	for {
		err := check()
		if err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("gave up after %v: %w", timeout, err)
		case <-ticker.C:
		}
	}
}

func expectStatus(ctx context.Context, client *http.Client, url string, want int) error {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("request %s: %w", url, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != want {
		return fmt.Errorf("GET %s: status %d, want %d", url, resp.StatusCode, want)
	}

	return nil
}
