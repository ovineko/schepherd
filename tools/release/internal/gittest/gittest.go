// Package gittest creates throwaway git repositories for the release tooling
// tests. Repositories live in t.TempDir(), and every git command runs with an
// isolated configuration, so the host's global settings (commit signing,
// excludes files, filters) never influence a test and are never modified.
package gittest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/tools/release/internal/gitx"
)

// Repo is a git repository in a temporary directory.
type Repo struct {
	T   testing.TB
	Dir string
	Git gitx.Runner
	Env []string
}

// Env returns the process environment with an isolated git configuration and
// a test identity. Pass it to code under test as its base environment.
func Env(tb testing.TB) []string {
	tb.Helper()

	config, err := gitx.NewEmptyConfig(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}

	return gitx.Isolated("", env.Environ(), config).With(
		"GIT_AUTHOR_NAME=Test Author",
		"GIT_AUTHOR_EMAIL=author@example.invalid",
		"GIT_COMMITTER_NAME=Test Author",
		"GIT_COMMITTER_EMAIL=author@example.invalid",
		"GIT_AUTHOR_DATE=2026-09-23T12:00:00Z",
		"GIT_COMMITTER_DATE=2026-09-23T12:00:00Z",
	).Env
}

// Init creates an empty repository with branch main.
func Init(tb testing.TB) *Repo {
	tb.Helper()

	dir := tb.TempDir()
	environ := Env(tb)
	repo := &Repo{T: tb, Dir: dir, Env: environ, Git: gitx.Runner{Dir: dir, Env: environ}}
	repo.Run("init", "-q", "-b", "main")

	return repo
}

// Run runs git in the repository and fails the test on error.
func (r *Repo) Run(args ...string) string {
	r.T.Helper()

	out, err := r.Git.Output(r.T.Context(), args...)
	if err != nil {
		r.T.Fatal(err)
	}

	return out
}

// Write creates or replaces a file with the given content and permissions.
func (r *Repo) Write(rel, content string, perm os.FileMode) {
	r.T.Helper()

	full := filepath.Join(r.Dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		r.T.Fatal(err)
	}

	if err := os.WriteFile(full, []byte(content), perm); err != nil {
		r.T.Fatal(err)
	}

	if err := os.Chmod(full, perm); err != nil {
		r.T.Fatal(err)
	}
}

// CommitAll stages everything and commits it, returning the commit id.
func (r *Repo) CommitAll(message string) string {
	r.T.Helper()

	r.Run("add", "-A")
	r.Run("-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", message)

	return r.Run("rev-parse", "HEAD")
}
