//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package cache

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func tryLockFile(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)

	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.EWOULDBLOCK), errors.Is(err, unix.EINTR):
		return false, nil
	default:
		return false, fmt.Errorf("flock %s: %w", f.Name(), err)
	}
}

func unlockFile(f *os.File) error {
	if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil {
		return fmt.Errorf("unlock %s: %w", f.Name(), err)
	}

	return nil
}
