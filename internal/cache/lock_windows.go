//go:build windows

package cache

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func tryLockFile(f *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})

	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION), errors.Is(err, windows.ERROR_IO_PENDING):
		return false, nil
	default:
		return false, fmt.Errorf("lock %s: %w", f.Name(), err)
	}
}

func unlockFile(f *os.File) error {
	if err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{}); err != nil {
		return fmt.Errorf("unlock %s: %w", f.Name(), err)
	}

	return nil
}
