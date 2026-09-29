//go:build unix

package runner

import (
	"os"
	"os/exec"
	"syscall"
)

// processGroup is the consumer's own process group. The consumer becomes its
// leader, so every descendant that does not create a new session can be
// signalled at once through the negative group ID.
type processGroup struct {
	pgid int
}

func newProcessGroup() *processGroup {
	return &processGroup{}
}

func (g *processGroup) configure(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func (g *processGroup) attach(p *os.Process) error {
	g.pgid = p.Pid

	return nil
}

func (g *processGroup) terminate() {
	g.signal(syscall.SIGTERM)
}

func (g *processGroup) kill() {
	g.signal(syscall.SIGKILL)
}

func (g *processGroup) release() {}

func (g *processGroup) signal(sig syscall.Signal) {
	if g.pgid > 0 {
		_ = syscall.Kill(-g.pgid, sig)
	}
}

func signalExitCode(state *os.ProcessState) (int, bool) {
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return 0, false
	}

	return 128 + int(status.Signal()), true
}
