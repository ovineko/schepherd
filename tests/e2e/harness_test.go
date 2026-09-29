//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
)

const defaultRunTimeout = 5 * time.Minute

// sandbox is an isolated user environment: HOME, the XDG directories,
// DOCKER_CONFIG and TMPDIR all live below Dir, so a binary under test never
// sees the developer's credentials, configuration or cache.
type sandbox struct {
	Dir string
	// Home is $HOME.
	Home string
	// CacheDir is where schepherd keeps its cache by default
	// ($XDG_CACHE_HOME/schepherd); it does not exist until schepherd
	// creates it.
	CacheDir string
	// DockerConfig is $DOCKER_CONFIG, an empty directory until a test writes
	// config.json into it (writeDockerConfig, writeCredsStoreConfig).
	DockerConfig string
	// Workspace is the default working directory of commands.
	Workspace string
	// Tmp is $TMPDIR.
	Tmp string

	xdg string
}

var sandboxes sync.Map

// sandboxOf returns the sandbox shared by every command of one test (and
// only that test: subtests get their own). Create more with newSandbox.
func sandboxOf(t *testing.T) *sandbox {
	t.Helper()

	if sb, ok := sandboxes.Load(t); ok {
		return sb.(*sandbox)
	}

	sb := newSandbox(t)
	sandboxes.Store(t, sb)
	t.Cleanup(func() { sandboxes.Delete(t) })

	return sb
}

// newSandbox creates a fresh sandbox below t.TempDir().
func newSandbox(t *testing.T) *sandbox {
	t.Helper()

	dir := t.TempDir()
	sb := &sandbox{
		Dir:          dir,
		Home:         filepath.Join(dir, "home"),
		DockerConfig: filepath.Join(dir, "docker"),
		Workspace:    filepath.Join(dir, "workspace"),
		Tmp:          filepath.Join(dir, "tmp"),
		xdg:          filepath.Join(dir, "xdg"),
	}

	sb.CacheDir = filepath.Join(sb.xdg, "cache", "schepherd")

	for _, d := range []string{sb.Home, sb.DockerConfig, sb.Workspace, sb.Tmp, filepath.Join(sb.xdg, "run")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("create sandbox: %v", err)
		}
	}

	return sb
}

// environ is the complete environment of a binary under test. PATH holds the
// run's binaries (so docker-credential-e2e and testconsumer resolve by
// name) and the system directories; nothing else of the developer's
// environment is passed on.
func (s *sandbox) environ() []string {
	return []string{
		"HOME=" + s.Home,
		"XDG_CACHE_HOME=" + filepath.Join(s.xdg, "cache"),
		"XDG_CONFIG_HOME=" + filepath.Join(s.xdg, "config"),
		"XDG_DATA_HOME=" + filepath.Join(s.xdg, "data"),
		"XDG_STATE_HOME=" + filepath.Join(s.xdg, "state"),
		"XDG_RUNTIME_DIR=" + filepath.Join(s.xdg, "run"),
		"DOCKER_CONFIG=" + s.DockerConfig,
		"TMPDIR=" + s.Tmp,
		"PATH=" + suite.bin + string(os.PathListSeparator) + "/usr/local/bin:/usr/bin:/bin",
		"TZ=UTC",
	}
}

// runOpts configures a command run by cli, publisher, runBin or startBin.
type runOpts struct {
	// Sandbox defaults to sandboxOf(t).
	Sandbox *sandbox
	// Dir is the working directory; default Sandbox.Workspace.
	Dir string
	// Env entries (KEY=VALUE) are added to the sandbox environment; a later
	// entry replaces an earlier one with the same key.
	Env []string
	// Stdin is the standard input; nil means /dev/null.
	Stdin []byte
	// Timeout kills a hanging command and fails the test; default 5m.
	Timeout time.Duration

	rawEnv bool
}

// result is the outcome of a finished command.
type result struct {
	Args     []string
	Stdout   []byte
	Stderr   []byte
	Code     int
	Duration time.Duration
	// Signal is the signal that killed the process (Code is -1 then).
	Signal syscall.Signal
	// Err is set when the command could not run or was killed by the
	// harness timeout; Code is -1 then.
	Err error
}

func (r result) String() string {
	return fmt.Sprintf("%s\nexit code %d (%v)\n--- stdout ---\n%s\n--- stderr ---\n%s",
		strings.Join(r.Args, " "), r.Code, r.Err, clip(r.Stdout), clip(r.Stderr))
}

// wantCode fails the test unless the command exited with code.
func (r result) wantCode(t *testing.T, code int) result {
	t.Helper()

	if r.Code != code {
		t.Fatalf("want exit code %d, got:\n%s", code, r)
	}

	return r
}

// ok fails the test unless the command exited with 0.
func (r result) ok(t *testing.T) result {
	t.Helper()

	return r.wantCode(t, 0)
}

// cli runs the schepherd binary.
func cli(t *testing.T, o runOpts, args ...string) result {
	t.Helper()

	return runBin(t, "schepherd", o, args...)
}

// publisher runs the schepherd-publisher binary.
func publisher(t *testing.T, o runOpts, args ...string) result {
	t.Helper()

	return runBin(t, "schepherd-publisher", o, args...)
}

// binPath returns the absolute path of a binary built for this run:
// schepherd, schepherd-publisher, testconsumer or docker-credential-e2e.
func binPath(name string) string {
	return filepath.Join(suite.bin, name)
}

// hostTool runs a developer tool on the host, for example
// "go run ./tools/release ..." or ".tools/bin/goreleaser", and waits for it.
// Unlike cli and publisher it gets the developer's environment (hostEnviron
// with GOPROXY=off, then o.Env on top) and no sandbox: o.Sandbox is ignored,
// o.Dir defaults to the repository root. name is a command on the host PATH
// or a path relative to the repository root.
func hostTool(t *testing.T, o runOpts, name string, args ...string) result {
	t.Helper()

	if strings.ContainsRune(name, '/') && !filepath.IsAbs(name) {
		name = filepath.Join(suite.root, filepath.FromSlash(name))
	}

	if o.Dir == "" {
		o.Dir = suite.root
	}

	o.Env = mergeEnv(hostEnviron(noGoDownloads), o.Env)
	o.rawEnv = true

	return runProcess(t, name, o, args...)
}

// perfEnabled reports whether E2E_PERF asked for the TestPerf_*
// measurements; an invalid value fails the run at start.
func perfEnabled() bool {
	return suite.perf
}

// runBin runs one of the run's binaries and waits for it. A command that
// cannot start or exceeds the timeout fails the test; any exit code is a
// normal result.
func runBin(t *testing.T, name string, o runOpts, args ...string) result {
	t.Helper()

	return runProcess(t, binPath(name), o, args...)
}

func runProcess(t *testing.T, path string, o runOpts, args ...string) result {
	t.Helper()

	p := startProcess(t, path, o, args...)

	res := p.Wait()
	if res.Err != nil {
		t.Fatalf("run: %v\n%s", res.Err, res)
	}

	return res
}

// process is a command started by startBin.
type process struct {
	cmd    *exec.Cmd
	args   []string
	stdout bytes.Buffer
	stderr bytes.Buffer
	start  time.Time
	ctx    context.Context //nolint:containedctx // the deadline of this one process, checked by Wait
	cancel context.CancelFunc

	once sync.Once
	res  result
}

// startBin starts one of the run's binaries without waiting; the test ends
// by calling Wait (from any goroutine). A process still running at the end
// of the test is killed.
func startBin(t *testing.T, name string, o runOpts, args ...string) *process {
	t.Helper()

	return startProcess(t, binPath(name), o, args...)
}

func startProcess(t *testing.T, path string, o runOpts, args ...string) *process {
	t.Helper()

	if o.Timeout == 0 {
		o.Timeout = defaultRunTimeout
	}

	env := o.Env
	if !o.rawEnv {
		sb := o.Sandbox
		if sb == nil {
			sb = sandboxOf(t)
		}

		if o.Dir == "" {
			o.Dir = sb.Workspace
		}

		env = mergeEnv(sb.environ(), o.Env)
	}

	ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = o.Dir
	cmd.Env = env
	// A consumer's grandchild can keep stdout open after the process
	// exits; Wait must not block on it forever.
	cmd.WaitDelay = 10 * time.Second

	if o.Stdin != nil {
		cmd.Stdin = bytes.NewReader(o.Stdin)
	}

	p := &process{cmd: cmd, args: append([]string{filepath.Base(path)}, args...), ctx: ctx, cancel: cancel}
	cmd.Stdout = &p.stdout
	cmd.Stderr = &p.stderr

	p.start = time.Now()
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start %s: %v", path, err)
	}

	t.Cleanup(func() {
		_ = p.cmd.Process.Kill()
		p.Wait()
	})

	return p
}

// PID is the process ID.
func (p *process) PID() int {
	return p.cmd.Process.Pid
}

// Signal sends sig to the process.
func (p *process) Signal(sig os.Signal) error {
	if err := p.cmd.Process.Signal(sig); err != nil {
		return fmt.Errorf("signal %d: %w", p.PID(), err)
	}

	return nil
}

// Wait waits for the process and returns its result; later calls return the
// same result.
func (p *process) Wait() result {
	p.once.Do(func() {
		err := p.cmd.Wait()
		timedOut := errors.Is(p.ctx.Err(), context.DeadlineExceeded)

		p.cancel()

		p.res = result{Args: p.args, Stdout: p.stdout.Bytes(), Stderr: p.stderr.Bytes(), Duration: time.Since(p.start), Code: -1}

		state := p.cmd.ProcessState
		switch {
		case state == nil:
			p.res.Err = err
		case timedOut && !state.Exited():
			p.res.Err = errors.New("killed by the harness timeout")
		default:
			p.res.Code = state.ExitCode()
			if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				p.res.Signal = ws.Signal()
			}
		}
	})

	return p.res
}

func mergeEnv(base, extra []string) []string {
	out := slices.Clone(base)

	for _, kv := range extra {
		key, _, _ := strings.Cut(kv, "=")
		out = slices.DeleteFunc(out, func(existing string) bool {
			k, _, _ := strings.Cut(existing, "=")

			return k == key
		})
		out = append(out, kv)
	}

	return out
}

func clip(b []byte) string {
	const limit = 8 << 10
	if len(b) <= limit {
		return string(b)
	}

	return string(b[:limit]) + fmt.Sprintf("\n... (%d more bytes)", len(b)-limit)
}

// registrySettings is one [registries."host"] table of a configuration.
type registrySettings struct {
	PlainHTTP       bool
	CAFile          string
	CredentialsFile string
}

// clientConfig renders a schepherd configuration file (config_version 1).
type clientConfig struct {
	Repository string
	Catalog    string
	// Registries replaces the generated table of a host. Every other
	// suite registry and live proxy gets its default table: plain_http for
	// the plain registries and all proxies, ca_file (the run's CA) for the
	// TLS registries. Map a host to nil to leave it out entirely.
	Registries map[string]*registrySettings
	// Extra is appended verbatim ([runner], [[mappings]], [limits], ...).
	Extra string
}

// TOML renders the configuration.
func (c clientConfig) TOML() string {
	var b strings.Builder

	b.WriteString("config_version = 1\n")

	if c.Repository != "" || c.Catalog != "" {
		b.WriteString("\n[catalog]\n")

		if c.Repository != "" {
			b.WriteString("repository = " + tomlString(c.Repository) + "\n")
		}

		if c.Catalog != "" {
			b.WriteString("digest = " + tomlString(c.Catalog) + "\n")
		}
	}

	settings := defaultRegistrySettings()
	maps.Copy(settings, c.Registries)

	b.WriteString(registriesTOML(settings))

	if c.Extra != "" {
		b.WriteString("\n" + strings.TrimSpace(c.Extra) + "\n")
	}

	return b.String()
}

func defaultRegistrySettings() map[string]*registrySettings {
	out := map[string]*registrySettings{}

	for _, r := range []*registry{suite.source, suite.mirror, suite.auth, suite.auth2} {
		if r.tls {
			out[r.Host()] = &registrySettings{CAFile: suite.pki.CAFile}
		} else {
			out[r.Host()] = &registrySettings{PlainHTTP: true}
		}

		r.mu.RLock()
		for _, p := range r.proxies {
			out[p.Host()] = &registrySettings{PlainHTTP: true}
		}
		r.mu.RUnlock()
	}

	return out
}

func registriesTOML(settings map[string]*registrySettings) string {
	hosts := make([]string, 0, len(settings))
	for host, s := range settings {
		if s != nil {
			hosts = append(hosts, host)
		}
	}

	slices.Sort(hosts)

	var b strings.Builder

	for _, host := range hosts {
		s := settings[host]
		b.WriteString("\n[registries." + tomlString(host) + "]\n")

		if s.PlainHTTP {
			b.WriteString("plain_http = true\n")
		}

		if s.CAFile != "" {
			b.WriteString("ca_file = " + tomlString(s.CAFile) + "\n")
		}

		if s.CredentialsFile != "" {
			b.WriteString("credentials_file = " + tomlString(s.CredentialsFile) + "\n")
		}
	}

	return b.String()
}

// tomlString quotes s as a TOML basic string; JSON string escapes are valid
// TOML escapes.
func tomlString(s string) string {
	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)

	return strings.TrimSpace(buf.String())
}

// writeConfig writes a configuration to ws/schepherd.toml and returns its
// absolute path. Pass it with --config; schepherd never finds it on its own.
func writeConfig(t *testing.T, ws, toml string) string {
	t.Helper()

	path := filepath.Join(ws, "schepherd.toml")
	writeFile(t, path, []byte(toml))

	return path
}

// publisherRegistryConfig writes the --registry-config file of the
// publisher for the current addresses (plain_http for plain registries and
// proxies; ca_file and a credentials file for the TLS registries) and
// returns its path.
func publisherRegistryConfig(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	settings := defaultRegistrySettings()

	for _, r := range []*registry{suite.auth, suite.auth2} {
		credDir := filepath.Join(dir, r.service)
		settings[r.Host()].CredentialsFile = writeDockerConfig(t, credDir, map[string]credential{r.Host(): r.user})
	}

	path := filepath.Join(dir, "registries.toml")
	writeFile(t, path, []byte(strings.TrimSpace(registriesTOML(settings))+"\n"))

	return path
}

// repoPath returns the repository path of the calling test,
// e2e/<test name>/<name> (name defaults to "schemas"). Registries are
// shared by all tests of a run, so every test works in its own paths.
func repoPath(t *testing.T, name ...string) string {
	t.Helper()

	leaf := "schemas"
	if len(name) > 0 {
		leaf = name[0]
	}

	names := append(strings.Split(t.Name(), "/"), leaf)
	parts := make([]string, 0, len(names)+1)
	parts = append(parts, "e2e")

	for _, p := range names {
		parts = append(parts, repoComponent(p))
	}

	return strings.Join(parts, "/")
}

var repoInvalid = regexp.MustCompile(`[^a-z0-9]+`)

func repoComponent(s string) string {
	s = strings.Trim(repoInvalid.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" {
		return "x"
	}

	return s
}

// fixture returns the absolute path of tests/e2e/fixtures/<parts...>.
func fixture(parts ...string) string {
	return filepath.Join(append([]string{suite.dir, "fixtures"}, parts...)...)
}

// newSet copies the fixture directory fixtures/<name> (a local source set:
// source.toml, licenses.toml, schemas/) into a fresh directory and returns
// its path, so a test can modify its copy.
func newSet(t *testing.T, name string) string {
	t.Helper()

	dst := filepath.Join(t.TempDir(), name)
	copyTree(t, fixture(name), dst)

	return dst
}

// publishResult is the --json output of schepherd-publisher publish
// (docs/publishing.md, "Publishing").
type publishResult struct {
	Status          string   `json:"status"`
	Repository      string   `json:"repository"`
	Revision        string   `json:"revision"`
	CatalogDigest   string   `json:"catalogDigest"`
	Added           []string `json:"added"`
	Changed         []string `json:"changed"`
	RemovedUpstream []string `json:"removedUpstream"`
	KeptArtifacts   []string `json:"keptArtifacts"`
	Tags            struct {
		Created  []string `json:"created"`
		Existing []string `json:"existing"`
	} `json:"tags"`
	CatalogSize     int64 `json:"catalogSize"`
	Unchanged       int   `json:"unchanged"`
	UploadedSchemas int   `json:"uploadedSchemas"`
	ReusedSchemas   int   `json:"reusedSchemas"`

	// Prepared is the prepared directory that was published.
	Prepared string `json:"-"`
	// Raw is the complete JSON output.
	Raw []byte `json:"-"`
}

// prepareSet runs "schepherd-publisher prepare" for set/source.toml with the
// pinned Sourcemeta binary and returns the prepared directory. extra flags
// are appended (for example --state <file>).
func prepareSet(t *testing.T, set string, extra ...string) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "prepared")
	args := append([]string{"prepare", "--source", filepath.Join(set, "source.toml"), "--out", out, "--jsonschema", suite.jsonschema, "--json"}, extra...)
	publisher(t, runOpts{}, args...).ok(t)

	return out
}

// publishPrepared runs "schepherd-publisher publish" for a prepared
// directory into repo (host/path) with the run's registry configuration and
// returns the decoded result; extra flags are appended (--now
// 20260101.0000, --state <file> --state-out <file>, --update-latest, ...).
// The command must succeed; use publisher() directly to test failures.
func publishPrepared(t *testing.T, repo, prepared string, extra ...string) publishResult {
	t.Helper()

	args := append([]string{"publish", "--prepared", prepared, "--repository", repo, "--registry-config", publisherRegistryConfig(t), "--json"}, extra...)
	res := publisher(t, runOpts{}, args...).ok(t)

	var out publishResult
	if err := json.Unmarshal(res.Stdout, &out); err != nil {
		t.Fatalf("decode publish result: %v\n%s", err, res)
	}

	out.Prepared = prepared
	out.Raw = res.Stdout

	return out
}

// publishSet prepares a set directory (see newSet) and publishes it to repo.
func publishSet(t *testing.T, repo, set string, extra ...string) publishResult {
	t.Helper()

	return publishPrepared(t, repo, prepareSet(t, set), extra...)
}

// preparedSet is prepared.json of a prepared directory.
type preparedSet struct {
	FormatVersion int             `json:"formatVersion"`
	Entries       []preparedEntry `json:"entries"`
}

type preparedEntry struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Dialect       string   `json:"dialect"`
	FileMatch     []string `json:"fileMatch"`
	Schema        string   `json:"schema"`
	ContentDigest string   `json:"contentDigest"`
	Notice        string   `json:"notice"`
}

// readPrepared decodes prepared.json of a prepared directory.
func readPrepared(t *testing.T, prepared string) preparedSet {
	t.Helper()

	var set preparedSet
	if err := json.Unmarshal(readFile(t, filepath.Join(prepared, "prepared.json")), &set); err != nil {
		t.Fatalf("decode prepared.json: %v", err)
	}

	return set
}

// preparedSchema returns the exact prepared bytes of one schema.
func preparedSchema(t *testing.T, prepared, id string) []byte {
	t.Helper()

	for _, e := range readPrepared(t, prepared).Entries {
		if e.ID == id {
			return readFile(t, filepath.Join(prepared, filepath.FromSlash(e.Schema)))
		}
	}

	t.Fatalf("prepared set %s has no schema %q", prepared, id)

	return nil
}

// writeFile writes data to path, creating parent directories.
func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create directory for %s: %v", path, err)
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeFiles writes files (slash-separated relative path -> content) below
// root and returns root.
func writeFiles(t *testing.T, root string, files map[string]string) string {
	t.Helper()

	for rel, content := range files {
		writeFile(t, filepath.Join(root, filepath.FromSlash(rel)), []byte(content))
	}

	return root
}

// readFile returns the content of path or fails the test.
func readFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return data
}

// copyTree copies the regular files and directories of src to dst.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()

	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		target := filepath.Join(dst, rel)

		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}

			return os.WriteFile(target, data, 0o644)
		default:
			return fmt.Errorf("%s is neither a file nor a directory", path)
		}
	})
	if err != nil {
		t.Fatalf("copy %s to %s: %v", src, dst, err)
	}
}

// fileTree lists the paths of all entries below root (slash-separated,
// sorted) with a trailing "/" for directories and " -> target" for
// symlinks; use it to assert that nothing was written somewhere.
func fileTree(t *testing.T, root string) []string {
	t.Helper()

	var out []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if path == root {
			return nil
		}

		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		switch {
		case d.IsDir():
			rel += "/"
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}

			rel += " -> " + target
		}

		out = append(out, rel)

		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("walk %s: %v", root, err)
	}

	return out
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// digestOf returns "sha256:<hex>" of b.
func digestOf(b []byte) string {
	return "sha256:" + sha256Hex(b)
}

func digestFor(b []byte) digest.Digest {
	return digest.Digest(digestOf(b))
}

// decodeJSON decodes data into a T or fails the test.
func decodeJSON[T any](t *testing.T, data []byte) T {
	t.Helper()

	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode %T: %v\n%s", v, err, clip(data))
	}

	return v
}

// eventually polls cond every 50ms until it returns true or timeout passes.
func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// processAlive reports whether pid exists and is not a zombie.
func processAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}

	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}

	// The state follows the parenthesized command name, which may itself
	// contain spaces and parentheses.
	fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))

	return len(fields) == 0 || fields[0] != "Z"
}

// waitGone waits until pid no longer runs (or is a zombie) and fails the
// test after timeout.
func waitGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()

	eventually(t, timeout, "process "+strconv.Itoa(pid)+" to exit", func() bool { return !processAlive(pid) })
}
