// Package bot holds the Git side of the autonomous catalog update: record
// the new state on the branch as one GitHub-signed commit, tag and release
// the revision, and tell whether an earlier run left that unfinished. Every
// operation is idempotent, so re-running a failed job completes it.
package bot

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/ovineko/schepherd/internal/calver"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/state"
	"github.com/ovineko/schepherd/tools/catalogbot/internal/github"
)

// DefaultCommitAttempts bounds how often Commit follows a moving branch.
const DefaultCommitAttempts = 5

func releaseTitle(r calver.Revision) string {
	return "Schemas " + r.String()
}

// CommitOptions describes the commit Commit makes.
type CommitOptions struct {
	// Log receives progress messages; nil discards them.
	Log func(format string, args ...any)
	// Branch receives the commit.
	Branch string
	// Base is the commit the new file contents were derived from.
	Base     string
	Headline string
	Body     string
	// Files are written in this order; the first one identifies the commit
	// that already recorded the same contents.
	Files []github.FileAddition
	// Attempts defaults to DefaultCommitAttempts.
	Attempts int
}

// CommitResult is the commit that holds the requested contents.
type CommitResult struct {
	OID string `json:"commit"`
	// Created is false when the branch already had the contents.
	Created bool `json:"created"`
}

// Commit makes the branch hold opts.Files with one commit created through
// the createCommitOnBranch mutation, so GitHub signs it.
//
// When the branch head is no longer Base, the commit goes on top of the new
// head only if none of the files differ between Base and that head: the new
// contents were computed from Base, and anything else would silently discard
// someone else's change. When the branch already holds exactly these
// contents (a re-run, or a response lost after the commit was made), no
// commit is created and the commit that last changed the first file is
// returned.
func Commit(ctx context.Context, c *github.Client, opts CommitOptions) (*CommitResult, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	logf := logger(opts.Log)

	attempts := opts.Attempts
	if attempts <= 0 {
		attempts = DefaultCommitAttempts
	}

	var lastErr error

	for range attempts {
		head, err := c.BranchHead(ctx, opts.Branch)
		if err != nil {
			return nil, err //nolint:wrapcheck // classified by package github
		}

		if oid, done, err := recorded(ctx, c, head, opts.Files); err != nil || done {
			if done {
				logf("%s already holds these contents in %s", opts.Branch, oid)

				return &CommitResult{OID: oid}, nil
			}

			return nil, err
		}

		if head != opts.Base {
			if err := unchangedSince(ctx, c, opts, head); err != nil {
				return nil, err
			}

			logf("%s moved from %s to %s without touching the files; committing on top", opts.Branch, opts.Base, head)
		}

		oid, err := c.CreateCommitOnBranch(ctx, github.CommitInput{
			Branch: opts.Branch, ExpectedHeadOID: head, Headline: opts.Headline, Body: opts.Body, Files: opts.Files,
		})
		if err == nil {
			logf("created %s on %s", oid, opts.Branch)

			return &CommitResult{OID: oid, Created: true}, nil
		}

		lastErr = err

		now, headErr := c.BranchHead(ctx, opts.Branch)
		if headErr != nil {
			return nil, errors.Join(err, headErr)
		}

		if now == head {
			return nil, err //nolint:wrapcheck // classified by package github
		}

		logf("%s moved to %s during the commit; looking again", opts.Branch, now)
	}

	return nil, fault.Wrap(fault.Registry, lastErr, "%s kept moving; gave up after %d attempts", opts.Branch, attempts)
}

func (o *CommitOptions) validate() error {
	switch {
	case o.Branch == "" || strings.ContainsAny(o.Branch, " ~^:?*[\\") || strings.HasPrefix(o.Branch, "refs/"):
		return fault.New(fault.Usage, "invalid branch %q", o.Branch)
	case !github.ValidObjectID(o.Base):
		return fault.New(fault.Usage, "the base %q must be a full commit ID", o.Base)
	case strings.TrimSpace(o.Headline) == "" || strings.ContainsAny(o.Headline, "\r\n"):
		return fault.New(fault.Usage, "the commit headline must be one non-empty line")
	case len(o.Files) == 0:
		return fault.New(fault.Usage, "no files to commit")
	}

	seen := map[string]bool{}

	for _, f := range o.Files {
		if err := CheckRepositoryPath(f.Path); err != nil {
			return err
		}

		if seen[f.Path] {
			return fault.New(fault.Usage, "%s is given twice", f.Path)
		}

		seen[f.Path] = true
	}

	return nil
}

// CheckRepositoryPath accepts a clean, relative, slash-separated path that
// stays outside .git.
func CheckRepositoryPath(p string) error {
	if p == "" || path.Clean(p) != p || path.IsAbs(p) || p == "." || strings.HasPrefix(p, "../") || p == ".." ||
		strings.ContainsAny(p, "\\\x00") {
		return fault.New(fault.Usage, "%q is not a clean relative repository path", p)
	}

	for part := range strings.SplitSeq(p, "/") {
		if strings.EqualFold(part, ".git") {
			return fault.New(fault.Usage, "%q points into .git", p)
		}
	}

	return nil
}

// recorded reports whether head already holds every file, and if so the
// commit that recorded them.
func recorded(ctx context.Context, c *github.Client, head string, files []github.FileAddition) (string, bool, error) {
	same, err := holds(ctx, c, head, files)
	if err != nil || !same {
		return "", false, err
	}

	candidate, err := c.LastCommitTouching(ctx, head, files[0].Path)
	if err != nil {
		return "", false, err //nolint:wrapcheck // classified by package github
	}

	// A later commit may have changed another of the files to the same
	// contents; the head then is the first commit known to hold all of them.
	if same, err := holds(ctx, c, candidate, files); err != nil || !same {
		return head, err == nil, err
	}

	return candidate, true, nil
}

func holds(ctx context.Context, c *github.Client, commit string, files []github.FileAddition) (bool, error) {
	for _, f := range files {
		oid, ok, err := c.BlobID(ctx, commit, f.Path)
		if err != nil {
			return false, err //nolint:wrapcheck // classified by package github
		}

		if !ok || oid != github.BlobID(f.Contents) {
			return false, nil
		}
	}

	return true, nil
}

func unchangedSince(ctx context.Context, c *github.Client, opts CommitOptions, head string) error {
	var changed []string

	for _, f := range opts.Files {
		before, beforeOK, err := c.BlobID(ctx, opts.Base, f.Path)
		if err != nil {
			return err //nolint:wrapcheck // classified by package github
		}

		after, afterOK, err := c.BlobID(ctx, head, f.Path)
		if err != nil {
			return err //nolint:wrapcheck // classified by package github
		}

		if before != after || beforeOK != afterOK {
			changed = append(changed, f.Path)
		}
	}

	if len(changed) > 0 {
		return fault.New(fault.Integrity, "%s changed on %s between %s and %s; the new contents were derived from %s, so nothing was committed; run the update again from the new head",
			strings.Join(changed, ", "), opts.Branch, opts.Base, head, opts.Base)
	}

	return nil
}

// ReleaseOptions describes the tag and release of a revision.
type ReleaseOptions struct {
	Log      func(format string, args ...any)
	Commit   string
	Notes    string
	Revision calver.Revision
}

// ReleaseResult reports the tag and release of a revision.
type ReleaseResult struct {
	Tag            string `json:"tag"`
	Commit         string `json:"commit"`
	URL            string `json:"url"`
	TagCreated     bool   `json:"tagCreated"`
	ReleaseCreated bool   `json:"releaseCreated"`
}

// Release creates the lightweight tag catalog-<revision> on opts.Commit and a
// GitHub release of it that is never marked as the latest release, so the
// latest release stays the client's. An existing tag on the same commit and
// an existing release are accepted as they are; a tag on another commit is
// an error.
func Release(ctx context.Context, c *github.Client, opts ReleaseOptions) (*ReleaseResult, error) {
	if _, err := calver.ParseRevision(opts.Revision.String()); err != nil {
		return nil, fault.Wrap(fault.Usage, err, "release")
	}

	if !github.ValidObjectID(opts.Commit) {
		return nil, fault.New(fault.Usage, "the commit %q must be a full commit ID", opts.Commit)
	}

	logf := logger(opts.Log)
	tag := opts.Revision.Tag()
	res := &ReleaseResult{Tag: tag, Commit: opts.Commit}

	created, err := ensureTag(ctx, c, tag, opts.Commit)
	if err != nil {
		return nil, err
	}

	res.TagCreated = created
	logf("tag %s is on %s", tag, opts.Commit)

	rel, err := c.ReleaseByTag(ctx, tag)
	if errors.Is(err, github.ErrNotFound) {
		rel, err = c.CreateRelease(ctx, github.NewRelease{Tag: tag, Name: releaseTitle(opts.Revision), Body: opts.Notes})
		res.ReleaseCreated = err == nil

		if errors.Is(err, github.ErrAlreadyExists) {
			rel, err = c.ReleaseByTag(ctx, tag)
		}
	}

	if err != nil {
		return nil, err //nolint:wrapcheck // classified by package github
	}

	res.URL = rel.HTMLURL
	logf("release %s: %s", tag, rel.HTMLURL)

	return res, nil
}

func ensureTag(ctx context.Context, c *github.Client, tag, commit string) (bool, error) {
	target, err := c.TagCommit(ctx, tag)
	if errors.Is(err, github.ErrNotFound) {
		err = c.CreateTag(ctx, tag, commit)
		if err == nil {
			return true, nil
		}

		if errors.Is(err, github.ErrAlreadyExists) {
			target, err = c.TagCommit(ctx, tag)
		}
	}

	if err != nil {
		return false, err //nolint:wrapcheck // classified by package github
	}

	if target != commit {
		return false, fault.New(fault.Integrity, "tag %s already points to %s, not %s; published tags never move", tag, target, commit)
	}

	return false, nil
}

// Latest values of Status.
const (
	LatestCurrent = "current"
	LatestOther   = "other"
	LatestUnknown = "unknown"
)

// Status tells whether the revision recorded in the state was fully
// published: its Git tag, its release and catalog-latest. Pending is true
// when one of them is missing, so the next run finishes that revision
// before anything newer.
type Status struct {
	Revision      string   `json:"revision,omitempty"`
	CatalogDigest string   `json:"catalogDigest,omitempty"`
	Latest        string   `json:"latest"`
	Missing       []string `json:"missing,omitempty"`
	Recorded      bool     `json:"recorded"`
	Tag           bool     `json:"tag"`
	Release       bool     `json:"release"`
	Pending       bool     `json:"pending"`
}

// CheckStatus reads the tag and release of the recorded revision. latest is
// the digest catalog-latest resolves to, or empty when that is unknown; an
// unknown catalog-latest never makes the revision pending.
func CheckStatus(ctx context.Context, c *github.Client, st *state.State, latest string) (*Status, error) {
	res := &Status{Latest: LatestUnknown}
	if st == nil {
		return res, nil
	}

	revision, err := calver.ParseRevision(st.Catalog.Revision)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "state")
	}

	res.Recorded, res.Revision, res.CatalogDigest = true, revision.String(), st.Catalog.Digest

	if _, err := c.TagCommit(ctx, revision.Tag()); err == nil {
		res.Tag = true
	} else if !errors.Is(err, github.ErrNotFound) {
		return nil, err //nolint:wrapcheck // classified by package github
	}

	if _, err := c.ReleaseByTag(ctx, revision.Tag()); err == nil {
		res.Release = true
	} else if !errors.Is(err, github.ErrNotFound) {
		return nil, err //nolint:wrapcheck // classified by package github
	}

	switch latest {
	case "":
	case st.Catalog.Digest:
		res.Latest = LatestCurrent
	default:
		res.Latest = LatestOther
	}

	if !res.Tag {
		res.Missing = append(res.Missing, "tag "+revision.Tag())
	}

	if !res.Release {
		res.Missing = append(res.Missing, "release "+revision.Tag())
	}

	if res.Latest == LatestOther {
		res.Missing = append(res.Missing, "catalog-latest")
	}

	res.Pending = len(res.Missing) > 0

	return res, nil
}

// String summarizes the status in one line.
func (s *Status) String() string {
	switch {
	case !s.Recorded:
		return "no catalog has been recorded yet"
	case s.Pending:
		return fmt.Sprintf("revision %s is not finished: missing %s", s.Revision, strings.Join(s.Missing, ", "))
	default:
		return fmt.Sprintf("revision %s is finished (catalog-latest %s)", s.Revision, s.Latest)
	}
}

func logger(f func(string, ...any)) func(string, ...any) {
	if f == nil {
		return func(string, ...any) {}
	}

	return f
}
