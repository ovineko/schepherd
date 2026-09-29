//go:build unix

package runner

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"syscall"
)

// processAlive treats zombies as dead: a killed grandchild stays a zombie
// until whichever process adopted it reaps it.
func processAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}

	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return !errors.Is(err, os.ErrNotExist) || !procMounted()
	}

	end := bytes.LastIndexByte(stat, ')')
	if end < 0 || end+2 >= len(stat) {
		return true
	}

	state := stat[end+2]

	return state != 'Z' && state != 'X'
}

func procMounted() bool {
	_, err := os.Stat("/proc/self/stat")

	return err == nil
}
