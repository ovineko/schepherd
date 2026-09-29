package prepare

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/bundle"
	"github.com/ovineko/schepherd/internal/publisher/ids"
	"github.com/ovineko/schepherd/internal/publisher/policy"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

// Report is report.json: one row per upstream record, in upstream order,
// and the held and excluded published schemas of prepared.json.
// GeneratedAt is the only value that differs between identical runs.
type Report struct {
	FormatVersion int            `json:"formatVersion"`
	GeneratedAt   string         `json:"generatedAt"`
	Source        Source         `json:"source"`
	Totals        Totals         `json:"totals"`
	Records       []RecordReport `json:"records"`
	Collisions    []Collision    `json:"collisions"`
	Held          []Hold         `json:"held"`
	Excluded      []Exclusion    `json:"excluded"`
}

// Totals counts upstream records by status (Records is their sum), the
// entries of the prepared set (Reused of them keep their recorded artifact),
// the published schemas held and excluded, dropped fileMatch patterns,
// version URLs that are not published, and how the other entries were
// produced and verified: Bundled is BehaviourCompared plus StructuralOnly.
// LicenseDetection counts the records automatic license detection decided,
// now or in the recorded decision a reused entry keeps.
type Totals struct {
	Records              int           `json:"records"`
	Entries              int           `json:"entries"`
	Reused               int           `json:"reused"`
	Held                 int           `json:"held"`
	Dropped              int           `json:"dropped"`
	Included             int           `json:"included"`
	Excluded             int           `json:"excluded"`
	PendingReview        int           `json:"pendingReview"`
	Failed               int           `json:"failed"`
	Regressions          int           `json:"regressions"`
	PatternsDropped      int           `json:"patternsDropped"`
	VersionsNotPublished int           `json:"versionsNotPublished"`
	Bundled              int           `json:"bundled"`
	CompactOnly          int           `json:"compactOnly"`
	BehaviourCompared    int           `json:"behaviourCompared"`
	StructuralOnly       int           `json:"structuralOnly"`
	LicenseDetection     LicenseTotals `json:"licenseDetection"`
}

// Verification methods of an included schema.
const (
	// VerifiedCompactOnly means the schema needed no bundling: its bytes are
	// the upstream bytes without insignificant whitespace.
	VerifiedCompactOnly = "compact-only"
	// VerifiedBehaviour means the bundle passed the structural checks and
	// agreed with the original schema on at least one JSON test instance.
	VerifiedBehaviour = "behaviour-compared"
	// VerifiedStructural means the bundle passed the structural checks only
	// (references and anchors kept, self-contained, compiles offline): no
	// JSON test instance existed to compare its behaviour on.
	VerifiedStructural = "structural-only"
)

// Verification tells how an included schema was verified. Valid and Invalid
// count the JSON test instances by the original schema's verdict, on each of
// which the bundle agreed; SkippedNonJSON counts instances in other formats.
type Verification struct {
	Method         string `json:"method"`
	Valid          int    `json:"valid"`
	Invalid        int    `json:"invalid"`
	SkippedNonJSON int    `json:"skippedNonJSON"`
}

func verificationOf(p *bundle.Prepared) Verification {
	v := Verification{
		Method: VerifiedCompactOnly, Valid: p.Checked.Valid, Invalid: p.Checked.Invalid, SkippedNonJSON: p.Checked.SkippedNonJSON,
	}

	switch {
	case !p.Bundled:
	case p.Checked.Agreed > 0:
		v.Method = VerifiedBehaviour
	default:
		v.Method = VerifiedStructural
	}

	return v
}

// RecordReport is the verdict for one upstream record. Records merged into
// the entry of an earlier record with the same URL name it in MergedInto.
// Rule is the license rule that decided and Detail the explanation. Reason is
// a failure code, or ReasonPatternUnsupported for an included record whose
// DroppedPatterns were left out of the entry. ID is set for included records
// and for published ones that are held or excluded; Held is then the hold
// reason. Reused marks an included record that keeps its recorded artifact.
// Regression marks an entry of a local source that was not prepared.
// Versions are the record's upstream version variants (name to URL), which
// are not published. Verification is set for included records that were
// prepared. LicenseDetections lists what automatic license detection found
// for the record's documents, whatever its status; a refused one carries the
// verdict "review" and its reason. SnapshotPath is the file of a SchemaStore
// record's schema in the upstream repository at the pinned commit.
type RecordReport struct {
	Name              string             `json:"name"`
	URL               string             `json:"url"`
	SnapshotPath      string             `json:"snapshotPath,omitempty"`
	Status            string             `json:"status"`
	ID                string             `json:"id,omitempty"`
	Held              string             `json:"held,omitempty"`
	Reused            bool               `json:"reused,omitempty"`
	Reason            string             `json:"reason,omitempty"`
	Rule              string             `json:"rule,omitempty"`
	Detail            string             `json:"detail,omitempty"`
	License           string             `json:"license,omitempty"`
	MergedInto        string             `json:"mergedInto,omitempty"`
	DroppedPatterns   []string           `json:"droppedPatterns,omitempty"`
	Versions          map[string]string  `json:"versions,omitempty"`
	Verification      *Verification      `json:"verification,omitempty"`
	LicenseDetections []policy.Detection `json:"licenseDetections,omitempty"`
	Regression        bool               `json:"regression,omitempty"`
}

// Collision reports an ID several sources wanted and how it was resolved.
type Collision struct {
	ID         string   `json:"id"`
	Resolution string   `json:"resolution"`
	Sources    []string `json:"sources"`
}

func (p *preparer) finish(outcomes []outcome) (*Result, error) {
	previous := p.reservations()
	sources := make([]string, 0, len(p.groups)+len(previous))

	for i, g := range p.groups {
		if outcomes[i].status == StatusIncluded {
			sources = append(sources, g.key)
		}
	}

	// Sources of the state take part even when they are not prepared now, so
	// their IDs stay reserved instead of passing to another source: publish
	// keeps held entries in the catalog and excluded IDs in the state.
	for source := range previous {
		sources = append(sources, source)
	}

	assigned, collisions, err := ids.Assign(sources, p.overrides, previous)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "assign schema IDs")
	}

	set, err := p.assemble(outcomes, assigned)
	if err != nil {
		return nil, err
	}

	ret := p.retention(outcomes, assigned, set)
	set.Document.Held, set.Document.Excluded = ret.held, ret.excluded

	if err := set.Document.validate(); err != nil {
		return nil, fault.Wrap(fault.Internal, err, "prepared set failed its own validation")
	}

	report := p.report(outcomes, assigned, collisions, set.Document.Source, ret)

	reportJSON, err := encodeJSON(report)
	if err != nil {
		return nil, err
	}

	if err := WriteSet(p.opts.OutDir, set, map[string][]byte{ReportFile: reportJSON}); err != nil {
		return nil, err
	}

	res := &Result{
		OutDir: p.opts.OutDir, Source: report.Source, Totals: report.Totals, Collisions: report.Collisions,
		Regressions: []RecordReport{}, Held: report.Held, Excluded: report.Excluded,
	}

	for _, rec := range report.Records {
		if rec.Regression {
			res.Regressions = append(res.Regressions, rec)
		}
	}

	p.logResult(res, report)

	return res, nil
}

func (p *preparer) logResult(res *Result, report *Report) {
	p.log("prepared %d entries (%d reused) from %d records: %d included, %d excluded, %d pending review, %d failed, %d regressions",
		res.Totals.Entries, res.Totals.Reused, res.Totals.Records, res.Totals.Included, res.Totals.Excluded, res.Totals.PendingReview,
		res.Totals.Failed, res.Totals.Regressions)

	if ld := res.Totals.LicenseDetection; ld.AutoAllowed+ld.Refused() > 0 {
		r := ld.AutoRefused
		p.log("license detection: %d record(s) auto-allowed, %d held for review (%d no-license, %d not-permissive, "+
			"%d not-asserted, %d unsupported-host, %d fetch-failed)",
			ld.AutoAllowed, ld.Refused(), r.NoLicense, r.NotPermissive, r.NotAsserted, r.UnsupportedHost, r.FetchFailed)
	}

	if res.Totals.StructuralOnly > 0 {
		p.log("warning: %d of %d bundled entries had no JSON test instance; only structural checks verified them: %s",
			res.Totals.StructuralOnly, res.Totals.Bundled, strings.Join(structuralOnly(report), ", "))
	}

	if len(res.Held) > 0 {
		held := make([]string, 0, len(res.Held))
		for _, h := range res.Held {
			held = append(held, h.ID+" ("+h.Reason+")")
		}

		p.log("warning: %d published schema(s) could not be refreshed; the catalog keeps their last entry and artifact: %s",
			len(held), strings.Join(held, ", "))
	}

	if len(res.Excluded) > 0 {
		excluded := make([]string, 0, len(res.Excluded))
		for _, x := range res.Excluded {
			excluded = append(excluded, x.ID+" (rule "+x.Rule+")")
		}

		p.log("warning: %d published schema(s) are excluded by an explicit rule and leave the catalog; their IDs stay reserved: %s",
			len(excluded), strings.Join(excluded, ", "))
	}
}

func structuralOnly(report *Report) []string {
	var ids []string

	for _, rec := range report.Records {
		if rec.Verification != nil && rec.Verification.Method == VerifiedStructural {
			ids = append(ids, rec.ID)
		}
	}

	slices.Sort(ids)

	return slices.Compact(ids)
}

// reservations is previousIDs plus a stand-in source for every published ID
// that an ID override gave a new ID to, so that the old ID, which publish
// keeps, cannot pass to another source.
func (p *preparer) reservations() map[string]string {
	previous := maps.Clone(p.previous)

	for source, id := range p.previous {
		if moved, ok := p.overrides[source]; ok && moved != id {
			previous[reservedSource+id] = id
		}
	}

	return previous
}

// retained is what happens to the published schemas of the state that the
// prepared entries do not carry: held or excluded. heldGroups and
// excludedGroups map the index of the group that still has the schema's ID
// to the hold reason or to true, for the report.
type retained struct {
	heldGroups     map[int]string
	excludedGroups map[int]bool
	held           []Hold
	excluded       []Exclusion
}

// retention holds every published schema of the state that is not an entry
// of the prepared set, unless an explicit exclude rule matches it: only that
// removes it from the catalog. The rule may match its source now, or the
// source or a dependency the state records for it, which are what the
// catalog would keep; so a takedown works also for a schema upstream no
// longer lists or whose new content failed before its dependencies were
// decided. A schema whose source no longer maps to its ID (upstream dropped
// the record, or an override moved it to another ID) is held as removed
// upstream.
func (p *preparer) retention(outcomes []outcome, assigned map[string]string, set *Set) retained {
	r := retained{heldGroups: map[int]string{}, excludedGroups: map[int]bool{}, held: []Hold{}, excluded: []Exclusion{}}
	if p.opts.State == nil {
		return r
	}

	byKey := make(map[string]int, len(p.groups))
	for i, g := range p.groups {
		byKey[g.key] = i
	}

	entries := make(map[string]bool, len(set.Document.Entries))
	for _, e := range set.Document.Entries {
		entries[e.ID] = true
	}

	for i := range p.opts.State.Schemas {
		rec := &p.opts.State.Schemas[i]
		if rec.Excluded() || entries[rec.ID] {
			continue
		}

		source := rec.Entry.Provenance.Source
		index, listed := byKey[source]
		current := listed && assigned[source] == rec.ID

		var (
			rule     string
			excluded bool
		)

		if current && outcomes[index].status == StatusExcluded {
			rule, excluded = outcomes[index].decision.RuleID, true
		} else {
			rule, excluded = p.recordedExclusion(rec)
		}

		switch {
		case excluded:
			r.excluded = append(r.excluded, Exclusion{ID: rec.ID, Rule: rule})
			if current {
				r.excludedGroups[index] = true
			}
		case !current:
			r.held = append(r.held, Hold{ID: rec.ID, Reason: state.HeldRemovedUpstream})
		default:
			reason := holdReason(&outcomes[index])
			r.held = append(r.held, Hold{ID: rec.ID, Reason: reason})
			r.heldGroups[index] = reason
		}
	}

	return r
}

// recordedExclusion decides the recorded source, dependencies and redirect
// targets of a published schema with the current policy and returns the
// exclude rule that removes it, if any. Exclude rules need no detection
// finding, so this contacts nothing.
func (p *preparer) recordedExclusion(rec *state.Schema) (string, bool) {
	d := p.decider.decideWith(rec.Entry.Provenance.Source, recordedDependencies(rec), recordedLookup(rec.License.Detections))

	return d.RuleID, d.Decision == policy.Exclude
}

// holdReason classifies why a published schema could not be refreshed.
func holdReason(o *outcome) string {
	switch {
	case o.status == StatusFailed && o.reason == ReasonFetchFailed:
		return state.HeldFetchFailed
	case o.status == StatusFailed:
		return state.HeldPrepareFailed
	}

	switch o.decision.AutoReason {
	case policy.RefusedFetchFailed, policy.RefusedUnsupportedHost:
		return state.HeldLicenseDetectionFailed
	case policy.RefusedNoLicense, policy.RefusedNotPermissive, policy.RefusedNotAsserted:
		return state.HeldLicenseRefused
	default:
		return state.HeldLicenseReview
	}
}

func (p *preparer) sourceInfo() Source {
	if p.src.local != nil {
		return Source{Kind: KindLocal, Name: p.src.local.name}
	}

	return Source{Kind: KindSchemaStore, Commit: p.snap.Commit, TarballDigest: p.snap.TarballDigest}
}

func (p *preparer) assemble(outcomes []outcome, assigned map[string]string) (*Set, error) {
	set := &Set{
		Document: Document{FormatVersion: FormatVersion, Recipe: Recipe, Source: p.sourceInfo(), Entries: []Entry{}},
		Schemas:  map[string][]byte{},
		Notices:  map[string][]byte{},
	}

	for i, g := range p.groups {
		o := &outcomes[i]
		if o.status != StatusIncluded {
			continue
		}

		id := assigned[g.key]

		var entry Entry

		if o.reused != nil {
			if id != o.reused.ID {
				return nil, fault.New(fault.Internal, "%s reuses the artifact of %s but was assigned ID %s", g.key, o.reused.ID, id)
			}

			entry = reusedEntry(g, o.reused)
		} else {
			entry = p.preparedEntry(set, g, o, id)
		}

		set.Document.Entries = append(set.Document.Entries, entry)
	}

	slices.SortFunc(set.Document.Entries, func(a, b Entry) int { return strings.Compare(a.ID, b.ID) })

	return set, nil
}

func (p *preparer) preparedEntry(set *Set, g *group, o *outcome, id string) Entry {
	entry := preparedEntryOf(g, o, id)

	if o.notice != nil {
		set.Notices[entry.Notice] = o.notice
	}

	set.Schemas[id] = o.prepared.Schema

	return entry
}

// preparedEntryOf is the prepared.json entry of an included record with new
// content.
func preparedEntryOf(g *group, o *outcome, id string) Entry {
	entry := Entry{
		ID: id, Name: g.name, Description: g.description, Dialect: o.prepared.Dialect,
		FileMatch: append([]string{}, g.patterns...), Schema: SchemaPath(id),
		ContentDigest: digest.FromBytes(o.prepared.Schema),
		Provenance: catalog.Provenance{
			Source: g.key, SourceDigest: o.sourceDigest, License: o.decision.License, Dependencies: dependencies(o.prepared),
		},
		License: o.basis,
	}

	if o.notice != nil {
		entry.Notice = NoticePathFor(o.notice)
	}

	return entry
}

// reusedEntry is the entry of a schema that keeps the recorded artifact of
// rec: content, provenance and license decision as recorded, metadata from
// upstream.
func reusedEntry(g *group, rec *state.Schema) Entry {
	prov := *rec.Entry.Provenance
	prov.Dependencies = slices.Clone(prov.Dependencies)
	artifact := rec.Entry.Artifact

	return Entry{
		ID: rec.ID, Name: g.name, Description: g.description, Dialect: rec.Entry.Dialect,
		FileMatch: append([]string{}, g.patterns...), ContentDigest: rec.ContentDigest, NoticeDigest: rec.NoticeDigest,
		Reused: &artifact, Provenance: prov, License: rec.License,
	}
}

func (p *preparer) report(outcomes []outcome, assigned map[string]string, collisions []ids.Collision, source Source, ret retained) *Report {
	r := &Report{
		FormatVersion: FormatVersion,
		GeneratedAt:   p.opts.Now().UTC().Format(time.RFC3339),
		Source:        source,
		Records:       make([]RecordReport, 0, len(p.records)),
		Collisions:    make([]Collision, 0, len(collisions)),
		Held:          ret.held,
		Excluded:      ret.excluded,
	}

	r.Totals.Held, r.Totals.Dropped = len(ret.held), len(ret.excluded)

	for _, c := range collisions {
		r.Collisions = append(r.Collisions, Collision{ID: c.ID, Resolution: c.Resolution, Sources: c.Sources})
	}

	for _, rec := range p.records {
		g, o := p.groups[rec.group], outcomes[rec.group]
		row := RecordReport{
			Name: rec.name, URL: rec.url, Status: o.status, Reason: o.reason, Rule: o.decision.RuleID,
			Detail: o.detail, MergedInto: rec.mergedInto, DroppedPatterns: rec.dropped, Versions: rec.versions,
			LicenseDetections: o.decision.Detections,
		}

		if g.snapshotFile != "" {
			row.SnapshotPath = snapshotSchemasPath + g.snapshotFile
		}

		if rec.problem != "" {
			row.Detail = "invalid upstream catalog entry: " + rec.problem
		}

		if reason, ok := ret.heldGroups[rec.group]; ok {
			row.ID, row.Held = assigned[g.key], reason
		} else if ret.excludedGroups[rec.group] {
			row.ID = assigned[g.key]
		}

		row.Regression = p.regression(g, o)

		if o.status == StatusIncluded {
			row.ID = assigned[g.key]
			row.License = o.decision.License
			row.Reused = o.reused != nil

			if o.prepared != nil {
				verification := verificationOf(o.prepared)
				row.Verification = &verification
			}

			if len(rec.dropped) > 0 {
				row.Reason = ReasonPatternUnsupported
				row.Detail = fmt.Sprintf("%d fileMatch pattern(s) dropped: Schepherd's matching dialect cannot express them", len(rec.dropped))
			}
		}

		r.Records = append(r.Records, row)
		r.Totals.count(row, rec)
		r.Totals.LicenseDetection.count(o.status, &o.decision)
	}

	for i := range p.groups {
		if o := outcomes[i]; o.status == StatusIncluded {
			r.Totals.Entries++

			if o.reused != nil {
				r.Totals.Reused++

				continue
			}

			switch verificationOf(o.prepared).Method {
			case VerifiedCompactOnly:
				r.Totals.CompactOnly++
			case VerifiedBehaviour:
				r.Totals.Bundled++
				r.Totals.BehaviourCompared++
			default:
				r.Totals.Bundled++
				r.Totals.StructuralOnly++
			}
		}
	}

	return r
}

func (t *Totals) count(row RecordReport, rec record) {
	t.Records++
	t.PatternsDropped += len(rec.dropped)
	t.VersionsNotPublished += len(rec.versions)

	switch row.Status {
	case StatusIncluded:
		t.Included++
	case StatusExcluded:
		t.Excluded++
	case StatusPendingReview:
		t.PendingReview++
	default:
		t.Failed++
	}

	if row.Regression {
		t.Regressions++
	}
}

// regression reports whether an entry of a local source that was never
// published was not prepared: a local source asks for every one of its
// entries, even over an explicit exclude rule. A published one is held or
// excluded instead, which never fails the run.
func (p *preparer) regression(g *group, o outcome) bool {
	_, published := p.previous[g.key]

	return g.required && !published && o.status != StatusIncluded
}

// parallel runs fn for 0..n-1 on at most jobs goroutines and returns the
// first error, after which no new index starts.
func parallel(ctx context.Context, n, jobs int, fn func(context.Context, int) error) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(jobs)

	for i := range n {
		if gctx.Err() != nil {
			break
		}

		g.Go(func() error {
			if gctx.Err() == nil {
				return fn(gctx, i)
			}

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return err //nolint:wrapcheck // fn's own error, which it classified
	}

	if err := ctx.Err(); err != nil {
		return fault.Wrap(fault.Canceled, err, "interrupted")
	}

	return nil
}
