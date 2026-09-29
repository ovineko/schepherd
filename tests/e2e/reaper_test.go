//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// The reaper removes the compose project when the test process dies without
// running its teardown: a panic from "go test -timeout", SIGKILL, or a crash.
// It is the test binary itself, re-executed in its own session (so a Ctrl-C
// or a closing terminal does not reach it), reading a pipe whose only writer
// is the test process. The pipe ends when the test process ends, however it
// ends; a clean teardown writes reaperDone first.
//
// A docker command the dead process started keeps running as an orphan: a
// "compose up" that is still pulling would create the network and the
// container after "down" finished. So the reaper first stops every process
// that carries the run's envOwner mark, and only then removes the project.

const (
	reaperDone = "done"
	// reaperRecheck is written by the signal teardown, which removed the
	// project while test goroutines could still start docker commands. The
	// reaper stops those once the test process is gone and removes whatever
	// they created, reporting only when it had something to do.
	reaperRecheck = "recheck"

	ownedStopTimeout = 30 * time.Second
)

type reaperConfig struct {
	Project     string   `json:"project"`
	ComposeFile string   `json:"composeFile"`
	ComposeVars []string `json:"composeVars"`
	Artifacts   string   `json:"artifacts"`
	Work        string   `json:"work"`
}

type reaper struct {
	pipe *os.File
	cmd  *exec.Cmd
	once sync.Once
}

func startReaper(cfg reaperConfig) (*reaper, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("reaper: locate test binary: %w", err)
	}

	job, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("reaper: encode job: %w", err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("reaper: pipe: %w", err)
	}

	cmd := exec.CommandContext(context.Background(), exe, "-test.run=^$")
	cmd.Env = hostEnviron(envReaper + "=" + string(job))
	cmd.Stdin = r
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()

		return nil, fmt.Errorf("reaper: start: %w", err)
	}

	_ = r.Close()

	return &reaper{pipe: w, cmd: cmd}, nil
}

// release tells the reaper that teardown completed and lets it exit.
func (r *reaper) release() {
	r.once.Do(func() {
		_, _ = io.WriteString(r.pipe, reaperDone)
		_ = r.pipe.Close()

		done := make(chan struct{})

		go func() {
			_ = r.cmd.Wait()

			close(done)
		}()

		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	})
}

// handOver asks the reaper for a second pass after the test process exits.
// The pipe stays open, so the reaper starts only when this process is gone
// and can no longer start docker commands.
func (r *reaper) handOver() {
	r.once.Do(func() {
		_, _ = io.WriteString(r.pipe, reaperRecheck)
	})
}

func runReaper(job string) int {
	var cfg reaperConfig
	if err := json.Unmarshal([]byte(job), &cfg); err != nil {
		return 2
	}

	data, _ := io.ReadAll(os.Stdin)

	switch string(data) {
	case reaperDone:
		return 0
	case reaperRecheck:
		recheckProject(cfg)

		return 0
	}

	var report bytes.Buffer

	fmt.Fprintf(&report, "reaper: test process of project %s ended without teardown; removing the project\n", cfg.Project)

	if err := stopOwnedProcesses(cfg.Project); err != nil {
		fmt.Fprintln(&report, err)
	}

	c := &compose{project: cfg.Project, file: cfg.ComposeFile, env: hostEnviron(cfg.ComposeVars...)}

	ctx, cancel := context.WithTimeout(context.Background(), dockerCmdTimeout)
	logs, _ := c.run(ctx, "--profile", profileAuth, "--profile", profileNpm, "logs", "--no-color", "--timestamps")

	cancel()

	if err := removeProject(c, cfg.Artifacts); err != nil {
		fmt.Fprintln(&report, err)
	} else {
		fmt.Fprintln(&report, "reaper: project removed")
	}

	if err := os.RemoveAll(cfg.Work); err != nil {
		fmt.Fprintln(&report, "reaper: remove work directory:", err)
	}

	if err := os.MkdirAll(cfg.Artifacts, 0o755); err == nil {
		_ = os.WriteFile(filepath.Join(cfg.Artifacts, "compose.log"), logs, 0o644)
		_ = os.WriteFile(filepath.Join(cfg.Artifacts, "reaper.log"), report.Bytes(), 0o644)
	}

	return 0
}

func recheckProject(cfg reaperConfig) {
	var errs bytes.Buffer

	if err := stopOwnedProcesses(cfg.Project); err != nil {
		fmt.Fprintln(&errs, err)
	}

	c := &compose{project: cfg.Project, file: cfg.ComposeFile, env: hostEnviron(cfg.ComposeVars...)}
	if err := removeProject(c, cfg.Artifacts); err != nil {
		fmt.Fprintln(&errs, err)
	}

	if err := os.RemoveAll(cfg.Work); err != nil {
		fmt.Fprintln(&errs, "reaper: remove work directory:", err)
	}

	if errs.Len() > 0 {
		if err := os.MkdirAll(cfg.Artifacts, 0o755); err == nil {
			report := "reaper: second pass after the signal teardown of project " + cfg.Project + "\n" + errs.String()
			_ = os.WriteFile(filepath.Join(cfg.Artifacts, "reaper.log"), []byte(report), 0o644)
		}
	}
}

// stopOwnedProcesses kills every process that carries the project's
// envOwner mark (the docker CLI and the compose plugin it executes) and
// waits until none is left, so that no docker command of the run can create
// anything after the project is removed. Only processes of the current user
// are visible, and the mark holds the unique project name, so nothing of
// another run or another program is touched.
func stopOwnedProcesses(project string) error {
	err := poll(context.Background(), ownedStopTimeout, func() error {
		pids := ownedProcesses(project)
		if len(pids) == 0 {
			return nil
		}

		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}

		return fmt.Errorf("docker processes %v of project %s are still running", pids, project)
	})
	if err != nil {
		return fmt.Errorf("stop docker processes: %w", err)
	}

	return nil
}

// ownedProcesses lists the processes whose initial environment contains the
// project's envOwner entry. A zombie has an empty environment and does not
// count: it can no longer do anything.
func ownedProcesses(project string) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}

	mark := []byte(ownerEntry(project))
	self := os.Getpid()

	var pids []int

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}

		environ, err := os.ReadFile("/proc/" + e.Name() + "/environ")
		if err != nil {
			continue
		}

		if slices.ContainsFunc(bytes.Split(environ, []byte{0}), func(kv []byte) bool { return bytes.Equal(kv, mark) }) {
			pids = append(pids, pid)
		}
	}

	return pids
}
