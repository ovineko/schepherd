package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ovineko/schepherd/internal/buildinfo/semver"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/tools/release/internal/gitx"
	"github.com/ovineko/schepherd/tools/release/internal/releasetag"
	"github.com/ovineko/schepherd/tools/release/internal/wrappers"
)

// npm dist-tags of the client packages: a release becomes "latest", a
// pre-release only "next", so `npm install @ovineko/schepherd` never picks a
// pre-release.
const (
	npmDistTagLatest = "latest"
	npmDistTagNext   = "next"
)

// defaultBase is the branch every release tag must be reachable from.
const defaultBase = "origin/main"

// versionInfo is the JSON printed by `version parse` and
// `version verify-tag --json`. PypiVersion and GemVersion are the versions of
// the wheels and the gem. PreviousTag, set only by verify-tag, is the release
// tag the changelog of this release starts from; it is empty for the first
// release.
type versionInfo struct {
	Version     string `json:"version"`
	Tag         string `json:"tag"`
	NpmDistTag  string `json:"npmDistTag"`
	PypiVersion string `json:"pypiVersion"`
	GemVersion  string `json:"gemVersion"`
	PreviousTag string `json:"previousTag,omitempty"`
	Prerelease  bool   `json:"prerelease"`
}

// describe fails for a version that PyPI or RubyGems cannot express in the
// same order, so such a tag is refused before anything is built for it.
func describe(v semver.Version) (versionInfo, error) {
	info := versionInfo{Version: v.String(), Tag: v.Tag(), Prerelease: v.IsPrerelease(), NpmDistTag: npmDistTagLatest}
	if info.Prerelease {
		info.NpmDistTag = npmDistTagNext
	}

	versions, err := wrappers.VersionsOf(v.String())
	if err != nil {
		return versionInfo{}, fmt.Errorf("version %s cannot be published to PyPI or RubyGems: %w", v, err)
	}

	info.PypiVersion, info.GemVersion = versions.PEP440, versions.Gem

	return info, nil
}

// parseReleaseForm accepts a release version X.Y.Z[-pre] or its tag.
func parseReleaseForm(s string) (semver.Version, error) {
	if strings.HasPrefix(s, semver.TagPrefix) {
		v, err := semver.ParseReleaseTag(s)
		if err != nil {
			return semver.Version{}, fmt.Errorf("parse release version: %w", err)
		}

		return v, nil
	}

	v, err := semver.ParseRelease(s)
	if err != nil {
		return semver.Version{}, fmt.Errorf("parse release version: %w", err)
	}

	return v, nil
}

func newVersionCommand(environ []string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Check and convert client release versions (SemVer vX.Y.Z[-prerelease])",
		Args:  cobra.ArbitraryArgs,
		RunE:  requireSubcommand,
	}

	cmd.AddCommand(
		newVersionParseCommand(),
		newVersionVerifyTagCommand(environ),
		newVersionSnapshotCommand(),
	)

	return cmd
}

// verifyTag checks a release tag against the other tags of the repository:
// it is a valid release tag, no other tag carries the same version, and it
// is newer than every release that is not a pre-release, so the release
// marked latest on GitHub and npm is always the newest one. Pre-releases may
// be followed by lower versions (v0.2.0-rc.1, then v0.1.1). Tags that are
// not versions, such as catalog-YYYYMMDD.HHMM, are ignored; the tag itself
// may be among others.
func verifyTag(tag string, others []string) (semver.Version, error) {
	v, err := semver.ParseReleaseTag(tag)
	if err != nil {
		return semver.Version{}, fault.Wrap(fault.Internal, err, "version verify-tag")
	}

	var (
		newest    semver.Version
		newestTag string
	)

	for _, other := range others {
		other = strings.TrimSpace(other)
		if other == tag || other == "" {
			continue
		}

		o, err := semver.ParseTag(other)
		if err != nil {
			continue
		}

		if semver.Compare(o, v) == 0 {
			return semver.Version{}, fmt.Errorf("tag %s duplicates the version of the existing tag %s", tag, other)
		}

		if o.IsPrerelease() || o.Build != "" {
			continue
		}

		if newestTag == "" || semver.Compare(o, newest) > 0 {
			newest, newestTag = o, other
		}
	}

	if newestTag != "" && semver.Compare(v, newest) < 0 {
		return semver.Version{}, fmt.Errorf("tag %s is older than the existing release %s; a release must be newer than every earlier release", tag, newestTag)
	}

	return v, nil
}

// previousTag returns the newest release tag below v among tags, the v* tags
// reachable from the tagged commit, so the changelog of v starts there and
// never at a catalog-YYYYMMDD.HHMM tag. The changelog of a release that is
// not a pre-release starts at the previous such release, so it also lists
// what its own release candidates already shipped.
func previousTag(v semver.Version, tags []string) string {
	var (
		best    semver.Version
		bestTag string
	)

	for _, tag := range tags {
		tag = strings.TrimSpace(tag)

		o, err := semver.ParseReleaseTag(tag)
		if err != nil || semver.Compare(o, v) >= 0 || (!v.IsPrerelease() && o.IsPrerelease()) {
			continue
		}

		if bestTag == "" || semver.Compare(o, best) > 0 {
			best, bestTag = o, tag
		}
	}

	return bestTag
}

// runVerifyTag checks the tag with verifyTag against every v* tag of the
// repository and requires its commit to be reachable from base, so only
// what reached the main branch through review is ever released. The tagged
// commit must also pass releasetag.CheckCommit: a module path that serves
// the tag's major version. A version without a PyPI or RubyGems form is
// refused first (describe).
func runVerifyTag(cmd *cobra.Command, git gitx.Runner, tag, base string) (versionInfo, error) {
	parsed, err := semver.ParseReleaseTag(tag)
	if err != nil {
		return versionInfo{}, fault.Wrap(fault.Internal, err, "version verify-tag")
	}

	if base == "" || strings.HasPrefix(base, "-") {
		return versionInfo{}, fault.New(fault.Usage, "--base %q is not a branch or commit", base)
	}

	info, err := describe(parsed)
	if err != nil {
		return versionInfo{}, fault.Wrap(fault.Internal, err, "version verify-tag")
	}

	ctx := cmd.Context()

	commit, err := git.Output(ctx, "rev-parse", "--verify", "--quiet", "refs/tags/"+tag+"^{commit}")
	if err != nil {
		return versionInfo{}, fmt.Errorf("tag %s is not a tag of this repository (fetch every tag first): %w", tag, err)
	}

	baseCommit, err := git.Output(ctx, "rev-parse", "--verify", "--quiet", base+"^{commit}")
	if err != nil {
		return versionInfo{}, fmt.Errorf("base %s is not a commit of this repository (fetch it first): %w", base, err)
	}

	if _, err := git.Run(ctx, "merge-base", "--is-ancestor", commit, baseCommit); err != nil {
		var exit *gitx.CommandError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return versionInfo{}, fmt.Errorf("tag %s points to commit %s, which is not reachable from %s; release only commits of %s", tag, commit, base, base)
		}

		return versionInfo{}, fmt.Errorf("check that %s is reachable from %s: %w", tag, base, err)
	}

	out, err := git.Output(ctx, "tag", "--list", "v*")
	if err != nil {
		return versionInfo{}, fmt.Errorf("list tags: %w", err)
	}

	v, err := verifyTag(tag, strings.Split(out, "\n"))
	if err != nil {
		return versionInfo{}, err
	}

	if err := releasetag.CheckCommit(ctx, git, commit, v); err != nil {
		return versionInfo{}, fmt.Errorf("version verify-tag: %w", err)
	}

	reachable, err := git.Output(ctx, "tag", "--list", "--merged", commit, "v*")
	if err != nil {
		return versionInfo{}, fmt.Errorf("list the tags reachable from %s: %w", tag, err)
	}

	info.PreviousTag = previousTag(v, strings.Split(reachable, "\n"))

	return info, nil
}

func newVersionParseCommand() *cobra.Command {
	var goMod string

	cmd := &cobra.Command{
		Use:   "parse <version|tag>",
		Short: "Print a release version, its tag, whether it is a pre-release, its npm dist-tag and its PyPI and RubyGems versions as JSON",
		Long: "Refuses a version whose major version the module path of --go-mod cannot serve: 2 and above need the " +
			"module path suffix /vN, because the release tag is also the Go module version. Also refuses a pre-release " +
			"that PyPI or RubyGems cannot express in SemVer order: only -alpha.N, -beta.N and -rc.N are accepted " +
			"(pypiVersion X.Y.ZaN, X.Y.ZbN or X.Y.ZrcN; gemVersion X.Y.Z.alpha.N, X.Y.Z.beta.N or X.Y.Z.rc.N).",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := parseReleaseForm(args[0])
			if err != nil {
				return fault.Wrap(fault.Internal, err, "version parse")
			}

			data, err := os.ReadFile(filepath.Clean(goMod))
			if err != nil {
				return fmt.Errorf("version parse: read the module path: %w", err)
			}

			modulePath, err := releasetag.ModulePath(data)
			if err != nil {
				return fmt.Errorf("version parse: %s: %w", goMod, err)
			}

			if err := releasetag.CheckModulePath(modulePath, v); err != nil {
				return fmt.Errorf("version parse: %w", err)
			}

			info, err := describe(v)
			if err != nil {
				return fmt.Errorf("version parse: %w", err)
			}

			return writeJSON(cmd, info)
		},
	}

	cmd.Flags().StringVar(&goMod, "go-mod", "go.mod", "go.mod of the module the version is released for")

	return cmd
}

func newVersionVerifyTagCommand(environ []string) *cobra.Command {
	var (
		repo, base string
		asJSON     bool
	)

	cmd := &cobra.Command{
		Use:   "verify-tag <tag>",
		Short: "Check a pushed release tag vX.Y.Z[-prerelease] and print its version",
		Long: "Accepts the tag only when it is a valid release version, no other tag carries the same version, it is " +
			"newer than every existing release tag v* that is not a pre-release, and its commit is reachable from --base. " +
			"In the tagged commit, the module path of go.mod must serve the tag's major version (/vN from 2 on). " +
			"A pre-release other than -alpha.N, -beta.N or -rc.N is refused, since PyPI and RubyGems could not publish " +
			"it in SemVer order. --json also prints whether it is a pre-release, its npm dist-tag (latest or next), its " +
			"PyPI and RubyGems versions and previousTag, the newest lower release tag reachable from the tag (for a " +
			"release, the newest lower release that is not a pre-release), where its changelog starts.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := runVerifyTag(cmd, gitx.New(repo, environ), args[0], base)
			if err != nil {
				return err
			}

			if asJSON {
				return writeJSON(cmd, info)
			}

			return writeLine(cmd, info.Version)
		},
	}

	cmd.Flags().StringVar(&repo, "repo", ".", "repository whose tags are read; it must have every v* tag")
	cmd.Flags().StringVar(&base, "base", defaultBase, "branch or commit the tagged commit must be reachable from")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the version, tag, pre-release flag, npm dist-tag, PyPI and RubyGems versions and previous tag as JSON")

	return cmd
}

// snapshotVersion returns the version GoReleaser gave a snapshot build, as
// recorded in dist/metadata.json, after checking that it is a snapshot
// version 0.0.0-snapshot-<commit> and therefore never a release.
func snapshotVersion(dist string) (string, error) {
	data, err := os.ReadFile(filepath.Clean(filepath.Join(dist, "metadata.json")))
	if err != nil {
		return "", fmt.Errorf("read GoReleaser metadata (build a snapshot first): %w", err)
	}

	var metadata struct {
		Version string `json:"version"`
	}

	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", fmt.Errorf("parse GoReleaser metadata: %w", err)
	}

	v, err := semver.Parse(metadata.Version)
	if err != nil {
		return "", fmt.Errorf("GoReleaser metadata: %w", err)
	}

	commit, ok := strings.CutPrefix(v.String(), wrappers.SnapshotPrefix)
	if !ok || v.Build != "" || commit == "" || strings.Trim(commit, "0123456789abcdef") != "" {
		return "", fmt.Errorf("GoReleaser built version %s, want a snapshot version %s<commit>", v, wrappers.SnapshotPrefix)
	}

	return v.String(), nil
}

func newVersionSnapshotCommand() *cobra.Command {
	var dist string

	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Print the snapshot version 0.0.0-snapshot-<commit> of a GoReleaser snapshot build",
		Long: "Reads the version from <dist>/metadata.json, so the npm packages of a snapshot carry the version of its " +
			"archives. A version that is not 0.0.0-snapshot-<commit> fails the command.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			v, err := snapshotVersion(dist)
			if err != nil {
				return fmt.Errorf("version snapshot: %w", err)
			}

			return writeLine(cmd, v)
		},
	}

	cmd.Flags().StringVar(&dist, "dist", "dist", "GoReleaser dist directory of a snapshot build")

	return cmd
}

func writeLine(cmd *cobra.Command, line string) error {
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), line); err != nil {
		return fmt.Errorf("write output: %w", err)
	}

	return nil
}

func writeJSON(cmd *cobra.Command, v any) error {
	if err := json.NewEncoder(cmd.OutOrStdout()).Encode(v); err != nil {
		return fmt.Errorf("write JSON: %w", err)
	}

	return nil
}
