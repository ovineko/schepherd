package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type invocation struct {
	args  []string
	env   []string
	stdin *os.File
	input []byte
	dir   string
}

type outcome struct {
	pid    int
	code   int
	stdout string
	stderr string
}

func buildConsumer(t *testing.T) string {
	t.Helper()

	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go toolchain not found: %v", err)
	}

	name := "testconsumer"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}

	bin := filepath.Join(t.TempDir(), name)

	out, err := exec.Command(goBin, "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	return bin
}

func runConsumer(t *testing.T, bin string, inv invocation) outcome {
	t.Helper()

	cmd := exec.Command(bin, inv.args...)
	cmd.Env = inv.env
	cmd.Dir = inv.dir

	switch {
	case inv.stdin != nil:
		cmd.Stdin = inv.stdin
	case inv.input != nil:
		cmd.Stdin = bytes.NewReader(inv.input)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	code := 0
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run consumer: %v", err)
	}

	return outcome{pid: cmd.Process.Pid, code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func readRecords(t *testing.T, dir string) map[string]record {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	out := map[string]record{}

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}

		var rec record
		if err := json.Unmarshal(data, &rec); err != nil {
			t.Fatalf("decode %s: %v", entry.Name(), err)
		}

		out[entry.Name()] = rec
	}

	return out
}

func onlyRecord(t *testing.T, dir string) (string, record) {
	t.Helper()

	records := readRecords(t, dir)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}

	for name, rec := range records {
		return name, rec
	}

	panic("unreachable")
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func TestConsumerRecord(t *testing.T) {
	bin := buildConsumer(t)
	logDir := t.TempDir()
	work := t.TempDir()

	content := []byte(`{"name": "demo"}`)
	docPath := filepath.Join(work, "doc.json")

	if err := os.WriteFile(docPath, content, 0o600); err != nil {
		t.Fatal(err)
	}

	args := []string{"--schemafile", docPath, filepath.Join(work, "missing.json"), work, "relative.json"}

	if err := os.WriteFile(filepath.Join(work, "relative.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	env := []string{
		"TC_LOG_DIR=" + logDir,
		"TC_ROLE=validator",
		"TC_STDOUT=out line\n",
		"TC_STDERR=err line\n",
		"SCHEMA_ID=package",
		"PATH=/nonexistent/bin",
		"NO_COLOR=1",
		"HOME=/nonexistent/home",
		"SECRET_TOKEN=do-not-record",
	}

	got := runConsumer(t, bin, invocation{args: args, env: env, dir: work})

	if got.code != 0 {
		t.Fatalf("exit code %d, stderr %q", got.code, got.stderr)
	}

	if got.stdout != "out line\n" || got.stderr != "err line\n" {
		t.Fatalf("stdout %q stderr %q", got.stdout, got.stderr)
	}

	if entries, err := os.ReadDir(logDir); err != nil || len(entries) != 1 {
		t.Fatalf("log dir must hold exactly the published record, got %v (%v)", entries, err)
	}

	name, rec := onlyRecord(t, logDir)

	if !regexp.MustCompile(`^[0-9]+-` + strconv.Itoa(got.pid) + `\.json$`).MatchString(name) {
		t.Fatalf("record name %q does not follow <unix-nano>-<pid>.json", name)
	}

	if rec.PID != got.pid || rec.PPID != os.Getpid() {
		t.Fatalf("pid %d ppid %d, want %d and %d", rec.PID, rec.PPID, got.pid, os.Getpid())
	}

	if want := append([]string{bin}, args...); !slices.Equal(rec.Argv, want) {
		t.Fatalf("argv %q, want %q", rec.Argv, want)
	}

	wantCwd, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}

	if gotCwd, err := filepath.EvalSymlinks(rec.Cwd); err != nil || gotCwd != wantCwd {
		t.Fatalf("cwd %q, want %q", rec.Cwd, wantCwd)
	}

	wantEnv := map[string]string{
		"TC_LOG_DIR": logDir,
		"TC_ROLE":    "validator",
		"TC_STDOUT":  "out line\n",
		"TC_STDERR":  "err line\n",
		"SCHEMA_ID":  "package",
		"PATH":       "/nonexistent/bin",
		"NO_COLOR":   "1",
	}
	if len(rec.Env) != len(wantEnv) {
		t.Fatalf("env %v, want %v", rec.Env, wantEnv)
	}

	for key, value := range wantEnv {
		if rec.Env[key] != value {
			t.Fatalf("env[%s] = %q, want %q", key, rec.Env[key], value)
		}
	}

	if rec.Role != "validator" || rec.Stdin != nil || !rec.StdinIsDevNull || rec.Grandchild != 0 {
		t.Fatalf("role %q stdin %v stdinIsDevNull %v grandchild %d", rec.Role, rec.Stdin, rec.StdinIsDevNull, rec.Grandchild)
	}

	wantFiles := map[string]argFile{
		docPath:         {Size: int64(len(content)), SHA256: sha256Hex(content)},
		"relative.json": {Size: 0, SHA256: sha256Hex(nil)},
	}
	if len(rec.ArgFiles) != len(wantFiles) {
		t.Fatalf("argFiles %v, want %v", rec.ArgFiles, wantFiles)
	}

	for path, want := range wantFiles {
		if rec.ArgFiles[path] != want {
			t.Fatalf("argFiles[%s] = %+v, want %+v", path, rec.ArgFiles[path], want)
		}
	}
}

func TestConsumerStdin(t *testing.T) {
	bin := buildConsumer(t)

	binary := []byte{0x00, 0xff, 0xfe, '\n', 'a', 0x80, '\r', '\n'}

	emptyFile, err := os.Create(filepath.Join(t.TempDir(), "empty"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = emptyFile.Close() })

	fullPath := filepath.Join(t.TempDir(), "full")
	if err := os.WriteFile(fullPath, binary, 0o600); err != nil {
		t.Fatal(err)
	}

	fullFile, err := os.Open(fullPath)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = fullFile.Close() })

	tests := []struct {
		name        string
		inv         invocation
		read        bool
		wantStdin   []byte
		wantDevNull bool
	}{
		{name: "null device", wantDevNull: true},
		{name: "null device read", read: true, wantStdin: []byte{}, wantDevNull: true},
		{name: "pipe not read", inv: invocation{input: binary}},
		{name: "pipe with binary bytes", inv: invocation{input: binary}, read: true, wantStdin: binary},
		{name: "empty pipe", inv: invocation{input: []byte{}}, read: true, wantStdin: []byte{}},
		{name: "empty regular file", inv: invocation{stdin: emptyFile}, read: true, wantStdin: []byte{}, wantDevNull: true},
		{name: "regular file", inv: invocation{stdin: fullFile}, read: true, wantStdin: binary},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logDir := t.TempDir()

			inv := tc.inv
			inv.env = []string{"TC_LOG_DIR=" + logDir}

			if tc.read {
				inv.env = append(inv.env, "TC_READ_STDIN=1")
			}

			if got := runConsumer(t, bin, inv); got.code != 0 {
				t.Fatalf("exit code %d, stderr %q", got.code, got.stderr)
			}

			_, rec := onlyRecord(t, logDir)

			if rec.StdinIsDevNull != tc.wantDevNull {
				t.Fatalf("stdinIsDevNull = %v, want %v", rec.StdinIsDevNull, tc.wantDevNull)
			}

			if !tc.read {
				if rec.Stdin != nil {
					t.Fatalf("stdin recorded without TC_READ_STDIN: %q", *rec.Stdin)
				}

				return
			}

			if rec.Stdin == nil {
				t.Fatal("stdin not recorded")
			}

			decoded, err := base64.StdEncoding.DecodeString(*rec.Stdin)
			if err != nil {
				t.Fatal(err)
			}

			if !bytes.Equal(decoded, tc.wantStdin) {
				t.Fatalf("stdin %q, want %q", decoded, tc.wantStdin)
			}
		})
	}
}

func TestConsumerExitStatus(t *testing.T) {
	t.Parallel()

	bin := buildConsumer(t)

	tests := []struct {
		name       string
		env        []string
		args       []string
		noLogDir   bool
		wantCode   int
		wantRecord bool
	}{
		{name: "default", wantCode: 0, wantRecord: true},
		{name: "configured", env: []string{"TC_EXIT=3"}, wantCode: 3, wantRecord: true},
		{name: "maximum", env: []string{"TC_EXIT=255"}, wantCode: 255, wantRecord: true},
		{
			name:     "argument match",
			env:      []string{"TC_EXIT_IF_ARG_CONTAINS=invalid", "TC_EXIT=4"},
			args:     []string{"ok.json", "/work/invalid-doc.json"},
			wantCode: 1, wantRecord: true,
		},
		{
			name:     "argument mismatch",
			env:      []string{"TC_EXIT_IF_ARG_CONTAINS=invalid", "TC_EXIT=4"},
			args:     []string{"ok.json"},
			wantCode: 4, wantRecord: true,
		},
		{name: "missing log dir", noLogDir: true, wantCode: exitHarnessError},
		{name: "non-numeric exit", env: []string{"TC_EXIT=x"}, wantCode: exitHarnessError},
		{name: "exit out of range", env: []string{"TC_EXIT=256"}, wantCode: exitHarnessError},
		{name: "negative exit", env: []string{"TC_EXIT=-1"}, wantCode: exitHarnessError},
		{name: "bad boolean", env: []string{"TC_HANG=sometimes"}, wantCode: exitHarnessError},
		{name: "false boolean", env: []string{"TC_HANG=0", "TC_SPAWN_GRANDCHILD=false"}, wantCode: 0, wantRecord: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			logDir := t.TempDir()

			env := slices.Clone(tc.env)
			if !tc.noLogDir {
				env = append(env, "TC_LOG_DIR="+logDir)
			}

			got := runConsumer(t, bin, invocation{args: tc.args, env: env})
			if got.code != tc.wantCode {
				t.Fatalf("exit code %d, want %d (stderr %q)", got.code, tc.wantCode, got.stderr)
			}

			if records := readRecords(t, logDir); (len(records) == 1) != tc.wantRecord || len(records) > 1 {
				t.Fatalf("got %d records, want record=%v", len(records), tc.wantRecord)
			}

			if tc.wantCode == exitHarnessError && !strings.HasPrefix(got.stderr, "testconsumer: ") {
				t.Fatalf("stderr %q does not explain the failure", got.stderr)
			}
		})
	}
}

func TestConsumerHang(t *testing.T) {
	bin := buildConsumer(t)
	logDir := t.TempDir()

	cmd := exec.Command(bin, "doc.json")
	cmd.Env = []string{"TC_LOG_DIR=" + logDir, "TC_HANG=1", "TC_EXIT=0"}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { done <- cmd.Wait() }()

	reaped := false

	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Kill()
			<-done
		}
	})

	waitForRecords(t, logDir, 1)

	select {
	case err := <-done:
		t.Fatalf("consumer exited while it should hang: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}

	err := <-done
	reaped = true

	if err == nil {
		t.Fatal("killed consumer reported success")
	}
}

func waitForRecords(t *testing.T, dir string, n int) map[string]record {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}

		count := 0

		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".json") {
				count++
			}
		}

		if count >= n {
			return readRecords(t, dir)
		}

		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d records, have %d", n, count)
		}

		time.Sleep(20 * time.Millisecond)
	}
}
