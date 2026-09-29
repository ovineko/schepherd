package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoticesGenerateState(t *testing.T) {
	root := fixtureRoot(t)
	out := filepath.Join(t.TempDir(), "notices.md")
	missing := filepath.Join(t.TempDir(), "absent.json")

	if code, _, stderr := execute(t, fixtureEnv(), "notices", "generate", "--root", root, "--state", missing, "--out", out); code != 2 ||
		!strings.Contains(stderr, "--state") {
		t.Errorf("generate --state naming a missing file: exit %d, stderr %q", code, stderr)
	}

	invalid := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(invalid, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code, _, stderr := execute(t, fixtureEnv(), "notices", "generate", "--root", root, "--state", invalid, "--out", out); code != 1 ||
		!strings.Contains(stderr, "notices generate") {
		t.Errorf("generate --state naming an invalid state: exit %d, stderr %q", code, stderr)
	}
}
