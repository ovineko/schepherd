//go:build unix

package config

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestLocalSchemaReadRefusesUnreadableFilesAndFIFOs(t *testing.T) {
	cfg, dir := mustLoadDoc(t, "config_version = 1\n[schemas.locked]\npath = \"locked.json\"\n[schemas.fifo]\npath = \"fifo.json\"\n", nil)

	if err := syscall.Mkfifo(filepath.Join(dir, "fifo.json"), 0o600); err != nil {
		t.Fatal(err)
	}

	fifo, _ := cfg.LocalSchema("fifo")
	done := make(chan error, 1)

	go func() {
		_, err := fifo.Read(cfg.Limits.MaxSchemaBytes)
		done <- err
	}()

	select {
	case err := <-done:
		wantUsage(t, err, "schemas.fifo.path", "fifo.json is not a regular file")
	case <-time.After(10 * time.Second):
		t.Fatal("Read blocked on a FIFO")
	}

	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of their mode")
	}

	locked := filepath.Join(dir, "locked.json")
	writeFile(t, locked, "{}")

	if err := os.Chmod(locked, 0o200); err != nil {
		t.Fatal(err)
	}

	ls, _ := cfg.LocalSchema("locked")
	_, err := ls.Read(cfg.Limits.MaxSchemaBytes)
	wantUsage(t, err, "schemas.locked.path", "permission denied")
}
