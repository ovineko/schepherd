//go:build !windows

package cache

import "os"

// readShared reads a file; only Windows needs a special share mode.
func readShared(path string) ([]byte, error) {
	return os.ReadFile(path)
}
