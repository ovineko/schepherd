package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

func buildHelper(t *testing.T) (dir, bin string) {
	t.Helper()

	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go toolchain not found: %v", err)
	}

	name := "docker-credential-e2e"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}

	dir = t.TempDir()
	bin = filepath.Join(dir, name)

	out, err := exec.Command(goBin, "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	return dir, bin
}

func TestBinaryNotFoundContract(t *testing.T) {
	_, bin := buildHelper(t)
	cfg := newConfig(t, map[string]entry{})

	cmd := exec.Command(bin, "get")
	cmd.Env = []string{keyDB + "=" + cfg.dbPath, keyLog + "=" + cfg.logPath}
	cmd.Stdin = strings.NewReader(testServer)

	out, err := cmd.Output()

	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("result %v, want exit status 1", err)
	}

	if string(out) != "credentials not found in native keychain\n" {
		t.Fatalf("stdout %q", out)
	}
}

func TestBinaryConcurrentLog(t *testing.T) {
	_, bin := buildHelper(t)
	cfg := newConfig(t, map[string]entry{testServer: {Username: "u", Secret: testSecret}})

	const n = 16

	var wg sync.WaitGroup

	errs := make(chan error, n)

	for range n {
		wg.Go(func() {
			cmd := exec.Command(bin, "get")
			cmd.Env = []string{keyDB + "=" + cfg.dbPath, keyLog + "=" + cfg.logPath}
			cmd.Stdin = strings.NewReader(testServer)

			if out, err := cmd.Output(); err != nil || !bytes.Contains(out, []byte(testSecret)) {
				errs <- errors.Join(err, errors.New("unexpected output "+string(out)))
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}

	lines := readLog(t, cfg.logPath)
	if len(lines) != n {
		t.Fatalf("got %d log lines, want %d", len(lines), n)
	}

	for _, line := range lines {
		if line != (logLine{Op: "get", Server: testServer}) {
			t.Fatalf("log line %+v", line)
		}
	}
}

// TestORASNativeStore drives the helper through the credential store the
// client uses, so a protocol mismatch shows up here rather than in e2e runs.
func TestORASNativeStore(t *testing.T) {
	dir, _ := buildHelper(t)
	cfg := newConfig(t, map[string]entry{testServer: {Username: "tester", Secret: testSecret}})

	t.Setenv("PATH", dir)
	t.Setenv(keyDB, cfg.dbPath)
	t.Setenv(keyLog, cfg.logPath)

	ctx := context.Background()
	store := credentials.NewNativeStore("e2e")

	if cred, err := store.Get(ctx, testServer); err != nil || cred != (auth.Credential{Username: "tester", Password: testSecret}) {
		t.Fatalf("get: %+v, %v", cred, err)
	}

	if cred, err := store.Get(ctx, "registry.test"); err != nil || cred != auth.EmptyCredential {
		t.Fatalf("get of an unknown server: %+v, %v", cred, err)
	}

	if err := store.Put(ctx, testServer, auth.Credential{Username: "u", Password: "p"}); err == nil {
		t.Fatal("store must fail: the helper only answers get")
	}

	if err := os.WriteFile(cfg.dbPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Get(ctx, testServer); err == nil {
		t.Fatal("a broken helper database must surface as an error, not as missing credentials")
	}

	lines := readLog(t, cfg.logPath)

	ops := make([]string, 0, len(lines))
	for _, line := range lines {
		ops = append(ops, line.Op+" "+line.Server)
	}

	want := []string{"get " + testServer, "get registry.test", "store ", "get " + testServer}
	if strings.Join(ops, ",") != strings.Join(want, ",") {
		t.Fatalf("log %q, want %q", ops, want)
	}
}
