package runner

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/interp"
)

// The test binary doubles as a fake consumer: when fakeVar is set it never
// runs tests and instead behaves as described by the JSON config it names.
const (
	fakeVar       = "SCHEPHERD_RUNNER_TEST_FAKE"
	keyVar        = "SCHEPHERD_RUNNER_TEST_KEY"
	grandchildVar = "SCHEPHERD_RUNNER_TEST_GRANDCHILD"
	defaultKey    = "*"
)

// raceNoExitDelay stops race-enabled fake consumers from sleeping a second
// before every successful exit; builds without -race ignore it.
const raceNoExitDelay = "GORACE=atexit_sleep_ms=0"

// coverDirVar receives the coverage counters of fake consumers. A
// coverage-instrumented test binary started without it prints a warning to
// stderr on exit, which would corrupt the consumer output the tests compare.
const coverDirVar = "GOCOVERDIR"

type fakeConfig struct {
	Behaviors map[string]fakeBehavior `json:"behaviors"`
	LogDir    string                  `json:"logDir"`
	EnvKeys   []string                `json:"envKeys"`
}

type fakeBehavior struct {
	Grandchild            string   `json:"grandchild"`
	WaitFile              string   `json:"waitFile"`
	Stdout                []string `json:"stdout"`
	Stderr                []string `json:"stderr"`
	Exit                  int      `json:"exit"`
	SleepMs               int      `json:"sleepMs"`
	PauseMs               int      `json:"pauseMs"`
	Hang                  bool     `json:"hang"`
	IgnoreTerm            bool     `json:"ignoreTerm"`
	SelfKill              bool     `json:"selfKill"`
	GrandchildHoldsOutput bool     `json:"grandchildHoldsOutput"`
}

type fakeRecord struct {
	Env   map[string]*string `json:"env"`
	Args  []string           `json:"args"`
	Cwd   string             `json:"cwd"`
	Key   string             `json:"key"`
	Stdin []byte             `json:"stdin"`
	Pid   int                `json:"pid"`
	Start int64              `json:"start"`
}

func TestMain(m *testing.M) {
	if pidFile, ok := env.Lookup(grandchildVar); ok {
		os.Exit(runGrandchild(pidFile))
	}

	if config, ok := env.Lookup(fakeVar); ok {
		os.Exit(runFake(config))
	}

	os.Exit(m.Run())
}

func runGrandchild(pidFile string) int {
	if err := writeFileAtomic(pidFile, []byte(strconv.Itoa(os.Getpid()))); err != nil {
		return 90
	}

	time.Sleep(30 * time.Second)

	return 0
}

func runFake(configPath string) int {
	data, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake consumer:", err)

		return 91
	}

	var cfg fakeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "fake consumer:", err)

		return 91
	}

	key, _ := env.Lookup(keyVar)

	b, ok := cfg.Behaviors[key]
	if !ok {
		b = cfg.Behaviors[defaultKey]
	}

	if b.IgnoreTerm {
		signal.Ignore(syscall.SIGTERM)
	}

	if err := logInvocation(cfg, key); err != nil {
		fmt.Fprintln(os.Stderr, "fake consumer:", err)

		return 92
	}

	if b.Grandchild != "" {
		if err := spawnGrandchild(b); err != nil {
			fmt.Fprintln(os.Stderr, "fake consumer:", err)

			return 93
		}
	}

	return behave(b)
}

func behave(b fakeBehavior) int {
	if b.WaitFile != "" && !waitForFile(b.WaitFile, time.Minute) {
		return 94
	}

	pause := time.Duration(b.PauseMs) * time.Millisecond

	for _, chunk := range b.Stdout {
		_, _ = os.Stdout.WriteString(chunk)
		time.Sleep(pause)
	}

	for _, chunk := range b.Stderr {
		_, _ = os.Stderr.WriteString(chunk)
		time.Sleep(pause)
	}

	time.Sleep(time.Duration(b.SleepMs) * time.Millisecond)

	if b.SelfKill {
		if p, err := os.FindProcess(os.Getpid()); err == nil {
			_ = p.Kill()
		}

		time.Sleep(time.Minute)
	}

	if b.Hang {
		time.Sleep(time.Hour)
	}

	return b.Exit
}

func logInvocation(cfg fakeConfig, key string) error {
	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	rec := fakeRecord{Args: os.Args, Cwd: cwd, Key: key, Stdin: stdin, Pid: os.Getpid(), Start: time.Now().UnixNano(), Env: map[string]*string{}}

	for _, name := range cfg.EnvKeys {
		if value, ok := env.Lookup(name); ok {
			rec.Env[name] = &value
		} else {
			rec.Env[name] = nil
		}
	}

	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode record: %w", err)
	}

	return writeFileAtomic(filepath.Join(cfg.LogDir, fmt.Sprintf("%d-%d.json", rec.Start, rec.Pid)), data)
}

func spawnGrandchild(b fakeBehavior) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}

	cmd := exec.Command(self)
	cmd.Env = append(env.Environ(), grandchildVar+"="+b.Grandchild)

	if b.GrandchildHoldsOutput {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start grandchild: %w", err)
	}

	if !waitForFile(b.Grandchild, 20*time.Second) {
		return errors.New("grandchild did not report its pid")
	}

	return nil
}

func waitForFile(path string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}

		time.Sleep(10 * time.Millisecond)
	}

	return false
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}

	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}

	return nil
}

type harness struct {
	t         *testing.T
	root      string
	exe       string
	config    string
	logDir    string
	workspace string
	cache     string
	coverDir  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	if strings.ContainsAny(exe, "{}") || strings.Contains(exe, "${") {
		t.Fatalf("test binary path %q cannot be used as a literal template", exe)
	}

	root := t.TempDir()
	h := &harness{
		t:         t,
		root:      root,
		exe:       exe,
		config:    filepath.Join(root, "fake.json"),
		logDir:    filepath.Join(root, "log"),
		workspace: filepath.Join(root, "ws"),
		cache:     filepath.Join(root, "cache"),
		coverDir:  filepath.Join(root, "cover"),
	}

	for _, dir := range []string{h.logDir, h.workspace, h.cache, h.coverDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	h.behave(map[string]fakeBehavior{defaultKey: {}})

	return h
}

func (h *harness) behave(behaviors map[string]fakeBehavior, envKeys ...string) {
	h.t.Helper()

	data, err := json.Marshal(fakeConfig{Behaviors: behaviors, LogDir: h.logDir, EnvKeys: envKeys})
	if err != nil {
		h.t.Fatal(err)
	}

	if err := os.WriteFile(h.config, data, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) spec(mode Mode, args ...string) Spec {
	return Spec{
		Mode:       mode,
		Command:    h.exe,
		Args:       args,
		Cwd:        DefaultCwd,
		Timeout:    30 * time.Second,
		Jobs:       1,
		InheritEnv: true,
		Env:        map[string]string{keyVar: "{" + interp.SchemaID + "}"},
	}
}

func (h *harness) options(spec Spec) Options {
	return Options{
		Spec:      spec,
		Workspace: h.workspace,
		CacheDir:  h.cache,
		Lookup:    lookupFrom(nil),
		Environ:   []string{fakeVar + "=" + h.config, raceNoExitDelay, coverDirVar + "=" + h.coverDir},
		KillGrace: 300 * time.Millisecond,
	}
}

func (h *harness) schemas(ids ...string) map[string]SchemaInfo {
	out := map[string]SchemaInfo{}
	for _, id := range ids {
		out[id] = SchemaInfo{
			ID:     id,
			Path:   filepath.Join(h.cache, "schemas", id, "schema.json"),
			Ref:    "registry.example/org/schemas@sha256:" + strings.Repeat(id[:1], 64),
			Origin: "catalog",
		}
	}

	return out
}

func (h *harness) file(name, content string) string {
	h.t.Helper()

	path := filepath.Join(h.workspace, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}

	return path
}

func (h *harness) run(inputs []Input, schemas map[string]SchemaInfo, opts Options) (*Report, error) {
	h.t.Helper()

	plan, err := BuildPlan(inputs, schemas, opts)
	if err != nil {
		h.t.Fatalf("BuildPlan: %v", err)
	}

	return Execute(h.t.Context(), plan, opts)
}

func (h *harness) records() []fakeRecord {
	h.t.Helper()

	entries, err := os.ReadDir(h.logDir)
	if err != nil {
		h.t.Fatal(err)
	}

	var out []fakeRecord

	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(h.logDir, e.Name()))
		if err != nil {
			h.t.Fatal(err)
		}

		var rec fakeRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			h.t.Fatal(err)
		}

		out = append(out, rec)
	}

	slices.SortFunc(out, func(a, b fakeRecord) int { return cmp.Compare(a.Start, b.Start) })

	return out
}

func lookupFrom(m map[string]string) interp.LookupFunc {
	return func(name string) (string, bool) {
		v, ok := m[name]

		return v, ok
	}
}

func readPid(t *testing.T, path string, within time.Duration) int {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var pid int
			if _, err := fmt.Sscan(string(data), &pid); err != nil {
				t.Fatalf("pid file %s: %v", path, err)
			}

			return pid
		}

		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("pid file %s did not appear within %s", path, within)

	return 0
}

func waitDead(t *testing.T, pid int, within time.Duration) {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("process %d is still alive after %s", pid, within)
}

func wantKind(t *testing.T, err error, kind fault.Kind) {
	t.Helper()

	if err == nil {
		t.Fatalf("got nil error, want kind %s", kind)
	}

	if got := fault.KindOf(err); got != kind {
		t.Fatalf("error %q has kind %s, want %s", err, got, kind)
	}
}
