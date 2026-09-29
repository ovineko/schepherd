package cache

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/digest"
)

const helperModeEnv = "SCHEPHERD_CACHE_TEST_HELPER"

const (
	exitHelperFailed  = 1
	exitHelperOverlap = 2
	exitHelperTimeout = 3
)

// TestHelperProcess is not a test: it is the body of child processes started
// by the cross-process tests, selected by helperModeEnv.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		return
	}

	args := helperArgs()

	code, err := runHelper(t.Context(), mode, args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper:", err)
	}

	os.Exit(code)
}

func helperArgs() []string {
	for i, arg := range os.Args {
		if arg == "--" {
			return os.Args[i+1:]
		}
	}

	return nil
}

func runHelper(ctx context.Context, mode string, args []string) (int, error) {
	if len(args) < 1 {
		return exitHelperFailed, errors.New("missing cache directory")
	}

	c, err := Open(args[0])
	if err != nil {
		return exitHelperFailed, err
	}
	defer func() { _ = c.Close() }()

	switch mode {
	case "counter":
		return helperCounter(ctx, c, args[1:])
	case "try":
		return helperTry(ctx, c, args[1:])
	case "hold":
		return helperHold(ctx, c, args[1:])
	case "materialize":
		return helperMaterialize(c, args[1:])
	default:
		return exitHelperFailed, fmt.Errorf("unknown mode %q", mode)
	}
}

func helperCounter(ctx context.Context, c *Cache, args []string) (int, error) {
	iterations, err := strconv.Atoi(args[1])
	if err != nil {
		return exitHelperFailed, err
	}

	for range iterations {
		overlap, err := lockedIncrement(ctx, c, args[0])
		if overlap {
			return exitHelperOverlap, err
		}

		if err != nil {
			return exitHelperFailed, err
		}
	}

	return 0, nil
}

// lockedIncrement performs a deliberately non-atomic read-modify-write of a
// counter file under the cache lock, and detects a second holder through an
// O_EXCL marker file.
func lockedIncrement(ctx context.Context, c *Cache, work string) (overlap bool, err error) {
	release, err := c.Lock(ctx, "counter")
	if err != nil {
		return false, err
	}
	defer release()

	marker := filepath.Join(work, "inside")

	f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return true, fmt.Errorf("another holder is inside: %w", err)
	}

	_ = f.Close()

	counterPath := filepath.Join(work, "counter")

	raw, err := os.ReadFile(counterPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	n := 0
	if len(raw) > 0 {
		if n, err = strconv.Atoi(string(raw)); err != nil {
			return false, err
		}
	}

	time.Sleep(time.Millisecond)

	if err := os.WriteFile(counterPath, []byte(strconv.Itoa(n+1)), 0o600); err != nil {
		return false, err
	}

	return false, os.Remove(marker)
}

func helperTry(ctx context.Context, c *Cache, args []string) (int, error) {
	wait, err := time.ParseDuration(args[1])
	if err != nil {
		return exitHelperFailed, err
	}

	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	release, err := c.Lock(ctx, args[0])
	if errors.Is(err, context.DeadlineExceeded) {
		return exitHelperTimeout, nil
	}

	if err != nil {
		return exitHelperFailed, err
	}

	release()

	return 0, nil
}

func helperHold(ctx context.Context, c *Cache, args []string) (int, error) {
	release, err := c.Lock(ctx, args[0])
	if err != nil {
		return exitHelperFailed, err
	}
	defer release()

	fmt.Println("locked")

	_, _ = io.Copy(io.Discard, os.Stdin)

	return 0, nil
}

func helperMaterialize(c *Cache, args []string) (int, error) {
	content, err := os.ReadFile(args[0])
	if err != nil {
		return exitHelperFailed, err
	}

	manifest := args[1]
	contentDigest := digest.FromBytes(content)

	fmt.Println("ready")

	_, _ = io.Copy(io.Discard, os.Stdin)

	var wg sync.WaitGroup

	errs := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			_, err := c.WriteMaterialized(manifest, contentDigest, int64(len(content)), writeChunks(content, 256, time.Millisecond))
			errs <- err
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			return exitHelperFailed, err
		}
	}

	return 0, nil
}

func helperCommand(t *testing.T, mode string, args ...string) *exec.Cmd {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), os.Args[0], append([]string{"-test.run=^TestHelperProcess$", "--"}, args...)...)
	cmd.Env = append(cmd.Environ(), helperModeEnv+"="+mode)
	cmd.Stderr = &syncBuffer{}

	return cmd
}

// Closing the returned stdin releases a child that blocks reading it.
func startHelper(t *testing.T, cmd *exec.Cmd, want string) io.WriteCloser {
	t.Helper()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = stdin.Close() })

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != want+"\n" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("helper did not report %q: %q, %v (stderr: %s)", want, line, err, cmd.Stderr)
	}

	return stdin
}

func exitCode(t *testing.T, cmd *exec.Cmd, err error) int {
	t.Helper()

	if err == nil {
		return 0
	}

	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode()
	}

	t.Fatalf("helper %v: %v (stderr: %s)", cmd.Args, err, cmd.Stderr)

	return -1
}

func TestLockAcrossProcesses(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	work := t.TempDir()
	c := openCacheAt(t, dir)

	const (
		processes  = 3
		goroutines = 3
		iterations = 12
	)

	cmds := make([]*exec.Cmd, 0, processes)
	for range processes {
		cmd := helperCommand(t, "counter", dir, work, strconv.Itoa(iterations))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}

		cmds = append(cmds, cmd)
	}

	var wg sync.WaitGroup

	errs := make(chan error, goroutines)
	for range goroutines {
		wg.Go(func() {
			for range iterations {
				if _, err := lockedIncrement(t.Context(), c, work); err != nil {
					errs <- err

					return
				}
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}

	for _, cmd := range cmds {
		if code := exitCode(t, cmd, cmd.Wait()); code != 0 {
			t.Fatalf("helper exited with %d: %s", code, cmd.Stderr)
		}
	}

	raw, err := os.ReadFile(filepath.Join(work, "counter"))
	if err != nil {
		t.Fatal(err)
	}

	if want := strconv.Itoa((processes + goroutines) * iterations); string(raw) != want {
		t.Fatalf("counter is %s, want %s: increments were lost, so holders overlapped", raw, want)
	}
}

func TestLockBlocksOtherProcess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	c := openCacheAt(t, dir)

	release, err := c.Lock(t.Context(), "busy")
	if err != nil {
		t.Fatal(err)
	}

	blocked := helperCommand(t, "try", dir, "busy", "200ms")
	if code := exitCode(t, blocked, blocked.Run()); code != exitHelperTimeout {
		t.Fatalf("child acquired a held lock (exit %d): %s", code, blocked.Stderr)
	}

	release()

	free := helperCommand(t, "try", dir, "busy", "10s")
	if code := exitCode(t, free, free.Run()); code != 0 {
		t.Fatalf("child could not acquire a released lock (exit %d): %s", code, free.Stderr)
	}
}

func TestLockReleasedWhenHolderDies(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	c := openCacheAt(t, dir)

	holder := helperCommand(t, "hold", dir, "crash")
	startHelper(t, holder, "locked")

	short, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	if _, err := c.Lock(short, "crash"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock held by a live process was acquired: %v", err)
	}

	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}

	_ = holder.Wait()

	long, cancelLong := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelLong()

	release, err := c.Lock(long, "crash")
	if err != nil {
		t.Fatalf("lock of a killed process was not released: %v", err)
	}

	release()
}

func TestWriteMaterializedAcrossProcesses(t *testing.T) {
	garbage := []byte("garbage")

	tests := []struct {
		name    string
		corrupt bool
	}{
		{name: "fresh entry"},
		{name: "corrupt entry", corrupt: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "cache")
			c := openCacheAt(t, dir)
			content := bytes.Repeat([]byte(`{"property":"value"},`), 300)
			contentDigest := digest.FromBytes(content)
			size := int64(len(content))
			path := c.SchemaPath(testManifest)

			contentFile := filepath.Join(t.TempDir(), "content.json")
			if err := os.WriteFile(contentFile, content, 0o600); err != nil {
				t.Fatal(err)
			}

			accepted := [][]byte{content}
			if tt.corrupt {
				plantFile(t, path, garbage)
				accepted = append(accepted, garbage)
			}

			// Children start slowly, especially under -race, so every writer
			// waits for all of them to be ready; otherwise the parent would
			// finish before any child writes and nothing would race.
			cmds := make([]*exec.Cmd, 0, 3)
			gates := make([]io.WriteCloser, 0, 3)

			for range 3 {
				cmd := helperCommand(t, "materialize", dir, contentFile, testManifest)
				gates = append(gates, startHelper(t, cmd, "ready"))
				cmds = append(cmds, cmd)
			}

			done := make(chan struct{})
			readerErrs := make(chan error, 3)

			var readers sync.WaitGroup
			for range 3 {
				readers.Go(func() { readerErrs <- watchForPartialReads(path, done, accepted...) })
			}

			start := make(chan struct{})
			errs := make(chan error, 4)

			var writers sync.WaitGroup
			for range 4 {
				writers.Go(func() {
					<-start

					_, err := c.WriteMaterialized(testManifest, contentDigest, size, writeChunks(content, 256, time.Millisecond))
					errs <- err
				})
			}

			for _, gate := range gates {
				_ = gate.Close()
			}

			close(start)
			writers.Wait()

			for _, cmd := range cmds {
				if code := exitCode(t, cmd, cmd.Wait()); code != 0 {
					t.Fatalf("helper exited with %d: %s", code, cmd.Stderr)
				}
			}

			close(done)
			readers.Wait()
			close(errs)
			close(readerErrs)

			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}

			for err := range readerErrs {
				if err != nil {
					t.Fatal(err)
				}
			}

			if err := c.VerifyMaterialized(testManifest, contentDigest, size); err != nil {
				t.Fatal(err)
			}

			assertReadOnly(t, path)
			assertNoTmp(t, c)

			if tt.corrupt {
				assertQuarantined(t, c, garbage, 1, content)
			} else {
				assertEmptyDir(t, filepath.Join(dir, "v1", "quarantine"))
			}
		})
	}
}

// syncBuffer collects a child's stderr; exec copies into it from its own
// goroutine while the test may format it.
type syncBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}
