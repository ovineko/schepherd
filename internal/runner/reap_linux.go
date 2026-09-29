package runner

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// awaitExit blocks until p has exited without reaping it. While the zombie
// exists its PID, and therefore the process group ID, cannot be reused, so
// the group can be killed afterwards without hitting an unrelated group.
func awaitExit(p *os.Process) bool {
	var info unix.Siginfo

	for {
		err := unix.Waitid(unix.P_PID, p.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err == nil {
			return true
		}

		if !errors.Is(err, unix.EINTR) {
			return false
		}
	}
}
