// Package notes renders the release notes of a catalog revision from the
// state that recorded it, and the job summary of every weekly run. The state
// alone tells what the revision changed, so notes can be rendered long after
// the publication, and the JSON result of `schepherd-publisher publish` only
// serves as a cross-check.
//
// Schema names come from upstream and are untrusted. They are only ever
// placed inside code spans, where GitHub neither renders Markdown or HTML nor
// turns @names and #numbers into mentions and links.
package notes

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ovineko/schepherd/internal/calver"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

// MaxBytes keeps the notes below the 125,000 characters GitHub accepts for
// a release body; longer lists are cut with a count of what was left out.
const MaxBytes = 100_000

const maxResultBytes = 64 << 20

// Publish statuses of the result.
const (
	StatusNoop      = "noop"
	StatusPublished = "published"
	StatusResumed   = "resumed"
)

var (
	githubRepository = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	ociRepository    = regexp.MustCompile(`^[a-z0-9.-]+(?::[0-9]+)?(?:/[a-z0-9._-]+)+$`)
	backtickRun      = regexp.MustCompile("`+")
)

// Result is the part of the publish result the notes use.
type Result struct {
	Status          string   `json:"status"`
	Revision        string   `json:"revision"`
	CatalogDigest   string   `json:"catalogDigest"`
	Added           []string `json:"added"`
	Changed         []string `json:"changed"`
	MetadataChanged []string `json:"metadataChanged"`
	Held            []Held   `json:"held"`
	Excluded        []string `json:"excluded"`
	CatalogSize     int64    `json:"catalogSize"`
	Unchanged       int      `json:"unchanged"`
	UploadedSchemas int      `json:"uploadedSchemas"`
	ReusedSchemas   int      `json:"reusedSchemas"`
}

// LoadResult reads a publish result file.
func LoadResult(path string) (*Result, error) {
	data, err := readLimited(path, maxResultBytes, "publish result")
	if err != nil {
		return nil, err
	}

	var res Result
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fault.Wrap(fault.Usage, err, "publish result %s", path)
	}

	return &res, nil
}

// Changes are what a catalog revision did to each schema of the state: the
// IDs it added, changed (new artifact), changed in metadata only (same
// artifact) and excluded by an explicit rule, the schemas it keeps at their
// last published version, and how many entries it left as they were.
type Changes struct {
	Added           []string
	Changed         []string
	MetadataChanged []string
	Held            []Held
	Excluded        []string
	Unchanged       int
}

// Render returns the Markdown notes of the revision st records in the OCI
// repository.
func Render(st *state.State, repository string) ([]byte, error) {
	switch {
	case st == nil:
		return nil, fault.New(fault.Usage, "no state: the notes describe a publication recorded in the state")
	case !ociRepository.MatchString(repository):
		return nil, fault.New(fault.Usage, "invalid OCI repository %q", repository)
	}

	revision, err := calver.ParseRevision(st.Catalog.Revision)
	if err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "state")
	}

	changes := ChangesOf(st)
	named := func(id string) string {
		rec, _ := st.Lookup(id)

		return "- " + codeSpan(id) + " " + codeSpan(rec.Entry.Name)
	}

	var b bytes.Buffer

	writeHeader(&b, &changes, st, revision, repository)

	writeSections(&b, MaxBytes-b.Len(), "##", []section{
		{title: "Added", lines: mapLines(changes.Added, named)},
		{title: "Changed", lines: mapLines(changes.Changed, named)},
		{
			title: "Metadata updated", lines: mapLines(changes.MetadataChanged, named),
			intro: "Only the name, description, file patterns or dialect changed; the schema artifact stays the same.",
		},
		{
			title: "Held", lines: heldLines(st, changes.Held),
			intro: "These schemas stay in the catalog at their last published version until a later run can refresh them.",
		},
		{
			title: "Excluded", lines: mapLines(changes.Excluded, named),
			intro: "An explicit exclude rule in `sources/licenses.toml` removed these schemas from the catalog; their IDs stay reserved.",
		},
	})

	// Only the header can still overflow, through an absurd repository name;
	// `catalogbot release` would refuse such notes after the state commit.
	if b.Len() > MaxBytes {
		return nil, fault.New(fault.Usage, "the release notes would be %d bytes, more than %d", b.Len(), MaxBytes)
	}

	return b.Bytes(), nil
}

func heldLines(st *state.State, held []Held) []string {
	lines := make([]string, 0, len(held))

	for _, h := range held {
		rec, _ := st.Lookup(h.ID)
		line := "- " + codeSpan(h.ID) + " " + codeSpan(rec.Entry.Name) + ": " + ReasonText(h.Reason)

		if rec.HeldSinceRevision != "" {
			line += " (since " + codeSpan(rec.HeldSinceRevision) + ")"
		}

		lines = append(lines, line)
	}

	return lines
}

func mapLines(ids []string, line func(string) string) []string {
	lines := make([]string, 0, len(ids))
	for _, id := range ids {
		lines = append(lines, line(id))
	}

	return lines
}

// Check verifies that res, the JSON result of `schepherd-publisher publish`,
// describes the publication st records: a published or resumed revision with
// the same revision, catalog digest and changes.
func Check(res *Result, st *state.State) error {
	switch {
	case st == nil:
		return fault.New(fault.Usage, "no state: the notes describe a publication recorded in the state")
	case res.Status != StatusPublished && res.Status != StatusResumed:
		return fault.New(fault.Usage, "the publish result has status %q; only a published or resumed revision gets notes", res.Status)
	}

	if _, err := calver.ParseRevision(res.Revision); err != nil {
		return fault.Wrap(fault.Usage, err, "publish result")
	}

	if err := digest.Validate(res.CatalogDigest); err != nil {
		return fault.Wrap(fault.Usage, err, "publish result catalogDigest")
	}

	if res.Revision != st.Catalog.Revision || res.CatalogDigest != st.Catalog.Digest {
		return fault.New(fault.Integrity, "the publish result (revision %s, catalog %s) and the state (revision %s, catalog %s) describe different catalogs",
			res.Revision, res.CatalogDigest, st.Catalog.Revision, st.Catalog.Digest)
	}

	changes := ChangesOf(st)
	if !slices.Equal(sorted(res.Added), changes.Added) || !slices.Equal(sorted(res.Changed), changes.Changed) ||
		!slices.Equal(sorted(res.MetadataChanged), changes.MetadataChanged) || !slices.Equal(sortHeld(res.Held), changes.Held) ||
		!slices.Equal(sorted(res.Excluded), changes.Excluded) || res.Unchanged != changes.Unchanged {
		return fault.New(fault.Integrity, "the publish result lists other changes than the state records for revision %s", st.Catalog.Revision)
	}

	return nil
}

func sorted(ids []string) []string {
	out := append([]string{}, ids...)
	slices.Sort(out)

	return out
}

func writeHeader(b *bytes.Buffer, c *Changes, st *state.State, revision calver.Revision, repository string) {
	fmt.Fprintf(b, "Catalog revision `%s` of `%s`: %s.\n\n", revision, repository, c.counts())

	fmt.Fprintf(b, "- Catalog digest: `%s`\n", st.Catalog.Digest)
	fmt.Fprintf(b, "- Tag: `%s:%s`\n", repository, revision.Tag())

	if src := st.Source; src.Commit != "" && githubRepository.MatchString(src.Repository) {
		fmt.Fprintf(b, "- Upstream: [`%s@%s`](%s/commit/%s)\n", strings.TrimPrefix(src.Repository, "https://github.com/"), src.Commit[:12], src.Repository, src.Commit)
	} else if src.Commit != "" {
		fmt.Fprintf(b, "- Upstream: `%s` at `%s`\n", src.Repository, src.Commit)
	}

	b.WriteString("\n## How to pin\n\nResolve the revision tag once and paste the printed `[catalog]` section into `schepherd.toml`:\n\n")
	fmt.Fprintf(b, "```sh\nschepherd pin %s:%s\n```\n\n", repository, revision.Tag())
	b.WriteString("or pin the digest directly:\n\n")
	fmt.Fprintf(b, "```toml\n[catalog]\nrepository = %s\ndigest = %s\n```\n", strconv.Quote(repository), strconv.Quote(st.Catalog.Digest))
}

// counts sums up c in words; held schemas are part of the unchanged ones.
func (c *Changes) counts() string {
	return fmt.Sprintf("%d added, %d changed, %d metadata updated, %d excluded, %d unchanged (%d of them held)",
		len(c.Added), len(c.Changed), len(c.MetadataChanged), len(c.Excluded), c.Unchanged, len(c.Held))
}

// section is a heading with its count, an optional intro and one Markdown
// line per item. count is what the heading shows; zero means len(lines).
type section struct {
	title string
	intro string
	lines []string
	count int
}

func (s *section) heading(level string) string {
	count := s.count
	if count == 0 {
		count = len(s.lines)
	}

	text := fmt.Sprintf("\n%s %s (%d)\n\n", level, s.title, count)
	if s.intro != "" {
		text += s.intro + "\n\n"
	}

	return text
}

// size is what the section takes with every line listed.
func (s *section) size(level string) int {
	if len(s.lines) == 0 {
		return 0
	}

	n := len(s.heading(level))
	for _, line := range s.lines {
		n += len(line) + 1
	}

	return n
}

func moreLine(n int) string {
	return fmt.Sprintf("- and %d more not listed here\n", n)
}

// writeSections appends the non-empty sections in at most budget bytes. The
// budget is shared: a section that fits in an equal share of what the
// smaller ones leave is listed in full, and the larger ones split the rest
// evenly, so a long section never crowds out a later one.
func writeSections(b *bytes.Buffer, budget int, level string, sections []section) {
	order := make([]int, len(sections))
	for i := range order {
		order[i] = i
	}

	slices.SortStableFunc(order, func(i, j int) int {
		return cmp.Compare(sections[i].size(level), sections[j].size(level))
	})

	shares := make([]int, len(sections))
	left := max(budget, 0)

	for k, i := range order {
		shares[i] = min(sections[i].size(level), left/(len(order)-k))
		left -= shares[i]
	}

	for i := range sections {
		sections[i].write(b, shares[i], level)
	}
}

// write appends the section in at most budget bytes: every line when they
// all fit, otherwise as many as fit together with a last line counting the
// others. When not even the heading and that count fit, it writes nothing.
func (s *section) write(b *bytes.Buffer, budget int, level string) {
	if len(s.lines) == 0 {
		return
	}

	heading := s.heading(level)
	complete := s.size(level) <= budget

	if !complete && len(heading)+len(moreLine(len(s.lines))) > budget {
		return
	}

	b.WriteString(heading)
	used := len(heading)

	for i, line := range s.lines {
		if !complete && used+len(line)+1+len(moreLine(len(s.lines)-i-1)) > budget {
			b.WriteString(moreLine(len(s.lines) - i))

			return
		}

		b.WriteString(line + "\n")
		used += len(line) + 1
	}
}

// codeSpan wraps s in a CommonMark code span that shows it literally: the
// fence is longer than any backtick run inside, and a space pads contents
// that start or end with a backtick. Line breaks and other control
// characters become spaces.
func codeSpan(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '\u2028' || r == '\u2029' {
			return ' '
		}

		return r
	}, s)

	longest := 0
	for _, run := range backtickRun.FindAllString(s, -1) {
		longest = max(longest, len(run))
	}

	fence := strings.Repeat("`", longest+1)

	if s == "" {
		return fence + " " + fence
	}

	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") || (strings.HasPrefix(s, " ") && strings.HasSuffix(s, " ") && strings.TrimSpace(s) != "") {
		s = " " + s + " "
	}

	return fence + s + fence
}
