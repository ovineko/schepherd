//go:build windows

package linktest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// mountPointHeader is the size of the four name offsets and lengths that
// precede the path buffer of a mount point reparse buffer.
const mountPointHeader = 8

// Junction creates link as a directory junction (a mount point reparse
// point) to target. Unlike symlinks, junctions need no privilege, so any
// user who can write to a directory can plant one there; os.Lstat reports
// them as fs.ModeIrregular, not fs.ModeSymlink. target need not exist.
func Junction(target, link string) error {
	abs, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("junction target %s: %w", target, err)
	}

	substitute := utf16.Encode([]rune(`\??\` + abs))
	printName := utf16.Encode([]rune(abs))

	path := make([]uint16, 0, len(substitute)+len(printName)+2)
	path = append(path, substitute...)
	path = append(path, 0)
	path = append(path, printName...)
	path = append(path, 0)

	buf := binary.LittleEndian.AppendUint32(nil, windows.IO_REPARSE_TAG_MOUNT_POINT)

	// ReparseDataLength and Reserved, then offset and length in bytes of the
	// substitute name and of the print name, both without their NUL.
	for _, n := range []int{mountPointHeader + 2*len(path), 0, 0, 2 * len(substitute), 2 * (len(substitute) + 1), 2 * len(printName)} {
		if n < 0 || n > math.MaxUint16 {
			return fmt.Errorf("junction target %s is too long", abs)
		}

		buf = binary.LittleEndian.AppendUint16(buf, uint16(n))
	}

	for _, u := range path {
		buf = binary.LittleEndian.AppendUint16(buf, u)
	}

	size := len(buf)
	if size > windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE {
		return fmt.Errorf("junction target %s is too long", abs)
	}

	if err := os.Mkdir(link, 0o750); err != nil {
		return fmt.Errorf("junction %s: %w", link, err)
	}

	name, err := windows.UTF16PtrFromString(link)
	if err != nil {
		return fmt.Errorf("junction %s: %w", link, err)
	}

	h, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", link, err)
	}

	defer func() { _ = windows.CloseHandle(h) }()

	var returned uint32
	if err := windows.DeviceIoControl(h, windows.FSCTL_SET_REPARSE_POINT, &buf[0], uint32(size), nil, 0, &returned, nil); err != nil {
		return fmt.Errorf("set mount point on %s: %w", link, err)
	}

	return nil
}

// Symlink creates a symbolic link like os.Symlink. ok is false when the
// process lacks the privilege to create symlinks (no administrator rights
// and no developer mode), so the caller can skip instead of failing.
func Symlink(target, link string) (ok bool, err error) {
	err = os.Symlink(target, link)
	if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("symlink %s: %w", link, err)
	}

	return true, nil
}
