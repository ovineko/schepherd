//go:build linux

package bundle

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// limitResources caps the virtual memory of a started CLI process and turns
// off its core dumps, which would otherwise be as large as that memory. Go
// cannot give a child resource limits before it executes (SysProcAttr has no
// rlimit field, and changing the parent's limits would affect every
// goroutine), so the caps are applied right after the start; a runaway CLI
// needs far longer than that window to exhaust memory. Lower limits that are
// already in place are kept.
func limitResources(pid int, addressSpace uint64) error {
	for _, limit := range []struct {
		resource int
		value    uint64
	}{{unix.RLIMIT_AS, addressSpace}, {unix.RLIMIT_CORE, 0}} {
		var current unix.Rlimit

		if err := unix.Prlimit(pid, limit.resource, nil, &current); err != nil {
			if errors.Is(err, unix.ESRCH) {
				return nil
			}

			return fmt.Errorf("read JSON Schema CLI resource limit %d: %w", limit.resource, err)
		}

		next := unix.Rlimit{Cur: min(limit.value, current.Cur), Max: min(limit.value, current.Max)}

		if err := unix.Prlimit(pid, limit.resource, &next, nil); err != nil {
			if errors.Is(err, unix.ESRCH) {
				return nil
			}

			return fmt.Errorf("set JSON Schema CLI resource limit %d: %w", limit.resource, err)
		}
	}

	return nil
}
