//go:build !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || windows)

package cache

import (
	"errors"
	"fmt"
	"os"
)

func tryLockFile(f *os.File) (bool, error) {
	return false, fmt.Errorf("lock %s: %w", f.Name(), errors.ErrUnsupported)
}

func unlockFile(*os.File) error {
	return nil
}
