//go:build !linux

package runner

import "os"

// awaitExit cannot wait without reaping on this platform; the group is then
// killed right after the consumer is reaped.
func awaitExit(*os.Process) bool {
	return false
}
