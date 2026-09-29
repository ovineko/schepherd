// Package publish packs a prepared set into OCI artifacts and publishes it
// as a new catalog revision (docs/publishing.md, "Publishing").
//
// The state of the last publication (package state) decides what is new:
// content it records keeps its artifact, held schemas stay in the catalog
// with their last entry, excluded ones leave it, and a prepared set that
// does not change the catalog is a noop that does not touch the registry. A
// revision is the UTC minute of its publication.
// Publishing never moves a published tag: an interrupted run resumes the
// newest revision when that has the same content, schema artifacts are
// pushed and verified before the catalog index that references them, the
// new state is written once the index and its revision tag exist, and
// catalog-latest, the only mutable tag, moves last and only on request.
// Schema artifacts get no tags of their own: the index references them.
package publish

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"

	"github.com/ovineko/schepherd/internal/artifact"
	"github.com/ovineko/schepherd/internal/calver"
	"github.com/ovineko/schepherd/internal/catalog"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/prepare"
	"github.com/ovineko/schepherd/internal/publisher/state"
	"github.com/ovineko/schepherd/internal/registry"
	"github.com/ovineko/schepherd/internal/store"
)

// Result statuses.
const (
	StatusNoop      = "noop"
	StatusPublished = "published"
	StatusResumed   = "resumed"
)

// LatestTag is the only mutable tag (docs/oci-format.md, "Tags").
const LatestTag = "catalog-latest"

const pushJobs = 8

// Target is the repository a catalog is published to. *registry.Repo
// implements it; every method must verify content by digest and classify its
// errors with package fault, and Resolve must report a missing tag with an
// error for which registry.IsNotFound is true.
type Target interface {
	Exists(ctx context.Context, desc ocispec.Descriptor) (bool, error)
	PushBlob(ctx context.Context, desc ocispec.Descriptor, data []byte) (bool, error)
	PushManifest(ctx context.Context, desc ocispec.Descriptor, data []byte) (bool, error)
	EnsureTag(ctx context.Context, desc ocispec.Descriptor, tag string) (bool, error)
	Tag(ctx context.Context, desc ocispec.Descriptor, tag string) error
	Resolve(ctx context.Context, tag string) (ocispec.Descriptor, error)
	Tags(ctx context.Context) ([]string, error)
	FetchManifestByDigest(ctx context.Context, dgst, mediaType string, limit int64) ([]byte, error)
	FetchTo(ctx context.Context, desc ocispec.Descriptor, w io.Writer) error
}

var _ Target = (*registry.Repo)(nil)

// Options configures Run.
type Options struct {
	// Now is the publication time; a new revision is its UTC minute.
	Now time.Time
	// Log receives progress messages; nil discards them.
	Log func(format string, args ...any)
	// State is the state of the last publication, nil before the first.
	State *state.State
	// StateOut receives the state after the publication; empty writes none.
	StateOut string
	// PreparedDir holds the output of prepare.
	PreparedDir string
	// Repository is reported in the result.
	Repository string
	// UpdateLatest moves catalog-latest after everything else succeeded. In
	// a noop it moves catalog-latest to the catalog of State, which finishes
	// a publication whose last step failed.
	UpdateLatest bool
}

// Result is the machine-readable outcome of Run. Added, Changed,
// MetadataChanged, Held, Excluded and Unchanged compare the catalog with
// State as Diff does. RemovedUpstream lists the IDs of Held whose reason is
// state.HeldRemovedUpstream. UploadedSchemas counts the distinct schema
// artifacts this run pushed, ReusedSchemas the entries that kept the
// artifact State records, and KeptArtifacts lists those among them whose
// content packs to a different artifact today.
type Result struct {
	State           *state.State `json:"-"`
	Status          string       `json:"status"`
	Repository      string       `json:"repository"`
	Revision        string       `json:"revision"`
	CatalogDigest   string       `json:"catalogDigest"`
	Added           []string     `json:"added"`
	Changed         []string     `json:"changed"`
	MetadataChanged []string     `json:"metadataChanged"`
	RemovedUpstream []string     `json:"removedUpstream"`
	Held            []Hold       `json:"held"`
	Excluded        []string     `json:"excluded"`
	KeptArtifacts   []string     `json:"keptArtifacts"`
	Tags            TagChanges   `json:"tags"`
	CatalogSize     int64        `json:"catalogSize"`
	Unchanged       int          `json:"unchanged"`
	UploadedSchemas int          `json:"uploadedSchemas"`
	ReusedSchemas   int          `json:"reusedSchemas"`
}

// TagChanges lists the tags created (moved, for catalog-latest) and the tags
// that already pointed at the right manifest.
type TagChanges struct {
	Created  []string `json:"created"`
	Existing []string `json:"existing"`
}

// artifactRef is one distinct schema artifact of the catalog. packed is nil
// when the prepared set cannot rebuild it byte for byte: it must then exist
// in the repository already.
type artifactRef struct {
	packed *artifact.Packed
	desc   ocispec.Descriptor
	ids    []string
}

type run struct {
	target  Target
	log     func(string, ...any)
	plan    *Plan
	result  *Result
	created []string
	present []string
	opts    Options
	mu      sync.Mutex
}

// Run publishes the prepared set in opts.PreparedDir to target. See the
// package documentation for the guarantees. Invalid prepared sets are
// fault.Usage or fault.Integrity, registry failures fault.Registry; a
// revision that is already taken by another catalog, or not newer than the
// newest one, is fault.Usage and leaves the repository unchanged.
func Run(ctx context.Context, target Target, opts Options) (*Result, error) {
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}

	set, err := prepare.Load(opts.PreparedDir)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "load prepared set")
	}

	plan, err := NewPlan(set, opts.State)
	if err != nil {
		return nil, err
	}

	r := &run{target: target, opts: opts, log: opts.Log, plan: plan}
	diff := plan.Diff()
	r.result = &Result{
		Repository: opts.Repository, Added: diff.Added, Changed: diff.Changed, MetadataChanged: diff.MetadataChanged,
		RemovedUpstream: []string{}, Held: diff.Held, Excluded: diff.Excluded, Unchanged: diff.Unchanged, KeptArtifacts: []string{},
	}

	for _, h := range diff.Held {
		if h.Reason == state.HeldRemovedUpstream {
			r.result.RemovedUpstream = append(r.result.RemovedUpstream, h.ID)
		}
	}

	if opts.State != nil && !plan.HasChanges() {
		return r.noop(ctx)
	}

	return r.publish(ctx)
}

// noop reports the catalog of the state, which the prepared set equals, and
// touches the repository only to finish a publication whose catalog-latest
// step failed.
func (r *run) noop(ctx context.Context) (*Result, error) {
	current := r.opts.State.Catalog

	r.result.Status, r.result.Revision = StatusNoop, current.Revision
	r.result.CatalogDigest, r.result.CatalogSize = current.Digest, current.Size
	r.result.ReusedSchemas = len(r.plan.items)
	r.result.State = r.opts.State
	r.log("the prepared set leaves catalog revision %s (%s) recorded in the state unchanged; nothing to publish",
		current.Revision, current.Digest)

	if len(r.result.Held) > 0 {
		r.log("%d schema(s) stay held with their last entry; the state records holds with the next publication", len(r.result.Held))
	}

	if r.opts.UpdateLatest {
		if err := r.latestToState(ctx); err != nil {
			return nil, err
		}
	}

	if err := r.writeState(r.opts.State); err != nil {
		return nil, err
	}

	r.finishTags()

	return r.result, nil
}

// latestToState moves catalog-latest to the catalog of the state after
// checking that its revision tag names exactly that catalog. The entries
// need no check: in a noop the prepared set already equals the state.
func (r *run) latestToState(ctx context.Context) error {
	desc, err := r.recordedTag(ctx)
	if err != nil {
		return err
	}

	return r.moveLatest(ctx, desc)
}

func (r *run) publish(ctx context.Context) (*Result, error) {
	refs, err := r.pack()
	if err != nil {
		return nil, err
	}

	candidate := r.plan.candidate()

	revision, resumed, err := r.chooseRevision(ctx, candidate)
	if err != nil {
		return nil, err
	}

	candidate.Revision = revision.String()

	packedCatalog, err := packCatalog(candidate)
	if err != nil {
		return nil, err
	}

	catalogDesc := packedCatalog.Index.Descriptor
	r.result.Revision = revision.String()
	r.result.CatalogDigest, r.result.CatalogSize = catalogDesc.Digest.String(), catalogDesc.Size

	if err := r.pushSchemas(ctx, refs); err != nil {
		return nil, err
	}

	if err := r.pushCatalog(ctx, packedCatalog, revision); err != nil {
		return nil, err
	}

	next := r.plan.nextState(revision.String(), catalogDesc.Digest.String(), catalogDesc.Size)
	if err := next.Validate(); err != nil {
		return nil, fault.Wrap(fault.Internal, err, "the new state failed its own validation")
	}

	r.result.State = next

	if err := r.writeState(next); err != nil {
		return nil, err
	}

	if r.opts.UpdateLatest {
		if err := r.moveLatest(ctx, catalogDesc); err != nil {
			return nil, err
		}
	}

	if resumed {
		r.result.Status = StatusResumed
	} else {
		r.result.Status = StatusPublished
	}

	r.finishTags()
	r.log("%s revision %s as %s: %d added, %d changed, %d metadata changed, %d excluded, %d unchanged (%d held); "+
		"%d schema artifacts uploaded, %d reused",
		r.result.Status, r.result.Revision, r.result.CatalogDigest, len(r.result.Added), len(r.result.Changed),
		len(r.result.MetadataChanged), len(r.result.Excluded), r.result.Unchanged, len(r.result.Held),
		r.result.UploadedSchemas, r.result.ReusedSchemas)

	return r.result, nil
}

func (r *run) writeState(next *state.State) error {
	if r.opts.StateOut == "" {
		return nil
	}

	if err := state.Save(r.opts.StateOut, next); err != nil {
		return fault.Wrap(fault.Internal, err, "the catalog is published, but its state was not written")
	}

	return nil
}

func (r *run) finishTags() {
	slices.Sort(r.created)
	slices.Sort(r.present)
	r.result.Tags = TagChanges{Created: append([]string{}, r.created...), Existing: append([]string{}, r.present...)}
}

// pack packs every prepared schema, fills in the artifacts of new and
// changed content and returns the distinct artifacts of the catalog in
// digest order. Unchanged content keeps its recorded artifact even when it
// packs differently today, which is reported: republishing it would give
// the same schema a second digest.
func (r *run) pack() ([]artifactRef, error) {
	limits := artifact.DefaultLimits()
	set := r.plan.set
	byDigest := map[string]*artifactRef{}

	for i := range r.plan.items {
		it := &r.plan.items[i]

		var packed *artifact.Packed

		if it.prepared != nil && it.prepared.Reused == nil {
			var notice []byte
			if it.prepared.Notice != "" {
				notice = set.Notices[it.prepared.Notice]
			}

			p, err := artifact.PackSchema(set.Schemas[it.prepared.ID], notice)
			if err != nil {
				return nil, fault.Wrap(fault.Integrity, err, "pack schema %s", it.prepared.ID)
			}

			packed = p
		}

		switch {
		case !it.reuse:
			if err := checkPacked(it.entry.ID, packed, limits); err != nil {
				return nil, err
			}

			desc := packed.Manifest.Descriptor
			it.entry.Artifact = catalog.Descriptor{MediaType: desc.MediaType, Digest: desc.Digest.String(), Size: desc.Size}
		case packed != nil && packed.Manifest.Descriptor.Digest.String() != it.entry.Artifact.Digest:
			r.log("warning: schema %s is unchanged but packs to %s today; keeping its published artifact %s",
				it.entry.ID, packed.Manifest.Descriptor.Digest, it.entry.Artifact.Digest)
			r.result.KeptArtifacts = append(r.result.KeptArtifacts, it.entry.ID)
			packed = nil
		}

		if it.reuse {
			r.result.ReusedSchemas++
		}

		ref, ok := byDigest[it.entry.Artifact.Digest]
		if !ok {
			ref = &artifactRef{desc: ocispec.Descriptor{
				MediaType: it.entry.Artifact.MediaType, Digest: godigest.Digest(it.entry.Artifact.Digest), Size: it.entry.Artifact.Size,
			}}
			byDigest[it.entry.Artifact.Digest] = ref
		}

		if ref.packed == nil {
			ref.packed = packed
		}

		ref.ids = append(ref.ids, it.entry.ID)
	}

	refs := make([]artifactRef, 0, len(byDigest))
	for _, ref := range byDigest {
		refs = append(refs, *ref)
	}

	slices.SortFunc(refs, func(a, b artifactRef) int { return strings.Compare(a.desc.Digest.String(), b.desc.Digest.String()) })

	return refs, nil
}

// checkPacked refuses artifacts a client with default limits would reject.
func checkPacked(id string, p *artifact.Packed, limits artifact.Limits) error {
	switch {
	case p.Manifest.Descriptor.Size > limits.MaxManifestBytes:
		return fault.New(fault.Integrity, "schema %s: manifest of %d bytes exceeds the client limit of %d", id, p.Manifest.Descriptor.Size, limits.MaxManifestBytes)
	case p.ContentSize > limits.MaxSchemaBytes:
		return fault.New(fault.Integrity, "schema %s: %d bytes exceed the client limit of %d", id, p.ContentSize, limits.MaxSchemaBytes)
	}

	for _, b := range p.Blobs {
		if b.Descriptor.MediaType != artifact.NoticeMediaType && b.Descriptor.Size > limits.MaxPayloadBytes {
			return fault.New(fault.Integrity, "schema %s: payload of %d bytes exceeds the client limit of %d", id, b.Descriptor.Size, limits.MaxPayloadBytes)
		}
	}

	return nil
}

func packCatalog(c *catalog.Catalog) (*artifact.PackedCatalog, error) {
	catalogJSON, err := catalog.Marshal(c)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "encode catalog")
	}

	if _, err := catalog.Parse(catalogJSON, catalog.DefaultLimits()); err != nil {
		return nil, fault.Wrap(fault.Integrity, err, "the new catalog would be rejected by clients")
	}

	if limit := artifact.DefaultLimits().MaxCatalogBytes; int64(len(catalogJSON)) > limit {
		return nil, fault.New(fault.Integrity, "the catalog has %d bytes; clients accept at most %d by default", len(catalogJSON), limit)
	}

	schemas := c.Artifacts()

	packed, err := artifact.PackCatalog(catalogJSON, schemas)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "pack catalog")
	}

	if err := checkIndexSize(packed, len(schemas), artifact.DefaultLimits().MaxManifestBytes); err != nil {
		return nil, err
	}

	return packed, nil
}

// checkIndexSize refuses a catalog index that clients with default limits
// and registries, which cap manifests at the same 4 MiB, would reject. The
// default entry limit keeps an index well below it; this guards the limits
// against drifting apart.
func checkIndexSize(p *artifact.PackedCatalog, schemas int, limit int64) error {
	if size := p.Index.Descriptor.Size; size > limit {
		return fault.New(fault.Integrity, "the catalog index references %d schema artifacts and has %d bytes; clients and registries "+
			"accept manifests of at most %d bytes, so the catalog must list fewer distinct schema artifacts", schemas, size, limit)
	}

	return nil
}

// fetchCatalog downloads the catalog whose index has digest dgst from
// target and verifies it with the client's rules and default limits.
func fetchCatalog(ctx context.Context, target Target, dgst string) (*catalog.Catalog, error) {
	snap, err := store.FetchCatalog(ctx, target, dgst, artifact.DefaultLimits(), catalog.DefaultLimits())
	if err != nil {
		return nil, fault.Wrap(fault.Registry, err, "read the published catalog")
	}

	return snap.Catalog, nil
}

// sameContent compares two catalogs with the revision left out.
func sameContent(a, b *catalog.Catalog) bool {
	x, y := *a, *b
	x.Revision, y.Revision = "", ""

	ma, errA := catalog.Marshal(&x)
	mb, errB := catalog.Marshal(&y)

	return errA == nil && errB == nil && bytes.Equal(ma, mb)
}

// chooseRevision resumes the newest revision of the repository when it is
// newer than the state's and its catalog has the candidate's content: an
// interrupted run leaves exactly that behind, whatever minute or day the
// rerun happens. An older revision with the same content was followed by a
// different catalog, so that content is published again under a new
// revision; reusing the old one would move catalog-latest backwards.
// Otherwise the revision is the UTC minute of Options.Now, which must be
// newer than every revision in the repository and in the state.
func (r *run) chooseRevision(ctx context.Context, candidate *catalog.Catalog) (calver.Revision, bool, error) {
	now := calver.RevisionAt(r.opts.Now)
	if err := now.Check(); err != nil {
		return calver.Revision{}, false, fault.Wrap(fault.Usage, err, "publication time %s", r.opts.Now.UTC().Format(time.RFC3339))
	}

	var known calver.Revision

	if r.opts.State != nil {
		parsed, err := calver.ParseRevision(r.opts.State.Catalog.Revision)
		if err != nil {
			return calver.Revision{}, false, fault.Wrap(fault.Integrity, err, "state")
		}

		known = parsed
	}

	tags, err := r.target.Tags(ctx)
	if err != nil {
		return calver.Revision{}, false, fault.Wrap(fault.Registry, err, "choose the revision")
	}

	var newest calver.Revision

	for _, tag := range tags {
		if v, err := calver.ParseRevisionTag(tag); err == nil && calver.Compare(v, newest) > 0 {
			newest = v
		}
	}

	if !newest.IsZero() && calver.Compare(newest, known) > 0 {
		resume, err := r.hasContent(ctx, newest, candidate)
		if err != nil {
			return calver.Revision{}, false, err
		}

		if resume {
			r.log("resuming revision %s, whose catalog has the same content", newest)

			return newest, true, nil
		}
	}

	for _, taken := range []struct {
		revision calver.Revision
		where    string
	}{{newest, "the repository " + r.opts.Repository}, {known, "the state"}} {
		if !taken.revision.IsZero() && calver.Compare(now, taken.revision) <= 0 {
			return calver.Revision{}, false, revisionTaken(now, taken.revision, taken.where)
		}
	}

	return now, false, nil
}

func revisionTaken(now, newest calver.Revision, where string) error {
	if calver.Compare(now, newest) == 0 {
		return fault.New(fault.Usage, "catalog revision %s already exists in %s with other content; a revision is the UTC minute "+
			"of its publication and never names two catalogs, so publish again in a later minute", now, where)
	}

	return fault.New(fault.Usage, "revision %s of the publication time is older than revision %s in %s; "+
		"revisions must increase, check the clock", now, newest, where)
}

// hasContent reports whether the catalog tagged for revision v has the
// candidate's content. A tag that vanished or holds no valid catalog does
// not.
func (r *run) hasContent(ctx context.Context, v calver.Revision, candidate *catalog.Catalog) (bool, error) {
	desc, err := r.target.Resolve(ctx, v.Tag())
	if registry.IsNotFound(err) {
		return false, nil
	}

	if err != nil {
		return false, fault.Wrap(fault.Registry, err, "resolve %s", v.Tag())
	}

	published, err := fetchCatalog(ctx, r.target, desc.Digest.String())
	if fault.KindOf(err) == fault.Integrity {
		r.log("ignoring %s: %v", v.Tag(), err)

		return false, nil
	}

	if err != nil {
		return false, err
	}

	return sameContent(candidate, published), nil
}

// pushSchemas uploads every schema artifact the repository lacks and then
// checks that all of them are present. A present manifest implies its
// blobs: registries accept a manifest only after every blob it references.
func (r *run) pushSchemas(ctx context.Context, refs []artifactRef) error {
	var uploaded atomic.Int64

	err := parallel(ctx, len(refs), pushJobs, func(ctx context.Context, i int) error {
		ref := &refs[i]
		name := strings.Join(ref.ids, ", ")

		present, err := r.target.Exists(ctx, ref.desc)
		if err != nil {
			return fault.Wrap(fault.Registry, err, "check schema %s", name)
		}

		if present {
			return nil
		}

		if ref.packed == nil {
			return fault.New(fault.Integrity, "schema %s: its published artifact %s is missing from %s, and the prepared set "+
				"cannot rebuild it byte for byte", name, ref.desc.Digest, r.opts.Repository)
		}

		uploaded.Add(1)

		if err := r.pushArtifact(ctx, ref.packed); err != nil {
			return fault.Wrap(fault.Registry, err, "push schema %s", name)
		}

		return nil
	})
	if err != nil {
		return err
	}

	r.result.UploadedSchemas = int(uploaded.Load())

	return parallel(ctx, len(refs), pushJobs, func(ctx context.Context, i int) error {
		present, err := r.target.Exists(ctx, refs[i].desc)
		if err != nil {
			return fault.Wrap(fault.Registry, err, "verify schema %s", strings.Join(refs[i].ids, ", "))
		}

		if !present {
			return fault.New(fault.Registry, "schema %s (%s) is missing from the repository after it was pushed",
				strings.Join(refs[i].ids, ", "), refs[i].desc.Digest)
		}

		return nil
	})
}

func (r *run) pushArtifact(ctx context.Context, p *artifact.Packed) error {
	for _, b := range p.Blobs {
		if _, err := r.target.PushBlob(ctx, b.Descriptor, b.Data); err != nil {
			return fault.Wrap(fault.Registry, err, "push blob %s", b.Descriptor.Digest)
		}
	}

	if _, err := r.target.PushManifest(ctx, p.Manifest.Descriptor, p.Manifest.Data); err != nil {
		return fault.Wrap(fault.Registry, err, "push manifest %s", p.Manifest.Descriptor.Digest)
	}

	return nil
}

func (r *run) pushCatalog(ctx context.Context, p *artifact.PackedCatalog, revision calver.Revision) error {
	desc := p.Index.Descriptor

	// The revision tag is checked before anything is pushed, so a refused
	// publication leaves no catalog behind that claims a revision some other
	// catalog already owns.
	if err := r.checkRevisionTag(ctx, revision, desc); err != nil {
		return err
	}

	present, err := r.target.Exists(ctx, desc)
	if err != nil {
		return fault.Wrap(fault.Registry, err, "check catalog")
	}

	if !present {
		if err := r.pushArtifact(ctx, p.Metadata); err != nil {
			return fault.Wrap(fault.Registry, err, "push catalog metadata")
		}

		if _, err := r.target.PushManifest(ctx, desc, p.Index.Data); err != nil {
			return fault.Wrap(fault.Registry, err, "push catalog index %s", desc.Digest)
		}
	}

	if err := r.ensureTag(ctx, desc, revision.Tag()); err != nil {
		if taken := r.checkRevisionTag(ctx, revision, desc); taken != nil {
			return taken
		}

		return err
	}

	return nil
}

// checkRevisionTag fails with the collision error when the tag of revision
// names another catalog, as it does when another publication took the same
// minute after this one listed the tags.
func (r *run) checkRevisionTag(ctx context.Context, revision calver.Revision, desc ocispec.Descriptor) error {
	taken, err := r.target.Resolve(ctx, revision.Tag())

	switch {
	case err == nil && taken.Digest != desc.Digest:
		return revisionTaken(revision, revision, "the repository "+r.opts.Repository+" (as "+taken.Digest.String()+")")
	case err != nil && !registry.IsNotFound(err):
		return fault.Wrap(fault.Registry, err, "resolve %s", revision.Tag())
	default:
		return nil
	}
}

func (r *run) moveLatest(ctx context.Context, desc ocispec.Descriptor) error {
	current, err := r.target.Resolve(ctx, LatestTag)

	switch {
	case err == nil && current.Digest == desc.Digest:
		r.record(LatestTag, false)

		return nil
	case err != nil && !registry.IsNotFound(err):
		return fault.Wrap(fault.Registry, err, "resolve %s", LatestTag)
	}

	if err := r.target.Tag(ctx, desc, LatestTag); err != nil {
		return fault.Wrap(fault.Registry, err, "move %s", LatestTag)
	}

	r.record(LatestTag, true)

	return nil
}

// ensureTag points tag at desc unless it already does; a tag that points
// elsewhere is never moved.
func (r *run) ensureTag(ctx context.Context, desc ocispec.Descriptor, tag string) error {
	created, err := r.target.EnsureTag(ctx, desc, tag)
	if err != nil {
		return fault.Wrap(fault.Registry, err, "tag %s", tag)
	}

	r.record(tag, created)

	return nil
}

func (r *run) record(tag string, created bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if created {
		r.created = append(r.created, tag)
	} else {
		r.present = append(r.present, tag)
	}
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

// String renders the result for humans.
func (r *Result) String() string {
	var b strings.Builder

	fmt.Fprintf(&b, "status: %s\nrepository: %s\nrevision: %s\ncatalog: %s (%d bytes)\n",
		r.Status, r.Repository, r.Revision, r.CatalogDigest, r.CatalogSize)
	fmt.Fprintf(&b, "changes: %d added, %d changed, %d metadata changed, %d excluded, %d unchanged\n",
		len(r.Added), len(r.Changed), len(r.MetadataChanged), len(r.Excluded), r.Unchanged)

	for _, h := range r.Held {
		fmt.Fprintf(&b, "held: %s (%s)\n", h.ID, h.Reason)
	}
	fmt.Fprintf(&b, "schema artifacts: %d uploaded, %d entries reused\n", r.UploadedSchemas, r.ReusedSchemas)

	if len(r.KeptArtifacts) > 0 {
		fmt.Fprintf(&b, "kept artifacts that pack differently today: %s\n", strings.Join(r.KeptArtifacts, ", "))
	}

	fmt.Fprintf(&b, "tags: %d created, %d present\n", len(r.Tags.Created), len(r.Tags.Existing))

	return b.String()
}
