package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ovineko/schepherd/internal/calver"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/bundle"
	"github.com/ovineko/schepherd/internal/publisher/prepare"
	"github.com/ovineko/schepherd/internal/publisher/publish"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

type prepareFlags struct {
	source      string
	out         string
	snapshotDir string
	state       string
	jsonschema  string
	policy      string
	ids         string
	refresh     []string
	jobs        int
	refreshAll  bool
	asJSON      bool
}

func (a *app) newPrepareCmd() *cobra.Command {
	var f prepareFlags

	cmd := &cobra.Command{
		Use:   "prepare",
		Short: "Prepare a source into verified, self-contained schemas",
		Long: "prepare reads a source description (kind \"upstream\" for the pinned SchemaStore snapshot, kind \"local\"\n" +
			"for an explicit list), decides every record (included, excluded, pending-review or failed), bundles\n" +
			"and verifies the included schemas and writes prepared.json, schemas/, notices/ and report.json\n" +
			"into --out, which must not exist or be empty.\n\n" +
			"--state is catalog/state.json of the last publication; a missing file means none. It keeps IDs stable.\n" +
			"A published schema whose source and dependencies still have their recorded digests keeps its recorded\n" +
			"artifact and license decision without asking license detection again; --refresh <id> (repeatable) and\n" +
			"--refresh-all prepare such schemas anew, to apply a recipe, bundler or policy change deliberately. A\n" +
			"published schema that cannot be refreshed is held: the next catalog keeps its last entry and artifact.\n" +
			"Only an explicit exclude rule removes one. None of this fails the run: it fails (exit 5, after writing\n" +
			"its output) only when an entry of a local source that was never published was not prepared.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.prepare(cmd, &f)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.source, "source", "", "source description file (TOML; required)")
	flags.StringVar(&f.out, "out", "", "output directory; must not exist or be empty (required)")
	flags.StringVar(&f.snapshotDir, "snapshot-dir", "", "SchemaStore snapshot directory: reused when present, downloaded into when missing")
	flags.StringVar(&f.state, "state", "", "publisher state of the last publication (catalog/state.json); a missing file means none")
	flags.StringVar(&f.jsonschema, "jsonschema", defaultBundler(), "the pinned Sourcemeta jsonschema binary (go run ./tools/install-jsonschema)")
	flags.StringVar(&f.policy, "policy", "", "license policy file; overrides the source's own (required for SchemaStore sources)")
	flags.StringVar(&f.ids, "ids", "", "ID overrides file ({\"<source URL>\": \"<id>\"}); overrides the source's own")
	flags.IntVar(&f.jobs, "jobs", prepare.DefaultJobs, "records prepared in parallel")
	flags.StringArrayVar(&f.refresh, "refresh", nil, "prepare this published schema ID anew even when its inputs did not change (repeatable)")
	flags.BoolVar(&f.refreshAll, "refresh-all", false, "prepare every published schema anew even when its inputs did not change")
	flags.BoolVar(&f.asJSON, "json", false, "print the result as JSON")
	markRequired(cmd, "source", "out")

	return cmd
}

func (a *app) prepare(cmd *cobra.Command, f *prepareFlags) error {
	if f.jobs < 1 {
		return fault.New(fault.Usage, "--jobs must be at least 1")
	}

	previous, err := loadState(f.state)
	if err != nil {
		return err
	}

	tool, err := bundle.FindTool(f.jsonschema, bundle.PinnedVersion)
	if err != nil {
		return fault.Wrap(fault.Usage, err, "--jsonschema")
	}

	res, err := prepare.Run(cmd.Context(), prepare.Options{
		Log: a.logf, Tool: tool, State: previous, SourceFile: f.source, OutDir: f.out,
		SnapshotDir: f.snapshotDir, PolicyFile: f.policy, IDsFile: f.ids, Jobs: f.jobs,
		Refresh: f.refresh, RefreshAll: f.refreshAll,
	})
	if err != nil {
		return fault.Wrap(fault.Internal, err, "prepare %s", f.source)
	}

	if f.asJSON {
		err = a.writeJSON(res)
	} else {
		err = a.writeText(prepareSummary(res))
	}

	if err != nil {
		return err
	}

	if err := res.Check(); err != nil {
		return fault.Wrap(fault.Integrity, err, "prepare %s", f.source)
	}

	return nil
}

// loadState reads --state; an empty flag or a missing file means that
// nothing has been published yet.
func loadState(path string) (*state.State, error) {
	if path == "" {
		return nil, nil //nolint:nilnil // no state flag means no previous publication, as a missing file does
	}

	s, err := state.Load(path)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "--state")
	}

	return s, nil
}

func prepareSummary(res *prepare.Result) string {
	var b strings.Builder

	t := res.Totals
	fmt.Fprintf(&b, "prepared %d entries (%d reusing their recorded artifact) from %d records into %s\n", t.Entries, t.Reused, t.Records, res.OutDir)
	fmt.Fprintf(&b, "records: %d included, %d excluded, %d pending review, %d failed\n", t.Included, t.Excluded, t.PendingReview, t.Failed)
	fmt.Fprintf(&b, "entries: %d bundled (%d behaviour-compared, %d structural-only), %d compacted only\n",
		t.Bundled, t.BehaviourCompared, t.StructuralOnly, t.CompactOnly)
	fmt.Fprintf(&b, "dropped fileMatch patterns: %d, versions not published: %d, ID collisions: %d\n",
		t.PatternsDropped, t.VersionsNotPublished, len(res.Collisions))

	for _, h := range res.Held {
		fmt.Fprintf(&b, "held: %s (%s)\n", h.ID, h.Reason)
	}

	for _, x := range res.Excluded {
		fmt.Fprintf(&b, "excluded: %s (rule %s)\n", x.ID, x.Rule)
	}

	for _, rec := range res.Regressions {
		fmt.Fprintf(&b, "regression: %s (%s)", rec.URL, rec.Status)

		if rec.Reason != "" {
			fmt.Fprintf(&b, " %s", rec.Reason)
		}

		b.WriteString("\n")
	}

	return b.String()
}

type diffFlags struct {
	prepared string
	state    string
	asJSON   bool
}

func (a *app) newDiffCmd() *cobra.Command {
	var f diffFlags

	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Compare a prepared set with the publisher state, without a registry",
		Long: "diff compares a prepared set with catalog/state.json of the last publication (a missing file means\n" +
			"none) exactly as publish would. --json prints {hasChanges, added, changed, metadataChanged, held,\n" +
			"excluded, unchanged}: the IDs whose entry is new, has a new artifact, or keeps its artifact with new\n" +
			"metadata; every schema the catalog keeps without refreshing it, as {id, reason}; the IDs an explicit\n" +
			"exclude rule removes now; and the number of other entries, held ones included. hasChanges tells\n" +
			"whether the catalog changes, which is when publishing makes a new revision; a hold alone never does.\n" +
			"The prepared set must have been prepared with this state (exit 2 otherwise). It never contacts a\n" +
			"registry and exits 0 whether or not anything changed.",
		Args: noArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return a.diff(&f)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.prepared, "prepared", "", "prepared directory written by prepare (required)")
	flags.StringVar(&f.state, "state", "", "publisher state of the last publication (catalog/state.json; required)")
	flags.BoolVar(&f.asJSON, "json", false, "print the result as JSON")
	markRequired(cmd, "prepared", "state")

	return cmd
}

func (a *app) diff(f *diffFlags) error {
	previous, err := loadState(f.state)
	if err != nil {
		return err
	}

	set, err := prepare.Load(f.prepared)
	if err != nil {
		return fault.Wrap(fault.Usage, err, "load prepared set")
	}

	plan, err := publish.NewPlan(set, previous)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "compare %s with the state", f.prepared)
	}

	d := plan.Diff()

	if f.asJSON {
		return a.writeJSON(d)
	}

	var b strings.Builder

	fmt.Fprintf(&b, "catalog changes: %t\n", d.HasChanges)

	held := make([]string, 0, len(d.Held))
	for _, h := range d.Held {
		held = append(held, h.ID+" ("+h.Reason+")")
	}

	for _, list := range []struct {
		name string
		ids  []string
	}{{"added", d.Added}, {"changed", d.Changed}, {"metadata changed", d.MetadataChanged}, {"held", held}, {"excluded", d.Excluded}} {
		fmt.Fprintf(&b, "%s: %d", list.name, len(list.ids))

		if len(list.ids) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(list.ids, ", "))
		}

		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "unchanged: %d\n", d.Unchanged)

	return a.writeText(b.String())
}

type publishFlags struct {
	prepared       string
	repository     string
	state          string
	stateOut       string
	now            string
	registryConfig string
	updateLatest   bool
	asJSON         bool
}

func (a *app) newPublishCmd() *cobra.Command {
	var f publishFlags

	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Publish a prepared set as a new catalog revision",
		Long: "publish compares the prepared set with --state (catalog/state.json of the last publication; a missing\n" +
			"file means none), which must be the state it was prepared with. When the catalog would not change it\n" +
			"is a noop that does not contact the registry; a hold alone changes nothing. Otherwise it uploads the\n" +
			"artifacts of new and changed content, reuses the recorded artifact of unchanged content, keeps held\n" +
			"schemas with their last entry, drops excluded ones (the state keeps their IDs reserved), records holds\n" +
			"and exclusions in the new state, and pushes the catalog index, which references every schema\n" +
			"artifact, tagged catalog-<YYYYMMDD.HHMM>, the UTC minute of the publication. The run resumes\n" +
			"the newest revision when that has the same content and refuses a minute that another catalog already\n" +
			"took. The new state goes to --state-out once the catalog and its tags exist. Published tags never\n" +
			"move; only --update-latest moves catalog-latest, last, and in a noop to the catalog of the state.\n" +
			"Plain HTTP, extra CA bundles and credentials files are configured per host in --registry-config.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.publish(cmd, &f)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.prepared, "prepared", "", "prepared directory written by prepare (required)")
	flags.StringVar(&f.repository, "repository", "", "OCI repository host[:port]/path (required)")
	flags.StringVar(&f.state, "state", "", "publisher state of the last publication (catalog/state.json); a missing file means none")
	flags.StringVar(&f.stateOut, "state-out", "", "file to write the state after the publication to (needs --state; may equal it)")
	flags.StringVar(&f.now, "now", "", "publish at this UTC minute YYYYMMDD.HHMM instead of the clock (tests and replays)")
	flags.StringVar(&f.registryConfig, "registry-config", "", "TOML file with [registries.\"host[:port]\"] plain_http, ca_file and credentials_file")
	flags.BoolVar(&f.updateLatest, "update-latest", false, "move catalog-latest to the published catalog")
	flags.BoolVar(&f.asJSON, "json", false, "print the result as JSON")
	markRequired(cmd, "prepared", "repository")

	return cmd
}

func (a *app) publish(cmd *cobra.Command, f *publishFlags) error {
	now := a.now()

	if f.now != "" {
		revision, err := calver.ParseRevision(f.now)
		if err != nil {
			return fault.Wrap(fault.Usage, err, "--now")
		}

		now = revision.Time()
	}

	if f.stateOut != "" && f.state == "" {
		return fault.New(fault.Usage, "--state-out needs --state: a new state is always derived from the previous one")
	}

	stateOut, err := checkStateOut(f.stateOut)
	if err != nil {
		return err
	}

	previous, err := loadState(f.state)
	if err != nil {
		return err
	}

	target, err := openRepository(f.repository, f.registryConfig)
	if err != nil {
		return err
	}

	res, err := publish.Run(cmd.Context(), target, publish.Options{
		Now: now, Log: a.logf, State: previous, StateOut: stateOut, PreparedDir: f.prepared,
		Repository: target.Name().String(), UpdateLatest: f.updateLatest,
	})
	if err != nil {
		return fault.Wrap(fault.Internal, err, "publish to %s", target.Name())
	}

	if f.asJSON {
		return a.writeJSON(res)
	}

	return a.writeText(res.String())
}

type latestFlags struct {
	repository     string
	state          string
	registryConfig string
	check          bool
	asJSON         bool
}

func (a *app) newLatestCmd() *cobra.Command {
	var f latestFlags

	cmd := &cobra.Command{
		Use:   "latest",
		Short: "Point catalog-latest at the catalog the publisher state records",
		Long: "latest moves catalog-latest to the catalog recorded in --state, without a prepared set. It first\n" +
			"checks that catalog-<revision> names exactly the recorded catalog index and that this catalog lists\n" +
			"exactly the recorded entries; otherwise it fails (exit 5) and moves nothing. --check only verifies.\n" +
			"It finishes a publication whose state was recorded but whose catalog-latest step failed, however\n" +
			"much upstream changed since.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.latest(cmd, &f)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.repository, "repository", "", "OCI repository host[:port]/path (required)")
	flags.StringVar(&f.state, "state", "", "publisher state that records the catalog (catalog/state.json; required, must exist)")
	flags.StringVar(&f.registryConfig, "registry-config", "", "TOML file with [registries.\"host[:port]\"] plain_http, ca_file and credentials_file")
	flags.BoolVar(&f.check, "check", false, "only verify the recorded catalog; leave catalog-latest alone")
	flags.BoolVar(&f.asJSON, "json", false, "print the result as JSON")
	markRequired(cmd, "repository", "state")

	return cmd
}

func (a *app) latest(cmd *cobra.Command, f *latestFlags) error {
	recorded, err := loadState(f.state)
	if err != nil {
		return err
	}

	if recorded == nil {
		return fault.New(fault.Usage, "--state %s does not exist: catalog-latest can only point at a recorded catalog", f.state)
	}

	target, err := openRepository(f.repository, f.registryConfig)
	if err != nil {
		return err
	}

	res, err := publish.Latest(cmd.Context(), target, recorded, publish.LatestOptions{
		Log: a.logf, Repository: target.Name().String(), CheckOnly: f.check,
	})
	if err != nil {
		return fault.Wrap(fault.Internal, err, "catalog-latest of %s", target.Name())
	}

	if f.asJSON {
		return a.writeJSON(res)
	}

	return a.writeText(fmt.Sprintf("catalog-latest: %s\nrepository: %s\nrevision: %s\ncatalog: %s (%d bytes)\n",
		res.Latest, res.Repository, res.Revision, res.CatalogDigest, res.CatalogSize))
}

// checkStateOut returns the absolute form of --state-out after checking
// that it names a regular file or nothing yet, so a bad path fails before
// anything is published.
func checkStateOut(path string) (string, error) {
	if path == "" {
		return "", nil
	}

	out, err := filepath.Abs(path)
	if err != nil {
		return "", fault.Wrap(fault.Usage, err, "--state-out")
	}

	info, err := os.Lstat(out)

	switch {
	case err == nil && !info.Mode().IsRegular():
		return "", fault.New(fault.Usage, "--state-out %s exists and is not a regular file", out)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return "", fault.Wrap(fault.Usage, err, "--state-out")
	}

	return out, nil
}
