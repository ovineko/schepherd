//go:build unix && !aix

package runner

import "golang.org/x/sys/unix"

const atEaccess = unix.AT_EACCESS
