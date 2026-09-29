package notes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ovineko/schepherd/internal/calver"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

const maxDiffBytes = 64 << 20

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Diff is the JSON of `schepherd-publisher diff --json`. HasChanges is true
// when the new catalog would differ from the recorded one; held records
// whose entries stay the same do not make a revision by themselves.
type Diff struct {
	Added           []string `json:"added"`
	Changed         []string `json:"changed"`
	MetadataChanged []string `json:"metadataChanged"`
	Held            []Held   `json:"held"`
	Excluded        []string `json:"excluded"`
	Unchanged       int      `json:"unchanged"`
	HasChanges      bool     `json:"hasChanges"`
}

var diffMembers = []string{"hasChanges", "added", "changed", "metadataChanged", "held", "excluded", "unchanged"}

// LoadDiff reads a diff result. Every member is required and no other is
// accepted, so a publisher that changed the format fails the run instead of
// having its changes or holds silently left out of the summary.
func LoadDiff(path string) (*Diff, error) {
	data, err := readLimited(path, maxDiffBytes, "diff result")
	if err != nil {
		return nil, err
	}

	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return nil, fault.Wrap(fault.Usage, err, "diff result %s", path)
	}

	for _, name := range diffMembers {
		if raw, ok := members[name]; !ok || string(raw) == "null" {
			return nil, fault.New(fault.Usage, "the diff result %s has no %q", path, name)
		}
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var d Diff
	if err := dec.Decode(&d); err != nil {
		return nil, fault.Wrap(fault.Usage, err, "diff result %s", path)
	}

	if d.Unchanged < 0 {
		return nil, fault.New(fault.Usage, "the diff result %s has a negative unchanged count", path)
	}

	return &d, nil
}

func readLimited(path string, limit int64, what string) ([]byte, error) {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "%s", what)
	}

	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "read the %s %s", what, path)
	}

	if int64(len(data)) > limit {
		return nil, fault.New(fault.Usage, "the %s %s is larger than %d bytes", what, path, limit)
	}

	return data, nil
}

// RunSummary renders the job summary of a run that prepared upstream commit
// upstream and compared it with the recorded state. Held records are listed
// whether or not the catalog changed, and so is the coverage of the
// preparation: how many upstream records it included, left pending review
// (by reason) and failed (each with its reason). publishing tells whether
// the run goes on to publish a changed catalog; a run that does not (one
// that is not on main) only reports what would change.
func RunSummary(d *Diff, cov *Coverage, checkedAt time.Time, upstream string, publishing bool) ([]byte, error) {
	switch {
	case !commitPattern.MatchString(upstream):
		return nil, fault.New(fault.Usage, "the upstream commit %q is not 40 lowercase hex digits", upstream)
	case cov == nil:
		return nil, fault.New(fault.Usage, "no prepare report: the summary shows what the preparation made of every upstream record")
	case cov.Source.Commit != upstream:
		return nil, fault.New(fault.Usage, "the prepare report describes upstream commit %q, not %s", cov.Source.Commit, upstream)
	}

	changes := Changes{
		Added: d.Added, Changed: d.Changed, MetadataChanged: d.MetadataChanged, Held: sortHeld(d.Held), Excluded: sorted(d.Excluded),
		Unchanged: d.Unchanged,
	}

	var b bytes.Buffer

	fmt.Fprintf(&b, "## Schema catalog update\n\nChecked at %s against upstream commit `%s`: %s.\n\n",
		checkedAt.UTC().Format(time.RFC3339), upstream, changes.counts())

	switch {
	case d.HasChanges && publishing:
		b.WriteString("The catalog changed, so this run publishes a new revision.\n")
	case d.HasChanges:
		b.WriteString("The catalog changed, but publication is not enabled for this run: nothing is published, committed or tagged.\n")
	case len(changes.Held) > 0:
		b.WriteString("The catalog did not change: nothing is published, committed or tagged. Held schemas alone do not make a new revision.\n")
	default:
		b.WriteString("The catalog did not change: nothing is published, committed or tagged.\n")
	}

	fmt.Fprintf(&b, "\nUpstream coverage: %s.\n", cov.counts())

	writeSummarySections(&b, &changes, cov.sections()...)

	return b.Bytes(), nil
}

// FinishSummary renders the job summary of a run that finishes the revision
// st records instead of preparing anything; missing names what that
// revision still lacks.
func FinishSummary(st *state.State, missing []string, checkedAt time.Time) ([]byte, error) {
	if st == nil {
		return nil, fault.New(fault.Usage, "no state: a finish run completes the revision recorded in the state")
	}

	revision, err := calver.ParseRevision(st.Catalog.Revision)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "state")
	}

	missingSpans := make([]string, 0, len(missing))
	for _, m := range missing {
		missingSpans = append(missingSpans, codeSpan(m))
	}

	changes := ChangesOf(st)

	var b bytes.Buffer

	fmt.Fprintf(&b, "## Schema catalog update\n\nChecked at %s: revision `%s` is recorded but not finished (missing: %s). "+
		"This run finishes it from the recorded state without preparing anything; upstream changes wait for the next run.\n\n",
		checkedAt.UTC().Format(time.RFC3339), revision, strings.Join(missingSpans, ", "))

	if commit := st.Source.Commit; commit != "" {
		fmt.Fprintf(&b, "Revision `%s` was prepared from upstream commit `%s`: %s.\n", revision, commit, changes.counts())
	} else {
		fmt.Fprintf(&b, "Revision `%s`: %s.\n", revision, changes.counts())
	}

	writeSummarySections(&b, &changes)

	return b.Bytes(), nil
}

// writeSummarySections writes the held and excluded schemas of c, then
// extra.
func writeSummarySections(b *bytes.Buffer, c *Changes, extra ...section) {
	held := make([]string, 0, len(c.Held))
	for _, h := range c.Held {
		held = append(held, "- "+codeSpan(h.ID)+": "+ReasonText(h.Reason))
	}

	excluded := make([]string, 0, len(c.Excluded))
	for _, id := range c.Excluded {
		excluded = append(excluded, "- "+codeSpan(id))
	}

	writeSections(b, MaxBytes-b.Len(), "###", append([]section{
		{title: "Held", intro: "Kept at their last published version:", lines: held},
		{title: "Excluded", intro: "Removed from the catalog by an explicit exclude rule in `sources/licenses.toml`:", lines: excluded},
	}, extra...))
}
