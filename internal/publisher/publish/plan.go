package publish

import (
	"bytes"
	"maps"
	"slices"
	"strings"

	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/prepare"
	"github.com/ovineko/schepherd/internal/publisher/state"
)

type change int

const (
	unchanged change = iota
	added
	changed
	metadataChanged
)

// item is one entry of the next catalog.
type item struct {
	// prepared is the entry of the prepared set; nil for a held schema.
	prepared *prepare.Entry
	// previous is nil for a schema the state does not know.
	previous *state.Schema
	// hold is the reason a held schema keeps its recorded entry.
	hold string
	// contentDigest and noticeDigest describe the content of the entry's
	// artifact; noticeDigest is empty when it has no notice.
	contentDigest string
	noticeDigest  string
	// entry has an empty Artifact until the prepared schema is packed,
	// unless reuse is set.
	entry  catalog.Entry
	reuse  bool
	change change
}

// Plan compares a prepared set with the state of the last publication. It
// needs no registry: artifacts of unchanged content come from the state,
// which is also why the comparison is exact without packing anything.
type Plan struct {
	set             *prepare.Set
	previous        *state.State
	items           []item
	excluded        []*state.Schema
	added           []string
	changed         []string
	metadataChanged []string
	unchanged       int
}

// Hold is a published schema the catalog keeps with its last entry and
// artifact, and why (one of state.HeldReasons).
type Hold struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// Diff is the machine-readable summary of a Plan (schepherd-publisher diff).
// Added, Changed and MetadataChanged are the IDs whose catalog entry is new,
// has a new artifact, or has the same artifact with new metadata; Excluded
// the IDs an explicit exclude rule removes from the catalog; Unchanged
// counts the other entries of the new catalog. Held lists every schema the
// new catalog keeps without refreshing it, whether or not it was held
// before; a held entry is unchanged. HasChanges tells whether the new
// catalog differs from the recorded one, which is exactly when publishing
// needs a new revision: a hold alone never does.
type Diff struct {
	HasChanges      bool     `json:"hasChanges"`
	Added           []string `json:"added"`
	Changed         []string `json:"changed"`
	MetadataChanged []string `json:"metadataChanged"`
	Held            []Hold   `json:"held"`
	Excluded        []string `json:"excluded"`
	Unchanged       int      `json:"unchanged"`
}

// NewPlan compares set with previous, the state of the last publication or
// nil before the first one.
//
// A schema whose prepared bytes and notice text have the digests the state
// records keeps its recorded artifact, whatever packing would produce now;
// a changed notice is new content, because the artifact must carry the
// notice that goes with the entry's license. A reused entry of the prepared
// set keeps the recorded artifact by definition. A held schema keeps its
// recorded entry, and an excluded one leaves the catalog while its record
// stays in the state. A schema the state records as excluded stays so
// unless the prepared set lists it as an entry again.
//
// The prepared set must have been prepared with the same state: every
// schema of the recorded catalog must be an entry, held or excluded, held
// schemas and reused entries must be in the recorded catalog, excluded ones
// must have a record, and a reused entry must carry the recorded artifact.
// Anything else is fault.Usage. An excluded schema the state already records
// as excluded stays so without a change, which makes publishing a set again
// against the state its publication wrote a noop.
func NewPlan(set *prepare.Set, previous *state.State) (*Plan, error) {
	if err := checkAccounts(set, previous); err != nil {
		return nil, err
	}

	p := &Plan{set: set, previous: previous, added: []string{}, changed: []string{}, metadataChanged: []string{}}
	prepared := make(map[string]*prepare.Entry, len(set.Document.Entries))

	for i := range set.Document.Entries {
		prepared[set.Document.Entries[i].ID] = &set.Document.Entries[i]
	}

	holds := make(map[string]string, len(set.Document.Held))
	for _, h := range set.Document.Held {
		holds[h.ID] = h.Reason
	}

	excluded := make(map[string]bool, len(set.Document.Excluded))
	for _, x := range set.Document.Excluded {
		excluded[x.ID] = true
	}

	ids := slices.Collect(maps.Keys(prepared))
	ids = append(ids, slices.Collect(maps.Keys(holds))...)
	slices.Sort(ids)

	for _, id := range ids {
		it := item{prepared: prepared[id], hold: holds[id]}

		if previous != nil {
			if rec, ok := previous.Lookup(id); ok {
				it.previous = rec
			}
		}

		if err := it.classify(set); err != nil {
			return nil, err
		}

		p.items = append(p.items, it)

		switch it.change {
		case added:
			p.added = append(p.added, id)
		case changed:
			p.changed = append(p.changed, id)
		case metadataChanged:
			p.metadataChanged = append(p.metadataChanged, id)
		case unchanged:
			p.unchanged++
		}
	}

	if previous != nil {
		for i := range previous.Schemas {
			if rec := &previous.Schemas[i]; excluded[rec.ID] && !rec.Excluded() {
				p.excluded = append(p.excluded, rec)
			}
		}
	}

	return p, nil
}

// checkAccounts checks that set was prepared with previous; see NewPlan.
func checkAccounts(set *prepare.Set, previous *state.State) error {
	doc := &set.Document
	listed := make(map[string]bool, len(doc.Entries)+len(doc.Held)+len(doc.Excluded))
	stale := func(format string, args ...any) error {
		return fault.New(fault.Usage, "the prepared set was not prepared with this publisher state: "+format+
			"; prepare again with the state of the last publication", args...)
	}

	published := func(id string) *state.Schema {
		if rec, ok := lookup(previous, id); ok && !rec.Excluded() {
			return rec
		}

		return nil
	}

	for i := range doc.Entries {
		e := &doc.Entries[i]
		listed[e.ID] = true

		if e.Reused == nil {
			continue
		}

		rec := published(e.ID)
		if rec == nil || *e.Reused != rec.Entry.Artifact || e.ContentDigest != rec.ContentDigest || e.NoticeDigest != rec.NoticeDigest {
			return stale("%s reuses an artifact the state does not record for it", e.ID)
		}
	}

	for _, id := range heldIDs(doc.Held) {
		listed[id] = true

		if published(id) == nil {
			return stale("%s is held, but the recorded catalog does not list it", id)
		}
	}

	// An exclusion the state already records is what the set's own
	// publication leaves behind; publishing the set again changes nothing.
	for _, id := range excludedIDs(doc.Excluded) {
		listed[id] = true

		if _, known := lookup(previous, id); !known {
			return stale("%s is excluded, but the state has no record of it", id)
		}
	}

	if previous == nil {
		return nil
	}

	for i := range previous.Schemas {
		if rec := &previous.Schemas[i]; !rec.Excluded() && !listed[rec.ID] {
			return stale("%s of the recorded catalog is neither an entry nor held nor excluded", rec.ID)
		}
	}

	return nil
}

// lookup finds the record of id in s, which may be nil.
func lookup(s *state.State, id string) (*state.Schema, bool) {
	if s == nil {
		return nil, false
	}

	return s.Lookup(id)
}

func heldIDs(holds []prepare.Hold) []string {
	out := make([]string, 0, len(holds))
	for _, h := range holds {
		out = append(out, h.ID)
	}

	return out
}

func excludedIDs(exclusions []prepare.Exclusion) []string {
	out := make([]string, 0, len(exclusions))
	for _, x := range exclusions {
		out = append(out, x.ID)
	}

	return out
}

func (it *item) classify(set *prepare.Set) error {
	switch {
	case it.prepared == nil:
		it.entry, it.reuse, it.change = it.previous.Entry, true, unchanged
		it.contentDigest, it.noticeDigest = it.previous.ContentDigest, it.previous.NoticeDigest

		return nil
	case it.prepared.Reused != nil:
		it.contentDigest, it.noticeDigest = it.prepared.ContentDigest, it.prepared.NoticeDigest
	default:
		it.contentDigest = it.prepared.ContentDigest
		if it.prepared.Notice != "" {
			it.noticeDigest = digest.FromBytes(set.Notices[it.prepared.Notice])
		}
	}

	it.entry = entryOf(it.prepared)

	switch {
	case it.previous == nil:
		it.change = added

		return nil
	case it.contentDigest != it.previous.ContentDigest || it.noticeDigest != it.previous.NoticeDigest:
		it.change = changed
		if it.previous.Excluded() {
			it.change = added
		}

		return nil
	}

	it.entry.Artifact, it.reuse = it.previous.Entry.Artifact, true

	// A schema that returns after an exclusion is added again; it keeps its
	// recorded artifact when its content is the same.
	if it.previous.Excluded() {
		it.change = added

		return nil
	}

	same, err := sameEntry(&it.entry, &it.previous.Entry)
	if err != nil {
		return err
	}

	if same {
		it.change = unchanged
	} else {
		it.change = metadataChanged
	}

	return nil
}

// entryOf returns the catalog entry of a prepared schema without its
// artifact.
func entryOf(e *prepare.Entry) catalog.Entry {
	entry := catalog.Entry{ID: e.ID, Name: e.Name, Description: e.Description, Dialect: e.Dialect}

	if len(e.FileMatch) > 0 {
		entry.FileMatch = slices.Clone(e.FileMatch)
	}

	prov := e.Provenance
	prov.Dependencies = slices.Clone(prov.Dependencies)
	entry.Provenance = &prov

	return entry
}

func sameEntry(a, b *catalog.Entry) (bool, error) {
	x, err := catalog.MarshalEntry(a)
	if err != nil {
		return false, fault.Wrap(fault.Internal, err, "compare entries")
	}

	y, err := catalog.MarshalEntry(b)
	if err != nil {
		return false, fault.Wrap(fault.Internal, err, "compare entries")
	}

	return bytes.Equal(x, y), nil
}

// HasChanges reports whether the new catalog differs from the recorded one,
// which is when publishing the plan needs a new revision.
func (p *Plan) HasChanges() bool {
	return len(p.added)+len(p.changed)+len(p.metadataChanged)+len(p.excluded) > 0
}

// Diff summarizes the plan.
func (p *Plan) Diff() Diff {
	return Diff{
		HasChanges: p.HasChanges(), Added: slices.Clone(p.added), Changed: slices.Clone(p.changed),
		MetadataChanged: slices.Clone(p.metadataChanged), Held: p.held(), Excluded: p.excludedIDs(), Unchanged: p.unchanged,
	}
}

func (p *Plan) held() []Hold {
	out := []Hold{}

	for i := range p.items {
		if it := &p.items[i]; it.hold != "" {
			out = append(out, Hold{ID: it.entry.ID, Reason: it.hold})
		}
	}

	return out
}

func (p *Plan) excludedIDs() []string {
	out := make([]string, 0, len(p.excluded))
	for _, rec := range p.excluded {
		out = append(out, rec.ID)
	}

	return out
}

// nextState returns the state after the plan was published as revision
// with the catalog index of the given digest and size. Every item must
// have its artifact by then.
func (p *Plan) nextState(revision, catalogDigest string, catalogSize int64) *state.State {
	src := p.set.Document.Source

	next := &state.State{
		FormatVersion: state.FormatVersion,
		Source:        state.Source{Kind: src.Kind, Name: src.Name, Commit: src.Commit, TarballDigest: src.TarballDigest},
		Recipe:        p.set.Document.Recipe,
		Catalog:       state.Catalog{Revision: revision, Digest: catalogDigest, Size: catalogSize},
		Schemas:       make([]state.Schema, 0, len(p.items)+len(p.excluded)),
	}

	if src.Kind == state.KindSchemaStore {
		next.Source.Repository = state.SchemaStoreRepository
	}

	for i := range p.items {
		next.Schemas = append(next.Schemas, p.items[i].record(revision))
	}

	dropped := map[string]bool{}

	for _, prev := range p.excluded {
		rec := *prev
		rec.ExcludedRevision, rec.HeldSinceRevision, rec.HeldReason = revision, "", ""
		next.Schemas = append(next.Schemas, rec)
		dropped[rec.ID] = true
	}

	if p.previous != nil {
		inCatalog := make(map[string]bool, len(p.items))
		for i := range p.items {
			inCatalog[p.items[i].entry.ID] = true
		}

		for i := range p.previous.Schemas {
			if rec := p.previous.Schemas[i]; rec.Excluded() && !inCatalog[rec.ID] && !dropped[rec.ID] {
				next.Schemas = append(next.Schemas, rec)
			}
		}
	}

	slices.SortFunc(next.Schemas, func(a, b state.Schema) int { return strings.Compare(a.ID, b.ID) })

	return next
}

// record returns the state record of the item after it was published as
// revision.
func (it *item) record(revision string) state.Schema {
	var rec state.Schema
	if it.previous != nil {
		rec = *it.previous
	}

	rec.ID, rec.Entry = it.entry.ID, it.entry
	rec.ContentDigest, rec.NoticeDigest = it.contentDigest, it.noticeDigest

	if it.prepared != nil {
		rec.License = it.prepared.License
	}

	switch it.change {
	case added:
		rec.FirstRevision, rec.ArtifactRevision, rec.LastChangedRevision, rec.ExcludedRevision = revision, "", revision, ""
	case changed:
		rec.ArtifactRevision, rec.LastChangedRevision = "", revision
	case metadataChanged:
		rec.ArtifactRevision, rec.LastChangedRevision = rec.ArtifactSince(), revision
	case unchanged:
	}

	switch {
	case it.hold == "":
		rec.HeldSinceRevision, rec.HeldReason = "", ""
	case rec.Held():
		rec.HeldReason = it.hold
	default:
		rec.HeldSinceRevision, rec.HeldReason = revision, it.hold
	}

	return rec
}

// candidate returns the catalog of the plan without its revision.
func (p *Plan) candidate() *catalog.Catalog {
	c := &catalog.Catalog{FormatVersion: catalog.FormatVersion, Schemas: make([]catalog.Entry, 0, len(p.items))}
	for i := range p.items {
		c.Schemas = append(c.Schemas, p.items[i].entry)
	}

	return c
}
