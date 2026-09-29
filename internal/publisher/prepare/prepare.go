// Package prepare turns an upstream schema source into a prepared set: a
// directory with one verified, self-contained schema per catalog entry, the
// notices that travel with them and a machine-readable report that accounts
// for every upstream record. prepared.json pins every input: the upstream
// commit and tarball, the recipe with the bundler version, and per entry the
// source and dependency digests and the license decision.
//
// Preparation is deterministic: the same source, snapshot, dependencies,
// policy, ID overrides, publisher state, bundler and answers of automatic
// license detection produce byte-identical prepared.json, schemas and
// notices. Only report.json carries a timestamp.
//
// A published schema whose source and dependencies still have the digests
// the publisher state records, and are still served directly or through a
// redirect to the recorded target, is not prepared again: its entry reuses
// the recorded artifact and license decision (docs/publishing.md), so a week
// without upstream changes asks automatic license detection nothing. A
// published schema that cannot be refreshed is held: the next catalog keeps
// its last entry. Only an explicit exclude rule removes one.
package prepare

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/match"
	"github.com/ovineko/schepherd/internal/publisher/bundle"
	"github.com/ovineko/schepherd/internal/publisher/httpfetch"
	"github.com/ovineko/schepherd/internal/publisher/ids"
	"github.com/ovineko/schepherd/internal/publisher/licensedetect"
	"github.com/ovineko/schepherd/internal/publisher/policy"
	"github.com/ovineko/schepherd/internal/publisher/state"
	"github.com/ovineko/schepherd/internal/publisher/upstream"
)

// DefaultJobs is the number of records prepared in parallel when
// Options.Jobs is not positive.
const DefaultJobs = 4

// Statuses of an upstream record.
const (
	StatusIncluded      = "included"
	StatusExcluded      = "excluded"
	StatusPendingReview = "pending-review"
	StatusFailed        = "failed"
)

// Failure reasons added by preparation to the bundle package's reasons.
const (
	// ReasonFetchFailed means the root document could not be obtained, or a
	// dependency could not be downloaded.
	ReasonFetchFailed = "fetch-failed"
	// ReasonDuplicateKeys means the root document has duplicate object keys.
	ReasonDuplicateKeys = "duplicate-keys"
	// ReasonInvalidMetadata means the name, description, dialect, patterns
	// or provenance of the record would be rejected by the catalog format.
	ReasonInvalidMetadata = "invalid-metadata"
	// ReasonPatternUnsupported marks fileMatch patterns dropped from an
	// entry because Schepherd's matching dialect cannot express them.
	ReasonPatternUnsupported = "pattern-unsupported"
	// ReasonTooLarge means the prepared schema or its notice exceeds the
	// artifact limits clients apply by default.
	ReasonTooLarge = "too-large"
)

const snapshotSchemasPath = "src/schemas/json/"

// Options configures Run.
type Options struct {
	// Log receives progress messages; nil discards them.
	Log func(format string, args ...any)
	// Now stamps report.json; nil means time.Now.
	Now func() time.Time
	// Tool is the verified bundler (required).
	Tool *bundle.Tool
	// Fetcher performs every network request, the SchemaStore tarball
	// download included. Nil builds one from the source file's fetch
	// settings (HTTPS to public addresses only unless the source allows
	// more).
	Fetcher *httpfetch.Fetcher
	// Transport, when set, carries the requests of a fetcher built from the
	// source file's fetch settings in place of the network (see
	// httpfetch.Policy.Transport). Tests serve public host names with it.
	Transport http.RoundTripper
	// LicenseDetector performs the automatic license detection the policy's
	// [auto] section enables; nil builds one limited to the policy's hosts,
	// with the GitHub token from GITHUB_TOKEN. Tests point it at local
	// services.
	LicenseDetector *licensedetect.Detector
	// State is the publisher state of the last publication
	// (catalog/state.json), nil before the first. It keeps IDs stable,
	// supplies the artifacts and license decisions of unchanged schemas and
	// names the published schemas the prepared set must account for: each
	// one is prepared or reused, held (kept with its last entry, see
	// state.HeldReasons) or, by an explicit exclude rule only, excluded.
	State *state.State
	// Refresh names schema IDs of the state that are prepared again even
	// when their source and dependencies did not change, so that a recipe,
	// bundler or policy change reaches them; RefreshAll does so for all.
	Refresh    []string
	RefreshAll bool
	// SourceFile is the source description (required).
	SourceFile string
	// OutDir receives the prepared set; it must not exist or be empty.
	OutDir string
	// SnapshotDir is where the SchemaStore snapshot lives: an existing
	// snapshot is reused, a missing directory is created by downloading the
	// pinned tarball into it. Empty means a temporary directory.
	SnapshotDir string
	// PolicyFile overrides the source file's license policy. SchemaStore
	// sources need one.
	PolicyFile string
	// IDsFile overrides the source file's ID overrides.
	IDsFile string
	// Jobs bounds records prepared in parallel; DefaultJobs when not positive.
	Jobs int
}

// Result summarizes a run. Held and Excluded are those of prepared.json: the
// published schemas the next catalog keeps unrefreshed, and those an
// explicit exclude rule removes. Regressions lists entries of a local source
// that were neither prepared nor published; Check turns them into an error.
// Nothing else is a regression: a published schema that fails is held, a
// new upstream record that fails stays out.
type Result struct {
	OutDir      string         `json:"outDir"`
	Source      Source         `json:"source"`
	Totals      Totals         `json:"totals"`
	Regressions []RecordReport `json:"regressions"`
	Collisions  []Collision    `json:"collisions"`
	Held        []Hold         `json:"held"`
	Excluded    []Exclusion    `json:"excluded"`
}

// Check returns a fault.Integrity error when an entry of a local source that
// was never published was not prepared: a local source asks for every one
// of its entries.
func (r *Result) Check() error {
	if len(r.Regressions) == 0 {
		return nil
	}

	names := make([]string, 0, len(r.Regressions))
	for _, rec := range r.Regressions {
		names = append(names, fmt.Sprintf("%s (%s)", rec.URL, statusText(rec)))
	}

	return fault.New(fault.Integrity, "%d entries of the local source were not prepared: %s", len(names), strings.Join(names, ", "))
}

func statusText(rec RecordReport) string {
	if rec.Reason != "" {
		return rec.Status + ": " + rec.Reason
	}

	return rec.Status
}

// record is one upstream record. problem explains why a malformed entry of
// the upstream catalog cannot be used.
type record struct {
	versions   map[string]string
	name       string
	url        string
	mergedInto string
	problem    string
	dropped    []string
	group      int
}

// group is one prospective catalog entry: every upstream record that shares
// its source URL.
type group struct {
	// key is the source identity: provenance.source and the ID key.
	key string
	// retrieve is the URI the root is retrieved and resolved from.
	retrieve string
	// snapshotFile is the SchemaStore file name of the root document, if
	// any; its upstream tests are only instances when fragment is empty.
	snapshotFile string
	// fragment is the part of the source URL after "#": the entry is the
	// subschema it names, not the document.
	fragment    string
	name        string
	description string
	localFile   string
	failReason  string
	failDetail  string
	patterns    []string
	instances   []instanceFile
	// required marks the entries of a local source, which must all be
	// prepared.
	required bool
}

// outcome is the verdict for one group. An included group has either
// prepared content or, when reused is set, the recorded artifact of that
// state record.
type outcome struct {
	prepared     *bundle.Prepared
	reused       *state.Schema
	notice       []byte
	decision     policy.Decision
	basis        state.LicenseDecision
	status       string
	reason       string
	detail       string
	sourceDigest string
}

type preparer struct {
	src            *source
	snap           *upstream.Snapshot
	decider        *decider
	provider       *provider
	overrides      map[string]string
	previous       map[string]string
	claimed        map[string]string
	refresh        map[string]bool
	recordedTarget map[string]policy.Finding
	log            func(string, ...any)
	groups         []*group
	records        []record
	upstreamNotice string
	opts           Options
}

// Run prepares the source described by opts.SourceFile into opts.OutDir.
//
// Every upstream record ends up included, excluded, pending-review or failed
// (see docs/publishing.md). A failed record never stops the run; Result.Check
// reports failures in the required set. Configuration problems are
// fault.Usage, an unusable snapshot fault.Integrity, cancellation
// fault.Canceled.
func Run(ctx context.Context, opts Options) (*Result, error) {
	p, cleanup, err := newPreparer(ctx, opts)
	if err != nil {
		return nil, err
	}

	defer cleanup()

	outcomes := make([]outcome, len(p.groups))

	err = parallel(ctx, len(p.groups), jobsOrDefault(opts.Jobs), func(ctx context.Context, i int) error {
		o, err := p.process(ctx, p.groups[i])
		if err != nil {
			return err
		}

		outcomes[i] = o

		return nil
	})
	if err != nil {
		return nil, err
	}

	return p.finish(outcomes)
}

func jobsOrDefault(jobs int) int {
	if jobs <= 0 {
		return DefaultJobs
	}

	return jobs
}

func newPreparer(ctx context.Context, opts Options) (*preparer, func(), error) {
	noop := func() {}

	switch {
	case opts.Tool == nil:
		return nil, noop, fault.New(fault.Usage, "prepare needs the JSON Schema bundler")
	case opts.SourceFile == "":
		return nil, noop, fault.New(fault.Usage, "prepare needs a source file")
	case opts.OutDir == "":
		return nil, noop, fault.New(fault.Usage, "prepare needs an output directory")
	}

	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}

	out, err := filepath.Abs(opts.OutDir)
	if err != nil {
		return nil, noop, fault.Wrap(fault.Usage, err, "output directory")
	}

	if err := checkOutDir(out); err != nil {
		return nil, noop, err
	}

	opts.OutDir = out

	src, err := loadSource(opts.SourceFile)
	if err != nil {
		return nil, noop, err
	}

	p := &preparer{opts: opts, src: src, log: opts.Log, previous: previousIDs(opts.State)}

	if p.refresh, err = refreshSet(opts.State, opts.Refresh); err != nil {
		return nil, noop, err
	}

	if err := p.loadPolicyAndIDs(); err != nil {
		return nil, noop, err
	}

	fetcher := opts.Fetcher
	if fetcher == nil {
		settings := src.fetch
		settings.AllowRedirect = p.vetRedirect
		settings.Transport = opts.Transport
		fetcher = httpfetch.New(settings)
	}

	cleanup := noop

	if src.local != nil {
		err = p.setupLocal(fetcher)
	} else {
		cleanup, err = p.setupSchemaStore(ctx, fetcher)
	}

	if err != nil {
		return nil, noop, err
	}

	if err := p.resolveOverrides(); err != nil {
		cleanup()

		return nil, noop, err
	}

	p.recordedTarget = p.recordedTargetFindings()
	p.provider.redirectAllowed = func(target string) bool { return p.redirectDecision(target).Decision == policy.Allow }

	return p, cleanup, nil
}

// vetRedirect keeps the fetcher from contacting a redirect target that the
// license policy does not allow; the refusal carries the policy decision so
// the record is held for review rather than reported as a fetch failure.
func (p *preparer) vetRedirect(target *url.URL) error {
	if d := p.redirectDecision(target.String()); d.Decision != policy.Allow {
		return &redirectRefusedError{target: target.Redacted(), decision: d}
	}

	return nil
}

// redirectDecision decides a redirect target before it is contacted: on the
// findings detection already has and, for a target that served a published
// schema the run may reuse, on the findings the state recorded for it. A
// reused schema asks detection nothing, so without them its recorded target
// would stay undecided.
func (p *preparer) redirectDecision(target string) policy.Decision {
	live := p.decider.lookup()
	if len(p.recordedTarget) == 0 {
		return p.decider.singleWith(target, live)
	}

	return p.decider.singleWith(target, func(url string) (policy.Finding, bool) {
		if live != nil {
			if f, ok := live(url); ok {
				return f, true
			}
		}

		f, ok := p.recordedTarget[url]

		return f, ok
	})
}

// recordedTargetFindings collects, by URL, the findings the state recorded
// for the redirect targets of the published schemas the run may reuse.
func (p *preparer) recordedTargetFindings() map[string]policy.Finding {
	if p.opts.State == nil || p.opts.RefreshAll {
		return nil
	}

	findings := map[string]policy.Finding{}

	for i := range p.opts.State.Schemas {
		rec := &p.opts.State.Schemas[i]
		if rec.Excluded() || p.refresh[rec.ID] {
			continue
		}

		for _, r := range rec.License.Redirects {
			target := p.decider.canon(r.Target)

			for j := range rec.License.Detections {
				if det := &rec.License.Detections[j]; det.URL == target {
					if _, ok := findings[target]; !ok {
						findings[target] = recordedFinding(det)
					}
				}
			}
		}
	}

	return findings
}

// resolveOverrides rewrites the keys of the ID overrides to the source
// identities they name, so that an override written with any spelling of a
// SchemaStore URL (the record's upstream URL, a json.schemastore.org or raw
// repository alias) applies to the canonical source. An override that names
// neither an upstream record nor a source of the state has no effect and is
// logged.
func (p *preparer) resolveOverrides() error {
	keys := make(map[string]string, len(p.groups))
	for _, g := range p.groups {
		if _, ok := keys[normalizeURI(g.key)]; !ok {
			keys[normalizeURI(g.key)] = g.key
		}
	}

	resolved := make(map[string]string, len(p.overrides))
	written := make(map[string]string, len(p.overrides))

	for _, raw := range slices.Sorted(maps.Keys(p.overrides)) {
		id, key := p.overrides[raw], p.decider.canon(raw)

		if known, ok := keys[normalizeURI(key)]; ok {
			key = known
		} else if _, ok := p.previous[key]; !ok {
			p.log("warning: the ID override for %s names no upstream record and no source of the state; it has no effect", raw)
		}

		if other, ok := resolved[key]; ok && other != id {
			return fault.New(fault.Usage, "the ID overrides for %s and %s name the same source %s with different IDs %q and %q",
				written[key], raw, key, other, id)
		}

		resolved[key], written[key] = id, raw
	}

	p.overrides = resolved
	p.claimed = make(map[string]string, len(resolved))

	for key, id := range resolved {
		p.claimed[id] = key
	}

	return nil
}

// reservedSource prefixes the stand-in source of a state ID whose source
// has another ID in the state as well, which an override that renamed a
// published schema leaves behind. Publish keeps such an ID in the catalog,
// so the stand-in keeps it reserved instead of letting another source
// derive it. No upstream record has such a source.
const reservedSource = "urn:schepherd:reserved-id:"

// previousIDs maps every source of the state to its ID; when a source has
// several, one whose upstream record still maps to it wins.
func previousIDs(s *state.State) map[string]string {
	previous := map[string]string{}
	if s == nil {
		return previous
	}

	stale := map[string]bool{}

	for _, rec := range s.Schemas {
		source := rec.Entry.Provenance.Source
		stale[rec.ID] = rec.HeldReason == state.HeldRemovedUpstream

		current, taken := previous[source]

		switch {
		case !taken:
			previous[source] = rec.ID
		case stale[current] && !stale[rec.ID]:
			previous[reservedSource+current], previous[source] = current, rec.ID
		default:
			previous[reservedSource+rec.ID] = rec.ID
		}
	}

	return previous
}

// refreshSet checks that every ID to refresh is one of the state.
func refreshSet(s *state.State, refresh []string) (map[string]bool, error) {
	set := make(map[string]bool, len(refresh))

	for _, id := range refresh {
		if s == nil {
			return nil, fault.New(fault.Usage, "cannot refresh %q: there is no publisher state", id)
		}

		if _, ok := s.Lookup(id); !ok {
			return nil, fault.New(fault.Usage, "cannot refresh %q: the publisher state has no schema with this ID", id)
		}

		set[id] = true
	}

	return set, nil
}

func (p *preparer) loadPolicyAndIDs() error {
	policyFile := firstNonEmpty(p.opts.PolicyFile, p.src.policy)
	idsFile := firstNonEmpty(p.opts.IDsFile, p.src.ids)

	if policyFile == "" && p.src.upstream != nil {
		return fault.New(fault.Usage, "a SchemaStore source needs a license policy file")
	}

	p.decider = &decider{}

	if policyFile != "" {
		pol, err := policy.Load(policyFile)
		if err != nil {
			return fault.Wrap(fault.Usage, err, "load the license policy")
		}

		p.decider.policy = pol

		if err := p.decider.enableDetection(p.opts.LicenseDetector, jobsOrDefault(p.opts.Jobs), p.log); err != nil {
			return err
		}
	}

	p.overrides = map[string]string{}

	if idsFile != "" {
		overrides, err := ids.LoadOverrides(idsFile)
		if err != nil {
			return fault.Wrap(fault.Usage, err, "load the ID overrides")
		}

		p.overrides = overrides
	}

	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}

func (p *preparer) setupLocal(fetcher *httpfetch.Fetcher) error {
	set := p.src.local
	files := map[string]string{}
	declared := map[string]string{}

	for i := range set.entries {
		e := &set.entries[i]

		if e.file != "" {
			files[normalizeURI(e.url)] = e.file
		}

		if e.license != "" {
			declared[normalizeURI(e.url)] = e.license
		}

		if e.id != "" {
			if other, ok := p.overrides[e.url]; ok && other != e.id {
				return fault.New(fault.Usage, "%s has id %q in the source file but %q in the ID overrides", e.url, e.id, other)
			}

			p.overrides[e.url] = e.id
		}

		g := &group{
			key: e.url, retrieve: e.url, name: e.name, description: e.description, localFile: e.file,
			patterns: e.fileMatch, instances: e.instances, required: true,
		}
		g.precheck()

		p.records = append(p.records, record{name: e.name, url: e.url, group: len(p.groups)})
		p.groups = append(p.groups, g)
	}

	for _, d := range set.documents {
		files[normalizeURI(d.uri)] = d.file

		if d.license != "" {
			declared[normalizeURI(d.uri)] = d.license
		}
	}

	p.decider.declared = declared
	p.provider = newProvider(nil, files, fetcher, p.src.limits.MaxDocumentBytes)

	return nil
}

func (p *preparer) setupSchemaStore(ctx context.Context, fetcher *httpfetch.Fetcher) (func(), error) {
	snap, cleanup, err := p.openSnapshot(ctx)
	if err != nil {
		return nil, err
	}

	entries, err := snap.Catalog()
	if err != nil {
		cleanup()

		return nil, fault.Wrap(fault.Integrity, err, "read the SchemaStore catalog")
	}

	p.snap = snap
	p.upstreamNotice = upstreamNotice(snap)
	p.decider.canonical = func(uri string) string {
		if canonical, ok := snap.CanonicalURL(uri); ok {
			return canonical
		}

		return uri
	}
	p.provider = newProvider(snap, nil, fetcher, p.src.limits.MaxDocumentBytes)
	p.schemaStoreGroups(entries)

	return cleanup, nil
}

func (p *preparer) openSnapshot(ctx context.Context) (*upstream.Snapshot, func(), error) {
	cfg := p.src.upstream
	dir := p.opts.SnapshotDir
	cleanup := func() {}

	if dir == "" {
		tmp, err := os.MkdirTemp("", "schepherd-snapshot-*")
		if err != nil {
			return nil, nil, fault.Wrap(fault.Internal, err, "create a temporary snapshot directory")
		}

		cleanup = func() { _ = os.RemoveAll(tmp) }
		dir = filepath.Join(tmp, "snapshot")
	}

	snap, err := p.snapshotAt(ctx, dir)
	if err != nil {
		cleanup()

		return nil, nil, err
	}

	if snap.Commit != cfg.Commit {
		cleanup()

		return nil, nil, fault.New(fault.Usage, "snapshot %s is for commit %s, the source pins %s", dir, snap.Commit, cfg.Commit)
	}

	return snap, cleanup, nil
}

func (p *preparer) snapshotAt(ctx context.Context, dir string) (*upstream.Snapshot, error) {
	cfg := p.src.upstream

	if _, err := os.Stat(dir); err == nil {
		p.log("using the SchemaStore snapshot in %s", dir)

		snap, err := upstream.OpenSnapshot(dir)
		if err != nil {
			return nil, fault.Wrap(fault.Usage, err, "open the SchemaStore snapshot")
		}

		return snap, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fault.Wrap(fault.Usage, err, "snapshot directory")
	}

	tarball := p.opts.Fetcher
	if tarball == nil {
		settings := p.src.fetch
		settings.MaxBytes = cfg.MaxTarballBytes
		settings.Transport = p.opts.Transport
		tarball = httpfetch.New(settings)
	}

	p.log("downloading SchemaStore %s", cfg.Commit)

	snap, err := upstream.FetchSchemaStore(ctx, tarball, cfg.TarballBaseURL, cfg.Commit, dir)
	if err != nil {
		return nil, fault.Wrap(fault.Registry, err, "download the SchemaStore snapshot")
	}

	return snap, nil
}

func (p *preparer) schemaStoreGroups(entries []upstream.CatalogEntry) {
	byKey := map[string]int{}

	var malformed []int

	for _, e := range entries {
		rec := record{name: e.Name, url: e.URL, versions: e.Versions}

		if e.Problem != "" {
			rec.problem = e.Problem
			malformed = append(malformed, len(p.records))
			p.records = append(p.records, rec)

			continue
		}

		key, retrieve, file, fragment := p.schemaStoreSource(e.URL)

		index, merged := byKey[key]
		if !merged {
			index = len(p.groups)
			byKey[key] = index
			p.groups = append(p.groups, &group{
				key: key, retrieve: retrieve, snapshotFile: file, fragment: fragment, name: e.Name, description: e.Description,
			})
		} else {
			rec.mergedInto = p.groups[index].name
		}

		g := p.groups[index]
		rec.group = index

		for _, pattern := range e.FileMatch {
			switch {
			case !supportedPattern(pattern):
				rec.dropped = append(rec.dropped, pattern)
			case !slices.Contains(g.patterns, pattern):
				g.patterns = append(g.patterns, pattern)
			}
		}

		p.records = append(p.records, rec)
	}

	for _, g := range p.groups {
		g.precheck()
	}

	p.malformedGroups(malformed, byKey)
}

// malformedGroups gives the malformed upstream entries failed groups. Such
// a group keeps the source identity of the entry's URL when no valid entry
// has it, so that a published schema whose entry turned malformed is held
// rather than taken for removed upstream; an entry that repeats the URL of a
// valid entry leaves that entry alone.
func (p *preparer) malformedGroups(records []int, valid map[string]int) {
	shared := map[string]int{}

	for _, i := range records {
		rec := &p.records[i]

		key := ""
		if rec.url != "" {
			key, _, _, _ = p.schemaStoreSource(rec.url)
		}

		if _, owned := valid[key]; owned {
			key = ""
		}

		index, ok := shared[key]
		if key == "" || !ok {
			index = len(p.groups)
			p.groups = append(p.groups, &group{key: key, name: rec.name, failReason: ReasonInvalidMetadata})

			if key != "" {
				shared[key] = index
			}
		}

		rec.group = index
	}
}

// schemaStoreSource returns the source identity of an upstream URL (the
// canonical URL of a snapshot file, plus the fragment), the URI its root is
// retrieved from, the snapshot file name and the fragment.
func (p *preparer) schemaStoreSource(uri string) (key, retrieve, file, fragment string) {
	document, fragment := splitFragment(uri)
	key, retrieve = uri, document

	if canonical, ok := p.snap.CanonicalURL(document); ok {
		key, retrieve, file = canonical, canonical, strings.TrimPrefix(canonical, upstream.CanonicalSchemaStoreBase)
		if fragment != "" {
			key += "#" + fragment
		}
	}

	return key, retrieve, file, fragment
}

func supportedPattern(pattern string) bool {
	return match.ValidatePattern(pattern) == nil && CheckMetadata("x", "", "", []string{pattern}, nil) == nil
}

// splitFragment separates a URL into the document and a non-empty fragment;
// an empty fragment is dropped.
func splitFragment(uri string) (document, fragment string) {
	document, fragment, _ = strings.Cut(uri, "#")

	return document, fragment
}

// precheck rejects, before anything is fetched, records that can never
// become catalog entries.
func (g *group) precheck() {
	if err := CheckMetadata(g.name, g.description, "", g.patterns, &catalog.Provenance{Source: g.key}); err != nil {
		g.failReason, g.failDetail = ReasonInvalidMetadata, err.Error()
	}
}
