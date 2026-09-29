package runner

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// stillActive is the STILL_ACTIVE pseudo exit code of a running process.
const stillActive = 259

// processGroup is a job object that closes over the consumer and every
// process it starts. KILL_ON_JOB_CLOSE makes the job lethal even if
// Schepherd itself dies before it can terminate the job explicitly.
type processGroup struct {
	job windows.Handle
}

func newProcessGroup() *processGroup {
	return &processGroup{}
}

func (g *processGroup) configure(cmd *exec.Cmd) {
	// A new console process group keeps Ctrl+C away from the consumer, as a
	// separate process group does on Unix; Schepherd stops it via the job.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

func (g *processGroup) attach(p *os.Process) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create job object: %w", err)
	}

	g.job = job

	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE

	// bearer:disable go_gosec_unsafe_unsafe
	// Win32 takes the job limits by address; there is no alternative.
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil { //nolint:gosec // G103: no alternative to passing the struct address to Win32.
		return fmt.Errorf("configure job object: %w", err)
	}

	var assignErr error

	if err := p.WithHandle(func(handle uintptr) {
		assignErr = assign(job, windows.Handle(handle))
	}); err != nil {
		return fmt.Errorf("access consumer process: %w", err)
	}

	return assignErr
}

// assign tolerates a consumer that already exited before it could be
// assigned: there is nothing left to contain and its exit status is real.
func assign(job, process windows.Handle) error {
	err := windows.AssignProcessToJobObject(job, process)
	if err == nil {
		return nil
	}

	var code uint32
	if windows.GetExitCodeProcess(process, &code) == nil && code != stillActive {
		return nil
	}

	return fmt.Errorf("assign consumer to job object: %w", err)
}

func (g *processGroup) terminate() {
	g.kill()
}

func (g *processGroup) kill() {
	if g.job != 0 {
		_ = windows.TerminateJobObject(g.job, 1)
	}
}

func (g *processGroup) release() {
	if g.job != 0 {
		_ = windows.CloseHandle(g.job)
		g.job = 0
	}
}

func signalExitCode(*os.ProcessState) (int, bool) {
	return 0, false
}
