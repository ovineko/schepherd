// Command credhelper is a Docker credential helper for the end-to-end suite,
// which installs it as docker-credential-e2e. It answers only `get` of the
// docker-credential-helpers protocol, the one operation Schepherd uses,
// from a JSON file named by E2E_CREDHELPER_DB that maps a server URL to
// {"Username","Secret"}; any other operation fails. Every invocation appends
// one {"op","server"} JSON line to the file named by E2E_CREDHELPER_LOG, so
// tests can assert when credentials were requested. Secrets are never logged.
//
// The environment is read only by settings: this helper deliberately does not
// import the product's internal/env package, so the suite never depends on the
// code it verifies.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	keyDB  = "E2E_CREDHELPER_DB"
	keyLog = "E2E_CREDHELPER_LOG"
)

// errNotFound carries the exact text clients match to tell "no credentials"
// apart from a helper failure.
var errNotFound = errors.New("credentials not found in native keychain")

var errNoServer = errors.New("no credentials server URL")

type config struct {
	dbPath  string
	logPath string
}

type serverCredentials struct {
	ServerURL string `json:"ServerURL"`
	Username  string `json:"Username"`
	Secret    string `json:"Secret"`
}

type entry struct {
	Username string `json:"Username"`
	Secret   string `json:"Secret"`
}

type logLine struct {
	Op     string `json:"op"`
	Server string `json:"server"`
}

// settings is the package's only access to the process environment.
func settings() config {
	return config{dbPath: os.Getenv(keyDB), logPath: os.Getenv(keyLog)}
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, settings()); err != nil {
		// The protocol reports failures on stdout, which is what clients read.
		_, _ = fmt.Fprintln(os.Stdout, err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer, cfg config) error {
	op := strings.Join(args, " ")

	var (
		server   string
		inputErr error
	)

	if op == "get" {
		server, inputErr = readServer(stdin)
	}

	if err := cfg.log(op, server); err != nil {
		return err
	}

	if op != "get" {
		return fmt.Errorf("unsupported operation %q: docker-credential-e2e only answers get", op)
	}

	if inputErr != nil {
		return inputErr
	}

	db, err := cfg.load()
	if err != nil {
		return err
	}

	found, ok := db[server]
	if !ok {
		return errNotFound
	}

	answer := serverCredentials{ServerURL: server, Username: found.Username, Secret: found.Secret}
	if err := json.NewEncoder(stdout).Encode(answer); err != nil { //nolint:gosec // G117: returning the secret is what get does; the helper only holds test fixtures
		return fmt.Errorf("write response: %w", err)
	}

	return nil
}

func readServer(stdin io.Reader) (string, error) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("read server URL: %w", err)
	}

	server := strings.TrimSpace(string(data))
	if server == "" {
		return "", errNoServer
	}

	return server, nil
}

// log appends with a single write so concurrent invocations never interleave
// within a line.
func (c config) log(op, server string) error {
	if c.logPath == "" {
		return nil
	}

	line, err := json.Marshal(logLine{Op: op, Server: server})
	if err != nil {
		return fmt.Errorf("encode log line: %w", err)
	}

	f, err := os.OpenFile(c.logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}

	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()

		return fmt.Errorf("append log: %w", err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close log: %w", err)
	}

	return nil
}

// load treats an unset variable or a missing file as an empty database, the
// same as a keychain without entries.
func (c config) load() (map[string]entry, error) {
	var db map[string]entry
	if c.dbPath == "" {
		return db, nil
	}

	data, err := os.ReadFile(c.dbPath)
	if errors.Is(err, os.ErrNotExist) {
		return db, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read credential database: %w", err)
	}

	if err := json.Unmarshal(data, &db); err != nil {
		return nil, fmt.Errorf("decode credential database: %w", err)
	}

	return db, nil
}
