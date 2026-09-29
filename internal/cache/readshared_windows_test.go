//go:build windows

package cache

import (
	"io"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// readShared reads a file the way the cache opens its own files (os.Root on
// Windows shares delete access), so a concurrent reader in a test does not
// itself block the renames it watches.
func readShared(path string) ([]byte, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}

	h, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}

	f := os.NewFile(uintptr(h), path)
	defer func() { _ = f.Close() }()

	return io.ReadAll(f)
}
