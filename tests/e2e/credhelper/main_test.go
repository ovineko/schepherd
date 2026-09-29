package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	testServer = "127.0.0.1:5000"
	testSecret = "s3cr3t-value"
)

func newConfig(t *testing.T, db map[string]entry) config {
	t.Helper()

	dir := t.TempDir()
	cfg := config{dbPath: filepath.Join(dir, "db.json"), logPath: filepath.Join(dir, "log.jsonl")}

	if db != nil {
		data, err := json.Marshal(db)
		if err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(cfg.dbPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return cfg
}

func invoke(cfg config, input string, args ...string) (string, error) {
	var stdout bytes.Buffer

	err := run(args, strings.NewReader(input), &stdout, cfg)

	return stdout.String(), err
}

func readLog(t *testing.T, path string) []logLine {
	t.Helper()

	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = f.Close() }()

	var lines []logLine

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var line logLine

		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()

		if err := decoder.Decode(&line); err != nil {
			t.Fatalf("log line %q: %v", scanner.Text(), err)
		}

		lines = append(lines, line)
	}

	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	return lines
}

func TestGet(t *testing.T) {
	t.Parallel()

	db := map[string]entry{testServer: {Username: "tester", Secret: testSecret}}

	tests := []struct {
		name       string
		input      string
		unsetDB    bool
		noFile     bool
		null       bool
		wantErr    error
		wantServer string
	}{
		{name: "found", input: testServer, wantServer: testServer},
		{name: "trailing newline", input: testServer + "\n", wantServer: testServer},
		{name: "unknown server", input: "127.0.0.1:6000", wantErr: errNotFound, wantServer: "127.0.0.1:6000"},
		{name: "scheme is part of the key", input: "https://" + testServer, wantErr: errNotFound, wantServer: "https://" + testServer},
		{name: "missing database file", input: testServer, noFile: true, wantErr: errNotFound, wantServer: testServer},
		{name: "database not configured", input: testServer, unsetDB: true, wantErr: errNotFound, wantServer: testServer},
		{name: "null database", input: testServer, wantErr: errNotFound, wantServer: testServer, null: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			seed := db
			if tc.noFile {
				seed = nil
			}

			cfg := newConfig(t, seed)
			if tc.unsetDB {
				cfg.dbPath = ""
			}

			if tc.null {
				if err := os.WriteFile(cfg.dbPath, []byte("null\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			out, err := invoke(cfg, tc.input, "get")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error %v, want %v", err, tc.wantErr)
			}

			if want := []logLine{{Op: "get", Server: tc.wantServer}}; !slices.Equal(readLog(t, cfg.logPath), want) {
				t.Fatalf("log %v, want %v", readLog(t, cfg.logPath), want)
			}

			if tc.wantErr != nil {
				if out != "" {
					t.Fatalf("unexpected output %q", out)
				}

				return
			}

			var got serverCredentials

			decoder := json.NewDecoder(strings.NewReader(out))
			decoder.DisallowUnknownFields()

			if err := decoder.Decode(&got); err != nil {
				t.Fatalf("decode %q: %v", out, err)
			}

			if want := (serverCredentials{ServerURL: testServer, Username: "tester", Secret: testSecret}); got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestNotFoundMessage(t *testing.T) {
	if errNotFound.Error() != "credentials not found in native keychain" {
		t.Fatalf("clients match this exact text, got %q", errNotFound.Error())
	}
}

func TestFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		input   string
		db      string
		wantLog logLine
		wantMsg string
	}{
		{name: "no operation", wantMsg: `unsupported operation ""`},
		{name: "store", args: []string{"store"}, input: `{"ServerURL":"h","Username":"u","Secret":"` + testSecret + `"}`, wantLog: logLine{Op: "store"}, wantMsg: `unsupported operation "store"`},
		{name: "erase", args: []string{"erase"}, input: testServer, wantLog: logLine{Op: "erase"}, wantMsg: `unsupported operation "erase"`},
		{name: "list", args: []string{"list"}, wantLog: logLine{Op: "list"}, wantMsg: `unsupported operation "list"`},
		{name: "get with extra arguments", args: []string{"get", "x"}, input: testServer, wantLog: logLine{Op: "get x"}, wantMsg: "unsupported operation"},
		{name: "get without server", args: []string{"get"}, input: " \n", wantLog: logLine{Op: "get"}, wantMsg: errNoServer.Error()},
		{
			name: "corrupt database", args: []string{"get"}, input: testServer, db: "{",
			wantLog: logLine{Op: "get", Server: testServer}, wantMsg: "decode credential database",
		},
		{
			name: "database is not an object", args: []string{"get"}, input: testServer, db: `["h"]`,
			wantLog: logLine{Op: "get", Server: testServer}, wantMsg: "decode credential database",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := newConfig(t, nil)
			if tc.db != "" {
				if err := os.WriteFile(cfg.dbPath, []byte(tc.db), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			_, err := invoke(cfg, tc.input, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error %v, want it to mention %q", err, tc.wantMsg)
			}

			if got := readLog(t, cfg.logPath); !slices.Equal(got, []logLine{tc.wantLog}) {
				t.Fatalf("log %v, want %v", got, tc.wantLog)
			}

			if data, _ := os.ReadFile(cfg.logPath); bytes.Contains(data, []byte(testSecret)) {
				t.Fatalf("log leaks a secret: %s", data)
			}
		})
	}
}

func TestLogDisabled(t *testing.T) {
	cfg := newConfig(t, map[string]entry{testServer: {Username: "u", Secret: "s"}})
	cfg.logPath = ""

	if _, err := invoke(cfg, testServer, "get"); err != nil {
		t.Fatal(err)
	}
}

func TestLogUnwritable(t *testing.T) {
	cfg := newConfig(t, nil)
	cfg.logPath = filepath.Join(cfg.logPath, "not-a-dir", "log.jsonl")

	if _, err := invoke(cfg, testServer, "get"); err == nil || errors.Is(err, errNotFound) {
		t.Fatalf("error %v, want a log failure", err)
	}
}
