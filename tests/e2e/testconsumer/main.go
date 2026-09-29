// Command testconsumer is a configurable stand-in for a schema validator. The
// end-to-end suite configures it as the runner's consumer to observe exactly
// how schepherd starts consumers; it is never shipped.
//
// Every invocation writes one JSON record, atomically, to
// $TC_LOG_DIR/<unix-nano>-<pid>.json with the fields pid, ppid, role, argv,
// cwd, env (only TC_*, SCHEMA_*, PATH and NO_COLOR), stdin (base64, only with
// TC_READ_STDIN), stdinIsDevNull, argFiles (size and hex sha256 of every
// argument after argv[0] that names a regular file) and grandchild.
//
// Behavior is configured only through TC_* variables:
//
//	TC_LOG_DIR               required; directory that receives the records
//	TC_EXIT                  exit status, default 0
//	TC_EXIT_IF_ARG_CONTAINS  exit 1 when an argument after argv[0] contains it
//	TC_READ_STDIN            read all of stdin and record it
//	TC_HANG                  sleep until killed after writing the record
//	TC_SPAWN_GRANDCHILD      start a hanging copy (TC_ROLE=grandchild) in the
//	                         same process group, sharing stdout and stderr
//	TC_ROLE                  label copied into the record
//	TC_STDOUT, TC_STDERR     literal text written to stdout and stderr
//
// The environment is read only by environ: this helper deliberately does not
// import the product's internal/env package, so the suite never depends on the
// code it verifies.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// exitHarnessError is returned when the consumer itself cannot do its job,
// distinct from any status a test configures through TC_EXIT.
const exitHarnessError = 99

type argFile struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type record struct {
	PID            int                `json:"pid"`
	PPID           int                `json:"ppid"`
	Role           string             `json:"role"`
	Argv           []string           `json:"argv"`
	Cwd            string             `json:"cwd"`
	Env            map[string]string  `json:"env"`
	Stdin          *string            `json:"stdin,omitempty"`
	StdinIsDevNull bool               `json:"stdinIsDevNull"`
	ArgFiles       map[string]argFile `json:"argFiles"`
	Grandchild     int                `json:"grandchild,omitempty"`
}

func main() {
	os.Exit(run())
}

func run() int {
	vars := environ()

	cfg, err := parseConfig(vars)
	if err != nil {
		return fail(err)
	}

	rec, err := observe(cfg, vars)
	if err != nil {
		return fail(err)
	}

	if cfg.spawnGrandchild {
		rec.Grandchild, err = spawnGrandchild(vars)
		if err != nil {
			return fail(err)
		}
	}

	if err := writeRecord(cfg.logDir, rec); err != nil {
		return fail(err)
	}

	if _, err := io.WriteString(os.Stdout, cfg.stdout); err != nil {
		return fail(err)
	}

	if _, err := io.WriteString(os.Stderr, cfg.stderr); err != nil {
		return fail(err)
	}

	if cfg.hang {
		for {
			time.Sleep(time.Hour)
		}
	}

	return cfg.exitStatus(os.Args[1:])
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "testconsumer: %v\n", err)

	return exitHarnessError
}

func observe(cfg config, vars []string) (record, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return record{}, fmt.Errorf("working directory: %w", err)
	}

	rec := record{
		PID:            os.Getpid(),
		PPID:           os.Getppid(),
		Role:           cfg.role,
		Argv:           os.Args,
		Cwd:            cwd,
		Env:            recordedEnv(vars),
		StdinIsDevNull: stdinIsNull(os.Stdin),
		ArgFiles:       map[string]argFile{},
	}

	if cfg.readStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return record{}, fmt.Errorf("read stdin: %w", err)
		}

		encoded := base64.StdEncoding.EncodeToString(data)
		rec.Stdin = &encoded
	}

	for _, arg := range os.Args[1:] {
		file, ok, err := inspectArg(arg)
		if err != nil {
			return record{}, err
		}

		if ok {
			rec.ArgFiles[arg] = file
		}
	}

	return rec, nil
}

// stdinIsNull reports whether stdin cannot deliver any data: the null device,
// an empty regular file, or no stdin at all. Pipes and sockets are never
// treated as empty because their content is unknown until read.
func stdinIsNull(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return true
	}

	mode := info.Mode()

	switch {
	case mode&(os.ModeNamedPipe|os.ModeSocket) != 0:
		return false
	case mode.IsRegular():
		return info.Size() == 0
	case mode&os.ModeDevice != 0:
		null, err := os.Stat(os.DevNull)

		return err == nil && os.SameFile(info, null)
	default:
		return false
	}
}

// inspectArg reports ok=false for arguments that do not name a regular file.
// It stats before opening so a FIFO argument never blocks the consumer.
//
//nolint:gosec // reading the files named on the command line is the purpose of this fake consumer
func inspectArg(arg string) (argFile, bool, error) {
	info, err := os.Stat(arg)
	if err != nil || !info.Mode().IsRegular() {
		return argFile{}, false, nil
	}

	f, err := os.Open(arg)
	if err != nil {
		return argFile{}, false, fmt.Errorf("open argument file: %w", err)
	}

	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return argFile{}, false, fmt.Errorf("hash argument file: %w", err)
	}

	return argFile{Size: info.Size(), SHA256: hex.EncodeToString(h.Sum(nil))}, true, nil
}

func spawnGrandchild(vars []string) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("locate executable: %w", err)
	}

	cmd := exec.CommandContext(context.Background(), self, os.Args[1:]...) //nolint:gosec // re-executes this very binary
	cmd.Env = grandchildEnv(vars)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start grandchild: %w", err)
	}

	return cmd.Process.Pid, nil
}

// writeRecord publishes the record under its final name only once it is
// complete, so a test polling the directory never reads a partial file.
func writeRecord(dir string, rec record) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("encode record: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".testconsumer-*.tmp")
	if err != nil {
		return fmt.Errorf("create record: %w", err)
	}

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())

		return fmt.Errorf("write record: %w", err)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())

		return fmt.Errorf("close record: %w", err)
	}

	name := strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + strconv.Itoa(rec.PID) + ".json"
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmp.Name())

		return fmt.Errorf("publish record: %w", err)
	}

	return nil
}
