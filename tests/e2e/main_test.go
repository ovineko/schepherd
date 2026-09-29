//go:build e2e

// Package e2e is Schepherd's black-box end-to-end suite. It starts real OCI
// registries with Docker Compose, builds the real binaries and runs them as
// subprocesses with isolated environments. TestMain owns the infrastructure;
// harness_test.go and its siblings are the helper API for scenario tests
// (documented in README.md).
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	moduleName = "github.com/ovineko/schepherd"

	serviceSource = "registry-source"
	serviceMirror = "registry-mirror"
	serviceAuth   = "registry-auth"
	serviceAuth2  = "registry-auth2"
	serviceNpm    = "verdaccio"

	profileAuth = "auth"
	profileNpm  = "npm"

	// runLabel marks containers the harness starts with docker run, so
	// teardown and the reaper can remove exactly those.
	runLabel = "com.ovineko.schepherd.e2e.project"

	readinessDeadline = 90 * time.Second
	readinessInterval = 200 * time.Millisecond
	composeUpTimeout  = 5 * time.Minute
	dockerCmdTimeout  = 2 * time.Minute
	buildTimeout      = 5 * time.Minute
)

// suite is set by TestMain before any test runs and never changes afterwards.
var suite *harness

type harness struct {
	root       string
	dir        string
	project    string
	work       string
	bin        string
	jsonschema string
	artifacts  string
	perf       bool
	compose    *compose
	pki        *pki
	reaper     *reaper

	source, mirror, auth, auth2 *registry
	sourceProxy, mirrorProxy    *proxy

	services map[string]*registry

	validatorsImage string

	npmMu  sync.Mutex
	npmURL string
	npmErr error

	failed     bool
	failedMu   sync.Mutex
	teardownMu sync.Once
	runSeq     sync.Mutex
	runCount   int
}

func TestMain(m *testing.M) {
	if job, ok := reaperJob(); ok {
		os.Exit(runReaper(job))
	}

	prep, err := prepareSetting()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	if prep {
		os.Exit(runPrepare())
	}

	os.Exit(runSuite(m))
}

func runSuite(m *testing.M) int {
	h, err := setUp()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: setup failed: %v\n", err)

		if h != nil {
			h.markFailed()

			if err := h.tearDown(); err != nil {
				fmt.Fprintf(os.Stderr, "e2e: teardown: %v\n", err)
			}

			h.releaseReaper()
		}

		return 1
	}

	suite = h
	stopSignals := h.trapSignals()

	if perfEnabled() {
		fmt.Fprintf(os.Stderr, "e2e: %s is on, the TestPerf_* measurements run\n", envPerf)
	}

	code := m.Run()

	stopSignals()

	if code != 0 {
		h.markFailed()
	}

	err = h.tearDown()

	h.releaseReaper()

	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: teardown: %v\n", err)

		return 1
	}

	return code
}

// suiteBase is what the suite and the preparation step both need: the
// repository, a working Docker and the pinned Sourcemeta binary.
type suiteBase struct {
	root       string
	dir        string
	jsonschema string
}

func newBase(ctx context.Context) (suiteBase, error) {
	if runtime.GOOS != "linux" {
		return suiteBase{}, fmt.Errorf("the end-to-end suite runs on Linux only (this is %s): it mounts the binaries it builds for the host "+
			"into Linux containers, copies the host's Sourcemeta binary into a Linux image and reads /proc; "+
			"run it on a Linux machine, in a Linux VM or in CI", runtime.GOOS)
	}

	if err := checkDocker(ctx); err != nil {
		return suiteBase{}, err
	}

	root, err := repoRoot()
	if err != nil {
		return suiteBase{}, err
	}

	b := suiteBase{root: root, dir: filepath.Join(root, "tests", "e2e"), jsonschema: filepath.Join(root, ".tools", "bin", "jsonschema")}

	if info, err := os.Stat(b.jsonschema); err != nil || info.Mode().Perm()&0o111 == 0 {
		return suiteBase{}, fmt.Errorf("the pinned Sourcemeta jsonschema binary is missing at %s; install it with 'pnpm dm exec task -- tools'", b.jsonschema)
	}

	return b, nil
}

func setUp() (*harness, error) {
	perf, err := perfSetting()
	if err != nil {
		return nil, err
	}

	b, err := newBase(context.Background())
	if err != nil {
		return nil, err
	}

	h := &harness{
		root:       b.root,
		dir:        b.dir,
		jsonschema: b.jsonschema,
		perf:       perf,
		services:   map[string]*registry{},
	}

	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	h.project = fmt.Sprintf("schepherd-e2e-%d-%s", os.Getpid(), hex.EncodeToString(suffix))
	h.artifacts = resolveArtifactsDir(h.root, h.project)

	if err := h.checkPrepared(); err != nil {
		return nil, err
	}

	// Containers of a remapped user namespace must traverse the work
	// directory to reach the certificates and htpasswd files.
	h.work, err = os.MkdirTemp("", h.project+"-")
	if err != nil {
		return nil, fmt.Errorf("create work directory: %w", err)
	}

	if err := os.Chmod(h.work, 0o755); err != nil {
		return h, fmt.Errorf("chmod work directory: %w", err)
	}

	fmt.Fprintf(os.Stderr, "e2e: project %s, work directory %s, artifacts %s\n", h.project, h.work, h.artifacts)

	composeVars := []string{
		envComposeCertsDir + "=" + filepath.Join(h.work, "certs"),
		envComposeAuthDir + "=" + filepath.Join(h.work, "auth"),
		envComposeAuth2Dir + "=" + filepath.Join(h.work, "auth2"),
	}
	h.compose = &compose{
		project: h.project,
		file:    filepath.Join(h.dir, "compose.yaml"),
		env:     hostEnviron(append(slices.Clone(composeVars), ownerEntry(h.project))...),
	}

	h.reaper, err = startReaper(reaperConfig{
		Project:     h.project,
		ComposeFile: h.compose.file,
		ComposeVars: composeVars,
		Artifacts:   h.artifacts,
		Work:        h.work,
	})
	if err != nil {
		return h, err
	}

	if err := h.buildBinaries(); err != nil {
		return h, err
	}

	if h.pki, err = newPKI(h.work); err != nil {
		return h, err
	}

	if err := h.startInfrastructure(); err != nil {
		return h, err
	}

	return h, nil
}

// checkPrepared fails before anything is started when an image of the run is
// missing, and remembers the validators image.
func (h *harness) checkPrepared() error {
	ctx, cancel := context.WithTimeout(context.Background(), dockerCmdTimeout)
	defer cancel()

	images, err := suiteImages(ctx, h.dir, h.jsonschema, h.artifacts)
	if err != nil {
		return err
	}

	if err := checkImages(ctx, images); err != nil {
		return err
	}

	for _, img := range images {
		if img.build != nil {
			h.validatorsImage = img.Ref
		}
	}

	return nil
}

func (h *harness) buildBinaries() error {
	h.bin = filepath.Join(h.work, "bin")

	builds := [][]string{
		{"build", "-trimpath", "-o", h.bin + string(filepath.Separator), "./cmd/schepherd", "./cmd/schepherd-publisher", "./tests/e2e/testconsumer"},
		{"build", "-trimpath", "-o", filepath.Join(h.bin, "docker-credential-e2e"), "./tests/e2e/credhelper"},
	}

	for _, args := range builds {
		ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
		out, err := runTool(ctx, h.root, hostEnviron("CGO_ENABLED=0", noGoDownloads), "go", args...)

		cancel()

		if err != nil {
			return fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, out)
		}
	}

	return nil
}

func (h *harness) startInfrastructure() error {
	ctx, cancel := context.WithTimeout(context.Background(), composeUpTimeout)
	defer cancel()

	// --wait only waits for the containers to run: no service has a
	// healthcheck, readiness is the /v2/ probe of discover.
	out, err := h.compose.run(ctx, "--profile", profileAuth, "up", "--detach", "--wait", "--pull", "never")
	if err != nil {
		return fmt.Errorf("docker compose up: %w\n%s", err, out)
	}

	h.source = &registry{service: serviceSource}
	h.mirror = &registry{service: serviceMirror}
	h.auth = &registry{service: serviceAuth, tls: true, user: h.pki.users[serviceAuth], pki: h.pki}
	h.auth2 = &registry{service: serviceAuth2, tls: true, user: h.pki.users[serviceAuth2], pki: h.pki}

	for _, r := range []*registry{h.source, h.mirror, h.auth, h.auth2} {
		h.services[r.service] = r

		if err := h.discover(ctx, r); err != nil {
			return err
		}
	}

	if h.sourceProxy, err = startProxy(h.source); err != nil {
		return err
	}

	if h.mirrorProxy, err = startProxy(h.mirror); err != nil {
		return err
	}

	return nil
}

// discover reads the service's current host port and waits until the
// registry answers /v2/. Docker assigns a new ephemeral port whenever a
// container starts, so this runs after every start.
func (h *harness) discover(ctx context.Context, r *registry) error {
	addr, err := h.compose.port(ctx, r.service, 5000)
	if err != nil {
		return err
	}

	r.setHost(addr)

	return r.waitReady(ctx, readinessDeadline)
}

func (h *harness) markFailed() {
	h.failedMu.Lock()
	defer h.failedMu.Unlock()

	h.failed = true
}

func (h *harness) isFailed() bool {
	h.failedMu.Lock()
	defer h.failedMu.Unlock()

	return h.failed
}

// tearDown stops the run's docker commands that are still running, dumps
// diagnostics after a failure, removes everything this run created and
// verifies that nothing of the project is left. It runs once, whether it is
// reached normally or from a signal.
func (h *harness) tearDown() error {
	var result error

	h.teardownMu.Do(func() {
		if h.compose != nil {
			result = stopOwnedProcesses(h.project)
		}

		for _, p := range []*proxy{h.sourceProxy, h.mirrorProxy} {
			if p != nil {
				p.p.Close()
			}
		}

		if h.compose != nil {
			if h.isFailed() {
				h.dumpDiagnostics()
			}

			result = errors.Join(result, removeProject(h.compose, h.artifacts))
		}

		if h.work != "" {
			if err := os.RemoveAll(h.work); err != nil {
				result = errors.Join(result, fmt.Errorf("remove work directory: %w", err))
			}
		}
	})

	return result
}

// releaseReaper lets the reaper exit without doing anything; call it after
// a teardown that ran with no test goroutine left.
func (h *harness) releaseReaper() {
	if h.reaper != nil {
		h.reaper.release()
	}
}

func (h *harness) dumpDiagnostics() {
	ctx, cancel := context.WithTimeout(context.Background(), dockerCmdTimeout)
	defer cancel()

	if err := os.MkdirAll(h.artifacts, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: create artifacts directory: %v\n", err)

		return
	}

	dumps := map[string][]string{
		"compose.log":    {"--profile", profileAuth, "--profile", profileNpm, "logs", "--no-color", "--timestamps"},
		"compose-ps.txt": {"--profile", profileAuth, "--profile", profileNpm, "ps", "--all"},
	}

	for name, args := range dumps {
		out, err := h.compose.run(ctx, args...)
		if err != nil {
			out = append(out, []byte("\n"+err.Error()+"\n")...)
		}

		if err := os.WriteFile(filepath.Join(h.artifacts, name), out, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: write %s: %v\n", name, err)
		}
	}

	fmt.Fprintf(os.Stderr, "e2e: compose logs saved to %s\n", h.artifacts)
}

func (h *harness) trapSignals() func() {
	signals := make(chan os.Signal, 1)
	done := make(chan struct{})

	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		select {
		case sig := <-signals:
			fmt.Fprintf(os.Stderr, "e2e: received %v, removing project %s\n", sig, h.project)
			h.markFailed()

			if err := h.tearDown(); err != nil {
				fmt.Fprintf(os.Stderr, "e2e: teardown: %v\n", err)
			}

			// Test goroutines keep running until os.Exit and may start
			// docker commands after the teardown; the reaper checks again
			// once this process is gone.
			if h.reaper != nil {
				h.reaper.handOver()
			}

			code := 1
			if s, ok := sig.(syscall.Signal); ok {
				code = 128 + int(s)
			}

			os.Exit(code)
		case <-done:
		}
	}()

	return func() {
		signal.Stop(signals)
		close(done)
	}
}

func checkDocker(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, dockerCmdTimeout)
	defer cancel()

	const hint = "the end-to-end suite needs a running Docker Engine with the Compose v2 and buildx plugins and never skips; " +
		"start Docker (or fix DOCKER_HOST / the docker context, or install the missing plugin) and run it again"

	if out, err := runTool(ctx, "", hostEnviron(), "docker", "version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("%s: 'docker version' failed: %w\n%s", hint, err, out)
	}

	if out, err := runTool(ctx, "", hostEnviron(), "docker", "compose", "version", "--short"); err != nil {
		return fmt.Errorf("%s: 'docker compose version' failed: %w\n%s", hint, err, out)
	}

	if out, err := runTool(ctx, "", hostEnviron(), "docker", "buildx", "version"); err != nil {
		return fmt.Errorf("%s: 'docker buildx version' failed: %w\n%s", hint, err, out)
	}

	return nil
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}

	for {
		if isModuleRoot(filepath.Join(dir, "go.mod")) {
			return dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod of module %s above the working directory", moduleName)
		}

		dir = parent
	}
}

func isModuleRoot(goMod string) bool {
	f, err := os.Open(goMod)
	if err != nil {
		return false
	}

	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if fields := strings.Fields(scanner.Text()); len(fields) == 2 && fields[0] == "module" {
			return fields[1] == moduleName
		}
	}

	return false
}

func resolveArtifactsDir(root, project string) string {
	dir := artifactsDirSetting()
	if dir == "" {
		return filepath.Join(root, "tests", "e2e", ".artifacts", project)
	}

	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}

	return filepath.Clean(dir)
}

func trimOutput(out []byte) string {
	return string(bytes.TrimSpace(out))
}
