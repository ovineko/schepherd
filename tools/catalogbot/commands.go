package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ovineko/schepherd/internal/calver"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/state"
	"github.com/ovineko/schepherd/tools/catalogbot/internal/bot"
	"github.com/ovineko/schepherd/tools/catalogbot/internal/github"
	"github.com/ovineko/schepherd/tools/catalogbot/internal/notes"
	"github.com/ovineko/schepherd/tools/catalogbot/internal/sourcefile"
)

// maxCommitFileBytes bounds one file of a commit; the state of the whole
// SchemaStore catalog is a few MiB.
const maxCommitFileBytes = 32 << 20

// outputPerm keeps written files readable only by the runner user.
const outputPerm = 0o600

func newRoot(cfg settings, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:           "catalogbot",
		Short:         "Record, tag and release schema catalog revisions on GitHub",
		Args:          cobra.NoArgs,
		RunE:          func(cmd *cobra.Command, _ []string) error { return usageError(cmd, "a command is required") },
		SilenceUsage:  true,
		SilenceErrors: true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
	}

	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return fault.Wrap(fault.Usage, err, "invalid flags")
	})

	logf := func(format string, args ...any) {
		_, _ = fmt.Fprintf(stderr, "catalogbot: %s\n", cfg.redact(fmt.Sprintf(format, args...)))
	}

	root.AddCommand(
		newCommitCmd(cfg, logf),
		newReleaseCmd(cfg, logf),
		newNotesCmd(),
		newSummaryCmd(),
		newStatusCmd(cfg),
		newSetCommitCmd(),
	)

	return root
}

type commitFlags struct {
	repo         string
	branch       string
	expectedHead string
	messageFile  string
	files        []string
	asJSON       bool
}

func newCommitCmd(cfg settings, logf func(string, ...any)) *cobra.Command {
	var f commitFlags

	cmd := &cobra.Command{
		Use:   "commit",
		Short: "Write files to a branch as one GitHub-signed commit",
		Long: "commit creates one commit on --branch through the GraphQL createCommitOnBranch mutation, so\n" +
			"GitHub signs it, and prints its ID. --expected-head is the commit the file contents were derived\n" +
			"from. When the branch has moved on, the commit goes on top of the new head only if none of the\n" +
			"files changed since --expected-head; otherwise nothing is committed and the command fails (exit\n" +
			"5). When the branch already holds exactly these contents, nothing is committed and the commit\n" +
			"that last changed the first --file is printed, so a re-run is safe. Needs GH_TOKEN.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCommit(cmd, cfg, &f, logf)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.repo, "repo", "", "GitHub repository owner/name (required)")
	flags.StringVar(&f.branch, "branch", "", "branch that receives the commit (required)")
	flags.StringVar(&f.expectedHead, "expected-head", "", "full ID of the commit the contents were derived from (required)")
	flags.StringVar(&f.messageFile, "message-file", "", "commit message: headline, blank line, body (required)")
	flags.StringArrayVar(&f.files, "file", nil, "PATH=LOCAL: write the local file LOCAL to PATH in the repository (repeatable, required)")
	flags.BoolVar(&f.asJSON, "json", false, `print {"commit": ..., "created": ...}`)
	markRequired(cmd, "repo", "branch", "expected-head", "message-file", "file")

	return cmd
}

func runCommit(cmd *cobra.Command, cfg settings, f *commitFlags, logf func(string, ...any)) error {
	headline, body, err := readMessage(f.messageFile)
	if err != nil {
		return err
	}

	files := make([]github.FileAddition, 0, len(f.files))

	for _, spec := range f.files {
		repoPath, local, ok := strings.Cut(spec, "=")
		if !ok || local == "" {
			return fault.New(fault.Usage, "--file %q must be PATH=LOCAL", spec)
		}

		if err := bot.CheckRepositoryPath(repoPath); err != nil {
			return err //nolint:wrapcheck // already fault.Usage
		}

		contents, err := readLocal(local, maxCommitFileBytes)
		if err != nil {
			return err
		}

		files = append(files, github.FileAddition{Path: repoPath, Contents: contents})
	}

	client, err := cfg.client(f.repo, true)
	if err != nil {
		return err
	}

	res, err := bot.Commit(cmd.Context(), client, bot.CommitOptions{
		Log: logf, Branch: f.branch, Base: f.expectedHead, Headline: headline, Body: body, Files: files,
	})
	if err != nil {
		return err //nolint:wrapcheck // classified by package bot
	}

	if f.asJSON {
		return writeJSON(cmd.OutOrStdout(), res)
	}

	return writeLine(cmd.OutOrStdout(), res.OID)
}

type releaseFlags struct {
	repo     string
	revision string
	commit   string
	notes    string
	asJSON   bool
}

func newReleaseCmd(cfg settings, logf func(string, ...any)) *cobra.Command {
	var f releaseFlags

	cmd := &cobra.Command{
		Use:   "release",
		Short: "Tag a catalog revision and publish its GitHub release (never marked latest)",
		Long: "release creates the tag catalog-<revision> on --commit and a GitHub release titled\n" +
			"\"Schemas <revision>\" with the body from --notes. The release is never marked as the latest\n" +
			"release: that stays the client's. An existing tag on the same commit and an existing release\n" +
			"are accepted as they are; a tag on another commit fails (exit 5), because published tags never\n" +
			"move. Prints the release URL. Needs GH_TOKEN.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			revision, err := calver.ParseRevision(f.revision)
			if err != nil {
				return fault.Wrap(fault.Usage, err, "--revision")
			}

			body, err := readLocal(f.notes, notes.MaxBytes)
			if err != nil {
				return err
			}

			client, err := cfg.client(f.repo, true)
			if err != nil {
				return err
			}

			res, err := bot.Release(cmd.Context(), client, bot.ReleaseOptions{
				Log: logf, Revision: revision, Commit: f.commit, Notes: string(body),
			})
			if err != nil {
				return err //nolint:wrapcheck // classified by package bot
			}

			if f.asJSON {
				return writeJSON(cmd.OutOrStdout(), res)
			}

			return writeLine(cmd.OutOrStdout(), res.URL)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.repo, "repo", "", "GitHub repository owner/name (required)")
	flags.StringVar(&f.revision, "revision", "", "catalog revision YYYYMMDD.HHMM (required)")
	flags.StringVar(&f.commit, "commit", "", "full ID of the commit that recorded the revision (required)")
	flags.StringVar(&f.notes, "notes", "", "Markdown file with the release notes (required)")
	flags.BoolVar(&f.asJSON, "json", false, "print the tag, commit, URL and what was created as JSON")
	markRequired(cmd, "repo", "revision", "commit", "notes")

	return cmd
}

func newNotesCmd() *cobra.Command {
	var result, statePath, repository, out string

	cmd := &cobra.Command{
		Use:   "notes",
		Short: "Render the release notes of a recorded catalog revision",
		Long: "notes renders Markdown release notes from the state that recorded a catalog revision (--state):\n" +
			"the IDs the revision added, changed, updated in metadata only and excluded by an explicit rule, the\n" +
			"schemas it keeps at their last published version with the reason, all with their names, the\n" +
			"catalog digest and revision, the upstream commit and how to pin the revision. --result, the JSON\n" +
			"result of `schepherd-publisher publish` that wrote the state, is cross-checked when given. The output\n" +
			"depends on its inputs only.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := state.Load(statePath)
			if err != nil {
				return err //nolint:wrapcheck // classified by package state
			}

			if st == nil {
				return fault.New(fault.Usage, "--state %s does not exist", statePath)
			}

			if result != "" {
				res, err := notes.LoadResult(result)
				if err != nil {
					return err //nolint:wrapcheck // classified by package notes
				}

				if err := notes.Check(res, st); err != nil {
					return err //nolint:wrapcheck // classified by package notes
				}
			}

			rendered, err := notes.Render(st, repository)
			if err != nil {
				return err //nolint:wrapcheck // classified by package notes
			}

			return writeOutput(cmd.OutOrStdout(), out, rendered)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&statePath, "state", "", "state file that records the revision (required)")
	flags.StringVar(&repository, "repository", "", "OCI repository of the catalog, host/path (required)")
	flags.StringVar(&result, "result", "", "JSON result of schepherd-publisher publish --json to cross-check with the state")
	flags.StringVar(&out, "out", "", "write the notes to this file instead of stdout")
	markRequired(cmd, "state", "repository")

	return cmd
}

func newSummaryCmd() *cobra.Command {
	var diffPath, reportPath, statePath, statusPath, upstream, checkedAt, out string

	publishing := true

	cmd := &cobra.Command{
		Use:   "summary",
		Short: "Render the job summary of a weekly catalog run",
		Long: "summary renders the Markdown job summary of a run: when it was checked (--checked-at, RFC 3339),\n" +
			"the upstream commit, how many schemas were added, changed, updated in metadata only, held and\n" +
			"excluded, and every held schema with the reason. With --diff (the JSON of `schepherd-publisher diff\n" +
			"--json`), --report (the report.json of the prepared set) and --upstream it describes a run that\n" +
			"prepared upstream, which publishes a changed catalog unless --publish=false, together with what the\n" +
			"preparation made of every upstream record: how many it included, left pending review (by reason)\n" +
			"and failed (each with its reason). With --state and --status (the JSON of `catalogbot status`) it\n" +
			"describes a run that finishes the recorded revision. The output depends on its inputs only.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			at, err := time.Parse(time.RFC3339, checkedAt)
			if err != nil {
				return fault.Wrap(fault.Usage, err, "--checked-at")
			}

			var rendered []byte

			switch {
			case diffPath != "" && reportPath != "" && statePath == "" && statusPath == "":
				rendered, err = runSummary(diffPath, reportPath, upstream, at, publishing)
			case diffPath == "" && reportPath == "" && statePath != "" && statusPath != "" && upstream == "" && !cmd.Flags().Changed("publish"):
				rendered, err = finishSummary(statePath, statusPath, at)
			default:
				return usageError(cmd, "pass either --diff, --report and --upstream (and --publish), or --state and --status")
			}

			if err != nil {
				return err
			}

			return writeOutput(cmd.OutOrStdout(), out, rendered)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&diffPath, "diff", "", "JSON result of schepherd-publisher diff --json")
	flags.StringVar(&reportPath, "report", "", "report.json of the prepared set, with --diff")
	flags.StringVar(&upstream, "upstream", "", "upstream commit the run prepared, with --diff")
	flags.BoolVar(&publishing, "publish", true, "whether the run publishes a changed catalog, with --diff (false for a run that is not on main)")
	flags.StringVar(&statePath, "state", "", "state file of the revision a finish run completes")
	flags.StringVar(&statusPath, "status", "", "JSON result of catalogbot status --json, with --state")
	flags.StringVar(&checkedAt, "checked-at", "", "time of the check, RFC 3339 (required)")
	flags.StringVar(&out, "out", "", "write the summary to this file instead of stdout")
	markRequired(cmd, "checked-at")

	return cmd
}

func runSummary(diffPath, reportPath, upstream string, at time.Time, publishing bool) ([]byte, error) {
	d, err := notes.LoadDiff(diffPath)
	if err != nil {
		return nil, err //nolint:wrapcheck // classified by package notes
	}

	cov, err := notes.LoadCoverage(reportPath)
	if err != nil {
		return nil, err //nolint:wrapcheck // classified by package notes
	}

	return notes.RunSummary(d, cov, at, upstream, publishing) //nolint:wrapcheck // classified by package notes
}

func finishSummary(statePath, statusPath string, at time.Time) ([]byte, error) {
	st, err := state.Load(statePath)
	if err != nil {
		return nil, err //nolint:wrapcheck // classified by package state
	}

	if st == nil {
		return nil, fault.New(fault.Usage, "--state %s does not exist", statePath)
	}

	data, err := readLocal(statusPath, 1<<20)
	if err != nil {
		return nil, err
	}

	var status bot.Status
	if err := json.Unmarshal(data, &status); err != nil {
		return nil, fault.Wrap(fault.Usage, err, "--status %s", statusPath)
	}

	if !status.Pending || status.Revision != st.Catalog.Revision {
		return nil, fault.New(fault.Usage, "--status %s does not describe the unfinished revision %s of --state", statusPath, st.Catalog.Revision)
	}

	return notes.FinishSummary(st, status.Missing, at) //nolint:wrapcheck // classified by package notes
}

func newStatusCmd(cfg settings) *cobra.Command {
	var repo, statePath, latest string

	var asJSON bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Tell whether the recorded catalog revision was tagged, released and made latest",
		Long: "status reads the revision recorded in --state and checks its Git tag and GitHub release, and\n" +
			"compares --latest-digest (what catalog-latest resolves to, empty when unknown) with the\n" +
			"recorded catalog digest. pending is true when any of them is missing, so the next run finishes\n" +
			"that revision first. A missing state file is not pending. GH_TOKEN is used when set.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if latest != "" {
				if err := digest.Validate(latest); err != nil {
					return fault.Wrap(fault.Usage, err, "--latest-digest")
				}
			}

			st, err := state.Load(statePath)
			if err != nil {
				return err //nolint:wrapcheck // classified by package state
			}

			client, err := cfg.client(repo, false)
			if err != nil {
				return err
			}

			res, err := bot.CheckStatus(cmd.Context(), client, st, latest)
			if err != nil {
				return err //nolint:wrapcheck // classified by package bot
			}

			if asJSON {
				return writeJSON(cmd.OutOrStdout(), res)
			}

			return writeLine(cmd.OutOrStdout(), res.String())
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&repo, "repo", "", "GitHub repository owner/name (required)")
	flags.StringVar(&statePath, "state", "", "state file, usually catalog/state.json (required)")
	flags.StringVar(&latest, "latest-digest", "", "digest catalog-latest resolves to; empty when unknown")
	flags.BoolVar(&asJSON, "json", false, "print the status as JSON")
	markRequired(cmd, "repo", "state")

	return cmd
}

func newSetCommitCmd() *cobra.Command {
	var source, commit, statePath, out string

	cmd := &cobra.Command{
		Use:   "set-commit",
		Short: "Write a copy of the upstream source description that records another commit",
		Long: "set-commit copies --source to --out with only its `commit = \"...\"` line changed, comments and\n" +
			"layout untouched, and checks that the copy loads as the same upstream description apart from the\n" +
			"commit. The commit is --commit, or the SchemaStore upstream commit that the state file --state\n" +
			"records, so the source description committed with a published state names what was published.\n" +
			"Given both, they must agree (exit 5 otherwise).",
		Args: noArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			recorded, err := commitToRecord(commit, statePath)
			if err != nil {
				return err
			}

			return sourcefile.Rewrite(source, out, recorded)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&source, "source", "", "upstream source description, usually sources/schemastore.toml (required)")
	flags.StringVar(&commit, "commit", "", "upstream commit to record, 40 lowercase hex digits")
	flags.StringVar(&statePath, "state", "", "state file whose SchemaStore upstream commit is recorded")
	flags.StringVar(&out, "out", "", "file to write (required)")
	markRequired(cmd, "source", "out")

	return cmd
}

// commitToRecord returns the upstream commit set-commit records: commit,
// or the one the state at statePath records, which commit must then equal.
func commitToRecord(commit, statePath string) (string, error) {
	if statePath == "" {
		if commit == "" {
			return "", fault.New(fault.Usage, "set-commit needs --commit or --state")
		}

		return commit, nil
	}

	st, err := state.Load(statePath)
	if err != nil {
		return "", err //nolint:wrapcheck // classified by package state
	}

	switch {
	case st == nil:
		return "", fault.New(fault.Usage, "--state %s does not exist", statePath)
	case st.Source.Kind != state.KindSchemaStore || st.Source.Commit == "":
		return "", fault.New(fault.Usage, "--state %s records no SchemaStore upstream commit", statePath)
	case commit != "" && commit != st.Source.Commit:
		return "", fault.New(fault.Integrity, "--commit %s is not the upstream commit %s that --state %s records", commit, st.Source.Commit, statePath)
	}

	return st.Source.Commit, nil
}

func (s settings) client(repo string, needToken bool) (*github.Client, error) {
	if needToken && s.token == "" {
		return nil, fault.New(fault.Usage, "%s is not set", keyToken)
	}

	return github.New(github.Options{ //nolint:wrapcheck // classified by package github
		APIURL: s.apiURL, GraphQLURL: s.graphqlURL, Token: s.token, Repository: repo,
	})
}

// readMessage splits a commit message file into its headline (first line)
// and body (everything after the blank line that follows it).
func readMessage(path string) (headline, body string, err error) {
	data, err := readLocal(path, 64<<10)
	if err != nil {
		return "", "", err
	}

	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	headline, body, _ = strings.Cut(text, "\n")

	headline = strings.TrimSpace(headline)
	if headline == "" {
		return "", "", fault.New(fault.Usage, "the commit message in %s has no headline", path)
	}

	return headline, strings.TrimSpace(body), nil
}

func readLocal(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "read %s", path)
	}

	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fault.New(fault.Usage, "%s must be a regular file of at most %d bytes", path, limit)
	}

	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "read %s", path)
	}

	if int64(len(data)) > limit {
		return nil, fault.New(fault.Usage, "%s grew beyond %d bytes while it was read", path, limit)
	}

	return data, nil
}

// writeOutput writes data to path, or to w when path is empty.
func writeOutput(w io.Writer, path string, data []byte) error {
	if path == "" {
		_, err := w.Write(data)

		return err //nolint:wrapcheck // a failed write to stdout needs no context
	}

	if err := os.WriteFile(filepath.Clean(path), data, outputPerm); err != nil {
		return fault.Wrap(fault.Usage, err, "write %s", path)
	}

	return nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(v) //nolint:wrapcheck // a failed write to stdout needs no context
}

func writeLine(w io.Writer, line string) error {
	_, err := fmt.Fprintln(w, line)

	return err //nolint:wrapcheck // a failed write to stdout needs no context
}

func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageError(cmd, "unexpected arguments: "+strings.Join(args, " "))
	}

	return nil
}

func usageError(cmd *cobra.Command, message string) error {
	return fault.New(fault.Usage, "%s: %s (see %s --help)", cmd.CommandPath(), message, cmd.CommandPath())
}

func markRequired(cmd *cobra.Command, names ...string) {
	for _, name := range names {
		if err := cmd.MarkFlagRequired(name); err != nil {
			panic(err)
		}
	}
}
