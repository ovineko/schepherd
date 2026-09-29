//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestConsumerGrandchild(t *testing.T) {
	bin := buildConsumer(t)

	t.Run("parent exits", func(t *testing.T) {
		logDir := t.TempDir()

		stdout, err := os.Create(t.TempDir() + "/stdout")
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = stdout.Close() })

		cmd := exec.Command(bin, "doc.json")
		cmd.Env = []string{
			"TC_LOG_DIR=" + logDir, "TC_SPAWN_GRANDCHILD=1", "TC_EXIT=5",
			"TC_ROLE=parent", "TC_STDOUT=once", "TC_READ_STDIN=1",
		}
		cmd.Stdout = stdout
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}

		group := cmd.Process.Pid
		t.Cleanup(func() { _ = syscall.Kill(-group, syscall.SIGKILL) })

		err = cmd.Wait()
		if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.ExitCode() != 5 {
			t.Fatalf("consumer result %v, want exit status 5", err)
		}

		parent, child := splitRoles(t, waitForRecords(t, logDir, 2))

		if parent.PID != group || parent.Grandchild == 0 || child.PID != parent.Grandchild {
			t.Fatalf("parent pid %d grandchild %d, grandchild record pid %d", parent.PID, parent.Grandchild, child.PID)
		}

		pgid, err := syscall.Getpgid(child.PID)
		if err != nil {
			t.Fatalf("grandchild is not running: %v", err)
		}

		if pgid != group {
			t.Fatalf("grandchild process group %d, want %d", pgid, group)
		}

		if child.Env["TC_HANG"] != "1" || child.Env["TC_ROLE"] != grandchildRole || child.Env["TC_LOG_DIR"] != logDir {
			t.Fatalf("grandchild env %v", child.Env)
		}

		for _, key := range []string{keySpawnGrandchild, keyStdout, keyReadStdin, keyExit} {
			if _, ok := child.Env[key]; ok {
				t.Fatalf("grandchild inherited %s", key)
			}
		}

		if child.Stdin != nil || !child.StdinIsDevNull {
			t.Fatalf("grandchild stdin %v devnull %v", child.Stdin, child.StdinIsDevNull)
		}

		if err := stdout.Sync(); err != nil {
			t.Fatal(err)
		}

		if data, err := os.ReadFile(stdout.Name()); err != nil || string(data) != "once" {
			t.Fatalf("stdout %q (%v), want the parent's text exactly once", data, err)
		}
	})

	t.Run("parent hangs", func(t *testing.T) {
		logDir := t.TempDir()

		cmd := exec.Command(bin)
		cmd.Env = []string{"TC_LOG_DIR=" + logDir, "TC_SPAWN_GRANDCHILD=1", "TC_HANG=1"}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}

		group := cmd.Process.Pid
		done := make(chan error, 1)

		go func() { done <- cmd.Wait() }()

		t.Cleanup(func() { _ = syscall.Kill(-group, syscall.SIGKILL) })

		parent, child := splitRoles(t, waitForRecords(t, logDir, 2))
		if parent.Grandchild != child.PID || child.PPID != parent.PID {
			t.Fatalf("parent %d recorded grandchild %d; grandchild record pid %d ppid %d",
				parent.PID, parent.Grandchild, child.PID, child.PPID)
		}

		select {
		case err := <-done:
			t.Fatalf("hanging parent exited: %v", err)
		case <-time.After(200 * time.Millisecond):
		}

		if err := syscall.Kill(-group, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}

		select {
		case err := <-done:
			if err == nil {
				t.Fatal("killed parent reported success")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("parent survived killing its process group")
		}
	})
}

func splitRoles(t *testing.T, records map[string]record) (parent, child record) {
	t.Helper()

	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}

	var haveParent, haveChild bool

	for _, rec := range records {
		if rec.Role == grandchildRole {
			child, haveChild = rec, true
		} else {
			parent, haveParent = rec, true
		}
	}

	if !haveParent || !haveChild {
		t.Fatalf("records do not contain one parent and one grandchild: %+v", records)
	}

	return parent, child
}

func TestConsumerSkipsFIFOArgument(t *testing.T) {
	bin := buildConsumer(t)
	logDir := t.TempDir()
	fifo := t.TempDir() + "/pipe"

	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, fifo)
	cmd.Env = []string{"TC_LOG_DIR=" + logDir}
	cmd.WaitDelay = time.Second

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("consumer failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("consumer blocked on a FIFO argument")
	}

	_, rec := onlyRecord(t, logDir)
	if len(rec.ArgFiles) != 0 {
		t.Fatalf("FIFO recorded as a file: %v", rec.ArgFiles)
	}
}
