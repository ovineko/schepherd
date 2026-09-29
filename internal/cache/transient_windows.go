//go:build windows

package cache

import (
	"errors"

	"golang.org/x/sys/windows"
)

// transient reports errors that Windows returns while another process holds
// the file open without FILE_SHARE_DELETE (a validator reading a schema, for
// example) or while a concurrent rename or delete of it is pending. They
// clear once that handle closes; the go command retries the same errors
// (cmd/go/internal/robustio).
func transient(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
